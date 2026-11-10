package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/monitor"
)

// The server check (Faz U1, panel-tasarim §31.1): `footops doctor --json` runs in a
// transient service — the agent's own sandbox hides /home and /root, and the check only
// reads, so its service is read-only — once a day, a little after the agent starts and
// whenever the panel asks. The last report is kept on disk. A problem becomes an alert
// once two runs in a row have seen it (a new one is looked at again 10 minutes later):
// something passing, like MongoDB restarting at that moment, does not stay an alert all
// day.
const (
	DefaultDoctorPath = "/var/lib/footops/doctor.json"
	// doctorHour:doctorMinute is the daily run (server time): after unattended-upgrades'
	// morning run, so updates it left are counted.
	doctorHour, doctorMinute = 8, 30
	doctorFirst              = 2 * time.Minute // after the agent starts
	doctorConfirm            = 10 * time.Minute
	doctorRetry              = 30 * time.Minute // after a run that failed
	doctorTimeout            = 6 * time.Minute
	doctorUnit               = "footops-doctor"
	doctorMaxReport          = 1 << 20
)

// doctorProps sandbox the check's service: it only reads; HOME is the private /tmp for
// what the tools it runs keep there (gpg, the docker command). ufw opens its lock file
// for writing even to show the firewall (`--dry-run` shows nothing): that file alone is
// writable, made beforehand if missing (ufwLock).
var doctorProps = []string{
	"ProtectSystem=strict", "ProtectHome=read-only", "PrivateTmp=yes", "NoNewPrivileges=yes",
	"ReadWritePaths=-" + ufwLock,
	"MemoryMax=256M", "MemorySwapMax=0", "CPUQuota=50%", "Nice=10", "IOSchedulingClass=idle",
	"RuntimeMaxSec=300",
}

// ufwLock is the file ufw locks; it makes it the same way (empty, root's, 0644).
const ufwLock = "/run/ufw.lock"

// Doctor runs the server check and keeps its report.
type Doctor struct {
	Path string // the report and what its alerts need; empty: memory only
	Self string // the footops binary; empty: this process's
	Log  *log.Logger
	// Exec runs the check and returns its JSON report (tests replace it).
	Exec func(ctx context.Context) ([]byte, error)
	// Changed is called after each run (the status file learns of it).
	Changed func()

	now      func() time.Time
	initOnce sync.Once
	kick     chan struct{}
	mu       sync.Mutex
	st       doctorState
	loaded   bool
	running  bool
	next     time.Time
	saveErr  string
}

type doctorState struct {
	Report *doctor.Report `json:"report,omitempty"`
	// Seen holds the last report's problems and when each was first seen; Confirmed
	// those seen by the run before too (the alerts).
	Seen      map[string]time.Time `json:"seen,omitempty"`
	Confirmed map[string]bool      `json:"confirmed,omitempty"`
	// Tried is the last attempt; Err, ErrSince and Fails describe the failing ones.
	Tried    time.Time `json:"tried,omitzero"`
	Err      string    `json:"error,omitempty"`
	ErrSince time.Time `json:"error_since,omitzero"`
	Fails    int       `json:"fails,omitempty"`
}

// DoctorStatus is the check page's.
type DoctorStatus struct {
	Report *doctor.Report `json:"report,omitempty"`
	// Confirmed: the problems that are alerts (seen twice in a row).
	Confirmed map[string]bool `json:"confirmed,omitempty"`
	Running   bool            `json:"running"`
	Next      time.Time       `json:"next,omitzero"`
	Tried     time.Time       `json:"tried,omitzero"`
	Err       string          `json:"error,omitempty"`
}

func (d *Doctor) init() {
	d.initOnce.Do(func() { d.kick = make(chan struct{}, 1) })
}

func (d *Doctor) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func (d *Doctor) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf(format, args...)
	}
}

// state returns the state (read from Path the first time). Maps are shared: callers
// replace them, never change them.
func (d *Doctor) state() doctorState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.loaded {
		d.loaded = true
		if d.Path != "" {
			if data, err := os.ReadFile(d.Path); err == nil {
				_ = json.Unmarshal(data, &d.st) // a broken file starts afresh: the next run fills it
			}
		}
	}
	return d.st
}

func (d *Doctor) saveState(st doctorState) {
	d.mu.Lock()
	d.st, d.loaded = st, true
	d.mu.Unlock()
	if d.Path == "" {
		return
	}
	data, err := json.Marshal(st)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(d.Path), 0o700); err == nil {
			err = writeSynced(d.Path, append(data, '\n'))
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case err != nil && d.saveErr != err.Error():
		d.saveErr = err.Error()
		d.logf("sunucu kontrolünün raporu kaydedilemedi (bellekte tutuluyor): %v", err)
	case err == nil:
		d.saveErr = ""
	}
}

// Status is the check page's view.
func (d *Doctor) Status() DoctorStatus {
	st := d.state()
	d.mu.Lock()
	defer d.mu.Unlock()
	return DoctorStatus{Report: st.Report, Confirmed: st.Confirmed, Running: d.running, Next: d.next, Tried: st.Tried, Err: st.Err}
}

// RunNow asks for a run at once; false if one is already running.
func (d *Doctor) RunNow() bool {
	d.init()
	d.mu.Lock()
	running := d.running
	d.mu.Unlock()
	if running {
		return false
	}
	select {
	case d.kick <- struct{}{}:
	default: // one is already asked for
	}
	return true
}

