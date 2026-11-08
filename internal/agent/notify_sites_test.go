package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// chatSites plays what the chat can do with sites.
type chatSites struct {
	mu       sync.Mutex
	terms    map[string]SiteTerm
	sets     []string // site=to
	setErr   error
	parks    []string
	unparks  []string // site=to
	startErr error
	done     func(Job)
	actors   []Actor
}

func (f *chatSites) Term(site string) (SiteTerm, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.terms[site]
	return t, ok
}

func (f *chatSites) Terms() ([]SiteTerm, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []SiteTerm
	for _, t := range f.terms {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *chatSites) SetExpiry(site, to string, was *string, a Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets, f.actors = append(f.sets, site+"="+to), append(f.actors, a)
	if f.setErr != nil {
		return f.setErr
	}
	t := f.terms[site]
	if was == nil {
		panic("the chat changes an end date without saying from which")
	}
	if *was != t.Expires {
		return errExpiryChanged
	}
	t.Expires = to
	f.terms[site] = t
	return nil
}

func (f *chatSites) StartPark(site string, a Actor, done func(Job)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.parks, f.actors, f.done = append(f.parks, site), append(f.actors, a), done
	return "job1", nil
}

func (f *chatSites) StartUnpark(site, to string, a Actor, done func(Job)) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	f.unparks, f.actors, f.done = append(f.unparks, site+"="+to), append(f.actors, a), done
	return "job2", nil
}

func (f *chatSites) set(t SiteTerm) {
	f.mu.Lock()
	f.terms[t.Name] = t
	f.mu.Unlock()
}

// finish ends the last started job as the jobs would.
func (f *chatSites) finish(j Job) {
	f.mu.Lock()
	done := f.done
	f.mu.Unlock()
	done(j)
}

type kbButton struct{ Text, Data string }

