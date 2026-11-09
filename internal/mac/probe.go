package mac

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
	"github.com/swbilisim/FootOps/internal/version"
)

// Visiting the sites from the Mac (panel-tasarim §29.4): only when asked, the domains
// the status files list, the way the outside watcher does (worker/src/check.js), so the
// two read side by side — one fails: whose trouble it is.

const (
	probeTimeout = 10 * time.Second
	// probeLanes: visits at a time (a run is ~35 visitors for the servers).
	probeLanes = 6
	// probeBody is the most read of an answer: Cloudflare's code in a 530's.
	probeBody = 4 << 10
)

// statusTR is the watcher's few words for a status code (check.js, STATUS).
var statusTR = map[int]string{
	500: "sitede sunucu hatası",
	502: "site yanıt vermiyor",
	503: "site geçici olarak hizmet dışı",
	504: "site zamanında yanıt vermedi",
	520: "sunucudan beklenmeyen yanıt",
	521: "sunucu bağlantıyı reddetti",
	522: "sunucuya bağlanılamadı (kapalı ya da ağı yok)",
	523: "sunucuya ulaşılamıyor",
	524: "sunucu zamanında yanıt vermedi",
	525: "sunucuyla SSL kurulamadı",
	526: "sunucunun SSL sertifikası geçersiz",
	530: "Cloudflare sunucuya ulaşamadı",
}

// cfCodeTR is Cloudflare's own error code in a 530's body (check.js, CF_CODE).
var cfCodeTR = map[string]string{
	"1016": "alan adının köken DNS kaydı çözülemiyor",
	"1033": "Cloudflare Tunnel bağlı değil",
}

var reCFCode = regexp.MustCompile(`(?i)error code:?\s*(\d{4})`)

// statusReason is a failing status's few words, as the watcher writes them.
func statusReason(status int, cf string) string {
	text := statusTR[status]
	if text == "" {
		text = "sunucu hatası"
	}
	text = strconv.Itoa(status) + " — " + text
	if cf != "" {
		text += " (Cloudflare " + cf
		if w := cfCodeTR[cf]; w != "" {
			text += ": " + w
		}
		text += ")"
	}
	return text
}

// Probe is one domain visited from the Mac, next to what the watcher last said of it.
type Probe struct {
	Domain     string `json:"domain"`
	Server     string `json:"server"` // the server's code
	ServerName string `json:"server_name"`
	Site       string `json:"site"`
	// OK: answered (anything but a 5xx: a redirect, a login page, a challenge…). Code is
	// the HTTP status, "timeout" or "error"; MS the time to the answer's first byte.
	OK         bool   `json:"ok"`
	Code       string `json:"code"`
	Reason     string `json:"reason,omitempty"`
	Note       string `json:"note,omitempty"` // cloudflare-challenge: Cloudflare answered for the site
	MS         int64  `json:"ms"`
	Cloudflare bool   `json:"cloudflare"` // the answer came through Cloudflare (cf-ray)
	// CertExpires is when the certificate the visit was shown runs out (Cloudflare's
	// edge one, behind Cloudflare).
	CertExpires time.Time `json:"cert_expires,omitzero"`
	// Watcher is the watcher's view (DomainStatus.Watcher); Verdict what the two
	// together say, when that is not "all is well".
	Watcher string `json:"watcher"`
	Verdict string `json:"verdict,omitempty"`
}

// Skipped is a site not visited: parked or stopped on purpose.
type Skipped struct {
	Server     string   `json:"server"`
	ServerName string   `json:"server_name"`
	Site       string   `json:"site"`
	State      string   `json:"state"`
	Label      string   `json:"label"`
	Domains    []string `json:"domains"`
}

// ProbeRun is footops-mac yokla's result.
type ProbeRun struct {
	Time    time.Time `json:"time"`
	TookMS  int64     `json:"took_ms"`
	Total   int       `json:"total"`
	Open    int       `json:"open"`
	Domains []Probe   `json:"domains"`
	Skipped []Skipped `json:"skipped,omitempty"`
	// Notes: what the run could not go by (a status file unread: that server's domains
	// came from the watcher's list).
	Notes     []string `json:"notes,omitempty"`
	Slowest   string   `json:"slowest,omitempty"`
	SlowestMS int64    `json:"slowest_ms,omitempty"`
}

