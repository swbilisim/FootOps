package agent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// expiryFixture is a linked chat and a store of sites, with one clock for both.
type expiryFixture struct {
	*notifyFixture
	st sites.Store
	e  *Expiry
}

func newExpiryFixture(t *testing.T) *expiryFixture {
	t.Helper()
	f := &expiryFixture{notifyFixture: readFixture(t)}
	f.st = sites.Store{Root: f.dir}
	f.e = f.expiry()
	return f
}

// expiry is an Expiry on the fixture's files, as after an agent (re)start.
func (f *expiryFixture) expiry() *Expiry {
	e := &Expiry{Sites: &SiteOps{Store: f.st}, Notify: f.n, StatePath: filepath.Join(f.dir, "expiry.json"), Log: log.New(f.logs, "", 0)}
	e.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	return e
}

func (f *expiryFixture) site(t *testing.T, name, expires string, parked bool) {
	t.Helper()
	s := sites.Spec{Name: name, Group: "musteri", Domains: []string{name + ".com", "www." + name + ".com"}, Expires: expires}
	if parked {
		s.Parked = &sites.ParkState{At: time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local), By: "furkan"}
	}
	if err := f.st.WriteMeta(s); err != nil {
		t.Fatal(err)
	}
	f.e.Sites.dropTerms()
}

// check looks at the end dates at the given time; it returns what the chat was told.
func (f *expiryFixture) check(at time.Time) []string {
	f.set(at)
	f.clearSent()
	f.e.Sites.dropTerms()
	f.e.Check(context.Background())
	return f.tg.texts()
}

// labels are the buttons under a sent message.
func labels(t *testing.T, m map[string][]string) string {
	t.Helper()
	var out []string
	for _, b := range keyboard(t, m) {
		out = append(out, b.Text)
	}
	return strings.Join(out, " | ")
}

var (
	monday = time.Date(2026, 11, 9, 10, 30, 0, 0, time.Local)
	dayOf  = func(t time.Time) string { return t.Format(sites.DayLayout) }
)

func TestExpiryReminders(t *testing.T) {
	f := newExpiryFixture(t)
	end := monday.AddDate(0, 0, 10) // Thursday 19.11.2026
	f.site(t, "magaza", dayOf(end), false)
	f.site(t, "blog", "", false) // no end: never a word
	if got := f.check(monday); len(got) != 0 {
		t.Fatalf("10 days before: %v", got)
	}
	// A week before: once.
	got := f.check(monday.AddDate(0, 0, 3))
	if len(got) != 1 || !strings.HasPrefix(got[0], "⏳ [H1] magaza sitesinin süresi 7 gün sonra doluyor: 19.11.2026 Perşembe.\nmagaza.com, www.magaza.com\nhttps://panel.example.com/sites/magaza") {
		t.Fatalf("7 days: %q", got)
	}
	if l := labels(t, f.sentForms()[0]); l != "+1 ay | +1 yıl" {
		t.Errorf("7 days' buttons: %s", l)
	}
	for _, at := range []time.Time{monday.AddDate(0, 0, 3).Add(3 * time.Hour), monday.AddDate(0, 0, 5)} {
		if got := f.check(at); len(got) != 0 {
			t.Fatalf("told twice: %v", got)
		}
	}
	// Three days before — not before 10:00.
	d3 := monday.AddDate(0, 0, 7)
	if got := f.check(time.Date(d3.Year(), d3.Month(), d3.Day(), 9, 59, 0, 0, time.Local)); len(got) != 0 {
		t.Fatalf("before 10:00: %v", got)
	}
	if got := f.check(time.Date(d3.Year(), d3.Month(), d3.Day(), 10, 0, 0, 0, time.Local)); len(got) != 1 || !strings.Contains(got[0], "süresi 3 gün sonra doluyor") {
		t.Fatalf("3 days: %v", got)
	}
	// The day: the question.
	got = f.check(end)
	if len(got) != 1 || !strings.HasPrefix(got[0], "⌛ [H1] magaza sitesinin süresi bugün doluyor (19.11.2026 Perşembe). Parka alınsın mı?") ||
		!strings.Contains(got[0], "Onay gelmezse site açık kalır, her gün 10:00'da yeniden sorulur.") {
		t.Fatalf("the day: %q", got)
	}
	if l := labels(t, f.sentForms()[0]); l != "⏸ Parka al | +1 ay | +1 yıl" {
		t.Errorf("the day's buttons: %s", l)
	}
	if got := f.check(end.Add(5 * time.Hour)); len(got) != 0 {
		t.Fatalf("asked twice the same day: %v", got)
	}
	// Every day after, once — the site stays up meanwhile.
	for d := 1; d <= 2; d++ {
		at := end.AddDate(0, 0, d)
		got := f.check(at)
		if len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("süresi %d gün önce doldu (19.11.2026); site hâlâ açık. Parka alınsın mı?", d)) {
			t.Fatalf("day %d after: %v", d, got)
		}
		if got := f.check(at.Add(2 * time.Hour)); len(got) != 0 {
			t.Fatalf("day %d asked twice: %v", d, got)
		}
	}
	// After a restart nothing is told twice.
	f.e = f.expiry()
	if got := f.check(end.AddDate(0, 0, 2).Add(3 * time.Hour)); len(got) != 0 {
		t.Fatalf("after a restart: %v", got)
	}
	// Parked: nothing more, and its record is forgotten (a new end date starts afresh).
	f.site(t, "magaza", dayOf(end), true)
	if got := f.check(end.AddDate(0, 0, 3)); len(got) != 0 {
		t.Fatalf("parked: %v", got)
	}
	if _, ok := f.e.state().Sites["magaza"]; ok {
		t.Error("a parked site's record is kept")
	}
}

