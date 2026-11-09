package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

// y3Node is an entry of a files snapshot, relative to the site's data directory.
type y3Node struct {
	Path    string
	Dir     bool
	Link    string
	Mode    os.FileMode
	Content string
}

// y3Snap is a snapshot of the fake repository.
type y3Snap struct {
	ID    string
	Time  time.Time
	Path  string
	Tags  []string
	Files int64
	Bytes int64
	Blob  []byte            // a database dump
	Tree  []y3Node          // a files part
	Def   map[string]string // a definition part: name → content
}

// y3Restic plays a repository for the restore side: snapshots, dump, restore and ls
// answer from what it holds.
type y3Restic struct {
	mu    sync.Mutex
	snaps []y3Snap
	calls [][]string
	cmds  []run.Cmd
	// short makes a restore leave one file out; unsure makes its summary say one item
	// was not restored.
	short, unsure bool
	// during runs inside a command (after its arguments are recorded).
	during func(ra []string, c run.Cmd)
	// catCode is the exit status of `cat config` (0: the repository opens).
	catCode int
}

func (r *y3Restic) find(id string) (y3Snap, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.snaps {
		if s.ID == id {
			return s, true
		}
	}
	return y3Snap{}, false
}

func (r *y3Restic) ran(cmd string) [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out [][]string
	for _, c := range r.calls {
		if c[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func (r *y3Restic) answer(ra []string, c run.Cmd) error {
	r.mu.Lock()
	r.calls = append(r.calls, ra)
	r.cmds = append(r.cmds, c)
	during := r.during
	r.mu.Unlock()
	if during != nil {
		during(ra, c)
	}
	if strings.HasPrefix(ra[0], "--") { // global flags first (a setup's look at the config)
		for i, a := range ra {
			if !strings.HasPrefix(a, "--") {
				ra = append([]string{a}, append(append([]string{}, ra[:i]...), ra[i+1:]...)...)
				break
			}
		}
	}
	args := []string{}
	for _, a := range ra[1:] {
		if !strings.HasPrefix(a, "--") {
			args = append(args, a)
		}
	}
	switch ra[0] {
	case "snapshots":
		var list []map[string]any
		r.mu.Lock()
		for _, s := range r.snaps {
			list = append(list, map[string]any{"id": s.ID, "time": s.Time, "paths": []string{s.Path}, "hostname": "T1", "tags": s.Tags,
				"summary": map[string]int64{"total_files_processed": s.Files, "total_bytes_processed": s.Bytes}})
		}
		r.mu.Unlock()
		b, _ := json.Marshal(list)
		if list == nil {
			b = []byte("[]")
		}
		_, err := c.Stdout.Write(append(b, '\n'))
		return err
	case "dump":
		spec, file := args[0], args[1]
		id, sub, _ := strings.Cut(spec, ":")
		s, ok := r.find(id)
		if !ok {
			return exitErr{1}
		}
		switch {
		case sub != "" && file == "/":
			return writeTree(c.Stdout, s.Tree)
		case file == s.Path && s.Blob != nil:
			_, err := c.Stdout.Write(s.Blob)
			return err
		case strings.HasPrefix(file, s.Path+"/"):
			rel := strings.TrimPrefix(file, s.Path+"/")
			if v, ok := s.Def[rel]; ok {
				_, err := io.WriteString(c.Stdout, v)
				return err
			}
			for _, n := range s.Tree {
				if n.Path == rel && !n.Dir && n.Link == "" {
					_, err := io.WriteString(c.Stdout, n.Content)
					return err
				}
			}
		}
		c.Stderr("Fatal: cannot dump file: path not found in snapshot")
		return exitErr{1}
	case "restore":
		id, _, _ := strings.Cut(args[0], ":")
		s, ok := r.find(id)
		if !ok {
			return exitErr{1}
		}
		dst := mountSource(c.Args, "/restore")
		if dst == "" {
			return errors.New("no target mounted")
		}
		n, skipped := 0, false
		for _, node := range s.Tree {
			p := filepath.Join(dst, filepath.FromSlash(node.Path))
			var err error
			switch {
			case node.Dir:
				err = os.MkdirAll(p, 0o755)
				if err == nil {
					err = os.Chmod(p, node.Mode)
				}
			case node.Link != "":
				err = os.Symlink(node.Link, p)
			case r.short && !skipped:
				skipped = true
				continue
			default:
				err = os.WriteFile(p, []byte(node.Content), node.Mode)
				if err == nil {
					err = os.Chmod(p, node.Mode)
				}
			}
			if err != nil {
				return err
			}
			n++
		}
		fmt.Fprintf(c.Stdout, `{"message_type":"status","total_bytes":%d,"bytes_restored":%d}`+"\n", s.Bytes, s.Bytes)
		done := n
		if r.unsure {
			done--
		}
		fmt.Fprintf(c.Stdout, `{"message_type":"summary","total_files":%d,"files_restored":%d,"total_bytes":%d,"bytes_restored":%d}`+"\n", n, done, s.Bytes, s.Bytes)
		return nil
	case "ls":
		s, ok := r.find(args[0])
		if !ok {
			return exitErr{1}
		}
		want := args[1]
		fmt.Fprintf(c.Stdout, `{"message_type":"snapshot","struct_type":"snapshot","id":"%s"}`+"\n", s.ID)
		node := func(p, kind string, size int64) {
			b, _ := json.Marshal(map[string]any{"struct_type": "node", "name": path.Base(p), "type": kind, "path": p, "size": size,
				"permissions": "-rw-r--r--", "mtime": s.Time, "uid": 1000})
			c.Stdout.Write(append(b, '\n'))
		}
		if want == s.Path {
			node(s.Path, "dir", 0)
		}
		for _, n := range s.Tree {
			full := s.Path + "/" + n.Path
			kind := map[bool]string{true: "dir", false: "file"}[n.Dir]
			if n.Link != "" {
				kind = "symlink"
			}
			if full == want || path.Dir(full) == want || (contains(ra, "--recursive") && strings.HasPrefix(full, want+"/")) {
				node(full, kind, int64(len(n.Content)))
			}
		}
		return nil
	case "cat":
		if r.catCode != 0 {
			return exitErr{r.catCode}
		}
		_, err := c.Stdout.Write([]byte(`{"version":2}`))
		return err
	case "unlock", "forget", "prune", "check", "stats":
		_, err := c.Stdout.Write([]byte("[]\n"))
		return err
	}
	return fmt.Errorf("unexpected restic %v", ra)
}

// writeTree writes a files part as restic dump does: a tar, relative names.
func writeTree(w io.Writer, tree []y3Node) error {
	tw := tar.NewWriter(w)
	for _, n := range tree {
		h := &tar.Header{Name: n.Path, Mode: int64(n.Mode), ModTime: time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC), Uid: 1000, Gid: 1000}
		switch {
		case n.Dir:
			h.Typeflag, h.Name = tar.TypeDir, n.Path+"/"
		case n.Link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, n.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(n.Content))
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.WriteString(tw, n.Content); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// y3Stream sends restic's tool containers to the fake repository, everything else to
// the fake MongoDB (its tools and the file helpers).
type y3Stream struct {
	restic *y3Restic
	mongo  *fakeMongo
}

func (s *y3Stream) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	u := run.Usage{PeakMem: 60 << 20, CPU: 200 * time.Millisecond, Wall: time.Second}
	if err := ctx.Err(); err != nil {
		return u, err
	}
	if i := indexOf(c.Args, "cat"); i > 0 && c.Args[i-1] == "--entrypoint" {
		_, err := c.Stdout.Write(fakeBinary)
		return u, err
	}
	if i := indexOf(c.Args, "restic"); i >= 0 && c.Name == "docker" {
		return u, s.restic.answer(c.Args[i+1:], c)
	}
	return s.mongo.Stream(ctx, c)
}

type y3Env struct {
	*f2Env
	b      *Backups
	restic *y3Restic
	now    time.Time
	audit  *Audit
}

const y3Repo = "s3:https://acc123.r2.cloudflarestorage.com/footops-t1"

func snapID(n int) string { return fmt.Sprintf("%064x", n) }

// newY3 is a site with a database, a data directory and two backups in the fake
// repository: RUN1 (the night: database, files, definition) and RUN2 (an hour later,
// the database only), besides snapshots of another site, of the server and one FootOps
// never took.
func newY3(t *testing.T) *y3Env {
	t.Helper()
	f := newF2(t)
	sum := sha256.Sum256(fakeBinary)
	old := resticSHA256[runtime.GOARCH]
	resticSHA256[runtime.GOARCH] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { resticSHA256[runtime.GOARCH] = old })
	rs := &y3Restic{}
	f.data.Stream = &y3Stream{restic: rs, mongo: f.mongo}
	e := &y3Env{f2Env: f, restic: rs, now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
	e.audit = NewAudit(filepath.Join(f.root, "audit.jsonl"))
	e.b = &Backups{Data: f.data, Jobs: NewJobs(), Audit: e.audit, Dir: filepath.Join(f.root, "backup"),
		SecretPath: filepath.Join(f.root, "etc/footops/secrets/backup.json"), ToolsDir: filepath.Join(f.root, "tools"),
		CacheDir: filepath.Join(f.root, "cache"), Server: "T1", now: func() time.Time { return e.now }, loc: time.UTC, trialEvery: -1}
	t.Cleanup(func() {
		e.b.viewMu.Lock()
		if e.b.viewTimer != nil {
			e.b.viewTimer.Stop()
		}
		e.b.viewMu.Unlock()
	})
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: y3Repo, KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"}); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Confirm(); err != nil {
		t.Fatal(err)
	}
	// What the site holds now: the data directory has one file.
	os.WriteFile(filepath.Join(f.root, "srv/musteri/magaza/data/simdi.txt"), []byte("şimdiki"), 0o644)

	night := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	dump := fakeArchive(map[string]map[string]int64{"magaza": {"urunler": 5, "sayfalar": 1, "yeni": 4}}, map[string][]string{"magaza": {"ucuz"}})
	tree := []y3Node{{Path: "alt", Dir: true, Mode: 0o750}, {Path: "alt/b.txt", Mode: 0o600, Content: "ikinci"},
		{Path: "a.txt", Mode: 0o644, Content: "merhaba"}, {Path: ".gizli", Mode: 0o644, Content: "x"},
		{Path: "bag", Link: "a.txt", Mode: 0o777}}
	var files, size int64
	for _, n := range tree {
		if !n.Dir && n.Link == "" {
			files++
			size += int64(len(n.Content))
		}
	}
	compose := sites.Compose(specFor("magaza"), map[string]string{"API_KEY": "gizli-değer", "COK_SATIR": "bir\niki", "PARA": "100$ \"tırnak\""})
	tags := func(site, part, kind, run string) []string {
		return []string{"footops", "site=" + site, "part=" + part, "kind=" + kind, "run=" + run}
	}
	rs.snaps = []y3Snap{
		{ID: snapID(1), Time: night, Path: "/footops/magaza/db.archive", Tags: tags("magaza", "db", "daily", "RUN1"), Files: 1, Bytes: int64(len(dump)), Blob: dump},
		{ID: snapID(2), Time: night.Add(30 * time.Second), Path: "/footops/magaza/files", Tags: tags("magaza", "files", "daily", "RUN1"), Files: files, Bytes: size, Tree: tree},
		{ID: snapID(3), Time: night.Add(40 * time.Second), Path: "/footops/magaza/def", Tags: tags("magaza", "def", "daily", "RUN1"), Files: 2, Bytes: int64(len(compose)),
			Def: map[string]string{"compose.yaml": compose, "footops.json": "{}"}},
		{ID: snapID(4), Time: night.Add(time.Hour), Path: "/footops/magaza/db.archive", Tags: tags("magaza", "db", "hourly", "RUN2"), Files: 1, Bytes: int64(len(dump)), Blob: dump},
		{ID: snapID(5), Time: night, Path: "/footops/baska/db.archive", Tags: tags("baska", "db", "daily", "RUN1"), Files: 1, Bytes: 10},
		{ID: snapID(6), Time: night, Path: "/footops/_server", Tags: tags("_server", "server", "daily", "RUN1"), Files: 9, Bytes: 100},
		{ID: snapID(7), Time: night, Path: "/home/elle", Tags: []string{"elle"}, Files: 1, Bytes: 1},
		{ID: snapID(8), Time: night, Path: "/footops/magaza/db.archive", Tags: []string{"footops", "site=../x", "part=db", "run=RUN9"}, Files: 1, Bytes: 1},
	}
	// The night's record, as the run kept it.
	e.b.saveRun(BackupRun{ID: "RUN1", Kind: RunDaily, Started: night, Finished: night.Add(time.Minute), Parts: []BackupPart{
		{Site: "magaza", Part: PartDB, Snapshot: snapID(1), Collections: map[string]int64{"urunler": 5, "sayfalar": 1, "yeni": 4, "system.js": 1}, Documents: 11},
		{Site: "magaza", Part: PartFiles, Snapshot: snapID(2), Files: files, Bytes: size},
		{Site: "baska", Part: PartDB, Snapshot: snapID(5)},
	}})
	return e
}

func (e *y3Env) list(t *testing.T) {
	t.Helper()
	if err := e.b.ListBackups(context.Background(), e.task()); err != nil {
		t.Fatal(err)
	}
}

func (e *y3Env) open(t *testing.T, run string) (BackupView, *Task) {
	t.Helper()
	tk := e.task()
	v, err := e.b.OpenBackup(context.Background(), tk, "magaza", run, Actor{User: "furkan"})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	return v, tk
}

func TestBackupIndex(t *testing.T) {
	e := newY3(t)
	l, err := e.b.SiteBackups("magaza")
	if err != nil || len(l.Points) != 0 || !l.Updated.IsZero() {
		t.Fatalf("before listing: %+v %v", l, err)
	}
	e.list(t)
	if c := e.restic.ran("snapshots"); len(c) != 1 || strings.Join(c[0], " ") != "snapshots --json --no-lock --host T1" {
		t.Errorf("listing: %v", c)
	}
	l, err = e.b.SiteBackups("magaza")
	if err != nil || len(l.Points) != 2 || l.Updated.IsZero() {
		t.Fatalf("points: %+v %v", l, err)
	}
	newest, night := l.Points[0], l.Points[1]
	if newest.Run != "RUN2" || newest.Kind != RunHourly || len(newest.Parts) != 1 || newest.Parts[PartDB].ID != snapID(4) {
		t.Errorf("newest %+v", newest)
	}
	if night.Run != "RUN1" || len(night.Parts) != 3 || !night.Time.Equal(time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)) ||
		night.Parts[PartFiles].Files != 3 || night.Parts[PartFiles].Bytes != 14 {
		t.Errorf("night %+v", night)
	}
	// The record's counts come with one backup; another site's parts stay out.
	d, err := e.b.SiteBackup("magaza", "RUN1")
	if err != nil || len(d.Record) != 2 || d.Repository != "footops-t1" {
		t.Fatalf("detail %+v %v", d, err)
	}
	if rec, ok := d.recordPart(PartDB); !ok || rec.Documents != 11 {
		t.Errorf("record %+v", rec)
	}
	if _, err := e.b.SiteBackup("magaza", "RUN9"); !errors.Is(err, errBackupGone) {
		t.Errorf("a snapshot with a bad site tag counts: %v", err)
	}
	if l, _ := e.b.SiteBackups("baska"); len(l.Points) != 1 {
		t.Errorf("baska %+v", l)
	}
	// Another repository's index is no index.
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: y3Repo + "/baska", KeyID: "KEYID12345",
		SecretKey: "SeCrEt/Key+123=", Password: "RepoPassword0123456789abcdefghijklmnop"}); err != nil {
		t.Fatal(err)
	}
	if l, err := e.b.SiteBackups("magaza"); err != nil || len(l.Points) != 0 {
		t.Errorf("index of another repository used: %+v %v", l, err)
	}
}

