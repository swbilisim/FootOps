package web

import (
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/history"
)

func TestRenderChart(t *testing.T) {
	from := time.Unix(0, 0)
	c := chartSpec{Title: "Bellek <b>", Unit: unitBytes, From: from, To: from.Add(10 * time.Minute), Step: 60, Limit: 512 << 20,
		Lines: []chartLine{{Name: `x"><script>`, Pts: []pt{{0, 100 << 20, 120 << 20}, {60, 200 << 20, 210 << 20}, {300, 300 << 20, 300 << 20}, {360, 310 << 20, 320 << 20}}}}}
	out := string(renderChart(c))
	if strings.Contains(out, "<script>") || strings.Contains(out, "<b>") || !strings.Contains(out, "x&#34;&gt;&lt;script&gt;") {
		t.Errorf("names and titles must be escaped:\n%s", out)
	}
	// Two segments around the gap between 60 and 300.
	line := out[strings.Index(out, `class="ln s1" d="`):]
	line = line[:strings.Index(line, `"/>`)]
	if n := strings.Count(line, "M"); n != 2 {
		t.Errorf("line has %d segments, want 2: %s", n, line)
	}
	if !strings.Contains(out, `class="limit"`) || !strings.Contains(out, "512 MiB") {
		t.Error("limit line missing")
	}
	if !strings.Contains(out, "320 MiB") { // the peak, in the legend
		t.Error("peak missing from legend")
	}
	if strings.Contains(out, "style=") {
		t.Error("inline styles are blocked by the CSP")
	}

	empty := string(renderChart(chartSpec{Title: "CPU", Unit: unitPercent, Empty: "Henüz veri yok"}))
	if !strings.Contains(empty, "Henüz veri yok") || strings.Contains(empty, "<svg") {
		t.Errorf("empty chart: %s", empty)
	}
}

func TestNiceMax(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		unit string
		want float64
	}{
		{37, unitPercent, 40}, {130, unitPercent, 200}, {0.2, unitPercent, 1}, {100, unitPercent, 100},
		{300 << 20, unitBytes, 512 << 20}, {3900 << 20, unitBytes, 4 << 30}, {0, unitBytes, 4 << 20},
	} {
		if got := niceMax(tc.v, tc.unit); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("niceMax(%v, %s) = %v, want %v", tc.v, tc.unit, got, tc.want)
		}
	}
}

func TestContainerLinesFoldsTheRest(t *testing.T) {
	h := agent.History{Containers: map[string][]history.Point{}}
	for i, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		h.Containers[n] = []history.Point{{T: 0, Mem: uint64(i+1) << 20, MemMax: uint64(i+1) << 20}}
	}
	lines := containerLines(h, memPts)
	if len(lines) != maxLines || lines[maxLines-1].Name != "Diğer (3)" {
		t.Fatalf("lines = %d, last %q", len(lines), lines[len(lines)-1].Name)
	}
	// The three smallest (a, b, c: 1+2+3 MiB) are folded; the rest stay in name order.
	if lines[0].Name != "d" || lines[maxLines-1].Pts[0].V != float64(6<<20) {
		t.Errorf("first %q, other %v", lines[0].Name, lines[maxLines-1].Pts)
	}
}

func TestFilterLogs(t *testing.T) {
	lines := []string{"INFO GET / 200", "WARN slow query", "ERROR Mongo down", "error: mongo again"}
	if got := filterLogs(lines, "", "warn"); len(got) != 3 || got[0].Level != "warn" || got[1].Level != "bad" {
		t.Errorf("warn filter: %+v", got)
	}
	if got := filterLogs(lines, "", "error"); len(got) != 2 {
		t.Errorf("error filter: %+v", got)
	}
	got := filterLogs(lines, "MONGO", "")
	if len(got) != 2 {
		t.Fatalf("search: %+v", got)
	}
	p := got[0].Parts
	if len(p) != 3 || p[1].Text != "Mongo" || !p[1].Hit || p[2].Text != " down" {
		t.Errorf("parts = %+v", p)
	}
	if got := filterLogs([]string{"a.b", "axb"}, ".", ""); len(got) != 1 {
		t.Errorf("search must be literal, not a regexp: %+v", got)
	}
}

