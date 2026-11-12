package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
)

// fakeIptables keeps the rehearsal's rule like the kernel would; seq, if set, records
// every command beside the site commands (to check what comes first).
type fakeIptables struct {
	mu    sync.Mutex
	rules int
	calls []string
	fail  error
	seq   func(string)
}

func (f *fakeIptables) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f.RunInput(ctx, "", name, args...)
}

func (f *fakeIptables) RunInput(_ context.Context, _, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if f.seq != nil {
		f.seq(cmd)
	}
	if f.fail != nil {
		return "", f.fail
	}
	switch {
	case contains(args, "-C"):
		if f.rules == 0 {
			return "iptables: Bad rule (does a matching rule exist in that chain?).", exitErr{1}
		}
	case contains(args, "-I"):
		f.rules++
	case contains(args, "-D"):
		if f.rules > 0 {
			f.rules--
		}
	}
	return "", nil
}

type y5Env struct {
	*opsEnv
	data   *DataOps
	mongo  *fakeMongo
	restic *y3Restic
	b      *Backups
	fw     *fakeIptables
	reh    *Rehearsal
	rec    *Recovery
	now    time.Time
	n      int
	mu     sync.Mutex
	seq    []string
}

func (e *y5Env) task() *Task {
	e.n++
	return &Task{j: NewJobs(), job: &Job{ID: "Rcv" + string(rune('A'+e.n))}}
}

func (e *y5Env) record(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seq = append(e.seq, s)
}

const y5Repo = "s3:https://acc123.r2.cloudflarestorage.com/footops-t1"

func y5Request() RecoverRequest {
	return RecoverRequest{Repository: y5Repo, KeyID: "READONLY12", SecretKey: "Read/Only+Key=", Password: "RepoPassword0123456789abcdefghijklmnop"}
}

