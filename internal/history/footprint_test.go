package history

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fullWeek returns a store holding a full week for the host and n containers, the most
// the agent ever keeps.
func fullWeek(n int) (*Store, time.Time) {
	s := New()
	t := t0
	m := map[string]Sample{}
	for i := 0; i < 7*24*360; i++ {
		clear(m)
		m[HostKey] = Sample{CPU: float64(i%97) / 3, Mem: 2 << 30, Swap: 100 << 20}
		for c := 0; c < n; c++ {
			m[fmt.Sprintf("container-%02d", c)] = Sample{CPU: float64((i+c)%89) / 7, Mem: uint64(200+c) << 20}
		}
		s.Add(t, m)
		t = t.Add(10 * time.Second)
	}
	return s, t
}

func heapInUse() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

// TestFootprint keeps the agent light: a full week for 16 containers must stay small in
// memory and on disk. Run with -v to see the numbers.
func TestFootprint(t *testing.T) {
	if testing.Short() {
		t.Skip("simulates a week")
	}
	before := heapInUse()
	s, end := fullWeek(16)
	mem := heapInUse() - before

	path := filepath.Join(t.TempDir(), "metrics.json")
	start := time.Now()
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	saveTook := time.Since(start)
	fi, _ := os.Stat(path)

	m := map[string]Sample{HostKey: {CPU: 1, Mem: 1}}
	for c := 0; c < 16; c++ {
		m[fmt.Sprintf("container-%02d", c)] = Sample{CPU: 1, Mem: 1}
	}
	allocs := testing.AllocsPerRun(100, func() { end = end.Add(10 * time.Second); s.Add(end, m) })
	addStart := time.Now()
	for i := 0; i < 1000; i++ {
		end = end.Add(10 * time.Second)
		s.Add(end, m)
	}
	addTook := time.Since(addStart) / 1000

	day, _ := RangeByName("24h")
	qStart := time.Now()
	q := s.Query(day, end)
	qTook := time.Since(qStart)

	t.Logf("17 series, full week: heap %.1f MiB, file %.1f MiB, save %v, Add %v (%.0f allocs), 24h query %v (%d series)",
		float64(mem)/(1<<20), float64(fi.Size())/(1<<20), saveTook.Round(time.Millisecond), addTook, allocs, qTook.Round(time.Microsecond), len(q))
	if mem > 8<<20 || fi.Size() > 4<<20 {
		t.Errorf("history too big: heap %d, file %d bytes", mem, fi.Size())
	}
}
