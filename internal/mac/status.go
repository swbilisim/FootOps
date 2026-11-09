package mac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
	"github.com/swbilisim/FootOps/internal/s3obj"
)

// What the menu bar shows (panel-tasarim §29.3): each server's status file and the
// outside watcher's heartbeat, read from R2, judged into one colour a server and one for
// them all. Nothing here reaches a server: R2 is where they leave word.

const (
	// statusSilent: a status file older than this (or than three of its own intervals)
	// is a server gone quiet — the watcher's "no news" (SILENT_AFTER_MIN).
	statusSilent = 15 * time.Minute
	// watcherSilent: a heartbeat older than this is a watcher that stopped; its messages
	// refused this long, a watcher in trouble — the agent's own alerts (internal/agent,
	// watcherSilentAfter, watcherTroubleAfter).
	watcherSilent  = 30 * time.Minute
	watcherTrouble = 15 * time.Minute
	// Percentages of the root disk and the memory in use worth a look.
	diskFull = 85
	memFull  = 90
	// fileMax is the most read of either file (the agent's are ~1.5 KB).
	fileMax = 1 << 20
)

// Colours, from the best: green (all is well), gray (nothing known: R2 out of reach, a
// Keychain item missing), yellow (worth a look), red (visitors are hit, or a server
// went quiet).
const (
	green  = "green"
	gray   = "gray"
	yellow = "yellow"
	red    = "red"
)

func rank(c string) int {
	switch c {
	case gray:
		return 1
	case yellow:
		return 2
	case red:
		return 3
	}
	return 0
}

func worse(a, b string) string {
	if rank(b) > rank(a) {
		return b
	}
	return a
}

// Status is footops-mac durum's answer (its --json): the colour of it all, and each
// server's.
type Status struct {
	Time  time.Time `json:"time"`
	Color string    `json:"color"`
	// Error: nothing could be read at all (the colour is gray); why.
	Error   string         `json:"error,omitempty"`
	Servers []ServerStatus `json:"servers"`
}

// ServerStatus is one server: its colour, why it is not green, and what the two files
// say.
type ServerStatus struct {
	Code   string `json:"code"`
	Name   string `json:"name"` // as its status file says (H1); else the code in capitals
	Bucket string `json:"bucket"`
	Panel  string `json:"panel,omitempty"`
	SSH    string `json:"ssh,omitempty"`
	Color  string `json:"color"`
	// Problems are why it is not green, the worst first.
	Problems []Problem `json:"problems,omitempty"`

	// The status file as read, and when R2 got it; or why it could not be read. Silent:
	// too old to tell how things are now (its sites' states are not judged).
	File      *monitor.StatusFile `json:"file,omitempty"`
	FileAt    time.Time           `json:"file_at,omitzero"`
	FileError string              `json:"file_error,omitempty"`
	Silent    bool                `json:"silent,omitempty"`

	// The watcher's heartbeat likewise; WatcherStale: too old to go by.
	Watcher      *monitor.WatcherBeat `json:"watcher,omitempty"`
	WatcherAt    time.Time            `json:"watcher_at,omitzero"`
	WatcherError string               `json:"watcher_error,omitempty"`
	WatcherStale bool                 `json:"watcher_stale,omitempty"`

	// Running of Total sites (parked ones among them); Sites one by one, with their
	// domains as the watcher sees them.
	Running int          `json:"running"`
	Total   int          `json:"total"`
	Sites   []SiteStatus `json:"sites"`
}

// Problem is one reason a server is not green.
type Problem struct {
	Color string `json:"color"`
	Text  string `json:"text"`
}

// SiteStatus is a site: its container's state and its domains.
type SiteStatus struct {
	Name    string         `json:"name"`
	State   string         `json:"state"` // the status file's: running, parked…
	Label   string         `json:"label"` // the state in Turkish
	Color   string         `json:"color"`
	Domains []DomainStatus `json:"domains"`
}