func TestRunRefreshesIndex(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	p, _ := e.b.Plan(RunManual, "magaza", e.now)
	tk := e.task("job1")
	if _, err := e.b.Run(context.Background(), tk, p, Actor{}); err != nil {
		t.Fatal(err)
	}
	c := e.restic.ran("snapshots")
	if len(c) != 1 || indexOf(c[0], "--no-lock") < 0 || !strings.Contains(steps(tk), "Yedek listesi yenilendi") {
		t.Errorf("index not refreshed after a run: %v\n%s", c, steps(tk))
	}
	if _, err := os.Stat(e.b.indexPath()); err != nil {
		t.Error(err)
	}
}

func TestOpenBackup(t *testing.T) {
	e := newY3(t)
	e.list(t)
	var mongoCfg string
	e.restic.during = func(ra []string, c run.Cmd) {
		if ra[0] == "dump" {
			// The repository's keys sit apart from the directory the MongoDB tools see.
			if src := mountSource(c.Args, "/run/footops"); !strings.Contains(src, "/staging/rst-") {
				t.Errorf("restic credentials in %s", src)
			}
		}
	}
	e.mongo.onRestore = func(int) {
		ents, _ := os.ReadDir(filepath.Join(e.root, "staging"))
		for _, en := range ents {
			if b, err := os.ReadFile(filepath.Join(e.root, "staging", en.Name(), "mongo.yaml")); err == nil {
				mongoCfg = string(b)
				if _, err := os.Stat(filepath.Join(e.root, "staging", en.Name(), "cfg")); err == nil {
					t.Error("the MongoDB tool's directory holds the repository's keys")
				}
			}
		}
	}
	v, tk := e.open(t, "RUN1")
	if !v.Ready || v.DB != "magaza" || v.Documents != 10 || v.Collections["yeni"] != 4 || len(v.Views) != 1 || v.Expires.Sub(e.now) != viewKeep {
		t.Fatalf("view %+v\n%s", v, steps(tk))
	}
	if !strings.Contains(mongoCfg, viewDBPrefix) {
		t.Errorf("restored with %q", mongoCfg)
	}
	tmp := viewDBPrefix + strings.ToLower(v.ID)
	if got := e.mongo.get(tmp); got["yeni"] != 4 || got["urunler"] != 5 {
		t.Errorf("temporary database %v", got)
	}
	if got := e.mongo.get("magaza"); got["eski"] != 7 {
		t.Errorf("the site's database was touched: %v", got)
	}
	if !strings.Contains(steps(tk), "Sayımlar yedek alınırken sayılanla aynı") {
		t.Errorf("record not compared:\n%s", steps(tk))
	}
	if c := e.restic.ran("dump"); len(c) != 1 || strings.Join(c[0], " ") != "dump --no-lock "+snapID(1)+" /footops/magaza/db.archive" {
		t.Errorf("dump %v", c)
	}
	rec, err := e.b.loadView(v.ID)
	if err != nil || rec.Password == "" || rec.SHA256 == "" {
		t.Fatalf("record %+v %v", rec, err)
	}
	if st, err := os.Stat(e.b.viewPath(v.ID)); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("record file %v", err)
	}
	if _, err := os.Stat(e.b.viewDump(v.ID)); err != nil {
		t.Error("dump not kept for the restore")
	}
	ents, _ := os.ReadDir(filepath.Join(e.root, "staging"))
	for _, en := range ents {
		if strings.HasPrefix(en.Name(), resticPrefix) || en.Name() == tk.ID() {
			t.Errorf("left in staging: %s", en.Name())
		}
	}
	// The panel sees the view, never its password.
	l, _ := e.b.SiteBackups("magaza")
	if len(l.Views) != 1 || l.Views[0].ID != v.ID {
		t.Errorf("views %+v", l.Views)
	}
	if b, _ := json.Marshal(l); strings.Contains(string(b), rec.Password) {
		t.Error("password in the list")
	}
	_, ctx, err := e.b.ViewContext(context.Background(), "magaza", v.ID)
	if c, ok := dbCredsFrom(ctx); err != nil || !ok || c.DB != tmp || c.User != tmp || c.Password != rec.Password {
		t.Errorf("view context %+v %v", c, err)
	}
	if _, _, err := e.b.ViewContext(context.Background(), "baska", v.ID); !errors.Is(err, errViewGone) {
		t.Errorf("another site's view: %v", err)
	}

	// Opening again closes the earlier view of the site.
	v2, _ := e.open(t, "RUN2")
	if _, err := e.b.loadView(v.ID); err == nil || e.mongo.get(tmp) != nil && len(e.mongo.get(tmp)) > 0 {
		t.Errorf("earlier view still there")
	}
	if err := e.b.CloseView("magaza", v2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.b.viewDump(v2.ID)); !os.IsNotExist(err) {
		t.Error("dump left after closing")
	}
	if len(e.mongo.get(viewDBPrefix+strings.ToLower(v2.ID))) != 0 {
		t.Error("temporary database left after closing")
	}
}

