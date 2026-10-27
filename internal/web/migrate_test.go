package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/remote"
)

// fakeMig is the agent's side of moving sites, for the web tests.
type fakeMig struct {
	sources []agent.SourceView
	view    *agent.MigrationView // nil: no move yet
	sets    []agent.MigrationChoice
	jobs    []string // verb/confirm
	tests   []string
	testErr error
	removed []string // id/cleanup
	forgot  int
}

func (f *fakeAgent) Sources(context.Context) ([]agent.SourceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.SourceView(nil), f.mig.sources...), nil
}

func (f *fakeAgent) CreateSource(_ context.Context, label, host string, port int, user string, a agent.Actor) (agent.SourceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p := remote.Validate(label, host, port, user); len(p) > 0 {
		return agent.SourceView{}, &agent.Error{Status: 422, Message: "kaynak bilgilerinde sorunlar var", Problems: p}
	}
	src := agent.SourceView{Source: remote.Source{ID: "yenikaynak01", Label: label, Host: host, Port: port, User: user,
		PublicKey: "ssh-ed25519 AAAAC3Nz footops-h1-yenikaynak01"}}
	src.Authorized = `restrict,from="203.0.113.10" ` + src.PublicKey
	src.Install = remote.InstallCommand(src.Authorized)
	f.mig.sources = append(f.mig.sources, src)
	return src, nil
}

func (f *fakeAgent) TestSource(_ context.Context, id string, a agent.Actor) (agent.SourceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mig.tests = append(f.mig.tests, id)
	if f.mig.testErr != nil {
		return agent.SourceView{}, f.mig.testErr
	}
	for i := range f.mig.sources {
		if f.mig.sources[i].ID == id {
			f.mig.sources[i].HostKeys, f.mig.sources[i].System = []string{"ED25519 SHA256:abc"}, "Eski Linux 7 · 3.10.0"
			f.mig.sources[i].Checked = time.Now()
			return f.mig.sources[i], nil
		}
	}
	return agent.SourceView{}, &agent.Error{Status: 404, Message: "kaynak sunucu bulunamadı"}
}

func (f *fakeAgent) ResetHostKey(_ context.Context, id string, a agent.Actor) (agent.SourceView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.mig.sources {
		if f.mig.sources[i].ID == id {
			f.mig.sources[i].HostKeys = nil
			return f.mig.sources[i], nil
		}
	}
	return agent.SourceView{}, &agent.Error{Status: 404, Message: "kaynak sunucu bulunamadı"}
}

func (f *fakeAgent) RemoveSource(_ context.Context, id string, cleanup bool, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []agent.SourceView
	for _, s := range f.mig.sources {
		if s.ID != id {
			keep = append(keep, s)
		}
	}
	f.mig.sources = keep
	f.mig.removed = append(f.mig.removed, id+"/"+map[bool]string{true: "cleanup", false: "local"}[cleanup])
	return nil
}

func (f *fakeAgent) Migration(_ context.Context, site string) (agent.MigrationView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := agent.MigrationView{}
	if f.mig.view != nil {
		v = *f.mig.view
	}
	v.Sources = append([]agent.SourceView(nil), f.mig.sources...)
	if v.Migration != nil {
		for i := range v.Sources {
			if v.Sources[i].ID == v.Migration.Source {
				v.Source = &v.Sources[i]
			}
		}
	}
	return v, nil
}

func (f *fakeAgent) SetMigration(_ context.Context, site string, ch agent.MigrationChoice, a agent.Actor) (agent.Migration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mig.sets = append(f.mig.sets, ch)
	if ch.FilesDir != "" {
		if err := remote.ValidDir(ch.FilesDir); err != nil {
			return agent.Migration{}, &agent.Error{Status: 422, Message: "x", Problems: []string{err.Error()}}
		}
	}
	if f.mig.view == nil {
		f.mig.view = &agent.MigrationView{}
	}
	if f.mig.view.Migration == nil {
		f.mig.view.Migration = &agent.Migration{Site: site}
	}
	m := f.mig.view.Migration
	if ch.Source != "" {
		m.Source = ch.Source
	}
	if !ch.SourceOnly {
		m.DB, m.Container, m.FilesDir, m.AppDir = ch.DB, ch.Container, ch.FilesDir, ch.AppDir
	}
	return *m, nil
}

func (f *fakeAgent) ForgetMigration(context.Context, string, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mig.view = nil
	f.mig.forgot++
	return nil
}

func (f *fakeAgent) MigrationJob(_ context.Context, site, verb, confirm string, a agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mig.jobs = append(f.mig.jobs, verb+"/"+confirm)
	return "jobmig", nil
}

// migSite is the fake agent's only site (see fakeAgent.Sites).
var migSite = func() string {
	list, _ := (&fakeAgent{}).Sites(context.Background())
	return list[0].Spec.Name
}()

func migEnv(t *testing.T) (*env, *client, string) {
	t.Helper()
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite, e.agent.dataSite = true, true
	return e, c, tok
}

func expect(t *testing.T, c *client, method, path string, form url.Values, code int) string {
	t.Helper()
	resp, body := c.do(method, path, form)
	if resp.StatusCode != code {
		t.Fatalf("%s %s: %d, want %d\n%s", method, path, resp.StatusCode, code, body)
	}
	return body
}

