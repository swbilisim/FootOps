package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// The watcher's heartbeat (Y4b, worker/): izleme/nobetci.json next to the status file,
// which the outside watcher rewrites every 5 minutes. The agent reads it after each
// status file it writes: the panel shows what the watcher sees, and a heartbeat gone old
// is an alert — without it a watcher that stopped would look like all being well.
const (
	WatcherKey = "izleme/nobetci.json"
	// watcherSilentAfter: six of the watcher's beats missed.
	watcherSilentAfter = 30 * time.Minute
	// watcherTroubleAfter: the watcher failing to send its messages (or keep its state)
	// this long is an alert — it would otherwise look alive and say nothing.
	watcherTroubleAfter = 15 * time.Minute
	// watcherKnown: a heartbeat read longer ago than this says nothing about now (the
	// reads follow the status file, every 5 minutes); its alert stays as it was.
	watcherKnown = 15 * time.Minute
	watcherMax   = 256 << 10
)

// WatcherBeat is izleme/nobetci.json (worker/src/logic.js, beat()).
type WatcherBeat struct {
	Format int       `json:"format"`
	Time   time.Time `json:"time"`
	Every  int       `json:"every"`    // seconds until the next one
	Visits int       `json:"interval"` // seconds between visits of a working domain
	// Domains are this server's, as the watcher judges them.
	Domains []WatcherDomain `json:"domains"`
	// The watcher's own trouble in the run that wrote this: its messages refused (since
	// SendFailing), its state not saved.
	SendError   string    `json:"send_error,omitempty"`
	SendFailing time.Time `json:"send_failing,omitzero"`
	StateError  string    `json:"state_error,omitempty"`
}

// WatcherDomain is one domain seen from outside: OK when its last visit was answered;
// Down when it failed often enough for the chat to hear of it.
type WatcherDomain struct {
	Name   string    `json:"name"`
	Site   string    `json:"site"`
	OK     bool      `json:"ok"`
	Down   bool      `json:"down,omitempty"`
	Code   string    `json:"code,omitempty"`
	Reason string    `json:"reason,omitempty"`
	Since  time.Time `json:"since,omitempty"`
	Note   string    `json:"note,omitempty"` // e.g. a Cloudflare challenge answered the visit
}

// Failing lists the domains whose last visit was not answered (for the panel).
func (b *WatcherBeat) Failing() []WatcherDomain {
	var out []WatcherDomain
	for _, d := range b.Domains {
		if !d.OK {
			out = append(out, d)
		}
	}
	return out
}

// Minutes is how often a working domain is visited (for the panel); 0 if not said.
func (b *WatcherBeat) Minutes() int { return b.Visits / 60 }

// Blind lists the domains whose visits Cloudflare answered for the site (a challenge):
// up as far as anyone can tell, but not watched.
func (b *WatcherBeat) Blind() []WatcherDomain {
	var out []WatcherDomain
	for _, d := range b.Domains {
		if d.OK && d.Note != "" {
			out = append(out, d)
		}
	}
	return out
}

// Trouble is the watcher's own problem, in a few words ("" when none).
func (b *WatcherBeat) Trouble() string {
	var parts []string
	if b.SendError != "" {
		parts = append(parts, "Telegram'a mesaj gönderemiyor ("+clipMsg(b.SendError, 200)+")")
	}
	if b.StateError != "" {
		parts = append(parts, "durumunu kaydedemiyor ("+clipMsg(b.StateError, 200)+"), aynı haberi yeniden gönderebilir")
	}
	return strings.Join(parts, "; ")
}

// StatusFile is izleme/durum.json.
type StatusFile struct {
	Format  int          `json:"format"`
	Server  string       `json:"server"`
	FootOps string       `json:"footops"`
	Time    time.Time    `json:"time"`
	Every   int          `json:"every"` // seconds until the next one
	Sites   []StatusSite `json:"sites"`
	// SitesError: the site list could not be read; Sites is then empty and the watcher
	// keeps the domains it had (it would otherwise take every site for removed).
	SitesError string       `json:"sites_error,omitempty"`
	Backup     StatusBackup `json:"backup"`
}

// StatusSite is a site and the state of its container.
type StatusSite struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains"`
	// State: running, unhealthy, restarting, paused (frozen for a backup), stopped,
	// missing (no container) or parked (switched off on purpose, Faz P1b: the watcher
	// leaves it alone).
	State string `json:"state"`
}

// StatusBackup is how the backups stand.
type StatusBackup struct {
	Ready  bool `json:"ready"` // set up and the password confirmed
	Paused bool `json:"paused,omitempty"`
	// LastOK is the newest part saved; NightDone the last night run's date.
	LastOK    time.Time `json:"last_ok,omitzero"`
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
	// The watcher's heartbeat as last read (a failed read keeps it); none: the bucket
	// has none; readAt: the last read that worked.
	watcher    *WatcherBeat
	watcherErr string
	none       bool
	readAt     time.Time
	// The watcher's alerts as last judged on a fresh read (kept while reads fail), and
	// since when the agent sees the watcher in trouble.
	silentOn, troubleOn bool
	troubleSince        time.Time
}

