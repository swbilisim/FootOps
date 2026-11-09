package mac

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
)

// selfSigned is a certificate for example.com no one trusts.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"}, DNSNames: []string{"example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// sites stands in for the sites behind Cloudflare: one TLS server answering by host,
// and the hosts that fail before an answer — no TLS, a certificate no one trusts, a
// port that refuses.
func sites(t *testing.T) *http.Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.RawQuery, "footops-mac=") || !strings.Contains(r.UserAgent(), "FootOps-Mac/") {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		switch r.Host {
		case "acik.example", "a-site.example", "b-site.example":
		case "yonlen.example":
			http.Redirect(w, r, "https://baska.example/", http.StatusMovedPermanently)
		case "hata.example":
			w.WriteHeader(http.StatusBadGateway)
		case "tunel.example":
			w.WriteHeader(530)
			io.WriteString(w, "error code: 1033")
		case "dogrula.example":
			w.Header().Set("Cf-Ray", "8f00000000000000-IST")
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
		case "yavas.example":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	untrusted := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	untrusted.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	untrusted.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused handshake
	untrusted.StartTLS()
	t.Cleanup(untrusted.Close)
	plain, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { plain.Close() })
	go func() {
		for {
			c, err := plain.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "HTTP/1.1 400 Bad Request\r\n\r\n")
			c.Close()
		}
	}()
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedAddr := closed.Addr().String()
	closed.Close()

	tr := srv.Client().Transport.(*http.Transport).Clone()
	tr.TLSClientConfig.ServerName = "example.com" // the test certificate's name
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		to := srv.Listener.Addr().String()
		switch host {
		case "tlsyok.example":
			to = plain.Addr().String()
		case "sertifika.example":
			to = untrusted.Listener.Addr().String()
		case "kapali.example":
			to = closedAddr
		}
		return (&net.Dialer{}).DialContext(ctx, network, to)
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// probeEnv: h1's status file lists the sites (one parked, one stopped, two names no
// domain), its heartbeat says how the watcher sees some of them; a's status file is
// missing, its heartbeat lists one domain of its own and one of h1's; b's status file
// could not read its site list, its heartbeat lists one.
func probeEnv(t *testing.T) (*env, *strings.Builder) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, _, _ := testEnv(t, r2, testNow)
	f := goodFile(testNow)
	f.Sites = []monitor.StatusSite{
		{Name: "magaza", Domains: []string{"acik.example", "yonlen.example", "dogrula.example"}, State: "running"},
		{Name: "blog", Domains: []string{"hata.example", "tunel.example"}, State: "running"},
		{Name: "yavas", Domains: []string{"yavas.example"}, State: "unhealthy"},
		{Name: "tls", Domains: []string{"tlsyok.example", "sertifika.example", "kapali.example", "10.0.0.1/admin?x=", "localhost:8443"}, State: "running"},
		{Name: "eski", Domains: []string{"eski.example"}, State: "parked"},
		{Name: "kapat", Domains: []string{"kapat.example"}, State: "stopped"},
	}
	r2.put("/footops-h1/kasa/izleme/durum.json", f, testNow.Add(-time.Minute))
	r2.put("/footops-h1/kasa/izleme/nobetci.json", monitor.WatcherBeat{Format: 1, Time: testNow.Add(-time.Minute), Every: 300,
		Domains: []monitor.WatcherDomain{
			{Name: "acik.example", Site: "magaza", OK: true},
			{Name: "hata.example", Site: "blog", Down: true, Code: "502", Reason: "502 — site yanıt vermiyor"},
			{Name: "tunel.example", Site: "blog", OK: true},
			{Name: "yonlen.example", Site: "magaza", Code: "timeout", Reason: "zaman aşımı"},
		}}, time.Time{})
	r2.put("/footops-a/izleme/nobetci.json", monitor.WatcherBeat{Format: 1, Time: testNow.Add(-time.Minute), Every: 300,
		Domains: []monitor.WatcherDomain{{Name: "a-site.example", Site: "asite", OK: true}, {Name: "acik.example", Site: "kopya", OK: true}}}, time.Time{})
	writeSettings(t, e, `{"format":1,"servers":{"b":{}}}`)
	fb := goodFile(testNow)
	fb.Server, fb.Sites, fb.SitesError = "B", []monitor.StatusSite{}, "footops.json okunamadı"
	r2.put("/footops-b/izleme/durum.json", fb, testNow.Add(-time.Minute))
	r2.put("/footops-b/izleme/nobetci.json", monitor.WatcherBeat{Format: 1, Time: testNow.Add(-time.Minute), Every: 300,
		Domains: []monitor.WatcherDomain{{Name: "B-Site.example.", Site: "bsite", OK: true}}}, time.Time{})
	var out strings.Builder
	e.out = &out
	e.probe = sites(t)
	return e, &out
}

