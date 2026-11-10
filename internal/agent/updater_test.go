package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/update"
)

// releaseServer serves releases the way GitHub's releases page does.
type releaseServer struct {
	mu        sync.Mutex
	files     map[string][]byte // path → content
	downloads int
}

func (s *releaseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(r.URL.Path, "footops_linux") {
		s.downloads++
	}
	_, _ = w.Write(b)
}

// publish makes ver the latest release, signed with priv.
func (s *releaseServer) publish(t *testing.T, priv ed25519.PrivateKey, ver string, bin []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, bin, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := release.FileOf(path)
	if err != nil {
		t.Fatal(err)
	}
	m := release.Manifest{Format: 1, Version: ver, Commit: "abc1234", Date: "2026-11-12T10:00:00Z", Notes: "Eski imajlar silinir.",
		Files: map[string]release.File{release.BinaryName("amd64"): f}}
	man, _ := json.Marshal(m)
	sig := release.Sign(priv, man)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string][]byte{}
	}
	s.files["/latest/download/"+release.ManifestName] = man
	s.files["/latest/download/"+release.SigName] = sig
	s.files["/download/"+ver+"/"+release.BinaryName("amd64")] = bin
}

type updaterHarness struct {
	u       *Updater
	srv     *releaseServer
	priv    ed25519.PrivateKey
	started [][]string
	active  bool
	plan    update.Plan
	mu      sync.Mutex
	// onStart runs as the update's service would (writes its record).
	onStart func(args []string)
}

func newUpdater(t *testing.T) *updaterHarness {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	h := &updaterHarness{srv: &releaseServer{}, priv: priv}
	ts := httptest.NewServer(h.srv)
	t.Cleanup(ts.Close)
	dir := t.TempDir()
	h.u = &Updater{Dir: dir, Running: "v0.1.0", Arch: "amd64", Jobs: NewJobs(), Audit: NewAudit(filepath.Join(dir, "audit.jsonl")),
		Server: "h1", Source: update.Source{Base: ts.URL, HTTP: ts.Client(), Keys: map[string]ed25519.PublicKey{release.KeyID(pub): pub}},
		Plan:   func(context.Context, string) (update.Plan, error) { return h.plan, nil },
		Verify: func(context.Context, string, string) error { return nil },
		Start: func(_ context.Context, args []string) error {
			h.mu.Lock()
			h.started = append(h.started, args)
			f := h.onStart
			h.mu.Unlock()
			if f != nil {
				f(args)
			}
			return nil
		},
		Active: func(context.Context) bool { h.mu.Lock(); defer h.mu.Unlock(); return h.active }}
	return h
}