// DomainStatus is a domain as the watcher last judged it: ok, blind (a Cloudflare
// challenge answered for the site), failing (its last visit not answered), down (the
// chat was told) or unknown (not on the watcher's list, or no heartbeat).
type DomainStatus struct {
	Name    string    `json:"name"`
	Watcher string    `json:"watcher"`
	Code    string    `json:"code,omitempty"`
	Reason  string    `json:"reason,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// stateTR names a site's state (the status file's, as /durum does).
var stateTR = map[string]string{
	"running":    "çalışıyor",
	"unhealthy":  "sağlıksız",
	"restarting": "yeniden başlıyor",
	"paused":     "yedek için duraklatılmış",
	"stopped":    "durduruldu",
	"missing":    "container yok",
	"parked":     "parkta",
}

// fetched is one file as read from a bucket: its bytes and when R2 got it, or why not.
type fetched struct {
	data []byte
	at   time.Time
	err  error
}

func (e *env) fetch(ctx context.Context, acc access, bucket, name string) fetched {
	o, err := s3obj.At(e.r2(acc.Account)+"/"+bucket, name, acc.KeyID, acc.Secret)
	if err != nil {
		return fetched{err: err}
	}
	data, at, err := o.Get(ctx, e.client, fileMax, e.now())
	if err != nil {
		// The network's own words may name the account's host.
		err = masked{strings.ReplaceAll(err.Error(), acc.Account, "<hesap>"), err}
	}
	return fetched{data: data, at: at, err: err}
}

// masked is an error said without the account, its kind kept for errors.Is / As.
type masked struct {
	text string
	err  error
}

func (m masked) Error() string { return m.text }
func (m masked) Unwrap() error { return m.err }

// collect reads both files of every server, all at once (four small GETs for two
// servers, over one connection to R2).
func (e *env) collect(ctx context.Context) Status {
	// Whole seconds (the files' own times may carry fractions: the app's decoder takes
	// both).
	out := Status{Time: e.now().Truncate(time.Second), Servers: []ServerStatus{}}
	list, acc, err := e.servers(ctx)
	if err != nil {
		out.Color, out.Error = gray, err.Error()
		return out
	}
	files := make([][2]fetched, len(list))
	var wg sync.WaitGroup
	for i, s := range list {
		for j, name := range []string{monitor.StatusKey, monitor.WatcherKey} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				files[i][j] = e.fetch(ctx, acc, s.Bucket, name)
			}()
		}
	}
	wg.Wait()
	now := e.now()
	out.Color = green
	for i, s := range list {
		ss := judge(s, files[i][0], files[i][1], now)
		out.Color = worse(out.Color, ss.Color)
		out.Servers = append(out.Servers, ss)
	}
	return out
}

// readTrouble is a file that could not be read: R2 out of reach or refusing the Mac's
// key is the Mac's own trouble (gray: nothing is known of the server); anything else —
// a file too big, one not understood — is the server's (bad).
func readTrouble(err error, bad string) (string, string) {
	var se *s3obj.Error
	switch {
	case errors.As(err, &se) && (se.Status == 401 || se.Status == 403):
		return gray, "R2 bu Mac'in anahtarını reddetti (" + se.Error() + ")"
	case errors.As(err, &se):
		return gray, "R2 yanıt vermedi (" + se.Error() + ")"
	case errors.Is(err, s3obj.ErrNetwork):
		if rest, ok := strings.CutPrefix(err.Error(), "depoya ulaşılamadı: "); ok {
			return gray, "R2'ye ulaşılamadı: " + rest
		}
		return gray, "R2'den okunurken bağlantı koptu (" + err.Error() + ")"
	}
	return bad, err.Error()
}

var errFormat = errors.New("biçimi tanınmadı")

func decode[T any](f fetched, ok func(*T) bool) (*T, error) {
	if f.err != nil {
		return nil, f.err
	}
	var v T
	if json.Unmarshal(f.data, &v) != nil || !ok(&v) {
		return nil, errFormat
	}
	return &v, nil
}

// judge puts a server's two files together into what the menu shows.
func judge(srv server, sf, wf fetched, now time.Time) ServerStatus {
	s := ServerStatus{Code: srv.Code, Name: strings.ToUpper(srv.Code), Bucket: srv.Bucket, Panel: srv.Panel, SSH: srv.SSH,
		Color: green, Sites: []SiteStatus{}}
	add := func(color, format string, args ...any) {
		s.Problems = append(s.Problems, Problem{Color: color, Text: fmt.Sprintf(format, args...)})
	}

	// The status file.
	file, err := decode(sf, func(f *monitor.StatusFile) bool { return f.Format >= 1 && !f.Time.IsZero() })
	var fileTrouble string
	switch {
	case err == nil:
		// The file as read, its version (shown in the menu) cleaned like the rest.
		shown := *file
		shown.FootOps = clip(clean(shown.FootOps), 40)
		s.File, s.FileAt = &shown, sf.at
		if n := clip(clean(file.Server), 64); n != "" {
			s.Name = n
		}
		at := sf.at
		if at.IsZero() {
			at = file.Time
		}
		limit := max(statusSilent, 3*time.Duration(file.Every)*time.Second)
		if now.Sub(at) > limit {
			s.Silent = true
			add(red, "Sunucudan %s haber yok (son durum dosyası %s): sunucu ya da FootOps durmuş olabilir", spanTR(now.Sub(at)), clock(at, now))
		}
	case errors.Is(err, s3obj.ErrNoObject):
		s.FileError = "durum dosyası yok"
		add(red, "Durum dosyası yok (%s/%s): FootOps hiç yazmamış ya da kova yanlış", srv.Bucket, monitor.StatusKey)
	default:
		color, text := readTrouble(err, red)
		s.FileError, fileTrouble = text, text
		add(color, "Durum dosyası okunamadı: %s", text)
	}

	// The watcher's heartbeat.
	beat, err := decode(wf, func(b *monitor.WatcherBeat) bool { return b.Format >= 1 && !b.Time.IsZero() })
	seen := map[string]monitor.WatcherDomain{}
	switch {
	case err == nil:
		s.Watcher, s.WatcherAt = beat, wf.at
		at := wf.at
		if at.IsZero() {
			at = beat.Time
		}
		if now.Sub(at) > watcherSilent {
			s.WatcherStale = true
			add(yellow, "Dış nöbetçiden %s haber yok (son yoklaması %s): Cloudflare Worker durmuş olabilir", spanTR(now.Sub(at)), clock(at, now))
			break
		}
		// Its messages refused a while, as the agent judges it (a moment's refusal is
		// Telegram's, and passes); its state not saved goes to the agent's own alert.
		if b := beat; b.SendError != "" && !b.SendFailing.IsZero() && now.Sub(b.SendFailing) >= watcherTrouble {
			add(yellow, "Dış nöbetçi %s Telegram'a mesaj gönderemiyor (%s)", spanTR(now.Sub(b.SendFailing)), clip(clean(b.SendError), 200))
		}
		for _, d := range beat.Domains {
			seen[d.Name] = d
		}
	case errors.Is(err, s3obj.ErrNoObject):
		s.WatcherError = "nöbetçi dosyası yok"
		add(yellow, "Dış nöbetçinin dosyası yok: nöbetçi bu sunucuyu izlemiyor olabilir")
	default:
		color, text := readTrouble(err, yellow)
		s.WatcherError = text
		if text != fileTrouble { // R2 out of reach says so once
			add(color, "Dış nöbetçinin dosyası okunamadı: %s", text)
		}
	}

	// The sites, with the watcher's view of their domains.
	byState := map[string][]string{}
	if file != nil {
		if file.SitesError != "" && !s.Silent {
			add(yellow, "Site listesi okunamadı: %s", clip(clean(file.SitesError), 200))
		}
		for _, site := range file.Sites {
			ss := SiteStatus{Name: clip(clean(site.Name), 64), State: clip(clean(site.State), 32), Label: stateTR[site.State],
				Color: green, Domains: []DomainStatus{}}
			if ss.Label == "" {
				ss.Label = ss.State
			}
			s.Total++
			switch site.State {
			case "running", "paused":
				s.Running++
			}
			if !s.Silent {
				switch site.State {
				case "parked", "paused":
					// Parked on purpose (Faz P), or frozen for a backup's moment: no colour
					// of their own. Stopped is not on purpose: the agent says so of any
					// container not running, and parking is how a site is switched off.
					ss.Color = gray
				case "missing", "stopped":
					ss.Color = red
					byState[ss.State] = append(byState[ss.State], ss.Name)
				case "unhealthy", "restarting":
					ss.Color = yellow
					byState[ss.State] = append(byState[ss.State], ss.Name)
				case "running":
				default:
					ss.Color = yellow
					byState[ss.State] = append(byState[ss.State], ss.Name)
				}
			} else {
				ss.Color = gray
			}
			for _, name := range site.Domains {
				ds := domainStatus(name, seen, s.Watcher != nil && !s.WatcherStale)
				switch ds.Watcher {
				case "down":
					ss.Color = red
				case "failing":
					ss.Color = worse(ss.Color, yellow)
				}
				ss.Domains = append(ss.Domains, ds)
			}
			s.Sites = append(s.Sites, ss)
		}
	}
	if names := byState["missing"]; len(names) > 0 {
		add(red, "Container'ı yok: %s", strings.Join(names, ", "))
	}
	if names := byState["stopped"]; len(names) > 0 {
		add(red, "Çalışmıyor (container durmuş): %s", strings.Join(names, ", "))
	}
	for _, st := range []string{"unhealthy", "restarting"} {
		if names := byState[st]; len(names) > 0 {
			add(yellow, "%s: %s", upperFirst(stateTR[st]), strings.Join(names, ", "))
		}
	}
	for st, names := range byState {
		if _, known := stateTR[st]; !known {
			add(yellow, "Bilinmeyen durumda (%s): %s", st, strings.Join(names, ", "))
		}
	}

	// What the watcher says of the server's domains, those not on the status file's
	// list too (it may be unreadable).
	if s.Watcher != nil && !s.WatcherStale {
		var down, failing []string
		for _, d := range s.Watcher.Domains {
			if d.OK {
				continue
			}
			text := clean(d.Name)
			if r := d.Reason; r != "" {
				text += " (" + clip(clean(r), 120)
				if !d.Since.IsZero() {
					text += ", " + clock(d.Since, now) + "'dan beri"
				}
				text += ")"
			}
			if d.Down {
				down = append(down, text)
			} else {
				failing = append(failing, text)
			}
		}
		if len(down) > 0 {
			add(red, "Nöbetçiye göre açılmıyor: %s", strings.Join(down, "; "))
		}
		if len(failing) > 0 {
			add(yellow, "Nöbetçi son yoklamada ulaşamadı, henüz kesinti sayılmadı: %s", strings.Join(failing, "; "))
		}
	}

	// The server's own state, from a file fresh enough to tell.
	if file != nil && !s.Silent {
		for _, p := range file.Backup.Problems {
			add(yellow, "Yedek: %s", clip(clean(p), 200))
		}
		if y := file.Sys; y != nil {
			if p := percent(y.DiskUsed, y.DiskTotal); p >= diskFull {
				add(yellow, "Disk %%%d dolu (%s / %s)", p, sizeTR(y.DiskUsed), sizeTR(y.DiskTotal))
			}
			if p := percent(y.MemUsed, y.MemTotal); p >= memFull {
				add(yellow, "Bellek %%%d dolu (%s / %s)", p, sizeTR(y.MemUsed), sizeTR(y.MemTotal))
			}
		}
	}

	sort.SliceStable(s.Problems, func(i, j int) bool { return rank(s.Problems[i].Color) > rank(s.Problems[j].Color) })
	for _, p := range s.Problems {
		s.Color = worse(s.Color, p.Color)
	}
	return s
}

func domainStatus(name string, seen map[string]monitor.WatcherDomain, fresh bool) DomainStatus {
	ds := DomainStatus{Name: clip(clean(name), 253), Watcher: "unknown"}
	d, ok := seen[name]
	if !fresh || !ok {
		return ds
	}
	switch {
	case d.Down:
		ds.Watcher = "down"
	case !d.OK:
		ds.Watcher = "failing"
	case d.Note != "":
		ds.Watcher = "blind"
	default:
		ds.Watcher = "ok"
	}
	if !d.OK {
		ds.Code, ds.Reason, ds.Since = clip(clean(d.Code), 16), clip(clean(d.Reason), 200), d.Since
	}
	return ds
}

// status is footops-mac durum.
func (e *env) status(ctx context.Context, jsonOut bool) error {
	st := e.collect(ctx)
	if ctx.Err() != nil {
		// Stopped (the app's SIGTERM): what was read by then says nothing.
		return errStopped
	}
	if jsonOut {
		e.event(st)
		if st.Error != "" {
			return errReported
		}
		return nil
	}
	if st.Error != "" {
		return errors.New(st.Error)
	}
	printStatus(e.out, st)
	return nil
}
