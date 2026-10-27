package staging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testArea(t *testing.T, free, total uint64) *Area {
	t.Helper()
	a := New(filepath.Join(t.TempDir(), "staging"), 10<<30)
	a.disk = func(string) (uint64, uint64, error) { return free, total, nil }
	return a
}

func TestDirIsPrivateAndValidated(t *testing.T) {
	a := testArea(t, 30<<30, 40<<30)
	d, err := a.Dir("job1")
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(d)
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v mode %v", err, st.Mode())
	}
	if again, err := a.Dir("job1"); err != nil || again != d {
		t.Errorf("second Dir: %q %v", again, err)
	}
	for _, bad := range []string{"", "../x", "a/b", ".", strings.Repeat("a", 65)} {
		if _, err := a.Dir(bad); err == nil {
			t.Errorf("Dir(%q) accepted", bad)
		}
		if err := a.Remove(bad); err == nil {
			t.Errorf("Remove(%q) accepted", bad)
		}
	}
}

func TestAllowQuotaAndReserve(t *testing.T) {
	// 40 GB disk, 30 GB free: quota min(10 GB, 7.5 GB) = 7.5 GB.
	a := testArea(t, 30<<30, 40<<30)
	if err := a.Allow(7 << 30); err != nil {
		t.Errorf("7 GB: %v", err)
	}
	if err := a.Allow(8 << 30); err == nil || !strings.Contains(err.Error(), "geçici alan") {
		t.Errorf("8 GB: %v", err)
	}
	// Nearly full disk: 5 GB free of 40; reserve is 4 GB (10%).
	b := testArea(t, 5<<30, 40<<30)
	if err := b.Allow(1 << 30); err != nil {
		t.Errorf("1 GB on a nearly full disk: %v", err)
	}
	if err := b.Allow(1<<30 + 1<<29); err == nil {
		t.Error("1.5 GB accepted although it eats into the quota/reserve")
	}
	if err := checkFree(5<<30, 40<<30, 2<<30); err == nil || !strings.Contains(err.Error(), "diskte yer yok") {
		t.Errorf("reserve not kept: %v", err)
	}
	if err := checkFree(5<<30, 10<<30, -1); err == nil {
		t.Error("negative need accepted")
	}
	if Reserve(10<<30) != 2<<30 || Reserve(100<<30) != 10<<30 {
		t.Error("reserve")
	}
}

func TestSweepRemovesOnlyStale(t *testing.T) {
	a := testArea(t, 30<<30, 40<<30)
	now := time.Now()
	a.now = func() time.Time { return now }
	old, _ := a.Dir("old")
	fresh, _ := a.Dir("fresh")
	resumed, _ := a.Dir("resumed")
	for _, d := range []string{old, fresh, resumed} {
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := now.Add(-MaxAge - time.Hour)
	for _, p := range []string{old, filepath.Join(old, "f"), resumed} {
		_ = os.Chtimes(p, long, long)
	}
	// "resumed": the directory is old but a file inside was just written.
	if got := a.Sweep(); len(got) != 1 || got[0] != "old" {
		t.Errorf("swept %v", got)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh removed")
	}
	if _, err := os.Stat(resumed); err != nil {
		t.Error("resumed upload removed")
	}
	if a.Used() != 2 {
		t.Errorf("used = %d", a.Used())
	}
}

// The first upload on a server comes before any job: the area does not exist yet.
func TestAllowCreatesTheArea(t *testing.T) {
	a := New(filepath.Join(t.TempDir(), "staging"), DefaultMax)
	if err := a.Allow(1); err != nil {
		t.Fatalf("Allow on a missing area: %v", err)
	}
	if st, err := os.Stat(a.Root); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("area: %v %v", st, err)
	}
}
