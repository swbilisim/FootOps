package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"
)

// auditMaxBytes: the log is rotated to <path>.1 above this size (one old generation).
const auditMaxBytes = 5 << 20

// Audit is an append-only JSON-lines log.
type Audit struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

func NewAudit(path string) *Audit { return &Audit{path: path, now: time.Now} }

// Record appends e. Free-text fields are length-limited and stripped of newlines so one
// entry is always one line.
func (a *Audit) Record(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = a.now().UTC()
	}
	e.Actor.User = clean(e.Actor.User, 64)
	e.Actor.IP = clean(e.Actor.IP, 64)
	e.Actor.Via = clean(e.Actor.Via, 16)
	e.Action = clean(e.Action, 64)
	e.Target = clean(e.Target, 128)
	e.Detail = clean(e.Detail, 512)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if st, err := os.Stat(a.path); err == nil && st.Size() > auditMaxBytes {
		if err := os.Rename(a.path, a.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Tail returns up to n entries, newest first.
func (a *Audit) Tail(n int) ([]AuditEntry, error) {
	a.mu.Lock()
	b, err := os.ReadFile(a.path)
	a.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	out := make([]AuditEntry, 0, min(n, len(all)))
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