// Run checks until ctx is done.
func (d *Doctor) Run(ctx context.Context) {
	d.init()
	d.setNext(d.clock().Add(doctorFirst))
	t := time.NewTimer(doctorFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.kick:
		}
		d.Check(ctx)
		now := d.clock()
		wait := d.nextWake(now)
		d.setNext(now.Add(wait))
		t.Reset(wait)
	}
}

func (d *Doctor) setNext(t time.Time) {
	d.mu.Lock()
	d.next = t
	d.mu.Unlock()
}

// nextWake: the daily run, sooner to confirm a new problem or after a failure.
func (d *Doctor) nextWake(now time.Time) time.Duration {
	local := now.Local()
	daily := time.Date(local.Year(), local.Month(), local.Day(), doctorHour, doctorMinute, 0, 0, local.Location())
	if !daily.After(local) {
		daily = daily.AddDate(0, 0, 1)
	}
	wait := daily.Sub(local)
	st := d.state()
	if st.Fails > 0 {
		wait = min(wait, doctorRetry)
	}
	if len(st.Seen) > len(st.Confirmed) {
		wait = min(wait, doctorConfirm)
	}
	return wait
}

// Check runs the server check once (none if one is running).
func (d *Doctor) Check(ctx context.Context) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.running = false
		d.mu.Unlock()
	}()

	cctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	out, err := d.exec(cctx)
	cancel()
	if ctx.Err() != nil {
		return // the agent is stopping: not the check's failure
	}
	var r doctor.Report
	if err == nil {
		err = parseDoctorReport(out, &r)
	}
	now := d.clock()
	st := d.state()
	st.Tried = now
	if err != nil {
		if st.Fails == 0 {
			st.ErrSince = now
		}
		st.Fails++
		st.Err = clipMsg(err.Error(), 300)
		d.logf("sunucu kontrolü çalışmadı: %v", err)
	} else {
		st.Fails, st.Err, st.ErrSince = 0, "", time.Time{}
		seen, confirmed := map[string]time.Time{}, map[string]bool{}
		for _, f := range r.Problems() {
			if first, ok := st.Seen[f.ID]; ok {
				seen[f.ID], confirmed[f.ID] = first, true
			} else {
				seen[f.ID] = now
			}
		}
		st.Report, st.Seen, st.Confirmed = &r, seen, confirmed
	}
	d.saveState(st)
	if d.Changed != nil {
		d.Changed()
	}
}

func parseDoctorReport(out []byte, r *doctor.Report) error {
	if err := json.Unmarshal(bytes.TrimSpace(out), r); err != nil {
		return fmt.Errorf("rapor okunamadı: %w", err)
	}
	if r.Format < 1 || r.Time.IsZero() {
		return errors.New("rapor okunamadı: biçim tanınmıyor")
	}
	return nil
}

func (d *Doctor) exec(ctx context.Context) ([]byte, error) {
	if d.Exec != nil {
		return d.Exec(ctx)
	}
	self := d.Self
	if self == "" {
		p, err := os.Executable()
		if err != nil {
			return nil, err
		}
		self = p
	}
	if f, err := os.OpenFile(ufwLock, os.O_CREATE|os.O_RDONLY, 0o644); err == nil {
		f.Close()
	}
	args := []string{"--unit=" + doctorUnit, "--description=FootOps sunucu kontrolü", "--quiet", "--collect", "--wait", "--pipe",
		"--setenv=HOME=/tmp"}
	for _, p := range doctorProps {
		args = append(args, "--property="+p)
	}
	args = append(args, "--", self, "doctor", "--json")
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	cmd.Cancel = func() error {
		// Stopping systemd-run leaves the service running: it is stopped by name.
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(stop, "systemctl", "stop", doctorUnit+".service").Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &capWriter{w: &stdout, n: doctorMaxReport}
	cmd.Stderr = &capWriter{w: &stderr, n: 64 << 10}
	if err := cmd.Run(); err != nil {
		if msg := lastLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// capWriter keeps the first n bytes and drops the rest (the command still sees them
// written).
type capWriter struct {
	w io.Writer
	n int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n > 0 {
		k := min(len(p), c.n)
		if _, err := c.w.Write(p[:k]); err != nil {
			return 0, err
		}
		c.n -= k
	}
	return len(p), nil
}

// Alerts are the confirmed problems, and the check itself failing twice in a row.
func (d *Doctor) Alerts(now time.Time) []Alert {
	st := d.state()
	var out []Alert
	if st.Report != nil {
		for _, f := range st.Report.Problems() {
			if !st.Confirmed[f.ID] {
				continue
			}
			out = append(out, Alert{Key: "doctor:" + f.ID, Level: f.Level, Title: "Kontrol: " + f.Title,
				Detail: f.Detail, Link: "/server/check", Since: st.Seen[f.ID]})
		}
	}
	if st.Fails >= 2 {
		out = append(out, Alert{Key: "doctor:run", Level: "warn", Title: "Sunucu kontrolü çalışmadı",
			Detail: st.Err, Link: "/server/check", Since: st.ErrSince})
	}
	return out
}

// StatusPart is the check for the status file: the confirmed problems.
func (d *Doctor) StatusPart() *monitor.StatusDoctor {
	st := d.state()
	if st.Report == nil {
		return nil
	}
	out := &monitor.StatusDoctor{Time: st.Report.Time.UTC().Truncate(time.Second)}
	for _, f := range st.Report.Problems() {
		if !st.Confirmed[f.ID] {
			continue
		}
		if f.Level == doctor.Bad {
			out.Bad++
		} else {
			out.Warn++
		}
		out.Problems = append(out.Problems, clipMsg(f.Title, 200))
	}
	return out
}
