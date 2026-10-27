package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

func spec(kind, target string) JobSpec {
	return JobSpec{Kind: kind, Target: target, Actor: Actor{User: "furkan"}}
}

func TestJobsAreExclusive(t *testing.T) {
	j := NewJobs()
	release := make(chan struct{})
	finished := make(chan Job, 1)
	id, err := j.Start(spec("site.create", "a"), func(ctx context.Context, tk *Task) error {
		tk.Step("step one")
		<-release
		return errors.New("failed")
	}, func(job Job) { finished <- job })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start(spec("site.remove", "b"), func(context.Context, *Task) error { return nil }, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("second job: %v", err)
	}
	if r, ok := j.Running(); !ok || r.ID != id {
		t.Errorf("running = %+v %v", r, ok)
	}
	close(release)
	job := <-finished
	if !job.Done || job.Err != "failed" || len(job.Steps) != 1 {
		t.Errorf("job = %+v", job)
	}
	if got, ok := j.Get(id); !ok || !got.Done {
		t.Error("job not retrievable")
	}
	if _, ok := j.Running(); ok {
		t.Error("still running")
	}
	if _, err := j.Start(spec("site.remove", "b"), func(context.Context, *Task) error { return nil }, nil); err != nil {
		t.Errorf("not free after finishing: %v", err)
	}
}

func TestJobProgressUsageResult(t *testing.T) {
	j := NewJobs()
	finished := make(chan Job, 1)
	_, err := j.Start(spec("db.snapshot", "a"), func(ctx context.Context, tk *Task) error {
		tk.SetBytes(10, 100)
		tk.AddUsage(run.Usage{PeakMem: 50, CPU: time.Second})
		tk.AddUsage(run.Usage{PeakMem: 70, CPU: time.Second})
		tk.SetResult("20261027-120000")
		return nil
	}, func(job Job) { finished <- job })
	if err != nil {
		t.Fatal(err)
	}
	job := <-finished
	if job.Bytes != 10 || job.Total != 100 || job.Usage.PeakMem != 70 || job.Usage.CPU != 2*time.Second || job.Result != "20261027-120000" || job.Err != "" {
		t.Errorf("job = %+v", job)
	}
}

func TestJobCancel(t *testing.T) {
	j := NewJobs()
	started := make(chan struct{})
	finished := make(chan Job, 1)
	id, _ := j.Start(JobSpec{Kind: "db.snapshot", Target: "a", Cancellable: true}, func(ctx context.Context, tk *Task) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, func(job Job) { finished <- job })
	<-started
	if err := j.Cancel(id); err != nil {
		t.Fatal(err)
	}
	job := <-finished
	if !job.Cancelled || job.Err != "iptal edildi" {
		t.Errorf("job = %+v", job)
	}
	if err := j.Cancel(id); err == nil {
		t.Error("cancelled a finished job")
	}
	if err := j.Cancel("nope"); !errors.Is(err, ErrJobNotFound) {
		t.Errorf("unknown: %v", err)
	}

	// Site changes are not cancellable: they finish or roll back on their own.
	release := make(chan struct{})
	done := make(chan Job, 1)
	id, _ = j.Start(spec("site.update", "a"), func(ctx context.Context, tk *Task) error { <-release; return nil }, func(job Job) { done <- job })
	if err := j.Cancel(id); err == nil || !strings.Contains(err.Error(), "iptal edilemez") {
		t.Errorf("cancel site.update: %v", err)
	}
	close(release)
	<-done
}

func TestCancelAfterTheWorkIsDone(t *testing.T) {
	j := NewJobs()
	ready, release := make(chan struct{}), make(chan struct{})
	finished := make(chan Job, 1)
	id, _ := j.Start(JobSpec{Kind: "db.snapshot", Target: "a", Cancellable: true}, func(ctx context.Context, tk *Task) error {
		close(ready)
		<-release // committed; the cancel arrives now and is ignored
		return nil
	}, func(job Job) { finished <- job })
	<-ready
	if err := j.Cancel(id); err != nil {
		t.Fatal(err)
	}
	close(release)
	job := <-finished
	if job.Cancelled || job.Err != "" || !strings.Contains(job.Steps[len(job.Steps)-1], "zaten bitmişti") {
		t.Errorf("job = %+v", job)
	}
}

func TestJobTimeoutIsReported(t *testing.T) {
	old := jobTimeouts
	jobTimeouts = map[string]time.Duration{"slow": 50 * time.Millisecond}
	defer func() { jobTimeouts = old }()
	j := NewJobs()
	finished := make(chan Job, 1)
	_, _ = j.Start(spec("slow", "a"), func(ctx context.Context, tk *Task) error {
		<-ctx.Done()
		return ctx.Err()
	}, func(job Job) { finished <- job })
	if job := <-finished; !strings.Contains(job.Err, "zaman aşımı") {
		t.Errorf("err = %q", job.Err)
	}
}

func TestJobsPersistAndMarkInterrupted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jobs")
	j, interrupted, err := LoadJobs(dir)
	if err != nil || len(interrupted) != 0 {
		t.Fatal(err, interrupted)
	}
	finished := make(chan Job, 1)
	doneID, _ := j.Start(spec("site.redeploy", "a"), func(ctx context.Context, tk *Task) error {
		tk.Step("✓ ok")
		return nil
	}, func(job Job) { finished <- job })
	<-finished
	// A job still running when the "agent stops".
	hang := make(chan struct{})
	hung := make(chan Job, 1)
	// Let the job end before the temporary directory is removed.
	defer func() { close(hang); <-hung }()
	runID, _ := j.Start(spec("db.snapshot", "b"), func(ctx context.Context, tk *Task) error {
		tk.Step("… mongodump çalışıyor")
		<-hang
		return nil
	}, func(job Job) { hung <- job })
	waitFor(t, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, runID+".json"))
		return strings.Contains(string(b), "mongodump")
	})
	st, _ := os.Stat(filepath.Join(dir, runID+".json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("job file mode %v", st.Mode())
	}

	j2, interrupted, err := LoadJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(interrupted) != 1 || interrupted[0].ID != runID || !interrupted[0].Interrupted {
		t.Fatalf("interrupted = %+v", interrupted)
	}
	got, ok := j2.Get(runID)
	if !ok || !got.Done || !strings.Contains(got.Err, "yarıda") || got.Steps[len(got.Steps)-1][:3] != "✗" {
		t.Errorf("reloaded = %+v", got)
	}
	if got, ok := j2.Get(doneID); !ok || !got.Done || got.Interrupted || got.Steps[0] != "✓ ok" {
		t.Errorf("finished job = %+v %v", got, ok)
	}
	// Marked on disk: a third start does not report it again.
	if _, again, _ := LoadJobs(dir); len(again) != 0 {
		t.Errorf("reported twice: %+v", again)
	}
	// Junk in the directory is ignored.
	_ = os.WriteFile(filepath.Join(dir, "x.json"), []byte("{"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "../evil.json"), []byte("{}"), 0o600)
	if _, _, err := LoadJobs(dir); err != nil {
		t.Error(err)
	}
}

func TestJobsTrimKeepsDiskInStep(t *testing.T) {
	dir := t.TempDir()
	j, _, _ := LoadJobs(dir)
	for i := 0; i < maxJobs+5; i++ {
		done := make(chan Job, 1)
		if _, err := j.Start(spec("site.redeploy", "a"), func(context.Context, *Task) error { return nil }, func(job Job) { done <- job }); err != nil {
			t.Fatal(err)
		}
		<-done
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != maxJobs {
		t.Errorf("%d files on disk, want %d", len(ents), maxJobs)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
