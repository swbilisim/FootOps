package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/sites"
)

// created returns an ops env with the enisparta site in place.
func created(t *testing.T) *opsEnv {
	t.Helper()
	e := newOps(t)
	err := e.ops.Create(context.Background(), specFor("enisparta"), map[string]string{"NODE_ENV": "production", "OLD": "x"}, e.progress)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.steps, e.run.calls = nil, nil
	return e
}

func updateFrom(s sites.Spec) SiteUpdate {
	return SiteUpdate{Domains: s.Domains, Image: s.Image, Port: s.Port, MemoryMB: s.MemoryMB, DataMount: s.DataMount}
}

func TestUpdateSite(t *testing.T) {
	e := created(t)
	dir := filepath.Join(e.root, "/srv/musteri/enisparta")
	before := readFile(t, filepath.Join(dir, "compose.yaml"))
	uri := before[strings.Index(before, "MONGODB_URI"):]
	uri = uri[:strings.Index(uri, "\n")]

	u := updateFrom(specFor("enisparta"))
	u.Domains = []string{"enisparta.com", "shop.enisparta.com"}
	u.MemoryMB, u.Port, u.Image = 512, 8080, "ghcr.io/swbilisim/enisparta:v2"
	u.Set = map[string]string{"NODE_ENV": "staging", "NEW_KEY": "s3cr$t"}
	u.Unset = []string{"OLD", "MISSING"}

	changes, err := e.ops.CheckUpdate("enisparta", u)
	if err != nil {
		t.Fatal(err)
	}
	summary := strings.Join(changes, "; ")
	for _, want := range []string{"+NEW_KEY", "~NODE_ENV", "-OLD", "shop.enisparta.com", "port 3000 → 8080", "bellek 256 → 512 MB", "imaj:"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q lacks %q", summary, want)
		}
	}
	if strings.Contains(summary, "staging") || strings.Contains(summary, "s3cr") {
		t.Errorf("summary leaks values: %q", summary)
	}

	if err := e.ops.Update(context.Background(), "enisparta", u, e.progress); err != nil {
		t.Fatalf("update: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	compose := readFile(t, filepath.Join(dir, "compose.yaml"))
	for _, want := range []string{uri, `NODE_ENV: "staging"`, `NEW_KEY: "s3cr$$t"`, "mem_limit: 512m", "ghcr.io/swbilisim/enisparta:v2"} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose lacks %q:\n%s", want, compose)
		}
	}
	if strings.Contains(compose, "OLD:") {
		t.Error("removed variable still present")
	}
	spec, _ := e.ops.Store.Get("enisparta")
	if strings.Join(spec.EnvKeys, ",") != "NEW_KEY,NODE_ENV" || spec.Port != 8080 || !spec.CreatedAt.Before(spec.UpdatedAt) && !spec.CreatedAt.Equal(spec.UpdatedAt) {
		t.Errorf("spec = %+v", spec)
	}
	caddy := readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"))
	if !strings.Contains(caddy, "https://shop.enisparta.com") || strings.Contains(caddy, "www.enisparta.com") || !strings.Contains(caddy, "reverse_proxy enisparta:8080") {
		t.Errorf("caddy:\n%s", caddy)
	}
	if e.run.ran("pull --quiet") != 1 || e.run.ran("caddy reload") != 1 {
		t.Errorf("calls:\n%s", strings.Join(e.run.calls, "\n"))
	}
}

func TestUpdateOnlyEnvSkipsPullAndCaddy(t *testing.T) {
	e := created(t)
	e.net.restarts = 4 // an old container with restarts behind it must not fail the update
	u := updateFrom(specFor("enisparta"))
	u.Set = map[string]string{"NODE_ENV": "staging"}
	if err := e.ops.Update(context.Background(), "enisparta", u, e.progress); err != nil {
		t.Fatalf("update: %v", err)
	}
	if e.run.ran("pull") != 0 || e.run.ran("caddy") != 0 {
		t.Errorf("unneeded work:\n%s", strings.Join(e.run.calls, "\n"))
	}
}

func TestUpdateRejects(t *testing.T) {
	e := created(t)
	other := specFor("other")
	other.Domains = []string{"other.enisparta.com"}
	other.Database = false
	other.DataMount = ""
	if err := e.ops.Create(context.Background(), other, nil, e.progress); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		mod  func(*SiteUpdate)
		want string
	}{
		"no change":      {func(*SiteUpdate) {}, "değişiklik yok"},
		"taken domain":   {func(u *SiteUpdate) { u.Domains = append(u.Domains, "other.enisparta.com") }, "other sitesinde"},
		"no certificate": {func(u *SiteUpdate) { u.Domains = []string{"example.org"} }, "sertifika yok"},
		"db var set":     {func(u *SiteUpdate) { u.Set = map[string]string{"MONGODB_URI": "x"} }, "veritabanı bağlantısı için ayrıldı"},
		"db var unset":   {func(u *SiteUpdate) { u.Unset = []string{"MONGODB_URI"} }, "silinemez"},
		"bad memory":     {func(u *SiteUpdate) { u.MemoryMB = 10 }, "bellek sınırı"},
		"CR in value":    {func(u *SiteUpdate) { u.Set = map[string]string{"A": "x\ry"} }, "satır başı"},
	} {
		u := updateFrom(specFor("enisparta"))
		tc.mod(&u)
		_, err := e.ops.CheckUpdate("enisparta", u)
		var ve *sites.ValidationError
		if !errors.As(err, &ve) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, err := e.ops.CheckUpdate("nope", updateFrom(specFor("nope"))); !errors.Is(err, sites.ErrNotFound) {
		t.Errorf("unknown site: %v", err)
	}
}

