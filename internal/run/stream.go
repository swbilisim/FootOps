package run

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Usage is what a command used. Memory and CPU come from its cgroup, sampled while it
// runs (Linux only; zero elsewhere or when the command ended before the first sample).
type Usage struct {
	PeakMem uint64        `json:"peak_mem,omitempty"` // bytes
	CPU     time.Duration `json:"cpu,omitempty"`
	Wall    time.Duration `json:"wall,omitempty"`
}

// Add combines the usage of commands that ran one after another.
func (u Usage) Add(v Usage) Usage {
	return Usage{PeakMem: max(u.PeakMem, v.PeakMem), CPU: u.CPU + v.CPU, Wall: u.Wall + v.Wall}
}

// Limits bound a heavy command so it cannot starve the sites on the same server.
type Limits struct {
	MemoryMB   int // hard limit, no swap
	CPUPercent int // of one core
	PIDs       int
}

// DefaultLimits are deliberately small: a 4 GB server runs the sites first.
var DefaultLimits = Limits{MemoryMB: 512, CPUPercent: 50, PIDs: 256}

// DockerFlags are the docker run flags enforcing l.
func (l Limits) DockerFlags() []string {
	mem := strconv.Itoa(l.MemoryMB) + "m"
	return []string{"--memory", mem, "--memory-swap", mem,
		"--cpus", strconv.FormatFloat(float64(l.CPUPercent)/100, 'f', 2, 64),
		"--pids-limit", strconv.Itoa(l.PIDs)}
}

// Cmd is a long command whose input and output are streams (a database dump, an
// archive). Unlike Exec it is stopped when its context ends: these commands only read,
// or write to a place the caller throws away on failure.
type Cmd struct {
	Name string
	Args []string // never secrets: they are visible in the process list
	// Stdin and Stdout are copied without buffering the whole stream.
	Stdin  io.Reader
	Stdout io.Writer
	// ExtraFiles become descriptors 3, 4, … of the command (files it may not be able to
	// open by path itself).
	ExtraFiles []*os.File
	// Stderr receives each line the command writes to stderr. The last lines are also
	// kept for the error message.
	Stderr func(line string)
	// Stop ends the work when the context ends or the command fails, alongside killing
	// the process. A docker run client can die while its container keeps running, so
	// tool containers stop the container here. It runs in its own goroutine and must
	// not block for long.
	Stop func()
	// Cgroup returns the cgroup directory to measure, "" while unknown. Without it
	// nothing is measured: the process would otherwise still be in the agent's own
	// cgroup, whose numbers include the agent.
	Cgroup func() string
}

// Streamer runs Cmds. Tests substitute a fake.
type Streamer interface {
	Stream(ctx context.Context, c Cmd) (Usage, error)
}

// StreamExec runs real commands.
type StreamExec struct {
	// SampleEvery is the cgroup sampling interval (default 100 ms: short tool runs end
	// within half a second, and a container's cgroup is gone once it exits). Each
	// sample reads two small files, and only while a command runs.
	SampleEvery time.Duration
}

const (
	stopGrace    = 5 * time.Second
	keptStderr   = 8
	maxLineBytes = 64 << 10
)

func (r StreamExec) Stream(ctx context.Context, c Cmd) (Usage, error) {
	if err := ctx.Err(); err != nil {
		return Usage{}, err
	}
	cmd := exec.Command(c.Name, c.Args...)
	cmd.Env = append(os.Environ(), execEnv...)
	cmd.Stdin = c.Stdin
	cmd.Stdout = c.Stdout
	cmd.ExtraFiles = c.ExtraFiles
	setOwnProcessGroup(cmd)
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return Usage{}, err
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Usage{}, err
	}

	var tail lastLines
	lines := make(chan struct{})
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 4096), maxLineBytes)
		for sc.Scan() {
			line := sc.Text()
			tail.add(line)
			if c.Stderr != nil {
				c.Stderr(line)
			}
		}
		_, _ = io.Copy(io.Discard, errPipe) // an over-long line must not block the command
	}()

	var meter meter
	sampling := make(chan struct{})
	every := r.SampleEvery
	if every <= 0 {
		every = 100 * time.Millisecond
	}
	stopSampling := make(chan struct{})
	go func() {
		defer close(sampling)
		if c.Cgroup == nil {
			return
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			meter.sample(c.Cgroup())
			select {
			case <-stopSampling:
				meter.sample(c.Cgroup()) // the cgroup may outlive the client for a moment
				return
			case <-t.C:
			}
		}
	}()

	exited := make(chan struct{})
	go func() {
		select {
		case <-exited:
		case <-ctx.Done():
			// Stop may hang (a wedged Docker daemon); the kill does not wait for it.
			if c.Stop != nil {
				go c.Stop()
			}
			select {
			case <-exited:
			case <-time.After(stopGrace):
				killGroup(cmd)
			}
		}
	}()

	<-lines // StderrPipe must be drained before Wait
	err = cmd.Wait()
	close(exited)
	close(stopSampling)
	<-sampling
	u := meter.usage()
	u.Wall = time.Since(start)
	if err != nil && ctx.Err() == nil && c.Stop != nil {
		// The client failed (e.g. its output could not be written): make sure the work
		// behind it stops too.
		go c.Stop()
	}
	if err != nil {
		if ctx.Err() != nil {
			return u, ctx.Err()
		}
		if msg := tail.String(); msg != "" {
			return u, fmt.Errorf("%s: %w: %s", c.Name, err, msg)
		}
		return u, fmt.Errorf("%s: %w", c.Name, err)
	}
	return u, nil
}

// lastLines keeps the last few stderr lines for error messages.
type lastLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *lastLines) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	l.lines = append(l.lines, s)
	if len(l.lines) > keptStderr {
		l.lines = l.lines[1:]
	}
}

func (l *lastLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.TrimSpace(strings.Join(l.lines, "\n"))
}

// meter keeps the highest memory and the latest CPU time seen in a cgroup.
type meter struct {
	peak uint64
	cpu  time.Duration
}

func (m *meter) sample(dir string) {
	if dir == "" {
		return
	}
	// memory.peak is the cgroup's own high-water mark (kernel 5.19+), so short spikes
	// between samples are not missed; memory.current is the fallback.
	for _, f := range []string{"memory.peak", "memory.current"} {
		if v, err := readUint(dir + "/" + f); err == nil {
			m.peak = max(m.peak, v)
			break
		}
	}
	if b, err := os.ReadFile(dir + "/cpu.stat"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
				if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
					m.cpu = max(m.cpu, time.Duration(n)*time.Microsecond)
				}
			}
		}
	}
}

func (m *meter) usage() Usage { return Usage{PeakMem: m.peak, CPU: m.cpu} }

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// CgroupRoot is where the unified (v2) cgroup hierarchy is mounted.
var CgroupRoot = "/sys/fs/cgroup"

// CgroupOfPID returns the cgroup v2 directory of a process, "" if unknown.
func CgroupOfPID(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return ""
	}
	return parseCgroup(string(b))
}

// parseCgroup reads the v2 entry ("0::/system.slice/docker-<id>.scope") of
// /proc/<pid>/cgroup.
func parseCgroup(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasPrefix(p, "/") && !strings.Contains(p, "..") {
			return CgroupRoot + p
		}
	}
	return ""
}
