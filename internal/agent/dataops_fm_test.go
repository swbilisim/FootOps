package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/datafs"
)

// The file manager end to end through the agent's API, with the helper's real code run
// in-process (fakeMongo.helper).
func TestFilesEndpoints(t *testing.T) {
	f := newF2(t)
	f.writeData(t, map[string]string{"fotolar/a.jpg": "aaaa", "fotolar/b.jpg": "bb", "not.txt": "merhaba"})
	jobs := NewJobs()
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = f.ops, f.data, jobs })
	a := Actor{User: "furkan", Via: "tunnel"}
	status := func(err error) int {
		var ae *Error
		if errors.As(err, &ae) {
			return ae.Status
		}
		return 0
	}

	l, err := c.Files(ctx, "magaza", "", datafs.ListOptions{})
	if err != nil || len(l.Entries) != 2 || l.Entries[0].Name != "fotolar" || l.Entries[1].Size != 7 {
		t.Fatalf("list: %+v %v", l, err)
	}
	if _, err := c.Files(ctx, "magaza", "../..", datafs.ListOptions{}); status(err) != 422 {
		t.Errorf("escape: %v", err)
	}
	if _, err := c.Files(ctx, "magaza", "yok", datafs.ListOptions{}); status(err) != 404 {
		t.Errorf("missing folder: %v", err)
	}
	// Quick operations run the helper directly, not in a systemd scope.
	if f.mongo.fsCalls[0] == "systemd-run" {
		t.Errorf("listing ran in a scope")
	}

	for _, ch := range []FilesChange{{Op: "mkdir", Name: "yeni"}, {Op: "rename", Name: "not.txt", To: "notlar.txt"},
		{Op: "move", Dir: "fotolar", Names: []string{"b.jpg"}, To: "yeni"}} {
		ch.Actor = a
		if _, err := c.ChangeFiles(ctx, "magaza", ch); err != nil {
			t.Fatalf("%s: %v", ch.Op, err)
		}
	}
	if _, err := c.ChangeFiles(ctx, "magaza", FilesChange{Op: "mkdir", Name: "yeni", Actor: a}); status(err) != 409 {
		t.Errorf("mkdir twice: %v", err)
	}
	e, err := c.ChangeFiles(ctx, "magaza", FilesChange{Op: "trash", Dir: "fotolar", Names: []string{"a.jpg"}, Actor: a})
	if err != nil || e == nil || e.Files != 1 || e.Bytes != 4 {
		t.Fatalf("trash: %+v %v", e, err)
	}
	want := map[string]string{"yeni/b.jpg": "bb", "notlar.txt": "merhaba"}
	if got := f.readData(t); !reflect.DeepEqual(got, want) {
		t.Errorf("data = %v", got)
	}
	// The trash is next to data/, so the app does not see it.
	trashDir := filepath.Join(filepath.Dir(f.dataDir()), "trash")
	if st, err := os.Stat(trashDir); err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("trash folder: %v %v", st, err)
	}
	list, err := c.Trash(ctx, "magaza")
	if err != nil || len(list) != 1 || list[0].ID != e.ID || list[0].Actor != "furkan" {
		t.Fatalf("trash list: %+v %v", list, err)
	}

	// Download: the helper opens the file and hands it over; ranges work (a resumed
	// download is not audited again).
	resp, err := c.DownloadFile(ctx, "magaza", "yeni/b.jpg", a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(resp.Body); string(body) != "bb" {
		t.Errorf("download = %q", body)
	}
	resp.Body.Close()
	resp, err = c.DownloadFile(ctx, "magaza", "yeni/b.jpg", a, http.Header{"Range": {"bytes=1-"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || string(body) != "b" || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="b.jpg"`) ||
		resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("download: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	if _, err := c.DownloadFile(ctx, "magaza", "yeni", a, nil); status(err) != 422 {
		t.Errorf("download a folder: %v", err)
	}

	// Zip of a folder.
	resp, err = c.ZipFiles(ctx, "magaza", "", []string{"yeni"}, a)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil || len(zr.File) != 2 || zr.File[1].Name != "yeni/b.jpg" || !strings.Contains(resp.Header.Get("Content-Disposition"), "yeni.zip") {
		t.Errorf("zip: %v %v", zr, err)
	}
	if _, err := c.ZipFiles(ctx, "magaza", "yok", nil, a); status(err) == 0 || status(err) == 200 {
		t.Errorf("zip of a missing folder: %v", err)
	}

	// Upload into a folder: the staged file is removed afterwards.
	up := f.upload(t, "çiçek.jpg", []byte("yeni foto"))
	p, err := c.PlaceUpload(ctx, "magaza", PlaceRequest{Upload: up, Dir: "yeni", Actor: a})
	if err != nil || p.Name != "çiçek.jpg" || p.Size != 9 {
		t.Fatalf("place: %+v %v", p, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir(), "yeni/çiçek.jpg")); string(b) != "yeni foto" {
		t.Errorf("placed = %q", b)
	}
	if _, err := c.Upload(ctx, up); status(err) != 404 {
		t.Errorf("staged upload kept: %v", err)
	}
	if _, err := c.PlaceUpload(ctx, "magaza", PlaceRequest{Upload: up, Dir: "yeni", Actor: a}); status(err) != 404 {
		t.Errorf("placed twice: %v", err)
	}

	// Restore from the trash.
	r, err := c.RestoreTrash(ctx, "magaza", e.ID, a)
	if err != nil || r.Dir != "fotolar" || len(r.Items) != 1 {
		t.Fatalf("restore: %+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir(), "fotolar/a.jpg")); string(b) != "aaaa" {
		t.Errorf("restored = %q", b)
	}

	// Unpack is a job, in a scope.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("arsiv/x.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.writeData(t, map[string]string{"gelen/a.zip": zbuf.String()})
	id, err := c.Unpack(ctx, "magaza", UnpackRequest{Path: "gelen/a.zip", TrashArchive: true, Actor: a})
	if err != nil {
		t.Fatal(err)
	}
	var j Job
	waitFor(t, func() bool { j, _ = c.Job(ctx, id); return j.Done })
	if j.Err != "" || j.Result != "gelen" || f.mongo.fsCalls[len(f.mongo.fsCalls)-1] != "systemd-run" {
		t.Fatalf("unpack job: %+v", j)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir(), "gelen/arsiv/x.txt")); string(b) != "x" {
		t.Errorf("unpacked = %q", b)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir(), "gelen/a.zip")); err == nil {
		t.Error("archive not moved to the trash")
	}

	// The web address of the files.
	if err := c.SetDataURL(ctx, "magaza", "/uploads/", a); err != nil {
		t.Fatal(err)
	}
	if spec, _ := f.ops.Store.Get("magaza"); spec.DataURL != "/uploads" {
		t.Errorf("data url = %q", spec.DataURL)
	}
	if err := c.SetDataURL(ctx, "magaza", "/../x", a); status(err) != 422 {
		t.Errorf("bad url: %v", err)
	}

	// Emptying the trash needs the site's name.
	if _, err := c.EmptyTrash(ctx, "magaza", "yanlış", a); status(err) != 422 {
		t.Errorf("empty without the name: %v", err)
	}
	res, err := c.EmptyTrash(ctx, "magaza", "magaza", a)
	if err != nil || res.Entries != 1 || res.Bytes != int64(len(zbuf.Bytes())) {
		t.Errorf("empty: %+v %v", res, err)
	}
	if list, _ := c.Trash(ctx, "magaza"); len(list) != 0 {
		t.Errorf("trash after emptying: %+v", list)
	}

	// While a job works on the site, changes wait (423); other sites are not held up.
	release := make(chan struct{})
	if _, err := jobs.Start(JobSpec{Kind: "site.redeploy", Target: "magaza"}, func(ctxJob context.Context, t *Task) error { <-release; return nil }, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ChangeFiles(ctx, "magaza", FilesChange{Op: "mkdir", Name: "x", Actor: a}); status(err) != 423 {
		t.Errorf("change during a job: %v", err)
	}
	if _, err := c.Files(ctx, "magaza", "", datafs.ListOptions{}); err != nil {
		t.Errorf("listing during a job: %v", err)
	}
	close(release)

	entries, _ := s.Audit.Tail(100)
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Action)
	}
	for _, want := range []string{"files.mkdir", "files.rename", "files.move", "files.trash", "files.download", "files.zip",
		"files.upload", "files.untrash", "files.unpack", "files.url", "files.empty-trash"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}
}

// A zip that fails before its first byte is answered with a status; one that fails
// later ends without its last chunk, so the download shows as failed.
func TestZipFailures(t *testing.T) {
	f := newF2(t)
	f.writeData(t, map[string]string{"a/b.txt": "b"})
	c, _ := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = f.ops, f.data, NewJobs() })
	a := Actor{User: "furkan"}
	if _, err := c.ZipFiles(ctx, "magaza", "yok", nil, a); err == nil || err.(*Error).Status != 404 {
		t.Errorf("missing folder: %v", err)
	}
	if _, err := c.ZipFiles(ctx, "magaza", "", []string{"../x"}, a); err == nil || err.(*Error).Status != 422 {
		t.Errorf("bad name: %v", err)
	}
	f.mongo.zipBreak = true
	resp, err := c.ZipFiles(ctx, "magaza", "a", nil, a)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Error("a broken zip looked complete")
	}
}

// The upload is first offered as a link; where that is not possible (not Linux here),
// the copy runs again in a limited scope.
func TestPlaceCopiesInScope(t *testing.T) {
	f := newF2(t)
	up := f.upload(t, "a.txt", []byte("veri"))
	p, err := f.data.PlaceUpload(ctx, "magaza", up, "", false, Actor{User: "furkan"})
	if err != nil || p.Name != "a.txt" {
		t.Fatalf("place: %+v %v", p, err)
	}
	want := []string{f.data.self()}
	if runtime.GOOS != "linux" {
		want = append(want, "systemd-run")
	}
	if !reflect.DeepEqual(f.mongo.fsCalls, want) || p.Linked != (runtime.GOOS == "linux") {
		t.Errorf("helpers = %v, placed %+v", f.mongo.fsCalls, p)
	}
}

// Emptying the trash removes what was there when asked, not what arrives meanwhile, and
// a record that is not a regular file cannot stop it.
func TestEmptyTrashCutoff(t *testing.T) {
	f := newF2(t)
	trash := filepath.Join(filepath.Dir(f.dataDir()), "trash")
	past := f.clock.Add(-time.Hour).Format("20060102-150405")
	future := f.clock.Add(time.Hour).Format("20060102-150405")
	for _, d := range []string{past + "-aaaaaa/f/x", future + "-bbbbbb/f/y"} {
		if err := os.MkdirAll(filepath.Join(trash, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(trash, past+"-aaaaaa", "meta.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var n int
	go func() { n, _, _ = f.data.EmptyTrash("magaza"); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emptying hung on a FIFO")
	}
	ents, _ := os.ReadDir(trash)
	if n != 1 || len(ents) != 1 || ents[0].Name() != future+"-bbbbbb" {
		t.Errorf("removed %d, left %v", n, ents)
	}
}

func TestFilesNeedDataDir(t *testing.T) {
	f := newF2(t)
	spec, _ := f.ops.Store.Get("magaza")
	spec.DataMount = ""
	if err := f.ops.Store.WriteMeta(spec); err != nil {
		t.Fatal(err)
	}
	c, _ := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = f.ops, f.data, NewJobs() })
	if _, err := c.Files(ctx, "magaza", "", datafs.ListOptions{}); err == nil || err.(*Error).Status != 400 {
		t.Errorf("err = %v", err)
	}
	if _, err := c.Files(ctx, "yok", "", datafs.ListOptions{}); err == nil || err.(*Error).Status != 404 {
		t.Errorf("err = %v", err)
	}
}

func TestHold(t *testing.T) {
	j := NewJobs()
	release, err := j.Hold("magaza")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start(JobSpec{Kind: "import.apply", Target: "magaza"}, func(context.Context, *Task) error { return nil }, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("job on a held site: %v", err)
	}
	if !j.Busy() {
		t.Error("not busy while held")
	}
	// Another site's job runs; a second hold on the same site is fine.
	done := make(chan struct{})
	id, err := j.Start(JobSpec{Kind: "site.export", Target: "blog"}, func(context.Context, *Task) error { <-done; return nil }, nil)
	if err != nil {
		t.Fatalf("other site: %v", err)
	}
	r2, err := j.Hold("magaza")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Hold("blog"); !errors.Is(err, ErrBusy) {
		t.Errorf("hold during a job on that site: %v", err)
	}
	release()
	release() // twice is harmless
	r2()
	close(done)
	waitFor(t, func() bool { job, _ := j.Get(id); return job.Done })
	if j.Busy() {
		t.Error("still busy")
	}
	if _, err := j.Start(JobSpec{Kind: "import.apply", Target: "magaza"}, func(context.Context, *Task) error { return nil }, nil); err != nil {
		t.Errorf("after release: %v", err)
	}
}

func TestSweepTrash(t *testing.T) {
	f := newF2(t)
	trash := filepath.Join(filepath.Dir(f.dataDir()), "trash")
	now := f.clock
	old := now.Add(-8 * 24 * time.Hour).Format("20060102-150405")
	recent := now.Add(-2 * 24 * time.Hour).Format("20060102-150405")
	for _, d := range []string{old + "-aaaaaa/f/x", recent + "-bbbbbb/f/y", datafs.UnpackPrefix + recent + "-cccccc/z",
		datafs.UnpackPrefix + now.Add(-time.Hour).Format("20060102-150405") + "-dddddd", "baska-bir-sey"} {
		if err := os.MkdirAll(filepath.Join(trash, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	removed := f.data.SweepTrash()
	if len(removed) != 2 {
		t.Errorf("removed = %v", removed)
	}
	ents, _ := os.ReadDir(trash)
	var left []string
	for _, e := range ents {
		left = append(left, e.Name())
	}
	if !reflect.DeepEqual(left, []string{datafs.UnpackPrefix + now.Add(-time.Hour).Format("20060102-150405") + "-dddddd", recent + "-bbbbbb", "baska-bir-sey"}) {
		t.Errorf("left = %v", left)
	}
}
