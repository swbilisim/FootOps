package agent

import (
	"context"
	"errors"
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
	mu       sync.Mutex
	state    string // the list's State and Status ("" and "": running, up for hours)
	status   string
	pid      int
	restarts int
	started  time.Time
	stats    int
	inspects int
	says     string // the State an inspect answers, when not the list's (a change in between)
	fail     error  // what an inspect answers instead
}

func (d *cgroupDocker) Containers(context.Context) ([]docker.Container, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state, status := "running", "Up 2 hours"
	if d.state != "" {
		state = d.state
	}
	if d.status != "" {
		status = d.status
	}
	return []docker.Container{{ID: "c0ffee", Names: []string{"/magaza"}, Image: "ghcr.io/x/magaza:main", State: state, Status: status,
		Labels: map[string]string{"com.docker.compose.project": "magaza"}}}, nil
}

func (d *cgroupDocker) Inspect(context.Context, string) (docker.Inspect, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inspects++
	var in docker.Inspect
	if d.fail != nil {
		return in, d.fail
	}
	in.ID, in.State.Running, in.State.Pid, in.RestartCount = "c0ffee", d.state == "" || d.state == "running", d.pid, d.restarts
	in.State.Status = d.state
	if in.State.Status == "" {
		in.State.Status = "running"
	}
	if d.says != "" {
		in.State.Status = d.says
	}
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
	d.pid, d.started, d.status = 5151, now.Add(-4*time.Second), "Up 4 seconds"
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

// dockerd is asked about a container only when its list entry changes (a restart, a
// health change, an exit), while it is younger than a minute, or every inspectAge.
func TestSamplerInspectsOnChange(t *testing.T) {
	now := time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)
	d := &cgroupDocker{pid: 4242, started: now.Add(-2 * time.Hour)}
	s := NewSampler(d, fakeSystem)
	s.cgroupOf = func(int) string { return "" } // no cgroup: dockerd's stats, beside the point here
	s.now = func() time.Time { return now }
	sample := func(status string, after time.Duration) ContainerInfo {
		t.Helper()
		now = now.Add(after)
		d.mu.Lock()
		d.status = status
		d.mu.Unlock()
		return s.Sample(context.Background()).Containers[0]
	}
	inspects := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.inspects }

	sample("Up 2 hours", 0)
	for i := 0; i < 5; i++ {
		sample("Up 2 hours", 10*time.Second)
	}
	if n := inspects(); n != 1 {
		t.Fatalf("unchanged: %d inspects in 6 samples, want 1", n)
	}
	// Started again: "Up 3 seconds", and asked every time while under a minute.
	d.mu.Lock()
	d.restarts, d.pid = 1, 5151
	d.mu.Unlock()
	if c := sample("Up 3 seconds", 10*time.Second); c.RestartCount != 1 || inspects() != 2 {
		t.Errorf("restart: %+v, %d inspects", c, inspects())
	}
	sample("Up 13 seconds", 10*time.Second)
	sample("Up 13 seconds", 0) // the same words a moment later: still under a minute
	if n := inspects(); n != 4 {
		t.Errorf("young: %d inspects, want 4", n)
	}
	sample("Up About a minute", 50*time.Second)
	sample("Up About a minute", 10*time.Second)
	if n := inspects(); n != 5 {
		t.Errorf("a minute up: %d inspects, want 5", n)
	}
	// A health change, an exit: asked.
	sample("Up About a minute (unhealthy)", 10*time.Second)
	d.mu.Lock()
	d.state = "exited"
	d.mu.Unlock()
	if c := sample("Exited (137) 1 second ago", 10*time.Second); c.State != "exited" || inspects() != 7 {
		t.Errorf("exit: %+v, %d inspects", c, inspects())
	}
	d.mu.Lock()
	d.state = "running"
	d.mu.Unlock()
	sample("Up 2 minutes", 10*time.Second)
	// Unchanged for inspectAge: asked again all the same.
	n := inspects()
	sample("Up 2 minutes", inspectAge-time.Second)
	if inspects() != n {
		t.Errorf("before inspectAge: asked")
	}
	sample("Up 2 minutes", 2*time.Second)
	if inspects() != n+1 {
		t.Errorf("after inspectAge: not asked")
	}
	// A container gone for 10 minutes is forgotten.
	s.inspects["gone"] = inspectMark{at: now.Add(-11 * time.Minute)}
	sample("Up 2 minutes", 10*time.Second)
	if _, ok := s.inspects["gone"]; ok {
		t.Error("an old inspect kept")
	}

	// An answer that does not tell what the list entry does (a change in between) is
	// not kept, nor is a failed one.
	n = inspects()
	d.mu.Lock()
	d.says = "restarting"
	d.mu.Unlock()
	sample("Up 3 minutes", 10*time.Second)
	sample("Up 3 minutes", 10*time.Second)
	if inspects() != n+2 {
		t.Errorf("an answer at odds with the list kept: %d inspects, want %d", inspects(), n+2)
	}
	d.mu.Lock()
	d.says, d.fail = "", errors.New("dockerd took too long")
	d.mu.Unlock()
	if o := s.Sample(context.Background()); len(o.Errors) != 1 {
		t.Errorf("failed inspect not said: %v", o.Errors)
	}
	d.mu.Lock()
	d.fail = nil
	d.mu.Unlock()
	n = inspects()
	sample("Up 3 minutes", 10*time.Second)
	sample("Up 3 minutes", 10*time.Second)
	if inspects() != n+1 {
		t.Errorf("after a failure: %d inspects, want %d", inspects(), n+1)
	}
}

