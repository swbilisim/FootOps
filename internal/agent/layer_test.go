package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

// changes builds a layer from "A /path", "C /path", "D /path" lines.
func changes(lines ...string) []docker.Change {
	kinds := map[byte]int{'A': docker.ChangeAdded, 'C': docker.ChangeModified, 'D': docker.ChangeDeleted}
	out := make([]docker.Change, 0, len(lines))
	for _, l := range lines {
		out = append(out, docker.Change{Kind: kinds[l[0]], Path: l[2:]})
	}
	return out
}

func TestLayerFiles(t *testing.T) {
	dirs := map[string]bool{"/app": true, "/app/uploads": true}
	isDir := func(p string) bool { return dirs[p] }
	for _, tc := range []struct {
		name    string
		changes []docker.Change
		point   string
		trees   []string
		want    LayerReport
	}{
		{"uploads the container made", changes("C /app", "C /app/public", "A /app/public/uploads", "A /app/public/uploads/a.jpg",
			"A /app/public/uploads/b.jpg", "A /app/public/uploads/2024", "A /app/public/uploads/2024/c.jpg"), "", nil,
			LayerReport{Files: 3, Paths: []string{"/app/public/uploads — 3 dosya"}}},
		{"uploads into a folder the image ships", changes("C /app", "C /app/uploads", "A /app/uploads/a.jpg", "A /app/uploads/b.jpg",
			"A /app/uploads/c.jpg"), "", nil,
			LayerReport{Files: 3, Paths: []string{"/app/uploads — 3 dosya"}}},
		{"a file of the image changed, one deleted", changes("C /app", "C /app/data.json", "D /app/old.txt"), "", nil,
			LayerReport{Files: 1, Paths: []string{"/app/data.json"}}},
		{"what is made again", changes("C /tmp", "A /tmp/x", "C /run", "A /run/nginx.pid", "C /etc", "C /etc/nginx",
			"C /etc/nginx/conf.d", "C /etc/nginx/conf.d/default.conf", "C /var", "C /var/cache", "A /var/cache/nginx",
			"A /var/cache/nginx/client_temp", "C /app", "A /app/.next", "A /app/.next/cache", "A /app/.next/cache/images/x",
			"C /root", "A /root/.ash_history", "A /root/.wget-hsts", "A /root/.npm", "A /root/.npm/_logs/1.log",
			"C /srv", "A /srv/__pycache__", "A /srv/__pycache__/m.pyc"), "", nil,
			LayerReport{}},
		{"a folder changed by a file made and deleted again", changes("C /app"), "", nil, LayerReport{}},
		{"an empty folder made at start", changes("C /app", "A /app/uploads"), "", nil, LayerReport{}},
		{"the mounts, the mountpoint and a folder about to be filled", changes("C /app", "A /app/data", "C /app/public",
			"A /app/public/uploads", "A /app/public/uploads/a.jpg", "A /app/notes.txt", "C /srv", "A /srv/x"), "/app//data",
			[]string{"/app/public/uploads/", "/srv"},
			LayerReport{Files: 1, Paths: []string{"/app/notes.txt"}}},
		{"the record's folder, not mounted (an update cut short)", changes("C /app", "A /app/uploads", "A /app/uploads/a.jpg"),
			"/app/uploads", nil,
			LayerReport{Files: 1, Paths: []string{"/app/uploads — 1 dosya"}}},
		{"biggest first", changes("C /app", "A /app/z.txt", "A /app/logs", "A /app/logs/1", "A /app/logs/2", "A /app/logs/3",
			"A /app/a.txt"), "", nil,
			LayerReport{Files: 5, Paths: []string{"/app/logs — 3 dosya", "/app — 2 dosya"}}},
	} {
		if got := layerFiles(tc.changes, isDir, tc.point, tc.trees...); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}

	// Twelve places: ten listed, two more.
	var many []string
	for i := 0; i < 12; i++ {
		many = append(many, fmt.Sprintf("C /app/d%02d", i), fmt.Sprintf("A /app/d%02d/f", i))
	}
	r := layerFiles(changes(many...), isDir, "")
	if r.Files != 12 || len(r.Paths) != layerShown || r.More != 2 || r.Paths[0] != "/app/d00/f" || !r.Risky() {
		t.Errorf("many: %+v", r)
	}
	if s := r.Summary(); s != "kalıcı klasör dışında 12 dosya (/app/d00/f, /app/d01/f, /app/d02/f, /app/d03/f, /app/d04/f, /app/d05/f, /app/d06/f, /app/d07/f, /app/d08/f, /app/d09/f ve 2 yer daha)" {
		t.Errorf("summary: %s", s)
	}
}