// newY5 is an empty new server, and a repository holding an old one: its server part,
// magaza (the night's full run: database, files, definition), yeni (made after it, a
// run of its own) and eski (older backups only: removed from the old server).
func newY5(t *testing.T) *y5Env {
	t.Helper()
	ops := newOps(t)
	// A new server has no certificate of its own: they come from the backup.
	if err := os.RemoveAll(filepath.Join(ops.root, sites.CertsDir)); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(fakeBinary)
	old := resticSHA256[runtime.GOARCH]
	resticSHA256[runtime.GOARCH] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { resticSHA256[runtime.GOARCH] = old })
	e := &y5Env{opsEnv: ops, mongo: newFakeMongo(), restic: &y3Restic{}, now: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)}
	ops.run.hook = func(cmd string) {
		if strings.Contains(cmd, " up --detach") {
			e.record("up " + cmd)
		}
	}
	root := ops.root
	e.data = &DataOps{Store: sites.Store{Root: root}, Docker: ops.net, Run: e.mongo, Stream: &y3Stream{restic: e.restic, mongo: e.mongo},
		Mongo: e.mongo, Sites: &fakeSites{running: true}, Staging: staging.New(filepath.Join(root, "staging"), 1<<30),
		SnapshotDir: filepath.Join(root, "snapshots"), RecoveryDir: filepath.Join(root, "recovery"),
		now: func() time.Time { return e.now }, checkFree: func(string, int64) error { return nil }}
	e.fw = &fakeIptables{seq: func(c string) { e.record(c) }}
	e.reh = &Rehearsal{Path: filepath.Join(root, "var/lib/footops/rehearsal.json"), Run: e.fw}
	e.b = &Backups{Data: e.data, Jobs: NewJobs(), Dir: filepath.Join(root, "var/lib/footops/backup"),
		SecretPath: filepath.Join(root, "etc/footops/secrets/backup/repository.json"), ToolsDir: filepath.Join(root, "tools"),
		CacheDir: filepath.Join(root, "cache"), Server: "T1", now: func() time.Time { return e.now }, loc: time.UTC, Rehearsal: e.reh}
	lib := filepath.Join(root, "var/lib/footops")
	e.rec = &Recovery{Backups: e.b, Ops: ops.ops, Rehearsal: e.reh, Dir: filepath.Join(lib, "recover"),
		AuthPath: filepath.Join(lib, "web/auth.json"), DockerConfig: filepath.Join(lib, "docker"),
		RegistriesPath: filepath.Join(lib, "registries.json"), SourcesDir: filepath.Join(lib, "sources"),
		PanelUID: os.Getuid(), PanelGID: os.Getgid()}

	night := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	crt, key := certPair(t, "enisparta.com", "*.enisparta.com")
	settings, _ := json.Marshal(BackupSettings{Night: "04:00", Defaults: BackupPolicy{DB: DBDaily, Files: FilesDaily, KeepHours: 48, KeepDays: 30, KeepMonths: 6},
		Sites: map[string]BackupPolicy{"magaza": {DB: DBHourly}}, Confirmed: night.Add(-72 * time.Hour)})
	server := []y3Node{
		{Path: "etc", Dir: true, Mode: 0o750}, {Path: "etc/secrets", Dir: true, Mode: 0o700},
		{Path: "etc/footops.json", Mode: 0o640, Content: `{"server_code":"T1","panel_host":"panel-t1.example.com","access_team":"takim","access_aud":"` + strings.Repeat("ab", 32) + `"}`},
		{Path: "etc/install.json", Mode: 0o600, Content: `{"server_code":"T1"}`},
		{Path: "etc/secrets/mongo.env", Mode: 0o600, Content: "MONGO_INITDB_ROOT_PASSWORD=OldServersRoot\n"},
		{Path: "etc/secrets/cloudflared.env", Mode: 0o600, Content: "TUNNEL_TOKEN=eyJold\n"},
		{Path: "etc/secrets/backup", Dir: true, Mode: 0o700},
		{Path: "etc/secrets/backup/repository.json", Mode: 0o600, Content: `{"repository":"` + y5Repo + `","key_id":"WRITEKEY01","secret_key":"Write/Key=","password":"RepoPassword0123456789abcdefghijklmnop"}`},
		{Path: "etc/secrets/notify", Dir: true, Mode: 0o700},
		{Path: "etc/secrets/notify/telegram.json", Mode: 0o600, Content: `{"token":"1:x","chat_id":5}`},
		{Path: "caddy", Dir: true, Mode: 0o755}, {Path: "caddy/etc", Dir: true, Mode: 0o755}, {Path: "caddy/etc/certs", Dir: true, Mode: 0o700},
		{Path: "caddy/etc/certs/0123456789abcdef.crt", Mode: 0o644, Content: crt},
		{Path: "caddy/etc/certs/0123456789abcdef.key", Mode: 0o600, Content: key},
		{Path: "caddy/etc/certs/eksik.crt", Mode: 0o644, Content: crt},
		{Path: "web", Dir: true, Mode: 0o700}, {Path: "web/auth.json", Mode: 0o600, Content: `{"username":"furkan"}`},
		{Path: "docker", Dir: true, Mode: 0o700}, {Path: "docker/config.json", Mode: 0o600, Content: `{"auths":{"ghcr.io":{}}}`},
		{Path: "registries.json", Mode: 0o600, Content: `[]`},
		{Path: "sources", Dir: true, Mode: 0o700}, {Path: "sources/b.json", Mode: 0o600, Content: `{"id":"b"}`},
		{Path: "sources/b", Mode: 0o600, Content: "PRIVATE KEY"},
		{Path: "backup", Dir: true, Mode: 0o700}, {Path: "backup/settings.json", Mode: 0o600, Content: string(settings)},
	}
	tags := func(site, part, kind, run string) []string {
		return []string{"footops", "site=" + site, "part=" + part, "kind=" + kind, "run=" + run}
	}
	def := func(spec sites.Spec, env map[string]string) map[string]string {
		meta, _ := spec.MarshalMeta()
		return map[string]string{"footops.json": string(meta), "compose.yaml": sites.Compose(spec, env)}
	}
	magaza := specFor("magaza")
	magaza.CreatedAt, magaza.UpdatedAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	magaza.EnvKeys = []string{"API_KEY"}
	yeni := specFor("yeni")
	yeni.Domains, yeni.DataMount = []string{"yeni.enisparta.com"}, ""
	eski := specFor("eski")
	eski.Domains = []string{"eski.enisparta.com"}
	oldURI := "mongodb://magaza:OldSitePassword@mongo:27017/magaza?authSource=magaza"
	dump := fakeArchive(map[string]map[string]int64{"magaza": {"urunler": 5, "sayfalar": 1}}, map[string][]string{"magaza": {"ucuz"}})
	dumpYeni := fakeArchive(map[string]map[string]int64{"yeni": {"kayitlar": 3}}, nil)
	tree := []y3Node{{Path: "alt", Dir: true, Mode: 0o750}, {Path: "alt/b.txt", Mode: 0o600, Content: "ikinci"},
		{Path: "a.txt", Mode: 0o644, Content: "merhaba"}, {Path: ".gizli", Mode: 0o644, Content: "x"}}
	var serverFiles, files, size int64
	for _, n := range server {
		if !n.Dir {
			serverFiles++
		}
	}
	for _, n := range tree {
		if !n.Dir {
			files++
			size += int64(len(n.Content))
		}
	}
	e.restic.snaps = []y3Snap{
		// newest first, as restic's list is sorted here
		{ID: snapID(20), Time: night.Add(5 * time.Hour), Path: "/footops/yeni/def", Tags: tags("yeni", "def", "manual", "RUN3"), Def: def(yeni, map[string]string{"MONGODB_URI": "mongodb://x"})},
		{ID: snapID(21), Time: night.Add(5 * time.Hour), Path: "/footops/yeni/db.archive", Tags: tags("yeni", "db", "manual", "RUN3"), Files: 1, Bytes: int64(len(dumpYeni)), Blob: dumpYeni},
		{ID: snapID(10), Time: night.Add(2 * time.Hour), Path: "/footops/magaza/db.archive", Tags: tags("magaza", "db", "hourly", "RUN2"), Files: 1, Bytes: int64(len(dump)), Blob: dump},
		{ID: snapID(1), Time: night, Path: "/footops/magaza/db.archive", Tags: tags("magaza", "db", "daily", "RUN1"), Files: 1, Bytes: int64(len(dump)), Blob: dump},
		{ID: snapID(2), Time: night.Add(10 * time.Second), Path: "/footops/magaza/files", Tags: tags("magaza", "files", "daily", "RUN1"), Files: files, Bytes: size, Tree: tree},
		{ID: snapID(3), Time: night.Add(20 * time.Second), Path: "/footops/magaza/def", Tags: tags("magaza", "def", "daily", "RUN1"),
			Def: def(magaza, map[string]string{"API_KEY": "gizli-değer", "MONGODB_URI": oldURI})},
		{ID: snapID(4), Time: night.Add(30 * time.Second), Path: "/footops/_server", Tags: tags("_server", "server", "daily", "RUN1"), Files: serverFiles, Tree: server},
		{ID: snapID(5), Time: night.Add(-30 * 24 * time.Hour), Path: "/footops/eski/def", Tags: tags("eski", "def", "daily", "RUN0"), Def: def(eski, nil)},
	}
	return e
}