func TestMigrateWizard(t *testing.T) {
	e, c, tok := migEnv(t)
	fa := e.agent
	base := "/sites/" + migSite + "/migrate"
	form := func(kv ...string) url.Values {
		v := url.Values{"csrf": {tok}}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Add(kv[i], kv[i+1])
		}
		return v
	}
	// No move yet: the page offers to add a source.
	body := expect(t, c, "GET", base, nil, 200)
	for _, want := range []string{"Kaynak sunucu", "Yeni kaynak ekle", `name="host"`} {
		if !strings.Contains(body, want) {
			t.Errorf("first page lacks %q", want)
		}
	}
	// A bad address is shown back as a problem.
	body = expect(t, c, "POST", base+"/source", form("new", "1", "label", "eski", "host", "-oProxyCommand=x", "port", "22", "user", "root"), 422)
	if !strings.Contains(body, "IP ya da alan adı") {
		t.Errorf("bad host not explained:\n%s", body)
	}
	expect(t, c, "POST", base+"/source", form("new", "1", "label", "eski sunucu", "host", "203.0.113.20", "port", "22", "user", "root"), 303)
	if len(fa.mig.sets) != 1 || fa.mig.sets[0].Source != "yenikaynak01" || !fa.mig.sets[0].SourceOnly {
		t.Fatalf("sets = %+v", fa.mig.sets)
	}
	// Not connected yet: the key line and the command are shown, the next steps are locked.
	body = expect(t, c, "GET", base, nil, 200)
	for _, want := range []string{"restrict,from=&#34;203.0.113.10&#34; ssh-ed25519", "authorized_keys", "Bağlantıyı dene"} {
		if !strings.Contains(body, want) {
			t.Errorf("unconnected page lacks %q", want)
		}
	}
	if strings.Contains(body, `action="`+base+`/copy"`) {
		t.Error("copy offered before the connection works")
	}
	expect(t, c, "POST", base+"/test", form(), 303)
	if len(fa.mig.tests) != 1 {
		t.Fatal("test not run")
	}
	// Connected: discovery, the choice, the copy with confirmation.
	expect(t, c, "POST", base+"/discover", form(), 303)
	fa.mig.view.Discovery = &remote.Discovery{Complete: true, Mongo: []remote.MongoSource{{Via: "host", Shell: "mongo",
		DBs: []remote.MongoDB{{Name: "eski", Collections: 2, Objects: 14, DataSize: 5000}}}},
		Candidates: []remote.Candidate{{Dir: "/home/eski/www/public/uploads", Counts: remote.Counts{Files: 3, Bytes: 20}}}}
	fa.mig.view.Suggest = &remote.Suggestion{FilesDir: "/home/eski/www/public/uploads", DB: &remote.DBChoice{Name: "eski"}}
	body = expect(t, c, "GET", base, nil, 200)
	for _, want := range []string{`value="|eski" selected`, "önerilen", `value="/home/eski/www/public/uploads"`} {
		if !strings.Contains(body, want) {
			t.Errorf("choice form lacks %q", want)
		}
	}
	body = expect(t, c, "POST", base+"/choose", form("db", "|eski", "files_dir", "/etc"), 422)
	if !strings.Contains(body, "sistem klasörü") {
		t.Errorf("bad folder not explained")
	}
	expect(t, c, "POST", base+"/choose", form("db", "|eski", "files_dir", "/home/eski/www/public/uploads"), 303)
	if m := fa.mig.view.Migration; m.DB != "eski" || m.Container != "" || m.FilesDir != "/home/eski/www/public/uploads" {
		t.Errorf("migration = %+v", m)
	}
	body = expect(t, c, "GET", base, nil, 200)
	if !strings.Contains(body, `action="`+base+`/copy"`) {
		t.Fatal("copy not offered")
	}
	expect(t, c, "POST", base+"/copy", form("confirm", "yanlis"), 422)
	resp, _ := c.do("POST", base+"/copy", form("confirm", migSite))
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/jobs/jobmig" {
		t.Errorf("copy: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if strings.Join(fa.mig.jobs, ",") != "discover/,copy/"+migSite {
		t.Errorf("jobs = %v", fa.mig.jobs)
	}
	// A finished copy shows both sides' counts.
	fa.mig.view.Migration.Copies = []agent.CopyResult{{Job: "jobmig", DB: "eski", SourceDocs: map[string]int64{"urunler": 10},
		SiteDocs: map[string]int64{"urunler": 10}, FilesDir: "/x", SourceFiles: &remote.Counts{Files: 3}, Finished: time.Now()}}
	body = expect(t, c, "GET", base, nil, 200)
	if !strings.Contains(body, "urunler") || !strings.Contains(body, "/jobs/jobmig") {
		t.Error("last copy not shown")
	}
	// CSRF holds.
	if resp, _ := c.do("POST", base+"/copy", url.Values{"confirm": {migSite}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without CSRF: %d", resp.StatusCode)
	}
	expect(t, c, "POST", base+"/forget", form(), 303)
	if fa.mig.forgot != 1 {
		t.Error("not forgotten")
	}
}

func TestSourcesPage(t *testing.T) {
	e, c, tok := migEnv(t)
	e.agent.CreateSource(context.Background(), "eski sunucu", "203.0.113.20", 22, "root", agent.Actor{})
	body := expect(t, c, "GET", "/sources", nil, 200)
	if !strings.Contains(body, "eski sunucu") || !strings.Contains(body, "root@203.0.113.20") {
		t.Errorf("sources page:\n%s", body)
	}
	expect(t, c, "GET", "/sources/yenikaynak01/remove", nil, 200)
	expect(t, c, "GET", "/sources/YOK/remove", nil, 404)
	expect(t, c, "POST", "/sources/yenikaynak01/remove", url.Values{"csrf": {tok}, "cleanup": {"on"}}, 303)
	if strings.Join(e.agent.mig.removed, ",") != "yenikaynak01/cleanup" {
		t.Errorf("removed = %v", e.agent.mig.removed)
	}
}
