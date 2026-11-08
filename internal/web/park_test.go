package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// fakePark plays the agent's end dates and parking (Faz P1b).
type fakePark struct {
	expires  string
	parked   *sites.ParkState
	set      []string // expiries asked for
	was      []string // the end dates they were asked from
	setErr   error
	parks    int
	unparks  []string
	parkErr  error
	unparkEr error
	// containers are added to the dashboard's.
	containers []agent.ContainerInfo
}

func (f *fakeAgent) SetExpiry(_ context.Context, name, expires string, was *string, _ agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pk.set = append(f.pk.set, expires)
	if was == nil {
		panic("the panel changes an end date without saying from which")
	}
	f.pk.was = append(f.pk.was, *was)
	if f.pk.setErr != nil {
		return f.pk.setErr
	}
	f.pk.expires = expires
	return nil
}

func (f *fakeAgent) ParkSite(context.Context, string, agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pk.parks++
	return "jobpark", f.pk.parkErr
}

func (f *fakeAgent) UnparkSite(_ context.Context, _, expires string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pk.unparks = append(f.pk.unparks, expires)
	return "jobunpark", f.pk.unparkEr
}

func day(d int) string { return time.Now().AddDate(0, 0, d).Format(sites.DayLayout) }

func TestSiteEndDate(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)

	_, body := c.do("GET", "/sites/enisparta", nil)
	for _, want := range []string{`id="sure"`, "Süresiz", `href="/sites/enisparta/park"`, "+1 ay (", "+1 yıl ("} {
		if !strings.Contains(body, want) {
			t.Errorf("site page lacks %q", want)
		}
	}
	if strings.Contains(body, "Süresiz yap") {
		t.Error("no end date to remove")
	}
	if _, body := c.do("GET", "/sites", nil); !strings.Contains(body, "süresiz") {
		t.Error("list: no end date not said")
	}

	e.agent.pk.expires = day(3)
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, dayTR(day(3))) || !strings.Contains(body, `badge warn">3 gün kaldı`) || !strings.Contains(body, "Süresiz yap") ||
		!strings.Contains(body, `name="was" value="`+day(3)+`"`) {
		t.Errorf("3 days left not shown:\n%s", body)
	}
	if _, body := c.do("GET", "/sites", nil); !strings.Contains(body, `badge warn">3 gün kaldı`) {
		t.Error("list: days left not shown")
	}
	e.agent.pk.expires = day(-2)
	if _, body := c.do("GET", "/sites", nil); !strings.Contains(body, `badge bad">2 gün önce doldu`) {
		t.Error("list: passed end date not shown")
	}

	post := func(v url.Values) (*http.Response, string) {
		v.Set("csrf", tok)
		return c.do("POST", "/sites/enisparta/expiry", v)
	}
	// +1 month from a passed end date counts from today.
	resp, _ := post(url.Values{"act": {"month"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/enisparta#sure" {
		t.Fatalf("month: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if want := sites.Extend(day(-2), 1, time.Now()); e.agent.pk.set[0] != want {
		t.Errorf("month: %v, want %s", e.agent.pk.set, want)
	}
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, "Bitiş tarihi: "+dayTR(e.agent.pk.set[0])) {
		t.Error("no flash after a change")
	}
	post(url.Values{"act": {"set"}, "expires": {"2027-03-31"}})
	post(url.Values{"act": {"year"}})
	post(url.Values{"act": {"none"}})
	if got := strings.Join(e.agent.pk.set[1:], ","); got != "2027-03-31,2028-03-31," {
		t.Errorf("set/year/none: %s", got)
	}
	// From the end date the page showed (was), not whatever is stored by then: a second
	// click is the agent's to refuse.
	n0 := len(e.agent.pk.set)
	post(url.Values{"act": {"month"}, "was": {"2027-01-31"}})
	if e.agent.pk.set[n0] != "2027-02-28" || e.agent.pk.was[n0] != "2027-01-31" {
		t.Errorf("from was: %s from %s", e.agent.pk.set[n0], e.agent.pk.was[n0])
	}
	// An empty date asks nothing of the agent; a refused one says why.
	n := len(e.agent.pk.set)
	post(url.Values{"act": {"set"}, "expires": {""}})
	if len(e.agent.pk.set) != n {
		t.Error("empty date sent")
	}
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, "Bir tarih seç") {
		t.Error("empty date not said")
	}
	e.agent.pk.setErr = &agent.Error{Status: 422, Message: "site tanımında sorunlar var", Problems: []string{"bitiş tarihi: 2020 ile 2100 arasında olmalı"}}
	post(url.Values{"act": {"set"}, "expires": {"1999-01-01"}})
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, "2020 ile 2100 arasında olmalı") {
		t.Error("refusal not shown")
	}
	if resp, _ := post(url.Values{"act": {"sil"}}); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown act: %d", resp.StatusCode)
	}
	// Without the session's token nothing happens.
	n = len(e.agent.pk.set)
	if resp, _ := c.do("POST", "/sites/enisparta/expiry", url.Values{"act": {"none"}}); resp.StatusCode == http.StatusSeeOther || len(e.agent.pk.set) != n {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
}