func (e *y5Env) recover(t *testing.T, req RecoverRequest) (*Task, error) {
	t.Helper()
	tk := e.task()
	return tk, e.rec.Recover(context.Background(), tk, req)
}

func TestRecoverServer(t *testing.T) {
	e := newY5(t)
	tk, err := e.recover(t, y5Request())
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, steps(tk))
	}
	st, ok := e.rec.State()
	if !ok || !st.Complete() || st.Repository != "footops-t1" || st.Server != "T1" {
		t.Fatalf("state = %+v", st)
	}
	if len(st.Sites) != 2 || st.Sites[0].Name != "magaza" || st.Sites[1].Name != "yeni" || len(st.Skipped) != 1 || !strings.HasPrefix(st.Skipped[0], "eski") {
		t.Errorf("sites %+v skipped %v", st.Sites, st.Skipped)
	}
	if !strings.Contains(st.Sites[0].Detail, "2 koleksiyon / 6 belge") || !strings.Contains(st.Sites[0].Detail, "3 dosya") {
		t.Errorf("magaza: %+v", st.Sites[0])
	}

	// The newest of each part: the hourly database, the night's files and definition.
	if got := e.mongo.get("magaza"); got["urunler"] != 5 || got["sayfalar"] != 1 {
		t.Errorf("magaza db = %v", got)
	}
	if v := e.mongo.views["magaza"]; len(v) != 1 || v[0] != "ucuz" {
		t.Errorf("views = %v", v)
	}
	if got := e.mongo.get("yeni"); got["kayitlar"] != 3 {
		t.Errorf("yeni db = %v", got)
	}
	for _, c := range e.restic.ran("dump") {
		if strings.Contains(strings.Join(c, " "), snapID(1)) {
			t.Error("the older night's database was fetched")
		}
	}
	dir := filepath.Join(e.root, "srv/musteri/magaza")
	if b, err := os.ReadFile(filepath.Join(dir, "data/alt/b.txt")); err != nil || string(b) != "ikinci" {
		t.Errorf("files: %q %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "data/alt/b.txt")); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	spec, err := e.ops.Store.Get("magaza")
	if err != nil || !spec.CreatedAt.Equal(time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("spec %+v %v", spec, err)
	}
	env, _ := e.ops.Store.Env(spec)
	if env["API_KEY"] != "gizli-değer" || env["MONGODB_URI"] == "" || strings.Contains(env["MONGODB_URI"], "OldSitePassword") {
		t.Errorf("env = %v", env)
	}
	if _, err := os.Stat(filepath.Join(e.root, core.CaddySitesDir, "magaza.caddy")); err != nil {
		t.Error("no Caddy block")
	}

	// The server's part: in place where it belongs, the rest kept for go-live.
	certs, _ := e.ops.Store.Certs()
	if len(certs) != 1 {
		t.Errorf("certs = %d", len(certs))
	}
	if !strings.Contains(steps(tk), "eksik.crt: anahtarı yok") {
		t.Error("a certificate without its key was not reported")
	}
	for p, want := range map[string]string{
		e.rec.AuthPath: `{"username":"furkan"}`, filepath.Join(e.rec.DockerConfig, "config.json"): `{"auths":{"ghcr.io":{}}}`,
		e.rec.RegistriesPath: "[]", filepath.Join(e.rec.SourcesDir, "b"): "PRIVATE KEY",
	} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s = %q %v", p, b, err)
		}
	}
	if s, err := e.b.Settings(); err != nil || s.Sites["magaza"].DB != DBHourly || s.Confirmed.IsZero() {
		t.Errorf("settings %+v %v", s, err)
	}
	for _, name := range []string{"footops.json", "install.json", "cloudflared.env", "repository.json", "telegram.json"} {
		st, err := os.Stat(filepath.Join(e.rec.HeldDir(), name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("held %s: %v %v", name, st, err)
		}
	}
	if st, _ := os.Stat(e.rec.HeldDir()); st.Mode().Perm() != 0o700 {
		t.Errorf("held dir mode %v", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(e.rec.HeldDir(), "mongo.env")); err == nil {
		t.Error("the old MongoDB root password was kept")
	}
	// Nothing of it is left in staging, and the repository is not set up here.
	if ents, _ := os.ReadDir(filepath.Join(e.root, "staging")); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	if _, err := e.b.secret(); !errors.Is(err, errNoBackup) {
		t.Errorf("a repository was set up: %v", err)
	}

	// Only ever read, never locked; the rule before the first site starts.
	for _, c := range e.restic.calls {
		line := strings.Join(c, " ")
		if !contains(c, "--no-lock") {
			t.Errorf("restic without --no-lock: %s", line)
		}
		for _, w := range []string{"backup", "forget", "prune", "unlock", "init", "check", "stats"} {
			if c[0] == w {
				t.Errorf("restic wrote: %s", line)
			}
		}
	}
	if !e.reh.Active() || e.fw.rules != 1 {
		t.Errorf("rehearsal %v, rules %d", e.reh.Active(), e.fw.rules)
	}
	firstUp, insert := -1, -1
	for i, s := range e.seq {
		if strings.HasPrefix(s, "up ") && firstUp < 0 {
			firstUp = i
		}
		if strings.Contains(s, " -I DOCKER-USER 1 ") && insert < 0 {
			insert = i
		}
	}
	if insert < 0 || firstUp < 0 || insert > firstUp {
		t.Errorf("rule at %d, first site start at %d: %v", insert, firstUp, e.seq)
	}
	for _, secret := range []string{"Read/Only+Key=", "RepoPassword0123", "OldServersRoot", "OldSitePassword", "Write/Key="} {
		if strings.Contains(steps(tk), secret) {
			t.Errorf("%q in the steps", secret)
		}
	}
	data, _ := os.ReadFile(e.rec.statePath())
	for _, secret := range []string{"Read/Only+Key=", "RepoPassword0123", "READONLY12"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("%q in the state file", secret)
		}
	}

	// A second run finds everything in place.
	tk, err = e.recover(t, y5Request())
	if err == nil || !errors.Is(err, errNotEmpty) {
		// The sites are its own: allowed, and nothing is made again.
		if err != nil {
			t.Fatalf("second run: %v", err)
		}
	}
	if !strings.Contains(steps(tk), "önceki denemede kuruldu") {
		t.Errorf("second run:\n%s", steps(tk))
	}
}

