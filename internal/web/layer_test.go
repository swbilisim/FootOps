package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/agent"
)

// What a container holds in its own layer is shown before it is made anew, and the
// operation goes on only with the box ticked (Faz L).
func TestLayerNotice(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.layer = agent.LayerReport{Files: 3, Paths: []string{"/app/public/uploads — 3 dosya"}}
	const notice, box = "kalıcı klasör dışında 3 dosya", `name="force" value="1" required`
	shows := func(body string, ask bool) bool {
		return strings.Contains(body, notice) && strings.Contains(body, "/app/public/uploads — 3 dosya") && strings.Contains(body, box) == ask
	}

	// Redeploy: the confirmation lists them and asks; without the box the agent refuses
	// and the page comes back.
	if _, body := c.do("GET", "/sites/enisparta/redeploy", nil); !shows(body, true) {
		t.Fatalf("redeploy confirmation:\n%s", body)
	}
	resp, body := c.do("POST", "/sites/enisparta/redeploy", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusConflict || !shows(body, true) || len(e.agent.siteJobs) != 0 {
		t.Fatalf("redeploy without the box: %d %v", resp.StatusCode, e.agent.siteJobs)
	}
	resp, _ = c.do("POST", "/sites/enisparta/redeploy", url.Values{"csrf": {tok}, "force": {"1"}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.siteJobs[0] != "redeploy! enisparta" {
		t.Fatalf("redeploy with the box: %d %v", resp.StatusCode, e.agent.siteJobs)
	}

	// Removal is confirmed by the name: the list is shown, nothing more is asked.
	if _, body := c.do("GET", "/sites/enisparta/remove", nil); !shows(body, false) {
		t.Errorf("remove confirmation:\n%s", body)
	}
	resp, _ = c.do("POST", "/sites/enisparta/remove", url.Values{"csrf": {tok}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.siteJobs[1] != "remove enisparta" {
		t.Errorf("remove: %d %v", resp.StatusCode, e.agent.siteJobs)
	}

	// A move always makes the container anew.
	if _, body := c.do("GET", "/sites/enisparta/move", nil); !shows(body, true) {
		t.Errorf("move form:\n%s", body)
	}
	move := url.Values{"csrf": {tok}, "group": {"kisisel"}, "confirm": {"enisparta"}}
	if resp, body := c.do("POST", "/sites/enisparta/move", move); resp.StatusCode != http.StatusUnprocessableEntity || !shows(body, true) ||
		!strings.Contains(body, `value="kisisel"`) || len(e.agent.f6.moves) != 0 {
		t.Errorf("move without the box: %d %v", resp.StatusCode, e.agent.f6.moves)
	}
	move.Set("force", "1")
	if resp, _ := c.do("POST", "/sites/enisparta/move", move); resp.StatusCode != http.StatusSeeOther || e.agent.f6.moves[0] != "enisparta/kisisel/enisparta!" {
		t.Errorf("move with the box: %d %v", resp.StatusCode, e.agent.f6.moves)
	}

	// The edit form tells beforehand; asks only when the agent says the change makes the
	// container anew, with what was typed kept.
	if _, body := c.do("GET", "/sites/enisparta/edit", nil); !shows(body, false) {
		t.Errorf("edit form:\n%s", body)
	}
	edit := url.Values{"csrf": {tok}, "domains": {"enisparta.com"}, "image": {"ghcr.io/x/enisparta:v2"}, "port": {"3000"},
		"memory_mb": {"256"}, "max_body_mb": {"100"}, "data_mount": {""}, "env": {""}}
	resp, body = c.do("POST", "/sites/enisparta/edit", edit)
	consent := regexp.MustCompile(`name="consent" value="([0-9a-f]+)"`).FindStringSubmatch(body)
	if resp.StatusCode != http.StatusUnprocessableEntity || !shows(body, false) || !strings.Contains(body, `name="force" value="1">`) ||
		consent == nil || !strings.Contains(body, `value="ghcr.io/x/enisparta:v2"`) || len(e.agent.updates) != 0 {
		t.Fatalf("edit without the box: %d %d\n%s", resp.StatusCode, len(e.agent.updates), body)
	}
	// The box counts for the update it was shown with; a form changed since is checked
	// again (and refused again here).
	edit.Set("force", "1")
	edit.Set("consent", consent[1])
	edit.Set("image", "ghcr.io/x/enisparta:v3")
	if resp, _ := c.do("POST", "/sites/enisparta/edit", edit); resp.StatusCode != http.StatusUnprocessableEntity || len(e.agent.updates) != 0 {
		t.Errorf("edit changed after the box: %d %+v", resp.StatusCode, e.agent.updates)
	}
	edit.Set("image", "ghcr.io/x/enisparta:v2")
	if resp, _ := c.do("POST", "/sites/enisparta/edit", edit); resp.StatusCode != http.StatusSeeOther || len(e.agent.updates) != 1 || !e.agent.updates[0].Force {
		t.Errorf("edit with the box: %d %+v", resp.StatusCode, e.agent.updates)
	}

	// The advice follows the site's persistent folder: one it has is where the app must
	// write; without one, the path can become it.
	if _, body := c.do("GET", "/sites/enisparta/redeploy", nil); !strings.Contains(body, "bu yolu <strong>kalıcı klasör</strong> yap") {
		t.Errorf("advice without a folder:\n%s", body)
	}
	e.agent.dataSite = true
	if _, body := c.do("GET", "/sites/enisparta/redeploy", nil); !strings.Contains(body, `kalıcı klasör olarak <span class="mono">/app/uploads</span> kullanıyor`) {
		t.Errorf("advice with a folder:\n%s", body)
	}
	e.agent.dataSite = false

	// Unreadable: said so, and asked. Nothing at stake: nothing shown or asked.
	e.agent.layer = agent.LayerReport{Err: "dockerd yanıt vermedi"}
	if _, body := c.do("GET", "/sites/enisparta/redeploy", nil); !strings.Contains(body, "Container'ın içi denetlenemedi") || !strings.Contains(body, box) {
		t.Errorf("unreadable:\n%s", body)
	}
	e.agent.layer = agent.LayerReport{}
	if _, body := c.do("GET", "/sites/enisparta/redeploy", nil); strings.Contains(body, "kalıcı klasör dışında") || strings.Contains(body, `name="force"`) {
		t.Errorf("nothing at stake:\n%s", body)
	}
	resp, _ = c.do("POST", "/sites/enisparta/redeploy", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.siteJobs[2] != "redeploy enisparta" {
		t.Errorf("redeploy, nothing at stake: %d %v", resp.StatusCode, e.agent.siteJobs)
	}
}
