package agent

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/history"
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
// dockerd's one-second stats sampling.
type Sampler struct {
	docker     DockerAPI
	readSystem SystemReader
	// History receives every periodic sample when set.
	History *history.Store
	// Alerts evaluates every periodic sample when set.
	Alerts *Alerter

	mu      sync.RWMutex
	last    Overview
	prevCPU sysinfo.CPUTimes
}

func NewSampler(d DockerAPI, rs SystemReader) *Sampler {
	if rs == nil {
		rs = ReadSystem
	}
	return &Sampler{docker: d, readSystem: rs}
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
			info, err := s.container(ctx, c)
			if err != nil {
				errMu.Lock()
				o.Errors = append(o.Errors, c.Name()+": "+err.Error())
				errMu.Unlock()
			}
			o.Containers[i] = info
		}(i, c)
	}
	wg.Wait()
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

func (s *Sampler) container(ctx context.Context, c docker.Container) (ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info := ContainerInfo{
		Name:    c.Name(),
		Image:   c.Image,
		State:   c.State,
		Project: c.Labels["com.docker.compose.project"],
	}
	in, err := s.docker.Inspect(ctx, info.Name)
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
	st, err := s.docker.Stats(ctx, info.Name)
	if err != nil {
		return info, err
	}
	info.CPUPercent, info.MemUsage, info.MemLimit = st.CPUPercent, st.MemUsage, st.MemLimit
	return info, nil
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
