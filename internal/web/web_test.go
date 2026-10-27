package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/cfaccess"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/sites"
)

type fakeAgent struct {
	mu        sync.Mutex
	restarts  []string
	actors    []agent.Actor
	audit     []agent.AuditEntry
	logLines  []string
	created   []agent.CreateSiteRequest
	createErr error
	siteJobs  []string
	jobDone   bool
	alerts    []agent.Alert
	updates   []agent.UpdateSiteRequest
	// database copies
	dbSite    bool
	job       *agent.Job
	snapshots []agent.Snapshot
	cancelled []string
	taken     []agent.Actor
	downloads []http.Header
	dlActors  []agent.Actor
	// imports and exports
	upload    agent.UploadStatus
	chunks    map[int][]byte
	sums      map[int]string
	single    []byte
	checks    []string // upload/sourceDB
	check     *agent.ImportCheck
	applies   []string // mode/confirm
	restores  []string
	exports   []agent.ExportInfo
	exported  int
	removedUp []string
	backups   []agent.FilesBackup
	dataSite  bool // the site has a data directory
	dataURL   string
	fm        fakeFM  // the file manager (files_test.go)
	mig       fakeMig // moving sites (migrate_test.go)
}

func (f *fakeAgent) Overview(context.Context) (agent.Overview, error) {
	return agent.Overview{
		SampledAt: time.Now(),
		System:    agent.SystemMetrics{Hostname: "h1", MemTotal: 4 << 30, MemUsed: 1 << 30},
		Containers: []agent.ContainerInfo{
			{Name: "footops-mongo", Image: "mongo:7.0.45", State: "running", Health: "healthy", Project: "footops-core"},
		},
	}, nil
}

func (f *fakeAgent) History(_ context.Context, rng, name string) (agent.History, error) {
	now := time.Now()
	h := agent.History{Range: rng, Step: 60, From: now.Add(-time.Hour), To: now, Containers: map[string][]history.Point{}}
	for i := 0; i < 30; i++ {
		p := history.Point{T: now.Add(time.Duration(i-30) * time.Minute).Unix(), CPU: float64(i), CPUMax: float64(i + 1), Mem: uint64(i) << 20, MemMax: uint64(i+1) << 20}
		if name == "" {
			h.Host = append(h.Host, p)
		}
		h.Containers["footops-mongo"] = append(h.Containers["footops-mongo"], p)
	}
	return h, nil
}

func (f *fakeAgent) Alerts(context.Context) ([]agent.Alert, error) { return f.alerts, nil }

func (f *fakeAgent) Restart(_ context.Context, name string, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarts = append(f.restarts, name)
	f.actors = append(f.actors, a)
	return nil
}

func (f *fakeAgent) Logs(context.Context, string, int) ([]string, error) { return f.logLines, nil }

func (f *fakeAgent) RecordAuth(_ context.Context, e agent.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audit = append(f.audit, e)
	return nil
}

func (f *fakeAgent) AuditTail(context.Context, int) ([]agent.AuditEntry, error) { return f.audit, nil }

func (f *fakeAgent) Sites(context.Context) ([]agent.SiteStatus, error) {
	return []agent.SiteStatus{{Spec: sites.Spec{Name: "enisparta", Group: "musteri", Domains: []string{"enisparta.com"},
		Image: "ghcr.io/x/enisparta:main", Port: 3000, MemoryMB: 256, EnvKeys: []string{"NODE_ENV"}, Database: f.dbSite,
		DataMount: map[bool]string{true: "/app/uploads", false: ""}[f.dataSite], DataURL: f.dataURL}}}, nil
}

func (f *fakeAgent) CreateSite(_ context.Context, req agent.CreateSiteRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	if f.createErr != nil {
		return "", f.createErr
	}
	return "job123", nil
}

func (f *fakeAgent) UpdateSite(_ context.Context, name string, req agent.UpdateSiteRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, req)
	if f.createErr != nil {
		return "", f.createErr
	}
	return "job789", nil
}

func (f *fakeAgent) SiteJob(_ context.Context, name, verb string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.siteJobs = append(f.siteJobs, verb+" "+name)
	return "job456", nil
}

