package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/remote"
)

// fakeNotify plays the agent's notification API.
type fakeNotify struct {
	status   agent.NotifyStatus
	tokens   []string
	keeps    int
	startErr error
	linkErr  error
	links    int
	cancels  int
	tests    int
	testErr  error
	removes  int
}

func (f *fakeAgent) NotifyStatus(context.Context) (agent.NotifyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nt.status, nil
}

func (f *fakeAgent) NotifyStart(_ context.Context, token string, keep bool, _ agent.Actor) (agent.NotifyLink, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nt.tokens = append(f.nt.tokens, token)
	if keep {
		f.nt.keeps++
	}
	if f.nt.startErr != nil {
		return agent.NotifyLink{}, f.nt.startErr
	}
	l := agent.NotifyLink{Bot: "OrnekBot", Code: "ABCD234567", URL: "https://t.me/OrnekBot?start=ABCD234567", Expires: time.Now().Add(30 * time.Minute)}
	f.nt.status.Link = &l
	return l, nil
}

func (f *fakeAgent) NotifyLink(context.Context, agent.Actor) (agent.NotifyStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nt.links++
	if f.nt.linkErr != nil {
		return agent.NotifyStatus{}, f.nt.linkErr
	}
	f.nt.status = agent.NotifyStatus{Linked: true, Bot: "OrnekBot", Chat: "Ayşe", Since: time.Now()}
	return f.nt.status, nil
}

func (f *fakeAgent) NotifyCancel(context.Context, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nt.cancels++
	f.nt.status.Link = nil
	return nil
}

func (f *fakeAgent) NotifyTest(context.Context, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nt.tests++
	return f.nt.testErr
}

func (f *fakeAgent) NotifyRemove(context.Context, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nt.removes++
	f.nt.status = agent.NotifyStatus{}
	return nil
}

