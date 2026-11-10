package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
)

// purgeMongo adds listing and dropping databases to the data tests' MongoDB.
type purgeMongo struct {
	*statsOnly
	mu      sync.Mutex
	dbs     map[string]int64
	dropped []string
	fail    error
}

func (m *purgeMongo) Databases(context.Context) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return nil, m.fail
	}
	out := map[string]int64{}
	for k, v := range m.dbs {
		out[k] = v
	}
	return out, nil
}

func (m *purgeMongo) DropSiteDB(_ context.Context, db string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped = append(m.dropped, db)
	delete(m.dbs, db)
	return nil
}

// gonePurge answers Docker's inspect like dockerd: containers in gone are not there.
type gonePurge struct {
	*fakeNet
	mu   sync.Mutex
	gone map[string]bool
}

func (g *gonePurge) Inspect(ctx context.Context, name string) (docker.Inspect, error) {
	g.mu.Lock()
	gone := g.gone[name]
	g.mu.Unlock()
	if gone {
		return docker.Inspect{}, &docker.APIError{Status: http.StatusNotFound, Message: "No such container: " + name}
	}
	return g.fakeNet.Inspect(ctx, name)
}

// removedEnv is a data environment with "magaza" removed (its folder and database kept).
func removedEnv(t *testing.T) (*dataEnv, *purgeMongo, RemovedSite) {
	t.Helper()
	d := newData(t)
	pm := &purgeMongo{statsOnly: d.data.Mongo.(*statsOnly), dbs: map[string]int64{"magaza": 81920, "admin": 4096}}
	d.data.Mongo = pm
	d.data.Docker = &gonePurge{fakeNet: d.net, gone: map[string]bool{"magaza": true}}
	d.data.DBEditsDir, d.data.MigrationsDir = filepath.Join(d.root, "dbedits"), filepath.Join(d.root, "migrations")
	for _, dir := range []string{d.data.DBEditsDir, d.data.MigrationsDir} {
		os.MkdirAll(filepath.Join(dir, "magaza"), 0o700)
		os.WriteFile(filepath.Join(dir, "magaza", "x.json"), []byte("{}"), 0o600)
	}
	os.WriteFile(filepath.Join(d.root, "srv/musteri/magaza/data/logo.png"), []byte("png-bytes"), 0o644)
	d.ops.now = func() time.Time { return time.Date(2026, 10, 30, 12, 13, 14, 0, time.UTC) }
	if err := d.ops.Remove(context.Background(), "magaza", d.progress); err != nil {
		t.Fatal(err)
	}
	list, err := d.data.Removed(context.Background(), true)
	if err != nil || len(list) != 1 {
		t.Fatalf("removed: %+v %v", list, err)
	}
	return d, pm, list[0]
}

func TestRemovedList(t *testing.T) {
	d, pm, r := removedEnv(t)
	if r.ID != "magaza-20261030-121314" || r.Site != "magaza" || r.Group != "musteri" || !r.Database || r.Current ||
		r.DBBytes != 81920 || r.Files == 0 || r.Bytes < int64(len("png-bytes")) || r.Image != "ghcr.io/swbilisim/enisparta:main" ||
		!r.Removed.Equal(time.Date(2026, 10, 30, 12, 13, 14, 0, time.UTC)) {
		t.Fatalf("entry: %+v", r)
	}
	// Without detail: no sizes, no MongoDB.
	pm.fail = errors.New("mongo kapalı")
	list, err := d.data.Removed(context.Background(), false)
	if err != nil || len(list) != 1 || list[0].Bytes != 0 || list[0].DBErr != "" {
		t.Fatalf("plain list: %+v %v", list, err)
	}
	list, _ = d.data.Removed(context.Background(), true)
	if list[0].DBErr == "" {
		t.Fatalf("mongo down not said: %+v", list[0])
	}
	// Directories that are no removed site are left out.
	base := filepath.Join(d.root, "srv/musteri/_removed")
	os.MkdirAll(filepath.Join(base, "elle-yapilmis"), 0o700)
	os.WriteFile(filepath.Join(base, "dosya-20261030-121314"), nil, 0o600)
	if list, _ := d.data.Removed(context.Background(), false); len(list) != 1 {
		t.Fatalf("stray entries listed: %+v", list)
	}
}

