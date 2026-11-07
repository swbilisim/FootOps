package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

func (f *fakeAgent) Removed(_ context.Context, detail bool) ([]agent.RemovedSite, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]agent.RemovedSite(nil), f.removed...)
	if !detail {
		for i := range out {
			out[i].Bytes, out[i].Files, out[i].DBBytes = 0, 0, 0
		}
	}
	return out, nil
}

func (f *fakeAgent) PurgeRemoved(_ context.Context, id, confirm string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, id+"/"+confirm)
	return "jobpurge", nil
}

func TestRemovedPages(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	if _, body := c.do("GET", "/sites", nil); strings.Contains(body, `href="/removed"`) {
		t.Error("link without removed sites")
	}
	e.agent.removed = []agent.RemovedSite{
		{ID: "posmagaza-20261102-101500", Site: "posmagaza", Group: "musteri", Removed: time.Now().Add(-120 * time.Hour), Database: true,
			Bytes: 3 << 20, Files: 12, DBBytes: 8 << 20, Image: "ghcr.io/x/pos:main"},
		{ID: "eski-20261001-090000", Site: "eski", Group: "musteri", Removed: time.Now().Add(-900 * time.Hour), Database: true, Current: true},
	}
	_, body := c.do("GET", "/sites", nil)
	if !strings.Contains(body, `href="/removed"`) || !strings.Contains(body, "Kaldırılanlar (2)") {
		t.Fatalf("sites page:\n%s", body)
	}
	_, body = c.do("GET", "/removed", nil)
	for _, want := range []string{"posmagaza", "3.0 MiB", "12 dosya", "8.0 MiB", "aynı adla yeni site var", `href="/removed/posmagaza-20261102-101500"`,
		`href="/sites" aria-current="page"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	_, body = c.do("GET", "/removed/posmagaza-20261102-101500", nil)
	if !strings.Contains(body, "son kopyası") || !strings.Contains(body, "7 gün") || !strings.Contains(body, "ghcr.io/x/pos:main") {
		t.Errorf("confirm:\n%s", body)
	}
	_, body = c.do("GET", "/removed/eski-20261001-090000", nil)
	if !strings.Contains(body, "Yalnız eski klasör silinir") {
		t.Error("current site not said")
	}
	// A wrong name: nothing asked of the agent.
	resp, body := c.do("POST", "/removed/posmagaza-20261102-101500", url.Values{"csrf": {tok}, "confirm": {"posmagaz"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "site adını aynen yaz") || len(e.agent.purges) != 0 {
		t.Fatalf("wrong name: %d %v", resp.StatusCode, e.agent.purges)
	}
	resp, _ = c.do("POST", "/removed/posmagaza-20261102-101500", url.Values{"csrf": {tok}, "confirm": {"posmagaza"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobpurge" || e.agent.purges[0] != "posmagaza-20261102-101500/posmagaza" {
		t.Fatalf("purge: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), e.agent.purges)
	}
	// Unknown or malformed ids.
	if resp, _ := c.do("GET", "/removed/yok-20261102-101500", nil); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/removed/..%2Fetc", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("malformed: %d", resp.StatusCode)
	}
}