func TestNotifyPageFlow(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.agent.alerts = []agent.Alert{{Key: "backup:failed", Level: "bad", Title: "Yedek alınamadı: magaza (veritabanı)", Link: "/backup", Since: time.Now()},
		{Key: "cpu", Level: "warn", Title: "CPU 5 dakikadır %95", Since: time.Now()}}

	resp, body := c.do("GET", "/notify", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Telegram'ı bağla") || !strings.Contains(body, `href="/notify" aria-current="page"`) {
		t.Fatalf("page: %d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Yedek alınamadı: magaza") || !strings.Contains(body, "yalnız panelde") || !strings.Contains(body, "gider") {
		t.Error("active alerts and where they go are not listed")
	}
	// The menu has the page; the dashboard links to it (phones have no menu entry).
	if _, body := c.do("GET", "/", nil); !strings.Contains(body, `href="/notify"`) || !strings.Contains(body, `href="/sources"`) {
		t.Error("menu or dashboard link missing")
	}

	// No token: refused here. A refused token never comes back in the page.
	resp, _ = c.do("POST", "/notify/telegram", url.Values{"csrf": {tok}, "token": {"  "}})
	if resp.StatusCode != http.StatusUnprocessableEntity || len(e.agent.nt.tokens) != 0 {
		t.Fatalf("empty token: %d %v", resp.StatusCode, e.agent.nt.tokens)
	}
	e.agent.nt.startErr = &agent.Error{Status: 422, Message: "Telegram token'ı geçersiz"}
	resp, body = c.do("POST", "/notify/telegram", url.Values{"csrf": {tok}, "token": {"123:SECRETSECRET"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "geçersiz") || strings.Contains(body, "SECRETSECRET") {
		t.Fatalf("refused token: %d", resp.StatusCode)
	}
	e.agent.nt.startErr = nil
	resp, _ = c.do("POST", "/notify/telegram", url.Values{"csrf": {tok}, "token": {" 123:SECRETSECRET "}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.nt.tokens[1] != "123:SECRETSECRET" {
		t.Fatalf("start: %d %v", resp.StatusCode, e.agent.nt.tokens)
	}
	_, body = c.do("GET", "/notify", nil)
	if !strings.Contains(body, "https://t.me/OrnekBot?start=ABCD234567") || !strings.Contains(body, "ABCD234567") || !strings.Contains(body, "Gönderdim, bağla") {
		t.Fatalf("link step:\n%s", body)
	}
	if strings.Contains(body, "SECRETSECRET") {
		t.Fatal("token in the page")
	}

	// The code has not arrived yet: said on the page, the link stays.
	e.agent.nt.linkErr = &agent.Error{Status: 409, Message: "kod henüz bota ulaşmadı"}
	resp, body = c.do("POST", "/notify/link", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "kod henüz bota ulaşmadı") || !strings.Contains(body, "ABCD234567") {
		t.Fatalf("not yet: %d", resp.StatusCode)
	}
	e.agent.nt.linkErr = nil
	resp, _ = c.do("POST", "/notify/link", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("link: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/notify", nil)
	if !strings.Contains(body, "Telegram bağlandı: @OrnekBot → Ayşe") || !strings.Contains(body, "Deneme mesajı gönder") || strings.Contains(body, `name="token"`) {
		t.Fatalf("linked:\n%s", body)
	}

	// Test message, its failure, another chat, removal.
	c.do("POST", "/notify/test", url.Values{"csrf": {tok}})
	if _, body = c.do("GET", "/notify", nil); !strings.Contains(body, "Deneme mesajı gönderildi") || e.agent.nt.tests != 1 {
		t.Error("test message")
	}
	e.agent.nt.testErr = &agent.Error{Status: 422, Message: "bot bu sohbette engellenmiş"}
	c.do("POST", "/notify/test", url.Values{"csrf": {tok}})
	if _, body = c.do("GET", "/notify", nil); !strings.Contains(body, "gönderilemedi: bot bu sohbette engellenmiş") {
		t.Error("test failure not shown")
	}
	c.do("POST", "/notify/telegram", url.Values{"csrf": {tok}, "keep": {"1"}})
	if e.agent.nt.keeps != 1 || e.agent.nt.tokens[2] != "" {
		t.Errorf("another chat: %d %v", e.agent.nt.keeps, e.agent.nt.tokens)
	}
	c.do("POST", "/notify/cancel", url.Values{"csrf": {tok}})
	c.do("POST", "/notify/remove", url.Values{"csrf": {tok}})
	if _, body = c.do("GET", "/notify", nil); e.agent.nt.cancels != 1 || e.agent.nt.removes != 1 || !strings.Contains(body, "Telegram bağlantısı kaldırıldı") {
		t.Error("cancel / remove")
	}
	// Without the CSRF token nothing happens.
	if resp, _ := c.do("POST", "/notify/remove", url.Values{}); resp.StatusCode != http.StatusForbidden || e.agent.nt.removes != 1 {
		t.Errorf("no csrf: %d", resp.StatusCode)
	}
}

func TestSourcesInMenuAndRemoveUnreachable(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	_, body := c.do("GET", "/sites", nil)
	if !strings.Contains(body, `href="/sources"`) {
		t.Fatal("sites page does not link the sources")
	}
	src := "eskikaynak01"
	e.agent.mig.sources = []agent.SourceView{{Source: remote.Source{ID: src, Label: "eski", Host: "198.51.100.7", Port: 22, User: "root",
		HostKeys: []string{"SHA256:abc"}, Checked: time.Now().Add(-72 * time.Hour)}}}
	_, body = c.do("GET", "/sources/"+src+"/remove", nil)
	if !strings.Contains(body, `name="cleanup" checked`) || !strings.Contains(body, "kaynak sunucu kapatıldıysa kutuyu boş bırak") ||
		!strings.Contains(body, `href="/sources" aria-current="page"`) {
		t.Fatalf("confirm:\n%s", body)
	}
	// The source is gone: the cleanup fails, nothing is removed, the box comes back clear.
	e.agent.mig.removeErr = &agent.Error{Status: 422, Message: "anahtar kaynaktan silinemedi: bağlantı zaman aşımı"}
	resp, body := c.do("POST", "/sources/"+src+"/remove", url.Values{"csrf": {tok}, "cleanup": {"on"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "hiçbir şey silinmedi") || strings.Contains(body, `name="cleanup" checked`) {
		t.Fatalf("unreachable: %d\n%s", resp.StatusCode, body)
	}
	e.agent.mig.removeErr = nil
	resp, _ = c.do("POST", "/sources/"+src+"/remove", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || len(e.agent.mig.removed) != 1 || e.agent.mig.removed[0] != src+"/local" {
		t.Fatalf("removed without cleanup: %d %v", resp.StatusCode, e.agent.mig.removed)
	}
}
