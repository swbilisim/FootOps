package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/version"
)

// The status file (panel-tasarim §27.8): every few minutes the agent writes
// izleme/durum.json next to its backup repository, for the outside watcher (a Cloudflare
// Worker, Y4b) — which sites run under which domains, how the backups stand, and when
// the agent last said so: a file that stops changing is the news that the server is
// gone. No secrets; one small PUT with the repository's key, no tool container.
const (
	StatusKey    = "izleme/durum.json"
	statusFormat = 1
	statusEvery  = 5 * time.Minute
	// statusFirst gives the sampler time for its first round after a start.
	statusFirst = time.Minute
	// statusAlertAfter: writes failing this long raise a panel alert (the watcher will
	// say the server is silent; this says why).
	statusAlertAfter = 20 * time.Minute
)

// StatusFile is izleme/durum.json.
type StatusFile struct {
	Format  int          `json:"format"`
	Server  string       `json:"server"`
	FootOps string       `json:"footops"`
	Time    time.Time    `json:"time"`
	Every   int          `json:"every"` // seconds until the next one
	Sites   []StatusSite `json:"sites"`
	Backup  StatusBackup `json:"backup"`
}

// StatusSite is a site and the state of its container.
type StatusSite struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	// State: running, unhealthy, restarting, paused (frozen for a backup), stopped or
	// missing (no container).
	State string `json:"state"`
}

// StatusBackup is how the backups stand.
type StatusBackup struct {
	Ready  bool `json:"ready"` // set up and the password confirmed
	Paused bool `json:"paused,omitempty"`
	// LastOK is the newest part saved; NightDone the last night run's date.
	LastOK    time.Time `json:"last_ok,omitempty"`
	NightDone string    `json:"night_done,omitempty"`
	// Problems are the backup alerts' titles.
	Problems []string `json:"problems,omitempty"`
}

// StatusWriter writes the status file.
type StatusWriter struct {
	Backups *Backups
	Sites   func() ([]sites.Spec, error)
	// Containers returns the latest sample's containers.
	Containers func(ctx context.Context) []ContainerInfo
	Server     string
	HTTP       *http.Client
	Log        *log.Logger

	now      func() time.Time
	kickOnce sync.Once
	kick     chan struct{}
	mu       sync.Mutex
	// region is the one the repository's server signs with, once learnt.
	region  string
	lastOK  time.Time
	lastErr string
	errAt   time.Time
	failing time.Time
}

// StatusFileState is how writing the status file goes (for the panel).
type StatusFileState struct {
	Key       string    `json:"key"` // bucket/…/izleme/durum.json; empty: no repository
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastErr   string    `json:"last_error,omitempty"`
	LastErrAt time.Time `json:"last_error_at,omitempty"`
	Failing   time.Time `json:"failing,omitempty"`
}

func (w *StatusWriter) clock() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *StatusWriter) logf(format string, args ...any) {
	if w.Log != nil {
		w.Log.Printf(format, args...)
	}
}

func (w *StatusWriter) kicks() chan struct{} {
	w.kickOnce.Do(func() { w.kick = make(chan struct{}, 1) })
	return w.kick
}

// Kick writes the file soon, without waiting for the next round: a site came or went,
// the repository was set up.
func (w *StatusWriter) Kick() {
	select {
	case w.kicks() <- struct{}{}:
	default:
	}
}

// Run writes the file every statusEvery (and when kicked) until ctx is done.
func (w *StatusWriter) Run(ctx context.Context) {
	t := time.NewTimer(statusFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kicks():
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		_ = w.Write(ctx)
		t.Reset(statusEvery)
	}
}