func TestHealthShown(t *testing.T) {
	health := func(h string) docker.Inspect {
		var in docker.Inspect
		if h != "" {
			in.State.Health = &struct {
				Status string `json:"Status"`
			}{h}
		}
		return in
	}
	for _, tc := range []struct {
		health, status string
		want           bool
	}{
		{"", "Up 2 hours", true},
		{"", "Up 2 hours (healthy)", false},
		{"healthy", "Up 2 hours (healthy)", true},
		{"healthy", "Up 2 hours (unhealthy)", false},
		{"unhealthy", "Up 2 hours (unhealthy)", true},
		{"unhealthy", "Up 2 hours (healthy)", false},
		{"starting", "Up 5 seconds (health: starting)", true},
		{"starting", "Up 5 seconds (healthy)", false},
	} {
		if got := healthShown(health(tc.health), tc.status); got != tc.want {
			t.Errorf("%q / %q: %v", tc.health, tc.status, got)
		}
	}
}

// What the list does not show, the cgroup does: a process killed for memory while the
// container runs on (dockerd marks it OOMKilled), or a start the list's words hid
// (dockerd stalled past the first minute). The inspect is asked again next sample.
func TestSamplerCgroupSaysInspectIsStale(t *testing.T) {
	dir := t.TempDir()
	write := func(usec int, ooms int) {
		t.Helper()
		for name, text := range map[string]string{
			"cpu.stat": "usage_usec " + strconv.Itoa(usec) + "\n", "memory.current": "1000\n", "memory.stat": "inactive_file 0\n",
			"memory.max": "max\n", "memory.events": "low 0\nhigh 0\nmax 3\noom 1\noom_kill " + strconv.Itoa(ooms) + "\n",
		} {
			os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644)
		}
	}
	now := time.Date(2026, 11, 10, 16, 0, 0, 0, time.UTC)
	d := &cgroupDocker{pid: 4242, started: now.Add(-2 * time.Hour)}
	s := NewSampler(d, fakeSystem)
	s.cgroupOf = func(int) string { return dir }
	s.now = func() time.Time { return now }
	inspects := func() int { d.mu.Lock(); defer d.mu.Unlock(); return d.inspects }
	step := func(usec, ooms int) {
		t.Helper()
		now = now.Add(10 * time.Second)
		write(usec, ooms)
		s.Sample(context.Background())
	}
	step(1_000_000, 0)
	step(2_000_000, 0)
	if inspects() != 1 {
		t.Fatalf("settled: %d inspects", inspects())
	}
	step(3_000_000, 1) // a process killed for memory
	if inspects() != 1 {
		t.Errorf("asked in the same sample: %d", inspects())
	}
	step(4_000_000, 1)
	if inspects() != 2 {
		t.Errorf("OOM kill: not asked again (%d inspects)", inspects())
	}
	step(5_000_000, 1)
	if inspects() != 2 {
		t.Errorf("after the OOM kill: %d inspects", inspects())
	}
	step(400_000, 1) // the CPU count went back: started again
	step(1_400_000, 1)
	if inspects() != 3 {
		t.Errorf("CPU count back: not asked again (%d inspects)", inspects())
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