func TestSiteLayer(t *testing.T) {
	net := newFakeNet()
	o := &SiteOps{Docker: net}
	spec := sites.Spec{Name: "magaza", DataMount: "/app/data"}
	net.layer = map[string][]docker.Change{spec.Container(): changes("C /app", "A /app/data", "A /app/up.txt", "A /app/empty",
		"C /app/public", "A /app/public/x.css")}
	net.mounts = map[string][]string{spec.Container(): {"/app/public"}}
	net.dirs = map[string]bool{"/app/empty": true}
	if r := o.Layer(context.Background(), spec); r.Files != 1 || r.Paths[0] != "/app/up.txt" {
		t.Errorf("layer: %+v", r)
	}
	// No container: nothing to lose. Unreadable: risky, with why.
	net.layerErr = &docker.APIError{Status: 404, Message: "no such container"}
	if r := o.Layer(context.Background(), spec); r.Risky() {
		t.Errorf("no container: %+v", r)
	}
	net.layerErr = errors.New("dockerd yanıt vermedi")
	if r := o.Layer(context.Background(), spec); !r.Risky() || r.Summary() != "container'ın katmanı okunamadı: dockerd yanıt vermedi" {
		t.Errorf("unreadable: %+v", r)
	}
}

// A redeploy, a move and an update that makes the container anew are refused while its
// layer holds files, until someone says go on; an update Caddy alone takes is not.
func TestLayerRefusals(t *testing.T) {
	e := createdSite(t)
	recDir := t.TempDir()
	jobs := NewJobs()
	c, _ := startServer(t, 999, func(s *Server) {
		s.Sites, s.Jobs = e.ops, jobs
		s.Data = &DataOps{RecoveryDir: recDir}
		s.Audit = NewAudit(filepath.Join(recDir, "audit.jsonl"))
	})
	spec, _ := e.ops.Store.Get("enisparta")
	e.net.layer = map[string][]docker.Change{spec.Container(): changes("C /app", "A /app/uploads", "A /app/notes.txt",
		"C /app/public", "A /app/public/img", "A /app/public/img/a.png")}
	who := Actor{User: "u"}
	refused := func(err error) bool {
		var ae *Error
		return errors.As(err, &ae) && ae.Status == 409 && ae.Layer != nil && ae.Layer.Files == 2
	}
	ran := func(id string, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		waitJob(t, jobs, id)
	}

	if _, err := c.SiteJob(ctx, "enisparta", "redeploy", false, who); !refused(err) {
		t.Errorf("redeploy: %v", err)
	}
	ran(c.SiteJob(ctx, "enisparta", "redeploy", true, who))

	// The domains and the port are Caddy's: the container stays.
	u := SiteUpdate{Domains: []string{"enisparta.com"}, Image: spec.Image, Port: 3001, MemoryMB: spec.MemoryMB, DataMount: spec.DataMount}
	ran(c.UpdateSite(ctx, "enisparta", UpdateSiteRequest{Update: u, Actor: who}))
	u.Image = "ghcr.io/swbilisim/enisparta:v2"
	if _, err := c.UpdateSite(ctx, "enisparta", UpdateSiteRequest{Update: u, Actor: who}); !refused(err) {
		t.Errorf("update of the image: %v", err)
	}
	ran(c.UpdateSite(ctx, "enisparta", UpdateSiteRequest{Update: u, Actor: who, Force: true}))

	if _, err := c.MoveSite(ctx, "enisparta", "kisisel", "enisparta", false, who); !refused(err) {
		t.Errorf("move: %v", err)
	}
	ran(c.MoveSite(ctx, "enisparta", "kisisel", "enisparta", true, who))
}

