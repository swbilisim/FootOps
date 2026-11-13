package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// A new server's dashboard opens with what it still needs (Faz R3), each step linked
// and ticked off from the server's state; the list goes once a certificate, a site and
// a backup repository are there, whatever the optional steps say.
func TestGettingStarted(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	e.agent.noSites = true

	_, body := c.do("GET", "/", nil)
	card := section(body)
	for _, want := range []string{"Başlarken", `href="/certs"`, `href="/registries"`, `href="/sites/new/project"`,
		`href="/backup"`, `href="/notify"`, `href="https://github.com/swbilisim/FootOps/blob/main/docs/INSTALL-TR.md" rel="noopener noreferrer"`} {
		if !strings.Contains(card, want) {
			t.Errorf("new server: list misses %q", want)
		}
	}
	if n, o := strings.Count(card, `<span class="badge warn">yapılacak</span>`), strings.Count(card, `<span class="badge">isteğe bağlı</span>`); n != 3 || o != 2 {
		t.Errorf("new server: %d to do, %d optional; want 3 and 2", n, o)
	}

	// Steps tick off as they are done; a connected repository whose password is not
	// confirmed takes no backups yet.
	e.agent.certs = []sites.Cert{{Names: []string{"*.example.com", "example.com"}}}
	e.agent.f6.regs = []agent.RegistryLogin{{Host: "ghcr.io", User: "o"}}
	e.agent.bk.status.Configured = true
	_, body = c.do("GET", "/", nil)
	card = section(body)
	if strings.Count(card, `<span class="badge ok">tamam</span>`) != 2 || !strings.Contains(card, `<span class="badge warn">yarım</span>`) ||
		strings.Contains(card, `href="/certs"`) || strings.Contains(card, `href="/registries"`) {
		t.Errorf("ticked steps:\n%s", card)
	}

	// Backing up without a site yet: the site is what is left.
	e.agent.bk.status.Settings = agent.DefaultBackupSettings()
	e.agent.bk.status.Settings.Confirmed = e.clock.Add(-time.Hour)
	_, body = c.do("GET", "/", nil)
	if card = section(body); strings.Count(card, `<span class="badge ok">tamam</span>`) != 3 || !strings.Contains(card, `href="/sites/new/project"`) {
		t.Errorf("backing up, no site:\n%s", card)
	}

	// A site and a confirmed repository: the list goes, Telegram or not. A server with
	// sites asks for nothing more than its backup status.
	e.agent.noSites = false
	e.agent.certs = nil // a site means a certificate
	e.agent.certCalls = 0
	if _, body = c.do("GET", "/", nil); strings.Contains(body, "Başlarken") || e.agent.certCalls != 0 {
		t.Errorf("set-up server: list shown or %d certificate reads", e.agent.certCalls)
	}

	// Without a confirmed repository a server with sites still shows it, the site and
	// its certificate ticked.
	e.agent.bk.status.Settings.Confirmed = time.Time{}
	_, body = c.do("GET", "/", nil)
	card = section(body)
	if card == "" || strings.Contains(card, `href="/certs"`) || strings.Contains(card, `href="/sites/new/project"`) || !strings.Contains(card, `href="/backup"`) {
		t.Errorf("site without backups:\n%s", card)
	}

	// A rehearsal has no repository on purpose; a live refresh leaves the list as it is.
	e.agent.bk.status.Rehearsal = true
	if _, body = c.do("GET", "/", nil); strings.Contains(body, "Başlarken") {
		t.Error("a rehearsal shows the list")
	}
	e.agent.bk.status.Rehearsal = false
	e.agent.certCalls = 0
	c.header = http.Header{liveHeader: {"1"}}
	if _, body = c.do("GET", "/", nil); strings.Contains(body, "Başlarken") || e.agent.certCalls != 0 {
		t.Error("a live refresh builds the list")
	}
}

// section is the Başlarken card of a page, its hint included; "" if there is none.
func section(body string) string {
	if i := strings.Index(body, `id="baslarken"`); i >= 0 {
		if j := strings.Index(body[i:], "</p>"); j >= 0 {
			return body[i : i+j]
		}
	}
	return ""
}
