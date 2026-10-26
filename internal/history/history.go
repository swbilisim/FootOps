// Package history keeps a week of host and container metrics: one point per minute for
// the last day and one per hour for the last week. It answers "why did memory grow?"
// without a database; the agent saves it to a file now and then, so a restart loses at
// most a few minutes.
package history

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// HostKey is the series of the server itself; container names never start with "@".
	HostKey = "@host"

	minuteKeep = 24 * time.Hour
	hourKeep   = 7 * 24 * time.Hour
)

// Sample is one reading. Swap is only set for the host.
type Sample struct {
	CPU  float64 // percent; containers can exceed 100 on multi-core hosts
	Mem  uint64  // bytes
	Swap uint64  // bytes
}

// Point summarises an interval: averages and peaks. The JSON keys are short because a
// week of points is saved to disk and sent to the web process.
type Point struct {
	T      int64   `json:"t"` // unix seconds, start of the interval
	CPU    float64 `json:"c"`
	CPUMax float64 `json:"cx"`
	Mem    uint64  `json:"m"`
	MemMax uint64  `json:"mx"`
	Swap   uint64  `json:"s,omitempty"` // peak
}

// acc accumulates readings (or finer points) for one interval.
type acc struct {
	Start  int64   `json:"t"`
	N      int     `json:"n"`
	CPUSum float64 `json:"cs"`
	CPUMax float64 `json:"cx"`
	MemSum float64 `json:"ms"`
	MemMax uint64  `json:"mx"`
	Swap   uint64  `json:"s"`
}

func (a *acc) add(start int64, cpu, cpuMax float64, mem, memMax, swap uint64) {
	if a.N == 0 {
		*a = acc{Start: start}
	}
	a.N++
	a.CPUSum += cpu
	a.CPUMax = max(a.CPUMax, cpuMax)
	a.MemSum += float64(mem)
	a.MemMax = max(a.MemMax, memMax)
	a.Swap = max(a.Swap, swap)
}

func (a *acc) point() Point {
	return Point{T: a.Start, CPU: round2(a.CPUSum / float64(a.N)), CPUMax: round2(a.CPUMax),
		Mem: uint64(a.MemSum / float64(a.N)), MemMax: a.MemMax, Swap: a.Swap}
}

// round2 keeps two decimals: more is noise, and it keeps the saved file small.
func round2(v float64) float64 { return math.Round(v*100) / 100 }

type series struct {
	Minutes []Point `json:"min"`
	Hours   []Point `json:"hour"`
	Minute  acc     `json:"cur_min"`  // the minute being filled
	Hour    acc     `json:"cur_hour"` // the hour being filled, from finished minutes
}

// Store is safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	series map[string]*series
}

func New() *Store { return &Store{series: map[string]*series{}} }

// Add records one round of readings taken at t. Series missing from a round simply get
// a gap (a stopped container).
func (s *Store) Add(t time.Time, samples map[string]Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := t.Unix()
	minute := now - now%60
	hour := now - now%3600
	// Close intervals that have ended, including those of series without a reading now.
	for _, se := range s.series {
		if se.Minute.N > 0 && se.Minute.Start < minute {
			se.flushMinute()
		}
		if se.Hour.N > 0 && se.Hour.Start < hour {
			se.Hours = append(se.Hours, se.Hour.point())
			se.Hour = acc{}
		}
	}
	for name, v := range samples {
		se := s.series[name]
		if se == nil {
			se = &series{}
			s.series[name] = se
		}
		se.Minute.add(minute, v.CPU, v.CPU, v.Mem, v.Mem, v.Swap)
	}
	s.prune(t)
}

func (se *series) flushMinute() {
	p := se.Minute.point()
	se.Minute = acc{}
	se.Minutes = append(se.Minutes, p)
	h := p.T - p.T%3600
	if se.Hour.N > 0 && se.Hour.Start != h {
		se.Hours = append(se.Hours, se.Hour.point())
		se.Hour = acc{}
	}
	se.Hour.add(h, p.CPU, p.CPUMax, p.Mem, p.MemMax, p.Swap)
}