// unmounted makes the test site one without a persistent folder (its record and compose
// file), as a site is before it has one.
func unmounted(t *testing.T, e *opsEnv) sites.Spec {
	t.Helper()
	spec, _ := e.ops.Store.Get("enisparta")
	env, err := e.ops.Store.Env(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.DataMount = ""
	if err := e.ops.Store.WriteCompose(spec, env); err != nil {
		t.Fatal(err)
	}
	if err := e.ops.Store.WriteMeta(spec); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(e.root, spec.Dir(), sites.DataDir))
	return spec
}

// A first persistent folder an update gives a site is filled from the container: what
// is under it is not at stake. Moving the folder a site has fills nothing.
func TestUpdateLayerKeepsTheNewFolder(t *testing.T) {
	e := createdSite(t)
	spec, _ := e.ops.Store.Get("enisparta")
	e.net.layer = map[string][]docker.Change{spec.Container(): changes("C /app", "A /app/notes.txt", "C /app/public",
		"A /app/public/img", "A /app/public/img/a.png")}
	u := SiteUpdate{Domains: spec.Domains, Image: spec.Image, Port: spec.Port, MemoryMB: spec.MemoryMB, DataMount: "/app/public/img"}
	// It has /app/uploads: that folder is mounted at img as it is, the container's img goes.
	if r, err := e.ops.UpdateLayer(ctx, "enisparta", u); err != nil || r.Files != 2 {
		t.Errorf("folder moved: %+v %v", r, err)
	}
	spec = unmounted(t, e)
	if r, err := e.ops.UpdateLayer(ctx, "enisparta", u); err != nil || r.Files != 1 || r.Paths[0] != "/app/notes.txt" {
		t.Errorf("first folder: %+v %v", r, err)
	}
	// Caddy's part only (the domains): the container stays, nothing is at stake.
	u = SiteUpdate{Domains: []string{"enisparta.com"}, Image: spec.Image, Port: spec.Port, MemoryMB: spec.MemoryMB}
	if r, err := e.ops.UpdateLayer(ctx, "enisparta", u); err != nil || r.Risky() {
		t.Errorf("domains: %+v %v", r, err)
	}
	// No change at all: the update's own problem, not the layer's.
	u.Domains = spec.Domains
	if _, err := e.ops.UpdateLayer(ctx, "enisparta", u); err == nil {
		t.Error("no change: no error")
	}
}

// Start (a restore's, an unpark's) never makes a container anew whose layer holds files,
// even when its compose file is not what FootOps writes now: it starts it as it is.
// With nothing at stake, compose rewrites the file and starts it, as before.
func TestStartAsIs(t *testing.T) {
	e := createdSite(t)
	spec, _ := e.ops.Store.Get("enisparta")
	file := filepath.Join(e.root, spec.Dir(), sites.ComposeFile)
	stale := strings.Replace(readFile(t, file), "interval: "+sites.HealthInterval, "interval: 30s", 1)
	os.WriteFile(file, []byte(stale), 0o600)
	e.net.layer = map[string][]docker.Change{spec.Container(): changes("C /app", "A /app/notes.txt")}
	if err := e.ops.Start(ctx, "enisparta"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.net.log, "start "+spec.Container()) || e.run.ran("compose.yaml up") != 0 || readFile(t, file) != stale {
		t.Errorf("files at stake: docker %v, commands %v", e.net.log, e.run.calls)
	}

	e.net.layer, e.net.log, e.run.calls = nil, nil, nil
	if err := e.ops.Start(ctx, "enisparta"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.net.log, "start "+spec.Container()) || e.run.ran("compose.yaml up --detach") != 1 || readFile(t, file) == stale {
		t.Errorf("nothing at stake: docker %v, commands %v", e.net.log, e.run.calls)
	}
}

