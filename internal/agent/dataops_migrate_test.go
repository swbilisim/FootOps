package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
)

// fakeSource plays the old server behind ssh: its databases, its files, and the ways
// a copy from it can go wrong. Everything else goes to fakeMongo.
type fakeSource struct {
	*fakeMongo
	mu       sync.Mutex
	dbs      map[string]map[string]int64 // the source's MongoDB
	files    map[string]string           // its uploads folder
	pin      string                      // host key written on the first connection
	commands []string                    // remote command lines seen

	tarExit    int    // exit status of the tar side (0: fine)
	tarSays    string // what tar says on stderr with tarExit
	tarCut     bool   // the connection drops half-way through the tar (ssh 255)
	noTar      bool   // the script fails before tar runs (exit 1, nothing sent)
	noEnd      bool   // the stream lacks its end line (yet ssh ends well)
	countExtra int    // files counted on the source beyond those sent
	extra      int    // files sent beyond those counted
	findError  bool   // find reports a folder it cannot read
	dumpExit   int
	dumpLie    int64 // mongodump reports this many documents more than it wrote
	dumpNames  int   // mongodump reports this many made-up collections
	discover   string
}

func remoteLine(c run.Cmd) string {
	if len(c.Args) == 0 {
		return ""
	}
	return c.Args[len(c.Args)-1]
}

func isSSH(c run.Cmd) bool {
	return c.Name == "ssh" || (c.Name == "systemd-run" && contains(c.Args, "ssh"))
}

// exitStatus makes the error StreamExec returns for a command that exited with code.
func exitStatus(code int, msg string) error {
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	return fmt.Errorf("ssh: %w: %s", err, msg)
}

func (f *fakeSource) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	if !isSSH(c) {
		return f.fakeMongo.Stream(ctx, c)
	}
	line := remoteLine(c)
	f.mu.Lock()
	f.commands = append(f.commands, line)
	f.mu.Unlock()
	u := run.Usage{PeakMem: 6 << 20, CPU: 10e6}
	if strings.Contains(strings.Join(c.Args, " "), "accept-new") && f.pin != "" {
		for _, a := range c.Args {
			if v, ok := strings.CutPrefix(a, "UserKnownHostsFile="); ok {
				_ = os.WriteFile(v, []byte(f.pin+"\n"), 0o600)
			}
		}
	}
	switch {
	case strings.Contains(line, "FOOTOPS_PROBE"):
		_, err := io.WriteString(c.Stdout, "FOOTOPS_PROBE root|Eski Linux 7|3.10.0\n")
		return u, err
	case strings.Contains(line, "FOOTOPS_COUNT"):
		var n, b int64
		for _, body := range f.files {
			n++
			b += int64(len(body))
		}
		n += int64(f.countExtra)
		_, err := fmt.Fprintf(c.Stdout, "FOOTOPS_COUNT %d %d 1 2 1 0 0\n", n, b)
		return u, err
	case strings.Contains(line, "emit msrc"):
		_, err := io.WriteString(c.Stdout, f.discover)
		return u, err
	case strings.Contains(line, "mongodump --archive --gzip"):
		if f.dumpExit != 0 {
			c.Stderr("mongodump: command not found")
			return u, exitStatus(f.dumpExit, "")
		}
		f.mu.Lock()
		db := map[string]map[string]int64{"eski": f.dbs["eski"]}
		f.mu.Unlock()
		for _, coll := range sortedKeys(db["eski"]) {
			c.Stderr(fmt.Sprintf("2026-10-28T10:00:00.000+0300\tdone dumping eski.%s (%d documents)", coll, db["eski"][coll]+f.dumpLie))
		}
		for i := 0; i < f.dumpNames; i++ {
			c.Stderr(fmt.Sprintf("x\tdone dumping eski.%s%d (1 documents)", strings.Repeat("y", 300), i))
		}
		_, err := c.Stdout.Write(gzipped(fakeArchive(db, nil)))
		return u, err
	case strings.Contains(line, "tar --null --no-recursion"):
		if f.noTar {
			c.Stderr("ionice: ioprio_set failed: Operation not permitted")
			return u, exitStatus(1, "")
		}
		if f.findError {
			c.Stderr("find: ‘./gizli’: Permission denied")
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		files := map[string]string{}
		for k, v := range f.files {
			files[k] = v
		}
		for i := 0; i < f.extra; i++ {
			files[fmt.Sprintf("fazla/%d.bin", i)] = strings.Repeat("z", 1000)
		}
		for _, name := range sortedStrKeys(files) {
			tw.WriteHeader(&tar.Header{Name: "./" + name, Size: int64(len(files[name])), Mode: 0o600, Typeflag: tar.TypeReg})
			tw.Write([]byte(files[name]))
		}
		tw.Close()
		if !f.noEnd {
			fmt.Fprintf(&buf, "%s %d\n", remote.TarEnd, f.tarExit)
		}
		b := buf.Bytes()
		if f.tarCut {
			// The connection drops exactly after the first member: what arrived looks
			// like a whole (shorter) tar; only ssh's exit status tells.
			c.Stdout.Write(b[:1024])
			c.Stderr("Connection closed by 203.0.113.10 port 22")
			return u, exitStatus(255, "Connection closed by 203.0.113.10 port 22")
		}
		if _, err := c.Stdout.Write(b); err != nil {
			return u, err
		}
		if f.tarExit != 0 {
			c.Stderr(f.tarSays)
			return u, exitStatus(f.tarExit, "")
		}
		return u, nil
	case strings.Contains(line, "authorized_keys"):
		return u, nil
	}
	return u, fmt.Errorf("unexpected remote command: %.80s", line)
}

