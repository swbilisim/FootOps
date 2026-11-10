package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/doctor"
)

func (f *fakeAgent) DoctorStatus(context.Context) (agent.DoctorStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.doctor, nil
}

func (f *fakeAgent) DoctorRun(_ context.Context, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.doctorRuns = append(f.doctorRuns, a)
	return f.doctorErr
}

func TestCheckPage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)

	// No run yet: said so; the dashboard's card too.
	resp, body := c.do("GET", "/server/check", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Henüz kontrol yapılmadı") || !strings.Contains(body, `href="/" aria-current="page"`) {
		t.Fatalf("empty page: %d\n%s", resp.StatusCode, body)
	}
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, `href="/server/check"`) || !strings.Contains(body, "henüz yok") {
		t.Error("dashboard card missing")
	}

	at := time.Now().Add(-time.Hour)
	steps := []doctor.Finding{}
	for _, n := range []string{"Sunucu adı", "Saat dilimi", "Takas alanı (swap)", "Docker"} {
		steps = append(steps, doctor.Finding{ID: "step:" + n, Section: "standard", Level: doctor.OK, Title: n})
	}
	e.agent.doctor = agent.DoctorStatus{Report: &doctor.Report{Format: 1, Host: "h1", FootOps: "v0.1.0", Time: at, TookMS: 2100,
		Findings: append(steps,
			doctor.Finding{ID: "step:ssh-hardening", Section: "standard", Level: doctor.Bad, Title: "Standarttan farklı: SSH ayarları",
				Detail: "Kurulumun bu adımı: <SSH> sertleştir.", Fix: "sudo footops install"},
			doctor.Finding{ID: "ports", Section: "network", Level: doctor.Warn, Title: "Beklenmeyen dinleyen port: tcp 8080 (node)"},
			doctor.Finding{ID: "images", Section: "disk", Level: doctor.Info, Title: "2 imajı hiçbir container kullanmıyor (~1,2 GB)"},
			doctor.Finding{ID: "time", Section: "network", Level: doctor.OK, Title: "Saat senkron (NTP)"})},
		Confirmed: map[string]bool{"step:ssh-hardening": true}, Next: time.Now().Add(time.Hour)}
	resp, body = c.do("GET", "/server/check", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("report page: %d\n%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"1 ciddi · 1 uyarı", "1 bilgi", "2.1 sn", "footops v0.1.0",
		"Standarttan farklı: SSH ayarları", "Kurulumun bu adımı: &lt;SSH&gt; sertleştir.", "Düzeltme: sudo footops install",
		"4 adım standartta", // folded
		`<li class="warn">`, "Beklenmeyen dinleyen port: tcp 8080 (node)",
		`title="Bir kontrol daha görürse uyarı olur">yeni</span>`, // the port: seen once
		`<li class="info">`, "Saat senkron (NTP)", "Sonraki kontrol",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "data-live=") {
		t.Error("a finished check refreshes itself")
	}
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, "1 ciddi") || !strings.Contains(body, "1 yeni bulgu") {
		t.Errorf("dashboard card:\n%s", body)
	}

	// Run now: the agent is asked, the page follows the run.
	resp, _ = c.do("POST", "/server/check", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/server/check" || len(e.agent.doctorRuns) != 1 || e.agent.doctorRuns[0].User != "furkan" {
		t.Fatalf("run: %d %v", resp.StatusCode, e.agent.doctorRuns)
	}
	e.agent.doctor.Running = true
	if _, body := c.do("GET", "/server/check", nil); !strings.Contains(body, `data-live="3"`) || !strings.Contains(body, "Kontrol sürüyor") || !strings.Contains(body, " disabled>") {
		t.Error("running check: no live refresh / message / disabled button")
	}
	// Already running: not an error worth a message.
	e.agent.doctorErr = &agent.Error{Status: http.StatusConflict, Message: "kontrol zaten sürüyor"}
	c.do("POST", "/server/check", url.Values{"csrf": {tok}})
	if _, body := c.do("GET", "/server/check", nil); strings.Contains(body, "başlatılamadı") {
		t.Error("conflict shown as a failure")
	}
	e.agent.doctorErr = &agent.Error{Status: http.StatusInternalServerError, Message: "systemd-run yok"}
	c.do("POST", "/server/check", url.Values{"csrf": {tok}})
	if _, body := c.do("GET", "/server/check", nil); !strings.Contains(body, "Kontrol başlatılamadı: systemd-run yok") {
		t.Error("failure not shown")
	}
	// Without the CSRF token nothing runs.
	n := len(e.agent.doctorRuns)
	c.do("POST", "/server/check", url.Values{})
	if len(e.agent.doctorRuns) != n {
		t.Error("ran without the CSRF token")
	}
}