func TestExpiryNewDates(t *testing.T) {
	f := newExpiryFixture(t)
	// Set five days before: the week's reminder counts as told — the operator just
	// decided —, the three days' comes.
	f.site(t, "bes", dayOf(monday.AddDate(0, 0, 5)), false)
	if got := f.check(monday); len(got) != 0 {
		t.Fatalf("set 5 days before: %v", got)
	}
	if got := f.check(monday.AddDate(0, 0, 2)); len(got) != 1 || !strings.Contains(got[0], "bes sitesinin süresi 3 gün sonra") {
		t.Fatalf("3 days: %v", got)
	}
	// Set to today, or to a day past: the question comes at once.
	f.site(t, "bugun", dayOf(monday.AddDate(0, 0, 2)), false)
	f.site(t, "gecmis", dayOf(monday), false)
	got := f.check(monday.AddDate(0, 0, 2).Add(time.Hour))
	if len(got) != 2 || !strings.Contains(strings.Join(got, "\n"), "bugun sitesinin süresi bugün doluyor") ||
		!strings.Contains(strings.Join(got, "\n"), "gecmis sitesinin süresi 2 gün önce doldu") {
		t.Fatalf("today / past: %v", got)
	}
	// Given more time: told afresh from the new date (the past one is asked every day).
	f.site(t, "bugun", dayOf(monday.AddDate(0, 0, 40)), false)
	if got := f.check(monday.AddDate(0, 0, 3)); len(got) != 1 || !strings.Contains(got[0], "gecmis sitesinin süresi 3 gün önce doldu") {
		t.Fatalf("after more time: %v", got)
	}
	if got := f.check(monday.AddDate(0, 0, 33)); !strings.Contains(strings.Join(got, "\n"), "bugun sitesinin süresi 7 gün sonra") {
		t.Fatalf("the new date's week: %v", got)
	}
	// A removed site's record goes.
	if _, ok := f.e.state().Sites["bes"]; !ok {
		t.Fatal("no record")
	}
	if err := os.RemoveAll(filepath.Join(f.dir, "srv/musteri/bes")); err != nil {
		t.Fatal(err)
	}
	f.check(monday.AddDate(0, 0, 34))
	if _, ok := f.e.state().Sites["bes"]; ok {
		t.Error("a removed site's record is kept")
	}
}

func TestExpiryNotTold(t *testing.T) {
	f := newExpiryFixture(t)
	f.site(t, "magaza", dayOf(monday), false)
	// Telegram refusing: not told, tried again in half an hour.
	f.tg.setFail(500, "Internal Server Error", 0)
	if got := f.check(monday); len(got) != 0 {
		t.Fatalf("sent: %v", got)
	}
	if w := f.e.nextWake(monday); w != remindRetry {
		t.Errorf("retry in %s", w)
	}
	f.tg.setFail(0, "", 0)
	if got := f.check(monday.Add(remindRetry)); len(got) != 1 {
		t.Fatalf("after the retry: %v", got)
	}
	if w := f.e.nextWake(monday.Add(remindRetry)); w != 23*time.Hour {
		t.Errorf("next look in %s", w)
	}
	// No chat: nothing, and no retry (linking one kicks a look: TestLinkTellsWhatIsOwed).
	if err := f.n.Remove(); err != nil {
		t.Fatal(err)
	}
	f.site(t, "blog", dayOf(monday), false)
	if got := f.check(monday.AddDate(0, 0, 1)); len(got) != 0 {
		t.Fatalf("no chat: %v", got)
	}
	if !f.e.retryAt.IsZero() || f.e.state().Sites["blog"].Said == len(remindStages) {
		t.Error("a message that never went counts as told")
	}
	// The day's look: 10:00, also from earlier that day.
	if w := f.e.nextWake(time.Date(2026, 11, 9, 9, 0, 0, 0, time.Local)); w != time.Hour {
		t.Errorf("09:00: %s", w)
	}
	if w := f.e.nextWake(time.Date(2026, 11, 9, 10, 0, 0, 0, time.Local)); w != 24*time.Hour {
		t.Errorf("10:00: %s", w)
	}
}

