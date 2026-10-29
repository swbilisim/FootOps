package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
)

// fakeStream plays a tool container: it writes out to stdout and lines to stderr.
type fakeStream struct {
	out   []byte
	lines []string
	err   error
	seen  []run.Cmd
	check func(c run.Cmd)
}

func (f *fakeStream) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	f.seen = append(f.seen, c)
	if f.check != nil {
		f.check(c)
	}
	for _, l := range f.lines {
		c.Stderr(l)
	}
	if _, err := c.Stdout.Write(f.out); err != nil {
		return run.Usage{}, err
	}
	return run.Usage{PeakMem: 30 << 20, CPU: 200 * time.Millisecond, Wall: time.Second}, f.err
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

var dumpOut = append([]byte{0x6d, 0xe2, 0x99, 0x81}, bytes.Repeat([]byte("bson"), 1000)...)

type dataEnv struct {
	*opsEnv
	data   *DataOps
	stream *fakeStream
	task   *Task
	now    time.Time
}

func newData(t *testing.T) *dataEnv {
	t.Helper()
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("magaza"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	st := &fakeStream{out: gz(t, dumpOut), lines: []string{
		"2026-10-27T11:00:00.000+0000\twriting magaza.users to archive on stdout",
		"2026-10-27T11:00:00.000+0000\tdone dumping `magaza.users` (12 documents)",
		"2026-10-27T11:00:00.000+0000\tdone dumping magaza.pages (1 document)",
	}}
	d := &dataEnv{opsEnv: e, stream: st, now: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)}
	d.data = &DataOps{Store: sites.Store{Root: e.root}, Docker: e.net, Run: e.run, Stream: st, Mongo: &statsOnly{fakeUsers{users: map[string]string{}}},
		Staging: staging.New(filepath.Join(e.root, "staging"), 1<<30), SnapshotDir: filepath.Join(e.root, "snapshots"),
		now: func() time.Time { return d.now }, checkFree: func(string, int64) error { return nil }}
	d.task = &Task{j: NewJobs(), job: &Job{ID: "job1"}}
	return d
}

// statsOnly answers dbStats for the site's database.
type statsOnly struct{ fakeUsers }

func (*statsOnly) Stats(_ context.Context, c dbCreds) (dbStats, error) {
	if c.DB != "magaza" || c.User != "magaza" || c.Password == "" {
		return dbStats{}, fmt.Errorf("unexpected credentials %+v", c)
	}
	return dbStats{DataSize: 4000, Objects: 13, Collections: 2}, nil
}

