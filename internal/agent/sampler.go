package agent

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sysinfo"
)

// DockerAPI is what the agent needs from dockerd. *docker.Client implements it.
type DockerAPI interface {
	Containers(ctx context.Context) ([]docker.Container, error)
	Inspect(ctx context.Context, name string) (docker.Inspect, error)
	Stats(ctx context.Context, name string) (docker.Stats, error)
	Restart(ctx context.Context, name string, timeout time.Duration) error
	Logs(ctx context.Context, name string, tail int) ([]string, error)
}

// SystemReader returns host metrics; prev is the previous CPU sample for computing usage.
type SystemReader func(prev sysinfo.CPUTimes) (SystemMetrics, sysinfo.CPUTimes, error)

// Sampler refreshes the overview in the background, so page loads never wait for
// dockerd. A running container's CPU and memory are read from its cgroup, the files the
// kernel keeps anyway and dockerd reads itself: its stats request cost it a few
// milliseconds of CPU and a second or two of waiting a container, every sample (Faz
// S3). dockerd is asked for them only where those files cannot be read.
type Sampler struct {
	docker     DockerAPI
	readSystem SystemReader
	// cgroupOf finds a process's cgroup directory, "" if it cannot; now is the clock CPU
	// shares are timed by (tests swap both).
	cgroupOf func(pid int) string
	now      func() time.Time
	// History receives every periodic sample when set.
	History *history.Store
	// Alerts evaluates every periodic sample when set.
	Alerts *Alerter

	mu      sync.RWMutex
	last    Overview
	prevCPU sysinfo.CPUTimes

	marksMu  sync.Mutex
	marks    map[string]cpuMark     // container ID → its last CPU reading
	inspects map[string]inspectMark // container ID → its last inspect
}

// inspectMark is a container's last inspect, with the list entry it was made for.
type inspectMark struct {
	state, status string
	in            docker.Inspect
	at            time.Time
}

// inspectAge is how long an inspect stands while the container's list entry stays
// the same.
const inspectAge = 5 * time.Minute

// cpuMark is a container's last CPU reading, with the process it was of: a container
// started again counts its CPU time from 0.
type cpuMark struct {
	pid int
	dir string // its cgroup
	cpu time.Duration
	oom uint64 // processes killed for memory so far
	at  time.Time
	pct float64 // the share it gave
}

// minCPUWindow is the shortest time a CPU share is measured over. Samples also follow
// jobs and restarts, a moment after the periodic one; the kernel counts CPU time in
// scheduler ticks, so a share over a few milliseconds reads 0 or several hundred.
const minCPUWindow = 2 * time.Second

func NewSampler(d DockerAPI, rs SystemReader) *Sampler {
	if rs == nil {
		rs = ReadSystem
	}
	return &Sampler{docker: d, readSystem: rs, cgroupOf: run.CgroupOfPID, now: time.Now, marks: map[string]cpuMark{},
		inspects: map[string]inspectMark{}}
}