// Build puts the status together.
func (w *StatusWriter) Build(ctx context.Context) StatusFile {
	now := w.clock().UTC().Truncate(time.Second)
	out := StatusFile{Format: statusFormat, Server: w.Server, FootOps: version.Version, Time: now,
		Every: int(statusEvery / time.Second), Sites: []StatusSite{}}
	byName := map[string]ContainerInfo{}
	if w.Containers != nil {
		for _, c := range w.Containers(ctx) {
			byName[c.Name] = c
		}
	}
	if w.Sites != nil {
		if list, err := w.Sites(); err == nil {
			for _, spec := range list {
				st := "missing"
				if c, ok := byName[spec.Container()]; ok {
					st = containerState(c)
				}
				out.Sites = append(out.Sites, StatusSite{Name: spec.Name, Domains: append([]string{}, spec.Domains...), State: st})
			}
		}
	}
	sort.Slice(out.Sites, func(i, j int) bool { return out.Sites[i].Name < out.Sites[j].Name })
	if b := w.Backups; b != nil {
		bs := b.Status()
		out.Backup = StatusBackup{Ready: bs.Configured && !bs.Settings.Confirmed.IsZero(), Paused: bs.Settings.Paused, NightDone: bs.NightDone}
		for _, l := range bs.Last {
			if l.Time.After(out.Backup.LastOK) {
				out.Backup.LastOK = l.Time.UTC()
			}
		}
		for _, a := range b.Alerts(now) {
			out.Backup.Problems = append(out.Backup.Problems, a.Title)
		}
	}
	return out
}

func containerState(c ContainerInfo) string {
	switch {
	case c.State == "running" && c.Health == "unhealthy":
		return "unhealthy"
	case c.State == "running", c.State == "restarting", c.State == "paused":
		return c.State
	}
	return "stopped"
}

// Write puts the status file next to the repository; without a repository it does
// nothing.
func (w *StatusWriter) Write(ctx context.Context) error {
	if w.Backups == nil {
		return nil
	}
	sec, err := w.Backups.secret()
	if errors.Is(err, errNoBackup) {
		return nil
	}
	now := w.clock()
	if err == nil {
		var obj s3Object
		if obj, err = s3ObjectOf(sec, StatusKey); err == nil {
			w.mu.Lock()
			if w.region != "" {
				obj.region = w.region
			}
			w.mu.Unlock()
			var body []byte
			if body, err = json.MarshalIndent(w.Build(ctx), "", " "); err == nil {
				client := &http.Client{Timeout: 30 * time.Second}
				if w.HTTP != nil {
					c := *w.HTTP
					client = &c
				}
				// A redirected PUT would come back as a GET without the body, and its 2xx
				// would count as written: a redirect is an answer, not a way.
				client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
				ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err = obj.putIn(ctx, client, append(body, '\n'), "application/json", now)
				cancel()
				if err == nil {
					w.mu.Lock()
					w.region = obj.region
					w.mu.Unlock()
				}
			}
		}
		if err != nil {
			err = errors.New(maskRepo(sec.Repository, err.Error()))
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		if w.lastErr != err.Error() {
			w.logf("durum dosyası yazılamadı: %v", err)
		}
		if w.failing.IsZero() {
			w.failing = now
		}
		w.lastErr, w.errAt = err.Error(), now
		return err
	}
	if !w.failing.IsZero() {
		w.logf("durum dosyası yeniden yazılabiliyor")
	}
	w.lastOK, w.failing, w.lastErr = now, time.Time{}, ""
	return nil
}

// State reports how writing goes.
func (w *StatusWriter) State() StatusFileState {
	var out StatusFileState
	if w.Backups != nil {
		if sec, err := w.Backups.secret(); err == nil {
			if obj, err := s3ObjectOf(sec, StatusKey); err == nil {
				out.Key = obj.bucket + "/" + obj.key
			}
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	out.LastOK, out.LastErr, out.LastErrAt, out.Failing = w.lastOK, w.lastErr, w.errAt, w.failing
	return out
}

// Alerts raises one alert once writes have failed for a while.
func (w *StatusWriter) Alerts(now time.Time) []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failing.IsZero() || now.Sub(w.failing) < statusAlertAfter {
		return nil
	}
	detail := w.lastErr
	if !strings.HasSuffix(detail, ".") {
		detail += "."
	}
	return []Alert{{Key: "monitor:status", Level: "warn", Title: "Durum dosyası yedek kovasına yazılamıyor",
		Detail: detail + " Dış nöbetçi bu sunucudan haber alamaz.", Link: "/notify", Since: w.failing}}
}