func TestOpenBackupFailures(t *testing.T) {
	t.Run("restore fails", func(t *testing.T) {
		e := newY3(t)
		e.list(t)
		e.mongo.failRestore = 1
		tk := e.task()
		if _, err := e.b.OpenBackup(context.Background(), tk, "magaza", "RUN1", Actor{}); err == nil {
			t.Fatal("no error")
		}
		if len(e.b.viewRecords()) != 0 {
			t.Error("record left")
		}
		if _, err := os.Stat(e.b.viewDump(tk.ID())); !os.IsNotExist(err) {
			t.Error("dump left")
		}
	})
	t.Run("expired and leftovers", func(t *testing.T) {
		e := newY3(t)
		e.list(t)
		v, _ := e.open(t, "RUN1")
		// An opening cut short by a restart: its job no longer runs.
		left := viewRecord{BackupView: BackupView{ID: "Cut1", Site: "magaza", Run: "RUN1"}, TempDB: viewDBPrefix + "cut1", Password: "x"}
		if err := e.b.saveView(left); err != nil {
			t.Fatal(err)
		}
		e.mongo.set(viewDBPrefix+"cut1", map[string]int64{"a": 1})
		if got := e.b.SweepViews(); len(got) != 1 {
			t.Errorf("swept %v", got)
		}
		if _, err := e.b.loadView("Cut1"); err == nil || len(e.mongo.get(viewDBPrefix+"cut1")) != 0 {
			t.Error("leftover kept")
		}
		e.now = e.now.Add(viewKeep + time.Minute)
		if _, _, err := e.b.ViewContext(context.Background(), "magaza", v.ID); !errors.Is(err, errViewGone) {
			t.Errorf("expired view browsable: %v", err)
		}
		e.b.SweepViews()
		if len(e.b.viewRecords()) != 0 {
			t.Error("expired view kept")
		}
	})
	t.Run("too many", func(t *testing.T) {
		e := newY3(t)
		e.list(t)
		for i := 0; i < viewsMax; i++ {
			e.b.saveView(viewRecord{BackupView: BackupView{ID: fmt.Sprintf("Other%d", i), Site: fmt.Sprintf("site%d", i), Ready: true,
				Expires: e.now.Add(time.Hour)}, TempDB: viewDBPrefix + fmt.Sprintf("other%d", i), Password: "x"})
		}
		if _, err := e.b.OpenBackup(context.Background(), e.task(), "magaza", "RUN1", Actor{}); err == nil || !strings.Contains(err.Error(), "en çok") {
			t.Errorf("limit: %v", err)
		}
	})
	t.Run("no database part", func(t *testing.T) {
		e := newY3(t)
		e.restic.snaps = e.restic.snaps[1:] // RUN1 without its database
		e.list(t)
		if _, err := e.b.OpenBackup(context.Background(), e.task(), "magaza", "RUN1", Actor{}); err == nil {
			t.Error("opened a backup without a database")
		}
	})
}

