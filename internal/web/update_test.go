package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/update"
)

func (f *fakeAgent) UpdateStatus(_ context.Context, brief bool) (agent.UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updBrief = append(f.updBrief, brief)
	return f.upd, nil
}

func (f *fakeAgent) UpdateCheck(context.Context, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updCalls = append(f.updCalls, "check")
	return f.updErr
}

func (f *fakeAgent) UpdateApply(_ context.Context, v string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updCalls = append(f.updCalls, "apply "+v)
	return "job1", f.updErr
}

func (f *fakeAgent) UpdateBack(context.Context, agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updCalls = append(f.updCalls, "back")
	return "job2", f.updErr
}

func TestUpdatePage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)

	// Current: nothing to install.
	e.agent.upd = agent.UpdateStatus{Running: "v0.1.0", Latest: &release.Manifest{Version: "v0.1.0", Commit: "abc1234", Date: "2026-11-10T13:00:00Z"},
		Checked: time.Now().Add(-time.Hour), Next: time.Now().Add(time.Hour)}
	resp, body := c.do("GET", "/server/update", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "güncel") || strings.Contains(body, "sürümüne güncelle") || !strings.Contains(body, `href="/" aria-current="page"`) {
		t.Fatalf("current: %d\n%s", resp.StatusCode, body)
	}
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, `href="/server/update"`) || !strings.Contains(body, "güncel") {
		t.Error("dashboard card")
	}
	full := false
	for _, b := range e.agent.updBrief {
		full = full || !b
	}
	if !full || !e.agent.updBrief[len(e.agent.updBrief)-1] {
		t.Errorf("the page asks in full, the dashboard briefly: %v", e.agent.updBrief)
	}

	// A newer one, ready: its notes, what changes, the button.
	e.agent.upd.Latest = &release.Manifest{Version: "v0.2.0", Commit: "def5678", Date: "2026-11-12T10:00:00Z", Notes: "Eski imajlar silinir.\n<b>yeni</b>"}
	e.agent.upd.Newer = true
	e.agent.upd.Ready = &agent.UpdateReady{Version: "v0.2.0", At: time.Now(), Plan: update.Plan{
		Steps:  []update.PlanStep{{Name: "core-stack", Title: "Çekirdek container'lar", Change: true, Detail: "Caddy, MongoDB"}, {Name: "swap", Title: "Takas", Change: false}},
		Impact: []string{"Caddy ve/veya MongoDB yeniden oluşturulur: siteler birkaç saniye erişilemez."}}}
	_, body = c.do("GET", "/server/update", nil)
	for _, want := range []string{"Eski imajlar silinir.\n&lt;b&gt;yeni&lt;/b&gt;", "siteler birkaç saniye erişilemez", "<strong>Çekirdek container&#39;lar</strong> — Caddy, MongoDB",
		`name="version" value="v0.2.0"`, "v0.2.0 sürümüne güncelle", "kendiliğinden dönülür"} {
		if !strings.Contains(body, want) {
			t.Errorf("ready page lacks %q", want)
		}
	}
	if strings.Contains(body, "<strong>Takas</strong>") {
		t.Error("an unchanged step is listed")
	}
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, "v0.2.0 hazır") || !strings.Contains(body, "yeni sürüm") {
		t.Error("dashboard card does not say a version is ready")
	}
	resp, _ = c.do("POST", "/server/update/apply", url.Values{"csrf": {tok}, "version": {"v0.2.0"}})
	if resp.StatusCode != http.StatusSeeOther || strings.Join(e.agent.updCalls, ",") != "apply v0.2.0" {
		t.Fatalf("apply: %d %v", resp.StatusCode, e.agent.updCalls)
	}
	e.agent.upd.Applying = true
	e.agent.upd.Run = &update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Phase: update.PhaseApplying}
	e.agent.upd.Log = []string{"[1/20] packages: already done", "[16/20] panel-binary: ok"}
	_, body = c.do("GET", "/server/update", nil)
	for _, want := range []string{"Güncelleme başladı", `data-live="3"`, "Güncelleme sürüyor", "panel-binary: ok", "sürüyor</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("applying page lacks %q", want)
		}
	}
	if !strings.Contains(body, `class="primary" disabled`) {
		t.Error("the button stays usable while an update runs")
	}

	// Rolled back: said, with the reason; going back is offered once it has ended.
	e.agent.upd.Applying = false
	e.agent.upd.Run = &update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now().Add(-time.Minute), Finished: time.Now(),
		Phase: update.PhaseRolledBack, Err: "v0.2.0 1m30s içinde açılmadı"}
	e.agent.upd.Prev = "v0.1.0"
	_, body = c.do("GET", "/server/update", nil)
	for _, want := range []string{"açılmadı, önceki sürüme dönüldü", "v0.2.0 1m30s içinde açılmadı", "Önceki sürüme dön (v0.1.0)"} {
		if !strings.Contains(body, want) {
			t.Errorf("rolled-back page lacks %q", want)
		}
	}
	c.do("POST", "/server/update/back", url.Values{"csrf": {tok}})
	c.do("POST", "/server/update/check", url.Values{"csrf": {tok}})
	if strings.Join(e.agent.updCalls, ",") != "apply v0.2.0,back,check" {
		t.Errorf("calls = %v", e.agent.updCalls)
	}

	// A plan that needs a terminal: no button, the SSH command.
	e.agent.upd.Ready.Plan.Interactive = []string{"SSH ayarları"}
	_, body = c.do("GET", "/server/update", nil)
	if strings.Contains(body, "sürümüne güncelle</button>") || !strings.Contains(body, "sudo footops self-update") || !strings.Contains(body, "(SSH ayarları)") {
		t.Error("interactive plan offered in the panel")
	}
	// A refusal comes back as a message.
	e.agent.updErr = &agent.Error{Status: 422, Message: "bu sürüm hazır değil"}
	c.do("POST", "/server/update/apply", url.Values{"csrf": {tok}, "version": {"v0.2.0"}})
	if _, body := c.do("GET", "/server/update", nil); !strings.Contains(body, "Güncelleme başlatılamadı: bu sürüm hazır değil") {
		t.Error("refusal not shown")
	}
	// A signature that does not hold is said loudly.
	e.agent.upd.Unverified = "sürüm doğrulanamadı: imza tutmuyor"
	if _, body := c.do("GET", "/server/update", nil); !strings.Contains(body, "imzası tutmuyor; kurulmadı") {
		t.Error("unverified release not shown")
	}
	// No CSRF token, no update.
	n := len(e.agent.updCalls)
	c.do("POST", "/server/update/apply", url.Values{"version": {"v0.2.0"}})
	if len(e.agent.updCalls) != n {
		t.Error("applied without the CSRF token")
	}
}