func TestSnapshot(t *testing.T) {
	d := newData(t)
	d.stream.check = func(c run.Cmd) {
		args := strings.Join(c.Args, " ")
		for _, want := range []string{"run --rm --name footops-job-job1", "--network container:footops-mongo", "--log-driver none",
			"--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--memory 512m", "--memory-swap 512m",
			"--cpus 0.50", "--pids-limit 256", "mongo:7.0.45 -c 3 nice -n 10 mongodump --config=/run/footops/mongo.yaml --db=magaza --archive --gzip --numParallelCollections=1"} {
			if !strings.Contains(args, want) {
				t.Errorf("tool command lacks %q:\n%s", want, args)
			}
		}
		cfg := filepath.Join(d.root, "staging", "job1", "mongo.yaml")
		st, err := os.Stat(cfg)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("config %v", err)
		}
		b := readFile(t, cfg)
		if !strings.HasPrefix(b, `uri: "mongodb://magaza:`) || !strings.Contains(b, "@127.0.0.1:27017/?authSource=magaza") {
			t.Errorf("config = %s", b)
		}
		pw := strings.TrimSuffix(strings.SplitN(strings.SplitN(b, ":", 4)[2], "@", 2)[0], "")
		if strings.Contains(args, pw) {
			t.Error("password on the command line")
		}
		if !strings.Contains(args, "--volume "+filepath.Join(d.root, "staging", "job1")+":/run/footops:ro") {
			t.Errorf("config not mounted read-only: %s", args)
		}
	}
	snap, err := d.data.Snapshot(context.Background(), d.task, "magaza", "manual", Actor{User: "furkan"})
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(d.task.job.Steps, "\n"))
	}
	sum := sha256.Sum256(d.stream.out)
	if snap.ID != "20261027-110000" || snap.Documents != 13 || snap.Collections["users"] != 12 || snap.Collections["pages"] != 1 ||
		snap.Bytes != int64(len(d.stream.out)) || snap.SHA256 != hex.EncodeToString(sum[:]) || snap.Usage.PeakMem != 30<<20 {
		t.Errorf("snap = %+v", snap)
	}
	if _, err := os.Stat(filepath.Join(d.root, "staging", "job1")); !os.IsNotExist(err) {
		t.Error("staging (with the password) left behind")
	}
	list, err := d.data.Snapshots("magaza")
	if err != nil || len(list) != 1 || list[0].ID != snap.ID || list[0].Actor != "furkan" {
		t.Fatalf("list = %+v %v", list, err)
	}
	got, path, err := d.data.SnapshotFile("magaza", snap.ID)
	if err != nil || got.SHA256 != snap.SHA256 {
		t.Fatal(err)
	}
	if b := readFile(t, path); b != string(d.stream.out) {
		t.Error("archive content differs")
	}
	st, _ := os.Stat(filepath.Dir(path))
	if st.Mode().Perm() != 0o700 {
		t.Errorf("snapshot dir mode %v", st.Mode())
	}
	if d.task.job.Result != snap.ID || d.task.job.Usage.PeakMem != 30<<20 || d.task.job.Bytes != snap.Bytes {
		t.Errorf("task = %+v", d.task.job)
	}
	// Same second again: refused, the first copy is untouched.
	if _, err := d.data.Snapshot(context.Background(), d.task, "magaza", "manual", Actor{}); err == nil {
		t.Error("second copy in the same second")
	}
	if list, _ := d.data.Snapshots("magaza"); len(list) != 1 {
		t.Errorf("list after refusal = %d", len(list))
	}
}

func TestSnapshotFailuresLeaveNothing(t *testing.T) {
	cases := map[string]func(d *dataEnv){
		"tool fails":     func(d *dataEnv) { d.stream.err = errors.New("exit status 1: Failed: auth error") },
		"not gzip":       func(d *dataEnv) { d.stream.out = []byte("garbage") },
		"not mongodump":  func(d *dataEnv) { d.stream.out = gz(t, []byte("hello world")) },
		"truncated gzip": func(d *dataEnv) { d.stream.out = gz(t, dumpOut)[:40] },
		"cancelled": func(d *dataEnv) {
			d.stream.err = context.Canceled
		},
	}
	for name, setup := range cases {
		d := newData(t)
		setup(d)
		_, err := d.data.Snapshot(context.Background(), d.task, "magaza", "manual", Actor{})
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		ents, _ := os.ReadDir(filepath.Join(d.root, "snapshots", "magaza"))
		if len(ents) != 0 {
			t.Errorf("%s: left %d entries", name, len(ents))
		}
		if _, err := os.Stat(filepath.Join(d.root, "staging", "job1")); !os.IsNotExist(err) {
			t.Errorf("%s: staging left behind", name)
		}
	}
}

func TestSnapshotRefusals(t *testing.T) {
	d := newData(t)
	d.data.checkFree = func(string, int64) error { return errors.New("diskte yer yok") }
	if _, err := d.data.Snapshot(context.Background(), d.task, "magaza", "manual", Actor{}); err == nil || !strings.Contains(err.Error(), "diskte yer yok") {
		t.Errorf("disk: %v", err)
	}
	if len(d.stream.seen) != 0 {
		t.Error("tool ran although the disk check failed")
	}

	d = newData(t)
	nodb := specFor("nodb")
	nodb.Database, nodb.Domains = false, []string{"nodb.enisparta.com"}
	if err := d.ops.Create(context.Background(), nodb, nil, d.progress); err != nil {
		t.Fatal(err)
	}
	if _, err := d.data.Snapshot(context.Background(), d.task, "nodb", "manual", Actor{}); err == nil || !strings.Contains(err.Error(), "veritabanı yok") {
		t.Errorf("no db: %v", err)
	}
	if _, err := d.data.Snapshot(context.Background(), d.task, "yok", "manual", Actor{}); err == nil {
		t.Error("unknown site")
	}

	// A connection string that is not FootOps' own is not used.
	d = newData(t)
	sp, _ := d.data.Store.Get("magaza")
	env, _ := d.data.Store.Env(sp)
	env["MONGODB_URI"] = "mongodb://x:y@cluster0.example.net:27017/magaza"
	if err := d.data.Store.WriteCompose(sp, env); err != nil {
		t.Fatal(err)
	}
	if _, err := d.data.Snapshot(context.Background(), d.task, "magaza", "manual", Actor{}); err == nil || !strings.Contains(err.Error(), "yerel MongoDB") {
		t.Errorf("foreign URI: %v", err)
	}
}