type migEnv struct {
	*f2Env
	src  *fakeSource
	srcs *remote.Store
	id   string
	blob string // the source's public key, as it appears in authorized_keys
}

func newMig(t *testing.T) *migEnv {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen yok")
	}
	f := newF2(t)
	fs := &fakeSource{fakeMongo: f.mongo, dbs: map[string]map[string]int64{"eski": {"urunler": 10, "yorumlar": 4}},
		files: map[string]string{"a.jpg": "jpeg-a", "blog/b.png": "png-b", "blog/2026/c ç.pdf": "pdf"}}
	f.data.Stream = fs
	store := &remote.Store{Dir: filepath.Join(f.root, "sources"), Run: run.Exec{}, Stream: fs, Name: "test"}
	f.data.Sources, f.data.MigrationsDir = store, filepath.Join(f.root, "migrations")
	src, err := store.Create(ctx, "eski sunucu", "203.0.113.10", 22, "root", "admin")
	if err != nil {
		t.Fatal(err)
	}
	pub := strings.Fields(src.PublicKey)
	fs.pin = "203.0.113.10 " + pub[0] + " " + pub[1] // any valid key will do as a host key
	if _, err := store.Test(ctx, src.ID); err != nil {
		t.Fatal(err)
	}
	f.writeData(t, map[string]string{"eski.jpg": "onceki"})
	return &migEnv{f2Env: f, src: fs, srcs: store, id: src.ID, blob: pub[1]}
}

func (m *migEnv) choose(t *testing.T, c MigrationChoice) {
	t.Helper()
	if c.Source == "" {
		c.Source = m.id
	}
	if _, err := m.data.SetMigration("magaza", c, "admin"); err != nil {
		t.Fatal(err)
	}
}

func both() MigrationChoice {
	return MigrationChoice{DB: "eski", FilesDir: "/home/eski/www/public/uploads", AppDir: "/home/eski/www"}
}