// targets lists what to visit: the domains of each server's status file, its sites
// switched off on purpose left out; then, for a server whose file could not be read,
// the watcher's list. A domain listed twice is visited once, for the first.
func targets(st Status, only map[string]bool) ([]Probe, []Skipped, []string) {
	list := []Probe{}
	var skipped []Skipped
	var notes []string
	seen := map[string]bool{}
	var invalid []string
	servers := make([]ServerStatus, 0, len(st.Servers))
	for _, s := range st.Servers {
		if len(only) == 0 || only[s.Code] {
			servers = append(servers, s)
		}
	}
	// Those whose status file lists their sites first: what a server says it serves
	// beats a list kept elsewhere.
	listed := func(s ServerStatus) bool { return s.File != nil && s.File.SitesError == "" }
	sort.SliceStable(servers, func(i, j int) bool { return listed(servers[i]) && !listed(servers[j]) })
	for _, s := range servers {
		add := func(domain, site, watcher string) {
			name, ok := domainName(domain)
			switch {
			case !ok:
				invalid = append(invalid, clip(domain, 80))
			case !seen[name]:
				seen[name] = true
				list = append(list, Probe{Domain: name, Server: s.Code, ServerName: s.Name, Site: site, Watcher: watcher})
			}
		}
		switch {
		case listed(s):
			for _, site := range s.Sites {
				if site.State == "parked" {
					sk := Skipped{Server: s.Code, ServerName: s.Name, Site: site.Name, State: site.State, Label: site.Label, Domains: []string{}}
					for _, d := range site.Domains {
						sk.Domains = append(sk.Domains, d.Name)
					}
					skipped = append(skipped, sk)
					continue
				}
				for _, d := range site.Domains {
					add(d.Name, site.Name, d.Watcher)
				}
			}
		case s.Watcher != nil:
			why := "durum dosyası okunamadı (" + s.FileError + ")"
			if s.File != nil {
				why = "durum dosyasında site listesi yok (" + clip(clean(s.File.SitesError), 120) + ")"
			}
			notes = append(notes, s.Name+": "+why+"; alan adları nöbetçinin listesinden")
			for _, d := range s.Watcher.Domains {
				ds := domainStatus(d.Name, map[string]monitor.WatcherDomain{d.Name: d}, !s.WatcherStale)
				add(ds.Name, clean(d.Site), ds.Watcher)
			}
		default:
			notes = append(notes, s.Name+": alan adlarının listesi yok (ne durum dosyasında ne nöbetçide); yoklanmadı")
		}
	}
	if len(invalid) > 0 {
		notes = append(notes, "alan adına benzemediği için yoklanmadı: "+strings.Join(invalid, ", "))
	}
	return list, skipped, notes
}

// reDomain is a host name as the watcher takes one (logic.js, DOMAIN_RE, its length
// apart): labels of letters, digits and hyphens, the last starting with a letter — no
// address, no port, no path. A status file is a server's word: one that lists
// "10.0.0.1/admin" must not send the Mac's visit into its own network.
var reDomain = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// domainName is a listed domain as visited (lower case, no final dot), if it is one.
func domainName(s string) (string, bool) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	return name, len(name) >= 4 && len(name) <= 253 && reDomain.MatchString(name)
}

func newProbeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	tr := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: dialer.DialContext, ForceAttemptHTTP2: true,
		TLSHandshakeTimeout: timeout, MaxIdleConnsPerHost: 1}
	// Redirects not followed: a redirect is an answer.
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var userAgent = "Mozilla/5.0 (compatible; FootOps-Mac/" + version.Version + "; +https://github.com/swbilisim/FootOps)"