// An update that gives a site its first persistent folder stops the site, fills the
// folder with what the container held there (which the folder would hide), whole or not
// at all, and makes the container anew with it; a path the container lacks leaves the
// folder empty.
func TestUpdateFillsTheNewFolder(t *testing.T) {
	e := createdSite(t)
	spec := unmounted(t, e)
	data := filepath.Join(e.root, spec.Dir(), sites.DataDir)
	e.run.hook = func(cmd string) {
		if f := strings.Fields(cmd); len(f) == 4 && f[0] == "docker" && f[1] == "cp" {
			os.MkdirAll(filepath.Join(f[3], "2024"), 0o755)
			os.WriteFile(filepath.Join(f[3], "a.jpg"), []byte("jpeg"), 0o644)
			os.WriteFile(filepath.Join(f[3], "2024", "b.jpg"), []byte("jpeg!"), 0o644)
		}
	}
	u := SiteUpdate{Domains: spec.Domains, Image: spec.Image, Port: spec.Port, MemoryMB: spec.MemoryMB, DataMount: "/app/public/img"}
	if err := e.ops.Update(ctx, "enisparta", u, false, e.progress); err != nil {
		t.Fatal(err)
	}
	cp := "docker cp " + spec.Container() + ":/app/public/img/. " + data + ".copy"
	stop, copied, up := callIndex(e.run.calls, "compose.yaml stop"), callIndex(e.run.calls, cp), callIndex(e.run.calls, "compose.yaml up")
	if e.run.ran(cp) != 1 || stop < 0 || stop > copied || copied > up || readFile(t, filepath.Join(data, "2024", "b.jpg")) != "jpeg!" ||
		!slices.Contains(e.steps, "✓ Container'daki /app/public/img kalıcı klasöre taşındı: 2 dosya, 9 B") {
		t.Errorf("filled: %v\n%v", e.run.calls, e.steps)
	}
	if _, err := os.Stat(data + ".copy"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the copy's folder is left: %v", err)
	}

	// The container has nothing there: an empty folder.
	spec = unmounted(t, e)
	e.run.hook, e.steps = nil, nil
	e.run.fail = map[string]error{"docker cp": errors.New("Error response from daemon: Could not find the file /app/files in container enisparta")}
	u.DataMount = "/app/files"
	if err := e.ops.Update(ctx, "enisparta", u, false, e.progress); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(data); err != nil || len(entries) != 0 || slices.ContainsFunc(e.steps, func(s string) bool { return strings.Contains(s, "taşındı") }) {
		t.Errorf("nothing there: %v %v %v", entries, err, e.steps)
	}
}

// An update that filled a first persistent folder fails once its new container is made:
// the files go back into the old container, made anew without the folder, and the folder
// goes. When they cannot go back, the folder stays and the rollback says where they are.
func TestUpdateFailedFillGoesBack(t *testing.T) {
	e := createdSite(t)
	spec := unmounted(t, e)
	data := filepath.Join(e.root, spec.Dir(), sites.DataDir)
	e.run.hook = func(cmd string) {
		if f := strings.Fields(cmd); len(f) == 4 && f[0] == "docker" && f[1] == "cp" {
			os.WriteFile(filepath.Join(f[3], "a.jpg"), []byte("jpeg"), 0o644)
		}
	}
	e.ops.https = func(context.Context, string) (int, error) { return 502, nil } // not up through Caddy
	u := SiteUpdate{Domains: spec.Domains, Image: spec.Image, Port: spec.Port, MemoryMB: spec.MemoryMB, DataMount: "/app/public/img"}
	if err := e.ops.Update(ctx, "enisparta", u, false, e.progress); err == nil {
		t.Fatal("expected an error")
	}
	back := "docker cp --archive " + data + "/. " + spec.Container() + ":/app/public/img"
	if _, err := os.Stat(data); e.run.ran(back) != 1 || !errors.Is(err, fs.ErrNotExist) ||
		!slices.Contains(e.steps, "✓ Kalıcı klasöre taşınan dosyalar container'a geri kondu") {
		t.Errorf("back: %v\n%v", e.run.calls, e.steps)
	}

	spec = unmounted(t, e)
	e.steps = nil
	e.run.fail = map[string]error{"docker cp --archive": errors.New("no space left on device")}
	if err := e.ops.Update(ctx, "enisparta", u, false, e.progress); err == nil {
		t.Fatal("expected an error")
	}
	if readFile(t, filepath.Join(data, "a.jpg")) != "jpeg" || !slices.ContainsFunc(e.steps, func(s string) bool {
		return strings.HasPrefix(s, "✗ Geri alma tamamlanamadı") && strings.Contains(s, data+" klasöründeler")
	}) {
		t.Errorf("not back: %v", e.steps)
	}
}