func TestMigrateCopy(t *testing.T) {
	m := newMig(t)
	m.choose(t, both())
	ct := m.task()
	res, err := m.data.MigrateCopy(ctx, ct, "magaza", Actor{User: "admin"})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(ct))
	}
	if got := m.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 10, "yorumlar": 4}) {
		t.Errorf("site database = %v", got)
	}
	if got := m.readData(t); !reflect.DeepEqual(got, m.src.files) {
		t.Errorf("site files = %v", got)
	}
	if res.SourceDocTotal() != 14 || res.SiteDocTotal() != 14 || res.SourceFiles.Files != 3 || res.SiteFiles.Files != 3 ||
		res.DumpBytes == 0 || res.Undo == "" || len(res.Notes) != 0 {
		t.Errorf("result = %+v", res)
	}
	// The previous state is kept: a safety copy of the database and the old folder.
	snaps, _ := m.data.Snapshots("magaza")
	if len(snaps) != 1 || snaps[0].Reason != "before-migrate" || snaps[0].Documents != 12 {
		t.Errorf("snapshots = %+v", snaps)
	}
	if b, _ := m.data.FilesBackups("magaza"); len(b) != 1 {
		t.Errorf("files backups = %+v", b)
	}
	if !m.running() {
		t.Error("site not running")
	}
	saved, err := m.data.Migration("magaza")
	if err != nil || len(saved.Copies) != 1 || saved.LastCopy().Job != ct.ID() || saved.LastCopy().Err != "" {
		t.Errorf("record = %+v %v", saved, err)
	}
	// Nothing left in staging (the dump, the tool's connection file).
	if ents, _ := os.ReadDir(filepath.Join(m.root, "staging")); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(m.root, "srv/musteri/magaza/data.new")); !os.IsNotExist(err) {
		t.Error("data.new left")
	}
	// The remote commands carried their arguments quoted and nothing secret.
	all := strings.Join(m.src.commands, "\n")
	for _, want := range []string{"'host' '-' 'eski'", "'/home/eski/www/public/uploads'"} {
		if !strings.Contains(all, want) {
			t.Errorf("remote commands lack %s:\n%s", want, all)
		}
	}
	// Copying again (the pre-copy is repeatable): the result is the same, one more copy each.
	m.src.files["yeni.jpg"] = "sonradan"
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err != nil {
		t.Fatal(err)
	}
	if got := m.readData(t); got["yeni.jpg"] != "sonradan" || len(got) != 4 {
		t.Errorf("after second copy: %v", got)
	}
	if saved, _ := m.data.Migration("magaza"); len(saved.Copies) != 2 {
		t.Errorf("copies = %d", len(saved.Copies))
	}
}

// unchanged checks that a failed copy left the site as it was.
func (m *migEnv) unchanged(t *testing.T) {
	t.Helper()
	if got := m.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}) {
		t.Errorf("site database changed: %v", got)
	}
	if got := m.readData(t); !reflect.DeepEqual(got, map[string]string{"eski.jpg": "onceki"}) {
		t.Errorf("site files changed: %v", got)
	}
	if !m.running() {
		t.Error("site left stopped")
	}
	if _, err := os.Stat(filepath.Join(m.root, "srv/musteri/magaza/data.new")); !os.IsNotExist(err) {
		t.Error("data.new left")
	}
	if len(m.data.Recoveries()) != 0 {
		t.Error("recovery record left")
	}
	if ents, _ := os.ReadDir(filepath.Join(m.root, "staging")); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
}

func TestMigrateConnectionDrops(t *testing.T) {
	m := newMig(t)
	m.choose(t, both())
	m.src.tarCut = true
	ct := m.task()
	_, err := m.data.MigrateCopy(ctx, ct, "magaza", Actor{})
	if err == nil || !strings.Contains(err.Error(), "koptu") {
		t.Fatalf("err = %v\n%s", err, steps(ct))
	}
	m.unchanged(t)
	if saved, _ := m.data.Migration("magaza"); saved.LastCopy() == nil || saved.LastCopy().Err == "" {
		t.Error("the failure is not kept in the record")
	}
}

func TestMigrateFindError(t *testing.T) {
	m := newMig(t)
	m.choose(t, MigrationChoice{FilesDir: "/home/eski/www/public/uploads"})
	m.src.findError = true
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err == nil || !strings.Contains(err.Error(), "okunamadı") {
		t.Fatalf("err = %v", err)
	}
	m.unchanged(t)
}

func TestMigrateTarChangedIsANote(t *testing.T) {
	m := newMig(t)
	m.choose(t, MigrationChoice{FilesDir: "/home/eski/www/public/uploads"})
	// The site still runs on the source: a file changed or vanished while read is a note.
	for _, c := range []struct {
		exit int
		says string
	}{{1, "tar: ./a.jpg: file changed as we read it"}, {2, "tar: ./b.jpg: Cannot stat: No such file or directory"}} {
		m.src.tarExit, m.src.tarSays = c.exit, c.says
		res, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{})
		if err != nil {
			t.Fatalf("%s: %v", c.says, err)
		}
		if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "okunurken değişti") {
			t.Errorf("notes = %v", res.Notes)
		}
	}
	// Any other tar error is not.
	m.src.tarExit, m.src.tarSays = 2, "tar: ./c.jpg: Cannot open: Permission denied"
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err == nil {
		t.Error("tar exit 2 accepted")
	}
}