func waitJob(t *testing.T, j *Jobs, id string) Job {
	t.Helper()
	for i := 0; i < 500; i++ {
		if job, ok := j.Get(id); ok && job.Done {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s did not end", id)
	return Job{}
}

func TestUpdaterCheck(t *testing.T) {
	ctx := context.Background()
	h := newUpdater(t)
	u := h.u
	h.plan = update.Plan{Steps: []update.PlanStep{{Name: "panel-binary", Change: true}}, Impact: []string{"Panel birkaç saniye yeniden başlar; siteler etkilenmez."}}

	// The same version: nothing to ready.
	h.srv.publish(t, h.priv, "v0.1.0", []byte("ikili v0.1.0"))
	u.Check(ctx)
	if st := u.Status(ctx, false); st.Newer || st.Ready != nil || st.Latest == nil || st.Latest.Version != "v0.1.0" || st.CheckErr != "" {
		t.Fatalf("current: %+v", st)
	}

	// A newer one: downloaded, checked, planned — once.
	h.srv.publish(t, h.priv, "v0.2.0", []byte("ikili v0.2.0"))
	u.Check(ctx)
	st := u.Status(ctx, false)
	if !st.Newer || st.Ready == nil || st.Ready.Version != "v0.2.0" || len(st.Ready.Plan.Impact) != 1 {
		t.Fatalf("newer: %+v", st)
	}
	if b, err := os.ReadFile(st.Ready.Bin); err != nil || string(b) != "ikili v0.2.0" || st.Ready.Bin != filepath.Join(u.Dir, "v0.2.0", "footops") {
		t.Fatalf("binary %s: %q %v", st.Ready.Bin, b, err)
	}
	u.Check(ctx)
	if h.srv.downloads != 1 {
		t.Errorf("downloaded %d times", h.srv.downloads)
	}
	if p := u.StatusPart(); p == nil || p.Available != "v0.2.0" {
		t.Errorf("status part = %+v", p)
	}

	// A release signed by another key: not installed, a serious alert.
	_, other, _ := ed25519.GenerateKey(nil)
	h.srv.publish(t, other, "v0.3.0", []byte("başka biri"))
	u.Check(ctx)
	st = u.Status(ctx, false)
	a := u.Alerts(time.Now())
	if st.Unverified == "" || len(a) != 1 || a[0].Key != "update:verify" || a[0].Level != "bad" || st.Ready.Version != "v0.2.0" || h.srv.downloads != 1 {
		t.Fatalf("unverified: %+v %+v", st, a)
	}
	// Ours again: the alert goes.
	h.srv.publish(t, h.priv, "v0.2.0", []byte("ikili v0.2.0"))
	u.Check(ctx)
	if a := u.Alerts(time.Now()); len(a) != 0 {
		t.Errorf("alerts after a good check: %+v", a)
	}
	// Unreachable: said, the ready one stays; tried again in two hours.
	h.srv.mu.Lock()
	h.srv.files = nil
	h.srv.mu.Unlock()
	u.Check(ctx)
	if st := u.Status(ctx, false); st.CheckErr == "" || st.Ready == nil || u.nextWake(time.Now()) > updateRetry {
		t.Errorf("unreachable: %+v", st)
	}
}

func TestUpdaterApplyAndSettle(t *testing.T) {
	ctx := context.Background()
	h := newUpdater(t)
	u := h.u
	h.srv.publish(t, h.priv, "v0.2.0", []byte("ikili v0.2.0"))

	// Nothing ready yet.
	if _, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"}); err == nil {
		t.Fatal("applied before a check")
	}
	u.Check(ctx)
	// A plan that needs a terminal is refused.
	setInteractive := func(steps ...string) {
		u.mutate(func(st *updateState) {
			r := *st.Ready
			r.Plan.Interactive = steps
			st.Ready = &r
		})
	}
	setInteractive("SSH ayarları")
	if _, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"}); err == nil || !strings.Contains(err.Error(), "SSH") {
		t.Fatalf("interactive: %v", err)
	}
	setInteractive()
	st := u.state()
	// A binary changed on disk since its check is refused.
	if err := os.WriteFile(st.Ready.Bin, []byte("değişmiş"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"}); err == nil || !strings.Contains(err.Error(), "değişmiş") {
		t.Fatalf("changed binary: %v", err)
	}
	if err := os.WriteFile(st.Ready.Bin, []byte("ikili v0.2.0"), 0o700); err != nil {
		t.Fatal(err)
	}

	// The service stops before restarting the agent (here: the record says done at once).
	h.onStart = func(args []string) {
		job := args[len(args)-1]
		_ = update.WriteRun(u.Dir, update.Run{Job: job, From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Finished: time.Now(), Phase: update.PhaseDone})
	}
	id, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	if job := waitJob(t, u.Jobs, id); job.Err != "" {
		t.Fatalf("job: %+v", job)
	}
	want := update.UnitArgs(st.Ready.Bin, "v0.1.0", "v0.2.0", id, false)
	if strings.Join(h.started[0], " ") != strings.Join(want, " ") {
		t.Errorf("service args:\n%v\nwant\n%v", h.started[0], want)
	}

	// The usual way: the agent restarts under the job. The next one settles it from the record.
	jobsDir := t.TempDir()
	old := &Job{ID: "j1", Kind: "server.update", Target: "v0.2.0", Actor: Actor{User: "furkan"}, Started: time.Now()}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(jobsDir, "j1.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, interrupted, err := LoadJobs(jobsDir)
	if err != nil || len(interrupted) != 1 {
		t.Fatal(interrupted, err)
	}
	u.Jobs = jobs
	_ = update.WriteRun(u.Dir, update.Run{Job: "j1", From: "v0.1.0", To: "v0.2.0", Started: time.Now().Add(-time.Minute), Finished: time.Now(),
		Phase: update.PhaseRolledBack, Err: "v0.2.0 1m30s içinde açılmadı"})
	u.Recover(ctx)
	u.Recover(ctx) // once only
	job, _ := jobs.Get("j1")
	if job.Interrupted || !strings.Contains(job.Err, "önceki sürüm (v0.1.0) yeniden çalışıyor") || !strings.Contains(job.Steps[len(job.Steps)-1], "✗") {
		t.Errorf("settled job: %+v", job)
	}
	// Two updates, two entries: the first ended while its agent lived (told at once), the
	// second by the next agent; newest first.
	entries, _ := u.Audit.Tail(10)
	var got []AuditEntry
	for _, e := range entries {
		if e.Action == "server.update" {
			got = append(got, e)
		}
	}
	if len(got) != 2 || got[0].Actor.User != "furkan" || got[0].Target != "v0.1.0 → v0.2.0" || !strings.Contains(got[0].Detail, "yeniden çalışıyor") || got[1].Detail != "" {
		t.Errorf("audit entries: %+v", got)
	}
	if a := u.Alerts(time.Now()); len(a) != 1 || a[0].Key != "update:failed" || a[0].Level != "warn" {
		t.Errorf("alerts: %+v", a)
	}
	if a := u.Alerts(time.Now().Add(updateFailedFor + time.Hour)); len(a) != 0 {
		t.Errorf("alert after days: %+v", a)
	}

	// An apply cut off with no service left (a reboot under it), the old version running:
	// closed as gone back, with what to look at.
	_ = update.WriteRun(u.Dir, update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Phase: update.PhaseApplying})
	u.Recover(ctx)
	if r, _ := update.ReadRun(u.Dir); r.Phase != update.PhaseRolledBack || !strings.Contains(r.Err, "yarıda kesildi") {
		t.Errorf("cut off: %+v", r)
	}
	// One still running is waited for in a job of its own.
	_ = update.WriteRun(u.Dir, update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Phase: update.PhaseApplying})
	h.mu.Lock()
	h.active = true
	h.mu.Unlock()
	u.Recover(ctx)
	var running Job
	for i := 0; i < 500; i++ { // the waiting job starts in the background
		if j, ok := u.Jobs.Running(); ok {
			running = j
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if running.Kind != "server.update" {
		t.Fatalf("no waiting job: %+v", running)
	}
	if _, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"}); err == nil {
		t.Error("a second update started while one runs")
	}
	_ = update.WriteRun(u.Dir, update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Finished: time.Now(), Phase: update.PhaseDone})
	h.mu.Lock()
	h.active = false
	h.mu.Unlock()
	if job := waitJob(t, u.Jobs, running.ID); job.Err != "" {
		t.Errorf("waiting job: %+v", job)
	}
}

