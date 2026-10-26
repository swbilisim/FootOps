package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/sysinfo"
)

type fakeDocker struct {
	mu        sync.Mutex
	restarted []string
}

func (f *fakeDocker) Containers(context.Context) ([]docker.Container, error) {
	return []docker.Container{
		{ID: "1", Names: []string{"/footops-mongo"}, Image: "mongo:7.0.45", State: "running", Labels: map[string]string{"com.docker.compose.project": "footops-core"}},
		{ID: "2", Names: []string{"/old-site"}, Image: "node:14", State: "exited"},
	}, nil
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (docker.Inspect, error) {
	var in docker.Inspect
	switch name {
	case "footops-mongo":
		in.State.Running = true
		in.State.StartedAt = "2026-10-26T01:00:00.123456789Z"
		in.RestartCount = 2
		in.State.Health = &struct {
			Status string `json:"Status"`
		}{"healthy"}
	case "old-site":
	default:
		return in, &docker.APIError{Status: 404, Message: "No such container"}
	}
	return in, nil
}

func (f *fakeDocker) Stats(context.Context, string) (docker.Stats, error) {
	return docker.Stats{CPUPercent: 3.5, MemUsage: 300 << 20, MemLimit: 4 << 30}, nil
}

func (f *fakeDocker) Restart(_ context.Context, name string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, name)
	return nil
}

func (f *fakeDocker) Logs(_ context.Context, name string, tail int) ([]string, error) {
	return []string{name, "line"}, nil
}

func fakeSystem(prev sysinfo.CPUTimes) (SystemMetrics, sysinfo.CPUTimes, error) {
	return SystemMetrics{Hostname: "h1", MemTotal: 4 << 30}, sysinfo.CPUTimes{Busy: 1, Total: 2}, nil
}

// startAgent serves on a real unix socket. peerUID is faked because SO_PEERCRED is
// Linux-only; uid is what the "kernel" reports for every client.
func startAgent(t *testing.T, uid uint32) (*Client, *fakeDocker, *Audit) {
	t.Helper()
	c, s := startServer(t, uid)
	return c, s.Docker.(*fakeDocker), s.Audit
}

// startServer starts an agent; setup runs before it serves, so tests can swap parts
// without racing the handlers.
func startServer(t *testing.T, uid uint32, setup ...func(*Server)) (*Client, *Server) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fo") // short path: unix socket paths are limited to ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDocker{}
	audit := NewAudit(filepath.Join(dir, "audit.jsonl"))
	s := &Server{
		Docker:      d,
		Audit:       audit,
		Sampler:     NewSampler(d, fakeSystem),
		AllowedUIDs: map[uint32]bool{0: true, 999: true},
		PeerUID:     func(net.Conn) (uint32, error) { return uid, nil },
	}
	for _, f := range setup {
		f(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return NewClient(sock), s
}

var ctx = context.Background()

func TestPeerNotAllowed(t *testing.T) {
	c, d, _ := startAgent(t, 1000)
	_, err := c.Overview(ctx)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("err = %v, want 403", err)
	}
	if err := c.Restart(ctx, "footops-mongo", Actor{User: "x"}); err == nil {
		t.Fatal("restart allowed for a foreign uid")
	}
	if len(d.restarted) != 0 {
		t.Error("restart executed")
	}
}

func TestOverview(t *testing.T) {
	c, _, _ := startAgent(t, 999)
	o, err := c.Overview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if o.System.Hostname != "h1" || len(o.Containers) != 2 {
		t.Fatalf("overview = %+v", o)
	}
	m := o.Containers[1] // sorted: project "" first, then footops-core
	if m.Name != "footops-mongo" || m.Health != "healthy" || m.RestartCount != 2 || m.CPUPercent != 3.5 || m.StartedAt.IsZero() {
		t.Errorf("mongo = %+v", m)
	}
	if o.Containers[0].MemUsage != 0 {
		t.Error("stats sampled for a stopped container")
	}
}