// visit visits one domain's home page as a visitor does — through Cloudflare, the
// redirect not followed — with a cache-buster no cache rule can answer for.
func visit(ctx context.Context, c *http.Client, p Probe, timeout time.Duration, now time.Time) Probe {
	vctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := "https://" + p.Domain + "/?footops-mac=" + strconv.FormatInt(now.Unix()/60, 10)
	req, err := http.NewRequestWithContext(vctx, http.MethodGet, addr, nil)
	if err != nil {
		p.Code, p.Reason = "error", "adres kurulamadı ("+clip(err.Error(), 120)+")"
		return p
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	start := time.Now()
	resp, err := c.Do(req)
	p.MS = time.Since(start).Milliseconds()
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		var cv *tls.CertificateVerificationError
		msg := clip(clean(err.Error()), 120)
		switch {
		case errors.Is(vctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
			p.Code, p.Reason = "timeout", fmt.Sprintf("zaman aşımı (%s içinde yanıt yok)", limitTR(timeout))
		case errors.As(err, &cv):
			p.Code, p.Reason = "error", "SSL sertifikası doğrulanamadı ("+msg+")"
		case strings.Contains(msg, "tls:"), strings.Contains(msg, "HTTP response to HTTPS client"):
			p.Code, p.Reason = "error", "SSL kurulamadı ("+msg+")"
		default:
			p.Code, p.Reason = "error", "bağlantı kurulamadı ("+msg+")"
		}
		return p
	}
	defer resp.Body.Close()
	p.Code = strconv.Itoa(resp.StatusCode)
	p.Cloudflare = resp.Header.Get("Cf-Ray") != ""
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		p.CertExpires = resp.TLS.PeerCertificates[0].NotAfter.UTC()
	}
	if resp.StatusCode >= 500 {
		cf := ""
		if resp.StatusCode == 530 {
			// The status is the answer; the body only adds Cloudflare's code, if it comes.
			body, _ := io.ReadAll(io.LimitReader(resp.Body, probeBody))
			if m := reCFCode.FindSubmatch(body); m != nil {
				cf = string(m[1])
			}
		}
		p.Reason = statusReason(resp.StatusCode, cf)
		return p
	}
	p.OK = true
	if m := resp.Header.Get("Cf-Mitigated"); m != "" {
		p.Note = "cloudflare-" + clip(clean(m), 32)
	}
	return p
}

// verdict is what the Mac's visit and the watcher's together say.
func verdict(p Probe) string {
	watcherBad := p.Watcher == "down" || p.Watcher == "failing"
	watcherOK := p.Watcher == "ok" || p.Watcher == "blind"
	switch {
	case !p.OK && watcherBad:
		return "sunucu ya da site sorunu: nöbetçi de ulaşamıyor"
	case !p.OK && watcherOK:
		return "yalnız bu Mac'ten açılmıyor: yerel ağ ya da DNS olabilir"
	case p.OK && watcherBad:
		return "bu Mac'ten açılıyor: nöbetçinin gördüğü bölgesel ya da geçmişte kalmış bir sorun olabilir"
	}
	return ""
}

// probeCmd is footops-mac yokla [server…].
func (e *env) probeCmd(ctx context.Context, only []string, jsonOut bool) error {
	st := e.collect(ctx)
	if st.Error != "" {
		return errors.New(st.Error)
	}
	want := map[string]bool{}
	for _, o := range only {
		code := strings.ToLower(o)
		found := false
		for _, s := range st.Servers {
			found = found || s.Code == code
		}
		if !found {
			var known []string
			for _, s := range st.Servers {
				known = append(known, s.Code)
			}
			return fmt.Errorf("bilinmeyen sunucu: %s (bilinenler: %s)", o, strings.Join(known, ", "))
		}
		want[code] = true
	}
	list, skipped, notes := targets(st, want)
	run := ProbeRun{Time: e.now().Truncate(time.Second), Total: len(list), Domains: list, Skipped: skipped, Notes: notes}
	if jsonOut {
		e.event(map[string]any{"event": "start", "total": run.Total})
	}
	c := e.probe
	if c == nil {
		c = newProbeClient(e.probeTimeout)
		defer c.CloseIdleConnections()
	}
	start := time.Now()
	var mu sync.Mutex
	done := 0
	next := 0
	var wg sync.WaitGroup
	for range min(probeLanes, len(list)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(list) || ctx.Err() != nil {
					mu.Unlock()
					return
				}
				i := next
				next++
				mu.Unlock()
				p := visit(ctx, c, list[i], e.probeTimeout, run.Time)
				if ctx.Err() != nil {
					return // cut short by the stop: no result, not a failure
				}
				p.Verdict = verdict(p)
				mu.Lock()
				list[i] = p
				done++
				if jsonOut {
					e.event(map[string]any{"event": "progress", "done": done, "total": len(list), "probe": p})
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return errStopped
	}
	run.TookMS = time.Since(start).Milliseconds()
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Server != list[j].Server {
			return list[i].Server < list[j].Server
		}
		return list[i].Domain < list[j].Domain
	})
	for _, p := range list {
		if p.OK {
			run.Open++
			if p.MS > run.SlowestMS {
				run.Slowest, run.SlowestMS = p.Domain, p.MS
			}
		}
	}
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			ProbeRun
		}{"result", run})
		return nil
	}
	printProbe(e.out, run)
	return nil
}
