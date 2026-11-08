package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// End dates (Faz P1b, panel-tasarim §28.3): a site may have a last day. The chat hears
// of it 7 and 3 days before and on the day, when it is asked whether to park the site;
// unless it is parked or given more time, it is asked again every morning — the site
// stays up meanwhile: nobody's site goes offline without a yes. What the chat was told
// is kept on disk, so a restart neither repeats a message nor skips one owed.
const (
	DefaultExpiryState = "/var/lib/footops/expiry.json"
	// remindHour is when (server time) the chat hears of end dates.
	remindHour = 10
	// remindRetry: a message that could not go is tried again after this.
	remindRetry = 30 * time.Minute
	// expiryFirst lets the agent's start pass before the first look.
	expiryFirst = time.Minute
)

// remindStages are the days before the end date the chat is told: the last one is the
// day itself, which asks about parking.
var remindStages = []int{7, 3, 0}

// expiryTold is what the chat was told about a site's end date.
type expiryTold struct {
	Expires string `json:"expires"` // the end date it was told of
	// Said: how many of remindStages it has heard (3: the day's question too).
	Said int `json:"said"`
	// Day is the last message's day: once the end date has passed, one a day.
	Day string `json:"day,omitempty"`
}

type expiryState struct {
	Sites map[string]expiryTold `json:"sites,omitempty"`
}

// Expiry tells the chat of end dates and parks or brings back sites (for the panel and
// the chat alike).
type Expiry struct {
	Sites *SiteOps
	Jobs  *Jobs
	// Backups takes a site's definition after a park or an unpark (nil: none).
	Backups *Backups
	// Notify is the chat (nil: none; the panel still shows end dates and alerts).
	Notify *Notifier
	Audit  *Audit
	// StatePath keeps what the chat was told; empty: memory only.
	StatePath string
	Log       *log.Logger
	// Changed is called once a park or unpark has ended: the dashboard and the status
	// file learn of it at once.
	Changed func()

	now      func() time.Time
	initOnce sync.Once
	wake     chan struct{}
	mu       sync.Mutex
	st       expiryState
	stLoaded bool
	saveErr  string
	retryAt  time.Time
	// readErr: the sites could not be read at the last look (since readFailing).
	readErr     string
	readFailing time.Time
}

func (e *Expiry) init() {
	e.initOnce.Do(func() {
		if e.now == nil {
			e.now = time.Now
		}
		e.wake = make(chan struct{}, 1)
	})
}

func (e *Expiry) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Printf(format, args...)
	}
}