func TestRestoreDatabase(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	tk := e.task()
	res, err := e.b.Restore(context.Background(), tk, "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID}, Actor{User: "furkan"})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	got := e.mongo.get("magaza")
	if got["yeni"] != 4 || got["urunler"] != 5 || got["sayfalar"] != 1 {
		t.Errorf("site database %v", got)
	}
	if _, ok := got["eski"]; ok {
		t.Error("a collection the backup does not have was kept")
	}
	// The safety copy (rule K1) holds what was there, and the result names it.
	snap, _, err := e.data.SnapshotFile("magaza", res.Snapshot)
	if err != nil || snap.Reason != "before-restore" || snap.Collections["eski"] != 7 {
		t.Errorf("safety copy %+v %v", snap, err)
	}
	if tk.job.Result != res.Snapshot || res.FilesOld != "" {
		t.Errorf("result %q %+v", tk.job.Result, res)
	}
	if _, err := e.b.loadView(v.ID); err == nil {
		t.Error("view not closed after the restore")
	}
	for _, want := range []string{"Site durduruldu", "Yüklendi ve sayıldı", "Site başlatıldı", "Açılan yedek kapatıldı", "Veritabanı kopyaları → " + copyLabel(res.Snapshot)} {
		if !strings.Contains(steps(tk), want) {
			t.Errorf("steps lack %q:\n%s", want, steps(tk))
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "staging", tk.ID())); !os.IsNotExist(err) {
		t.Error("job staging left")
	}
}

func TestRestoreDatabaseRefused(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	ctx := context.Background()
	if _, err := e.b.Restore(ctx, e.task(), "magaza", BackupRestore{Run: "RUN2", DB: true, View: v.ID}, Actor{}); !errors.Is(err, errViewGone) {
		t.Errorf("view of another backup: %v", err)
	}
	if _, err := e.b.Restore(ctx, e.task(), "magaza", BackupRestore{Run: "RUN1", DB: true, View: "Yok1"}, Actor{}); !errors.Is(err, errViewGone) {
		t.Errorf("no view: %v", err)
	}
	// Counts that do not add up: rolled back, the site as it was.
	e.mongo.lieNext = true
	tk := e.task()
	_, err := e.b.Restore(ctx, tk, "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID}, Actor{})
	if err == nil || !strings.Contains(err.Error(), "geri alındı") {
		t.Fatalf("lying restore: %v\n%s", err, steps(tk))
	}
	if got := e.mongo.get("magaza"); got["eski"] != 7 || got["urunler"] != 3 {
		t.Errorf("not rolled back: %v", got)
	}
	if _, err := e.b.loadView(v.ID); err != nil {
		t.Error("a failed restore closed the view (it may be tried again)")
	}
	// Expired: refused before anything happens.
	e.now = e.now.Add(2 * viewKeep)
	if _, err := e.b.Restore(ctx, e.task(), "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID}, Actor{}); err == nil || !strings.Contains(err.Error(), "süresi doldu") {
		t.Errorf("expired view: %v", err)
	}
}

func TestRestoreFiles(t *testing.T) {
	e := newY3(t)
	e.list(t)
	data := filepath.Join(e.root, "srv/musteri/magaza/data")
	tk := e.task()
	res, err := e.b.Restore(context.Background(), tk, "magaza", BackupRestore{Run: "RUN1", Files: true}, Actor{User: "furkan"})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	if got := readFile(t, filepath.Join(data, "alt/b.txt")); got != "ikinci" {
		t.Errorf("b.txt %q", got)
	}
	if got := readFile(t, filepath.Join(data, ".gizli")); got != "x" {
		t.Errorf("hidden file %q", got)
	}
	if st, err := os.Stat(filepath.Join(data, "alt/b.txt")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode of b.txt: %v", st.Mode())
	}
	if l, err := os.Readlink(filepath.Join(data, "bag")); err != nil || l != "a.txt" {
		t.Errorf("link %q %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(data, "simdi.txt")); !os.IsNotExist(err) {
		t.Error("the current file stayed (replace)")
	}
	old := filepath.Join(e.root, "srv/musteri/magaza", dataOldPrefix+res.FilesOld)
	if got := readFile(t, filepath.Join(old, "simdi.txt")); got != "şimdiki" {
		t.Errorf("previous directory %q", got)
	}
	c := e.restic.ran("restore")
	if len(c) != 1 || strings.Join(c[0], " ") != "restore --json --no-lock "+snapID(2)+":/footops/magaza/files --target /restore" {
		t.Errorf("restore %v", c)
	}
	args := strings.Join(e.restic.cmds[len(e.restic.cmds)-1].Args, " ")
	for _, want := range []string{"--cap-add CHOWN", "--cap-add FOWNER", "--cap-add DAC_OVERRIDE",
		"type=bind,source=" + filepath.Join(e.root, "srv/musteri/magaza/data.new") + ",target=/restore --"} {
		if !strings.Contains(args, want) {
			t.Errorf("restore container lacks %q: %s", want, args)
		}
	}
	if strings.Contains(args, "DAC_READ_SEARCH") || strings.Contains(args, "SYS_ADMIN") {
		t.Errorf("more capabilities than needed: %s", args)
	}
	if !strings.Contains(steps(tk), "Dosya kopyaları → "+copyLabel(res.FilesOld)) {
		t.Errorf("no undo hint:\n%s", steps(tk))
	}
}

func TestRestoreFilesShort(t *testing.T) {
	e := newY3(t)
	e.list(t)
	e.restic.short = true
	data := filepath.Join(e.root, "srv/musteri/magaza/data")
	tk := e.task()
	_, err := e.b.Restore(context.Background(), tk, "magaza", BackupRestore{Run: "RUN1", Files: true}, Actor{})
	if err == nil || !strings.Contains(err.Error(), "yedektekinden farklı") {
		t.Fatalf("short restore: %v\n%s", err, steps(tk))
	}
	if got := readFile(t, filepath.Join(data, "simdi.txt")); got != "şimdiki" {
		t.Error("data directory changed")
	}
	if _, err := os.Stat(filepath.Join(e.root, "srv/musteri/magaza", dataNew)); !os.IsNotExist(err) {
		t.Error("data.new left")
	}
	if contains(e.sites.calls, "stop") {
		t.Errorf("the site was stopped for a restore that could not be made: %v", e.sites.calls)
	}
}

func TestRestoreBoth(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	tk := e.task()
	res, err := e.b.Restore(context.Background(), tk, "magaza", BackupRestore{Run: "RUN1", DB: true, Files: true, View: v.ID}, Actor{})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	if res.Snapshot == "" || res.FilesOld == "" || tk.job.Result != res.Snapshot+" files-"+res.FilesOld {
		t.Errorf("result %+v %q", res, tk.job.Result)
	}
	if strings.Join(e.sites.calls, " ") != "health1 stop start health15" {
		t.Errorf("site calls %v, want one stop and start", e.sites.calls)
	}
	if got := e.mongo.get("magaza"); got["yeni"] != 4 {
		t.Errorf("db %v", got)
	}
}

func TestExportBackup(t *testing.T) {
	e := newY3(t)
	e.list(t)
	tk := e.task()
	info, err := e.b.ExportBackup(context.Background(), tk, "magaza", BackupExport{Run: "RUN1", DB: true, Files: true, Env: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	if info.File != "magaza-"+time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC).Local().Format("20060102-150405")+".footops.tar" || !info.Env || info.Backup.IsZero() || info.Files != 3 || info.Documents != 10 {
		t.Errorf("info %+v", info)
	}
	if !strings.Contains(steps(tk), "1 öğe alınmadı") {
		t.Errorf("the link was not left out:\n%s", steps(tk))
	}
	_, path, err := e.data.ExportFile("magaza", info.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// It imports like any FootOps archive: the env is noted, never applied.
	var ic ImportCheck
	if _, err := inspectFootOps(context.Background(), f, &ic, nil); err != nil {
		t.Fatalf("import check: %v", err)
	}
	if !ic.HasDB || ic.Files == nil || ic.Files.Archive.Files != 3 || ic.Manifest.Backup == nil || ic.Manifest.Backup.Run != "RUN1" ||
		ic.Manifest.Backup.Snapshots[PartDB] != snapID(1) || ic.Manifest.Collections["yeni"] != 4 {
		t.Errorf("check %+v manifest %+v", ic, ic.Manifest)
	}
	if len(ic.Notes) != 1 || !strings.Contains(ic.Notes[0], "3 değişken") {
		t.Errorf("notes %v", ic.Notes)
	}
	// The env reads back to the values of the time.
	f.Seek(0, io.SeekStart)
	tr := tar.NewReader(f)
	var envText string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == archive.MemberEnv {
			b, _ := io.ReadAll(tr)
			envText = string(b)
		}
	}
	entries, err := sites.ParseDotenv(envText)
	if err != nil {
		t.Fatal(err)
	}
	m := sites.EnvMap(entries)
	if m["API_KEY"] != "gizli-değer" || m["COK_SATIR"] != "bir\niki" || m["PARA"] != "100$ \"tırnak\"" || !strings.HasPrefix(envText, "# magaza ortam") {
		t.Errorf("env %q", envText)
	}
	for _, c := range e.restic.ran("dump") {
		if indexOf(c, "--no-lock") < 0 {
			t.Errorf("dump takes a lock: %v", c)
		}
	}
	if len(e.restic.ran("dump")) != 3 {
		t.Errorf("dumps %v", e.restic.ran("dump"))
	}
}

func TestExportBackupChoices(t *testing.T) {
	e := newY3(t)
	e.list(t)
	ctx := context.Background()
	if _, err := e.b.ExportBackup(ctx, e.task(), "magaza", BackupExport{Run: "RUN1", Env: true}); err == nil {
		t.Error("an archive of the env alone")
	}
	if _, err := e.b.ExportBackup(ctx, e.task(), "magaza", BackupExport{Run: "RUN2", Files: true}); err == nil {
		t.Error("files of a backup without them")
	}
	tk := e.task()
	info, err := e.b.ExportBackup(ctx, tk, "magaza", BackupExport{Run: "RUN2", DB: true})
	if err != nil || info.Env || info.Files != 0 {
		t.Fatalf("%+v %v\n%s", info, err, steps(tk))
	}
	// A restic failure leaves nothing behind.
	e.restic.snaps[3].Blob = nil
	tk = e.task()
	if _, err := e.b.ExportBackup(ctx, tk, "magaza", BackupExport{Run: "RUN2", DB: true}); err == nil {
		t.Fatal("export of a missing dump")
	}
	if _, err := os.Stat(filepath.Join(e.root, "staging", exportPrefix+tk.ID())); !os.IsNotExist(err) {
		t.Error("partial archive left")
	}
}

func TestBackupFiles(t *testing.T) {
	e := newY3(t)
	e.list(t)
	ctx := context.Background()
	top, err := e.b.BackupFiles(ctx, "magaza", "RUN1", "")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range top.Entries {
		names = append(names, en.Type+":"+en.Name)
	}
	if strings.Join(names, " ") != "dir:alt file:.gizli file:a.txt symlink:bag" {
		t.Errorf("top %v", names)
	}
	sub, err := e.b.BackupFiles(ctx, "magaza", "RUN1", "alt/")
	if err != nil || len(sub.Entries) != 1 || sub.Entries[0].Name != "b.txt" || sub.Path != "alt" {
		t.Errorf("alt %+v %v", sub, err)
	}
	for _, bad := range []string{"../x", "alt/../..", "a\x00b"} {
		if _, err := e.b.BackupFiles(ctx, "magaza", "RUN1", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := e.b.BackupFiles(ctx, "magaza", "RUN1", "yok"); !errors.Is(err, errNoBackupPath) {
		t.Errorf("missing: %v", err)
	}
	if _, err := e.b.BackupFiles(ctx, "magaza", "RUN2", ""); err == nil {
		t.Error("files of a backup without them")
	}
	var buf bytes.Buffer
	var got BackupFile
	if err := e.b.StreamBackupFile(ctx, "magaza", "RUN1", "alt/b.txt", func(f BackupFile) io.Writer { got = f; return &buf }); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "ikinci" || got.Size != 6 || got.Name != "b.txt" {
		t.Errorf("file %q %+v", buf.String(), got)
	}
	if err := e.b.StreamBackupFile(ctx, "magaza", "RUN1", "alt", func(BackupFile) io.Writer { return &buf }); !errors.Is(err, errNoBackupPath) {
		t.Errorf("a folder streamed: %v", err)
	}
	// One read at a time.
	e.b.reads <- struct{}{}
	if _, err := e.b.BackupFiles(ctx, "magaza", "RUN1", ""); !errors.Is(err, ErrReadBusy) {
		t.Errorf("second read: %v", err)
	}
	<-e.b.reads
	ents, _ := os.ReadDir(filepath.Join(e.root, "staging"))
	for _, en := range ents {
		if strings.HasPrefix(en.Name(), resticPrefix) {
			t.Errorf("credentials left: %s", en.Name())
		}
	}
}

func TestStartViewsRemovesCredentials(t *testing.T) {
	e := newY3(t)
	dir, _ := e.data.Staging.Dir(resticPrefix + "Gone1")
	os.WriteFile(filepath.Join(dir, "password"), []byte("x"), 0o600)
	keep, _ := e.data.Staging.Dir(exportPrefix + "Keep1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // no sweep later
	e.b.StartViews(ctx)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("credentials of a cut-off job kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("an export was removed")
	}
}

func TestSurvey(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".gizli/ic"), 0o755)
	os.WriteFile(filepath.Join(dir, ".gizli/ic/a"), []byte("12345"), 0o600)
	os.WriteFile(filepath.Join(dir, "b"), []byte("123"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "bag"))
	sv, err := survey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sv.Files != 2 || sv.Bytes != 8 || sv.Dirs != 2 || sv.Skipped != 1 || len(sv.Owners) != 1 {
		t.Errorf("survey %+v", sv)
	}
	if st, _ := countFiles(dir); st.Files != 1 {
		t.Errorf("count leaves hidden out: %+v", st)
	}
}

func TestCountsDiffer(t *testing.T) {
	rec := map[string]int64{"a": 2, "system.js": 1, "v": 0}
	if d := countsDiffer(rec, map[string]int64{"a": 2}, []string{"v"}); d != "" {
		t.Errorf("same counts differ: %s", d)
	}
	if d := countsDiffer(rec, map[string]int64{"a": 1, "b": 3}, []string{"v"}); !strings.Contains(d, "a: kayıtta 2, yüklenen 1") || !strings.Contains(d, "b: kayıtta yok") {
		t.Errorf("diff %s", d)
	}
}

func TestSnapshotOf(t *testing.T) {
	at := time.Date(2026, 11, 6, 1, 0, 0, 0, time.FixedZone("TSİ", 3*3600))
	s, ok := snapshotOf(snapID(1), at, []string{"/footops/magaza/files"}, []string{"footops", "site=magaza", "part=files", "kind=daily", "run=Ab12"})
	if !ok || s.Site != "magaza" || s.Part != PartFiles || s.Run != "Ab12" || s.Time.Location() != time.UTC {
		t.Errorf("%+v %v", s, ok)
	}
	for _, c := range []struct {
		id, path string
		tags     []string
	}{
		{snapID(1), "/footops/magaza/files", []string{"site=magaza", "part=files"}},                       // not ours
		{"abc", "/footops/magaza/files", []string{"footops", "site=magaza", "part=files"}},                // id
		{snapID(1), "/footops/magaza/db.archive", []string{"footops", "site=magaza", "part=files"}},       // path and part disagree
		{snapID(1), "/footops/Magaza/files", []string{"footops", "site=Magaza", "part=files"}},            // site name
		{snapID(1), "/footops/magaza/files", []string{"footops", "site=magaza", "part=files", "run=a/b"}}, // run
		{snapID(1), "/footops/_server", []string{"footops", "site=_server", "part=db"}},                   // server part
		{snapID(1), "/footops/magaza/x", []string{"footops", "site=magaza", "part=x"}},                    // part
	} {
		if _, ok := snapshotOf(c.id, at, []string{c.path}, c.tags); ok {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestPointsOf(t *testing.T) {
	base := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	list := []BackupSnapshot{
		{ID: "c", Time: base.Add(2 * time.Hour), Site: "m", Part: PartDB, Run: "R3"},
		{ID: "b2", Time: base.Add(time.Minute), Site: "m", Part: PartFiles, Run: "R1"},
		{ID: "b1", Time: base, Site: "m", Part: PartDB, Run: "R1"},
		{ID: "x", Time: base, Site: "n", Part: PartDB, Run: "R1"},
		{ID: "lone", Time: base.Add(time.Hour), Site: "m", Part: PartDB},
	}
	pts := pointsOf(list, "m")
	var got []string
	for _, p := range pts {
		var parts []string
		for k := range p.Parts {
			parts = append(parts, k)
		}
		sort.Strings(parts)
		got = append(got, p.Run+"="+strings.Join(parts, "+")+"@"+p.Time.Format("15:04"))
	}
	if strings.Join(got, " ") != "R3=db@03:00 lone=db@02:00 R1=db+files@01:00" {
		t.Errorf("points %v", got)
	}
}

func TestFormatDotenvRoundTrip(t *testing.T) {
	env := map[string]string{"A": "", "B": `q"uo\te`, "C": "çok\nsatır\r\nve\tsekme", "D": "$HOME #yorum değil", "E": "'tek'"}
	text := sites.FormatDotenv(env, "başlık\nikinci satır")
	entries, err := sites.ParseDotenv(text)
	if err != nil {
		t.Fatal(err)
	}
	got := sites.EnvMap(entries)
	if len(got) != len(env) {
		t.Fatalf("got %v", got)
	}
	for k, v := range env {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}

func TestRepack(t *testing.T) {
	var in bytes.Buffer
	writeTree(&in, []y3Node{{Path: "alt", Dir: true, Mode: 0o750}, {Path: "alt/b.txt", Mode: 0o600, Content: "ikinci"},
		{Path: ".gizli", Mode: 0o644, Content: "x"}, {Path: "bag", Link: "/etc/passwd"}, {Path: "kötü\x01ad", Mode: 0o644, Content: "y"}})
	var out bytes.Buffer
	st, err := datafs.Repack(&in, &out, datafs.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Dirs != 1 || st.Skipped != 2 || st.Bytes != 7 {
		t.Errorf("stats %+v", st)
	}
	s, err := archive.CheckFilesMember(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil || s.Files != 2 || s.Hidden != 1 {
		t.Errorf("an import refuses it: %+v %v", s, err)
	}
}

func TestGoneOr(t *testing.T) {
	for _, msg := range []string{"path footops: id dfb1a73d not found in repository", "Fatal: failed to find snapshot: no matching ID found for prefix \"abc\""} {
		if err := goneOr(errors.New(msg)); !errors.Is(err, errSnapshotGone) || !strings.Contains(err.Error(), "listesini yenile") {
			t.Errorf("%q: %v", msg, err)
		}
	}
	if err := goneOr(errors.New("connection refused")); errors.Is(err, errSnapshotGone) {
		t.Error("an unreachable repository taken for a missing snapshot")
	}
	if goneOr(nil) != nil {
		t.Error("nil")
	}
}

func TestViewTimerWait(t *testing.T) {
	now := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		next  time.Time
		floor time.Duration
		want  time.Duration
	}{
		{now.Add(time.Hour), 0, time.Hour + time.Second},
		{now.Add(-time.Hour), 0, time.Minute}, // past: not every second
		{now.Add(20 * time.Second), 0, time.Minute},
		{now.Add(-time.Minute), 8 * time.Minute, 8 * time.Minute}, // closes failing
	} {
		if got := viewTimerWait(c.next, now, c.floor); got != c.want {
			t.Errorf("wait(%v, %v) = %v, want %v", c.next.Sub(now), c.floor, got, c.want)
		}
	}
}

func TestSweepBacksOffWhileClosesFail(t *testing.T) {
	e := newY3(t)
	e.list(t)
	e.open(t, "RUN1")
	e.now = e.now.Add(2 * viewKeep)
	e.mongo.failDrop = true
	e.b.SweepViews()
	e.b.SweepViews()
	e.b.viewMu.Lock()
	wait := e.b.viewWait
	e.b.viewMu.Unlock()
	if wait != 4*time.Minute {
		t.Errorf("after two failed closes the floor is %v", wait)
	}
	e.mongo.failDrop = false
	if got := e.b.SweepViews(); len(got) != 1 {
		t.Fatalf("closed %v", got)
	}
	e.b.viewMu.Lock()
	wait = e.b.viewWait
	e.b.viewMu.Unlock()
	if wait != 0 {
		t.Errorf("floor not reset: %v", wait)
	}
}

func TestViewInUseAndClosing(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	// Being closed: a restore cannot take it.
	e.b.viewMu.Lock()
	e.b.viewClosing = map[string]bool{v.ID: true}
	e.b.viewMu.Unlock()
	if _, _, err := e.b.useView("magaza", v.ID, "RUN1"); !errors.Is(err, errViewGone) {
		t.Errorf("a view being closed was taken: %v", err)
	}
	e.b.viewMu.Lock()
	e.b.viewClosing = nil
	e.b.viewMu.Unlock()
	// In use: neither a close nor a sweep takes it, even past its time.
	_, release, err := e.b.useView("magaza", v.ID, "RUN1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.b.CloseView("magaza", v.ID); err == nil {
		t.Error("closed while in use")
	}
	e.now = e.now.Add(2 * viewKeep)
	e.b.SweepViews()
	if _, err := e.b.loadView(v.ID); err != nil {
		t.Error("swept while in use")
	}
	release()
	release() // once only
	e.b.viewMu.Lock()
	n := len(e.b.viewUse)
	e.b.viewMu.Unlock()
	if n != 0 {
		t.Errorf("still in use: %d", n)
	}
	e.b.SweepViews()
	if _, err := e.b.loadView(v.ID); err == nil {
		t.Error("not swept once let go")
	}
}

func TestOpeningIsHeldAndApartFromChecks(t *testing.T) {
	e := newY3(t)
	e.list(t)
	swept := false
	e.mongo.onRestore = func(int) {
		// A sweep (the timer, the hourly round) while the backup opens leaves it alone;
		// so does the start's cleanup of a check's temporary databases.
		e.b.SweepViews()
		e.mongo.DropTemps(context.Background())
		swept = true
	}
	v, _ := e.open(t, "RUN1")
	if !swept {
		t.Fatal("hook did not run")
	}
	if !strings.HasPrefix(viewDBPrefix+strings.ToLower(v.ID), viewDBPrefix) || len(e.mongo.get(viewDBPrefix+strings.ToLower(v.ID))) == 0 {
		t.Errorf("temporary database gone: %v", e.mongo.get(viewDBPrefix+strings.ToLower(v.ID)))
	}
	if _, err := e.b.loadView(v.ID); err != nil {
		t.Errorf("record gone: %v", err)
	}
	if temporaryDB("magaza") || !temporaryDB(viewDBPrefix+"x") || !temporaryDB(tmpDBPrefix+"x") {
		t.Error("temporaryDB")
	}
}

func TestRecordDiffNeedsConsent(t *testing.T) {
	e := newY3(t)
	e.list(t)
	// The run counted 6 urunler as it dumped; the backup restores 5.
	rec, _ := e.b.runRecord("RUN1")
	rec.Parts[0].Collections["urunler"] = 6
	e.b.saveRun(rec)
	v, tk := e.open(t, "RUN1")
	if !v.RecordChecked || !strings.Contains(v.RecordDiff, "urunler: kayıtta 6, yüklenen 5") || !strings.Contains(steps(tk), "onaylanırsa") {
		t.Fatalf("view %+v\n%s", v, steps(tk))
	}
	ctx := context.Background()
	if _, err := e.b.Restore(ctx, e.task(), "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID}, Actor{}); err == nil ||
		!strings.Contains(err.Error(), "farkı onayla") {
		t.Errorf("restored without consent: %v", err)
	}
	if got := e.mongo.get("magaza"); got["eski"] != 7 {
		t.Error("the site changed")
	}
	if _, err := e.b.Restore(ctx, e.task(), "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID, AcceptDiff: true}, Actor{}); err != nil {
		t.Errorf("with consent: %v", err)
	}
	// No record to compare with: said so, nothing held back.
	e2 := newY3(t)
	os.RemoveAll(e2.b.runsDir())
	e2.list(t)
	v2, tk2 := e2.open(t, "RUN1")
	if v2.RecordChecked || v2.RecordDiff != "" || !strings.Contains(steps(tk2), "karşılaştırılamadı") {
		t.Errorf("without a record %+v\n%s", v2, steps(tk2))
	}
}

func TestRestoreRefusedBeforeTheSiteStops(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	os.Remove(e.b.viewDump(v.ID))
	_, err := e.b.Restore(context.Background(), e.task(), "magaza", BackupRestore{Run: "RUN1", DB: true, View: v.ID}, Actor{})
	if err == nil || !strings.Contains(err.Error(), "dökümü bulunamadı") || contains(e.sites.calls, "stop") || e.mongo.get("magaza")["eski"] != 7 {
		t.Errorf("missing dump: %v %v", err, e.sites.calls)
	}
}

func TestOpenedBackupOffTheList(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN2")
	// Retention takes the hourly backup off the list while it is open.
	e.restic.snaps = append(e.restic.snaps[:3], e.restic.snaps[4:]...)
	e.list(t)
	d, err := e.b.SiteBackup("magaza", "RUN2")
	if err != nil || d.View == nil || d.Parts[PartDB].ID != snapID(4) {
		t.Fatalf("from the view: %+v %v", d, err)
	}
	if _, err := e.b.Restore(context.Background(), e.task(), "magaza", BackupRestore{Run: "RUN2", DB: true, View: v.ID}, Actor{}); err != nil {
		t.Errorf("restore from a view off the list: %v", err)
	}
	if _, err := e.b.SiteBackup("magaza", "RUN2"); !errors.Is(err, errBackupGone) {
		t.Errorf("closed view still listed: %v", err)
	}
}

func TestRestoreBothRollsBack(t *testing.T) {
	e := newY3(t)
	e.list(t)
	v, _ := e.open(t, "RUN1")
	data := filepath.Join(e.root, "srv/musteri/magaza/data")
	e.sites.health = func(started int) error {
		if started > 0 {
			return errors.New("502 Bad Gateway")
		}
		return nil
	}
	tk := e.task()
	_, err := e.b.Restore(context.Background(), tk, "magaza", BackupRestore{Run: "RUN1", DB: true, Files: true, View: v.ID}, Actor{})
	if err == nil || !strings.Contains(err.Error(), "geri alındı") {
		t.Fatalf("unhealthy site: %v\n%s", err, steps(tk))
	}
	if got := e.mongo.get("magaza"); got["eski"] != 7 || got["urunler"] != 3 {
		t.Errorf("database not back: %v", got)
	}
	if readFile(t, filepath.Join(data, "simdi.txt")) != "şimdiki" {
		t.Error("files not back")
	}
	if _, err := os.Stat(filepath.Join(data, "alt")); !os.IsNotExist(err) {
		t.Error("the backup's files stayed")
	}
	if _, err := e.b.loadView(v.ID); err != nil {
		t.Error("a failed restore closed the view")
	}
}

func TestRestoreFilesCountMismatch(t *testing.T) {
	e := newY3(t)
	e.list(t)
	e.restic.unsure = true
	_, err := e.b.Restore(context.Background(), e.task(), "magaza", BackupRestore{Run: "RUN1", Files: true}, Actor{})
	if err == nil || !strings.Contains(err.Error(), "tanesini açtı") || contains(e.sites.calls, "stop") {
		t.Errorf("a short restic summary: %v", err)
	}
}

func TestSoleOwnerAndSetID(t *testing.T) {
	if _, ok := soleOwner(map[int]int{1000: 3, 0: 1}); ok {
		t.Error("two owners")
	}
	if uid, ok := soleOwner(map[int]int{1000: 3}); !ok || uid != 1000 {
		t.Error("one owner")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("x"), 0o755)
	os.Chmod(filepath.Join(dir, "a"), 0o755|os.ModeSetuid)
	os.WriteFile(filepath.Join(dir, "b"), []byte("y"), 0o644)
	sv, _ := survey(dir)
	if sv.Special != 1 {
		t.Fatalf("special %d", sv.Special)
	}
	if n, err := clearSetID(dir); err != nil || n != 1 {
		t.Fatalf("cleared %d %v", n, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "a")); st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
}

func TestExportEnvFirstWithoutDBURI(t *testing.T) {
	e := newY3(t)
	e.list(t)
	// A compose.yaml edited by hand: the archive fails before anything is written.
	good := e.restic.snaps[2].Def["compose.yaml"]
	e.restic.snaps[2].Def["compose.yaml"] = "services:\n  site:\n    environment:\n      bozuk satır\n"
	tk := e.task()
	if _, err := e.b.ExportBackup(context.Background(), tk, "magaza", BackupExport{Run: "RUN1", DB: true, Files: true, Env: true}); err == nil {
		t.Fatal("exported with an unreadable environment")
	}
	if len(e.restic.ran("dump")) != 1 {
		t.Errorf("dumped before the environment was read: %v", e.restic.ran("dump"))
	}
	if _, err := os.Stat(filepath.Join(e.root, "staging", exportPrefix+tk.ID())); !os.IsNotExist(err) {
		t.Error("staging left")
	}
	// The site's database connection FootOps made is not in the archive.
	spec, _ := e.data.Store.Get("magaza")
	if spec.DBVar == "" {
		t.Fatal("the stored site has no database variable")
	}
	env, err := sites.ParseComposeEnv(good)
	if err != nil {
		t.Fatal(err)
	}
	env[spec.DBVar] = "mongodb://magaza:sifre@mongo:27017/magaza"
	e.restic.snaps[2].Def["compose.yaml"] = sites.Compose(spec, env)
	tk = e.task()
	info, err := e.b.ExportBackup(context.Background(), tk, "magaza", BackupExport{Run: "RUN1", DB: true, Env: true})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(tk))
	}
	_, path, _ := e.data.ExportFile("magaza", info.ID)
	f, _ := os.Open(path)
	defer f.Close()
	tr := tar.NewReader(f)
	var text string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		if h.Name == archive.MemberEnv {
			b, _ := io.ReadAll(tr)
			text = string(b)
		}
	}
	if strings.Contains(text, "sifre") || !strings.Contains(text, spec.DBVar+" alınmadı") || !strings.Contains(steps(tk), "3 değişken") {
		t.Errorf("env %q\n%s", text, steps(tk))
	}
}

func TestIndexUnreadable(t *testing.T) {
	e := newY3(t)
	e.list(t)
	os.WriteFile(e.b.indexPath(), []byte("{bozuk"), 0o600)
	if _, err := e.b.SiteBackups("magaza"); err == nil || !strings.Contains(err.Error(), "yedek listesi okunamadı") {
		t.Errorf("corrupt index: %v", err)
	}
	if _, err := e.b.SiteBackup("magaza", "RUN1"); err == nil {
		t.Error("detail from a corrupt index")
	}
	e.list(t) // read again from the repository, it is whole once more
	if l, err := e.b.SiteBackups("magaza"); err != nil || len(l.Points) != 2 {
		t.Errorf("after a refresh: %+v %v", l, err)
	}
}

func TestPlainTarStartingWithEnv(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"env", "A=1\n"}, {"index.html", "<h1>"}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	p := filepath.Join(t.TempDir(), "site.tar")
	os.WriteFile(p, buf.Bytes(), 0o600)
	f, _ := os.Open(p)
	defer f.Close()
	var ic ImportCheck
	if _, err := inspectFootOps(context.Background(), f, &ic, nil); !errors.Is(err, archive.ErrNotFootOps) {
		t.Errorf("a plain tar whose first file is env: %v", err)
	}
}

func TestReadsLeaveTheLockFlagAlone(t *testing.T) {
	e := newY3(t)
	e.list(t)
	e.b.unlocked = true
	ctx, cancel := context.WithCancel(context.Background())
	e.restic.during = func(ra []string, c run.Cmd) {
		if ra[0] == "ls" {
			cancel() // the browser went away
		}
	}
	e.b.BackupFiles(ctx, "magaza", "RUN1", "")
	if !e.b.unlocked {
		t.Error("a read outside the jobs (no lock) asked the next job to clear locks")
	}
	for _, c := range e.restic.cmds {
		if args := strings.Join(c.Args, " "); strings.Contains(args, " ls ") && !strings.Contains(args, "--memory 256m") {
			t.Errorf("a read with the jobs' limits: %s", args)
		}
	}
}
