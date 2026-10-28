package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// fakeF6 is the agent's side of the new-site flow, registry logins, group moves and the
// DNS check, for the web tests.
type fakeF6 struct {
	checked  []agent.CreateSiteRequest
	checkErr error
	images   []string
	moves    []string // name/group/confirm
	moveErr  error
	dns      []agent.DomainCheck
	regs     []agent.RegistryLogin
	logins   []string // host/user/token
	loginErr error
	logouts  []string
}

func (f *fakeAgent) CheckSite(_ context.Context, req agent.CreateSiteRequest) (sites.Spec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.f6.checked = append(f.f6.checked, req)
	if f.f6.checkErr != nil {
		return sites.Spec{}, f.f6.checkErr
	}
	sp := req.Spec
	sp.Normalize()
	return sp, nil
}

func (f *fakeAgent) CheckImage(_ context.Context, image string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.f6.images = append(f.f6.images, image)
	return "imgjob1", nil
}

func (f *fakeAgent) MoveSite(_ context.Context, name, group, confirm string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.f6.moves = append(f.f6.moves, name+"/"+group+"/"+confirm)
	if f.f6.moveErr != nil {
		return "", f.f6.moveErr
	}
	return "movejob1", nil
}

func (f *fakeAgent) SiteDNS(context.Context, string) ([]agent.DomainCheck, error) {
	return f.f6.dns, nil
}

func (f *fakeAgent) Registries(context.Context) ([]agent.RegistryLogin, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.RegistryLogin(nil), f.f6.regs...), nil
}

func (f *fakeAgent) RegistryLogin(_ context.Context, host, user, token string, a agent.Actor) (agent.RegistryLoginResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.f6.logins = append(f.f6.logins, host+"/"+user+"/"+token)
	if f.f6.loginErr != nil {
		return agent.RegistryLoginResponse{}, f.f6.loginErr
	}
	return agent.RegistryLoginResponse{Login: agent.RegistryLogin{Host: host, User: user}, Notes: []string{"token gereğinden geniş yetkili (repo)"}}, nil
}

func (f *fakeAgent) RegistryLogout(_ context.Context, host string, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.f6.logouts = append(f.f6.logouts, host)
	return nil
}

var reReview = regexp.MustCompile(`^/sites/new/review/([A-Za-z0-9_-]+)$`)

func imageJob(t *testing.T, info agent.ImageInfo, errMsg string) *agent.Job {
	t.Helper()
	b, _ := json.Marshal(info)
	j := &agent.Job{ID: "imgjob1", Kind: "image.check", Target: info.Image, Done: true, Steps: []string{"✓ İmaj hazır"}, Err: errMsg}
	if errMsg == "" {
		j.Result = string(b)
	}
	return j
}