func TestRecoverRefusals(t *testing.T) {
	e := newY5(t)
	// A site made otherwise: the server is not empty.
	if err := e.ops.Store.WriteMeta(specFor("baska")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.recover(t, y5Request()); !errors.Is(err, errNotEmpty) {
		t.Errorf("with a site: %v", err)
	}
	if e.reh.Active() {
		t.Error("refused, yet a rehearsal")
	}
	e = newY5(t)
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: y5Repo, Password: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.recover(t, y5Request()); !errors.Is(err, errNotEmpty) {
		t.Errorf("with a repository: %v", err)
	}

	e = newY5(t)
	e.restic.catCode = 12
	if _, err := e.recover(t, y5Request()); err == nil || err.Error() != "depo şifresi yanlış" {
		t.Errorf("wrong password: %v", err)
	}
	e.restic.catCode = 10
	if _, err := e.recover(t, y5Request()); err == nil || !strings.Contains(err.Error(), "yedek deposu yok") {
		t.Errorf("no repository: %v", err)
	}
	if e.reh.Active() || len(e.net.networks) != 0 {
		t.Error("changed something before the repository opened")
	}

	// No server part under this code (another server's code was typed).
	e = newY5(t)
	e.restic.snaps = e.restic.snaps[:3]
	if _, err := e.recover(t, y5Request()); err == nil || !strings.Contains(err.Error(), "sunucu kodu (T1)") {
		t.Errorf("no server part: %v", err)
	}

	// The rule cannot be put in place: no site starts.
	e = newY5(t)
	e.fw.fail = errors.New("iptables: not found")
	if _, err := e.recover(t, y5Request()); err == nil || !strings.Contains(err.Error(), "siteler kurulmadı") {
		t.Errorf("firewall: %v", err)
	}
	if _, err := e.ops.Store.Get("magaza"); err == nil {
		t.Error("a site was made without the rule")
	}
	if len(e.reh.Alerts(e.now)) != 2 {
		t.Errorf("alerts %+v", e.reh.Alerts(e.now))
	}
}

func TestRecoverResumes(t *testing.T) {
	e := newY5(t)
	e.run.fail["compose --project-name yeni --file"] = errors.New("exit status 1: pull access denied")
	tk, err := e.recover(t, y5Request())
	if err == nil || !strings.Contains(err.Error(), "1 site kurulamadı: yeni") {
		t.Fatalf("first run: %v\n%s", err, steps(tk))
	}
	st, _ := e.rec.State()
	if st.Complete() || st.Sites[1].State != recoverFailed || !strings.Contains(st.Sites[1].Error, "imaj indirilemedi") {
		t.Errorf("state %+v", st.Sites)
	}
	if _, err := e.ops.Store.Get("yeni"); err == nil {
		t.Error("the failed site was not undone")
	}
	delete(e.run.fail, "compose --project-name yeni --file")
	restores := e.mongo.restores
	tk, err = e.recover(t, y5Request())
	if err != nil {
		t.Fatalf("second run: %v\n%s", err, steps(tk))
	}
	if st, _ := e.rec.State(); !st.Complete() {
		t.Errorf("state %+v", st)
	}
	if e.mongo.restores != restores+1 || !strings.Contains(steps(tk), "magaza önceki denemede kuruldu") ||
		!strings.Contains(steps(tk), "Sunucu ayarları önceki denemede geri geldi") {
		t.Errorf("second run did more than the missing site (%d restores):\n%s", e.mongo.restores-restores, steps(tk))
	}
}

func TestRecoverHalfMadeSite(t *testing.T) {
	e := newY5(t)
	// An earlier run stopped while magaza was being made.
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Fatal(err)
	}
	st, _ := e.rec.State()
	st.Sites[0].State = recoverStarted
	st.Finished = time.Time{}
	if err := e.rec.save(st); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, "srv/musteri/magaza/data/sonradan.txt"), []byte("yarım"), 0o644)
	tk, err := e.recover(t, y5Request())
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	if !strings.Contains(steps(tk), "yarım kalan site temizleniyor") {
		t.Errorf("steps:\n%s", steps(tk))
	}
	if _, err := os.Stat(filepath.Join(e.root, "srv/musteri/magaza/data/sonradan.txt")); err == nil {
		t.Error("the half-made site's files were kept")
	}
	if st, _ := e.rec.State(); !st.Complete() {
		t.Errorf("state %+v", st)
	}
}

