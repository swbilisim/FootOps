package agent

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// readFixture is a linked notifier (chat 555, linked by user 555) with a report.
func readFixture(t *testing.T) *notifyFixture {
	t.Helper()
	f := newNotifyFixture(t)
	f.n.Report = func(context.Context) ServerReport { return testReport() }
	f.link(t)
	return f
}

func testReport() ServerReport {
	return ServerReport{
		Overview: Overview{SampledAt: time.Date(2026, 11, 7, 11, 59, 50, 0, time.UTC),
			System: SystemMetrics{CPUPercent: 7.4, Load1: 0.21, Load5: 0.18, Load15: 0.15,
				MemTotal: 4 << 30, MemUsed: 2254857830, SwapTotal: 4 << 30, SwapUsed: 300 << 20,
				DiskTotal: 38 << 30, DiskUsed: 21 << 30, Uptime: 12*24*time.Hour + 4*time.Hour},
			Containers: []ContainerInfo{{Name: "magaza", State: "running"}, {Name: "blog", State: "exited"},
				{Name: "kasa", State: "running", Health: "unhealthy"}}},
		Sites: []sites.Spec{{Name: "magaza"}, {Name: "blog"}, {Name: "kasa"}, {Name: "yeni"}},
		Backup: &BackupStatus{Configured: true, Settings: BackupSettings{Confirmed: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
			Last:      map[string]BackupLast{"magaza/db": {Time: time.Date(2026, 11, 7, 3, 2, 0, 0, time.Local)}, "blog/files": {Time: time.Date(2026, 11, 6, 3, 1, 0, 0, time.Local)}},
			NightDone: "2026-11-07"},
		Alerts: []Alert{{Key: "disk", Level: "warn", Title: "Disk %82 dolu"}, {Key: "backup:failed", Level: "bad", Title: "Yedek alınamadı: blog"}},
	}
}

func (f *notifyFixture) sentForms() []url.Values {
	f.tg.mu.Lock()
	defer f.tg.mu.Unlock()
	return append([]url.Values(nil), f.tg.sent...)
}

func (f *notifyFixture) clearSent() {
	f.tg.mu.Lock()
	f.tg.sent, f.tg.edits, f.tg.answers = nil, nil, nil
	f.tg.mu.Unlock()
}

// buttonOf is the callback data of the (single) button under a sent message.
func buttonOf(t *testing.T, m url.Values) string {
	t.Helper()
	var mk struct {
		Keyboard [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(m.Get("reply_markup")), &mk); err != nil || len(mk.Keyboard) != 1 || len(mk.Keyboard[0]) != 1 {
		t.Fatalf("no single button: %q (%v)", m.Get("reply_markup"), err)
	}
	return mk.Keyboard[0][0].Data
}

func TestReadCommands(t *testing.T) {
	f := readFixture(t)
	ctx := context.Background()
	now := f.now
	if c, _ := f.n.config(); c.UserID != 555 || c.User != "Ayşe" {
		t.Fatalf("the commander is not kept: %+v", c)
	}
	// Someone else, a message from before an outage, a command for another bot, and a
	// group's member who is not the commander: no answer at all.
	f.tg.say(999, 999, "private", "/durum", now)
	f.tg.say(555, 555, "private", "/durum", now.Add(-staleCommand-time.Minute))
	f.tg.say(555, 555, "private", "/durum@BaskaBot", now)
	f.tg.say(555, 777, "private", "/durum", now)
	f.n.readOnce(ctx)
	if got := f.sentForms(); len(got) != 0 {
		t.Fatalf("answered what it should not: %v", got)
	}
	if !strings.Contains(f.logs.String(), "bağlı olmayan bir sohbetten") || strings.Contains(f.logs.String(), "/durum") {
		t.Fatalf("stranger log: %s", f.logs.String())
	}
	f.tg.mu.Lock()
	first := f.tg.reads[0]
	menus := append([]url.Values(nil), f.tg.menus...)
	f.tg.mu.Unlock()
	// The link's code and what came before it are not read again; the menu is the
	// linked chat's alone.
	if first.Get("offset") != "2" || first.Get("timeout") != "50" || first.Get("limit") != "10" ||
		first.Get("allowed_updates") != `["message","callback_query","channel_post"]` ||
		len(menus) != 1 || menus[0].Get("scope") != `{"chat_id":555,"type":"chat"}` || !strings.Contains(menus[0].Get("commands"), `"durum"`) {
		t.Fatalf("first read: %v, menus %v", first, menus)
	}

	// The commander: /durum (also addressed to this bot), the help, an unknown command,
	// plain text.
	f.tg.say(555, 555, "private", "/durum", now)
	f.tg.say(555, 555, "private", "/DURUM@ornekbot", now)
	f.tg.say(555, 555, "private", "/yardim", now)
	f.tg.say(555, 555, "private", "/sil magaza", now)
	f.tg.say(555, 555, "private", "merhaba", now)
	// What comes as it happens is not judged by its date (the server's clock may be off);
	// a forwarded command is someone's words passed on.
	f.tg.say(555, 555, "private", "/yardim", now.Add(-time.Hour))
	f.tg.mu.Lock()
	f.tg.lastID++
	f.tg.updates = append(f.tg.updates, map[string]any{"update_id": f.tg.lastID, "message": map[string]any{
		"message_id": 9, "date": now.Unix(), "text": "/durum", "forward_origin": map[string]any{"type": "user", "date": 1},
		"chat": map[string]any{"id": 555, "type": "private"}, "from": map[string]any{"id": 555, "is_bot": false, "first_name": "Ayşe"}}})
	f.tg.mu.Unlock()
	f.n.readOnce(ctx)
	got := f.sentForms()
	if len(got) != 6 || !strings.Contains(got[5].Get("text"), "Komutlar:") {
		t.Fatalf("answers: %d %v", len(got), got)
	}
	r := got[0].Get("text")
	for _, want := range []string{"🖥 [H1] Sunucu durumu · ", "İşlemci: %7 · yük 0,21 / 0,18 / 0,15", "Bellek: 2,1 GB / 4,0 GB (%52) · takas 300 MB / 4,0 GB",
		"Disk: 21,0 GB / 38,0 GB (%55)", "Açık kalma: 12 gün 4 sa", "Siteler: 1 / 4 çalışıyor", "  container yok: yeni", "  duruyor: blog", "  sağlıksız: kasa",
		"Yedek: son ", "son gece yedeği 07.11", "Açık uyarılar (2):\n🟡 Disk %82 dolu\n🔴 Yedek alınamadı: blog", "FootOps "} {
		if !strings.Contains(r, want) {
			t.Fatalf("report lacks %q:\n%s", want, r)
		}
	}
	if got[0].Get("chat_id") != "555" || buttonOf(t, got[0]) == "" || !strings.Contains(got[1].Get("text"), "Sunucu durumu") || buttonOf(t, got[1]) == buttonOf(t, got[0]) {
		t.Fatalf("report message: %v", got[:2])
	}
	if !strings.Contains(got[2].Get("text"), "/durum — sunucunun durumu") || strings.Contains(got[2].Get("text"), "Bilinmeyen") {
		t.Fatalf("help: %q", got[2].Get("text"))
	}
	if !strings.HasPrefix(got[3].Get("text"), "Bilinmeyen komut: /sil") || !strings.Contains(got[4].Get("text"), "Komutlar:") {
		t.Fatalf("unknown / text: %q %q", got[3].Get("text"), got[4].Get("text"))
	}
	st := f.n.Status().Commands
	if st == nil || !st.Reading || st.User != "Ayşe" || st.LastCommand != "/yardim" || st.Off != "" {
		t.Fatalf("panel: %+v", st)
	}
	// Read once: the next read asks past them, and nothing is answered twice.
	f.clearSent()
	f.n.readOnce(ctx)
	if got := f.sentForms(); len(got) != 0 {
		t.Fatalf("answered again: %v", got)
	}
	f.tg.mu.Lock()
	if left := len(f.tg.updates); left != 0 {
		t.Errorf("%d updates not confirmed", left)
	}
	f.tg.mu.Unlock()
}

func TestReadAudit(t *testing.T) {
	f := newNotifyFixture(t)
	f.n.Audit = NewAudit(filepath.Join(f.dir, "audit.log"))
	f.n.Report = func(context.Context) ServerReport { return testReport() }
	f.link(t)
	f.tg.say(555, 555, "private", "/durum", f.now)
	f.n.readOnce(context.Background())
	data, _ := os.ReadFile(filepath.Join(f.dir, "audit.log"))
	if !strings.Contains(string(data), `"via":"telegram"`) || !strings.Contains(string(data), `"action":"telegram.command","target":"/durum"`) || strings.Contains(string(data), "AAH") {
		t.Fatalf("audit: %s", data)
	}
}

func TestReadButtons(t *testing.T) {
	f := readFixture(t)
	ctx := context.Background()
	f.tg.say(555, 555, "private", "/durum", f.now)
	f.n.readOnce(ctx)
	sent := f.sentForms()
	code := buttonOf(t, sent[0])
	f.tg.mu.Lock()
	msg := int64(100 + f.tg.nextMsg) // the fake numbers messages from 101
	f.tg.mu.Unlock()
	if len(code) != buttonCodeLen {
		t.Fatalf("code %q", code)
	}
	f.clearSent()

	// Someone else's press, a press under another message, an unknown code: no change.
	f.tg.press(555, 777, msg, code)
	f.tg.press(555, 555, msg+1, code)
	f.tg.press(555, 555, msg, "ZZZZZZZZZZZZ")
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	edits, answers := len(f.tg.edits), append([]url.Values(nil), f.tg.answers...)
	f.tg.mu.Unlock()
	if edits != 0 || len(answers) != 3 || answers[0].Get("text") != "" || !strings.Contains(answers[1].Get("text"), "süresi doldu") || !strings.Contains(answers[2].Get("text"), "süresi doldu") {
		t.Fatalf("refused presses: %d edits, answers %v", edits, answers)
	}

	// The commander's press: the message is brought up to date in place, with the same
	// button; a press after an agent restart works too.
	f.clearSent()
	f.tg.press(555, 555, msg, code)
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	ed := append([]url.Values(nil), f.tg.edits...)
	an := append([]url.Values(nil), f.tg.answers...)
	f.tg.mu.Unlock()
	if len(ed) != 1 || ed[0].Get("message_id") != itoa64(msg) || !strings.Contains(ed[0].Get("text"), "Sunucu durumu") || buttonOf(t, ed[0]) != code ||
		len(an) != 1 || an[0].Get("text") != "Güncellendi" {
		t.Fatalf("refresh: edits %v answers %v", ed, an)
	}
	fi, err := os.Stat(f.n.buttonsPath())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("buttons file: %v %v", fi, err)
	}
	f.n.readOnce(ctx) // asks past the press: read for good
	f.n = f.notifier()
	f.n.Report = func(context.Context) ServerReport { return testReport() }
	f.clearSent()
	f.tg.press(555, 555, msg, code)
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	edits = len(f.tg.edits)
	f.tg.mu.Unlock()
	if edits != 1 {
		t.Fatalf("after a restart: %d edits", edits)
	}

	// Past its time the button says so.
	f.clearSent()
	f.set(f.now.Add(buttonTTL + time.Minute))
	f.tg.press(555, 555, msg, code)
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	edits, answers = len(f.tg.edits), append([]url.Values(nil), f.tg.answers...)
	f.tg.mu.Unlock()
	if edits != 0 || len(answers) != 1 || !strings.Contains(answers[0].Get("text"), "süresi doldu") {
		t.Fatalf("expired: %d edits %v", edits, answers)
	}

	// Removing the chat forgets the buttons.
	if err := f.n.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.n.buttonsPath()); !os.IsNotExist(err) {
		t.Fatalf("buttons left after remove: %v", err)
	}
}