func TestSnapshotPathsAreValidated(t *testing.T) {
	d := newData(t)
	for _, c := range [][2]string{{"../etc", "20261027-110000"}, {"magaza", "../../x"}, {"magaza", "20261027"}} {
		if _, _, err := d.data.SnapshotFile(c[0], c[1]); err == nil {
			t.Errorf("SnapshotFile(%q, %q) accepted", c[0], c[1])
		}
	}
	if _, err := d.data.Snapshots("../etc"); err == nil {
		t.Error("Snapshots(../etc) accepted")
	}
}

func TestSweepSnapshots(t *testing.T) {
	d := newData(t)
	mk := func(site, id string, complete bool) {
		dir := filepath.Join(d.root, "snapshots", site, id)
		os.MkdirAll(dir, 0o700)
		if complete {
			os.WriteFile(filepath.Join(dir, snapshotManifest), []byte(`{"id":"`+id+`","site":"`+site+`"}`), 0o600)
		}
	}
	mk("a", "20261001-000000", true)  // old
	mk("a", "20261019-000000", true)  // old
	mk("a", "20261026-000000", true)  // recent
	mk("b", "20261001-000000", true)  // old but b's newest: kept
	mk("b", "20261001-000001", false) // incomplete, stale
	mk("c", "20261027-105000", false) // incomplete, maybe still being written
	got := d.data.SweepSnapshots(false)
	want := "a/20261001-000000 a/20261019-000000 b/20261001-000001"
	if strings.Join(got, " ") != want {
		t.Errorf("removed %v, want %s", got, want)
	}
	if list, _ := d.data.Snapshots("b"); len(list) != 1 {
		t.Error("b's only copy removed")
	}
	if _, err := os.Stat(filepath.Join(d.root, "snapshots", "c", "20261027-105000")); err != nil {
		t.Error("copy in progress removed")
	}
	// At agent start nothing is being written: a half copy goes at once.
	if got := d.data.SweepSnapshots(true); strings.Join(got, " ") != "c/20261027-105000" {
		t.Errorf("idle sweep removed %v", got)
	}
}

func TestResumes(t *testing.T) {
	for rg, want := range map[string]bool{"": false, "bytes=0-": false, "bytes=0-99": false, "bytes=-500": false,
		"bytes=100-": true, "bytes=100-200": true, "bytes=100-200,0-5": false, "items=5-": false, "bytes=x-": false} {
		if got := resumes(rg); got != want {
			t.Errorf("resumes(%q) = %v", rg, got)
		}
	}
}

func TestSweepKeepsCopyWithBrokenManifest(t *testing.T) {
	d := newData(t)
	dir := filepath.Join(d.root, "snapshots", "a", "20261027-100000")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, snapshotFile), []byte("archive"), 0o600)
	os.WriteFile(filepath.Join(dir, snapshotManifest), nil, 0o600) // crash before the manifest reached the disk
	if got := d.data.SweepSnapshots(true); len(got) != 0 {
		t.Errorf("removed %v", got)
	}
	d.now = d.now.Add(SnapshotKeep + 24*time.Hour) // past its time: goes like any old copy
	if got := d.data.SweepSnapshots(false); len(got) != 1 {
		t.Errorf("old orphan kept: %v", got)
	}
}

func TestCountingWriterReports(t *testing.T) {
	var got []int64
	w := &countingWriter{w: io.Discard, report: func(n int64) { got = append(got, n) }}
	for i := 0; i < 5; i++ {
		w.Write(make([]byte, 600<<10))
	}
	if w.n != 5*600<<10 || len(got) != 2 {
		t.Errorf("n=%d reports=%v", w.n, got)
	}
}