// A run after one cut short reads the newest definition of the half-made site: whatever
// the repository says, its cleanup stays under /srv.
func TestRecoverHalfMadeStaysUnderSrv(t *testing.T) {
	e := newY5(t)
	night := time.Date(2026, 11, 8, 1, 0, 0, 0, time.UTC)
	st := RecoverState{Job: "Earlier", Repository: "footops-t1", RepoID: repoID(y5Repo), Server: "T1", Started: night,
		Sites: []RecoverSite{{Name: "legacy", State: recoverStarted}}}
	if err := e.rec.save(st); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(e.root, "opt/legacy")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("services: {}\n"), 0o644)
	spec := specFor("legacy")
	spec.Group, spec.Domains, spec.Database, spec.DataMount = "../opt", []string{"legacy.enisparta.com"}, false, ""
	meta, _ := spec.MarshalMeta()
	e.restic.snaps = append([]y3Snap{{ID: snapID(510), Time: night.Add(6 * time.Hour), Path: "/footops/legacy/def",
		Tags: []string{"footops", "site=legacy", "part=def", "kind=manual", "run=RUN9"},
		Def:  map[string]string{"footops.json": string(meta), "compose.yaml": sites.Compose(spec, nil)}}}, e.restic.snaps...)
	tk, err := e.recover(t, y5Request())
	if err == nil {
		t.Fatal("a definition with group ../opt was recovered")
	}
	if !strings.Contains(steps(tk), "yedekteki tanım geçersiz") {
		t.Errorf("steps:\n%s", steps(tk))
	}
	if _, err := os.Stat(filepath.Join(outside, "compose.yaml")); err != nil {
		t.Errorf("a folder outside /srv was taken for the half-made site: %v", err)
	}
	for _, c := range e.run.calls {
		if strings.Contains(c, "/opt/legacy") {
			t.Errorf("ran %s", c)
		}
	}
}

// discard takes away nothing but /srv/<group>/<name>.
func TestDiscardStaysUnderSrv(t *testing.T) {
	e := newOps(t)
	outside := filepath.Join(e.root, "opt/legacy")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "compose.yaml"), []byte("services: {}\n"), 0o644)
	for _, s := range []sites.Spec{{Name: "legacy", Group: "../opt"}, {Name: "../../opt/legacy", Group: "musteri"}, {Name: "legacy", Group: "core"}} {
		if err := e.ops.discard(context.Background(), s); err == nil {
			t.Errorf("discarded %q in %q", s.Name, s.Group)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "compose.yaml")); err != nil || len(e.run.calls) > 0 {
		t.Errorf("touched outside /srv: %v, %q", err, e.run.calls)
	}
}

func TestRecoverAppThatDoesNotAnswer(t *testing.T) {
	e := newY5(t)
	e.ops.dial = func(context.Context, string) error { return errors.New("connection refused") }
	var shift atomic.Int64 // every wait moves the clock two minutes on: the app's time is up at once
	e.ops.now = func() time.Time { return time.Now().Add(time.Duration(shift.Load())) }
	e.ops.sleep = func(ctx context.Context, d time.Duration) error {
		shift.Add(int64(2 * time.Minute))
		return ctx.Err()
	}
	tk, err := e.recover(t, y5Request())
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	st, _ := e.rec.State()
	if !st.Complete() || st.Sites[0].State != recoverWarn || len(st.Sites[0].Warnings) == 0 {
		t.Errorf("state %+v", st.Sites)
	}
	if _, err := e.ops.Store.Get("magaza"); err != nil {
		t.Error("a site whose data came back was undone")
	}
	if _, err := os.Stat(filepath.Join(e.root, core.CaddySitesDir, "magaza.caddy")); err != nil {
		t.Error("no Caddy block for the site that does not answer yet")
	}
}

func TestRecoverAt(t *testing.T) {
	e := newY5(t)
	req := y5Request()
	req.At = time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC) // after the night, before the hourly and yeni
	if _, err := e.recover(t, req); err != nil {
		t.Fatal(err)
	}
	st, _ := e.rec.State()
	if len(st.Sites) != 1 || st.Sites[0].Name != "magaza" {
		t.Errorf("sites %+v", st.Sites)
	}
	dumped := ""
	for _, c := range e.restic.ran("dump") {
		dumped += strings.Join(c, " ") + "\n"
	}
	if !strings.Contains(dumped, snapID(1)) || strings.Contains(dumped, snapID(10)) {
		t.Errorf("dumps:\n%s", dumped)
	}
}