func TestPurge(t *testing.T) {
	d, pm, r := removedEnv(t)
	ctx := context.Background()
	dir := filepath.Join(d.root, "srv/musteri/_removed", r.ID)
	detail, err := d.data.Purge(ctx, d.task, r, Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(d.task.job.Steps, "\n")
	if len(pm.dropped) != 1 || pm.dropped[0] != "magaza" {
		t.Errorf("dropped: %v", pm.dropped)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("folder still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(dir)); !os.IsNotExist(err) {
		t.Errorf("empty _removed kept: %v", err)
	}
	// It was the group's last site: the group goes with it.
	if _, err := os.Stat(filepath.Join(d.root, "srv/musteri")); !os.IsNotExist(err) || !strings.Contains(steps, "grubun klasörü de silindi") {
		t.Errorf("empty group kept: %v\n%s", err, steps)
	}
	for _, base := range []string{d.data.DBEditsDir, d.data.MigrationsDir} {
		if _, err := os.Stat(filepath.Join(base, "magaza")); !os.IsNotExist(err) {
			t.Errorf("%s kept", base)
		}
	}
	// The last copy is there, made before the drop.
	ents, _ := os.ReadDir(filepath.Join(d.data.SnapshotDir, "magaza"))
	if len(ents) != 1 {
		t.Fatalf("last copy: %v", ents)
	}
	snap, err := d.data.readSnapshot("magaza", ents[0].Name())
	if err != nil || snap.Reason != "before-purge" || snap.Actor != "furkan" {
		t.Fatalf("copy: %+v %v", snap, err)
	}
	if strings.Index(steps, "Kopya hazır") > strings.Index(steps, "veritabanı ve kullanıcıları silindi") {
		t.Errorf("dropped before the copy:\n%s", steps)
	}
	if !strings.Contains(detail, "son kopya "+snap.ID) || !strings.Contains(detail, "veritabanı silindi") || !strings.Contains(detail, "imaj silindi") {
		t.Errorf("detail: %s", detail)
	}
	if !strings.Contains(strings.Join(d.run.calls, "\n"), "docker image rm ghcr.io/swbilisim/enisparta:main") {
		t.Errorf("image not removed: %v", d.run.calls)
	}
	// The copy was made as the site's own user, with a password root gave it just then
	// (the kept one may not work any more).
	if calls := strings.Join(pm.calls, ","); !strings.Contains(calls, "set magaza magaza") {
		t.Errorf("no temporary access: %s", calls)
	}
	// Gone now.
	if list, _ := d.data.Removed(ctx, false); len(list) != 0 {
		t.Errorf("still listed: %+v", list)
	}
	if _, err := d.data.Purge(ctx, d.task, r, Actor{}); !errors.Is(err, errRemovedGone) {
		t.Errorf("twice: %v", err)
	}

	// The last copy of a site removed for good expires like the rest (the newest of a
	// site that exists is kept however old).
	d.now = d.now.Add(SnapshotKeep + 30*24*time.Hour)
	if gone := d.data.SweepSnapshots(false); len(gone) != 1 || !strings.HasPrefix(gone[0], "magaza/") {
		t.Errorf("sweep: %v", gone)
	}
}

func TestPurgeKeepsWhatIsNotItsOwn(t *testing.T) {
	d, pm, r := removedEnv(t)
	ctx := context.Background()
	// A site of the same name again: its database, records and copies are its own.
	sp := specFor("magaza")
	if err := d.ops.Create(ctx, sp, nil, d.progress); err != nil {
		t.Fatal(err)
	}
	list, _ := d.data.Removed(ctx, true)
	if len(list) != 1 || !list[0].Current {
		t.Fatalf("current: %+v", list)
	}
	detail, err := d.data.Purge(ctx, d.task, list[0], Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pm.dropped) != 0 || !strings.Contains(detail, "dokunulmadı") {
		t.Errorf("touched the new site's database: %v %s", pm.dropped, detail)
	}
	if _, err := os.Stat(filepath.Join(d.data.DBEditsDir, "magaza")); err != nil {
		t.Errorf("new site's records removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.root, "srv/musteri/_removed", r.ID)); !os.IsNotExist(err) {
		t.Errorf("old folder kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(d.root, "srv/musteri/magaza")); err != nil {
		t.Errorf("new site's folder: %v", err)
	}
	if steps := strings.Join(d.task.job.Steps, "\n"); strings.Contains(steps, "grubun klasörü") {
		t.Errorf("a group with a site said to go:\n%s", steps)
	}
}

func TestPurgeStopsEarly(t *testing.T) {
	ctx := context.Background()
	// The copy fails: nothing goes.
	d, pm, r := removedEnv(t)
	d.stream.err = errors.New("mongodump: bağlantı koptu")
	if _, err := d.data.Purge(ctx, d.task, r, Actor{}); err == nil || !strings.Contains(err.Error(), "hiçbir şey silinmedi") {
		t.Fatalf("copy failed: %v", err)
	}
	if len(pm.dropped) != 0 {
		t.Errorf("dropped without a copy")
	}
	if _, err := os.Stat(filepath.Join(d.root, "srv/musteri/_removed", r.ID)); err != nil {
		t.Errorf("folder removed: %v", err)
	}
	// MongoDB cannot be asked: nothing goes.
	d.stream.err = nil
	pm.fail = errors.New("bağlanılamadı")
	if _, err := d.data.Purge(ctx, d.task, r, Actor{}); err == nil || !strings.Contains(err.Error(), "hiçbir şey silinmedi") {
		t.Fatalf("mongo down: %v", err)
	}
	pm.fail = nil
	// The definition is unreadable and a database of that name exists: nothing goes.
	meta := filepath.Join(d.root, "srv/musteri/_removed", r.ID, "footops.json")
	os.WriteFile(meta, []byte("{bozuk"), 0o600)
	list, _ := d.data.Removed(ctx, true)
	if list[0].Broken == "" {
		t.Fatalf("broken not said: %+v", list[0])
	}
	if _, err := d.data.Purge(ctx, d.task, list[0], Actor{}); err == nil || !strings.Contains(err.Error(), "elle bakılmalı") || len(pm.dropped) != 0 {
		t.Fatalf("broken with a database: %v %v", err, pm.dropped)
	}
	// Without one, the folder alone goes.
	delete(pm.dbs, "magaza")
	if _, err := d.data.Purge(ctx, d.task, list[0], Actor{}); err != nil || len(pm.dropped) != 0 {
		t.Fatalf("broken without a database: %v %v", err, pm.dropped)
	}
}

// The API: confirmation with the site's name, a job, the audit log.
func TestPurgeAPI(t *testing.T) {
	d, pm, r := removedEnv(t)
	jobs := NewJobs()
	c, s := startServer(t, 999, func(s *Server) { s.Data, s.Jobs, s.Sites = d.data, jobs, d.ops })
	a := Actor{User: "furkan", Via: "ssh"}
	status := func(err error) int {
		var ae *Error
		if errors.As(err, &ae) {
			return ae.Status
		}
		return 0
	}
	list, err := c.Removed(ctx, true)
	if err != nil || len(list) != 1 || list[0].ID != r.ID {
		t.Fatalf("list: %+v %v", list, err)
	}
	if _, err := c.PurgeRemoved(ctx, r.ID, "yanlis", a); status(err) != 422 {
		t.Fatalf("wrong confirm: %v", err)
	}
	if _, err := c.PurgeRemoved(ctx, "../../etc-20261030-121314", "magaza", a); status(err) != 404 && status(err) != 400 {
		t.Fatalf("bad id: %v", err)
	}
	id, err := c.PurgeRemoved(ctx, r.ID, "magaza", a)
	if err != nil {
		t.Fatal(err)
	}
	var j Job
	for i := 0; i < 200; i++ {
		if j, _ = jobs.Get(id); j.Done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !j.Done || j.Err != "" || j.Kind != "site.purge" || j.Target != "magaza" || len(pm.dropped) != 1 {
		t.Fatalf("job: %+v %v", j, pm.dropped)
	}
	entries, _ := s.Audit.Tail(10)
	if len(entries) == 0 || entries[0].Action != "site.purge" || !strings.Contains(entries[0].Detail, r.ID+": son kopya") {
		t.Fatalf("audit: %+v", entries)
	}
}

// A container of the site's name without a site; two removals of one name; the image of
// a removed site that a live one also uses.
func TestPurgeGuards(t *testing.T) {
	ctx := context.Background()
	d, pm, r := removedEnv(t)
	d.data.Docker.(*gonePurge).gone["magaza"] = false
	if _, err := d.data.Purge(ctx, d.task, r, Actor{}); err == nil || !strings.Contains(err.Error(), "container çalışıyor") || len(pm.dropped) != 0 {
		t.Fatalf("container of that name: %v %v", err, pm.dropped)
	}
	d.data.Docker.(*gonePurge).gone["magaza"] = true

	// The same name made and removed again: the database is the newest removal's.
	sp := specFor("magaza")
	if err := d.ops.Create(ctx, sp, nil, d.progress); err != nil {
		t.Fatal(err)
	}
	d.ops.now = func() time.Time { return time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC) }
	if err := d.ops.Remove(ctx, "magaza", d.progress); err != nil {
		t.Fatal(err)
	}
	list, _ := d.data.Removed(ctx, true)
	if len(list) != 2 || list[0].Older || !list[1].Older {
		t.Fatalf("older: %+v", list)
	}
	if _, err := d.data.Purge(ctx, d.task, list[1], Actor{}); err != nil || len(pm.dropped) != 0 {
		t.Fatalf("older entry dropped the database: %v %v", err, pm.dropped)
	}
	// A live site on the same image: the image stays.
	other := specFor("blog")
	other.Domains = []string{"blog.enisparta.com"}
	if err := d.ops.Create(ctx, other, nil, d.progress); err != nil {
		t.Fatal(err)
	}
	d.run.calls = nil
	if _, err := d.data.Purge(ctx, d.task, list[0], Actor{}); err != nil || len(pm.dropped) != 1 {
		t.Fatalf("newest: %v %v", err, pm.dropped)
	}
	if strings.Contains(strings.Join(d.run.calls, "\n"), "image rm") {
		t.Errorf("image of a live site removed: %v", d.run.calls)
	}
	// A database of that name the definition does not claim is shown, not touched.
	pm.dbs["sahipsiz"] = 4096
	os.MkdirAll(filepath.Join(d.root, "srv/musteri/_removed/sahipsiz-20261103-100000"), 0o700)
	os.WriteFile(filepath.Join(d.root, "srv/musteri/_removed/sahipsiz-20261103-100000/footops.json"),
		[]byte(`{"name":"sahipsiz","group":"musteri","database":false}`), 0o600)
	list, _ = d.data.Removed(ctx, true)
	if len(list) != 1 || list[0].Database || list[0].DBBytes != 4096 {
		t.Fatalf("unclaimed database: %+v", list)
	}
}