// keyboard is every button under a sent or edited message, row after row.
func keyboard(t *testing.T, m url.Values) []kbButton {
	t.Helper()
	var mk struct {
		Keyboard [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(m.Get("reply_markup")), &mk); err != nil {
		t.Fatalf("no buttons: %q (%v)", m.Get("reply_markup"), err)
	}
	var out []kbButton
	for _, row := range mk.Keyboard {
		for _, b := range row {
			out = append(out, kbButton{b.Text, b.Data})
		}
	}
	return out
}

// siteChat is a linked, live reader whose sites are chatSites; the clock is a Thursday.
type siteChat struct {
	*notifyFixture
	fs *chatSites
}

var thursday = time.Date(2026, 11, 12, 10, 0, 0, 0, time.Local)

func newSiteChat(t *testing.T, terms ...SiteTerm) *siteChat {
	t.Helper()
	f := &siteChat{notifyFixture: readFixture(t), fs: &chatSites{terms: map[string]SiteTerm{}}}
	for _, x := range terms {
		f.fs.terms[x.Name] = x
	}
	f.n.Sites = f.fs
	f.set(thursday)
	f.n.readOnce(context.Background()) // live: presses come as they happen
	f.clearSent()
	return f
}

// press presses data under the message msg as the commander and lets the reader see it.
func (f *siteChat) press(msg int64, data string) {
	f.tg.press(555, 555, msg, data)
	f.n.readOnce(context.Background())
}

// last is the newest sent message and its id.
func (f *siteChat) last(t *testing.T) (url.Values, int64) {
	t.Helper()
	f.tg.mu.Lock()
	defer f.tg.mu.Unlock()
	if len(f.tg.sent) == 0 {
		t.Fatal("nothing sent")
	}
	return f.tg.sent[len(f.tg.sent)-1], int64(100 + f.tg.nextMsg)
}

func (f *siteChat) lastEdit(t *testing.T) url.Values {
	t.Helper()
	f.tg.mu.Lock()
	defer f.tg.mu.Unlock()
	if len(f.tg.edits) == 0 {
		t.Fatal("nothing edited")
	}
	return f.tg.edits[len(f.tg.edits)-1]
}

func (f *siteChat) lastAnswer() string {
	f.tg.mu.Lock()
	defer f.tg.mu.Unlock()
	if len(f.tg.answers) == 0 {
		return ""
	}
	return f.tg.answers[len(f.tg.answers)-1].Get("text")
}

func (f *siteChat) sentCount() int {
	f.tg.mu.Lock()
	defer f.tg.mu.Unlock()
	return len(f.tg.sent)
}

func TestSiteButtonsExtend(t *testing.T) {
	ctx := context.Background()
	f := newSiteChat(t, SiteTerm{Name: "magaza", Domains: []string{"magaza.com"}, Expires: "2026-11-19"})
	if err := f.n.SendSiteNotice(ctx, f.fs.terms["magaza"], 7, thursday); err != nil {
		t.Fatal(err)
	}
	m, msg := f.last(t)
	kb := keyboard(t, m)
	if len(kb) != 2 || kb[0].Text != "+1 ay" || kb[1].Text != "+1 yıl" {
		t.Fatalf("buttons: %+v", kb)
	}
	// Someone else: the press only stops spinning.
	f.tg.press(555, 999, msg, kb[0].Data)
	f.n.readOnce(ctx)
	if f.sentCount() != 1 || f.lastAnswer() != "" {
		t.Fatalf("a stranger was answered: %d %q", f.sentCount(), f.lastAnswer())
	}
	// +1 ay: asked once more, with what it does.
	f.press(msg, kb[0].Data)
	q, qmsg := f.last(t)
	if q.Get("text") != "❓ [H1] magaza sitesinin süresi uzatılsın mı?\nŞu anki bitiş: 19.11.2026 → yeni bitiş: 19.12.2026 Cumartesi (+1 ay).\nBu onay 5 dakika geçerli." ||
		f.lastAnswer() != "Onayın bekleniyor" || len(f.fs.sets) != 0 {
		t.Fatalf("question: %q (%q) %v", q.Get("text"), f.lastAnswer(), f.fs.sets)
	}
	qb := keyboard(t, q)
	if len(qb) != 2 || qb[0].Text != "✅ Evet, uzat" || !strings.HasSuffix(qb[0].Data, "/y") || qb[1].Text != "Vazgeç" || !strings.HasSuffix(qb[1].Data, "/n") ||
		strings.TrimSuffix(qb[0].Data, "/y") != strings.TrimSuffix(qb[1].Data, "/n") {
		t.Fatalf("answers: %+v", qb)
	}
	// Vazgeç: nothing done; the question's [Evet] is used up with it.
	f.press(qmsg, qb[1].Data)
	if e := f.lastEdit(t); e.Get("text") != "Vazgeçildi: magaza için bir şey yapılmadı." || e.Has("reply_markup") || len(f.fs.sets) != 0 {
		t.Fatalf("cancel: %v %v", e, f.fs.sets)
	}
	f.press(qmsg, qb[0].Data)
	if len(f.fs.sets) != 0 || !strings.Contains(f.lastAnswer(), "süresi doldu") {
		t.Fatalf("a used question acted: %v %q", f.fs.sets, f.lastAnswer())
	}
	// The proposal works again; this time yes.
	f.press(msg, kb[0].Data)
	q, qmsg = f.last(t)
	f.press(qmsg, keyboard(t, q)[0].Data)
	if strings.Join(f.fs.sets, ",") != "magaza=2026-12-19" || f.fs.actors[0] != (Actor{User: "Ayşe", Via: "telegram"}) {
		t.Fatalf("extended: %v %+v", f.fs.sets, f.fs.actors)
	}
	if e := f.lastEdit(t); e.Get("text") != "✅ [H1] magaza: yeni bitiş 19.12.2026 Cumartesi (+1 ay)." || f.lastAnswer() != "Uzatıldı" {
		t.Fatalf("result: %q %q", e.Get("text"), f.lastAnswer())
	}
	// The old message's other button is stale now: no second extension.
	n := f.sentCount()
	f.press(msg, kb[1].Data)
	if f.sentCount() != n || !strings.Contains(f.lastAnswer(), "Bu düğme eski") {
		t.Fatalf("stale press: %d %q", f.sentCount(), f.lastAnswer())
	}
	// A refusal that can pass asks again in place.
	f.fs.set(SiteTerm{Name: "magaza", Domains: []string{"magaza.com"}, Expires: "2026-11-19"})
	f.press(msg, kb[1].Data)
	q, qmsg = f.last(t)
	if !strings.Contains(q.Get("text"), "yeni bitiş: 19.11.2027 Cuma (+1 yıl)") {
		t.Fatalf("+1 yıl: %q", q.Get("text"))
	}
	f.fs.setErr = errSiteBusy
	f.press(qmsg, keyboard(t, q)[0].Data)
	e := f.lastEdit(t)
	again := keyboard(t, e)
	if !strings.HasPrefix(e.Get("text"), "⏳ Şu an bu sunucuda bir site işlemi sürüyor") || len(again) != 2 || again[0].Data == keyboard(t, q)[0].Data {
		t.Fatalf("busy: %q %+v", e.Get("text"), again)
	}
	f.fs.setErr = nil
	f.press(qmsg, again[0].Data)
	if f.fs.sets[len(f.fs.sets)-1] != "magaza=2027-11-19" {
		t.Fatalf("after busy: %v", f.fs.sets)
	}
}

func TestSiteButtonsPark(t *testing.T) {
	ctx := context.Background()
	f := newSiteChat(t, SiteTerm{Name: "magaza", Domains: []string{"magaza.com", "www.magaza.com"}, Expires: "2026-11-12"})
	if err := f.n.SendSiteNotice(ctx, f.fs.terms["magaza"], 0, thursday); err != nil {
		t.Fatal(err)
	}
	m, msg := f.last(t)
	kb := keyboard(t, m)
	if len(kb) != 3 || kb[0].Text != "⏸ Parka al" {
		t.Fatalf("buttons: %+v", kb)
	}
	f.press(msg, kb[0].Data)
	q, qmsg := f.last(t)
	if !strings.HasPrefix(q.Get("text"), "❓ [H1] magaza parka alınsın mı?\nContainer durur; magaza.com, www.magaza.com bakım sayfası gösterir (HTTP 503).") ||
		keyboard(t, q)[0].Text != "⏸ Evet, parka al" {
		t.Fatalf("question: %q", q.Get("text"))
	}
	// An answer after five minutes is no answer.
	f.set(thursday.Add(confirmTTL + time.Second))
	f.press(qmsg, keyboard(t, q)[0].Data)
	if len(f.fs.parks) != 0 || !strings.Contains(f.lastAnswer(), "süresi doldu") {
		t.Fatalf("late answer: %v %q", f.fs.parks, f.lastAnswer())
	}
	// Busy (the night backup): asked again in place, with a fresh [Evet].
	f.press(msg, kb[0].Data)
	q, qmsg = f.last(t)
	f.fs.startErr = ErrBusy
	f.press(qmsg, keyboard(t, q)[0].Data)
	e := f.lastEdit(t)
	if !strings.HasPrefix(e.Get("text"), "⏳ Şu an başka bir işlem sürüyor") || len(f.fs.parks) != 0 {
		t.Fatalf("busy: %q", e.Get("text"))
	}
	f.fs.startErr = nil
	// Pressed while the reader was away (a restart): may be hours old, so it waits.
	f.n.mu.Lock()
	f.n.rd.live = false
	f.n.mu.Unlock()
	f.press(qmsg, keyboard(t, e)[0].Data)
	if len(f.fs.parks) != 0 || !strings.Contains(f.lastAnswer(), "eski olabilir") {
		t.Fatalf("backlog: %v %q", f.fs.parks, f.lastAnswer())
	}
	f.press(qmsg, keyboard(t, e)[0].Data)
	if strings.Join(f.fs.parks, ",") != "magaza" {
		t.Fatalf("park: %v", f.fs.parks)
	}
	if e := f.lastEdit(t); e.Get("text") != "⏳ [H1] magaza parka alınıyor…" || e.Has("reply_markup") || f.lastAnswer() != "Parka alınıyor…" {
		t.Fatalf("started: %q %q", e.Get("text"), f.lastAnswer())
	}
	// The job ends: the question says so, with what brings the site back.
	f.fs.set(SiteTerm{Name: "magaza", Domains: []string{"magaza.com", "www.magaza.com"}, Expires: "2026-11-12", Parked: &sites.ParkState{At: thursday}})
	f.fs.finish(Job{Kind: "site.park", Target: "magaza"})
	e = f.lastEdit(t)
	back := keyboard(t, e)
	if !strings.HasPrefix(e.Get("text"), "✅ [H1] magaza parka alındı: container durdu") || len(back) != 2 || back[1].Text != "▶ +1 yıl ile aç" {
		t.Fatalf("parked: %q %+v", e.Get("text"), back)
	}
	// Back for a year: asked, then started with the end date the question named.
	f.press(qmsg, back[1].Data)
	q, qmsg = f.last(t)
	if !strings.Contains(q.Get("text"), "magaza yeniden yayına alınsın mı?\nContainer başlar, magaza.com, www.magaza.com yeniden siteyi gösterir; yeni bitiş 12.11.2027 Cuma (+1 yıl).") {
		t.Fatalf("unpark question: %q", q.Get("text"))
	}
	f.press(qmsg, keyboard(t, q)[0].Data)
	if strings.Join(f.fs.unparks, ",") != "magaza=2027-11-12" || f.lastEdit(t).Get("text") != "⏳ [H1] magaza yeniden yayına alınıyor…" {
		t.Fatalf("unpark: %v %q", f.fs.unparks, f.lastEdit(t).Get("text"))
	}
	f.fs.finish(Job{Kind: "site.unpark", Target: "magaza", Err: "container çalışmıyor"})
	if e := f.lastEdit(t); e.Get("text") != "✗ [H1] magaza yayına alınamadı, site parkta kaldı: container çalışmıyor" {
		t.Fatalf("failed unpark: %q", e.Get("text"))
	}
	// The old notice's park button: the site changed since.
	f.press(msg, kb[0].Data)
	if !strings.Contains(f.lastAnswer(), "Bu düğme eski") {
		t.Fatalf("stale park: %q", f.lastAnswer())
	}
}

func TestSitesCommand(t *testing.T) {
	f := newSiteChat(t,
		SiteTerm{Name: "blog", Expires: "2026-11-01", Parked: &sites.ParkState{At: time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local)}},
		SiteTerm{Name: "magaza", Expires: "2026-11-10"},
		SiteTerm{Name: "kasa", Expires: "2026-11-15"},
		SiteTerm{Name: "uzak", Expires: "2027-02-20"},
		SiteTerm{Name: "suresiz"})
	f.tg.say(555, 555, "private", "/siteler", thursday)
	f.n.readOnce(context.Background())
	m, _ := f.last(t)
	if want := "📅 [H1] Süreli ve parktaki siteler · 12.11.2026\n⏸ blog — parkta (02.11.2026 tarihinden beri); bitişi 01.11.2026\n" +
		"⌛ magaza — süresi 2 gün önce doldu (10.11.2026)\n⏳ kasa — 3 gün kaldı (15.11.2026 Pazar)\n• uzak — 100 gün kaldı (20.02.2027)\n" +
		"Süresiz: 1 site\nhttps://panel.example.com/sites"; m.Get("text") != want {
		t.Fatalf("list:\n%s\nwant:\n%s", m.Get("text"), want)
	}
	var got []string
	for _, b := range keyboard(t, m) {
		got = append(got, b.Text)
	}
	if strings.Join(got, " | ") != "▶ blog: +1 ay ile aç | ▶ blog: +1 yıl ile aç | ⏸ magaza: parka al | magaza: +1 ay | magaza: +1 yıl | kasa: +1 ay | kasa: +1 yıl" {
		t.Fatalf("buttons: %s", strings.Join(got, " | "))
	}
	// Nothing with an end date: says where one is given.
	f2 := newSiteChat(t, SiteTerm{Name: "suresiz"})
	f2.tg.say(555, 555, "private", "/siteler", thursday)
	f2.n.readOnce(context.Background())
	if m, _ := f2.last(t); !strings.HasPrefix(m.Get("text"), "📅 [H1] Süreli ya da parktaki site yok.") || m.Has("reply_markup") {
		t.Fatalf("none: %q", m.Get("text"))
	}
	// Help and menu name it.
	f2.tg.say(555, 555, "private", "/yardim", thursday)
	f2.n.readOnce(context.Background())
	if m, _ := f2.last(t); !strings.Contains(m.Get("text"), "/siteler — süreli ve parktaki siteler") {
		t.Fatalf("help: %q", m.Get("text"))
	}
	f2.tg.mu.Lock()
	menus := append([]url.Values(nil), f2.tg.menus...)
	f2.tg.mu.Unlock()
	if len(menus) == 0 || !strings.Contains(menus[0].Get("commands"), `"siteler"`) {
		t.Fatalf("menu: %v", menus)
	}
}