func (f *fakeAgent) Job(_ context.Context, id string) (agent.Job, error) {
	if f.job != nil {
		return *f.job, nil
	}
	return agent.Job{ID: id, Kind: "site.create", Target: "enisparta", Steps: []string{"✓ Klasör"}, Done: f.jobDone}, nil
}

func (f *fakeAgent) CancelJob(_ context.Context, id string, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, id)
	return nil
}

func (f *fakeAgent) Snapshots(context.Context, string) ([]agent.Snapshot, error) {
	return f.snapshots, nil
}

func (f *fakeAgent) TakeSnapshot(_ context.Context, site string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taken = append(f.taken, a)
	return "jobsnap", nil
}

// snapshotBody is what the fake serves for a download.
const snapshotBody = "0123456789abcdefghij"

func (f *fakeAgent) DownloadSnapshot(_ context.Context, site, id string, a agent.Actor, hdr http.Header) (*http.Response, error) {
	f.mu.Lock()
	f.downloads = append(f.downloads, hdr.Clone())
	f.dlActors = append(f.dlActors, a)
	f.mu.Unlock()
	if id != "20261027-110000" {
		return nil, &agent.Error{Status: 404, Message: "kopya bulunamadı"}
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/gzip")
	rec.Header().Set("Content-Disposition", `attachment; filename="enisparta-20261027-110000.archive.gz"`)
	rec.Header().Set("X-Content-Sha256", "abc")
	req := httptest.NewRequest("GET", "/", nil)
	if v := hdr.Get("Range"); v != "" {
		req.Header.Set("Range", v)
	}
	http.ServeContent(rec, req, "", time.Time{}, strings.NewReader(snapshotBody))
	return rec.Result(), nil
}

const testUpload = "Up0123456789abcdefghijkl"

func (f *fakeAgent) CreateUpload(_ context.Context, site, name string, size int64, a agent.Actor) (agent.UploadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upload = agent.UploadStatus{Upload: agent.Upload{ID: testUpload, Site: site, Name: name, Size: size, Chunk: 8 << 20, Actor: a}, Missing: []int{0}}
	return f.upload, nil
}

func (f *fakeAgent) Upload(_ context.Context, id string) (agent.UploadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != testUpload || f.upload.ID == "" {
		return agent.UploadStatus{}, &agent.Error{Status: 404, Message: "yükleme bulunamadı"}
	}
	return f.upload, nil
}

func (f *fakeAgent) PutChunk(_ context.Context, id string, n int, sum string, body io.Reader, length int64) (agent.UploadStatus, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return agent.UploadStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunks == nil {
		f.chunks, f.sums = map[int][]byte{}, map[int]string{}
	}
	f.chunks[n], f.sums[n] = b, sum
	return f.upload, nil
}

func (f *fakeAgent) FinishUpload(_ context.Context, id string, a agent.Actor) (agent.UploadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upload.Finished = true
	return f.upload, nil
}

func (f *fakeAgent) RemoveUpload(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedUp = append(f.removedUp, id)
	return nil
}

func (f *fakeAgent) SingleUpload(_ context.Context, site, name string, body io.Reader, a agent.Actor) (agent.UploadStatus, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return agent.UploadStatus{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.single = b
	f.upload = agent.UploadStatus{Upload: agent.Upload{ID: testUpload, Site: site, Name: name, Size: int64(len(b)), Finished: true}}
	return f.upload, nil
}

func (f *fakeAgent) CheckImport(_ context.Context, site, upload, sourceDB string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks = append(f.checks, upload+"/"+sourceDB)
	return "jobcheck", nil
}

func (f *fakeAgent) Import(_ context.Context, site, upload string) (agent.ImportCheck, error) {
	if f.check == nil {
		return agent.ImportCheck{}, &agent.Error{Status: 404, Message: "bu yükleme için kontrol sonucu yok"}
	}
	return *f.check, nil
}

func (f *fakeAgent) ApplyImport(_ context.Context, site, upload string, opt agent.ApplyOptions, confirm string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies = append(f.applies, fmt.Sprintf("db=%v:%s files=%v:%s strip=%v/%s", opt.DB, opt.DBMode, opt.Files, opt.FilesMode, opt.Strip, confirm))
	return "jobapply", nil
}

func (f *fakeAgent) FilesBackups(context.Context, string) ([]agent.FilesBackup, error) {
	return f.backups, nil
}

func (f *fakeAgent) RestoreFiles(_ context.Context, site, id, confirm string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, "files:"+id+"/"+confirm)
	return "jobfiles", nil
}

func (f *fakeAgent) RestoreSnapshot(_ context.Context, site, id, confirm string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restores = append(f.restores, id+"/"+confirm)
	return "jobrestore", nil
}

func (f *fakeAgent) Export(_ context.Context, site string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exported++
	return "jobexport", nil
}

func (f *fakeAgent) Exports(context.Context, string) ([]agent.ExportInfo, error) {
	return f.exports, nil
}

func (f *fakeAgent) DownloadExport(_ context.Context, site, id string, a agent.Actor, hdr http.Header) (*http.Response, error) {
	if id != "jobexport" {
		return nil, &agent.Error{Status: 404, Message: "arşiv bulunamadı"}
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/x-tar")
	rec.Header().Set("Content-Disposition", `attachment; filename="enisparta-20261027-110000.footops.tar"`)
	http.ServeContent(rec, httptest.NewRequest("GET", "/", nil), "", time.Time{}, strings.NewReader(snapshotBody))
	return rec.Result(), nil
}

func (f *fakeAgent) Certs(context.Context) ([]sites.Cert, error) { return nil, nil }

func (f *fakeAgent) InstallCert(_ context.Context, c, k string, _ agent.Actor) (sites.Cert, error) {
	if !strings.Contains(c, "BEGIN CERTIFICATE") {
		return sites.Cert{}, errors.New("PEM sertifika bulunamadı")
	}
	return sites.Cert{Names: []string{"enisparta.com"}}, nil
}

func (f *fakeAgent) RemoveCert(context.Context, string, agent.Actor) error { return nil }

func (f *fakeAgent) Server(context.Context) (agent.ServerInfo, error) {
	return agent.ServerInfo{IPv4: []string{"203.0.113.10"}}, nil
}

type fakeAccess struct{ email string }

func (a *fakeAccess) Verify(_ context.Context, tok string) (cfaccess.Claims, error) {
	if tok != "good-token" {
		return cfaccess.Claims{}, cfaccess.ErrInvalid
	}
	return cfaccess.Claims{Email: a.email}, nil
}

type env struct {
	srv    *Server
	agent  *fakeAgent
	access *fakeAccess
	secret string
	clock  *time.Time
	local  *httptest.Server
	tunnel *httptest.Server
}

const password = "test-password-123"

func newEnv(t *testing.T) *env {
	t.Helper()
	secret, _ := auth.NewTOTPSecret()
	hash, _ := auth.HashPassword(password)
	_, recovery, _ := auth.NewRecoveryCodes(2)
	store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err := store.Save(auth.Account{Username: "furkan", PasswordHash: hash, TOTPSecret: secret, RecoveryCodes: recovery}); err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{}
	acc := &fakeAccess{email: "furkan@example.com"}
	cfg := config.Panel{ServerCode: "H1", PanelHost: "panel-h1.example.com", ListenTunnel: config.DefaultTunnel, ListenLocal: config.DefaultLocal}
	s, err := NewServer(cfg, fa, store, acc, nil)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	s.now = func() time.Time { return clock }
	s.sessions.now = s.now
	e := &env{srv: s, agent: fa, access: acc, secret: secret, clock: &clock}
	e.local = httptest.NewServer(s.Handler(viaSSH))
	// Browsers reach the tunnel over HTTPS (Cloudflare), so the test does too: Secure
	// cookies are only sent back over TLS.
	e.tunnel = httptest.NewTLSServer(s.Handler(viaTunnel))
	t.Cleanup(e.local.Close)
	t.Cleanup(e.tunnel.Close)
	return e
}

func (e *env) advance(d time.Duration) { *e.clock = e.clock.Add(d) }

// client follows no redirects and keeps cookies. For the local listener it uses the
// localhost host name the gate expects; for the tunnel it sets the panel host and token.
type client struct {
	t      *testing.T
	base   string
	host   string
	token  string
	hc     *http.Client
	origin string
	header http.Header // added to every request
}

func (e *env) client(t *testing.T, tunnel bool) *client {
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, hc: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if tunnel {
		c.base, c.host, c.token = e.tunnel.URL, "panel-h1.example.com", "good-token"
		c.hc.Transport = e.tunnel.Client().Transport
		c.origin = "https://" + c.host
	} else {
		u, _ := url.Parse(e.local.URL)
		c.base, c.host = e.local.URL, "localhost:"+u.Port()
		c.origin = "http://" + c.host
	}
	return c
}

func (c *client) do(method, path string, form url.Values) (*http.Response, string) {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.base+path, body)
	req.Host = c.host
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", c.origin)
	}
	for k, v := range c.header {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set(cfaccess.Header, c.token)
		req.Header.Set("Cf-Connecting-Ip", "203.0.113.7")
	}
	// The cookie jar keys on the URL host, not req.Host; that is fine for these tests.
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var reCSRF = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func csrfFrom(t *testing.T, html string) string {
	t.Helper()
	m := reCSRF.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no csrf token in page:\n%s", html)
	}
	return m[1]
}

