package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBotToken = "123456789:AAH-test_secret-part-0123456789abcdefgh"

// fakeTelegram plays the Bot API: getMe, getUpdates and sendMessage for one token.
type fakeTelegram struct {
	mu       sync.Mutex
	token    string
	updates  []map[string]any
	sent     []url.Values
	fail     int    // sendMessage answers with this error code (0: works)
	failDesc string // and this description
	retry    int    // retry_after for 429
	failFrom int    // with fail: only once this many messages went (0: at once)
	migrate  int64  // migrate_to_chat_id with a failure
	calls    []string
}

func (f *fakeTelegram) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+f.token+"/")
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
		return
	}
	_ = r.ParseForm()
	f.calls = append(f.calls, method)
	switch method {
	case "getMe":
		fmt.Fprint(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"FootOps","username":"OrnekBot"}}`)
	case "getUpdates":
		// Only the newest hundred are asked for (offset -100).
		if r.PostForm.Get("offset") != "-100" {
			http.Error(w, `{"ok":false,"error_code":400,"description":"expected offset -100"}`, http.StatusBadRequest)
			return
		}
		ups := f.updates
		if len(ups) > 100 {
			ups = ups[len(ups)-100:]
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "result": ups})
		w.Write(b)
	case "sendMessage":
		if f.fail != 0 && len(f.sent) >= f.failFrom && (f.migrate == 0 || r.PostForm.Get("chat_id") != fmt.Sprint(f.migrate)) {
			w.WriteHeader(f.fail)
			params := map[string]any{"retry_after": f.retry}
			if f.migrate != 0 {
				params = map[string]any{"migrate_to_chat_id": f.migrate}
			}
			b, _ := json.Marshal(map[string]any{"ok": false, "error_code": f.fail, "description": f.failDesc, "parameters": params})
			w.Write(b)
			return
		}
		f.sent = append(f.sent, r.PostForm)
		fmt.Fprint(w, `{"ok":true,"result":{"message_id":7}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"ok":false,"error_code":404,"description":"Not Found"}`)
	}
}

func (f *fakeTelegram) message(chat int64, text string, extra map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := map[string]any{"id": chat, "type": "private", "first_name": "Ayşe"}
	for k, v := range extra {
		c[k] = v
	}
	f.updates = append(f.updates, map[string]any{"update_id": len(f.updates) + 1,
		"message": map[string]any{"message_id": 1, "date": 1, "text": text, "chat": c}})
}

func (f *fakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, v := range f.sent {
		out = append(out, v.Get("text"))
	}
	return out
}

func (f *fakeTelegram) setFail(code int, desc string, retry int) {
	f.mu.Lock()
	f.fail, f.failDesc, f.retry = code, desc, retry
	f.mu.Unlock()
}

type notifyFixture struct {
	n      *Notifier
	tg     *fakeTelegram
	srv    *httptest.Server
	dir    string
	now    time.Time
	active []Alert
	mu     sync.Mutex
	logs   *strings.Builder
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	f := &notifyFixture{tg: &fakeTelegram{token: testBotToken}, dir: t.TempDir(),
		now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC), logs: &strings.Builder{}}
	f.srv = httptest.NewServer(f.tg)
	t.Cleanup(f.srv.Close)
	f.n = f.notifier()
	return f
}

// notifier is a Notifier on the fixture's files, as after an agent (re)start.
func (f *notifyFixture) notifier() *Notifier {
	n := &Notifier{SecretPath: filepath.Join(f.dir, "etc/notify/telegram.json"), StatePath: filepath.Join(f.dir, "notify.json"),
		Server: "h1", PanelHost: "panel.example.com", API: f.srv.URL, HTTP: f.srv.Client(),
		Log: log.New(f.logs, "", 0)}
	n.Active = func() []Alert {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]Alert(nil), f.active...)
	}
	n.now = func() time.Time {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.now
	}
	// Long running: the hold after a start is tested on its own.
	n.init()
	n.started = time.Time{}
	return n
}

func (f *notifyFixture) set(now time.Time, alerts ...Alert) {
	f.mu.Lock()
	f.now = now
	f.active = alerts
	f.mu.Unlock()
}

// link makes the chat 555 the linked one.
func (f *notifyFixture) link(t *testing.T) {
	t.Helper()
	l, err := f.n.StartLink(context.Background(), testBotToken, false)
	if err != nil {
		t.Fatal(err)
	}
	f.tg.message(555, "/start "+l.Code, nil)
	if _, err := f.n.FinishLink(context.Background(), "furkan"); err != nil {
		t.Fatal(err)
	}
	f.tg.mu.Lock()
	f.tg.sent = nil
	f.tg.mu.Unlock()
}

func TestNotifyLink(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	if _, err := f.n.StartLink(ctx, "not-a-token", false); err == nil || !strings.Contains(err.Error(), "biçiminde") {
		t.Fatalf("bad token: %v", err)
	}
	// A token Telegram does not know.
	if _, err := f.n.StartLink(ctx, "123456789:AAH-wrong_secret-part-0123456789abcdefgh", false); err == nil ||
		!strings.Contains(err.Error(), "geçersiz") || strings.Contains(err.Error(), "wrong_secret") {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := f.n.FinishLink(ctx, "furkan"); err == nil || !strings.Contains(err.Error(), "başlatılmadı") {
		t.Fatalf("finish without a link: %v", err)
	}
	if _, err := f.n.StartLink(ctx, "", true); !errors.Is(err, errNoNotify) {
		t.Fatalf("keep without a chat: %v", err)
	}

	l, err := f.n.StartLink(ctx, " "+testBotToken+" ", false)
	if err != nil {
		t.Fatal(err)
	}
	if l.Bot != "OrnekBot" || len(l.Code) != 10 || l.URL != "https://t.me/OrnekBot?start="+l.Code || !l.Expires.Equal(f.now.Add(linkTTL)) {
		t.Fatalf("link: %+v", l)
	}
	st := f.n.Status()
	if st.Linked || st.Link == nil || st.Link.Code != l.Code {
		t.Fatalf("status while linking: %+v", st)
	}
	// Nothing yet, then someone else writes first: not good enough.
	if _, err := f.n.FinishLink(ctx, "furkan"); !errors.Is(err, errCodeNotSeen) {
		t.Fatalf("no message yet: %v", err)
	}
	f.tg.message(999, "/start", map[string]any{"first_name": "Yabancı"})
	f.tg.message(998, "/start WRONGCODE1", nil)
	if _, err := f.n.FinishLink(ctx, "furkan"); !errors.Is(err, errCodeNotSeen) {
		t.Fatalf("other chats: %v", err)
	}
	if _, err := os.Stat(f.n.SecretPath); !os.IsNotExist(err) {
		t.Fatalf("saved before the code came: %v", err)
	}
	f.tg.message(555, "/start "+l.Code, map[string]any{"last_name": "Yılmaz", "username": "ayse"})
	got, err := f.n.FinishLink(ctx, "furkan")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Linked || got.Chat != "Ayşe Yılmaz (@ayse)" || got.Bot != "OrnekBot" || got.Link != nil || got.By != "furkan" {
		t.Fatalf("linked: %+v", got)
	}
	texts := f.tg.texts()
	if len(texts) != 1 || !strings.Contains(texts[0], "[H1] FootOps bu sohbete bağlandı") || f.tg.sent[0].Get("chat_id") != "555" {
		t.Fatalf("welcome: %q %v", texts, f.tg.sent)
	}
	fi, err := os.Stat(f.n.SecretPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", fi, err)
	}
	// The status never carries the token.
	b, _ := json.Marshal(f.n.Status())
	if strings.Contains(string(b), "AAH") {
		t.Fatalf("token in status: %s", b)
	}
	// Linking another chat with the saved token.
	l2, err := f.n.StartLink(ctx, "", true)
	if err != nil || l2.Code == l.Code {
		t.Fatalf("keep: %+v %v", l2, err)
	}
	f.n.CancelLink()
	if st := f.n.Status(); st.Link != nil || !st.Linked {
		t.Fatalf("cancelled: %+v", st)
	}
	// A code past its time.
	l3, _ := f.n.StartLink(ctx, "", true)
	f.tg.message(556, "/start "+l3.Code, nil)
	f.set(f.now.Add(linkTTL + time.Second))
	if _, err := f.n.FinishLink(ctx, "furkan"); err == nil || !strings.Contains(err.Error(), "süresi doldu") {
		t.Fatalf("expired: %v", err)
	}
	if c, _ := f.n.config(); c.ChatID != 555 {
		t.Fatalf("chat changed: %+v", c)
	}
	// Remove forgets everything.
	if err := f.n.Remove(); err != nil {
		t.Fatal(err)
	}
	if st := f.n.Status(); st.Linked {
		t.Fatalf("after remove: %+v", st)
	}
	if err := f.n.Test(ctx); !errors.Is(err, errNoNotify) {
		t.Fatalf("test after remove: %v", err)
	}
}

func TestNotifyEvents(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.link(t)
	t0 := f.now
	backup := Alert{Key: "backup:failed", Level: "bad", Title: "Yedek alınamadı: magaza (veritabanı)", Detail: "restic: bağlantı yok", Link: "/backup", Since: t0.Add(-time.Hour)}
	disk := Alert{Key: "disk", Level: "warn", Title: "Disk %82 dolu", Link: "/", Since: t0}
	quiet := []Alert{
		{Key: "cpu", Level: "warn", Title: "CPU 5 dakikadır %95"},
		{Key: "limit:magaza", Level: "warn", Title: "magaza bellek sınırının %92'inde"},
		{Key: "memory", Level: "warn", Title: "RAM %91 dolu"},
		{Key: "down:magaza", Level: "bad", Title: "magaza çalışmıyor"},
		{Key: "health:magaza", Level: "bad", Title: "magaza sağlıksız"},
		{Key: "notify:telegram", Level: "warn", Title: "Bildirimler Telegram'a gönderilemiyor"},
	}
	f.set(t0, append([]Alert{backup, disk}, quiet...)...)
	f.n.Tick(ctx)
	texts := f.tg.texts()
	if len(texts) != 1 {
		t.Fatalf("one message for the lot: %q", texts)
	}
	m := texts[0]
	for _, want := range []string{"🔴 [H1] Yedek alınamadı: magaza (veritabanı)\nrestic: bağlantı yok\nhttps://panel.example.com/backup",
		"🟡 [H1] Disk %82 dolu\nhttps://panel.example.com/"} {
		if !strings.Contains(m, want) {
			t.Fatalf("message lacks %q:\n%s", want, m)
		}
	}
	for _, q := range quiet {
		if strings.Contains(m, q.Title) {
			t.Fatalf("%s should not be sent:\n%s", q.Key, m)
		}
	}
	if strings.Index(m, "Yedek") > strings.Index(m, "Disk") {
		t.Fatalf("bad first:\n%s", m)
	}

	// Nothing changes, or only the wording: nothing is sent.
	backup2 := backup
	backup2.Title = "Yedek alınamadı: magaza (veritabanı), blog (dosyalar)"
	f.set(t0.Add(time.Minute), backup2, disk)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("wording change sent: %q", f.tg.texts())
	}
	// Worse: warn → bad.
	disk2 := disk
	disk2.Level, disk2.Title = "bad", "Disk %91 dolu"
	f.set(t0.Add(2*time.Minute), backup2, disk2)
	f.n.Tick(ctx)
	if texts := f.tg.texts(); len(texts) != 2 || !strings.HasPrefix(texts[1], "🔴 [H1] Kötüleşti: Disk %91 dolu") {
		t.Fatalf("worse: %q", texts)
	}
	// Milder again, then worse again: the chat already knows it is bad.
	f.set(t0.Add(3*time.Minute), backup2, disk)
	f.n.Tick(ctx)
	f.set(t0.Add(4*time.Minute), backup2, disk2)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 2 {
		t.Fatalf("flapping level sent: %q", f.tg.texts())
	}
	// The backup clears: not before clearHold, and not if it comes back meanwhile.
	f.set(t0.Add(5*time.Minute), disk2)
	f.n.Tick(ctx)
	f.set(t0.Add(8*time.Minute), disk2)
	f.n.Tick(ctx)
	f.set(t0.Add(9*time.Minute), backup2, disk2)
	f.n.Tick(ctx)
	f.set(t0.Add(10*time.Minute), disk2)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 2 {
		t.Fatalf("cleared too early: %q", f.tg.texts())
	}
	f.set(t0.Add(15*time.Minute), disk2)
	f.n.Tick(ctx)
	texts = f.tg.texts()
	if len(texts) != 3 || texts[2] != "✅ [H1] Düzeldi: Yedek alınamadı: magaza (veritabanı) (1 sa 10 dk sürdü)" {
		t.Fatalf("cleared: %q", texts)
	}
	// It opens again later: a new message.
	f.set(t0.Add(20*time.Minute), backup, disk2)
	f.n.Tick(ctx)
	if texts := f.tg.texts(); len(texts) != 4 || !strings.HasPrefix(texts[3], "🔴 [H1] Yedek alınamadı") {
		t.Fatalf("again: %q", texts)
	}

	// The server's own containers do go; memory only when critical, and it stays the
	// chat's business until it clears even when it eases.
	core := []Alert{{Key: "down:footops-mongo", Level: "bad", Title: "footops-mongo çalışmıyor"},
		{Key: "memory", Level: "bad", Title: "RAM %96 dolu"}}
	f.set(t0.Add(21*time.Minute), append([]Alert{backup, disk2}, core...)...)
	f.n.Tick(ctx)
	texts = f.tg.texts()
	if len(texts) != 5 || !strings.Contains(texts[4], "footops-mongo çalışmıyor") || !strings.Contains(texts[4], "RAM %96 dolu") {
		t.Fatalf("core: %q", texts)
	}
	f.set(t0.Add(22*time.Minute), backup, disk2, core[0], Alert{Key: "memory", Level: "warn", Title: "RAM %91 dolu"})
	f.n.Tick(ctx)
	f.set(t0.Add(30*time.Minute), backup, disk2, core[0])
	f.n.Tick(ctx)
	f.set(t0.Add(36*time.Minute), backup, disk2, core[0])
	f.n.Tick(ctx)
	if texts := f.tg.texts(); len(texts) != 6 || texts[5] != "✅ [H1] Düzeldi: RAM %96 dolu" {
		t.Fatalf("memory cleared: %q", texts)
	}
}

func TestNotifyRestartAndFailures(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	f.link(t)
	t0 := f.now
	a := Alert{Key: "backup:stale", Level: "bad", Title: "Yedek gecikti: magaza (veritabanı)", Link: "/backup", Since: t0}
	b := Alert{Key: "cert:x", Level: "warn", Title: "x sertifikasının bitmesine 20 gün var", Link: "/certs", Since: t0}
	f.set(t0, a, b)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("first: %q", f.tg.texts())
	}

	// An agent restart: the alerts take a moment to be worked out again; nothing is
	// repeated, nothing reported cleared.
	f.n = f.notifier()
	f.set(t0.Add(time.Minute))
	f.n.Tick(ctx)
	f.set(t0.Add(2*time.Minute), a, b)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("after restart: %q", f.tg.texts())
	}
	// One gone after a restart: not "cleared" before startHold (some alerts need a
	// history the restart lost), then reported.
	f.n = f.notifier()
	f.n.started = t0.Add(3 * time.Minute)
	f.set(t0.Add(3*time.Minute), a)
	f.n.Tick(ctx)
	f.set(t0.Add(9*time.Minute), a)
	f.n.Tick(ctx)
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("cleared within the start hold: %q", f.tg.texts())
	}
	f.set(t0.Add(28*time.Minute), a)
	f.n.Tick(ctx)
	if texts := f.tg.texts(); len(texts) != 2 || !strings.HasPrefix(texts[1], "✅ [H1] Düzeldi: x sertifikasının") {
		t.Fatalf("cleared over a restart: %q", texts)
	}
	t0 = t0.Add(20 * time.Minute) // the rest as before, later

	// Telegram refuses: nothing is marked sent, the error is kept (without the token),
	// and the next try waits.
	c := Alert{Key: "backup:size", Level: "warn", Title: "Yedek deposu 3.1 GB (uyarı eşiği 3.0 GB)", Link: "/backup", Since: t0}
	f.tg.setFail(500, "Internal Server Error "+testBotToken, 0)
	f.set(t0.Add(10*time.Minute), a, c)
	f.n.Tick(ctx)
	st := f.n.Status()
	if st.LastErr == "" || strings.Contains(st.LastErr, "AAH") || st.Failing.IsZero() || st.RetryAt.IsZero() || st.Told != 1 {
		t.Fatalf("failure: %+v", st)
	}
	if strings.Contains(f.logs.String(), "AAH") || !strings.Contains(f.logs.String(), "bildirim gönderilemedi") {
		t.Fatalf("log: %s", f.logs.String())
	}
	f.tg.setFail(0, "", 0)
	f.set(t0.Add(10*time.Minute+30*time.Second), a, c)
	f.n.Tick(ctx) // within the backoff: no try
	if n := len(f.tg.texts()); n != 2 {
		t.Fatalf("tried within the backoff: %q", f.tg.texts())
	}
	// The panel's alert about it comes after 10 minutes of failing.
	f.tg.setFail(429, "Too Many Requests: retry after 120", 120)
	f.set(t0.Add(12*time.Minute), a, c)
	f.n.Tick(ctx)
	if st := f.n.Status(); !strings.Contains(st.LastErr, "120 sn") || st.RetryAt.Before(t0.Add(13*time.Minute+59*time.Second)) {
		t.Fatalf("429: %+v", st)
	}
	if al := f.n.Alerts(t0.Add(21 * time.Minute)); len(al) != 1 || al[0].Key != "notify:telegram" || Notifiable(al[0]) {
		t.Fatalf("own alert: %+v", al)
	}
	f.tg.setFail(0, "", 0)
	f.set(t0.Add(15*time.Minute), a, c)
	f.n.Tick(ctx)
	if texts := f.tg.texts(); len(texts) != 3 || !strings.Contains(texts[2], "Yedek deposu 3.1 GB") {
		t.Fatalf("after recovery: %q", texts)
	}
	if st := f.n.Status(); st.LastErr != "" && !st.Failing.IsZero() || st.Told != 2 {
		t.Fatalf("recovered: %+v", st)
	}
	if al := f.n.Alerts(t0.Add(30 * time.Minute)); len(al) != 0 {
		t.Fatalf("own alert after recovery: %+v", al)
	}

	// A test message; a blocked bot says so in Turkish.
	if err := f.n.Test(ctx); err != nil {
		t.Fatal(err)
	}
	f.tg.setFail(403, "Forbidden: bot was blocked by the user", 0)
	if err := f.n.Test(ctx); err == nil || !strings.Contains(err.Error(), "engellenmiş") {
		t.Fatalf("blocked: %v", err)
	}
}

// Go's client errors carry the address, and the address carries the token.
func TestNotifyTransportErrorHidesToken(t *testing.T) {
	f := newNotifyFixture(t)
	f.link(t)
	f.srv.Close()
	err := f.n.Test(context.Background())
	if err == nil || strings.Contains(err.Error(), "AAH") || strings.Contains(err.Error(), "123456789") || !strings.Contains(err.Error(), "ulaşılamadı") {
		t.Fatalf("transport error: %v", err)
	}
	if s := scrubToken("x "+testBotToken+" y "+strings.SplitN(testBotToken, ":", 2)[1], testBotToken); strings.Contains(s, "AAH") {
		t.Fatalf("scrub: %s", s)
	}
}

func TestNotifyMessagesSplit(t *testing.T) {
	n := &Notifier{Server: "a"}
	var evs []alertEvent
	for i := 0; i < 40; i++ {
		evs = append(evs, alertEvent{kind: "open", alert: Alert{Key: fmt.Sprint(i), Level: "bad", Title: fmt.Sprintf("uyarı %d", i),
			Detail: strings.Repeat("ayrıntı ", 80)}})
	}
	msgs := n.messages(evs)
	if len(msgs) < 2 {
		t.Fatalf("not split: %d", len(msgs))
	}
	total, told := 0, 0
	for _, m := range msgs {
		if len(m.text) > telegramMax {
			t.Fatalf("too long: %d", len(m.text))
		}
		total += strings.Count(m.text, "🔴 [A] uyarı")
		told += len(m.evs)
	}
	if total != 40 || told != 40 {
		t.Fatalf("lost events: %d %d", total, told)
	}
	// Not UTF-8 (another program's words): made valid, Telegram would refuse it.
	if r := n.render(alertEvent{kind: "open", alert: Alert{Level: "bad", Title: "x\xffy", Detail: "a\xc3"}}); !strings.Contains(r, "x?y") || !strings.Contains(r, "a?") {
		t.Fatalf("utf-8: %q", r)
	}
	if got := spanTR(90 * time.Second); got != "1 dk" {
		t.Fatal(got)
	}
	if got := spanTR(26*time.Hour + 5*time.Minute); got != "1 gün 2 sa" {
		t.Fatal(got)
	}
	if got := spanTR(2 * time.Hour); got != "2 sa" {
		t.Fatal(got)
	}
}

// The API: no token in answers or the audit log.
func TestNotifyAPI(t *testing.T) {
	f := newNotifyFixture(t)
	c, s := startServer(t, 999, func(s *Server) { s.Notify = f.n })
	status := func(err error) int {
		var ae *Error
		if errors.As(err, &ae) {
			return ae.Status
		}
		return 0
	}
	a := Actor{User: "furkan", Via: "ssh"}
	if st, err := c.NotifyStatus(ctx); err != nil || st.Linked {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, err := c.NotifyStart(ctx, "kötü", false, a); status(err) != 422 {
		t.Fatalf("bad token: %v", err)
	}
	l, err := c.NotifyStart(ctx, testBotToken, false, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.NotifyLink(ctx, a); status(err) != 409 {
		t.Fatalf("not yet: %v", err)
	}
	f.tg.message(555, "/start "+l.Code, nil)
	st, err := c.NotifyLink(ctx, a)
	if err != nil || !st.Linked || st.Chat != "Ayşe" {
		t.Fatalf("link: %+v %v", st, err)
	}
	if err := c.NotifyTest(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := c.NotifyRemove(ctx, a); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.Audit.Tail(100)
	var actions []string
	for _, e := range entries {
		b, _ := json.Marshal(e)
		if strings.Contains(string(b), "AAH") {
			t.Fatalf("token in the audit log: %s", b)
		}
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, " "); got != "notify.remove notify.test notify.link notify.start notify.start" {
		t.Fatalf("audit: %s", got)
	}
}

// Review findings (Y4): a disk that takes no state, a batch that breaks halfway, a
// failure that ends with nothing left to send, a group that became a supergroup, the
// same chat linked again, a settings directory the agent cannot write.
func TestNotifyEdges(t *testing.T) {
	ctx := context.Background()
	f := newNotifyFixture(t)
	f.link(t)
	t0 := f.now
	a := Alert{Key: "backup:failed", Level: "bad", Title: "Yedek alınamadı: magaza (veritabanı)", Since: t0}
	b := Alert{Key: "disk", Level: "warn", Title: "Disk %82 dolu", Since: t0}

	// The state file cannot be written: the state is kept in memory, nothing repeats,
	// the log says so once.
	f.n.StatePath = filepath.Join(f.dir, "yok", "klasor", "notify.json")
	os.WriteFile(filepath.Join(f.dir, "yok"), []byte("dosya"), 0o600) // a file where the directory should be
	for i := 0; i < 4; i++ {
		f.set(t0.Add(time.Duration(i)*time.Minute), a)
		f.n.Tick(ctx)
	}
	if n := len(f.tg.texts()); n != 1 {
		t.Fatalf("repeated without a state file: %q", f.tg.texts())
	}
	if c := strings.Count(f.logs.String(), "bildirim durumu kaydedilemedi"); c != 1 {
		t.Fatalf("logged %d times:\n%s", c, f.logs.String())
	}
	f.n.StatePath = filepath.Join(f.dir, "notify.json")

	// A batch that breaks after its first message: that one is recorded, only the rest
	// goes again.
	many := []Alert{a}
	for i := 0; i < 12; i++ {
		many = append(many, Alert{Key: fmt.Sprintf("cert:%02d", i), Level: "warn", Title: fmt.Sprintf("sertifika %02d", i),
			Detail: strings.Repeat("uzun ayrıntı ", 40), Since: t0})
	}
	f.tg.mu.Lock()
	f.tg.fail, f.tg.failDesc, f.tg.failFrom = 500, "Internal Server Error", len(f.tg.sent)+1
	f.tg.mu.Unlock()
	f.set(t0.Add(5*time.Minute), many...)
	f.n.Tick(ctx)
	first := len(f.tg.texts())
	told := f.n.Status().Told
	if first != 2 || told < 2 || told >= len(many) {
		t.Fatalf("half a batch: %d messages, %d told", first, told)
	}
	f.tg.setFail(0, "", 0)
	f.set(t0.Add(30*time.Minute), many...)
	f.n.Tick(ctx)
	all := strings.Join(f.tg.texts(), "\n")
	for _, x := range many {
		if c := strings.Count(all, x.Title+"\n"); c != 1 && !(x.Key == "backup:failed" && c == 1) {
			t.Fatalf("%s sent %d times", x.Key, c)
		}
	}

	// Telegram down while an alert opens and clears again: once nothing waits to be
	// sent, nothing is failing (and the panel stops saying so).
	f.tg.setFail(502, "Bad Gateway", 0)
	f.set(t0.Add(40*time.Minute), append(many, b)...)
	f.n.Tick(ctx)
	if st := f.n.Status(); st.Failing.IsZero() {
		t.Fatalf("not failing: %+v", st)
	}
	f.set(t0.Add(80*time.Minute), many...)
	f.n.Tick(ctx)
	if st := f.n.Status(); !st.Failing.IsZero() || st.LastErr != "" {
		t.Fatalf("still failing with nothing to send: %+v", st)
	}

	// The group became a supergroup: the new id is kept, the message goes there.
	f.tg.mu.Lock()
	f.tg.fail, f.tg.failDesc, f.tg.failFrom, f.tg.migrate = 400, "Bad Request: group chat was upgraded to a supergroup chat", 0, -1009
	f.tg.mu.Unlock()
	f.set(t0.Add(90*time.Minute), append(many, b)...)
	f.n.Tick(ctx)
	f.n.Tick(ctx)
	if c, _ := f.n.config(); c.ChatID != -1009 {
		t.Fatalf("chat id: %d", c.ChatID)
	}
	f.tg.mu.Lock()
	last := f.tg.sent[len(f.tg.sent)-1]
	f.tg.mu.Unlock()
	if last.Get("chat_id") != "-1009" || !strings.Contains(last.Get("text"), "Disk %82 dolu") {
		t.Fatalf("after the move: %v", last)
	}
	f.tg.mu.Lock()
	f.tg.fail, f.tg.migrate = 0, 0
	f.tg.mu.Unlock()

	// The same chat linked again keeps what it was told: nothing is sent afresh.
	before := len(f.tg.texts())
	l, err := f.n.StartLink(ctx, "", true)
	if err != nil {
		t.Fatal(err)
	}
	f.tg.mu.Lock()
	f.tg.updates = nil
	f.tg.mu.Unlock()
	f.tg.message(-1009, "/start "+l.Code, map[string]any{"type": "supergroup", "title": "Nöbet"})
	if _, err := f.n.FinishLink(ctx, "furkan"); err != nil {
		t.Fatal(err)
	}
	f.set(t0.Add(95*time.Minute), append(many, b)...)
	f.n.Tick(ctx)
	if got := f.tg.texts()[before:]; len(got) != 1 || !strings.Contains(got[0], "bağlandı") {
		t.Fatalf("relinked the same chat: %q", got)
	}

	// A settings directory the agent cannot write: said before anything is sent.
	g := newNotifyFixture(t)
	g.n.SecretPath = filepath.Join(g.dir, "dosya", "telegram.json")
	os.WriteFile(filepath.Join(g.dir, "dosya"), []byte("x"), 0o600)
	if _, err := g.n.StartLink(ctx, testBotToken, false); err == nil || !strings.Contains(err.Error(), "kaydedilemiyor") {
		t.Fatalf("not writable: %v", err)
	}
	if len(g.tg.calls) != 0 {
		t.Fatalf("asked Telegram anyway: %v", g.tg.calls)
	}
}