// The three steps: image check, form filled in from it, review without values, create.
func TestNewSiteFlow(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)

	resp, body := c.do("GET", "/sites/new", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `value="nextjs"`) || !strings.Contains(body, "İmajı kontrol et") {
		t.Fatalf("step 1: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/sites/new", url.Values{"csrf": {tok}, "template": {"nextjs"}, "image": {" ghcr.io/ornek/magaza:main "}, "action": {"check"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/imgjob1?tpl=nextjs" || e.agent.f6.images[0] != "ghcr.io/ornek/magaza:main" {
		t.Fatalf("check: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), e.agent.f6.images)
	}
	e.agent.job = imageJob(t, agent.ImageInfo{Image: "ghcr.io/ornek/magaza:main", Platform: "linux/amd64", UID: 1001, User: "nextjs",
		Ports: []int{4000}, Volumes: []string{"/app/uploads"}, Size: 300 << 20}, "")
	_, body = c.do("GET", "/jobs/imgjob1?tpl=nextjs", nil)
	if !strings.Contains(body, `href="/sites/new/form?check=imgjob1&amp;tpl=nextjs"`) || strings.Contains(body, "Siteye git") {
		t.Fatalf("job page links:\n%s", body)
	}

	_, body = c.do("GET", "/sites/new/form?check=imgjob1&tpl=nextjs", nil)
	for _, want := range []string{`name="port" value="4000"`, `name="data_mount" value="/app/uploads"`, `name="memory_mb" value="512"`,
		`<option value="" selected disabled>`, "İmaj kontrol edildi", `name="database" checked`, `list="dbvars"`, `data-env-file="env"`, "envfile.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("form misses %q", want)
		}
	}

	envText := "export NODE_ENV=production\nSECRET=\"s3cr3t value\"\nMONGODB_URI=mongodb://localhost:27017/magaza\nNEXT_PUBLIC_API=http://localhost:3000\n"
	form := url.Values{"csrf": {tok}, "template": {"nextjs"}, "check": {"imgjob1"}, "name": {"magaza"}, "group": {"kisisel"},
		"domains": {"magaza.example.com"}, "image": {"ghcr.io/ornek/magaza:main"}, "port": {"3000"}, "memory_mb": {"512"},
		"data_mount": {"/app/uploads"}, "database": {"on"}, "db_var": {"MONGODB_URI"}, "env": {envText}}
	resp, _ = c.do("POST", "/sites/new/form", form)
	m := reReview.FindStringSubmatch(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || m == nil {
		t.Fatalf("review redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if env := e.agent.f6.checked[0].Env; env["SECRET"] != "s3cr3t value" || env["MONGODB_URI"] != "" || len(env) != 3 {
		t.Errorf("checked env = %v", env)
	}

	resp, body = c.do("GET", m[0], nil)
	if resp.StatusCode != 200 {
		t.Fatalf("review: %d", resp.StatusCode)
	}
	for _, want := range []string{"/srv/kisisel/magaza", "prj_kisisel_magaza", "SECRET", "12 karakter", "localhost içeriyor",
		"derlemede koda gömülür", "MONGODB_URI ortam değişkenlerinde vardı (satır 3)", "İmaj 4000 portunu bildiriyor", "203.0.113.10"} {
		if !strings.Contains(body, want) {
			t.Errorf("review misses %q", want)
		}
	}
	if strings.Contains(body, "s3cr3t") || strings.Contains(body, "localhost:27017") {
		t.Error("review page shows a value")
	}

	resp, _ = c.do("POST", m[0], url.Values{"csrf": {tok}, "action": {"create"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/job123" {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req := e.agent.created[0]
	if req.Spec.Name != "magaza" || req.Spec.Group != "kisisel" || req.Env["SECRET"] != "s3cr3t value" || req.Env["NODE_ENV"] != "production" ||
		req.Actor.User != "furkan" {
		t.Errorf("created = %+v", req)
	}
	if _, ok := req.Env["MONGODB_URI"]; ok {
		t.Error("the local database address went to the site")
	}
	// Used once.
	resp, _ = c.do("POST", m[0], url.Values{"csrf": {tok}, "action": {"create"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/new" || len(e.agent.created) != 1 {
		t.Errorf("second create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestNewSiteSkipCheckUsesTemplate(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	resp, _ := c.do("POST", "/sites/new", url.Values{"csrf": {tok}, "template": {"static"}, "image": {"nginxinc/nginx-unprivileged:1.27"}, "action": {"skip"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/sites/new/form?") || len(e.agent.f6.images) != 0 {
		t.Fatalf("skip: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body := c.do("GET", resp.Header.Get("Location"), nil)
	for _, want := range []string{`name="port" value="8080"`, `name="memory_mb" value="64"`, `value="nginxinc/nginx-unprivileged:1.27"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form misses %q", want)
		}
	}
	if strings.Contains(body, `name="database" checked`) || strings.Contains(body, "İmaj kontrol edildi") {
		t.Error("static template with a database, or a check that never ran")
	}
	// A failed check is not taken as a check.
	e.agent.job = imageJob(t, agent.ImageInfo{Image: "x/y:1", Ports: []int{9999}}, "imaja erişim yok")
	_, body = c.do("GET", "/sites/new/form?check=imgjob1&tpl=static", nil)
	if strings.Contains(body, "9999") {
		t.Error("failed check used")
	}
	_, body = c.do("GET", "/jobs/imgjob1?tpl=static", nil)
	if !strings.Contains(body, `href="/registries"`) || !strings.Contains(body, "İmaja dön") {
		t.Error("failed check page lacks its ways out")
	}
}

func TestNewSiteFormProblems(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	form := url.Values{"csrf": {tok}, "template": {"node"}, "name": {"magaza"}, "group": {"musteri"}, "domains": {"magaza.example.com"},
		"image": {"img"}, "port": {"3000"}, "memory_mb": {"384"}, "env": {"A=1\nA=2"}}
	resp, body := c.do("POST", "/sites/new/form", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "satır 2") || !strings.Contains(body, ">A=1\nA=2</textarea>") {
		t.Errorf("duplicate key: %d\n%s", resp.StatusCode, body)
	}
	form.Set("env", "A=1")
	e.agent.f6.checkErr = &agent.Error{Status: 422, Message: "sorunlar", Problems: []string{"<b>magaza.example.com</b> için sertifika yok"}}
	resp, body = c.do("POST", "/sites/new/form", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "&lt;b&gt;magaza.example.com&lt;/b&gt; için sertifika yok") ||
		!strings.Contains(body, `value="magaza"`) || !strings.Contains(body, `<option value="musteri" selected>`) {
		t.Errorf("agent problems: %d", resp.StatusCode)
	}
	if len(e.agent.created) != 0 {
		t.Error("created despite problems")
	}
}

// A review belongs to the session that made it, expires, and can go back to the form.
func TestReviewIsPrivateAndExpires(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	form := url.Values{"csrf": {tok}, "template": {"node"}, "name": {"magaza"}, "group": {"musteri"}, "domains": {"magaza.example.com"},
		"image": {"img"}, "port": {"3000"}, "memory_mb": {"384"}, "env": {"TOKEN=abc123"}}
	resp, _ := c.do("POST", "/sites/new/form", form)
	review := resp.Header.Get("Location")

	other := e.client(t, false)
	e.advance(31 * time.Second) // a fresh 2FA window for the second login
	otok := e.login(t, other)
	if resp, _ := other.do("GET", review, nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("other session sees the review: %d", resp.StatusCode)
	}
	if resp, _ := other.do("POST", review, url.Values{"csrf": {otok}, "action": {"create"}}); resp.StatusCode != http.StatusSeeOther || len(e.agent.created) != 0 {
		t.Errorf("other session created it: %d", resp.StatusCode)
	}

	resp, body := c.do("POST", review, url.Values{"csrf": {tok}, "action": {"edit"}})
	if resp.StatusCode != 200 || !strings.Contains(body, ">TOKEN=abc123</textarea>") || !strings.Contains(body, `value="magaza"`) {
		t.Errorf("back to the form: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/sites/new/form", form)
	review = resp.Header.Get("Location")
	e.advance(16 * time.Minute)
	if resp, _ := c.do("GET", review, nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/new" {
		t.Errorf("expired review: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRegistriesPage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	exp := e.srv.now().Add(10 * 24 * time.Hour)
	e.agent.f6.regs = []agent.RegistryLogin{{Host: "ghcr.io", User: "furkan", Saved: e.srv.now(), Expires: exp, Scopes: []string{"read:packages"}}}
	resp, body := c.do("GET", "/registries", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "ghcr.io") || !strings.Contains(body, "10 gün kaldı") || !strings.Contains(body, `type="password" name="token"`) {
		t.Fatalf("page: %d\n%s", resp.StatusCode, body)
	}
	resp, _ = c.do("POST", "/registries", url.Values{"csrf": {tok}, "host": {" ghcr.io "}, "user": {"furkan"}, "token": {"ghp_SECRET1"}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.f6.logins[0] != "ghcr.io/furkan/ghp_SECRET1" {
		t.Fatalf("login: %d %v", resp.StatusCode, e.agent.f6.logins)
	}
	_, body = c.do("GET", "/registries", nil)
	if !strings.Contains(body, "Giriş kaydedildi") || !strings.Contains(body, "geniş yetkili") {
		t.Error("flash missing")
	}
	e.agent.f6.loginErr = &agent.Error{Status: 422, Message: "giriş reddedildi"}
	resp, body = c.do("POST", "/registries", url.Values{"csrf": {tok}, "host": {"ghcr.io"}, "user": {"furkan"}, "token": {"ghp_SECRET2"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "giriş reddedildi") || strings.Contains(body, "ghp_SECRET2") {
		t.Errorf("refused login: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/registries/remove?host=quay.io", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown registry: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/registries/remove?host=ghcr.io", nil)
	if !strings.Contains(body, "girişi kaldırılsın mı") {
		t.Error("confirm page")
	}
	resp, _ = c.do("POST", "/registries/remove", url.Values{"csrf": {tok}, "host": {"ghcr.io"}})
	if resp.StatusCode != http.StatusSeeOther || len(e.agent.f6.logouts) != 1 {
		t.Errorf("remove: %d %v", resp.StatusCode, e.agent.f6.logouts)
	}
}

func TestSiteMovePage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	_, body := c.do("GET", "/sites/enisparta/move", nil)
	if strings.Contains(body, `<option value="musteri"`) || !strings.Contains(body, `<option value="kisisel"`) {
		t.Errorf("group choices:\n%s", body)
	}
	resp, body := c.do("POST", "/sites/enisparta/move", url.Values{"csrf": {tok}, "group": {"kisisel"}, "confirm": {"x"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "site adını aynen yaz") || len(e.agent.f6.moves) != 0 {
		t.Errorf("wrong name: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/sites/enisparta/move", url.Values{"csrf": {tok}, "group": {"kisisel"}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/movejob1" || e.agent.f6.moves[0] != "enisparta/kisisel/enisparta" {
		t.Errorf("move: %d %v", resp.StatusCode, e.agent.f6.moves)
	}
	e.agent.f6.moveErr = &agent.Error{Status: 409, Message: "bu sitede yarım kalmış bir geri yükleme var"}
	resp, body = c.do("POST", "/sites/enisparta/move", url.Values{"csrf": {tok}, "group": {"kisisel"}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "yarım kalmış") || !strings.Contains(body, `<option value="kisisel" selected>`) {
		t.Errorf("agent refusal: %d", resp.StatusCode)
	}
}

func TestSiteDNSPage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	e.agent.f6.dns = []agent.DomainCheck{
		{Domain: "enisparta.com", DNS: "cloudflare", IPs: []string{"104.21.1.1"}, Public: 200, CFRay: true, Local: 200},
		{Domain: "www.enisparta.com", DNS: "elsewhere", IPs: []string{"198.51.100.20"}, PublicErr: "timeout", LocalErr: "connection refused"},
	}
	resp, body := c.do("GET", "/sites/enisparta/dns", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("dns: %d", resp.StatusCode)
	}
	for _, want := range []string{"Cloudflare, proxy açık", "Cloudflare üzerinden", "başka bir yer", "198.51.100.20", "connection refused", "Full (strict)"} {
		if !strings.Contains(body, want) {
			t.Errorf("dns page misses %q", want)
		}
	}
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, `id="dns"`) || !strings.Contains(body, "203.0.113.10") || !strings.Contains(body, "/sites/enisparta/move") {
		t.Error("site page lacks the DNS guide or the group link")
	}
}

func TestDNSRecords(t *testing.T) {
	certs := []sites.Cert{{Names: []string{"enisparta.com", "*.enisparta.com"}}, {Names: []string{"ornek.bel.tr", "*.ornek.bel.tr"}}}
	got := dnsRecords([]string{"enisparta.com", "www.enisparta.com", "shop.ornek.bel.tr", "x.ornek.com.tr", "a.b.example.org"}, certs, "203.0.113.10")
	want := []dnsRecord{
		{"enisparta.com", "enisparta.com", "@", "A", "203.0.113.10"},
		{"www.enisparta.com", "enisparta.com", "www", "CNAME", "enisparta.com"},
		{"shop.ornek.bel.tr", "ornek.bel.tr", "shop", "A", "203.0.113.10"},
		{"x.ornek.com.tr", "ornek.com.tr", "x", "A", "203.0.113.10"},
		{"a.b.example.org", "example.org", "a.b", "A", "203.0.113.10"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPendingStore(t *testing.T) {
	now := time.Now()
	p := newPendingStore(func() time.Time { return now })
	var ids []string
	for i := 0; i < pendingPerSession+2; i++ {
		ids = append(ids, p.put(&pendingSite{session: "s1"}))
		now = now.Add(time.Second)
	}
	if _, ok := p.get("s1", ids[0]); ok {
		t.Error("oldest kept past the per-session limit")
	}
	if _, ok := p.get("s1", ids[len(ids)-1]); !ok {
		t.Error("newest lost")
	}
	if _, ok := p.take("s2", ids[len(ids)-1]); ok {
		t.Error("another session took it")
	}
	p.dropSession("s1")
	if len(p.m) != 0 {
		t.Errorf("left after logout: %d", len(p.m))
	}
}

// A create the agent refuses (a name taken since the review) brings the form back as
// typed, with the reason.
func TestNewSiteCreateRefusedKeepsForm(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	form := url.Values{"csrf": {tok}, "template": {"node"}, "name": {"magaza"}, "group": {"musteri"}, "domains": {"magaza.example.com"},
		"image": {"img"}, "port": {"3000"}, "memory_mb": {"384"}, "env": {"TOKEN=abc123"}}
	resp, _ := c.do("POST", "/sites/new/form", form)
	review := resp.Header.Get("Location")
	e.agent.createErr = &agent.Error{Status: 422, Message: "sorunlar", Problems: []string{"\"magaza\" adında bir site zaten var"}}
	resp, body := c.do("POST", review, url.Values{"csrf": {tok}, "action": {"create"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "adında bir site zaten var") ||
		!strings.Contains(body, ">TOKEN=abc123</textarea>") || !strings.Contains(body, `<option value="musteri" selected>`) {
		t.Errorf("refused create: %d", resp.StatusCode)
	}
}

// A check of one image is not shown for another typed into the form afterwards.
func TestReviewIgnoresCheckOfAnotherImage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.job = imageJob(t, agent.ImageInfo{Image: "ghcr.io/ornek/a:main", Platform: "linux/amd64", UID: 0, Ports: []int{4000}}, "")
	form := url.Values{"csrf": {tok}, "template": {"node"}, "check": {"imgjob1"}, "name": {"magaza"}, "group": {"musteri"},
		"domains": {"magaza.example.com"}, "image": {"ghcr.io/ornek/b:main"}, "port": {"3000"}, "memory_mb": {"384"}, "data_mount": {"/data"}}
	resp, _ := c.do("POST", "/sites/new/form", form)
	_, body := c.do("GET", resp.Header.Get("Location"), nil)
	if strings.Contains(body, "4000") || strings.Contains(body, "root olarak") || !strings.Contains(body, "yapılmadı") {
		t.Errorf("review used the other image's check:\n%s", body)
	}
}

func TestPendingSweep(t *testing.T) {
	now := time.Now()
	p := newPendingStore(func() time.Time { return now })
	p.put(&pendingSite{session: "s1"})
	now = now.Add(pendingTTL + time.Second)
	p.sweep()
	if len(p.m) != 0 {
		t.Error("an abandoned review stayed in memory")
	}
}
