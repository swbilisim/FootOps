package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// trialEnv is newY3 with the night's parts recorded as the latest backups, and the
// site's live files as the night took them (same content, same modification time).
func trialEnv(t *testing.T) *y3Env {
	t.Helper()
	e := newY3(t)
	night := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	e.b.updateState(func(st *backupState) {
		st.Last = map[string]BackupLast{
			"magaza/db":    {Time: night, Snapshot: snapID(1), Run: "RUN1"},
			"magaza/files": {Time: night, Snapshot: snapID(2), Run: "RUN1"},
		}
	})
	data := filepath.Join(e.root, "srv/musteri/magaza/data")
	snap, _ := e.restic.find(snapID(2))
	for _, n := range snap.Tree {
		p := filepath.Join(data, n.Path)
		switch {
		case n.Dir:
			os.MkdirAll(p, 0o750)
		case n.Link != "":
		default:
			os.WriteFile(p, []byte(n.Content), 0o644)
			os.Chtimes(p, snap.Time, snap.Time)
		}
	}
	e.b.trialEvery = 0                  // the real interval
	e.b.trialPick = func(n int) []int { // in order
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	return e
}

func TestTrialRestore(t *testing.T) {
	e := trialEnv(t)
	tk := e.task()
	res, err := e.b.Trial(context.Background(), tk, "magaza")
	if err != nil {
		t.Fatalf("trial: %v\n%s", err, steps(tk))
	}
	if !res.OK || !strings.Contains(res.Detail, "3 koleksiyon / 10 belge aynı") || !strings.Contains(res.Detail, "3 dosyanın 3 tanesi aynı") {
		t.Errorf("result %+v\n%s", res, steps(tk))
	}
	st := e.b.state()
	if st.Trial == nil || !st.Trial.OK || st.Trial.Site != "magaza" || st.Trial.Job != tk.ID() {
		t.Errorf("state %+v", st.Trial)
	}
	// The temporary database is gone, the site's own untouched, and the repository
	// was only read.
	for db := range e.mongo.dbs {
		if strings.HasPrefix(db, tmpDBPrefix) {
			t.Errorf("temporary database left: %s", db)
		}
	}
	if got := e.mongo.get("magaza"); got["urunler"] != 3 {
		t.Errorf("site's database changed: %v", got)
	}
	for _, c := range e.restic.calls {
		if !contains(c, "--no-lock") {
			t.Errorf("restic without --no-lock: %v", c)
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(e.root, "staging")); len(ents) != 0 {
		t.Errorf("staging left: %v", ents)
	}
	if a := e.b.Alerts(e.now); strings.Contains(alertKeys(a), "backup:trial") {
		t.Errorf("alert after a clean trial: %s", alertKeys(a))
	}
}

func TestTrialFindsTrouble(t *testing.T) {
	// A live file unchanged since the backup, yet different from what comes back.
	e := trialEnv(t)
	p := filepath.Join(e.root, "srv/musteri/magaza/data/a.txt")
	snap, _ := e.restic.find(snapID(2))
	os.WriteFile(p, []byte("MERHABA"), 0o644) // same size as "merhaba"
	os.Chtimes(p, snap.Time, snap.Time)
	if _, err := e.b.Trial(context.Background(), e.task(), "magaza"); err == nil || !strings.Contains(err.Error(), "a.txt yedekte canlıdakinden farklı") {
		t.Fatalf("err = %v", err)
	}
	st := e.b.state()
	if st.Trial == nil || st.Trial.OK || !strings.Contains(st.Trial.Err, "farklı") {
		t.Errorf("state %+v", st.Trial)
	}
	if a := e.b.Alerts(e.now); !strings.Contains(alertKeys(a), "backup:trial") {
		t.Errorf("no alert: %s", alertKeys(a))
	}

	// Changed since (another time): only its size is checked.
	os.Chtimes(p, snap.Time.Add(time.Hour), snap.Time.Add(time.Hour))
	tk := e.task()
	res, err := e.b.Trial(context.Background(), tk, "magaza")
	if err != nil || !strings.Contains(res.Detail, "3 dosyanın 2 tanesi aynı (1 tanesi yedekten beri değiştiği için yalnız boyutu)") {
		t.Errorf("changed file: %+v %v\n%s", res, err, steps(tk))
	}
	if a := e.b.Alerts(e.now); strings.Contains(alertKeys(a), "backup:trial") {
		t.Error("the alert stayed after a clean trial")
	}

	// The counts do not add up with mongodump's.
	e = trialEnv(t)
	rec, _ := e.b.runRecord("RUN1")
	rec.Parts[0].Collections["urunler"] = 6
	e.b.saveRun(rec)
	if _, err := e.b.Trial(context.Background(), e.task(), "magaza"); err == nil || !strings.Contains(err.Error(), "urunler: kayıtta 6, yüklenen 5") {
		t.Errorf("counts: %v", err)
	}
	// What mongorestore writes is not what it says.
	e = trialEnv(t)
	e.mongo.lieNext = true
	if _, err := e.b.Trial(context.Background(), e.task(), "magaza"); err == nil {
		t.Error("a short restore passed")
	}
	// The snapshot is gone from the repository.
	e = trialEnv(t)
	e.restic.snaps = e.restic.snaps[1:]
	if _, err := e.b.Trial(context.Background(), e.task(), "magaza"); err == nil {
		t.Error("a missing snapshot passed")
	}
}

func TestTrialCancelledLeavesNoFinding(t *testing.T) {
	e := trialEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.b.Trial(ctx, e.task(), "magaza"); err == nil {
		t.Fatal("cancelled trial passed")
	}
	if st := e.b.state(); st.Trial != nil {
		t.Errorf("a cancelled trial was recorded: %+v", st.Trial)
	}
}

func TestTrialTurnsAndTiming(t *testing.T) {
	e := trialEnv(t)
	zeta := specFor("zeta")
	zeta.Domains = []string{"zeta.enisparta.com"}
	if err := e.data.Store.WriteMeta(zeta); err != nil {
		t.Fatal(err)
	}
	other := specFor("ara") // no backup yet: not its turn
	other.Domains = []string{"ara.enisparta.com"}
	if err := e.data.Store.WriteMeta(other); err != nil {
		t.Fatal(err)
	}
	e.b.updateState(func(st *backupState) { st.Last["zeta/db"] = BackupLast{Time: e.now, Snapshot: snapID(9), Run: "RUN9"} })
	st := e.b.state()
	if got := e.b.trialSite(st); got != "magaza" {
		t.Errorf("first: %s", got)
	}
	st.Trial = &BackupTrial{Site: "magaza"}
	if got := e.b.trialSite(st); got != "zeta" {
		t.Errorf("after magaza: %s", got)
	}
	st.Trial = &BackupTrial{Site: "zeta"}
	if got := e.b.trialSite(st); got != "magaza" {
		t.Errorf("after zeta: %s", got)
	}

	if !e.b.trialDue(e.now) {
		t.Error("never tried: not due")
	}
	e.b.updateState(func(st *backupState) {
		st.Trial = &BackupTrial{Time: e.now.Add(-10 * 24 * time.Hour), Site: "magaza", OK: true}
	})
	if e.b.trialDue(e.now) {
		t.Error("due after 10 days")
	}
	e.b.updateState(func(st *backupState) { st.Trial.Time = e.now.Add(-29 * 24 * time.Hour) })
	if !e.b.trialDue(e.now) {
		t.Error("not due after 29 days")
	}
	// A night that ended with an error starts nothing; a clean one starts the trial.
	e.b.afterNight(Job{Err: "1 işlem başarısız"})
	if e.b.Jobs.Busy() {
		t.Fatal("started after a failed night")
	}
	e.b.afterNight(Job{})
	j, ok := e.b.Jobs.Running()
	if !ok || j.Kind != "backup.trial" || j.Target != "zeta" {
		t.Errorf("after a clean night: %+v %v", j, ok)
	}
	for e.b.Jobs.Busy() {
		time.Sleep(5 * time.Millisecond)
	}
	// In a rehearsal nothing is due and nothing starts.
	e.b.Rehearsal = &Rehearsal{Path: filepath.Join(e.root, "rehearsal.json"), Run: &fakeIptables{}}
	e.b.Rehearsal.Begin(context.Background(), RehearsalInfo{})
	e.b.updateState(func(st *backupState) { st.Trial = nil })
	if e.b.trialDue(e.now) {
		t.Error("due in a rehearsal")
	}
	if _, err := e.b.StartTrial("", Actor{}); err == nil {
		t.Error("started in a rehearsal")
	}
}

func TestTrialSample(t *testing.T) {
	b := &Backups{trialPick: func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = n - 1 - i
		}
		return out
	}}
	var nodes []trialNode
	for i := 0; i < 30; i++ {
		nodes = append(nodes, trialNode{Path: string(rune('a' + i%26)), Size: int64(i)})
	}
	nodes = append(nodes, trialNode{Path: "big", Size: trialFileMax + 1})
	got := b.trialSample(nodes)
	if len(got) != trialSample {
		t.Fatalf("%d picked", len(got))
	}
	for _, n := range got {
		if n.Size > trialFileMax {
			t.Error("a large file picked while small ones were there")
		}
	}
	if got := b.trialSample([]trialNode{{Path: "big", Size: trialFileMax + 1}}); len(got) != 1 {
		t.Errorf("only a large file: %v", got)
	}
	if got := b.trialSample(nil); len(got) != 0 {
		t.Errorf("no files: %v", got)
	}
}

func TestTrialOnlyWhatCanBeTried(t *testing.T) {
	e := trialEnv(t)
	other := specFor("bos")
	other.Domains = []string{"bos.enisparta.com"}
	if err := e.data.Store.WriteMeta(other); err != nil {
		t.Fatal(err)
	}
	if _, err := e.b.StartTrial("bos", Actor{}); err == nil || !strings.Contains(err.Error(), "denenecek bir yedeği yok") {
		t.Errorf("a site without backups: %v", err)
	}
	if e.b.Jobs.Busy() || e.b.state().Trial != nil {
		t.Error("recorded or started")
	}
	// Backups turned off: not its turn, and not tried when asked for.
	s, _ := e.b.Settings()
	s.Sites = map[string]BackupPolicy{"magaza": {Off: true}}
	if err := e.b.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	if got := e.b.trialSite(e.b.state()); got != "" {
		t.Errorf("turn of a site with backups off: %q", got)
	}
	if _, err := e.b.StartTrial("magaza", Actor{}); err == nil {
		t.Error("tried a site with backups off")
	}
}