func TestUpdaterBack(t *testing.T) {
	ctx := context.Background()
	h := newUpdater(t)
	u := h.u
	u.Running = "v0.2.0"
	if _, err := u.Back(ctx, Actor{User: "furkan"}); err == nil {
		t.Fatal("went back with nothing kept")
	}
	prev := update.PrevDir(u.Dir)
	if err := os.MkdirAll(prev, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(prev, "footops"), []byte("ikili v0.1.0"), 0o700)
	_ = os.WriteFile(filepath.Join(prev, "version"), []byte("v0.1.0\n"), 0o600)
	if st := u.Status(ctx, false); st.Prev != "v0.1.0" {
		t.Fatalf("prev = %q", st.Prev)
	}
	h.onStart = func(args []string) {
		_ = update.WriteRun(u.Dir, update.Run{Job: args[len(args)-2], Back: true, From: "v0.2.0", To: "v0.1.0", Started: time.Now(), Finished: time.Now(), Phase: update.PhaseDone})
	}
	id, err := u.Back(ctx, Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, u.Jobs, id)
	// The installed binary oversees; the kept one is the target.
	if got := strings.Join(h.started[0], " "); !strings.HasSuffix(got, "--job "+id+" --back") ||
		!strings.Contains(got, "-- "+update.InstalledBin+" self-update --apply --target "+filepath.Join(prev, "footops")+" --from v0.2.0 --to v0.1.0") ||
		!strings.Contains(got, "--property=KillMode=mixed") {
		t.Errorf("args: %s", got)
	}
}

func TestUpdaterRefusedAndRetries(t *testing.T) {
	ctx := context.Background()
	h := newUpdater(t)
	u := h.u
	h.srv.publish(t, h.priv, "v0.2.0", []byte("ikili v0.2.0"))

	// A plan that failed is made again at the next check (the binary is not fetched again).
	planned := 0
	u.Plan = func(context.Context, string) (update.Plan, error) {
		planned++
		if planned == 1 {
			return update.Plan{}, errors.New("RuntimeMaxSec aşıldı")
		}
		return update.Plan{Impact: []string{"Panel birkaç saniye yeniden başlar; siteler etkilenmez."}}, nil
	}
	u.Check(ctx)
	if r := u.state().Ready; r == nil || r.PlanErr == "" {
		t.Fatalf("first plan: %+v", r)
	}
	if p := u.StatusPart(); p == nil || p.Available != "v0.2.0" || p.Ready {
		t.Errorf("status part with a failed plan: %+v", p)
	}
	u.Check(ctx)
	if r := u.state().Ready; r == nil || r.PlanErr != "" || planned != 2 || h.srv.downloads != 1 {
		t.Fatalf("second plan: %+v, planned %d, downloads %d", r, planned, h.srv.downloads)
	}
	if p := u.StatusPart(); p == nil || !p.Ready {
		t.Errorf("status part: %+v", p)
	}

	// A refusal is an end: the job ends with it, the page does not wait for more.
	h.onStart = func(args []string) {
		_ = update.WriteRun(u.Dir, update.Run{Job: args[len(args)-1], From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Finished: time.Now(),
			Phase: update.PhaseRefused, Err: "bu sürümle terminalde soru soran bir ayar değişiyor"})
	}
	id, err := u.Apply(ctx, "v0.2.0", Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, u.Jobs, id)
	if !strings.Contains(job.Err, "hiçbir şey değişmedi") {
		t.Errorf("refused job: %+v", job)
	}
	if st := u.Status(ctx, false); st.Applying || st.Run == nil || !st.Run.Final() {
		t.Errorf("status after a refusal: %+v", st)
	}
	// A refusal is a warning, not a serious alert.
	if a := u.Alerts(time.Now()); len(a) != 1 || a[0].Level != "warn" {
		t.Errorf("alerts: %+v", a)
	}

	// A check that takes long does not undo what was settled meanwhile (the chat would hear twice).
	release1 := make(chan struct{})
	u.Plan = func(context.Context, string) (update.Plan, error) {
		<-release1
		return update.Plan{}, nil
	}
	u.mutate(func(st *updateState) { st.Ready = nil })
	done := make(chan struct{})
	go func() { u.Check(ctx); close(done) }()
	for u.Status(ctx, true).Checking == false {
		time.Sleep(5 * time.Millisecond)
	}
	u.mutate(func(st *updateState) { st.Reported = "settled" })
	close(release1)
	<-done
	if got := u.state().Reported; got != "settled" {
		t.Errorf("reported = %q after a long check", got)
	}
}