func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// nreads is how many times the reader read the bot.
func (f *fakeTelegram) nreads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reads)
}

func TestReadButtonsBounded(t *testing.T) {
	f := readFixture(t)
	for i := 0; i < buttonsMax+10; i++ {
		if _, err := f.n.newButton(tgButton{Kind: "refresh", Chat: 555, Expires: f.now.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	var kept map[string]tgButton
	data, _ := os.ReadFile(f.n.buttonsPath())
	if err := json.Unmarshal(data, &kept); err != nil || len(kept) != buttonsMax {
		t.Fatalf("kept %d (%v)", len(kept), err)
	}
	for _, b := range kept {
		if b.Expires.Before(f.now.Add(10 * time.Minute)) {
			t.Fatalf("an older button was kept over a newer one: %v", b.Expires)
		}
	}
}

func TestReadLimit(t *testing.T) {
	f := readFixture(t)
	for i := 0; i < commandsPerMinute+5; i++ {
		f.tg.say(555, 555, "private", "/yardim", f.now)
	}
	f.n.readOnce(context.Background())
	if n := len(f.sentForms()); n != commandsPerMinute {
		t.Fatalf("answered %d", n)
	}
	f.clearSent()
	f.set(f.now.Add(time.Minute + time.Second))
	f.tg.say(555, 555, "private", "/yardim", f.now)
	f.n.readOnce(context.Background())
	if n := len(f.sentForms()); n != 1 {
		t.Fatalf("after a minute: %d", n)
	}
}

func TestReadStandsAside(t *testing.T) {
	ctx := context.Background()
	// A rehearsal reads nothing (the live server reads its bot).
	f := readFixture(t)
	marker := filepath.Join(f.dir, "rehearsal.json")
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.n.Rehearsal = &Rehearsal{Path: marker}
	if wait := f.n.readOnce(ctx); wait != readIdle || f.tg.nreads() != 0 {
		t.Fatalf("rehearsal: wait %v, %d reads", wait, f.tg.nreads())
	}
	f.n.Rehearsal = nil

	// While a link is made the bot is not read; once linked, reading goes on past the
	// new code.
	l, err := f.n.StartLink(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if wait := f.n.readOnce(ctx); wait <= 0 || f.tg.nreads() != 0 || !f.n.Status().Commands.Paused {
		t.Fatalf("while linking: wait %v, %d reads", wait, f.tg.nreads())
	}
	f.tg.message(555, "/start "+l.Code, nil)
	if _, err := f.n.FinishLink(ctx, "furkan"); err != nil {
		t.Fatal(err)
	}
	f.clearSent()
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	reads := append([]url.Values(nil), f.tg.reads...)
	f.tg.mu.Unlock()
	f.tg.mu.Lock()
	next := itoa64(int64(f.tg.lastID + 1))
	f.tg.mu.Unlock()
	if len(reads) != 1 || reads[0].Get("offset") != next || len(f.sentForms()) != 0 {
		t.Fatalf("after the link: reads %v sent %v", reads, f.sentForms())
	}
	// Not linked: nothing to read.
	if err := f.n.Remove(); err != nil {
		t.Fatal(err)
	}
	if wait := f.n.readOnce(ctx); wait != readIdle || f.tg.nreads() != 1 {
		t.Fatalf("not linked: wait %v", wait)
	}
}

func TestReadFailures(t *testing.T) {
	f := readFixture(t)
	ctx := context.Background()
	f.tg.mu.Lock()
	f.tg.readFail, f.tg.readDesc = 409, "Conflict: terminated by other getUpdates request; make sure that only one bot instance is running"
	f.tg.mu.Unlock()
	t0 := f.now
	if wait := f.n.readOnce(ctx); wait < 30*time.Second {
		t.Fatalf("conflict wait %v", wait)
	}
	st := f.n.Status().Commands
	if st.Reading || !strings.Contains(st.LastErr, "başka bir yer de okuyor") || !st.Failing.Equal(t0) {
		t.Fatalf("conflict status: %+v", st)
	}
	// Not read again before its time.
	if wait := f.n.readOnce(ctx); wait <= 0 || f.tg.nreads() != 1 {
		t.Fatalf("read before the wait: %d", f.tg.nreads())
	}
	if a := f.n.Alerts(f.now); len(a) != 0 {
		t.Fatalf("alert too soon: %v", a)
	}
	f.set(t0.Add(readAlertAfter))
	f.n.readOnce(ctx)
	a := f.n.Alerts(f.now)
	if len(a) != 1 || a[0].Key != "telegram:read" || !Notifiable(a[0]) || !a[0].Since.Equal(t0) || !strings.Contains(a[0].Detail, "her sunucuya ayrı bot") {
		t.Fatalf("alert: %+v", a)
	}
	// A webhook is another story; the token gone waits longest.
	f.tg.mu.Lock()
	f.tg.readDesc = "Conflict: can't use getUpdates method while webhook is active; use deleteWebhook to delete the webhook first"
	f.tg.mu.Unlock()
	f.set(f.now.Add(time.Hour))
	f.n.readOnce(ctx)
	if st := f.n.Status().Commands; !strings.Contains(st.LastErr, "webhook") {
		t.Fatalf("webhook: %+v", st)
	}
	f.tg.mu.Lock()
	f.tg.readFail, f.tg.readDesc = 401, "Unauthorized"
	f.tg.mu.Unlock()
	f.set(f.now.Add(time.Hour))
	if wait := f.n.readOnce(ctx); wait != readBackoffMax {
		t.Fatalf("401 wait %v", wait)
	}
	// It works again: all clear.
	f.tg.mu.Lock()
	f.tg.readFail = 0
	f.tg.mu.Unlock()
	f.set(f.now.Add(time.Hour))
	f.n.readOnce(ctx)
	if st := f.n.Status().Commands; !st.Reading || st.LastErr != "" || len(f.n.Alerts(f.now)) != 0 {
		t.Fatalf("recovered: %+v", st)
	}
	if strings.Contains(f.logs.String(), "AAH") {
		t.Fatal("token in the log")
	}
}

func TestReadCommander(t *testing.T) {
	// Linked before P1a: a private chat's person still counts (its id is theirs); a
	// group's nobody, until linked again.
	for _, tc := range []struct {
		chat int64
		off  bool
	}{{555, false}, {-100123, true}} {
		f := newNotifyFixture(t)
		f.n.Report = func(context.Context) ServerReport { return testReport() }
		cfg := telegramConfig{Token: testBotToken, Bot: "OrnekBot", ChatID: tc.chat, Chat: "Eski", Linked: f.now}
		if err := f.n.saveConfig(cfg); err != nil {
			t.Fatal(err)
		}
		f.tg.say(tc.chat, 555, "group", "/durum", f.now)
		f.n.readOnce(context.Background())
		st := f.n.Status().Commands
		if got := len(f.sentForms()); (got == 0) != tc.off || (st.Off != "") != tc.off || (!tc.off && st.User != "Eski") {
			t.Fatalf("chat %d: %d answers, %+v", tc.chat, got, st)
		}
	}
	// A channel post carries no person: linked, but no commands.
	f := newNotifyFixture(t)
	l, _ := f.n.StartLink(context.Background(), testBotToken, false)
	f.tg.mu.Lock()
	f.tg.lastID++
	f.tg.updates = append(f.tg.updates, map[string]any{"update_id": f.tg.lastID, "channel_post": map[string]any{"message_id": 1, "date": 1,
		"text": "/start " + l.Code, "chat": map[string]any{"id": -100777, "type": "channel", "title": "Kanal"}}})
	f.tg.mu.Unlock()
	st, err := f.n.FinishLink(context.Background(), "furkan")
	if err != nil || st.Commands == nil || st.Commands.Off == "" || strings.Contains(f.tg.texts()[0], "/durum") {
		t.Fatalf("channel: %+v %v %q", st.Commands, err, f.tg.texts())
	}
}

func TestLinkAnotherBotTellsAfresh(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.link(t)
	disk := Alert{Key: "disk", Level: "warn", Title: "Disk %82 dolu", Since: f.now}
	f.set(f.now, disk)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("first: %d", n)
	}
	// The same chat with the same bot keeps what it was told...
	l, _ := f.n.StartLink(ctx, "", true)
	f.tg.message(555, "/start "+l.Code, nil)
	if _, err := f.n.FinishLink(ctx, "furkan"); err != nil {
		t.Fatal(err)
	}
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 2 { // the welcome only
		t.Fatalf("same bot: %q", f.tg.texts())
	}
	// ...another bot (a private chat keeps its id: it is the person's) starts afresh.
	if err := f.n.saveConfig(telegramConfig{Token: testBotToken, Bot: "EskiBot", ChatID: 555, Chat: "Ayşe", Linked: f.now}); err != nil {
		t.Fatal(err)
	}
	l, _ = f.n.StartLink(ctx, testBotToken, false)
	f.tg.message(555, "/start "+l.Code, nil)
	if _, err := f.n.FinishLink(ctx, "furkan"); err != nil {
		t.Fatal(err)
	}
	f.n.Tick(ctx)
	texts := f.tg.texts()
	if len(texts) != 4 || !strings.Contains(texts[3], "Disk %82 dolu") {
		t.Fatalf("another bot: %q", texts)
	}
}

func TestParseCommand(t *testing.T) {
	for in, want := range map[string][2]string{
		"/durum":             {"durum", "y"},
		"  /Durum  ":         {"durum", "y"},
		"/durum@OrnekBot x":  {"durum", "y"},
		"/durum@ornekbot":    {"durum", "y"},
		"/durum@BaskaBot":    {"", "y"},
		"/":                  {"", "y"},
		"durum":              {"", ""},
		"":                   {"", ""},
		"/yardım":            {"yardım", "y"},
		"/start ABCD234567":  {"start", "y"},
		"/@OrnekBot":         {"", "y"},
		"/durum@OrnekBot@x":  {"", "y"},
		"\u00a0/durum":       {"durum", "y"},
		"/durum\n/yardim":    {"durum", "y"},
		"/DURUM@ORNEKBOT  ?": {"durum", "y"},
	} {
		cmd, ok := parseCommand(in, "OrnekBot")
		if cmd != want[0] || ok != (want[1] == "y") {
			t.Errorf("%q: %q %v", in, cmd, ok)
		}
	}
}

func TestRenderReportEdges(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
	r := renderReport("[A]", ServerReport{}, now)
	for _, want := range []string{"🖥 [A] Sunucu durumu · 07.11.2026 12:00:00", "Ölçüm henüz yok", "Site yok", "Yedek: bu sunucuda yedekleme yok", "Açık uyarı yok ✓"} {
		if !strings.Contains(r, want) {
			t.Fatalf("empty report lacks %q:\n%s", want, r)
		}
	}
	var many []Alert
	for i := 0; i < reportAlertsMax+3; i++ {
		many = append(many, Alert{Key: "x", Level: "warn", Title: "uyarı"})
	}
	r = renderReport("[A]", ServerReport{SitesErr: "okunamadı", Alerts: many, Backup: &BackupStatus{},
		Overview: Overview{SampledAt: now, Errors: []string{"docker: bağlantı yok"}}}, now)
	for _, want := range []string{"Siteler okunamadı: okunamadı", "Yedek: depo bağlı değil", "… ve 3 tane daha (panelde)", "⚠ Ölçülemeyen: docker: bağlantı yok", "takas yok"} {
		if !strings.Contains(r, want) {
			t.Fatalf("report lacks %q:\n%s", want, r)
		}
	}
	paused := &BackupStatus{Configured: true, Settings: BackupSettings{Confirmed: now, Paused: true}, Running: &Job{}}
	if r := renderReport("[A]", ServerReport{Backup: paused}, now); !strings.Contains(r, "Yedek: henüz alınmadı · zamanlanmış yedekler duraklatıldı · şu an alınıyor") {
		t.Fatalf("paused:\n%s", r)
	}
	if got := shortTime(now.Add(-time.Hour), now); got != "11:00" {
		t.Errorf("today: %s", got)
	}
	if got := shortTime(now.Add(-24*time.Hour), now); got != "06.11 12:00" {
		t.Errorf("yesterday: %s", got)
	}
}

func TestLinkCodeOnce(t *testing.T) {
	ctx := context.Background()
	// The code posted again by someone else (a group's member) or forwarded elsewhere:
	// spent, nothing linked, the link is started again.
	for name, second := range map[string]func(f *fakeTelegram, code string){
		"another person": func(f *fakeTelegram, code string) {
			f.say(-100500, 777, "group", "/start@OrnekBot "+code, time.Unix(1, 0))
		},
		"another chat": func(f *fakeTelegram, code string) { f.say(888, 555, "private", "/start "+code, time.Unix(1, 0)) },
	} {
		f := newNotifyFixture(t)
		l, err := f.n.StartLink(ctx, testBotToken, false)
		if err != nil {
			t.Fatal(err)
		}
		f.tg.say(-100500, 555, "group", "/start@OrnekBot "+l.Code, time.Unix(1, 0))
		second(f.tg, l.Code)
		if _, err := f.n.FinishLink(ctx, "furkan"); err == nil || !strings.Contains(err.Error(), "birden çok") {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(f.n.SecretPath); !os.IsNotExist(err) || f.n.Status().Link != nil || len(f.tg.texts()) != 0 {
			t.Fatalf("%s: linked anyway, or the link stays", name)
		}
	}
	// A forwarded copy and a bot's do not count; the first typed sending does, and the
	// person who typed it is named in the welcome.
	f := newNotifyFixture(t)
	l, _ := f.n.StartLink(ctx, testBotToken, false)
	f.tg.mu.Lock()
	f.tg.lastID++
	f.tg.updates = append(f.tg.updates, map[string]any{"update_id": f.tg.lastID, "message": map[string]any{"message_id": 1, "date": 1,
		"text": "/start " + l.Code, "forward_date": 5, "chat": map[string]any{"id": 777, "type": "private"},
		"from": map[string]any{"id": 777, "is_bot": false, "first_name": "Yabancı"}}})
	f.tg.lastID++
	f.tg.updates = append(f.tg.updates, map[string]any{"update_id": f.tg.lastID, "message": map[string]any{"message_id": 2, "date": 1,
		"text": "/start " + l.Code, "chat": map[string]any{"id": 778, "type": "private"},
		"from": map[string]any{"id": 778, "is_bot": true, "first_name": "Bot"}}})
	f.tg.mu.Unlock()
	if _, err := f.n.FinishLink(ctx, "furkan"); err != errCodeNotSeen {
		t.Fatalf("forwarded / bot: %v", err)
	}
	f.tg.say(555, 555, "private", "/start "+l.Code, time.Unix(1, 0))
	f.tg.say(555, 555, "private", "/start "+l.Code, time.Unix(1, 0)) // Start pressed twice
	st, err := f.n.FinishLink(ctx, "furkan")
	if err != nil || st.Commands == nil || st.Commands.User != "Ayşe (@ayse)" {
		t.Fatalf("linked: %+v %v", st.Commands, err)
	}
	if w := f.tg.texts()[0]; !strings.Contains(w, "komutları yalnız Ayşe (@ayse) verebilir") {
		t.Fatalf("welcome: %q", w)
	}
	f.tg.mu.Lock()
	lr := f.tg.linkReads[len(f.tg.linkReads)-1]
	last := f.tg.lastID
	f.tg.mu.Unlock()
	if lr.Get("allowed_updates") != allowedUpdates {
		t.Fatalf("the link's read asks for %q", lr.Get("allowed_updates"))
	}
	// Both sendings of the code are past the reader.
	f.clearSent()
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	off := f.tg.reads[0].Get("offset")
	f.tg.mu.Unlock()
	if off != itoa64(int64(last+1)) || len(f.sentForms()) != 0 {
		t.Fatalf("offset %s (want %d), answers %v", off, last+1, f.sentForms())
	}
}

func TestReadOffsets(t *testing.T) {
	f := readFixture(t)
	ctx := context.Background()
	// An answer that comes back empty has confirmed everything: the next read asks from
	// the oldest unread (an update number Telegram picks anew after a quiet week cannot
	// fall behind an old offset).
	f.n.readOnce(ctx)
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	r := append([]url.Values(nil), f.tg.reads...)
	f.tg.mu.Unlock()
	if r[0].Get("offset") != "2" || r[1].Has("offset") {
		t.Fatalf("offsets: %q then %q", r[0].Get("offset"), r[1].Get("offset"))
	}
	// One update that does not decode is read past, the others are answered.
	f.tg.mu.Lock()
	f.tg.lastID++
	f.tg.updates = append(f.tg.updates, map[string]any{"update_id": f.tg.lastID, "message": "bozuk"})
	f.tg.mu.Unlock()
	f.tg.say(555, 555, "private", "/yardim", f.now)
	f.n.readOnce(ctx)
	if n := len(f.sentForms()); n != 1 || !strings.Contains(f.logs.String(), "1 güncelleme okunamadı") {
		t.Fatalf("bad update: %d answers, log %s", n, f.logs.String())
	}
	// A server that gives read updates again: nothing new, so the reader waits instead of
	// asking again at once.
	f.clearSent()
	f.tg.mu.Lock()
	f.tg.ignoreOffset = true
	f.tg.mu.Unlock()
	if wait := f.n.readOnce(ctx); wait <= 0 || len(f.sentForms()) != 0 {
		t.Fatalf("read again: wait %v, %v", wait, f.sentForms())
	}
}

func TestReadConfirmsOnExit(t *testing.T) {
	f := readFixture(t)
	f.tg.say(555, 555, "private", "/yardim", f.now)
	// Answered, but the read that would confirm it has not come: the agent stops.
	f.n.readOnce(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.n.Read(ctx)
	f.tg.mu.Lock()
	last := f.tg.reads[len(f.tg.reads)-1]
	left, id := len(f.tg.updates), f.tg.lastID
	f.tg.mu.Unlock()
	if len(f.sentForms()) != 1 || last.Get("offset") != itoa64(int64(id+1)) || last.Get("timeout") != "0" || left != 0 {
		t.Fatalf("on exit: %v, %d left", last, left)
	}
}

func TestOnceButtons(t *testing.T) {
	f := readFixture(t)
	ctx := context.Background()
	once, err := f.n.newButton(tgButton{Kind: "refresh", Chat: 555, Msg: 900, Once: true, Expires: f.now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Found waiting (a backlog): it may be hours old — not taken, kept for a new press.
	f.tg.press(555, 555, 900, once)
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	ans := append([]url.Values(nil), f.tg.answers...)
	edits := len(f.tg.edits)
	f.tg.mu.Unlock()
	if edits != 0 || len(ans) != 1 || !strings.Contains(ans[0].Get("text"), "eski olabilir") {
		t.Fatalf("backlog press: %d edits, %v", edits, ans)
	}
	// As it happens: taken, and used up before the work.
	f.clearSent()
	f.tg.press(555, 555, 900, once)
	f.tg.press(555, 555, 900, once) // a double tap
	f.n.readOnce(ctx)
	f.tg.mu.Lock()
	ans = append([]url.Values(nil), f.tg.answers...)
	edits = len(f.tg.edits)
	f.tg.mu.Unlock()
	if edits != 1 || len(ans) != 2 || ans[0].Get("text") != "Güncellendi" || !strings.Contains(ans[1].Get("text"), "süresi doldu") {
		t.Fatalf("live presses: %d edits, %v", edits, ans)
	}
	var kept map[string]tgButton
	data, _ := os.ReadFile(f.n.buttonsPath())
	_ = json.Unmarshal(data, &kept)
	if _, ok := kept[once]; ok {
		t.Fatal("a used Once button is still on disk")
	}
	// No buttons for a chat that is gone.
	if err := f.n.saveConfig(telegramConfig{Token: testBotToken, Bot: "OrnekBot", ChatID: 556, Chat: "Başka", Linked: f.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.n.newButton(tgButton{Kind: "refresh", Chat: 555, Expires: f.now.Add(time.Hour)}); err == nil {
		t.Fatal("a button for a chat no longer linked")
	}
}