func TestPlanRecovery(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 8, h, 0, 0, 0, time.UTC) }
	snap := func(site, part, run string, h int) BackupSnapshot {
		return BackupSnapshot{ID: snapID(h*10 + len(site)), Time: at(h), Site: site, Part: part, Run: run}
	}
	list := []BackupSnapshot{ // newest first
		snap("c", PartDef, "R3", 9),
		snap("_server", PartServer, "R2", 4),
		snap("a", PartDB, "R2", 4), snap("b", PartFiles, "R2", 4), // b's definition failed in R2
		snap("a", PartDef, "R2", 4),
		snap("b", PartDef, "R1", 3), snap("d", PartDef, "R1", 3),
		snap("_server", PartServer, "R1", 3),
	}
	p, err := planRecovery(list, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range p.Sites {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "a,b,c" || len(p.Skipped) != 1 || !strings.HasPrefix(p.Skipped[0], "d") || p.Server.Run != "R2" {
		t.Errorf("sites %s skipped %v server %s", got, p.Skipped, p.Server.Run)
	}
	if p.Sites[1].Def.Run != "R1" || p.Sites[1].Files.Run != "R2" {
		t.Errorf("b: %+v", p.Sites[1])
	}
	p, err = planRecovery(list, at(3))
	if err != nil || p.Server.Run != "R1" || len(p.Sites) != 2 || p.Sites[0].Name != "b" || p.Sites[1].Name != "d" {
		t.Errorf("at 03:00: %+v %v", p, err)
	}
	if _, err := planRecovery(list, at(1)); err == nil {
		t.Error("a time before every server part was accepted")
	}
}

func TestRehearsalRule(t *testing.T) {
	fw := &fakeIptables{}
	r := &Rehearsal{Path: filepath.Join(t.TempDir(), "rehearsal.json"), Run: fw}
	ctx := context.Background()
	r.Ensure(ctx, true) // not a rehearsal: nothing to add, nothing to remove
	if fw.rules != 0 || r.Active() || r.Alerts(time.Now()) != nil {
		t.Fatal("not a rehearsal")
	}
	if err := r.Begin(ctx, RehearsalInfo{Repository: "footops-h1", Server: "H1"}); err != nil {
		t.Fatal(err)
	}
	if !r.Active() || fw.rules != 1 {
		t.Fatalf("active %v rules %d", r.Active(), fw.rules)
	}
	in, _ := r.Info()
	if in.Server != "H1" || in.Since.IsZero() {
		t.Errorf("info %+v", in)
	}
	rule := fw.calls[len(fw.calls)-1]
	for _, w := range []string{"-w 10", "DOCKER-USER", "-i br-+ ! -o br-+", "--ctstate NEW", "--comment footops-prova", "-j REJECT"} {
		if !strings.Contains(rule, w) {
			t.Errorf("rule %q lacks %q", rule, w)
		}
	}
	// Docker rebuilt its chains (a reboot): the rule comes back.
	fw.rules = 0
	r.Ensure(ctx, false)
	if fw.rules != 1 {
		t.Error("rule not put back")
	}
	r.Ensure(ctx, false)
	if fw.rules != 1 {
		t.Errorf("rule doubled: %d", fw.rules)
	}
	a := r.Alerts(time.Now())
	if len(a) != 1 || a[0].Key != "rehearsal" || !strings.Contains(a[0].Title, "footops-h1") {
		t.Errorf("alerts %+v", a)
	}
	if Notifiable(a[0]) {
		// The server sends nothing anyway; the alert is the panel's.
	}
	fw.rules = 2 // put twice by something
	if err := r.End(ctx); err != nil {
		t.Fatal(err)
	}
	if r.Active() || fw.rules != 0 {
		t.Errorf("after end: active %v rules %d", r.Active(), fw.rules)
	}
	// A rule left over from an end cut short goes when the agent starts.
	fw.rules = 1
	r.Ensure(ctx, true)
	if fw.rules != 0 {
		t.Error("left-over rule kept")
	}
	// An unreadable marker still marks a rehearsal.
	os.WriteFile(r.Path, []byte("{bozuk"), 0o600)
	if !r.Active() {
		t.Error("a broken marker is no rehearsal")
	}
}

func TestRehearsalKeepsEverythingIn(t *testing.T) {
	e := newY5(t)
	if err := e.reh.Begin(context.Background(), RehearsalInfo{}); err != nil {
		t.Fatal(err)
	}
	setup := BackupSetup{Repository: y5Repo, KeyID: "WRITEKEY01", SecretKey: "Write/Key=", Password: "RepoPassword0123456789abcdefghijklmnop"}
	for _, c := range []struct {
		root, golive bool
		ok           bool
	}{{false, false, false}, {false, true, false}, {true, false, false}, {true, true, true}} {
		s := setup
		s.GoLive = c.golive
		if err := e.b.MayReplace(s, c.root); (err == nil) != c.ok || (err != nil && !errors.Is(err, errRehearsal)) {
			t.Errorf("setup root=%v go-live=%v: %v", c.root, c.golive, err)
		}
	}
	// Even with a repository and a confirmed password, nothing is scheduled or started.
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: y5Repo, KeyID: "K1234567", SecretKey: "S12345678", Password: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Confirm(); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.b.active(); ok {
		t.Error("timetable active in rehearsal")
	}
	if _, err := e.b.Start(runPlan{}, Actor{}); !errors.Is(err, errRehearsal) {
		t.Errorf("start: %v", err)
	}
	if a := e.b.Alerts(e.now); len(a) != 0 {
		t.Errorf("backup alerts in rehearsal: %+v", a)
	}
	put := 0
	w := &StatusWriter{Backups: e.b, HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		put++
		return nil, errors.New("no")
	})}}
	if err := w.Write(context.Background()); err != nil || put != 0 {
		t.Errorf("status file written in rehearsal: %v %d", err, put)
	}
	n := &Notifier{SecretPath: filepath.Join(e.root, "telegram.json"), StatePath: filepath.Join(e.root, "notify.json"), Rehearsal: e.reh}
	if _, err := n.StartLink(context.Background(), "123:abc", false); !errors.Is(err, errRehearsal) {
		t.Errorf("link: %v", err)
	}
	if err := n.Test(context.Background()); !errors.Is(err, errRehearsal) {
		t.Errorf("test message: %v", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRecoverEndpointsAreRoot(t *testing.T) {
	e := newY5(t)
	srv := &Server{Backups: e.b, Jobs: e.b.Jobs, Data: e.data, Sites: e.ops, Recovery: e.rec, Rehearsal: e.reh,
		Audit: NewAudit(filepath.Join(e.root, "audit.jsonl")), AllowedUIDs: map[uint32]bool{0: true, 1000: true},
		Sampler: NewSampler(&fakeDocker{}, fakeSystem)}
	var uid atomic.Uint32
	srv.PeerUID = func(net.Conn) (uint32, error) { return uid.Load(), nil }
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, keyUID, uid.Load())
	}
	ts.Start()
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	post := func(path, body string) int {
		resp, err := client.Post(ts.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	req, _ := json.Marshal(y5Request())
	uid.Store(1000) // the panel
	if code := post("/v1/recover", string(req)); code != 403 {
		t.Errorf("panel recover: %d", code)
	}
	if code := post("/v1/rehearsal/end", `{}`); code != 403 {
		t.Errorf("panel end: %d", code)
	}
	resp, err := client.Get(ts.URL + "/v1/recover")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status: %v", err)
	}
	resp.Body.Close()
	uid.Store(0)
	if code := post("/v1/recover", `{"repository":"x"}`); code != 422 {
		t.Errorf("bad request: %d", code)
	}
	if code := post("/v1/recover", string(req)); code != 202 && code != 200 {
		t.Errorf("root recover: %d", code)
	}
	for e.b.Jobs.Busy() { // the job finishes before the test's files go
		time.Sleep(10 * time.Millisecond)
	}
}

var _ run.Runner = (*fakeIptables)(nil)

func TestRecoverLeavesWhatItDidNotMakeAlone(t *testing.T) {
	// Run 1 cannot make yeni; then someone makes a yeni of their own.
	e := newY5(t)
	e.run.fail["compose --project-name yeni --file"] = errors.New("exit status 1: pull access denied")
	if _, err := e.recover(t, y5Request()); err == nil {
		t.Fatal("first run passed")
	}
	delete(e.run.fail, "compose --project-name yeni --file")
	theirs := specFor("yeni")
	theirs.Domains, theirs.DataMount = []string{"yeni.enisparta.com"}, ""
	theirs.CreatedAt = e.now
	if err := e.ops.Store.WriteMeta(theirs); err != nil {
		t.Fatal(err)
	}
	tk, err := e.recover(t, y5Request())
	if err == nil || !strings.Contains(err.Error(), "yeni") {
		t.Fatalf("second run: %v", err)
	}
	st, _ := e.rec.State()
	if s, _ := st.site("yeni"); s.State != recoverConflict || !strings.Contains(s.Error, "bu kurtarmanın kurduğu değil") {
		t.Errorf("yeni: %+v\n%s", s, steps(tk))
	}
	if got, err := e.ops.Store.Get("yeni"); err != nil || !got.CreatedAt.Equal(e.now) {
		t.Errorf("their site was touched: %+v %v", got, err)
	}
	// A third run leaves it alone just the same.
	if _, err := e.recover(t, y5Request()); err == nil {
		t.Error("third run passed")
	}
	if _, err := e.ops.Store.Get("yeni"); err != nil {
		t.Error("their site went on the third run")
	}

	// A hand-made folder without a definition: the same.
	e = newY5(t)
	e.run.fail["compose --project-name yeni --file"] = errors.New("exit status 1: pull access denied")
	e.recover(t, y5Request())
	delete(e.run.fail, "compose --project-name yeni --file")
	dir := filepath.Join(e.root, "srv/musteri/yeni")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "elle.txt"), []byte("benim"), 0o644)
	if _, err := e.recover(t, y5Request()); err == nil {
		t.Error("ran over a hand-made folder")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "elle.txt")); err != nil || string(b) != "benim" {
		t.Error("the hand-made folder was touched")
	}

	// Recorded as being made, yet another creation time: someone's, not half-made.
	e = newY5(t)
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Fatal(err)
	}
	st, _ = e.rec.State()
	st.Sites[0].State, st.Finished = recoverStarted, time.Time{}
	e.rec.save(st)
	spec, _ := e.ops.Store.Get("magaza")
	spec.CreatedAt = e.now
	e.ops.Store.WriteMeta(spec)
	if _, err := e.recover(t, y5Request()); err == nil {
		t.Error("a site with another creation time was taken for half-made")
	}
	if got, err := e.ops.Store.Get("magaza"); err != nil || !got.CreatedAt.Equal(e.now) {
		t.Errorf("magaza was touched: %v", err)
	}
}