func TestUpdaterCutRecords(t *testing.T) {
	ctx := context.Background()
	h := newUpdater(t)
	u := h.u
	installed := filepath.Join(t.TempDir(), "footops")
	u.Installed = installed
	pending := update.PendingDir(u.Dir)
	put := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(installedContent string) {
		_ = os.RemoveAll(update.PrevDir(u.Dir))
		put(filepath.Join(update.PrevDir(u.Dir), "footops"), "ikili v0.0.9")
		put(filepath.Join(update.PrevDir(u.Dir), "version"), "v0.0.9\n")
		put(filepath.Join(pending, "footops"), "ikili v0.1.0")
		put(filepath.Join(pending, "version"), "v0.1.0\n")
		put(installed, installedContent)
	}

	// Cut after the new binary was installed, and the new agent runs: done, and the binary
	// it replaced is the way back.
	setup("ikili v0.2.0")
	_ = update.WriteRun(u.Dir, update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now(), Phase: update.PhaseApplying})
	u.Running = "v0.2.0"
	u.Recover(ctx)
	if r, _ := update.ReadRun(u.Dir); r.Phase != update.PhaseDone || update.PrevVersion(u.Dir) != "v0.1.0" {
		t.Errorf("cut after the swap: %+v, prev %s", r, update.PrevVersion(u.Dir))
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Error("prev.new left")
	}

	// Cut before anything was replaced, the old agent runs: no update, the way back as it was.
	setup("ikili v0.1.0")
	_ = update.WriteRun(u.Dir, update.Run{From: "v0.1.0", To: "v0.2.0", Started: time.Now().Add(time.Second), Phase: update.PhaseApplying})
	u.Running = "v0.1.0"
	u.Recover(ctx)
	if r, _ := update.ReadRun(u.Dir); r.Phase != update.PhaseRolledBack || update.PrevVersion(u.Dir) != "v0.0.9" || !strings.Contains(r.Err, "doctor") {
		t.Errorf("cut before the swap: %+v, prev %s", r, update.PrevVersion(u.Dir))
	}
	if _, err := os.Stat(pending); !os.IsNotExist(err) {
		t.Error("prev.new left")
	}

	// A waiting job a second restart cut off is settled with the first.
	jobsDir := t.TempDir()
	at := time.Now().Add(-time.Minute)
	for i, j := range []*Job{{ID: "ilk", Kind: "server.update", Target: "v0.3.0", Started: at}, {ID: "bekleyen", Kind: "server.update", Target: "v0.3.0", Started: at.Add(10 * time.Second)}} {
		b, _ := json.Marshal(j)
		if err := os.WriteFile(filepath.Join(jobsDir, j.ID+".json"), b, 0o600); err != nil {
			t.Fatal(i, err)
		}
	}
	jobs, _, err := LoadJobs(jobsDir)
	if err != nil {
		t.Fatal(err)
	}
	u.Jobs = jobs
	if got := jobs.InterruptedSince("server.update", at); len(got) != 2 {
		t.Fatalf("interrupted = %v", got)
	}
	_ = update.WriteRun(u.Dir, update.Run{Job: "ilk", From: "v0.2.0", To: "v0.3.0", Started: at, Finished: time.Now(), Phase: update.PhaseDone})
	u.Recover(ctx)
	for _, id := range []string{"ilk", "bekleyen"} {
		if j, _ := jobs.Get(id); j.Interrupted || j.Err != "" {
			t.Errorf("%s: %+v", id, j)
		}
	}
}