func TestExpiryAlerts(t *testing.T) {
	f := newExpiryFixture(t)
	f.site(t, "gecti", dayOf(monday.AddDate(0, 0, -2)), false)
	f.site(t, "bugun", dayOf(monday), false)
	f.site(t, "parkta", dayOf(monday.AddDate(0, 0, -9)), true)
	f.site(t, "yakin", dayOf(monday.AddDate(0, 0, 2)), false)
	got := f.e.Alerts(monday)
	if alertKeys(got) != "expiry:bugun=warn expiry:gecti=warn" {
		t.Fatalf("alerts: %s", alertKeys(got))
	}
	if got[0].Title != "bugun sitesinin süresi bugün doluyor" || got[1].Title != "gecti sitesinin süresi 2 gün önce doldu (07.11.2026)" ||
		got[1].Link != "/sites/gecti" || !got[1].Since.Equal(time.Date(2026, 11, 7, 0, 0, 0, 0, time.Local)) {
		t.Errorf("alert: %+v", got)
	}
	for _, a := range got {
		if Notifiable(a) {
			t.Errorf("%s goes to the chat besides its own message", a.Key)
		}
	}
}

func TestExpiryJobs(t *testing.T) {
	e := parkOps(t)
	var mu sync.Mutex
	var changed int
	var done []Job
	x := &Expiry{Sites: e.ops, Jobs: NewJobs(), Audit: NewAudit(filepath.Join(e.root, "audit.jsonl")),
		Changed: func() { mu.Lock(); changed++; mu.Unlock() }}
	actor := Actor{User: "Ayşe", Via: "telegram"}
	id, err := x.StartPark("enisparta", actor, func(j Job) { mu.Lock(); done = append(done, j); mu.Unlock() })
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(done) == 1 })
	if j, _ := x.Jobs.Get(id); j.Kind != "site.park" || j.Err != "" || done[0].ID != id || changed != 1 {
		t.Fatalf("job %+v, changed %d", j, changed)
	}
	if s, _ := e.ops.Store.Get("enisparta"); !s.IsParked() || s.Parked.By != "Ayşe" {
		t.Fatalf("not parked: %+v", s.Parked)
	}
	if _, err := x.StartUnpark("enisparta", "2027-02-01", actor, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return changed == 2 })
	if s, _ := e.ops.Store.Get("enisparta"); s.IsParked() || s.Expires != "2027-02-01" {
		t.Fatalf("not back: %+v", s)
	}
	if err := x.SetExpiry("enisparta", "2027-03-01", nil, actor); err != nil {
		t.Fatal(err)
	}
	entries, _ := x.Audit.Tail(10)
	var acts []string
	for _, a := range entries {
		acts = append(acts, a.Action+" "+a.Actor.Via+" "+a.Detail)
	}
	if got := strings.Join(acts, "; "); !strings.Contains(got, "site.park telegram ") || !strings.Contains(got, "site.unpark telegram ") ||
		!strings.Contains(got, "site.expiry telegram bitiş 01.03.2027") {
		t.Fatalf("audit: %s", got)
	}
}

func TestExpiryUnreadable(t *testing.T) {
	f := newExpiryFixture(t)
	f.site(t, "magaza", dayOf(monday), false)
	// One broken footops.json fails the whole list: tried again soon, said in the panel.
	broken := filepath.Join(f.dir, "srv/musteri/bozuk/footops.json")
	os.MkdirAll(filepath.Dir(broken), 0o755)
	os.WriteFile(broken, []byte("{"), 0o600)
	if got := f.check(monday); len(got) != 0 {
		t.Fatalf("told with the list unreadable: %v", got)
	}
	if w := f.e.nextWake(monday); w != remindRetry {
		t.Errorf("retry in %s", w)
	}
	got := f.e.Alerts(monday)
	if len(got) == 0 || got[0].Key != "expiry" || !strings.Contains(got[0].Detail, "bozuk") || Notifiable(got[0]) {
		t.Fatalf("alerts: %+v", got)
	}
	os.Remove(broken)
	if got := f.check(monday.Add(remindRetry)); len(got) != 1 {
		t.Fatalf("after the fix: %v", got)
	}
	if got := f.e.Alerts(monday.Add(remindRetry)); alertKeys(got) != "expiry:magaza=warn" {
		t.Errorf("alerts after the fix: %s", alertKeys(got))
	}
}