// Kick looks again soon: an end date changed (one set to today asks at once).
func (e *Expiry) Kick() {
	e.init()
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run looks at the end dates every day at remindHour (and once soon after the start,
// when kicked, and after a message that did not go) until ctx is done.
func (e *Expiry) Run(ctx context.Context) {
	e.init()
	t := time.NewTimer(expiryFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.wake:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		e.Check(ctx)
		t.Reset(e.nextWake(e.now()))
	}
}

// nextWake is how long to sleep: until the next remindHour, or a retry if sooner.
func (e *Expiry) nextWake(now time.Time) time.Duration {
	local := now.Local()
	next := time.Date(local.Year(), local.Month(), local.Day(), remindHour, 0, 0, 0, local.Location())
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	e.mu.Lock()
	if !e.retryAt.IsZero() && e.retryAt.Before(next) {
		next = e.retryAt
	}
	e.mu.Unlock()
	return max(next.Sub(now), time.Second)
}

// stagesDue is how many of remindStages a site left days before its end has reached.
func stagesDue(left int) int {
	n := 0
	for _, s := range remindStages {
		if left <= s {
			n++
		}
	}
	return n
}

// stagesAtSet is what a newly given end date counts as told: the stage the site is in
// already (it was just decided), but never the day's question.
func stagesAtSet(left int) int {
	return min(stagesDue(left), len(remindStages)-1)
}

// Check tells the chat what is due of the end dates (from remindHour on).
func (e *Expiry) Check(ctx context.Context) {
	e.init()
	now := e.now()
	local := now.Local()
	terms, err := e.Sites.Terms()
	e.mu.Lock()
	if err != nil {
		// One unreadable footops.json fails the whole list: tried again soon, and an
		// alert says why no reminder goes.
		if e.readErr != err.Error() {
			e.logf("süreler okunamadı (hatırlatmalar bekliyor): %v", err)
		}
		if e.readFailing.IsZero() {
			e.readFailing = now
		}
		e.readErr, e.retryAt = err.Error(), now.Add(remindRetry)
		e.mu.Unlock()
		return
	}
	e.readErr, e.readFailing = "", time.Time{}
	e.mu.Unlock()
	st := e.state()
	today := local.Format(sites.DayLayout)
	changed := false
	seen := map[string]bool{}
	retry := false
	for _, t := range terms {
		left, ok := sites.DaysLeft(t.Expires, local)
		if !ok || t.Parked != nil {
			continue // no end, or parked: nothing to tell (a new end date starts afresh)
		}
		seen[t.Name] = true
		told, had := st.Sites[t.Name]
		if !had || told.Expires != t.Expires {
			told = expiryTold{Expires: t.Expires, Said: stagesAtSet(left)}
			st.Sites[t.Name], changed = told, true
		}
		due := stagesDue(left)
		owed := due > told.Said || (due == len(remindStages) && left < 0 && told.Day != today)
		if !owed || local.Hour() < remindHour || e.Notify == nil {
			continue
		}
		if err := e.Notify.SendSiteNotice(ctx, t, left, local); err != nil {
			if !errors.Is(err, errNoNotify) {
				e.logf("süre hatırlatması gönderilemedi (%s): %v", t.Name, err)
				retry = true
			}
			continue // not told: again at the next look
		}
		told.Said, told.Day = due, today
		st.Sites[t.Name], changed = told, true
	}
	for name := range st.Sites {
		if !seen[name] {
			delete(st.Sites, name)
			changed = true
		}
	}
	e.mu.Lock()
	e.retryAt = time.Time{}
	if retry {
		e.retryAt = now.Add(remindRetry)
	}
	e.mu.Unlock()
	if changed {
		e.saveState(st)
	}
}

// state returns a copy of the state (read from StatePath the first time).
func (e *Expiry) state() expiryState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.stLoaded {
		e.stLoaded = true
		if e.StatePath != "" {
			if data, err := os.ReadFile(e.StatePath); err == nil {
				_ = json.Unmarshal(data, &e.st) // a broken file starts afresh: at worst a stage goes untold
			}
		}
	}
	out := expiryState{Sites: make(map[string]expiryTold, len(e.st.Sites))}
	for k, v := range e.st.Sites {
		out.Sites[k] = v
	}
	return out
}

// saveState makes st the state and writes it through; a failing write is logged once.
func (e *Expiry) saveState(st expiryState) {
	e.mu.Lock()
	e.st, e.stLoaded = st, true
	e.mu.Unlock()
	if e.StatePath == "" {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(e.StatePath), 0o700); err == nil {
			err = writeSynced(e.StatePath, append(data, '\n'))
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case err != nil && e.saveErr != err.Error():
		e.saveErr = err.Error()
		e.logf("süre hatırlatma durumu kaydedilemedi (bellekte tutuluyor): %v", err)
	case err == nil:
		e.saveErr = ""
	}
}

// Alerts: a site whose end date has come and that is not parked (in the panel and
// /durum; the chat has its own message, see Notifiable), and the sites unreadable at
// the last look.
func (e *Expiry) Alerts(now time.Time) []Alert {
	terms, _ := e.Sites.Terms()
	local := now.Local()
	var out []Alert
	e.mu.Lock()
	if e.readErr != "" {
		out = append(out, Alert{Key: "expiry", Level: "warn", Title: "Sitelerin bitiş tarihleri okunamıyor",
			Detail: clipMsg(e.readErr, 300) + " — süre hatırlatmaları gönderilemiyor; yarım saatte bir yeniden denenir.",
			Link:   "/sites", Since: e.readFailing})
	}
	e.mu.Unlock()
	for _, t := range terms {
		left, ok := sites.DaysLeft(t.Expires, local)
		if !ok || left > 0 || t.Parked != nil {
			continue
		}
		day, _ := time.ParseInLocation(sites.DayLayout, t.Expires, local.Location())
		title := t.Name + " sitesinin süresi bugün doluyor"
		if left < 0 {
			title = fmt.Sprintf("%s sitesinin süresi %d gün önce doldu (%s)", t.Name, -left, dayTR(t.Expires))
		}
		out = append(out, Alert{Key: "expiry:" + t.Name, Level: "warn", Title: title,
			Detail: "Site açık. Parka almak ya da süresini uzatmak için sitenin sayfasına git (Telegram her gün " +
				fmt.Sprintf("%02d:00", remindHour) + "'da sorar).",
			Link: "/sites/" + t.Name, Since: day})
	}
	return out
}

// ---- what the panel and the chat do ----

// Term is one site's end date and parking.
func (e *Expiry) Term(site string) (SiteTerm, bool) {
	e.Sites.dropTerms() // a decision is made on what the files say now
	list, err := e.Sites.Terms()
	if err != nil {
		return SiteTerm{}, false
	}
	for _, t := range list {
		if t.Name == site {
			return t, true
		}
	}
	return SiteTerm{}, false
}

// SetExpiry gives the site a new end date ("" : none) — with was, only if it still has
// that one (SiteOps.SetExpiry) — and records who did.
func (e *Expiry) SetExpiry(site, to string, was *string, actor Actor) error {
	_, err := e.Sites.SetExpiry(site, to, was)
	detail := "süresiz"
	if to != "" {
		detail = "bitiş " + dayTR(to)
	}
	e.record(AuditEntry{Actor: actor, Action: "site.expiry", Target: site, OK: err == nil, Detail: errOr(err, detail)})
	if err == nil {
		e.Kick()
	}
	return err
}

// StartPark parks the site as a job; done (may be nil) hears how it ended.
func (e *Expiry) StartPark(site string, actor Actor, done func(Job)) (string, error) {
	return e.Jobs.Start(JobSpec{Kind: "site.park", Target: site, Actor: actor}, func(ctx context.Context, t *Task) error {
		return e.Sites.Park(ctx, site, actor.User, t.Step)
	}, e.finished(done))
}

// StartUnpark brings the site back with the end date to ("" : none) as a job.
func (e *Expiry) StartUnpark(site, to string, actor Actor, done func(Job)) (string, error) {
	return e.Jobs.Start(JobSpec{Kind: "site.unpark", Target: site, Actor: actor}, func(ctx context.Context, t *Task) error {
		return e.Sites.Unpark(ctx, site, to, t.Step)
	}, e.finished(done))
}

func (e *Expiry) finished(done func(Job)) func(Job) {
	return func(j Job) {
		e.record(AuditEntry{Actor: j.Actor, Action: j.Kind, Target: j.Target, OK: j.Err == "", Detail: j.Err})
		e.Kick()
		if e.Changed != nil {
			e.Changed()
		}
		if done != nil {
			done(j)
		}
		e.backupDef(j.Target)
	}
}

// parkActor is who the definition's backup after a park or an unpark is recorded as.
var parkActor = Actor{User: "footops", Via: "park"}

// backupDef backs up the site's definition at once, after a park or an unpark (also a
// failed one: the record may have changed). No repository, a pause, a rehearsal or
// another job running: the night run takes it.
func (e *Expiry) backupDef(site string) {
	if e.Backups == nil {
		return
	}
	if _, ok := e.Backups.active(); !ok {
		return
	}
	p, err := e.Backups.DefPlan(site)
	if err == nil {
		_, err = e.Backups.Start(p, parkActor)
	}
	if err != nil && !errors.Is(err, ErrBusy) {
		e.logf("%s sitesinin tanımı yedeklenemedi (gece yedeği alır): %v", site, err)
	}
}

func (e *Expiry) record(a AuditEntry) {
	if e.Audit == nil {
		return
	}
	if err := e.Audit.Record(a); err != nil {
		e.logf("denetim kaydı yazılamadı: %v", err)
	}
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}

// ---- the words ----

var weekdayTR = [...]string{"Pazar", "Pazartesi", "Salı", "Çarşamba", "Perşembe", "Cuma", "Cumartesi"}

// dayLongTR is "16.11.2026 Pazartesi".
func dayLongTR(day string) string {
	t, err := time.Parse(sites.DayLayout, day)
	if err != nil {
		return day
	}
	return t.Format("02.01.2006") + " " + weekdayTR[t.Weekday()]
}

// leftTR says how an end date stands: "7 gün kaldı", "bugün doluyor", "2 gün önce doldu".
func leftTR(left int) string {
	switch {
	case left > 0:
		return fmt.Sprintf("%d gün kaldı", left)
	case left == 0:
		return "bugün doluyor"
	}
	return fmt.Sprintf("%d gün önce doldu", -left)
}

// sortTerms orders sites by how soon they end (parked ones first, then no end last).
func sortTerms(list []SiteTerm, now time.Time) {
	key := func(t SiteTerm) (int, int) {
		if t.Parked != nil {
			return 0, 0
		}
		if left, ok := sites.DaysLeft(t.Expires, now); ok {
			return 1, left
		}
		return 2, 0
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, x := key(list[i])
		b, y := key(list[j])
		if a != b {
			return a < b
		}
		if x != y {
			return x < y
		}
		return strings.Compare(list[i].Name, list[j].Name) < 0
	})
}