// StatusFileState is how writing the status file goes (for the panel).
type StatusFileState struct {
	Key       string    `json:"key"` // bucket/…/izleme/durum.json; empty: no repository
	LastOK    time.Time `json:"last_ok,omitempty"`
	LastErr   string    `json:"last_error,omitempty"`
	LastErrAt time.Time `json:"last_error_at,omitempty"`
	Failing   time.Time `json:"failing,omitempty"`
	// Watcher is the outside watcher's last heartbeat; WatcherNone: the bucket has none
	// (no watcher set up, or it has not written yet).
	Watcher     *WatcherBeat `json:"watcher,omitempty"`
	WatcherNone bool         `json:"watcher_none,omitempty"`
	WatcherErr  string       `json:"watcher_error,omitempty"`
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
		if list, err := w.Sites(); err != nil {
			out.SitesError = clipMsg(err.Error(), 200)
		} else {
			for _, spec := range list {
				st := siteState(spec, byName)
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

// siteState is the site's state for the status file and /durum: parked, or its
// container's.
func siteState(spec sites.Spec, byName map[string]ContainerInfo) string {
	if spec.IsParked() {
		return "parked"
	}
	if c, ok := byName[spec.Container()]; ok {
		return containerState(c)
	}
	return "missing"
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
	// A rehearsal writes nowhere: next to the old server's backups it would pass for it.
	if w.Backups == nil || w.Backups.Rehearsal.Active() {
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
				pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				err = obj.putIn(pctx, client, append(body, '\n'), "application/json", now)
				cancel()
				if err == nil {
					w.mu.Lock()
					w.region = obj.region
					w.mu.Unlock()
					// Same server, same connection: the heartbeat costs one small GET.
					w.readWatcher(ctx, client, sec, obj.region, now)
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
	if w.watcher != nil {
		b := *w.watcher
		b.Domains = append([]WatcherDomain(nil), b.Domains...)
		out.Watcher = &b
	}
	out.WatcherNone, out.WatcherErr = w.none, w.watcherErr
	return out
}

// readWatcher reads the outside watcher's heartbeat. A read that fails keeps the last
// one: the alert stands on what is known (a write failing too says why, monitor:status).
func (w *StatusWriter) readWatcher(ctx context.Context, c *http.Client, sec backupSecret, region string, now time.Time) {
	obj, err := s3ObjectOf(sec, WatcherKey)
	var data []byte
	if err == nil {
		obj.region = region
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		data, err = obj.get(gctx, c, watcherMax, now)
		cancel()
	}
	var beat WatcherBeat
	if err == nil {
		if json.Unmarshal(data, &beat) != nil || beat.Format < 1 || beat.Time.IsZero() {
			err = errors.New("biçimi tanınmadı")
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case errors.Is(err, errNoObject):
		w.watcher, w.none, w.watcherErr, w.readAt = nil, true, "", now
		w.troubleSince = time.Time{}
	case err != nil:
		msg := maskRepo(sec.Repository, err.Error())
		if msg != w.watcherErr {
			w.logf("dış nöbetçinin dosyası okunamadı: %s", msg)
		}
		w.watcherErr = msg
	default:
		w.watcher, w.none, w.watcherErr, w.readAt = &beat, false, "", now
		switch {
		case beat.Trouble() == "":
			w.troubleSince = time.Time{}
		case w.troubleSince.IsZero():
			// The watcher says since when its messages fail; the agent's own first sight
			// stands in for a state it could not save.
			w.troubleSince = now
			if !beat.SendFailing.IsZero() && beat.SendFailing.Before(now) {
				w.troubleSince = beat.SendFailing
			}
		}
	}
}

// Alerts raises one alert once writes have failed for a while, and one when the outside
// watcher's heartbeat has gone old.
func (w *StatusWriter) Alerts(now time.Time) []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Alert
	if !w.failing.IsZero() && now.Sub(w.failing) >= statusAlertAfter {
		detail := w.lastErr
		if !strings.HasSuffix(detail, ".") {
			detail += "."
		}
		out = append(out, Alert{Key: "monitor:status", Level: "warn", Title: "Durum dosyası yedek kovasına yazılamıyor",
			Detail: detail + " Dış nöbetçi bu sunucudan haber alamaz.", Link: "/notify", Since: w.failing})
	}
	// Judged only on a fresh read: a heartbeat not read for a while (the bucket out of
	// reach — monitor:status says so) tells nothing about the watcher, and an alert
	// stays as it was rather than clear for no reason.
	b := w.watcher
	if !w.readAt.IsZero() && now.Sub(w.readAt) <= watcherKnown {
		w.silentOn = b != nil && now.Sub(b.Time) >= watcherSilentAfter
		w.troubleOn = b != nil && !w.troubleSince.IsZero() && now.Sub(w.troubleSince) >= watcherTroubleAfter
	}
	if w.silentOn && b != nil {
		out = append(out, Alert{Key: "monitor:watcher", Level: "warn", Title: "Dış nöbetçiden haber yok",
			Detail: fmt.Sprintf("Son yoklaması %s önce (%s). Cloudflare Worker durmuş olabilir: bir site çalışmazsa ya da bu sunucu susarsa "+
				"haber veren olmaz. Cloudflare panelinde Workers → footops-nobetci → Logs'a bak.", spanTR(now.Sub(b.Time)), b.Time.Local().Format("02.01 15:04")),
			Link: "/notify", Since: b.Time.Add(watcherSilentAfter)})
	}
	if w.troubleOn && b != nil && b.Trouble() != "" {
		out = append(out, Alert{Key: "monitor:watcher-trouble", Level: "warn", Title: "Dış nöbetçi sorunlu",
			Detail: "Nöbetçi " + b.Trouble() + ". Bir site çalışmazsa haberi gelmeyebilir; Cloudflare panelinde Workers → footops-nobetci → Logs'a bak.",
			Link:   "/notify", Since: w.troubleSince.Add(watcherTroubleAfter)})
	}
	return out
}
