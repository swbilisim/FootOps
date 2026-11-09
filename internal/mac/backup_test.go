package mac

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/install"
	"github.com/swbilisim/FootOps/internal/mongo"
	sitedef "github.com/swbilisim/FootOps/internal/sites"
)

// The backups opened on the Mac, against a real restic (when there is one: the Mac has
// it, CI may not) and a local repository made the way the agent makes them. Made-up
// values only.

const (
	fixturePassword = "Test-Repo-Password-0123456789"
	nightRun        = "NightRun0000000000000001"
	hourRun         = "HourRun00000000000000002"
)

// fixtureRepo is a repository made the way a server's agent makes them, once for all the
// tests (each restic call opens its key anew, ~0.5 s); they only read it.
type fixtureRepo struct {
	restic, dir, repo, root string
	db, hourDB              []byte // the database dumps of the night and the hour
}

var (
	sharedOnce sync.Once
	shared     *fixtureRepo
	sharedErr  error
)

func TestMain(m *testing.M) {
	if conf := os.Getenv("FOOTOPS_MAC_TEST_CHILD"); conf != "" {
		os.Exit(childOpen(conf)) // TestOpenAppGone's footops-mac
	}
	code := m.Run()
	if shared != nil {
		removeTree(shared.dir)
	}
	os.Exit(code)
}

// fixture is one test's footops-mac on the shared repository: its own cache, Downloads,
// list of what was opened.
type fixture struct {
	*fixtureRepo
	tmp       string
	e         *env
	out, errb *bytes.Buffer
}

// findRestic is restic, or the test is skipped.
func findRestic(t *testing.T) string {
	t.Helper()
	p, err := lookTool("restic")
	if err != nil {
		t.Skip("restic yok")
	}
	return p
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	restic := findRestic(t)
	sharedOnce.Do(func() { shared, sharedErr = makeRepo(restic) })
	if sharedErr != nil {
		t.Fatal(sharedErr)
	}
	f := &fixture{fixtureRepo: shared, tmp: t.TempDir()}
	f.e, f.out, f.errb = f.env(t)
	// Opened folders keep the backup's read-only ones.
	t.Cleanup(func() { removeTree(f.tmp) })
	return f
}