func (e *env) code(t *testing.T) string {
	c, err := auth.TOTPCode(e.secret, *e.clock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// login performs the full login flow and returns the dashboard's CSRF token.
func (e *env) login(t *testing.T, c *client) string {
	t.Helper()
	_, page := c.do("GET", "/login", nil)
	resp, body := c.do("POST", "/login", url.Values{
		"csrf": {csrfFrom(t, page)}, "username": {"furkan"}, "password": {password}, "code": {e.code(t)},
	})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %s\n%s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	resp, body = c.do("GET", "/", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dashboard after login: %d", resp.StatusCode)
	}
	return csrfFrom(t, body)
}

func TestUnauthenticatedRedirectsToLogin(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	for _, p := range []string{"/", "/audit", "/containers/footops-mongo/logs"} {
		resp, _ := c.do("GET", p, nil)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestLoginAndDashboard(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	_, body := c.do("GET", "/", nil)
	if !strings.Contains(body, "footops-mongo") || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Errorf("dashboard missing content:\n%s", body)
	}
	if len(e.agent.audit) != 1 || e.agent.audit[0].Action != "auth.login" || !e.agent.audit[0].OK || e.agent.audit[0].Actor.Via != "ssh" {
		t.Errorf("audit = %+v", e.agent.audit)
	}
}

func TestLoginFailures(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	_, page := c.do("GET", "/login", nil)
	tok := csrfFrom(t, page)

	resp, body := c.do("POST", "/login", url.Values{"csrf": {tok}, "username": {"furkan"}, "password": {"wrong"}, "code": {e.code(t)}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "hatalı") {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	if e.agent.audit[0].OK || e.agent.audit[0].Detail != "bad-credentials" {
		t.Errorf("audit = %+v", e.agent.audit[0])
	}
	if strings.Contains(body, "wrong") {
		t.Error("submitted password echoed back")
	}
	_, page = c.do("GET", "/login", nil)
	resp, _ = c.do("POST", "/login", url.Values{"csrf": {"forged"}, "username": {"furkan"}, "password": {password}, "code": {e.code(t)}})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("login without matching CSRF: %d", resp.StatusCode)
	}
	_ = page
}

func TestLoginRateLimitPerIP(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	var last int
	for i := 0; i < 11; i++ {
		_, page := c.do("GET", "/login", nil)
		resp, _ := c.do("POST", "/login", url.Values{"csrf": {csrfFrom(t, page)}, "username": {"x"}, "password": {"y"}, "code": {"000000"}})
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("11th attempt: %d, want 429", last)
	}
}

func TestRestartRequiresCSRFAndRecordsActor(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)

	resp, _ := c.do("POST", "/containers/footops-mongo/restart", url.Values{"csrf": {"wrong"}})
	if resp.StatusCode != http.StatusForbidden || len(e.agent.restarts) != 0 {
		t.Fatalf("restart without CSRF: %d, restarts %v", resp.StatusCode, e.agent.restarts)
	}
	resp, _ = c.do("POST", "/containers/footops-mongo/restart", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || len(e.agent.restarts) != 1 {
		t.Fatalf("restart: %d %v", resp.StatusCode, e.agent.restarts)
	}
	if a := e.agent.actors[0]; a.User != "furkan" || a.Via != "ssh" {
		t.Errorf("actor = %+v", a)
	}
	_, body := c.do("GET", "/", nil)
	if !strings.Contains(body, "yeniden başlatıldı") {
		t.Error("flash message missing")
	}
	_, body = c.do("GET", "/", nil)
	if strings.Contains(body, "yeniden başlatıldı") {
		t.Error("flash shown twice")
	}
}

func TestCrossOriginPostRefused(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	c.origin = "https://evil.example"
	resp, _ := c.do("POST", "/containers/footops-mongo/restart", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusForbidden || len(e.agent.restarts) != 0 {
		t.Errorf("cross-origin restart: %d", resp.StatusCode)
	}
}

func TestLocalListenerRejectsForeignHost(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	c.host = "rebind.attacker.example"
	resp, _ := c.do("GET", "/login", nil)
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("DNS-rebinding host: %d", resp.StatusCode)
	}
}

func TestTunnelRequiresAccessToken(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	c.token = ""
	if resp, _ := c.do("GET", "/login", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	c.token = "forged"
	if resp, _ := c.do("GET", "/login", nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad token: %d", resp.StatusCode)
	}
	c.token = "good-token"
	c.host = "other.example.com"
	if resp, _ := c.do("GET", "/login", nil); resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("wrong host: %d", resp.StatusCode)
	}
}

func TestTunnelLoginBindsAccessIdentity(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	if a := e.agent.audit[0]; a.Actor.IP != "203.0.113.7" || a.Actor.Via != "tunnel" || !strings.Contains(a.Detail, "furkan@example.com") {
		t.Errorf("audit = %+v", a)
	}
	// Another Access identity presenting the same cookie loses the session.
	e.access.email = "someone-else@example.com"
	if resp, _ := c.do("GET", "/", nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("session survived an Access identity change: %d", resp.StatusCode)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	e := newEnv(t)
	for _, tunnel := range []bool{false, true} {
		c := e.client(t, tunnel)
		_, page := c.do("GET", "/login", nil)
		resp, _ := c.do("POST", "/login", url.Values{"csrf": {csrfFrom(t, page)}, "username": {"furkan"}, "password": {password}, "code": {e.code(t)}})
		e.advance(31 * time.Second) // next TOTP step for the next login
		var sc *http.Cookie
		for _, ck := range resp.Cookies() {
			if ck.Name == sessionCookie {
				sc = ck
			}
		}
		if sc == nil || !sc.HttpOnly || sc.SameSite != http.SameSiteStrictMode || sc.Secure != tunnel {
			t.Errorf("tunnel=%v cookie = %+v", tunnel, sc)
		}
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	e.advance(sessionIdle + time.Second)
	if resp, _ := c.do("GET", "/", nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("idle session still valid: %d", resp.StatusCode)
	}
}

func TestLogsAreEscaped(t *testing.T) {
	e := newEnv(t)
	e.agent.logLines = []string{`<script>alert(1)</script>`}
	c := e.client(t, false)
	e.login(t, c)
	_, body := c.do("GET", "/containers/footops-mongo/logs", nil)
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("log line not escaped:\n%s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.client(t, false).do("GET", "/login", nil)
	for h, want := range map[string]string{
		"Content-Security-Policy": "default-src 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
		"Cache-Control":           "no-store",
	} {
		if !strings.Contains(resp.Header.Get(h), want) {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
}

func TestLogout(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	resp, _ := c.do("POST", "/logout", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/", nil); resp.StatusCode != http.StatusSeeOther {
		t.Error("session still valid after logout")
	}
}

func TestNoAccountMessage(t *testing.T) {
	e := newEnv(t)
	e.srv.store = auth.NewStore(filepath.Join(t.TempDir(), "none.json"))
	c := e.client(t, false)
	_, page := c.do("GET", "/login", nil)
	resp, body := c.do("POST", "/login", url.Values{"csrf": {csrfFrom(t, page)}, "username": {"a"}, "password": {"b"}, "code": {"1"}})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "reset-password") {
		t.Errorf("no account: %d", resp.StatusCode)
	}
}
