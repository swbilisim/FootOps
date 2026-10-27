package run

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamCopiesAndReportsLines(t *testing.T) {
	var out bytes.Buffer
	var lines []string
	u, err := StreamExec{}.Stream(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "cat; echo one >&2; echo two >&2"},
		Stdin: strings.NewReader("data in"), Stdout: &out,
		Stderr: func(l string) { lines = append(lines, l) },
	})
	if err != nil || out.String() != "data in" || strings.Join(lines, ",") != "one,two" || u.Wall <= 0 {
		t.Errorf("out=%q lines=%v u=%+v err=%v", out.String(), lines, u, err)
	}
}

func TestStreamErrorCarriesLastLines(t *testing.T) {
	_, err := StreamExec{}.Stream(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo Failed: auth error >&2; exit 3"}, Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "Failed: auth error") || !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("err = %v", err)
	}
}

func TestStreamCancelStopsThenKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var stopped atomic.Bool
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		// Stop does nothing (like a docker kill that failed): the process is killed
		// after the grace period, together with its child.
		_, err := StreamExec{}.Stream(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & wait"}, Stdout: &bytes.Buffer{},
			Stop: func() { stopped.Store(true) }})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled || !stopped.Load() {
			t.Errorf("err = %v, stopped = %v", err, stopped.Load())
		}
		if d := time.Since(start); d < stopGrace || d > stopGrace+5*time.Second {
			t.Errorf("took %s", d)
		}
	case <-time.After(stopGrace + 10*time.Second):
		t.Fatal("not killed")
	}
}

func TestStreamFailedWriterStopsCommand(t *testing.T) {
	_, err := StreamExec{}.Stream(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "yes"}, Stdout: failingWriter{}})
	if err == nil {
		t.Error("no error although the output could not be written")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestMeterReadsCgroupFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("104857600\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 1500000\nuser_usec 1000000\n"), 0o644)
	var m meter
	m.sample(dir)
	os.WriteFile(filepath.Join(dir, "memory.peak"), []byte("52428800\n"), 0o644) // never goes down
	m.sample(dir)
	m.sample("")
	if u := m.usage(); u.PeakMem != 100<<20 || u.CPU != 1500*time.Millisecond {
		t.Errorf("usage = %+v", u)
	}
	// memory.current when memory.peak is missing (older kernels).
	dir2 := t.TempDir()
	os.WriteFile(filepath.Join(dir2, "memory.current"), []byte("2048\n"), 0o644)
	var m2 meter
	m2.sample(dir2)
	if m2.peak != 2048 {
		t.Errorf("peak = %d", m2.peak)
	}
}

func TestParseCgroup(t *testing.T) {
	cases := map[string]string{
		"0::/system.slice/docker-abc.scope\n":               "/sys/fs/cgroup/system.slice/docker-abc.scope",
		"12:memory:/docker/abc\n0::/system.slice/x.scope\n": "/sys/fs/cgroup/system.slice/x.scope",
		"0::/../../etc\n":         "",
		"12:memory:/docker/abc\n": "",
	}
	for in, want := range cases {
		if got := parseCgroup(in); got != want {
			t.Errorf("parseCgroup(%q) = %q, want %q", in, got, want)
		}
	}
	if CgroupOfPID(0) != "" || CgroupOfPID(-1) != "" {
		t.Error("bad pid")
	}
}

func TestUsageAddAndDockerFlags(t *testing.T) {
	u := Usage{PeakMem: 10, CPU: time.Second, Wall: time.Second}.Add(Usage{PeakMem: 5, CPU: time.Second, Wall: 2 * time.Second})
	if u.PeakMem != 10 || u.CPU != 2*time.Second || u.Wall != 3*time.Second {
		t.Errorf("add = %+v", u)
	}
	got := strings.Join(Limits{MemoryMB: 256, CPUPercent: 25, PIDs: 64}.DockerFlags(), " ")
	if got != "--memory 256m --memory-swap 256m --cpus 0.25 --pids-limit 64" {
		t.Errorf("flags = %s", got)
	}
}

func TestStreamStopDoesNotBlockTheKill(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hang := make(chan struct{})
	defer close(hang)
	done := make(chan error, 1)
	go func() {
		// Stop hangs like a docker kill against a wedged daemon.
		_, err := StreamExec{}.Stream(ctx, Cmd{Name: "sleep", Args: []string{"30"}, Stdout: &bytes.Buffer{}, Stop: func() { <-hang }})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("err = %v", err)
		}
	case <-time.After(stopGrace + 5*time.Second):
		t.Fatal("a hanging Stop kept the command alive")
	}
}

func TestStreamStopsWorkAfterFailure(t *testing.T) {
	stopped := make(chan struct{})
	_, err := StreamExec{}.Stream(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 1"}, Stdout: &bytes.Buffer{},
		Stop: func() { close(stopped) }})
	if err == nil {
		t.Fatal("no error")
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Error("Stop not called after a failure")
	}
}

func TestExecKillOnCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Exec{KillOnCancel: true}.Run(ctx, "sh", "-c", "sleep 30 & wait")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %s", err, time.Since(start))
	}
}