// yokla: each domain visited once, the way the watcher does, judged as it judges, next
// to its view; sites switched off are left out and said so.
func TestProbe(t *testing.T) {
	e, out := probeEnv(t)
	if code := e.run(context.Background(), []string{"yokla", "--json"}); code != 0 {
		t.Fatalf("yokla: %d\n%s", code, out)
	}
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	var lines []map[string]json.RawMessage
	for sc.Scan() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 14 || string(lines[0]["event"]) != `"start"` || string(lines[0]["total"]) != "12" ||
		string(lines[1]["event"]) != `"progress"` || string(lines[13]["event"]) != `"result"` {
		t.Fatalf("events:\n%s", out)
	}
	var run ProbeRun
	data, _ := json.Marshal(lines[13])
	if err := json.Unmarshal(data, &run); err != nil {
		t.Fatal(err)
	}
	got := map[string]Probe{}
	for _, p := range run.Domains {
		got[p.Domain] = p
	}
	check := func(domain string, ok bool, code, reason, watcher, verdict string) {
		t.Helper()
		p := got[domain]
		if p.OK != ok || p.Code != code || !strings.Contains(p.Reason, reason) || p.Watcher != watcher || !strings.Contains(p.Verdict, verdict) ||
			(verdict == "" && p.Verdict != "") {
			t.Errorf("%s: %+v", domain, p)
		}
	}
	check("acik.example", true, "200", "", "ok", "")
	check("yonlen.example", true, "301", "", "failing", "bu Mac'ten açılıyor")
	check("dogrula.example", true, "403", "", "unknown", "")
	check("hata.example", false, "502", "502 — site yanıt vermiyor", "down", "sunucu ya da site sorunu")
	check("tunel.example", false, "530", "530 — Cloudflare sunucuya ulaşamadı (Cloudflare 1033: Cloudflare Tunnel bağlı değil)", "ok", "yalnız bu Mac'ten")
	check("yavas.example", false, "timeout", "zaman aşımı (300 ms içinde yanıt yok)", "unknown", "")
	check("tlsyok.example", false, "error", "SSL kurulamadı", "unknown", "")
	check("sertifika.example", false, "error", "SSL sertifikası doğrulanamadı", "unknown", "")
	check("kapali.example", false, "error", "bağlantı kurulamadı", "unknown", "")
	check("a-site.example", true, "200", "", "ok", "")
	check("b-site.example", true, "200", "", "ok", "")
	// Stopped is no reason to look away: the Mac checks what the colour says.
	check("kapat.example", true, "418", "", "unknown", "")
	if p := got["dogrula.example"]; p.Note != "cloudflare-challenge" || !p.Cloudflare || got["acik.example"].Cloudflare {
		t.Errorf("challenge: %+v", p)
	}
	if got["acik.example"].CertExpires.IsZero() || got["acik.example"].Server != "h1" || got["acik.example"].Site != "magaza" {
		t.Errorf("acik: %+v", got["acik.example"])
	}
	if run.Total != 12 || run.Open != 6 || len(run.Skipped) != 1 || run.Skipped[0].Label != "parkta" || run.Skipped[0].Domains[0] != "eski.example" ||
		len(run.Notes) != 3 || !strings.Contains(run.Notes[0], "A: durum dosyası okunamadı") ||
		!strings.Contains(run.Notes[1], "B: durum dosyasında site listesi yok (footops.json okunamadı); alan adları nöbetçinin listesinden") ||
		run.Notes[2] != "alan adına benzemediği için yoklanmadı: 10.0.0.1/admin?x=, localhost:8443" {
		t.Errorf("run: %+v", run)
	}
	// One server only, in the Terminal's words.
	out.Reset()
	if code := e.run(context.Background(), []string{"yokla", "A"}); code != 0 {
		t.Fatalf("yokla A: %d\n%s", code, out)
	}
	text := out.String()
	for _, w := range []string{"bu Mac'ten 2 alan adı", "⚠ A: durum dosyası okunamadı (durum dosyası yok); alan adları nöbetçinin listesinden",
		"✓ a-site.example", "200 · ", "nöbetçi: ✓ açık", "Sonuç: 2 / 2 açık"} {
		if !strings.Contains(text, w) {
			t.Errorf("text lacks %q:\n%s", w, text)
		}
	}
	if strings.Contains(text, "hata.example") {
		t.Errorf("another server's domain:\n%s", text)
	}
	var errb strings.Builder
	e.errw = &errb
	if code := e.run(context.Background(), []string{"yokla", "zz"}); code != 1 || !strings.Contains(errb.String(), "bilinmeyen sunucu: zz (bilinenler: a, b, h1)") {
		t.Errorf("unknown server: %d %s", code, errb.String())
	}
}

// What the two views together say.
func TestVerdict(t *testing.T) {
	for _, c := range []struct {
		ok      bool
		watcher string
		in      string
	}{
		{true, "ok", ""}, {true, "blind", ""}, {true, "unknown", ""}, {false, "unknown", ""},
		{false, "down", "nöbetçi de ulaşamıyor"}, {false, "failing", "nöbetçi de ulaşamıyor"},
		{false, "ok", "yerel ağ ya da DNS"}, {false, "blind", "yerel ağ ya da DNS"},
		{true, "down", "bölgesel ya da geçmişte"}, {true, "failing", "bölgesel ya da geçmişte"},
	} {
		v := verdict(Probe{OK: c.ok, Watcher: c.watcher})
		if (c.in == "" && v != "") || !strings.Contains(v, c.in) {
			t.Errorf("%v %s: %q", c.ok, c.watcher, v)
		}
	}
}

// Nothing to visit: an empty list, not null (the app decodes it); stopped half-way: no
// result, and no visit cut short reported as a failure.
func TestProbeEmptyAndStopped(t *testing.T) {
	e, out, _ := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	if code := e.run(context.Background(), []string{"yokla", "--json"}); code != 0 ||
		!strings.Contains(out.String(), `"total":0,"open":0,"domains":[]`) {
		t.Errorf("nothing: %d %s", code, out)
	}

	p, pout := probeEnv(t)
	p.probeTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	if code := p.run(ctx, []string{"yokla", "h1", "--json"}); code != 1 {
		t.Fatalf("stopped: %d\n%s", code, pout)
	}
	text := pout.String()
	if !strings.HasSuffix(text, `{"event":"error","message":"durduruldu"}`+"\n") || strings.Contains(text, "canceled") ||
		strings.Contains(text, `"domain":"yavas.example"`) || strings.Contains(text, `"event":"result"`) {
		t.Errorf("stopped:\n%s", text)
	}
}