func TestDashboardChartsAndLightRefresh(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	_, body := c.do("GET", "/?range=7d", nil)
	if !strings.Contains(body, `data-live-id="charts"`) || !strings.Contains(body, "Container bellek") || !strings.Contains(body, `aria-current="true">7 gün`) {
		t.Errorf("dashboard charts missing:\n%s", body)
	}
	if !strings.Contains(body, `data-live="10"`) || !strings.Contains(body, `src="/static/live.js"`) {
		t.Error("dashboard is not live")
	}
	c.header = http.Header{liveHeader: {"1"}}
	_, body = c.do("GET", "/", nil)
	if strings.Contains(body, `data-live-id="charts"`) || !strings.Contains(body, `data-live-id="cards"`) {
		t.Error("a light refresh must leave the charts out and keep the cards")
	}
	c.header = http.Header{liveHeader: {"full"}}
	if _, body = c.do("GET", "/", nil); !strings.Contains(body, `data-live-id="charts"`) {
		t.Error("a full refresh must include the charts")
	}
}

func TestLiveRefreshDoesNotKeepSessionAlive(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	c.header = http.Header{liveHeader: {"1"}}
	for i := 0; i < 4; i++ {
		e.advance(sessionIdle / 4)
		if resp, _ := c.do("GET", "/", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("live refresh %d: %d", i, resp.StatusCode)
		}
	}
	e.advance(time.Second)
	if resp, _ := c.do("GET", "/", nil); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("session outlived the idle limit through background refreshes: %d", resp.StatusCode)
	}
}

func TestLiveRefreshKeepsFlash(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	csrf := e.login(t, c)
	c.do("POST", "/containers/footops-mongo/restart", map[string][]string{"csrf": {csrf}})
	c.header = http.Header{liveHeader: {"1"}}
	if _, body := c.do("GET", "/", nil); strings.Contains(body, "yeniden başlatıldı") {
		t.Error("a background refresh consumed the flash message")
	}
	c.header = nil
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, "yeniden başlatıldı") {
		t.Error("flash message lost")
	}
}

func TestContainerAndLogPages(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	resp, body := c.do("GET", "/containers/footops-mongo", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "çalışıyor") || !strings.Contains(body, "Bellek") {
		t.Errorf("container page: %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := c.do("GET", "/containers/..%2Fetc", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("bad name: %d", resp.StatusCode)
	}

	e.agent.logLines = []string{"INFO ok", "ERROR <b>boom</b>"}
	_, body = c.do("GET", "/containers/footops-mongo/logs?q=boom&level=error&live=0", nil)
	if !strings.Contains(body, "<mark>boom</mark>") || strings.Contains(body, "<b>") || strings.Contains(body, "INFO ok") {
		t.Errorf("filtered logs:\n%s", body)
	}
	if strings.Contains(body, "data-live=") || !strings.Contains(body, "Canlı takip et") {
		t.Error("live=0 must stop following")
	}
	if resp, _ := c.do("GET", "/containers/footops-mongo/logs?q=%ff%fe", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("invalid UTF-8 in the search: %d", resp.StatusCode)
	}
}

func TestAlertsShown(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	if _, body := c.do("GET", "/sites", nil); strings.Contains(body, `class="count`) {
		t.Error("badge without alerts")
	}
	e.agent.alerts = []agent.Alert{
		{Key: "disk", Level: "warn", Title: "Disk %85 dolu", Link: "/"},
		{Key: "down:web", Level: "bad", Title: "<web> çalışmıyor", Link: "/containers/web"},
	}
	_, body := c.do("GET", "/sites", nil)
	if !strings.Contains(body, `<span class="count" title="2 uyarı">2</span>`) {
		t.Errorf("menu badge missing:\n%s", body)
	}
	_, body = c.do("GET", "/", nil)
	if !strings.Contains(body, "Disk %85 dolu") || !strings.Contains(body, "&lt;web&gt; çalışmıyor") || !strings.Contains(body, `href="/containers/web"`) {
		t.Errorf("alert list missing:\n%s", body)
	}
}