// A copy that cannot be shown to be whole changes nothing.
func TestMigrateRefusesIncompleteStreams(t *testing.T) {
	for name, set := range map[string]func(*fakeSource){
		"script failed before tar ran (exit 1, nothing sent)": func(f *fakeSource) { f.noTar = true },
		"stream without its end line":                         func(f *fakeSource) { f.noEnd = true },
		"far fewer files than counted": func(f *fakeSource) {
			f.files = map[string]string{"a.jpg": "jpeg-a"}
			f.countExtra = 10
		},
		"far more than counted": func(f *fakeSource) { f.extra = 1500 },
	} {
		t.Run(name, func(t *testing.T) {
			m := newMig(t)
			m.choose(t, MigrationChoice{FilesDir: "/home/eski/www/public/uploads"})
			set(m.src)
			ct := m.task()
			if _, err := m.data.MigrateCopy(ctx, ct, "magaza", Actor{}); err == nil {
				t.Fatalf("applied:\n%s", steps(ct))
			} else {
				t.Log(err)
			}
			m.unchanged(t)
		})
	}
}

func TestMigrateRefusesOddDumpReports(t *testing.T) {
	m := newMig(t)
	m.choose(t, MigrationChoice{DB: "eski"})
	m.src.dumpNames = 3
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err == nil || !strings.Contains(err.Error(), "beklenmeyen") {
		t.Errorf("err = %v", err)
	}
	m.unchanged(t)
}

func TestMigrateDumpProblems(t *testing.T) {
	m := newMig(t)
	m.choose(t, both())
	m.src.dumpExit = remote.ExitNoMongo
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err == nil || !strings.Contains(err.Error(), "mongodump yok") {
		t.Errorf("no mongodump: %v", err)
	}
	m.unchanged(t)
	// mongodump says more than the archive holds: the trial restore catches it.
	m.src.dumpExit, m.src.dumpLie = 0, 1
	ct := m.task()
	if _, err := m.data.MigrateCopy(ctx, ct, "magaza", Actor{}); err == nil || !strings.Contains(err.Error(), "tutmuyor") {
		t.Errorf("counts: %v\n%s", err, steps(ct))
	}
	m.unchanged(t)
	if len(m.mongo.users) != 0 {
		t.Errorf("temporary user left: %v", m.mongo.users)
	}
}

func TestMigrateRollsBackWhenSiteFails(t *testing.T) {
	m := newMig(t)
	m.choose(t, both())
	m.sites.health = func(started int) error {
		if started == 1 {
			return errors.New("502 Bad Gateway")
		}
		return nil
	}
	ct := m.task()
	_, err := m.data.MigrateCopy(ctx, ct, "magaza", Actor{})
	if err == nil || !strings.Contains(err.Error(), "geri alındı") {
		t.Fatalf("err = %v\n%s", err, steps(ct))
	}
	m.unchanged(t)
}