func TestHalfMade(t *testing.T) {
	dir := t.TempDir()
	spec := specFor("magaza")
	spec.CreatedAt = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	same := spec
	if !halfMade(spec, same, true, dir) {
		t.Error("its own definition")
	}
	for _, f := range []func(*sites.Spec){
		func(s *sites.Spec) { s.CreatedAt = s.CreatedAt.Add(time.Second) },
		func(s *sites.Spec) { s.Group = "kisisel" },
		func(s *sites.Spec) { s.Image = "ghcr.io/baska/imaj:main" },
	} {
		other := spec
		f(&other)
		if halfMade(spec, other, true, dir) {
			t.Errorf("taken for its own: %+v", other)
		}
	}
	zero := spec
	zero.CreatedAt = time.Time{}
	if halfMade(zero, zero, true, dir) {
		t.Error("no creation time to prove it")
	}
	if !halfMade(spec, sites.Spec{}, false, dir) {
		t.Error("an empty folder")
	}
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("x"), 0o600)
	if !halfMade(spec, sites.Spec{}, false, dir) {
		t.Error("a folder with compose.yaml only")
	}
	os.Mkdir(filepath.Join(dir, "data"), 0o755)
	if halfMade(spec, sites.Spec{}, false, dir) {
		t.Error("a folder with more than CreateWith writes first")
	}
}