// makeRepo makes the repository: site magaza's night (its three parts, the last a
// minute after the first) and an hour's database, site blog's files, the server's own
// settings, and snapshots not FootOps' or not as the agent makes them.
func makeRepo(resticPath string) (*fixtureRepo, error) {
	dir, err := os.MkdirTemp("", "footops-mac-yedek-test-")
	if err != nil {
		return nil, err
	}
	f := &fixtureRepo{restic: resticPath, dir: dir, repo: filepath.Join(dir, "repo"), root: filepath.Join(dir, "footops")}
	var failed error
	restic := func(stdin []byte, args ...string) {
		if failed != nil {
			return
		}
		cmd := exec.Command(f.restic, args...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+f.repo, "RESTIC_PASSWORD="+fixturePassword,
			"RESTIC_CACHE_DIR="+filepath.Join(dir, "cache"), "TZ=UTC")
		if stdin != nil {
			cmd.Stdin = bytes.NewReader(stdin)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			failed = fmt.Errorf("restic %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel string, data []byte, mode os.FileMode) {
		p := filepath.Join(f.root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil && failed == nil {
			failed = err
		}
		if err := os.WriteFile(p, data, mode); err != nil && failed == nil {
			failed = err
		}
	}
	restic(nil, "init")

	f.db = make([]byte, 100<<10)
	rand.Read(f.db)
	f.hourDB = []byte("saatlik döküm")
	write("magaza/files/a.txt", []byte("merhaba\n"), 0o644)
	big := make([]byte, 300<<10)
	rand.Read(big)
	write("magaza/files/alt/b.bin", big, 0o600)
	write("magaza/files/kilitli/c.txt", []byte("salt okunur klasörde\n"), 0o444)
	if err := os.Symlink("../a.txt", filepath.Join(f.root, "magaza/files/alt/bag")); err != nil {
		return f, err
	}
	if err := os.Chmod(filepath.Join(f.root, "magaza/files/kilitli"), 0o555); err != nil {
		return f, err
	}
	spec := sitedef.Spec{Name: "magaza", Group: "musteri", Domains: []string{"magaza.example"}, Image: "ghcr.io/ornek/magaza:main",
		Port: 3000, MemoryMB: 256, DataMount: "/app/uploads", Database: true, DBVar: "MONGODB_URI", EnvKeys: []string{"API_KEY", "NOTE"}}
	meta, _ := json.Marshal(spec)
	write("magaza/def/footops.json", meta, 0o600)
	write("magaza/def/compose.yaml", []byte(sitedef.Compose(spec, map[string]string{
		"API_KEY": `gizli$anahtar"1`, "NOTE": "iki\nsatır", "MONGODB_URI": "mongodb://magaza:SunucununSifresi@mongo:27017/magaza"})), 0o600)
	write("_server/etc/footops.json", []byte(`{"ornek":true}`), 0o600)
	write("blog/files/index.html", []byte("<p>blog</p>"), 0o644)

	tags := func(site, part, kind, run string) string {
		return strings.Join([]string{"footops", "site=" + site, "part=" + part, "kind=" + kind, "run=" + run}, ",")
	}
	restic(f.db, "backup", "--host", "t1", "--tag", tags("magaza", "db", "daily", nightRun), "--time", "2026-11-07 03:00:05",
		"--stdin", "--stdin-filename", f.root+"/magaza/db.archive")
	restic(nil, "backup", "--host", "t1", "--tag", tags("magaza", "files", "daily", nightRun), "--time", "2026-11-07 03:00:40",
		f.root+"/magaza/files")
	restic(nil, "backup", "--host", "t1", "--tag", tags("magaza", "def", "daily", nightRun), "--time", "2026-11-07 03:01:10",
		f.root+"/magaza/def")
	restic(nil, "backup", "--host", "t1", "--tag", tags("_server", "server", "daily", nightRun), "--time", "2026-11-07 03:01:20",
		f.root+"/_server")
	restic(nil, "backup", "--host", "t1", "--tag", tags("blog", "files", "daily", nightRun), "--time", "2026-11-07 03:01:30",
		f.root+"/blog/files")
	restic(f.hourDB, "backup", "--host", "t1", "--tag", tags("magaza", "db", "hourly", hourRun), "--time", "2026-11-07 14:00:03",
		"--stdin", "--stdin-filename", f.root+"/magaza/db.archive")
	// Not FootOps', and FootOps-tagged at the wrong place: neither is read.
	restic([]byte("x"), "backup", "--host", "t1", "--tag", "baska", "--time", "2026-11-08 01:00:00", "--stdin", "--stdin-filename", "/x")
	restic([]byte("y"), "backup", "--host", "t1", "--tag", tags("magaza", "db", "daily", "Wrong1"), "--time", "2026-11-08 02:00:00",
		"--stdin", "--stdin-filename", "/baska/yer/db.archive")
	return f, failed
}

// env is footops-mac on the fixture: the Keychain's t1 with its password (read by a
// stand-in for security), the repository in place of R2.
func (f *fixture) env(t *testing.T) (*env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	kc := filepath.Join(f.tmp, "kc.sh")
	if err := os.WriteFile(kc, []byte("#!/bin/sh\ncase \"$1\" in\nrestic-t1) echo '"+fixturePassword+"' ;;\n*) exit 44 ;;\nesac\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	mongoURI := os.Getenv("FOOTOPS_TEST_MONGO")
	if mongoURI == "" {
		mongoURI = "mongodb://127.0.0.1:1" // nothing there: the tests that need one say so
	}
	e := &env{out: &out, errw: &errb,
		keychain: fakeKeychain{items: map[string]string{"r2-hesap": testAccount, "r2-mac-erisim": testKeyID, "r2-mac-gizli": testSecret,
			"restic-t1": fixturePassword}, cmd: []string{kc}},
		settings: filepath.Join(f.tmp, "ayarlar.json"),
		r2:       func(account string) string { return "https://" + account + ".r2.cloudflarestorage.com" },
		now:      func() time.Time { return time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC) },
		look: func(name string) (string, error) {
			if name == "restic" {
				return f.restic, nil
			}
			return lookTool(name)
		},
		repository:  func(access, string) string { return f.repo },
		backupRoot:  f.root,
		resticCache: filepath.Join(f.tmp, "cache"),
		downloads:   filepath.Join(f.tmp, "Downloads"),
		opened:      filepath.Join(f.tmp, "ayarlar", "acilanlar.json"),
		tmp:         filepath.Join(f.tmp, "gecici"),
		mongoURI:    mongoURI,
		terminal:    func() bool { return false },
		free:        freeRoom,
	}
	if err := os.MkdirAll(e.tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	return e, &out, &errb
}

func (f *fixture) run(t *testing.T, args ...string) int {
	t.Helper()
	f.out.Reset()
	f.errb.Reset()
	return f.e.run(context.Background(), append([]string{"yedek"}, args...))
}

// events reads a command's JSON lines.
func events(t *testing.T, out string) []map[string]any {
	t.Helper()
	var list []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		list = append(list, m)
	}
	return list
}

// Snapshots not as the agent makes them are left out; the rest grouped by run, newest
// first.
func TestParseSnapshots(t *testing.T) {
	id := strings.Repeat("ab", 32)
	snap := func(time, path string, tags ...string) map[string]any {
		return map[string]any{"id": id, "time": time, "paths": []string{path}, "tags": tags,
			"summary": map[string]any{"total_files_processed": 3, "total_bytes_processed": 1234}}
	}
	list := []map[string]any{
		snap("2026-11-07T03:00:05.123456789+03:00", "/footops/magaza/db.archive", "footops", "site=magaza", "part=db", "kind=daily", "run=R1"),
		snap("2026-11-07T00:00:40Z", "/footops/magaza/files", "footops", "site=magaza", "part=files", "kind=daily", "run=R1"),
		snap("2026-11-07T00:00:41Z", "/footops/_server", "footops", "site=_server", "part=server", "run=R1"),
		snap("2026-11-07T00:00:42Z", "/footops/magaza/db.archive", "site=magaza", "part=db"),                      // not FootOps'
		snap("2026-11-07T00:00:43Z", "/elsewhere/db.archive", "footops", "site=magaza", "part=db"),               // wrong place
		snap("2026-11-07T00:00:44Z", "/footops/../etc/db.archive", "footops", "site=../etc", "part=db"),          // no site
		snap("2026-11-07T00:00:45Z", "/footops/magaza/db.archive", "footops", "site=magaza", "part=db", "run=a/b"), // no run
		snap("2026-11-07T00:00:46Z", "/footops/magaza/x", "footops", "site=magaza", "part=x"),                    // no part
	}
	bad := snap("2026-11-07T00:00:47Z", "/footops/magaza/db.archive", "footops", "site=magaza", "part=db")
	bad["id"] = "kisa"
	list = append(list, bad)
	data, _ := json.Marshal(list)
	got, other, err := parseSnapshots(data, "/footops")
	if err != nil || len(got) != 3 || other != 6 {
		t.Fatalf("%v %d %+v", err, other, got)
	}
	if got[0].Part != partServer || got[2].Part != partDB || !got[2].Time.Equal(time.Date(2026, 11, 7, 0, 0, 5, 123456789, time.UTC)) ||
		got[2].Time.Location() != time.UTC || got[1].Files != 3 || got[1].Bytes != 1234 {
		t.Errorf("%+v", got)
	}
	sites := pointsOf(got)
	if len(sites) != 1 || sites[0].Name != "magaza" || len(sites[0].Points) != 1 || len(sites[0].Points[0].Parts) != 2 ||
		!sites[0].Points[0].Time.Equal(got[2].Time) {
		t.Errorf("%+v", sites)
	}
	if _, _, err := parseSnapshots([]byte("{"), "/footops"); err == nil {
		t.Error("broken list read")
	}
}

// What ac opens: the newest, a backup by its id (or its first letters), a minute with
// the backup begun in it whole, and the parts it did not take from the backups before.
func TestChoose(t *testing.T) {
	at := func(h, m, s int) time.Time { return time.Date(2026, 11, 7, h, m, s, 0, time.UTC) }
	list := []snapshot{
		{ID: "h-db", Time: at(14, 0, 3), Site: "magaza", Part: partDB, Run: "HourRun1"},
		{ID: "n-def", Time: at(3, 1, 10), Site: "magaza", Part: partDef, Run: "NightRun"},
		{ID: "n-files", Time: at(3, 0, 40), Site: "magaza", Part: partFiles, Run: "NightRun"},
		{ID: "n-db", Time: at(3, 0, 5), Site: "magaza", Part: partDB, Run: "NightRun"},
		{ID: "o-db", Time: at(2, 0, 5), Site: "magaza", Part: partDB, Run: "OldRun00"},
		{ID: "b-files", Time: at(3, 1, 30), Site: "blog", Part: partFiles, Run: "NightRun"},
	}
	ids := func(m map[string]snapshot) string {
		var out []string
		for _, p := range siteParts {
			if s, ok := m[p]; ok {
				out = append(out, s.ID)
			}
		}
		return strings.Join(out, " ")
	}
	for _, c := range []struct{ when, want, err string }{
		{"", "h-db n-files n-def", ""},
		{"NightRun", "n-db n-files n-def", ""},
		{"Night", "", "depoda yok"}, // fewer than 6 letters: the whole id
		{"NightR", "n-db n-files n-def", ""},
		{"2026-11-07 03:00", "n-db n-files n-def", ""}, // its def came a minute later: taken all the same
		{"2026-11-07 02:59", "o-db", ""},
		{"2026-11-07 14:00", "h-db n-files n-def", ""},
		{"2026-11-07 01:00", "", "için yedeği yok"},
		{"YokBoyle1", "", "depoda yok"},
		{"07.11.2026 03:00", "", "YYYY-AA-GG"},
	} {
		got, err := choose(list, "magaza", c.when, time.UTC)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%q: %v", c.when, err)
		case c.err == "" && (err != nil || ids(got) != c.want):
			t.Errorf("%q: %q %v", c.when, ids(got), err)
		}
	}
	// The same minute in another zone is another moment.
	ist := time.FixedZone("TR", 3*3600)
	if got, err := choose(list, "magaza", "2026-11-07 06:00", ist); err != nil || ids(got) != "n-db n-files n-def" {
		t.Errorf("zone: %q %v", ids(got), err)
	}
	// A snapshot of no run is a backup of its own, named by its id.
	lone := append(list, snapshot{ID: strings.Repeat("c", 64), Time: at(1, 0, 0), Site: "magaza", Part: partDB})
	if got, err := choose(lone, "magaza", strings.Repeat("c", 8), time.UTC); err != nil || ids(got) != strings.Repeat("c", 64) {
		t.Errorf("by id: %q %v", ids(got), err)
	}
	two := append(list, snapshot{ID: "x", Time: at(5, 0, 0), Site: "magaza", Part: partDB, Run: "NightRux"})
	if _, err := choose(two, "magaza", "NightR", time.UTC); err == nil || !strings.Contains(err.Error(), "birden çok") {
		t.Errorf("ambiguous: %v", err)
	}
}

// sunucular lists the Keychain's servers without reading their passwords; liste reads
// the repository: every site, or one, as the panel groups them.
func TestBackupListing(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "sunucular", "--json"); code != 0 || !strings.Contains(f.out.String(), `{"code":"t1","bucket":"footops-t1","password":true}`) {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if code := f.run(t, "dene", "t1"); code != 0 || !strings.Contains(f.out.String(), "footops-t1 açıldı (salt okunur): 6 anlık görüntü") {
		t.Fatalf("dene: %d %s %s", code, f.out, f.errb)
	}
	if code := f.run(t, "liste", "t1", "--json"); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	var l BackupList
	if err := json.Unmarshal(f.out.Bytes(), &l); err != nil {
		t.Fatal(err)
	}
	if l.Server != "t1" || l.Snapshots != 6 || l.Other != 1 || len(l.Sites) != 2 || l.Sites[0].Name != "blog" || l.Sites[1].Name != "magaza" ||
		!l.ServerPart.Equal(time.Date(2026, 11, 7, 3, 1, 20, 0, time.UTC)) {
		t.Fatalf("%+v", l)
	}
	m := l.Sites[1].Points
	if len(m) != 2 || m[0].Run != hourRun || m[0].Kind != "hourly" || len(m[0].Parts) != 1 || m[1].Run != nightRun || len(m[1].Parts) != 3 ||
		m[1].Parts[partFiles].Files != 3 || m[1].Parts[partDB].Bytes != int64(len(f.db)) ||
		!m[1].Time.Equal(time.Date(2026, 11, 7, 3, 0, 5, 0, time.UTC)) {
		t.Errorf("%+v", m)
	}
	if code := f.run(t, "liste", "t1"); code != 0 || !strings.Contains(f.out.String(), "magaza  2026-11-07 14:00") ||
		!strings.Contains(f.out.String(), "Sunucu ayarları: 2026-11-07 03:01") {
		t.Errorf("table: %s", f.out)
	}
	if code := f.run(t, "liste", "t1", "magaza"); code != 0 || !strings.Contains(f.out.String(), "2026-11-07 03:00  gece     NightRun") ||
		!strings.Contains(f.out.String(), "dosyalar (3 dosya, 300 KB)") {
		t.Errorf("site: %s", f.out)
	}
	if code := f.run(t, "liste", "t1", "yok-boyle"); code != 1 || !strings.Contains(f.errb.String(), "bu sitenin yedeği yok") {
		t.Errorf("no site: %d %s", code, f.errb)
	}
	// The repository's password not the Keychain's; no repository there.
	f.e.keychain = fakeKeychain{items: map[string]string{"r2-hesap": testAccount, "r2-mac-erisim": testKeyID, "r2-mac-gizli": testSecret,
		"restic-t1": "x"}, cmd: []string{"/bin/echo"}}
	if code := f.run(t, "dene", "t1"); code != 1 || !strings.Contains(f.errb.String(), "depo şifresi yanlış") {
		t.Errorf("password: %d %s", code, f.errb)
	}
	f.e.repository = func(access, string) string { return filepath.Join(f.tmp, "yok") }
	if code := f.run(t, "dene", "t1", "--json"); code != 1 || !strings.Contains(f.out.String(), "restic deposu yok") {
		t.Errorf("no repository: %d %s", code, f.out)
	}
	if code := f.run(t, "dene", "zz"); code != 1 || !strings.Contains(f.errb.String(), "bilinmeyen sunucu: zz (bilinenler: t1)") {
		t.Errorf("unknown: %d %s", code, f.errb)
	}
}

// ac opens a backup into a folder of the owner's, as the panel's İndir gives it.
func TestBackupOpen(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "ac", "t1", "magaza", "2026-11-07 03:00", "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	ev := events(t, f.out.String())
	if ev[0]["event"] != "start" || ev[len(ev)-1]["event"] != "result" {
		t.Fatalf("%v", ev)
	}
	target := filepath.Join(f.e.downloads, "magaza-20261107-0301")
	if ev[len(ev)-1]["target"] != target {
		t.Fatalf("%v", ev[len(ev)-1])
	}
	parts := map[string]bool{}
	for _, e := range ev[1 : len(ev)-1] {
		if e["event"] != "progress" {
			t.Fatalf("%v", e)
		}
		parts[e["part"].(string)] = true
	}
	if !parts[partDB] || !parts[partFiles] {
		t.Errorf("progress: %v", parts)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder: %v %v", fi, err)
	}
	for name, want := range map[string][]byte{"db.archive": f.db, "files/a.txt": []byte("merhaba\n"), "files/kilitli/c.txt": []byte("salt okunur klasörde\n")} {
		if got, err := os.ReadFile(filepath.Join(target, name)); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if l, err := os.Readlink(filepath.Join(target, "files/alt/bag")); err != nil || l != "../a.txt" {
		t.Errorf("link: %q %v", l, err)
	}
	for _, name := range []string{"db.archive", "env", "footops.json"} {
		if fi, err := os.Stat(filepath.Join(target, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "compose.yaml")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("compose.yaml written: %v", err)
	}
	env, _ := os.ReadFile(filepath.Join(target, "env"))
	vars, err := sitedef.ParseDotenv(string(env))
	if err != nil || len(vars) != 2 || sitedef.EnvMap(vars)["API_KEY"] != `gizli$anahtar"1` || sitedef.EnvMap(vars)["NOTE"] != "iki\nsatır" ||
		strings.Contains(string(env), "SunucununSifresi") ||
		!strings.Contains(string(env), "# magaza ortam değişkenleri, yedek 07.11.2026 03:01\n# SIRLAR İÇERİR") ||
		!strings.Contains(string(env), "# MONGODB_URI alınmadı") {
		t.Errorf("env: %v\n%s", err, env)
	}
	// On the list, its mark in it.
	if code := f.run(t, "acilanlar", "--json"); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	var v OpenedView
	if err := json.Unmarshal(f.out.Bytes(), &v); err != nil || len(v.Folders) != 1 || v.Folders[0].Path != target ||
		v.Folders[0].Files != 6 || v.Folders[0].Site != "magaza" || v.MongoError == "" {
		t.Fatalf("%v %+v", err, v)
	}

	// The same again: a folder of its own beside it. One part only (its folder named
	// after its time); a backup by its id.
	if code := f.run(t, "ac", "t1", "magaza", nightRun); code != 0 || !strings.Contains(f.out.String(), "✓ Bitti: "+target+"-2") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if code := f.run(t, "ac", "t1", "magaza", "2026-11-07 03:00", "--db"); code != 0 ||
		!strings.Contains(f.out.String(), "✓ Bitti: "+filepath.Join(f.e.downloads, "magaza-20261107-0300")+"\n") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.e.downloads, "magaza-20261107-0300")); len(entries) != 2 { // db.archive and the mark
		t.Errorf("--db: %v", entries)
	}
	if code := f.run(t, "ac", "t1", "magaza", hourRun[:8], "--hedef", filepath.Join(f.tmp, "saatlik"), "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if got, _ := os.ReadFile(filepath.Join(f.tmp, "saatlik", "db.archive")); !bytes.Equal(got, f.hourDB) {
		t.Errorf("hour: %q", got)
	}
	// A folder that is there and not empty is not written into.
	if code := f.run(t, "ac", "t1", "magaza", "--hedef", target); code != 1 || !strings.Contains(f.errb.String(), "dolu") {
		t.Errorf("full: %d %s", code, f.errb)
	}
	if code := f.run(t, "ac", "t1", "magaza", "--dosyalar", "--mongo"); code != 2 {
		t.Errorf("--mongo without --db: %d", code)
	}
	if code := f.run(t, "ac", "t1", "blog", "--ayarlar"); code != 1 || !strings.Contains(f.errb.String(), "bu yedekte ayarlar yok") {
		t.Errorf("no def: %d %s", code, f.errb)
	}

	// Deleted: only what is on the list and still holds its mark — read-only folders and
	// all.
	if code := f.run(t, "sil", filepath.Join(f.tmp, "baska")); code != 1 || !strings.Contains(f.errb.String(), "açtıklarından değil") {
		t.Errorf("not ours: %d %s", code, f.errb)
	}
	os.WriteFile(filepath.Join(target+"-2", openedMark), []byte("baskasi\n"), 0o600)
	if code := f.run(t, "sil", target+"-2"); code != 1 || !strings.Contains(f.errb.String(), "işareti yok ya da değişmiş") {
		t.Errorf("mark changed: %d %s", code, f.errb)
	}
	if code := f.run(t, "sil", target); code != 0 || !strings.Contains(f.out.String(), "Silindi: "+target) {
		t.Fatalf("%d %s", code, f.errb)
	}
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}
	if code := f.run(t, "sil", "magaza_yedek_x"); code != 1 || !strings.Contains(f.errb.String(), "bağlanılamadı") {
		t.Errorf("no MongoDB: %d %s", code, f.errb)
	}
	if code := f.run(t, "sil", "benimdb"); code != 1 || !strings.Contains(f.errb.String(), "_yedek_") {
		t.Errorf("not a backup's: %d %s", code, f.errb)
	}
	// What is gone from the disk leaves the list.
	removeTree(target + "-2")
	if code := f.run(t, "acilanlar", "--json"); code != 0 || strings.Contains(f.out.String(), target+"-2") ||
		!strings.Contains(f.out.String(), "saatlik") {
		t.Errorf("%d %s", code, f.out)
	}
}

// A week on, what was opened is pointed out (the menu says so too).
func TestOpenedOld(t *testing.T) {
	f := newFixture(t)
	if code := f.run(t, "ac", "t1", "magaza", "--db"); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if n := f.e.openedOldCount(); n != 0 {
		t.Errorf("fresh: %d", n)
	}
	f.e.now = func() time.Time { return time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC) }
	if n := f.e.openedOldCount(); n != 1 {
		t.Errorf("a week on: %d", n)
	}
	if code := f.run(t, "acilanlar"); code != 0 || !strings.Contains(f.out.String(), "⚠ bir haftadan eski") {
		t.Errorf("%d %s", code, f.out)
	}
}

// Stopped half-way (the app's SIGTERM), nothing of what was begun is left behind, and no
// half file is on the list.
func TestBackupOpenStopped(t *testing.T) {
	f := newFixture(t)
	// A restic that sends a little of the dump, then waits to be stopped.
	slow := filepath.Join(f.tmp, "restic-yavas")
	os.WriteFile(slow, []byte("#!/bin/sh\ncase \"$1\" in\ndump) printf 'yarim'; exec sleep 30 ;;\n*) exec "+f.restic+" \"$@\" ;;\nesac\n"), 0o700)
	f.e.look = func(name string) (string, error) { return slow, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	out := &lockedBuffer{}
	f.e.out = out
	go func() { done <- f.e.run(ctx, []string{"yedek", "ac", "t1", "magaza", "--json"}) }()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), `"event":"start"`) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case code := <-done:
		if code != 1 || !strings.Contains(out.String(), `"message":"durduruldu"`) {
			t.Errorf("%d %s", code, out.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("not stopped")
	}
	if entries, _ := os.ReadDir(f.e.downloads); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if data, _ := os.ReadFile(f.e.opened); strings.Contains(string(data), "magaza") {
		t.Errorf("on the list: %s", data)
	}
}

// lockedBuffer is a buffer written by a command while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Not enough room on the Mac: nothing is begun.
func TestBackupOpenNoRoom(t *testing.T) {
	f := newFixture(t)
	f.e.free = func(string) (uint64, error) { return 100 << 20, nil }
	if code := f.run(t, "ac", "t1", "magaza"); code != 1 || !strings.Contains(f.errb.String(), "Mac'te yer yetmiyor") {
		t.Errorf("%d %s", code, f.errb)
	}
	if entries, _ := os.ReadDir(f.e.downloads); len(entries) != 0 {
		t.Errorf("begun: %v", entries)
	}
}

// The recovery line is what the installer reads, written only into a pipe, never as
// JSON; no secret on stderr.
func TestRecoveryLine(t *testing.T) {
	e, out, errb := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	e.r2 = func(account string) string { return "https://" + account + ".r2.cloudflarestorage.com" }
	e.repository = func(acc access, bucket string) string { return "s3:" + e.r2(acc.Account) + "/" + bucket }
	e.terminal = func() bool { return false }
	ctx := context.Background()
	if code := e.run(ctx, []string{"yedek", "kurtarma", "H1"}); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	r, err := install.ParseRecoveryInfo(out.String())
	if err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	want := install.RecoverAnswers{Repository: "s3:https://" + testAccount + ".r2.cloudflarestorage.com/footops-h1/kasa",
		KeyID: testKeyID, SecretKey: testSecret, Password: "RepoPassword-h1"}
	if r != want {
		t.Errorf("%+v", r)
	}
	if strings.Contains(errb.String(), "RepoPassword") || strings.Contains(errb.String(), testSecret) {
		t.Error("a secret on stderr")
	}
	for _, c := range []struct {
		args     []string
		terminal bool
		code     int
		in       string
	}{
		{[]string{"yedek", "kurtarma", "h1"}, true, 1, "ekrana yazılmaz"},
		{[]string{"yedek", "kurtarma", "h1", "--json"}, false, 1, "JSON'a yazılmaz"},
		{[]string{"yedek", "kurtarma", "yok"}, false, 1, "bilinmeyen sunucu"},
		{[]string{"yedek", "kurtarma"}, false, 2, "Kullanım: footops-mac yedek"},
	} {
		out.Reset()
		errb.Reset()
		e.terminal = func() bool { return c.terminal }
		if code := e.run(ctx, c.args); code != c.code || !strings.Contains(out.String()+errb.String(), c.in) ||
			strings.Contains(out.String()+errb.String(), "RepoPassword") {
			t.Errorf("%v: %d %s %s", c.args, code, out, errb)
		}
	}
	out.Reset()
	e.terminal = func() bool { return true }
	if code := e.run(ctx, []string{"yedek", "kurtarma", "h1", "--goster"}); code != 0 || !strings.HasPrefix(out.String(), recoveryPrefix) {
		t.Errorf("--goster: %d %s", code, out)
	}
}

// yedek's arguments: what is wrong is said, with yedek's usage.
func TestBackupArgs(t *testing.T) {
	e, out, errb := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	for _, c := range []struct {
		args []string
		in   string
	}{
		{[]string{"yedek"}, "Kullanım: footops-mac yedek"},
		{[]string{"yedek", "yok"}, "bilinmeyen komut: yedek yok"},
		{[]string{"yedek", "liste"}, "Kullanım: footops-mac yedek"},
		{[]string{"yedek", "liste", "h1", "--hizli"}, "Kullanım: footops-mac yedek"},
		{[]string{"yedek", "ac", "h1"}, "Kullanım: footops-mac yedek"},
		{[]string{"yedek", "ac", "h1", "site", "--hedef"}, "--hedef bir değer ister"},
		{[]string{"yedek", "ac", "h1", "site", "--hedef="}, "--hedef boş olamaz"},
		{[]string{"yedek", "ac", "h1", "site", "--ad", "x"}, "yalnız --mongo ile"},
		{[]string{"yedek", "ac", "h1", "site", "--uzerine"}, "yalnız --mongo ile"},
		{[]string{"yedek", "ac", "h1", "site", "--bilinmez"}, "bilinmeyen seçenek: --bilinmez"},
		{[]string{"yedek", "mongo", "h1", "site", "--hedef", "x"}, "bilinmeyen seçenek: --hedef"},
		{[]string{"yedek", "sil"}, "Kullanım: footops-mac yedek"},
	} {
		out.Reset()
		errb.Reset()
		if code := e.run(context.Background(), c.args); code != 2 || !strings.Contains(errb.String(), c.in) {
			t.Errorf("%v: %d %s", c.args, code, errb)
		}
	}
	if code := e.run(context.Background(), []string{"yedek", "ac", "h1", "../x"}); code != 1 || !strings.Contains(errb.String(), "geçersiz site adı") {
		t.Errorf("site: %d %s", code, errb)
	}
}

func TestMongoURI(t *testing.T) {
	for _, c := range []struct {
		uri, addr, user, src, label string
	}{
		{"mongodb://127.0.0.1:27017", "127.0.0.1:27017", "", "", "mongodb://127.0.0.1:27017"},
		{"mongodb://localhost", "localhost:27017", "", "", "mongodb://localhost"},
		{"mongodb://root:rootpw@127.0.0.1:27099", "127.0.0.1:27099", "root", "admin", "mongodb://root:xxxxx@127.0.0.1:27099"},
		{"mongodb://u:p@h:1/db?authSource=x", "h:1", "u", "x", "mongodb://u:xxxxx@h:1/db?authSource=x"},
		{"mongodb://u:p@h:1/db", "h:1", "u", "db", "mongodb://u:xxxxx@h:1/db"},
	} {
		got, err := parseMongoURI(c.uri)
		if err != nil || got.addr != c.addr || got.cred.User != c.user || got.cred.Source != c.src || got.label() != c.label {
			t.Errorf("%s: %+v %v %s", c.uri, got, err, got.label())
		}
	}
	for _, bad := range []string{"mongodb+srv://x.example", "mongodb://a:1,b:2", "http://x", "mongodb://h:99999", "mongodb://u:gizli@"} {
		if _, err := parseMongoURI(bad); err == nil || strings.Contains(err.Error(), "gizli") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	if defaultDBName("magaza-v1", time.Date(2026, 11, 7, 3, 0, 5, 0, time.UTC)) != "magaza_v1_yedek_20261107_0300" {
		t.Error(defaultDBName("magaza-v1", time.Date(2026, 11, 7, 3, 0, 5, 0, time.UTC)))
	}
	for _, bad := range []string{"", "a.b", "admin", "a b", strings.Repeat("x", 64), "a/b"} {
		if checkDBName(bad) == nil {
			t.Errorf("%q taken", bad)
		}
	}
}

// A backup's database loaded into a MongoDB (FOOTOPS_TEST_MONGO: one for tests, or the
// Mac's own — the names here are the test's alone and dropped at its end): under its
// default name, refused when the name is taken, written over when asked, listed,
// deleted.
func TestMongoLoad(t *testing.T) {
	uri := os.Getenv("FOOTOPS_TEST_MONGO")
	if uri == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok")
	}
	restic := findRestic(t)
	dump, err := lookTool("mongodump")
	if err != nil {
		t.Skip("mongodump yok")
	}
	mt, err := parseMongoURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	site := "mactest-" + newID()[:6]
	under := strings.ReplaceAll(site, "-", "_")
	t.Cleanup(func() {
		dbs, _ := mt.databases(ctx)
		for name := range dbs {
			if strings.HasPrefix(name, under) || name == site {
				mt.drop(ctx, name)
			}
		}
	})
	// The site's database as on a server, dumped as the agent does.
	c, err := mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, coll := range []string{"urunler", "siparisler"} {
		b := mongo.NewBuilder().String("insert", coll).StartArray("documents")
		for i := range 5 {
			b.StartDoc("").Int32("n", int32(i)).String("ad", coll).End()
		}
		if _, err := c.Run(ctx, site, b.End().MustBuild(), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	archive, err := exec.Command(dump, "--uri="+uri, "--db="+site, "--archive", "--quiet").Output()
	if err != nil {
		t.Fatal(err)
	}
	mt.drop(ctx, site)

	f := &fixture{fixtureRepo: &fixtureRepo{restic: restic}, tmp: t.TempDir()}
	f.repo, f.root = filepath.Join(f.tmp, "repo"), filepath.Join(f.tmp, "footops")
	for _, args := range [][]string{{"init"}, {"backup", "--host", "t1", "--tag", "footops,site=" + site + ",part=db,kind=daily,run=" + nightRun,
		"--time", "2026-11-07 03:00:05", "--stdin", "--stdin-filename", f.root + "/" + site + "/db.archive"}} {
		cmd := exec.Command(restic, args...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+f.repo, "RESTIC_PASSWORD="+fixturePassword, "TZ=UTC",
			"RESTIC_CACHE_DIR="+filepath.Join(f.tmp, "cache-make"))
		cmd.Stdin = bytes.NewReader(archive)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	f.e, f.out, f.errb = f.env(t)
	f.e.mongoURI = uri
	name := under + "_yedek_20261107_0300"

	if code := f.run(t, "mongo", "t1", site, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	ev := events(t, f.out.String())
	res := ev[len(ev)-1]
	if db, _ := res["database"].(map[string]any); res["event"] != "result" || db["name"] != name || db["documents"] != 10.0 || res["target"] != nil {
		t.Fatalf("%v", ev)
	}
	if entries, _ := os.ReadDir(f.e.tmp); len(entries) != 0 {
		t.Errorf("temporary folder left: %v", entries)
	}
	// The name taken: asked first, nothing fetched.
	if code := f.run(t, "mongo", "t1", site, "--json"); code != 1 ||
		!strings.Contains(f.out.String(), `"code":"db_exists"`) || !strings.Contains(f.out.String(), `"database":"`+name+`"`) ||
		strings.Contains(f.out.String(), `"start"`) {
		t.Errorf("taken: %d %s", code, f.out)
	}
	if code := f.run(t, "mongo", "t1", site, "--uzerine"); code != 0 || !strings.Contains(f.out.String(), "yüklendi: "+name+" (10 belge)") {
		t.Errorf("over: %d %s %s", code, f.out, f.errb)
	}
	// A folder and the database under another name.
	other := under + "_yedek_ikinci"
	if code := f.run(t, "ac", "t1", site, "--mongo", "--ad", other); code != 0 {
		t.Fatalf("ac --mongo: %d %s %s", code, f.out, f.errb)
	}
	if code := f.run(t, "acilanlar", "--json"); code != 0 {
		t.Fatal(f.errb)
	}
	var v OpenedView
	json.Unmarshal(f.out.Bytes(), &v)
	listed := map[string]bool{}
	for _, d := range v.Databases {
		listed[d.Name] = d.Registered
	}
	if v.MongoError != "" || !listed[name] || !listed[other] || len(v.Folders) != 1 {
		t.Errorf("%+v", v)
	}
	if code := f.run(t, "sil", name); code != 0 || !strings.Contains(f.out.String(), "Silindi: MongoDB / "+name) {
		t.Errorf("%d %s %s", code, f.out, f.errb)
	}
	if dbs, _ := mt.databases(ctx); dbs == nil || dbs[name] != 0 {
		if _, still := dbs[name]; still {
			t.Error("not dropped")
		}
	}
	if code := f.run(t, "sil", name); code != 0 || !strings.Contains(f.out.String(), "zaten yok") {
		t.Errorf("again: %d %s", code, f.out)
	}
}

// Files restic could not write are warnings, the rest opened; a restore that fails
// before its summary fails the open (restic says both on stderr, in JSON).
func TestBackupOpenRestoreWarnings(t *testing.T) {
	f := newFixture(t)
	fake := func(name, restore string) string {
		p := filepath.Join(f.tmp, name)
		os.WriteFile(p, []byte("#!/bin/sh\ncase \"$1\" in\nrestore)\n"+restore+"\n;;\n*) exec "+f.restic+" \"$@\" ;;\nesac\n"), 0o700)
		return p
	}
	warn := fake("restic-uyari", `printf '{"message_type":"summary","total_files":3,"files_restored":2,"total_bytes":10,"bytes_restored":8}\n'
	printf '{"message_type":"error","error":{"message":"permission denied"},"during":"restore","item":"/kilitli/c.txt"}\n' >&2
	printf '{"message_type":"exit_error","code":1,"message":"Fatal: There were 1 errors"}\n' >&2
	exit 1`)
	f.e.look = func(string) (string, error) { return warn, nil }
	if code := f.run(t, "ac", "t1", "magaza", "--dosyalar", "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	ev := events(t, f.out.String())
	if w, _ := ev[len(ev)-1]["warnings"].([]any); len(w) != 1 || w[0] != "1 dosya açılamadı: /kilitli/c.txt: permission denied" {
		t.Errorf("%v", ev[len(ev)-1])
	}
	broken := fake("restic-bozuk", `printf '{"message_type":"exit_error","code":1,"message":"Fatal: unable to open repository at s3:https://`+testAccount+`.r2.cloudflarestorage.com/x: Access Denied"}\n' >&2
	exit 1`)
	f.e.look = func(string) (string, error) { return broken, nil }
	code := f.run(t, "ac", "t1", "magaza", "--dosyalar", "--json")
	ev = events(t, f.out.String())
	if msg, _ := ev[len(ev)-1]["message"].(string); code != 1 ||
		!strings.HasPrefix(msg, "dosyalar açılamadı: restic: unable to open repository at s3:https://<hesap>.r2") {
		t.Errorf("%d %s", code, f.out)
	}
	if strings.Contains(f.out.String(), testAccount) {
		t.Error("the account shown")
	}
}

func TestToolWords(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"Fatal: wrong password or no key found\n", "wrong password or no key found"},
		{"created new cache\nFatal: unable to open config file: Stat: Access Denied.\nIs there a repository?\n", "unable to open config file: Stat: Access Denied."},
		{`{"message_type":"error","error":{"message":"no space left"},"item":"/a"}` + "\n" + `{"message_type":"exit_error","code":1,"message":"Fatal: There were 3 errors"}`,
			"/a: no space left (There were 3 errors)"},
		{"2026-11-09T22:00:00 Failed: error connecting to host: connection refused\n", "2026-11-09T22:00:00 Failed: error connecting to host: connection refused"},
		{"Failed: error connecting to host: connection refused\n", "error connecting to host: connection refused"},
		{"", "exit status 1"},
	} {
		if got := toolWords(c.in, "exit status 1"); got != c.want {
			t.Errorf("%q: %q", c.in, got)
		}
	}
}


// childOpen is footops-mac yedek ac --json as the app runs it, in a process of its own
// (this test binary again), its signals as Run sets them.
func childOpen(conf string) int {
	var c struct{ Restic, Repo, Root, Tmp, KC string }
	if err := json.Unmarshal([]byte(conf), &c); err != nil {
		return 3
	}
	ctx, lost, done := commandContext()
	defer done()
	e := &env{out: os.Stdout, errw: os.Stderr,
		keychain: fakeKeychain{items: map[string]string{"r2-hesap": testAccount, "r2-mac-erisim": testKeyID, "r2-mac-gizli": testSecret,
			"restic-t1": fixturePassword}, cmd: []string{c.KC}},
		settings: filepath.Join(c.Tmp, "ayarlar.json"), r2: func(a string) string { return "https://" + a + ".example" }, now: time.Now,
		look: func(string) (string, error) { return c.Restic, nil }, repository: func(access, string) string { return c.Repo },
		backupRoot: c.Root, resticCache: filepath.Join(c.Tmp, "cache"), downloads: filepath.Join(c.Tmp, "Downloads"),
		opened: filepath.Join(c.Tmp, "ayarlar", "acilanlar.json"), tmp: filepath.Join(c.Tmp, "gecici"), mongoURI: "mongodb://127.0.0.1:1",
		terminal: func() bool { return false }, free: freeRoom, lost: lost}
	return e.run(ctx, []string{"yedek", "ac", "t1", "magaza", "--json"})
}

// The app gone under a yedek ac (quit, crashed, killed): its end of the pipe closes, the
// core is not killed by SIGPIPE — the next line it cannot write (a heartbeat while restic
// is silent) stops it, and it removes what it began.
func TestOpenAppGone(t *testing.T) {
	f := newFixture(t)
	slow := filepath.Join(f.tmp, "restic-yavas")
	os.WriteFile(slow, []byte("#!/bin/sh\ncase \"$1\" in\ndump) printf 'yarim'; exec sleep 30 ;;\n*) exec "+f.restic+" \"$@\" ;;\nesac\n"), 0o700)
	conf, _ := json.Marshal(map[string]string{"Restic": slow, "Repo": f.repo, "Root": f.root, "Tmp": f.tmp, "KC": filepath.Join(f.tmp, "kc.sh")})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "FOOTOPS_MAC_TEST_CHILD="+string(conf))
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	started := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"event":"progress"`) { // the folder made, the dump under way
				started <- true
				return
			}
		}
		started <- false
	}()
	select {
	case ok := <-started:
		if !ok {
			t.Fatal("no progress line")
		}
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("not started")
	}
	if entries, _ := os.ReadDir(f.e.downloads); len(entries) != 1 {
		t.Fatalf("no folder under way: %v", entries)
	}
	out.Close() // the app is gone
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the core did not notice")
	}
	if ws := cmd.ProcessState.Sys().(syscall.WaitStatus); ws.Signaled() || ws.ExitStatus() != 1 {
		t.Errorf("ended %v (want exit 1, no signal)", cmd.ProcessState)
	}
	if entries, _ := os.ReadDir(f.e.downloads); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	if data, _ := os.ReadFile(f.e.opened); strings.Contains(string(data), "magaza") {
		t.Errorf("on the list: %s", data)
	}
}

// A folder is on the list from its first moment: under way (its mark held) it is busy
// and not deleted; cut short (the mark let go, as by a killed process) it is shown
// partial and deleted like any other.
func TestOpenedPartial(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.e.downloads, "magaza-20261107-0300")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "db.archive"), []byte("yarim"), 0o600)
	m, err := makeMark(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.beginOpened(f.e.folderEntry(m, dir, "t1", "magaza", time.Date(2026, 11, 7, 3, 0, 5, 0, time.UTC), false)); err != nil {
		t.Fatal(err)
	}
	view := func() OpenedFolder {
		t.Helper()
		if code := f.run(t, "acilanlar", "--json"); code != 0 {
			t.Fatalf("%d %s", code, f.errb)
		}
		var v OpenedView
		json.Unmarshal(f.out.Bytes(), &v)
		if len(v.Folders) != 1 {
			t.Fatalf("%+v", v)
		}
		return v.Folders[0]
	}
	if got := view(); !got.Partial || !got.Busy {
		t.Errorf("under way: %+v", got)
	}
	if code := f.run(t, "sil", dir); code != 1 || !strings.Contains(f.errb.String(), "şu an açılıyor") {
		t.Errorf("deleted under way: %d %s", code, f.errb)
	}
	m.release()
	if got := view(); !got.Partial || got.Busy {
		t.Errorf("cut short: %+v", got)
	}
	if code := f.run(t, "acilanlar"); code != 0 {
		t.Fatal(f.errb)
	}
	if code := f.run(t, "sil", dir); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("still there: %v", err)
	}
}

// A folder that cannot be looked at now (macOS privacy, a disk not mounted) keeps its
// line, said so; once it can, it is deleted as before.
func TestOpenedUnreadableKept(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(f.tmp, "kapali")
	os.MkdirAll(parent, 0o700)
	target := filepath.Join(parent, "x")
	if code := f.run(t, "ac", "t1", "magaza", "--db", "--hedef", target); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	os.Chmod(parent, 0)
	defer os.Chmod(parent, 0o700)
	if code := f.run(t, "acilanlar", "--json"); code != 0 {
		t.Fatal(f.errb)
	}
	var v OpenedView
	json.Unmarshal(f.out.Bytes(), &v)
	if len(v.Folders) != 1 || !strings.HasPrefix(v.Folders[0].Error, "bakılamadı") {
		t.Fatalf("%+v", v)
	}
	os.Chmod(parent, 0o700)
	if code := f.run(t, "sil", target); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
}

// A delete cut short leaves the mark (removed last): the folder is still one opened, and
// deleted once it can be.
func TestDeleteCutShort(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chflags: macOS")
	}
	f := newFixture(t)
	target := filepath.Join(f.tmp, "kesik")
	if code := f.run(t, "ac", "t1", "magaza", "--hedef", target); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if out, err := exec.Command("chflags", "uchg", filepath.Join(target, "env")).CombinedOutput(); err != nil {
		t.Fatalf("chflags: %v %s", err, out)
	}
	defer exec.Command("chflags", "nouchg", filepath.Join(target, "env")).Run()
	if code := f.run(t, "sil", target); code != 1 || !strings.Contains(f.errb.String(), "silinemedi") {
		t.Fatalf("%d %s", code, f.errb)
	}
	if _, err := os.Stat(filepath.Join(target, openedMark)); err != nil {
		t.Errorf("mark gone: %v", err)
	}
	if code := f.run(t, "acilanlar"); code != 0 || !strings.Contains(f.out.String(), target) {
		t.Errorf("off the list: %s", f.out)
	}
	exec.Command("chflags", "nouchg", filepath.Join(target, "env")).Run()
	if code := f.run(t, "sil", target); code != 0 || !strings.Contains(f.out.String(), "Silindi: "+target) {
		t.Errorf("%d %s %s", code, f.out, f.errb)
	}
}

// Delete removes what footops-mac wrote, not what the owner put in the folder since; a
// folder that was there before (an empty --hedef, the Finder's .DS_Store aside) stays.
func TestDeleteKeepsOwnersFiles(t *testing.T) {
	f := newFixture(t)
	mine := filepath.Join(f.tmp, "benim")
	os.MkdirAll(mine, 0o755)
	os.WriteFile(filepath.Join(mine, ".DS_Store"), []byte("finder"), 0o644)
	if code := f.run(t, "ac", "t1", "magaza", "--hedef", mine); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if code := f.run(t, "sil", mine); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if entries, err := os.ReadDir(mine); err != nil || len(entries) != 0 {
		t.Errorf("the folder of the owner's: %v %v", entries, err)
	}
	if code := f.run(t, "ac", "t1", "magaza", "--db"); code != 0 {
		t.Fatal(f.errb)
	}
	target := filepath.Join(f.e.downloads, "magaza-20261107-1400")
	os.WriteFile(filepath.Join(target, "notlarim.txt"), []byte("benim"), 0o600)
	if code := f.run(t, "sil", target); code != 0 || !strings.Contains(f.out.String(), "klasör bırakıldı") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if entries, _ := os.ReadDir(target); len(entries) != 1 || entries[0].Name() != "notlarim.txt" {
		t.Errorf("%v", entries)
	}
	if code := f.run(t, "acilanlar"); code != 0 || strings.Contains(f.out.String(), target) {
		t.Errorf("still listed: %s", f.out)
	}
}

// The account is masked before a long message is clipped (else its start would show).
func TestMaskBeforeClip(t *testing.T) {
	r := &repo{account: testAccount, bucket: "b", code: "t1"}
	err := r.failure(errors.New("exit status 1"), "Fatal: "+strings.Repeat("x", 280)+" s3:https://"+testAccount+".r2.cloudflarestorage.com/b")
	if strings.Contains(err.Error(), testAccount[:6]) {
		t.Errorf("%q", err)
	}
}

// A recovery line sent into a file: the file is the owner's only.
func TestRecoveryToFile(t *testing.T) {
	e, _, errb := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	e.terminal = func() bool { return false }
	e.repository = func(acc access, bucket string) string { return "s3:https://" + acc.Account + ".example/" + bucket }
	path := filepath.Join(t.TempDir(), "satir.txt")
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	e.out = out
	if code := e.run(context.Background(), []string{"yedek", "kurtarma", "h1"}); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("%v", fi.Mode())
	}
}

// A password in MONGO_URI goes to mongorestore in a private file, not on its command
// line (ps shows that); the file goes when it ends.
func TestMongoConfigFile(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "mongorestore")
	os.WriteFile(tool, []byte(`#!/bin/sh
echo "$@" > "`+dir+`/args"
for a in "$@"; do case "$a" in --config=*) cat "${a#--config=}" > "`+dir+`/conf"; ls -l "${a#--config=}" > "`+dir+`/mode" ;; esac; done
cat > /dev/null
echo "2026-11-09T22:00:00.000+0300	3 document(s) restored successfully. 0 document(s) failed to restore." >&2
`), 0o700)
	archive := filepath.Join(dir, "db.archive")
	os.WriteFile(archive, []byte("arsiv"), 0o600)
	e := &env{look: func(string) (string, error) { return tool, nil }, tmp: dir}
	mt, err := parseMongoURI("mongodb://kisi:Gizli-Sifre@127.0.0.1:1/?authSource=admin")
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.loadDB(context.Background(), mt, archive, "magaza", "magaza_yedek_x", false, nil)
	if err != nil || db.Documents != 3 {
		t.Fatalf("%v %+v", err, db)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	conf, _ := os.ReadFile(filepath.Join(dir, "conf"))
	mode, _ := os.ReadFile(filepath.Join(dir, "mode"))
	if strings.Contains(string(args), "Gizli-Sifre") || !strings.Contains(string(args), "--config=") ||
		string(conf) != "uri: \"mongodb://kisi:Gizli-Sifre@127.0.0.1:1/?authSource=admin\"\n" || !strings.HasPrefix(string(mode), "-rw-------") {
		t.Errorf("args %q conf %q mode %q", args, conf, mode)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "footops-mac-mongo-*")); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
}

// A line that could not be put on the list at the start is put there at the end: a
// folder opened whole is always listed.
func TestEndOpenedAddsMissing(t *testing.T) {
	dir := t.TempDir()
	e := &env{opened: filepath.Join(dir, "acilanlar.json"), now: func() time.Time { return testNow }}
	it := openedEntry{ID: "abc", Kind: "folder", Path: filepath.Join(dir, "x"), Server: "t1", Site: "magaza", Partial: true}
	if err := e.endOpened(it, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(e.opened)
	var l openedList
	if json.Unmarshal(data, &l) != nil || len(l.Items) != 1 || l.Items[0].Partial || l.Items[0].ID != "abc" || !l.Items[0].At.Equal(testNow) {
		t.Errorf("%s", data)
	}
}

// Only a folder footops-mac made is kept out of Time Machine: one that was there before
// is the owner's.
func TestTimeMachineOnlyMadeFolders(t *testing.T) {
	f := newFixture(t)
	var excluded []string
	f.e.exclude = func(dir string) { excluded = append(excluded, dir) }
	mine := filepath.Join(f.tmp, "benim")
	os.MkdirAll(mine, 0o700)
	if code := f.run(t, "ac", "t1", "magaza", "--db", "--hedef", mine); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if code := f.run(t, "ac", "t1", "magaza", "--db"); code != 0 {
		t.Fatalf("%d %s", code, f.errb)
	}
	if len(excluded) != 1 || excluded[0] != filepath.Join(f.e.downloads, "magaza-20261107-1400") {
		t.Errorf("%v", excluded)
	}
}

// mongorestore's own words name the URI, its password encoded: none of it shows.
func TestMongoWordsMasked(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "mongorestore")
	os.WriteFile(tool, []byte("#!/bin/sh\ncat > /dev/null\necho 'error connecting to host: failed to connect to mongodb://kisi:rev%40pw@127.0.0.1:1/?authSource=admin: refused' >&2\nexit 1\n"), 0o700)
	archive := filepath.Join(dir, "db.archive")
	os.WriteFile(archive, []byte("arsiv"), 0o600)
	e := &env{look: func(string) (string, error) { return tool, nil }, tmp: dir}
	mt, err := parseMongoURI("mongodb://kisi:rev%40pw@127.0.0.1:1/?authSource=admin")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.loadDB(context.Background(), mt, archive, "magaza", "magaza_yedek_x", false, nil)
	if err == nil || strings.Contains(err.Error(), "rev") || strings.Contains(err.Error(), "kisi") || !strings.Contains(err.Error(), "mongodb://xxxxx@") {
		t.Errorf("%v", err)
	}
}

// The Terminal's list says what is under way, cut short, not looked at.
func TestPrintOpenedStates(t *testing.T) {
	var b bytes.Buffer
	now := testNow
	printOpened(&b, OpenedView{Time: now, Folders: []OpenedFolder{
		{Path: "/a", Server: "t1", Site: "s", Time: now, At: now, Busy: true, Partial: true},
		{Path: "/b", Server: "t1", Site: "s", Time: now, At: now, Partial: true},
		{Path: "/c", Server: "t1", Site: "s", Time: now, At: now, Error: "bakılamadı: izin yok"},
	}, MongoError: "yok"})
	out := b.String()
	for _, want := range []string{"/a", "… açılıyor", "⚠ yarım kaldı", "t1 / s · bakılamadı: izin yok"} {
		if !strings.Contains(out, want) {
			t.Errorf("%q not in\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "bakılamadı") && strings.Contains(line, "dosya") {
			t.Errorf("a folder not looked at counted: %q", line)
		}
	}
}