// Run samples every interval until ctx is done.
func (s *Sampler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for first := true; ; first = false {
		o := s.Sample(ctx)
		// The first host CPU reading has nothing to compare with and is always 0.
		if !first {
			if s.History != nil {
				s.History.Add(o.SampledAt, o.samples())
			}
			if s.Alerts != nil {
				s.Alerts.Evaluate(o, o.SampledAt)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Latest returns the newest overview, sampling now if there is none yet.
func (s *Sampler) Latest(ctx context.Context) Overview {
	s.mu.RLock()
	o := s.last
	s.mu.RUnlock()
	if o.SampledAt.IsZero() {
		return s.Sample(ctx)
	}
	return o
}

const statsConcurrency = 4

// Sample collects a fresh overview and stores it.
func (s *Sampler) Sample(ctx context.Context) Overview {
	o := Overview{SampledAt: time.Now().UTC()}

	s.mu.RLock()
	prev := s.prevCPU
	s.mu.RUnlock()
	sys, cpu, err := s.readSystem(prev)
	if err != nil {
		o.Errors = append(o.Errors, "sistem: "+err.Error())
	}
	o.System = sys

	list, err := s.docker.Containers(ctx)
	if err != nil {
		o.Errors = append(o.Errors, "docker: "+err.Error())
	}
	o.Containers = make([]ContainerInfo, len(list))
	sem := make(chan struct{}, statsConcurrency)
	var wg sync.WaitGroup
	var errMu sync.Mutex
	for i, c := range list {
		wg.Add(1)
		go func(i int, c docker.Container) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			info, err := s.container(ctx, c, sys.MemTotal)
			if err != nil {
				errMu.Lock()
				o.Errors = append(o.Errors, c.Name()+": "+err.Error())
				errMu.Unlock()
			}
			o.Containers[i] = info
		}(i, c)
	}
	wg.Wait()
	s.forgetMarks(s.now().Add(-10 * time.Minute))
	sort.Slice(o.Containers, func(i, j int) bool {
		a, b := o.Containers[i], o.Containers[j]
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Name < b.Name
	})

	s.mu.Lock()
	s.last, s.prevCPU = o, cpu
	s.mu.Unlock()
	return o
}

// samples converts an overview into history readings: the host and running containers.
func (o Overview) samples() map[string]history.Sample {
	m := map[string]history.Sample{}
	if o.System.MemTotal > 0 {
		m[history.HostKey] = history.Sample{CPU: o.System.CPUPercent, Mem: o.System.MemUsed, Swap: o.System.SwapUsed}
	}
	for _, c := range o.Containers {
		if c.State == "running" && c.Name != "" {
			m[c.Name] = history.Sample{CPU: c.CPUPercent, Mem: c.MemUsage}
		}
	}
	return m
}

func (s *Sampler) container(ctx context.Context, c docker.Container, memTotal uint64) (ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info := ContainerInfo{
		Name:    c.Name(),
		Image:   c.Image,
		State:   c.State,
		Project: c.Labels["com.docker.compose.project"],
	}
	in, err := s.inspect(ctx, c)
	if err != nil {
		return info, err
	}
	info.RestartCount = in.RestartCount
	info.OOMKilled = in.State.OOMKilled
	if in.State.Health != nil {
		info.Health = in.State.Health.Status
	}
	info.StartedAt, _ = time.Parse(time.RFC3339Nano, in.State.StartedAt)
	if !in.State.Running {
		return info, nil
	}
	if cpu, cg, stale, ok := s.fromCgroup(c.ID, in.State.Pid, info.StartedAt); ok {
		if stale {
			s.dropInspect(c.ID) // asked again next time
		}
		info.CPUPercent, info.MemUsage, info.MemLimit = cpu, cg.MemUsage, cg.MemLimit
		if info.MemLimit == 0 || memTotal > 0 && info.MemLimit > memTotal {
			info.MemLimit = memTotal // no limit: the server's memory, as dockerd gives it
		}
		return info, nil
	}
	st, err := s.docker.Stats(ctx, info.Name)
	if err != nil {
		return info, err
	}
	info.CPUPercent, info.MemUsage, info.MemLimit = st.CPUPercent, st.MemUsage, st.MemLimit
	return info, nil
}

// inspect asks dockerd about a container only when its list entry changed — a restart
// ("Up 2 seconds"), a health change ("(unhealthy)"), an exit all show there — or the
// last answer is inspectAge old. A container up for less than a minute is always asked:
// its entry could read the same across a restart. On a server's slow cores the
// inspects of every container cost dockerd tens of milliseconds a sample (Faz S3b).
func (s *Sampler) inspect(ctx context.Context, c docker.Container) (docker.Inspect, error) {
	now := s.now()
	s.marksMu.Lock()
	m, ok := s.inspects[c.ID]
	s.marksMu.Unlock()
	if ok && c.ID != "" && m.state == c.State && m.status == c.Status && now.Sub(m.at) < inspectAge && !strings.Contains(c.Status, "second") {
		return m.in, nil
	}
	in, err := s.docker.Inspect(ctx, c.Name())
	if err != nil {
		s.dropInspect(c.ID)
		return in, err
	}
	// Kept only if it tells what the list entry does: a change in between (or another
	// container under the name now) is asked about again next time.
	if in.ID == c.ID && in.State.Status == c.State && healthShown(in, c.Status) {
		s.marksMu.Lock()
		s.inspects[c.ID] = inspectMark{state: c.State, status: c.Status, in: in, at: now}
		s.marksMu.Unlock()
	} else {
		s.dropInspect(c.ID)
	}
	return in, nil
}

func (s *Sampler) dropInspect(id string) {
	s.marksMu.Lock()
	delete(s.inspects, id)
	s.marksMu.Unlock()
}

// healthShown reports whether a list entry's Status ("Up 2 hours (healthy)") says the
// health the inspect does.
func healthShown(in docker.Inspect, status string) bool {
	want := ""
	if h := in.State.Health; h != nil {
		switch h.Status {
		case "starting":
			want = "(health: starting)"
		case "healthy", "unhealthy":
			want = "(" + h.Status + ")"
		}
	}
	if want == "" {
		return !strings.Contains(status, "health")
	}
	return strings.Contains(status, want)
}

// fromCgroup reads a running container's cgroup; ok is false where it cannot (no cgroup
// v2, or the container stopped just now). The CPU is the share of one core it used since
// the last sample, as `docker stats` gives it, but over the whole interval: 0 the first
// time it is seen. stale says the inspect behind pid is out of date though the list
// entry did not change: the kernel killed a process for memory (dockerd marks the
// container OOMKilled, running or not), or the CPU count went back (it started again).
func (s *Sampler) fromCgroup(id string, pid int, started time.Time) (float64, sysinfo.Cgroup, bool, bool) {
	if pid <= 0 || id == "" {
		return 0, sysinfo.Cgroup{}, false, false
	}
	s.marksMu.Lock()
	prev, had := s.marks[id]
	s.marksMu.Unlock()
	dir := prev.dir
	if !had || prev.pid != pid {
		dir = containerCgroup(s.cgroupOf(pid), id)
	}
	if dir == "" {
		return 0, sysinfo.Cgroup{}, false, false
	}
	cg, err := sysinfo.ReadCgroup(dir)
	now := s.now()
	if err != nil {
		return 0, sysinfo.Cgroup{}, false, false
	}
	stale := had && prev.pid == pid && (cg.OOMKills > prev.oom || cg.CPU < prev.cpu)
	if had && prev.pid == pid && now.Sub(prev.at) < minCPUWindow && !stale {
		return prev.pct, cg, false, true // the mark stays: the next share spans from it
	}
	var cpu float64
	switch {
	case had && prev.pid == pid && cg.CPU >= prev.cpu && now.After(prev.at):
		cpu = float64(cg.CPU-prev.cpu) / float64(now.Sub(prev.at)) * 100
	case had && prev.pid != pid && started.After(prev.at) && now.After(started):
		cpu = float64(cg.CPU) / float64(now.Sub(started)) * 100 // started again since: all of it is since then
	}
	s.marksMu.Lock()
	s.marks[id] = cpuMark{pid: pid, dir: dir, cpu: cg.CPU, oom: cg.OOMKills, at: now, pct: cpu}
	s.marksMu.Unlock()
	return cpu, cg, stale, true
}

// containerCgroup is the cgroup of the container as a whole: its process's, or the
// nearest one above it named after the container (systemd inside a container moves its
// processes into a child cgroup, which would count only them and have no limit).
func containerCgroup(dir, id string) string {
	for d := dir; strings.HasPrefix(d, run.CgroupRoot+"/"); d = path.Dir(d) {
		if strings.Contains(path.Base(d), id) {
			return d
		}
	}
	return dir
}

// forgetMarks drops the readings of containers not seen since before: gone ones.
func (s *Sampler) forgetMarks(before time.Time) {
	s.marksMu.Lock()
	defer s.marksMu.Unlock()
	for id, m := range s.marks {
		if m.at.Before(before) {
			delete(s.marks, id)
		}
	}
	for id, m := range s.inspects {
		if m.at.Before(before) {
			delete(s.inspects, id)
		}
	}
}

// ReadSystem reads host metrics from /proc and statfs.
func ReadSystem(prev sysinfo.CPUTimes) (SystemMetrics, sysinfo.CPUTimes, error) {
	var m SystemMetrics
	var errs []error
	m.Hostname, _ = os.Hostname()
	cpu, err := sysinfo.ReadCPUTimes()
	if err != nil {
		errs = append(errs, err)
	} else if prev.Total > 0 {
		m.CPUPercent = sysinfo.CPUPercent(prev, cpu)
	}
	if l, err := sysinfo.ReadLoad(); err == nil {
		m.Load1, m.Load5, m.Load15 = l.One, l.Five, l.Fifteen
	} else {
		errs = append(errs, err)
	}
	if mem, err := sysinfo.ReadMemory(); err == nil {
		m.MemTotal, m.MemUsed = mem.Total, mem.Total-mem.Available
		m.SwapTotal, m.SwapUsed = mem.SwapTotal, mem.SwapTotal-mem.SwapFree
	} else {
		errs = append(errs, err)
	}
	if d, err := sysinfo.ReadDisk("/"); err == nil {
		m.DiskTotal, m.DiskUsed = d.Total, d.Used
	} else {
		errs = append(errs, err)
	}
	m.Uptime, _ = sysinfo.ReadUptime()
	if len(errs) > 0 {
		return m, cpu, fmt.Errorf("%v", errs)
	}
	return m, cpu, nil
}