func TestSitePark(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)

	_, body := c.do("GET", "/sites/enisparta/park", nil)
	for _, want := range []string{"enisparta parka alınsın mı?", "bakım sayfası", "HTTP 503", `action="/sites/enisparta/park"`} {
		if !strings.Contains(body, want) {
			t.Errorf("confirm lacks %q", want)
		}
	}
	resp, _ := c.do("POST", "/sites/enisparta/park", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobpark" || e.agent.pk.parks != 1 {
		t.Fatalf("park: %d %s %d", resp.StatusCode, resp.Header.Get("Location"), e.agent.pk.parks)
	}
	e.agent.pk.parkErr = &agent.Error{Status: 409, Message: "başka bir işlem sürüyor"}
	resp, _ = c.do("POST", "/sites/enisparta/park", url.Values{"csrf": {tok}})
	if resp.Header.Get("Location") != "/sites/enisparta" {
		t.Errorf("refused park: %s", resp.Header.Get("Location"))
	}
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, "başka bir işlem sürüyor") {
		t.Error("refusal not shown")
	}

	// Parked: the page says so, offers Yayına al, hides what would start the container.
	e.agent.pk.parked = &sites.ParkState{At: time.Now().Add(-48 * time.Hour), By: "furkan"}
	e.agent.pk.expires = day(-3)
	_, body = c.do("GET", "/sites/enisparta", nil)
	for _, want := range []string{"Bu site parkta", "furkan", `href="/sites/enisparta/unpark"`, "Yayına al…", "yayına alırken"} {
		if !strings.Contains(body, want) {
			t.Errorf("parked page lacks %q", want)
		}
	}
	for _, not := range []string{`/redeploy"`, `/edit"`, `/restart"`, `/move"`, `href="/sites/enisparta/park"`, `action="/sites/enisparta/expiry"`} {
		if strings.Contains(body, not) {
			t.Errorf("parked page offers %q", not)
		}
	}
	if _, body := c.do("GET", "/sites", nil); !strings.Contains(body, `badge warn">parkta`) {
		t.Error("list: parked not shown")
	}
	// Its container's pages offer no restart.
	if _, body := c.do("GET", "/containers/enisparta", nil); !strings.Contains(body, "parktaki") || strings.Contains(body, `href="/containers/enisparta/restart"`) {
		t.Error("container page offers a restart")
	}
	if _, body := c.do("GET", "/containers/enisparta/restart", nil); !strings.Contains(body, "yeniden başlatılamaz") || strings.Contains(body, `action="/containers/enisparta/restart"`) {
		t.Error("restart page offers a restart")
	}
	// A park left half-done (its container still running) can be finished.
	e.agent.pk.containers = []agent.ContainerInfo{{Name: "enisparta", State: "running", Project: "enisparta"}}
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, "Park yarım kalmış") || !strings.Contains(body, `href="/sites/enisparta/park"`) {
		t.Error("a half-done park is not offered to finish")
	}
	if _, body := c.do("GET", "/sites/enisparta/park", nil); !strings.Contains(body, "parkı tamamlansın mı?") {
		t.Error("finishing a park: wrong question")
	}
	e.agent.pk.containers = []agent.ContainerInfo{{Name: "enisparta", Image: "ghcr.io/x/enisparta:main", State: "exited", Project: "enisparta"}}
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, `badge warn">parkta`) || strings.Contains(body, `href="/containers/enisparta/restart"`) {
		t.Error("dashboard: a parked site's container shown as stopped, or offered a restart")
	}

	_, body = c.do("GET", "/sites/enisparta/unpark", nil)
	year := sites.Extend(day(-3), 12, time.Now())
	for _, want := range []string{"yeniden yayına alınsın mı?", "&#43;1 yıl → " + dayTR(year), "Süresiz", `value="year" checked`, "Eski bitiş tarihi: " + dayTR(day(-3))} {
		if !strings.Contains(body, want) {
			t.Errorf("unpark form lacks %q", want)
		}
	}
	unpark := func(v url.Values) (*http.Response, string) {
		v.Set("csrf", tok)
		return c.do("POST", "/sites/enisparta/unpark", v)
	}
	resp, _ = unpark(url.Values{"choice": {"year"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobunpark" || e.agent.pk.unparks[0] != year {
		t.Fatalf("unpark: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), e.agent.pk.unparks)
	}
	unpark(url.Values{"choice": {"none"}})
	unpark(url.Values{"choice": {"set"}, "expires": {"2027-01-15"}})
	if got := strings.Join(e.agent.pk.unparks[1:], ","); got != ",2027-01-15" {
		t.Errorf("none/set: %q", got)
	}
	n := len(e.agent.pk.unparks)
	if resp, body := unpark(url.Values{"choice": {"set"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Bir tarih seç") {
		t.Errorf("empty date: %d", resp.StatusCode)
	}
	if resp, _ := unpark(url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity || len(e.agent.pk.unparks) != n {
		t.Errorf("no choice: %d", resp.StatusCode)
	}
	e.agent.pk.unparkEr = &agent.Error{Status: 409, Message: "site parkta değil"}
	if resp, body := unpark(url.Values{"choice": {"none"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "site parkta değil") {
		t.Errorf("refused unpark: %d", resp.StatusCode)
	}
}

func TestNotifyListening(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	e.agent.nt.status = agent.NotifyStatus{Linked: true, Bot: "OrnekBot", Chat: "Ayşe", Since: time.Now(),
		Commands: &agent.CommandsStatus{User: "Ayşe (@ayse)", Listening: true}}
	_, body := c.do("GET", "/notify", nil)
	if !strings.Contains(body, "Bot dinleniyor.") || strings.Contains(body, "henüz okunmadı") {
		t.Errorf("first read waiting:\n%s", body)
	}
	if !strings.Contains(body, "/siteler") {
		t.Error("commands do not name /siteler")
	}
}