func TestRestartIsAudited(t *testing.T) {
	c, d, audit := startAgent(t, 999)
	actor := Actor{User: "furkan", IP: "203.0.113.5", Via: "tunnel"}
	if err := c.Restart(ctx, "footops-mongo", actor); err != nil {
		t.Fatal(err)
	}
	if strings.Join(d.restarted, ",") != "footops-mongo" {
		t.Errorf("restarted = %v", d.restarted)
	}
	err := c.Restart(ctx, "nope", actor)
	var ae *Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("missing container: %v", err)
	}
	if err := c.Restart(ctx, "../images", actor); err == nil {
		t.Error("path traversal accepted")
	}
	entries, err := audit.Tail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].OK || entries[0].Target != "nope" || !entries[1].OK || entries[1].Actor != actor {
		t.Errorf("audit = %+v", entries)
	}
}

func TestLogsTailBounds(t *testing.T) {
	c, _, _ := startAgent(t, 0)
	lines, err := c.Logs(ctx, "footops-mongo", 50)
	if err != nil || len(lines) != 2 {
		t.Fatalf("lines = %v, %v", lines, err)
	}
	if _, err := c.Logs(ctx, "footops-mongo", 5000); err == nil {
		t.Error("tail above limit accepted")
	}
}

func TestAuditOnlyAcceptsAuthEvents(t *testing.T) {
	c, _, audit := startAgent(t, 999)
	if err := c.RecordAuth(ctx, AuditEntry{Action: "auth.login", Actor: Actor{User: "furkan"}, OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordAuth(ctx, AuditEntry{Action: "container.restart", Target: "x", OK: true}); err == nil {
		t.Error("web process could forge a server action")
	}
	forgedTime := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_ = c.RecordAuth(ctx, AuditEntry{Action: "auth.logout", Time: forgedTime})
	entries, _ := audit.Tail(10)
	if len(entries) != 2 || entries[0].Time.Equal(forgedTime) {
		t.Errorf("entries = %+v", entries)
	}
}

func TestAuditSanitisesAndRotates(t *testing.T) {
	dir := t.TempDir()
	a := NewAudit(filepath.Join(dir, "audit.jsonl"))
	if err := a.Record(AuditEntry{Action: "auth.login", Detail: "line1\nline2\x1b[31m"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Count(string(b), "\n") != 1 || strings.Contains(string(b), `\u001b`) {
		t.Errorf("entry not one clean line: %q", b)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.jsonl"), make([]byte, auditMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.Record(AuditEntry{Action: "auth.login"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit.jsonl.1")); err != nil {
		t.Error("not rotated")
	}
	if e, _ := a.Tail(5); len(e) != 1 {
		t.Errorf("after rotation: %d entries", len(e))
	}
}

func TestHistory(t *testing.T) {
	c, _ := startServer(t, 999)
	if h, err := c.History(ctx, "24h", ""); err != nil || len(h.Host) != 0 || h.Containers == nil {
		t.Fatalf("without a store: %+v, %v", h, err)
	}

	store := history.New()
	c, _ = startServer(t, 999, func(s *Server) { s.Sampler.History = store })
	start := time.Now().Add(-5 * time.Minute)
	for i := 0; i < 30; i++ {
		o := Overview{
			SampledAt:  start.Add(time.Duration(i) * 10 * time.Second),
			System:     SystemMetrics{CPUPercent: 20, MemTotal: 4 << 30, MemUsed: 1 << 30},
			Containers: []ContainerInfo{{Name: "web", State: "running", CPUPercent: 5, MemUsage: 100 << 20}, {Name: "old", State: "exited"}},
		}
		store.Add(o.SampledAt, o.samples())
	}
	h, err := c.History(ctx, "1h", "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Step != 60 || len(h.Host) == 0 || h.Host[0].CPU != 20 || len(h.Containers) != 1 || len(h.Containers["web"]) == 0 {
		t.Errorf("history = %+v", h)
	}
	if h, err := c.History(ctx, "1h", "web"); err != nil || len(h.Host) != 0 || len(h.Containers) != 1 {
		t.Errorf("one container: %+v, %v", h, err)
	}
	if _, err := c.History(ctx, "1y", ""); err == nil {
		t.Error("unknown range accepted")
	}
	if _, err := c.History(ctx, "1h", "../x"); err == nil {
		t.Error("bad container name accepted")
	}
}