func TestUpdateRollsBack(t *testing.T) {
	ups := map[string]int{"image missing": 1} // the new container never started
	for name, breakIt := range map[string]func(*opsEnv){
		"app crashes": func(e *opsEnv) {
			e.net.running = false
			// The old settings work again: the second "up" brings the old container back.
			ups := 0
			e.run.hook = func(cmd string) {
				if strings.Contains(cmd, " up ") {
					if ups++; ups == 2 {
						e.net.mu.Lock()
						e.net.running = true
						e.net.mu.Unlock()
					}
				}
			}
		},
		"image missing":    func(e *opsEnv) { e.run.fail["pull"] = errors.New("manifest unknown") },
		"caddy invalid":    func(e *opsEnv) { e.run.fail["caddy validate"] = errors.New("exit status 1") },
		"site unreachable": func(e *opsEnv) { e.ops.https = func(context.Context, string) (int, error) { return 502, nil } },
	} {
		t.Run(name, func(t *testing.T) {
			e := created(t)
			dir := filepath.Join(e.root, "/srv/musteri/enisparta")
			caddyPath := filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")
			oldCompose, oldMeta, oldCaddy := readFile(t, filepath.Join(dir, "compose.yaml")), readFile(t, filepath.Join(dir, "footops.json")), readFile(t, caddyPath)

			e.run.out["--entrypoint id ghcr.io/swbilisim/enisparta:v2"] = fmt.Sprintf("uid=%d gid=%d(x)", os.Getuid(), os.Getgid())
			breakIt(e)
			u := updateFrom(specFor("enisparta"))
			u.Domains = []string{"enisparta.com"}
			u.Image = "ghcr.io/swbilisim/enisparta:v2"
			u.DataMount = "/srv/files" // a new data directory: removed again on failure
			os.RemoveAll(filepath.Join(dir, "data"))
			u.Set = map[string]string{"NODE_ENV": "broken"}
			if err := e.ops.Update(context.Background(), "enisparta", u, e.progress); err == nil {
				t.Fatal("expected error")
			}
			if readFile(t, filepath.Join(dir, "compose.yaml")) != oldCompose || readFile(t, filepath.Join(dir, "footops.json")) != oldMeta {
				t.Error("files not restored")
			}
			if got, err := os.ReadFile(caddyPath); err != nil || string(got) != oldCaddy {
				t.Errorf("caddy not restored: %v\n%s", err, got)
			}
			// The old container is started again with the old files.
			want := 2
			if n, ok := ups[name]; ok {
				want = n
			}
			if n := e.run.ran("compose --project-name enisparta --file /srv/musteri/enisparta/compose.yaml up"); n != want {
				t.Errorf("compose up ran %d times, want %d:\n%s", n, want, strings.Join(e.run.calls, "\n"))
			}
			if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "geri yüklendi") {
				t.Errorf("last step = %q\n%s", last, strings.Join(e.steps, "\n"))
			}
			if _, err := os.Stat(filepath.Join(dir, "data")); err == nil {
				t.Error("data directory created by the failed update was left behind")
			}
		})
	}
}

func TestUpdateEndpoint(t *testing.T) {
	e := created(t)
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Jobs = e.ops, NewJobs() })
	u := updateFrom(specFor("enisparta"))
	if _, err := c.UpdateSite(ctx, "enisparta", UpdateSiteRequest{Update: u}); err == nil || len(err.(*Error).Problems) == 0 {
		t.Errorf("no change accepted: %v", err)
	}
	if _, err := c.UpdateSite(ctx, "nope", UpdateSiteRequest{Update: u}); err == nil || err.(*Error).Status != 404 {
		t.Errorf("unknown site: %v", err)
	}
	u.MemoryMB = 384
	u.Set = map[string]string{"SECRET": "hunter2"}
	id, err := c.UpdateSite(ctx, "enisparta", UpdateSiteRequest{Update: u, Actor: Actor{User: "furkan"}})
	if err != nil {
		t.Fatal(err)
	}
	// The audit entry is written once the job has finished.
	var entry *AuditEntry
	for i := 0; i < 300 && entry == nil; i++ {
		time.Sleep(10 * time.Millisecond)
		entries, _ := s.Audit.Tail(5)
		for _, a := range entries {
			if a.Action == "site.update" {
				entry = &a
			}
		}
	}
	if j, _ := c.Job(ctx, id); !j.Done || j.Err != "" || j.Kind != "site.update" {
		t.Fatalf("job = %+v", j)
	}
	if entry == nil || !entry.OK || !strings.Contains(entry.Detail, "+SECRET") || !strings.Contains(entry.Detail, "bellek 256 → 384 MB") ||
		strings.Contains(entry.Detail, "hunter2") || entry.Actor.User != "furkan" {
		t.Errorf("audit = %+v", entry)
	}
}

func TestUpdateRollbackRestoresCaddyEvenIfContainerFails(t *testing.T) {
	e := created(t)
	caddyPath := filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")
	oldCaddy := readFile(t, caddyPath)
	e.ops.https = func(context.Context, string) (int, error) { return 502, nil }
	ups := 0
	e.run.hook = func(cmd string) {
		if strings.Contains(cmd, " up ") {
			if ups++; ups == 2 {
				e.run.fail[" up "] = errors.New("old container failed")
			}
		}
	}
	u := updateFrom(specFor("enisparta"))
	u.Domains = []string{"enisparta.com"}
	if err := e.ops.Update(context.Background(), "enisparta", u, e.progress); err == nil {
		t.Fatal("expected error")
	}
	if readFile(t, caddyPath) != oldCaddy {
		t.Error("Caddy left with the new domains")
	}
	if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "Geri alma tamamlanamadı") || !strings.Contains(last, "old container failed") {
		t.Errorf("last step = %q", last)
	}
}