func TestSetMigration(t *testing.T) {
	m := newMig(t)
	for _, c := range []MigrationChoice{
		{Source: m.id, FilesDir: "/etc"},
		{Source: m.id, FilesDir: "relative/path"},
		{Source: m.id, DB: "admin"},
		{Source: m.id, DB: "x'; drop"},
		{Source: m.id, DB: "eski", Container: "-it"},
		{Source: "yokyokyokyok"},
	} {
		if _, err := m.data.SetMigration("magaza", c, ""); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	m.choose(t, both())
	if err := m.data.saveJSON("magaza", discoveryFile, remote.Discovery{Complete: true}); err != nil {
		t.Fatal(err)
	}
	// Choosing another source forgets the picks and what was found on the old one.
	other, err := m.srcs.Create(ctx, "başka", "203.0.113.11", 22, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.data.SetMigration("magaza", MigrationChoice{Source: other.ID, SourceOnly: true}, "")
	if err != nil || got.DB != "" || got.FilesDir != "" || got.Source != other.ID {
		t.Errorf("after source change: %+v %v", got, err)
	}
	if d, _ := m.data.MigrationSaved("magaza"); d != nil {
		t.Error("discovery of the old source kept")
	}
	// The new source was never connected to: no copy.
	if _, err := m.data.MigrateCopy(ctx, m.task(), "magaza", Actor{}); err == nil || !strings.Contains(err.Error(), "Bağlantıyı dene") {
		t.Errorf("unconnected source: %v", err)
	}
	if err := m.data.ForgetMigration("magaza"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.data.Migration("magaza"); !errors.Is(err, ErrNoMigration) {
		t.Errorf("after forget: %v", err)
	}
}

func TestMigrateDiscover(t *testing.T) {
	m := newMig(t)
	m.choose(t, MigrationChoice{})
	domain := specFor("magaza").Domains[0] // the test site's own domain
	m.src.discover = strings.Join([]string{
		"os\tEski Linux 7\t3.10.0\tx86_64\teski\troot",
		"msrc\thost\t-\tmongo",
		"FOOTOPS_MVER\t4.0.28",
		"FOOTOPS_MDB\teski\t2\t14\t5000\t9000",
		"ngx\tserver { server_name " + domain + "; location / { proxy_pass http://127.0.0.1:8196; } }",
		"proc\t20\t1\troot\t1000\t/usr/bin/node\t/home/eski/www\tnode app.js\tname=eski|pm_id=0|",
		"listen\tLISTEN 0 511 *:8196 *:* users:((\"node\",pid=20,fd=20))",
		"cand\t/home/eski/www/public/uploads\t3\t20\t2\t0\t0\t0\t0",
		"end",
	}, "\n")
	dt := m.task()
	if _, err := m.data.Discover(ctx, dt, "magaza"); err != nil {
		t.Fatalf("%v\n%s", err, steps(dt))
	}
	d, _ := m.data.MigrationSaved("magaza")
	if d == nil || len(d.Mongo) != 1 || d.Mongo[0].DBs[0].Name != "eski" {
		t.Fatalf("saved = %+v", d)
	}
	sg := remote.Suggest(*d, nil, "magaza", specFor("magaza").Domains)
	if sg.AppDir != "/home/eski/www" || sg.FilesDir != "/home/eski/www/public/uploads" {
		t.Errorf("suggestion = %+v", sg)
	}
	m.src.discover = "os\tyarım"
	if _, err := m.data.Discover(ctx, m.task(), "magaza"); err == nil {
		t.Error("a cut-off discovery accepted")
	}
}

func TestMigrationEndpoints(t *testing.T) {
	m := newMig(t)
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = m.ops, m.data, NewJobs() })
	_ = s
	a := Actor{User: "admin", Via: "tunnel"}
	if _, err := c.CreateSource(ctx, "x", "-oProxyCommand=id", 22, "root", a); err == nil || len(err.(*Error).Problems) == 0 {
		t.Errorf("bad source: %v", err)
	}
	list, err := c.Sources(ctx)
	if err != nil || len(list) != 1 || !strings.HasPrefix(list[0].Authorized, "restrict") || !strings.Contains(list[0].Install, "authorized_keys") {
		t.Fatalf("sources = %+v %v", list, err)
	}
	if _, err := c.SetMigration(ctx, "magaza", MigrationChoice{Source: m.id, DB: "eski", FilesDir: "/home/eski/uploads"}, a); err != nil {
		t.Fatal(err)
	}
	v, err := c.Migration(ctx, "magaza")
	if err != nil || v.Migration == nil || v.Source == nil || v.Source.ID != m.id || len(v.Source.UsedBy) != 1 {
		t.Fatalf("view = %+v %v", v, err)
	}
	if _, err := c.MigrationJob(ctx, "magaza", "copy", "yanlis", a); err == nil || err.(*Error).Status != 422 {
		t.Errorf("copy without confirmation: %v", err)
	}
	id, err := c.MigrationJob(ctx, "magaza", "copy", "magaza", a)
	if err != nil {
		t.Fatal(err)
	}
	var j Job
	waitFor(t, func() bool { j, _ = c.Job(ctx, id); return j.Done })
	if j.Err != "" || j.Cancellable || j.Result == "" {
		t.Fatalf("job = %+v", j)
	}
	if v, _ := c.Migration(ctx, "magaza"); v.Migration.LastCopy() == nil || v.Migration.LastCopy().SiteDocTotal() != 14 {
		t.Errorf("last copy = %+v", v.Migration.LastCopy())
	}
	// Removing the source takes this server's key off it first.
	if err := c.RemoveSource(ctx, m.id, true, a); err != nil {
		t.Fatal(err)
	}
	if last := m.src.commands[len(m.src.commands)-1]; !strings.Contains(last, "authorized_keys") || !strings.Contains(last, m.blob) {
		t.Errorf("last remote command = %s", last)
	}
	if list, _ := c.Sources(ctx); len(list) != 0 {
		t.Error("source not removed")
	}
	entries, _ := s.Audit.Tail(50)
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	sort.Strings(actions)
	for _, want := range []string{"migrate.copy", "migrate.set", "source.remove"} {
		if !contains(actions, want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}
}
