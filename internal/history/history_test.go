package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)

// feed adds one reading every 10 seconds from start for d, using f for the values.
func feed(s *Store, start time.Time, d time.Duration, f func(time.Time) map[string]Sample) time.Time {
	t := start
	for ; t.Before(start.Add(d)); t = t.Add(10 * time.Second) {
		s.Add(t, f(t))
	}
	return t
}

func TestMinuteAveragesAndPeaks(t *testing.T) {
	s := New()
	i := 0
	end := feed(s, t0, 2*time.Minute, func(time.Time) map[string]Sample {
		i++
		return map[string]Sample{HostKey: {CPU: float64(i), Mem: uint64(i) * 100, Swap: uint64(i)}}
	})
	s.Add(end, nil) // closes the second minute
	got := s.Query(Ranges[0], end)[HostKey]
	if len(got) != 2 {
		t.Fatalf("points = %d, want 2: %+v", len(got), got)
	}
	// First minute: readings 1..6.
	if p := got[0]; p.T != t0.Unix() || p.CPU != 3.5 || p.CPUMax != 6 || p.Mem != 350 || p.MemMax != 600 || p.Swap != 6 {
		t.Errorf("first minute = %+v", p)
	}
	if p := got[1]; p.T != t0.Add(time.Minute).Unix() || p.CPUMax != 12 {
		t.Errorf("second minute = %+v", p)
	}
}

func TestHoursAndGaps(t *testing.T) {
	s := New()
	end := feed(s, t0, 3*time.Hour, func(t time.Time) map[string]Sample {
		m := map[string]Sample{HostKey: {CPU: 10, Mem: 1000}}
		if t.Before(t0.Add(90 * time.Minute)) { // the container stops half way through hour 2
			m["web"] = Sample{CPU: 50, Mem: 2000}
		}
		return m
	})
	week, _ := RangeByName("7d")
	got := s.Query(week, end)
	if n := len(got[HostKey]); n != 3 {
		t.Errorf("host hours = %d, want 3", n)
	}
	web := got["web"]
	if len(web) != 2 || web[1].T != t0.Add(time.Hour).Unix() || web[1].Mem != 2000 {
		t.Errorf("web hours = %+v", web)
	}

	// Minutes of the unfinished hour show up in the weekly view too.
	end = feed(s, end, 20*time.Minute, func(time.Time) map[string]Sample {
		return map[string]Sample{HostKey: {CPU: 30, Mem: 3000}}
	})
	got = s.Query(week, end)
	if n := len(got[HostKey]); n != 4 || got[HostKey][3].CPU != 30 {
		t.Errorf("with partial hour: %+v", got[HostKey])
	}

	day, _ := RangeByName("24h")
	got = s.Query(day, end, "web")
	if len(got) != 1 || len(got["web"]) != 9 { // 90 minutes in 10-minute steps
		t.Errorf("24h web = %d points (%d series)", len(got["web"]), len(got))
	}
}

func TestPruneDropsOldSeries(t *testing.T) {
	s := New()
	end := feed(s, t0, 5*time.Minute, func(time.Time) map[string]Sample {
		return map[string]Sample{"old": {CPU: 1}}
	})
	s.Add(end.Add(8*24*time.Hour), map[string]Sample{HostKey: {CPU: 1}})
	if _, ok := s.series["old"]; ok {
		t.Error("series older than a week was kept")
	}
}

func TestSaveLoad(t *testing.T) {
	s := New()
	end := feed(s, t0, 75*time.Minute, func(time.Time) map[string]Sample {
		return map[string]Sample{HostKey: {CPU: 20, Mem: 500}}
	})
	path := filepath.Join(t.TempDir(), "metrics.json")
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	l, err := Load(path, end)
	if err != nil {
		t.Fatal(err)
	}
	// The hour in progress survives the restart and is completed afterwards.
	end = feed(l, end, time.Hour, func(time.Time) map[string]Sample {
		return map[string]Sample{HostKey: {CPU: 20, Mem: 500}}
	})
	week, _ := RangeByName("7d")
	got := l.Query(week, end)[HostKey]
	if len(got) != 3 || got[1].T != t0.Add(time.Hour).Unix() || got[1].CPU != 20 {
		t.Errorf("after reload: %+v", got)
	}

	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := Load(path, end); err == nil || l == nil {
		t.Error("damaged file: want an empty store and an error")
	}
	if l, err := Load(filepath.Join(t.TempDir(), "none.json"), end); err != nil || l == nil {
		t.Errorf("missing file: %v", err)
	}
}

func TestDownsample(t *testing.T) {
	ps := []Point{
		{T: 0, CPU: 10, CPUMax: 20, Mem: 100, MemMax: 150},
		{T: 60, CPU: 30, CPUMax: 90, Mem: 300, MemMax: 400},
		{T: 120, CPU: 5, CPUMax: 5, Mem: 50, MemMax: 50},
	}
	got := Downsample(ps, 0, 120)
	if len(got) != 2 || got[0].CPU != 20 || got[0].CPUMax != 90 || got[0].Mem != 200 || got[0].MemMax != 400 {
		t.Errorf("got %+v", got)
	}
	if got := Downsample(ps, 60, 60); len(got) != 2 {
		t.Errorf("from is not applied: %+v", got)
	}
}
