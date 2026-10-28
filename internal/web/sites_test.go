package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/agent"
)

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
		"/sites/enisparta":          "203.0.113.10",
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

func TestEditSiteForm(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)

	resp, body := c.do("GET", "/sites/enisparta/edit", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `value="NODE_ENV"`) || !strings.Contains(body, ">enisparta.com</textarea>") || !strings.Contains(body, `value="256"`) {
		t.Fatalf("form: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := c.do("POST", "/sites/enisparta/edit", url.Values{"image": {"x"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("no CSRF: %d", resp.StatusCode)
	}

	form := url.Values{
		"csrf": {tok}, "domains": {"enisparta.com\nshop.enisparta.com"}, "image": {"ghcr.io/x/enisparta:v2"},
		"port": {"8080"}, "memory_mb": {"512"}, "data_mount": {""}, "env": {"NEW=1\nNODE_ENV=staging"}, "unset": {"NODE_ENV", "OLD"},
	}
	// What the new-site review would warn about must be seen once on a live site too.
	resp, body = c.do("POST", "/sites/enisparta/edit", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "production değil") || !strings.Contains(body, `name="env_ack"`) ||
		strings.Contains(body, `name="env_ack" checked`) || len(e.agent.updates) != 0 {
		t.Fatalf("notes not shown first: %d", resp.StatusCode)
	}
	form.Set("env_ack", "on")
	resp, _ = c.do("POST", "/sites/enisparta/edit", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/job789" {
		t.Fatalf("update: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u := e.agent.updates[0]
	if len(u.Update.Domains) != 2 || u.Update.Port != 8080 || u.Update.MemoryMB != 512 || u.Update.Set["NEW"] != "1" ||
		strings.Join(u.Update.Unset, ",") != "NODE_ENV,OLD" || u.Actor.User != "furkan" {
		t.Errorf("request = %+v", u)
	}

	e.agent.createErr = &agent.Error{Status: 422, Message: "sorunlar", Problems: []string{"değişiklik yok"}}
	resp, body = c.do("POST", "/sites/enisparta/edit", url.Values{
		"csrf": {tok}, "domains": {"enisparta.com"}, "image": {"img"}, "port": {"3000"}, "memory_mb": {"256"}, "unset": {"NODE_ENV"},
	})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "değişiklik yok") || !strings.Contains(body, `value="NODE_ENV" checked`) {
		t.Errorf("problems: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := c.do("GET", "/sites/nope/edit", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown site: %d", resp.StatusCode)
	}
}