func TestSnapshotEndpoints(t *testing.T) {
	d := newData(t)
	d.data.now = time.Now // the job writes with the real clock
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = d.ops, d.data, NewJobs() })
	if _, err := c.TakeSnapshot(ctx, "nope", Actor{}); err == nil || err.(*Error).Status != 404 {
		t.Errorf("unknown site: %v", err)
	}
	id, err := c.TakeSnapshot(ctx, "magaza", Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	var j Job
	waitFor(t, func() bool { j, _ = c.Job(ctx, id); return j.Done })
	if j.Err != "" || j.Result == "" || !j.Cancellable || j.Usage.PeakMem == 0 {
		t.Fatalf("job = %+v", j)
	}
	list, err := c.Snapshots(ctx, "magaza")
	if err != nil || len(list) != 1 || list[0].ID != j.Result {
		t.Fatalf("list = %+v %v", list, err)
	}

	resp, err := c.DownloadSnapshot(ctx, "magaza", j.Result, Actor{User: "furkan"}, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(body, d.stream.out) || resp.Header.Get("Content-Type") != "application/gzip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename="magaza-`+j.Result+`.archive.gz"`) ||
		resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("X-Content-Sha256") != list[0].SHA256 {
		t.Errorf("download: %d bytes, headers %v", len(body), resp.Header)
	}
	// Resume from byte 10.
	resp, err = c.DownloadSnapshot(ctx, "magaza", j.Result, Actor{User: "furkan"}, http.Header{"Range": {"bytes=10-"}})
	if err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(rest, d.stream.out[10:]) {
		t.Errorf("range: %d, %d bytes", resp.StatusCode, len(rest))
	}
	if _, err := c.DownloadSnapshot(ctx, "magaza", "20200101-000000", Actor{}, http.Header{}); err == nil || err.(*Error).Status != 404 {
		t.Errorf("missing copy: %v", err)
	}
	if _, err := c.DownloadSnapshot(ctx, "magaza", "..%2f..%2fx", Actor{}, http.Header{}); err == nil {
		t.Error("traversal accepted")
	}

	entries, _ := s.Audit.Tail(10)
	var took, downloads int
	for _, e := range entries {
		switch e.Action {
		case "db.snapshot":
			took++
			if !e.OK || e.Actor.User != "furkan" || !strings.Contains(e.Detail, j.Result) {
				t.Errorf("audit = %+v", e)
			}
		case "db.snapshot.download":
			downloads++
			if e.Actor.User != "furkan" {
				t.Errorf("download audit = %+v", e)
			}
		}
	}
	// A suffix range returns the whole file: audited like a new download.
	resp, err = c.DownloadSnapshot(ctx, "magaza", j.Result, Actor{User: "furkan"}, http.Header{"Range": {"bytes=-99999999"}})
	if err != nil {
		t.Fatal(err)
	}
	drain(resp.Body)
	entries, _ = s.Audit.Tail(10)
	downloads = 0
	for _, e := range entries {
		if e.Action == "db.snapshot.download" {
			downloads++
		}
	}
	if took != 1 || downloads != 2 { // the resumed range is the same download
		t.Errorf("audit: %d copies, %d downloads", took, downloads)
	}

	// A running copy can be cancelled.
	block := make(chan struct{})
	d.stream.check = func(run.Cmd) { <-block }
	d.data.now = func() time.Time { return time.Now().Add(time.Hour) }
	id, err = c.TakeSnapshot(ctx, "magaza", Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ = c.Job(ctx, id); return len(j.Steps) >= 2 })
	if err := c.CancelJob(ctx, id, Actor{User: "furkan"}); err != nil {
		t.Fatal(err)
	}
	close(block) // the fake tool ignores its context; the job still reports cancelled
	waitFor(t, func() bool { j, _ = c.Job(ctx, id); return j.Done })
	if !j.Cancelled || j.Err != "iptal edildi" {
		t.Errorf("cancelled job = %+v", j)
	}
	if list, _ := c.Snapshots(ctx, "magaza"); len(list) != 1 {
		t.Errorf("cancelled copy kept: %d", len(list))
	}
}
