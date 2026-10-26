package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/agent"
)

func TestCreateSiteForm(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)

	resp, body := c.do("GET", "/sites/new", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="domains"`) || !strings.Contains(body, `value="musteri" selected`) {
		t.Fatalf("form: %d", resp.StatusCode)
	}

	form := url.Values{
		"csrf": {tok}, "name": {"enisparta"}, "group": {"musteri"}, "domains": {"enisparta.com\r\nwww.enisparta.com"},
		"image": {"ghcr.io/swbilisim/enisparta:main"}, "port": {"3000"}, "memory_mb": {"384"},
		"data_mount": {"/app/uploads"}, "database": {"on"}, "db_var": {"MONGODB_URI"},
		"env": {"NODE_ENV=production\nSECRET=s3cr3t"},
	}
	resp, _ = c.do("POST", "/sites/new", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/job123" {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req := e.agent.created[0]
	if req.Spec.Name != "enisparta" || len(req.Spec.Domains) != 2 || req.Spec.MemoryMB != 384 || !req.Spec.Database ||
		req.Env["SECRET"] != "s3cr3t" || req.Actor.User != "furkan" {
		t.Errorf("request = %+v", req)
	}
}

func TestCreateSiteProblemsKeepInput(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.createErr = &agent.Error{Status: 422, Message: "sorunlar", Problems: []string{"<b>enisparta.com</b> için sertifika yok"}}
	resp, body := c.do("POST", "/sites/new", url.Values{
		"csrf": {tok}, "name": {"enisparta"}, "group": {"musteri"}, "domains": {"enisparta.com"},
		"image": {"img"}, "port": {"3000"}, "memory_mb": {"256"}, "env": {"A=1"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(body, "&lt;b&gt;enisparta.com&lt;/b&gt; için sertifika yok") || strings.Contains(body, "<b>enisparta.com</b>") {
		t.Error("problem not shown escaped")
	}
	if !strings.Contains(body, `value="enisparta"`) || !strings.Contains(body, ">A=1</textarea>") {
		t.Error("typed values lost")
	}
	resp, _ = c.do("POST", "/sites/new", url.Values{"csrf": {tok}, "env": {"no equals sign"}, "port": {"1"}, "memory_mb": {"1"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad env: %d", resp.StatusCode)
	}
}

func TestRemoveSiteNeedsTypedName(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	resp, body := c.do("POST", "/sites/enisparta/remove", url.Values{"csrf": {tok}, "confirm": {"yes"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "site adını aynen yaz") || len(e.agent.siteJobs) != 0 {
		t.Fatalf("remove without the name: %d %v", resp.StatusCode, e.agent.siteJobs)
	}
	resp, _ = c.do("POST", "/sites/enisparta/remove", url.Values{"csrf": {tok}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.siteJobs[0] != "remove enisparta" {
		t.Errorf("remove: %d %v", resp.StatusCode, e.agent.siteJobs)
	}
	resp, _ = c.do("POST", "/sites/unknown/remove", url.Values{"csrf": {tok}, "confirm": {"unknown"}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown site: %d", resp.StatusCode)
	}
}

func TestSitePagesRender(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	for path, want := range map[string]string{
		"/sites":                    "enisparta.com",
		"/sites/enisparta":          "2.31.81.202",
		"/sites/enisparta/redeploy": "yeniden dağıtılsın mı",
		"/certs":                    "Origin Certificate",
		"/jobs/job123":              "✓ Klasör",
	} {
		resp, body := c.do("GET", path, nil)
		if resp.StatusCode != 200 || !strings.Contains(body, want) {
			t.Errorf("%s: %d, missing %q", path, resp.StatusCode, want)
		}
	}
	_, body := c.do("GET", "/jobs/job123", nil)
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("running job page does not refresh")
	}
	e.agent.jobDone = true
	_, body = c.do("GET", "/jobs/job123", nil)
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("finished job page keeps refreshing")
	}
	if resp, _ := c.do("GET", "/jobs/..%2Fetc", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad job id: %d", resp.StatusCode)
	}
}

func TestCertInstallFlash(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	c.do("POST", "/certs", url.Values{"csrf": {tok}, "cert": {"nope"}, "key": {"nope"}})
	_, body := c.do("GET", "/certs", nil)
	if !strings.Contains(body, "Sertifika yüklenemedi") {
		t.Error("error not shown")
	}
}
