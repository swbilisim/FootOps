package agent

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
)

// cgroupDocker is dockerd for the sampler: one running site, whose process (pid) the
// test changes to start it again, and a count of the stats requests it gets.
type cgroupDocker struct {
	fakeDocker
	mu      sync.Mutex
	pid     int
	started time.Time
	stats   int
}

func (d *cgroupDocker) Containers(context.Context) ([]docker.Container, error) {
	return []docker.Container{{ID: "c0ffee", Names: []string{"/magaza"}, Image: "ghcr.io/x/magaza:main", State: "running",
		Labels: map[string]string{"com.docker.compose.project": "magaza"}}}, nil
}

func (d *cgroupDocker) Inspect(context.Context, string) (docker.Inspect, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var in docker.Inspect
	in.ID, in.State.Running, in.State.Pid = "c0ffee", true, d.pid
	in.State.StartedAt = d.started.UTC().Format(time.RFC3339Nano)
	return in, nil
}

func (d *cgroupDocker) Stats(context.Context, string) (docker.Stats, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stats++
	return docker.Stats{CPUPercent: 3.5, MemUsage: 300 << 20, MemLimit: 4 << 30}, nil
}

// A running container's CPU and memory come from its cgroup, not from dockerd: memory
// less the inactive page cache, CPU as the share of one core between two samples.
func TestSamplerReadsCgroups(t *testing.T) {
	dir := t.TempDir()
	write := func(usec int, max string) {
		t.Helper()
		for name, text := range map[string]string{
			"cpu.stat":       "usage_usec " + strconv.Itoa(usec) + "\nuser_usec 1\n",
			"memory.current": "209715200\n", // 200 MB, 50 of it inactive page cache
			"memory.stat":    "anon 1\ninactive_file 52428800\n",
			"memory.max":     max + "\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Date(2026, 11, 10, 14, 0, 0, 0, time.UTC)
	d := &cgroupDocker{pid: 4242, started: now.Add(-time.Hour)}
	s := NewSampler(d, fakeSystem)
	asked := []int{}
	s.cgroupOf = func(pid int) string { asked = append(asked, pid); return dir }
	s.now = func() time.Time { return now }

	write(5_000_000, "536870912")
	c := s.Sample(context.Background()).Containers[0]
	if c.CPUPercent != 0 || c.MemUsage != 150<<20 || c.MemLimit != 512<<20 || d.stats != 0 {
		t.Fatalf("first sample: %+v, %d stats requests", c, d.stats)
	}
	// 10 s later, 2.5 s more CPU: a quarter of a core.
	now = now.Add(10 * time.Second)
	write(7_500_000, "536870912")
	if c := s.Sample(context.Background()).Containers[0]; math.Abs(c.CPUPercent-25) > 1e-9 {
		t.Errorf("cpu = %v, want 25", c.CPUPercent)
	}
	// A sample a moment later (after a job) says the same share, and does not move the
	// mark: the next share spans the whole time since it.
	now = now.Add(300 * time.Millisecond)
	write(7_600_000, "536870912")
	if c := s.Sample(context.Background()).Containers[0]; math.Abs(c.CPUPercent-25) > 1e-9 {
		t.Errorf("short window: cpu = %v, want 25", c.CPUPercent)
	}
	now = now.Add(9700 * time.Millisecond)
	write(12_500_000, "536870912")
	if c := s.Sample(context.Background()).Containers[0]; math.Abs(c.CPUPercent-50) > 1e-9 {
		t.Errorf("after a short window: cpu = %v, want 50 (5 s over the 10 s since the mark)", c.CPUPercent)
	}
	// Two and a half cores' worth: more than 100, as docker stats would say.
	now = now.Add(10 * time.Second)
	write(37_500_000, "max")
	c = s.Sample(context.Background()).Containers[0]
	if math.Abs(c.CPUPercent-250) > 1e-9 || c.MemLimit != 4<<30 { // no limit: the server's memory
		t.Errorf("busy, unlimited: %+v", c)
	}
	if len(asked) != 1 || d.stats != 0 {
		t.Errorf("cgroup looked up %v times; %d stats requests", asked, d.stats)
	}

	// Started again 4 s ago, with 1 s of CPU since: its count started again too.
	now = now.Add(10 * time.Second)
	d.mu.Lock()
	d.pid, d.started = 5151, now.Add(-4*time.Second)
	d.mu.Unlock()
	write(1_000_000, "536870912")
	if c := s.Sample(context.Background()).Containers[0]; math.Abs(c.CPUPercent-25) > 1e-9 {
		t.Errorf("after a restart: cpu = %v, want 25", c.CPUPercent)
	}
	if len(asked) != 2 || asked[1] != 5151 {
		t.Errorf("new process's cgroup not looked up: %v", asked)
	}

	// The cgroup cannot be read (gone, or no cgroup v2): dockerd is asked as before.
	os.Remove(filepath.Join(dir, "cpu.stat"))
	now = now.Add(10 * time.Second)
	if c := s.Sample(context.Background()).Containers[0]; c.CPUPercent != 3.5 || d.stats != 1 {
		t.Errorf("fallback: %+v, %d stats requests", c, d.stats)
	}
	s.cgroupOf = func(int) string { return "" }
	s.marks = map[string]cpuMark{}
	if c := s.Sample(context.Background()).Containers[0]; c.CPUPercent != 3.5 || d.stats != 2 {
		t.Errorf("no cgroup: %+v, %d stats requests", c, d.stats)
	}

	// A container not seen for 10 minutes is forgotten.
	s.marks["gone"] = cpuMark{at: now.Add(-11 * time.Minute)}
	now = now.Add(10 * time.Second)
	s.Sample(context.Background())
	if _, ok := s.marks["gone"]; ok {
		t.Error("an old reading kept")
	}
}

// The cgroup counted is the container's own scope, even when its process sits in a
// child cgroup (systemd inside the container moves it there).
func TestContainerCgroup(t *testing.T) {
	root := t.TempDir()
	old := run.CgroupRoot
	run.CgroupRoot = root
	defer func() { run.CgroupRoot = old }()
	id := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	scope := root + "/system.slice/docker-" + id + ".scope"
	for dir, want := range map[string]string{
		scope:                    scope,
		scope + "/init.scope":    scope,
		scope + "/a/b":           scope,
		root + "/system.slice/x": root + "/system.slice/x", // nothing named after it: as found
		"/elsewhere/" + id:       "/elsewhere/" + id,       // not under the cgroup root
		"":                       "",
	} {
		if got := containerCgroup(dir, id); got != want {
			t.Errorf("%q: %q, want %q", dir, got, want)
		}
	}
}