func (s *Store) prune(t time.Time) {
	minCut := t.Add(-minuteKeep).Unix()
	hourCut := t.Add(-hourKeep).Unix()
	for name, se := range s.series {
		se.Minutes = dropBefore(se.Minutes, minCut)
		se.Hours = dropBefore(se.Hours, hourCut)
		if len(se.Minutes) == 0 && len(se.Hours) == 0 && se.Minute.N == 0 && se.Hour.N == 0 {
			delete(s.series, name)
		}
	}
}

// dropBefore removes old points in place, so the arrays are reused instead of
// reallocated every minute.
func dropBefore(ps []Point, cut int64) []Point {
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= cut })
	if i == 0 {
		return ps
	}
	n := copy(ps, ps[i:])
	return ps[:n]
}

// Range is a time window and the width of each point shown for it.
type Range struct {
	Name string
	Span time.Duration
	Step time.Duration
}

// Ranges are the windows the panel offers; each yields at most a few hundred points.
var Ranges = []Range{
	{"1h", time.Hour, time.Minute},
	{"6h", 6 * time.Hour, 2 * time.Minute},
	{"24h", 24 * time.Hour, 10 * time.Minute},
	{"7d", 7 * 24 * time.Hour, time.Hour},
}

// RangeByName returns the named range, or the 24-hour one.
func RangeByName(name string) (Range, bool) {
	for _, r := range Ranges {
		if r.Name == name {
			return r, true
		}
	}
	return Ranges[2], false
}

// Query returns points of r.Step width covering [now-r.Span, now) for the named series,
// or for every series when names is empty. Series without points in the window are
// left out. The minute or hour still being filled is not included.
func (s *Store) Query(r Range, now time.Time, names ...string) map[string][]Point {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := now.Add(-r.Span).Unix()
	step := int64(r.Step / time.Second)
	from -= from % step
	out := map[string][]Point{}
	for name, se := range s.series {
		if len(names) > 0 && !contains(names, name) {
			continue
		}
		var src []Point
		if r.Step >= time.Hour {
			src = append(src, se.Hours...)
			// Finished minutes of the current hour are not in Hours yet.
			var next int64
			if len(se.Hours) > 0 {
				next = se.Hours[len(se.Hours)-1].T + 3600
			}
			for _, p := range se.Minutes {
				if p.T >= next {
					src = append(src, p)
				}
			}
		} else {
			src = se.Minutes
		}
		if ps := Downsample(src, from, step); len(ps) > 0 {
			out[name] = ps
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Downsample merges sorted points at or after from into step-second buckets: averages
// are averaged, peaks keep the highest value.
func Downsample(ps []Point, from, step int64) []Point {
	var out []Point
	var a acc
	for _, p := range ps {
		if p.T < from {
			continue
		}
		b := p.T - p.T%step
		if a.N > 0 && a.Start != b {
			out = append(out, a.point())
			a = acc{}
		}
		a.add(b, p.CPU, p.CPUMax, p.Mem, p.MemMax, p.Swap)
	}
	if a.N > 0 {
		out = append(out, a.point())
	}
	return out
}

// Save writes the store to path atomically.
func (s *Store) Save(path string) error {
	s.mu.Lock()
	b, err := json.Marshal(s.series)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Load reads a store saved by Save. A missing file gives an empty store; a damaged one
// gives an empty store and the error, so the agent can log it and carry on.
func Load(path string, now time.Time) (*Store, error) {
	s := New()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var m map[string]*series
	if err := json.Unmarshal(b, &m); err != nil {
		return s, err
	}
	for k, v := range m {
		if v != nil {
			s.series[k] = v
		}
	}
	s.prune(now)
	return s, nil
}