func TestListeningStatus(t *testing.T) {
	f := readFixture(t)
	// Just linked: the first read waits at Telegram — listening, not "not read yet".
	f.n.mu.Lock()
	f.n.rd = readState{token: testBotToken, cancel: func() {}}
	f.n.mu.Unlock()
	if c := f.n.Status().Commands; c == nil || !c.Listening || c.Reading {
		t.Fatalf("first read: %+v", c)
	}
	f.n.readOnce(context.Background())
	if c := f.n.Status().Commands; !c.Reading || c.Listening {
		t.Fatalf("after a read: %+v", c)
	}
}

func TestButtonsEvictQuestionsLast(t *testing.T) {
	f := readFixture(t)
	now := f.n.now()
	// The store full: a question's [Evet] (expiring soonest) and proposals.
	q, err := f.n.newButton(tgButton{Kind: "confirm", Once: true, Chat: 555, Site: "magaza", Act: actPark, Expires: now.Add(confirmTTL)})
	if err != nil {
		t.Fatal(err)
	}
	var many []tgButton
	for i := 0; i < buttonsMax-1; i++ {
		many = append(many, tgButton{Kind: "site", Chat: 555, Site: "magaza", Act: actExtend, Months: 1, Expires: now.Add(buttonTTL + time.Duration(i)*time.Second)})
	}
	first, err := f.n.newButtons(many)
	if err != nil {
		t.Fatal(err)
	}
	// One more: the proposal closest to expiring goes, not the question.
	if _, err := f.n.newButton(tgButton{Kind: "refresh", Chat: 555, Expires: now.Add(buttonTTL)}); err != nil {
		t.Fatal(err)
	}
	f.n.btnMu.Lock()
	_, kept := f.n.buttons[q]
	_, gone := f.n.buttons[first[0]]
	n := len(f.n.buttons)
	f.n.btnMu.Unlock()
	if !kept || gone || n != buttonsMax {
		t.Fatalf("question kept %v, oldest proposal gone %v, %d buttons", kept, !gone, n)
	}
}

func TestLinkTellsWhatIsOwed(t *testing.T) {
	f := newNotifyFixture(t)
	kicked := 0
	f.n.OnLink = func() { kicked++ }
	f.link(t)
	if kicked != 1 {
		t.Fatalf("linking a chat kicked the end dates %d times", kicked)
	}
}