func TestRecoverSameRequestOnly(t *testing.T) {
	e := newY5(t)
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Fatal(err)
	}
	other := y5Request()
	other.At = time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	if _, err := e.recover(t, other); err == nil || !strings.Contains(err.Error(), "başka bir kurtarmayla kuruldu") {
		t.Errorf("another moment: %v", err)
	}
	other = y5Request()
	other.Repository = "s3:https://acc123.r2.cloudflarestorage.com/footops-baska"
	if _, err := e.recover(t, other); err == nil || !strings.Contains(err.Error(), "başka bir kurtarmayla kuruldu") {
		t.Errorf("another repository: %v", err)
	}
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Errorf("the same again: %v", err)
	}
}

func TestRecoverForgetsWhatThePlanNoLongerHas(t *testing.T) {
	e := newY5(t)
	e.run.fail["compose --project-name yeni --file"] = errors.New("exit status 1: pull access denied")
	if _, err := e.recover(t, y5Request()); err == nil {
		t.Fatal("first run passed")
	}
	// The newest backups moved on: yeni is gone from the repository.
	var kept []y3Snap
	for _, sn := range e.restic.snaps {
		if !strings.Contains(sn.Path, "/yeni/") {
			kept = append(kept, sn)
		}
	}
	e.restic.snaps = kept
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Fatal(err)
	}
	st, _ := e.rec.State()
	if !st.Complete() {
		t.Errorf("a failed site the plan no longer has keeps it incomplete: %+v", st.Sites)
	}
	if _, ok := st.site("yeni"); ok {
		t.Error("yeni's record stayed")
	}
}

func TestRecoverMissingPartsAndDefinitions(t *testing.T) {
	e := newY5(t)
	// yeni has a database but its backup is gone; "sadece" has only hourly database
	// backups (made after the night: no definition).
	var kept []y3Snap
	for _, sn := range e.restic.snaps {
		if sn.Path != "/footops/yeni/db.archive" {
			kept = append(kept, sn)
		}
	}
	dump := fakeArchive(map[string]map[string]int64{"sadece": {"x": 1}}, nil)
	kept = append(kept, y3Snap{ID: snapID(30), Time: time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC), Path: "/footops/sadece/db.archive",
		Tags: []string{"footops", "site=sadece", "part=db", "kind=hourly", "run=RUN4"}, Files: 1, Bytes: int64(len(dump)), Blob: dump})
	e.restic.snaps = kept
	if _, err := e.recover(t, y5Request()); err != nil {
		t.Fatal(err)
	}
	st, _ := e.rec.State()
	if s, _ := st.site("yeni"); s.State != recoverWarn || len(s.Warnings) == 0 || !strings.Contains(s.Warnings[0], "veritabanının yedeği yok") {
		t.Errorf("yeni without its database backup: %+v", s)
	}
	found := false
	for _, x := range st.Skipped {
		found = found || strings.HasPrefix(x, "sadece (tanımının yedeği yok")
	}
	if !found {
		t.Errorf("skipped: %v", st.Skipped)
	}
}

func TestReadOnlyRepository(t *testing.T) {
	for args, want := range map[string]bool{
		"--no-cache --no-lock --json cat config":   true,
		"snapshots --json --no-lock --host T1":     true,
		"dump --no-lock abc /footops/a/db.archive": true,
		"restore --json --no-lock abc --target /r": true,
		"ls --json --no-lock --recursive abc /x":   true,
		"snapshots --json":                         false,
		"forget --no-lock --keep-within 1h":        false,
		"unlock --remove-all":                      false,
		"backup --no-lock /x":                      false,
		"--no-lock init":                           false,
	} {
		if got := readCommand(strings.Fields(args)); got != want {
			t.Errorf("readCommand(%q) = %v", args, got)
		}
	}
	e := newY5(t)
	env := &resticEnv{readOnly: true, repo: y5Repo}
	if _, err := e.b.restic(context.Background(), e.task(), env, resticOpts{}, "unlock", "--remove-all"); err == nil {
		t.Error("unlock ran on a read-only repository")
	}
	if len(e.restic.calls) != 0 {
		t.Errorf("restic ran: %v", e.restic.calls)
	}
}

func TestRehearsalEndIsNotUndoneByACheck(t *testing.T) {
	for i := 0; i < 50; i++ {
		fw := &fakeIptables{}
		r := &Rehearsal{Path: filepath.Join(t.TempDir(), "rehearsal.json"), Run: fw}
		if err := r.Begin(context.Background(), RehearsalInfo{}); err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
					r.Ensure(context.Background(), false)
				}
			}
		}()
		if err := r.End(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(stop)
		<-done
		fw.mu.Lock()
		rules := fw.rules
		fw.mu.Unlock()
		if rules != 0 || r.Active() {
			t.Fatalf("after End: %d rules, active %v", rules, r.Active())
		}
	}
	// Anything but "no such rule" is an error, not an absent rule.
	fw := &fakeIptables{rules: 1}
	r := &Rehearsal{Path: filepath.Join(t.TempDir(), "rehearsal.json"), Run: fw}
	r.Begin(context.Background(), RehearsalInfo{})
	fw.fail = exitErr{4}
	if err := r.End(context.Background()); err == nil || !r.Active() {
		t.Errorf("end with iptables failing: %v, active %v", err, r.Active())
	}
}

func TestRecoveryHoldsFileChanges(t *testing.T) {
	j := NewJobs()
	block := make(chan struct{})
	if _, err := j.Start(JobSpec{Kind: "server.recover", Target: "footops-h1"}, func(context.Context, *Task) error {
		<-block
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Hold("magaza"); !errors.Is(err, ErrBusy) {
		t.Errorf("hold during a recovery: %v", err)
	}
	close(block)
	for j.Busy() {
		time.Sleep(time.Millisecond)
	}
	release, err := j.Hold("magaza")
	if err != nil {
		t.Fatal(err)
	}
	release()
}
