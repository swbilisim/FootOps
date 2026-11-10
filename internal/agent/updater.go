package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/update"
)

// Updates (Faz U3, panel-tasarim §31.3): once a day, and when the panel asks, the agent
// looks for a newer signed release; it downloads and checks the binary and asks it what
// it would change (its plan, read-only) before anyone is asked, and the chat hears of it
// once. "Güncelle" starts the transient service footops-update, which restarts this
// agent: the agent that comes up after settles the job from the update's record
// (run.json) and tells the audit log and the chat how it went.
const (
	updateStateName = "state.json"
	updateHour      = 9               // the daily check: 09:00 plus a minute of the server's own
	updateFirst     = 3 * time.Minute // after the agent starts
	updateRetry     = 2 * time.Hour   // after a check that failed
	updateTimeout   = 15 * time.Minute
	// updateFailedFor: how long a failed update stays an alert (a later one clears it).
	updateFailedFor = 3 * 24 * time.Hour
	updateLogLines  = 200
)

// Updater checks for releases and starts updates.
type Updater struct {
	Dir string // update.Dir
	// Installed is the installed binary (update.InstalledBin), which oversees updates.
	Installed string
	Source    update.Source
	Running   string // this agent's version
	Arch      string // runtime.GOARCH
	Jobs      *Jobs
	Audit     *Audit
	Notify    *Notifier // nil: no chat
	Server    string    // the server's code: its daily minute
	Log       *log.Logger
	// Plan asks a release's binary what applying it would change (a read-only transient
	// service); Verify runs its `version`; Start starts the update's service; Active
	// tells whether it runs. Tests replace them.
	Plan   func(ctx context.Context, bin string) (update.Plan, error)
	Verify func(ctx context.Context, bin, version string) error
	Start  func(ctx context.Context, args []string) error
	Active func(ctx context.Context) bool

	now      func() time.Time
	initOnce sync.Once
	kick     chan struct{}
	mu       sync.Mutex
	saveMu   sync.Mutex // one change of the state at a time (mutate)
	st       updateState
	loaded   bool
	checking bool
	next     time.Time
	saveErr  string
}

type updateState struct {
	Checked  time.Time `json:"checked,omitzero"`
	CheckErr string    `json:"check_error,omitempty"`
	// Unverified: the newest release's signature did not hold — someone else's release,
	// or one changed on the way (an alert).
	Unverified string            `json:"unverified,omitempty"`
	Latest     *release.Manifest `json:"latest,omitempty"`
	Ready      *UpdateReady      `json:"ready,omitempty"`
	// Told is the version the chat heard of; Reported the apply (its start) the audit
	// log and the chat heard the outcome of.
	Told     string `json:"told,omitempty"`
	Reported string `json:"reported,omitempty"`
}

// UpdateReady is a release downloaded, checked and planned.
type UpdateReady struct {
	Version string      `json:"version"`
	Bin     string      `json:"bin"`
	Plan    update.Plan `json:"plan"`
	PlanErr string      `json:"plan_error,omitempty"`
	At      time.Time   `json:"at"`
}

// UpdateStatus is the Güncelleme page's.
type UpdateStatus struct {
	Running    string            `json:"running"`
	Latest     *release.Manifest `json:"latest,omitempty"`
	Newer      bool              `json:"newer"`
	Ready      *UpdateReady      `json:"ready,omitempty"`
	Checking   bool              `json:"checking"`
	Checked    time.Time         `json:"checked,omitzero"`
	CheckErr   string            `json:"check_error,omitempty"`
	Unverified string            `json:"unverified,omitempty"`
	Next       time.Time         `json:"next,omitzero"`
	// Run is the last apply, Log its log's end; Applying: one runs now.
	Run      *update.Run `json:"run,omitempty"`
	Log      []string    `json:"log,omitempty"`
	Applying bool        `json:"applying"`
	// Prev is the kept previous binary's version (going back), if any.
	Prev string `json:"prev,omitempty"`
}

var (
	errNotReady   = errors.New("bu sürüm hazır değil: önce denetlenip indirilmeli")
	errUpdateBusy = errors.New("bir güncelleme zaten sürüyor")
)

func (u *Updater) init() {
	u.initOnce.Do(func() { u.kick = make(chan struct{}, 1) })
}

func (u *Updater) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

func (u *Updater) logf(format string, args ...any) {
	if u.Log != nil {
		u.Log.Printf(format, args...)
	}
}

func (u *Updater) state() updateState {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.loaded {
		u.loaded = true
		if data, err := os.ReadFile(filepath.Join(u.Dir, updateStateName)); err == nil {
			_ = json.Unmarshal(data, &u.st) // a broken file starts afresh: the next check fills it
		}
	}
	return u.st
}

// mutate changes the state and writes it through: read, change and write under one
// lock, so a check that took minutes cannot undo what was settled meanwhile. f replaces
// what it changes (the pointers are shared with readers), never changes it in place.
func (u *Updater) mutate(f func(*updateState)) {
	u.saveMu.Lock()
	defer u.saveMu.Unlock()
	st := u.state()
	f(&st)
	u.mu.Lock()
	u.st, u.loaded = st, true
	u.mu.Unlock()
	data, err := json.Marshal(st)
	if err == nil {
		if err = os.MkdirAll(u.Dir, 0o700); err == nil {
			err = writeSynced(filepath.Join(u.Dir, updateStateName), append(data, '\n'))
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case err != nil && u.saveErr != err.Error():
		u.saveErr = err.Error()
		u.logf("güncelleme durumu kaydedilemedi (bellekte tutuluyor): %v", err)
	case err == nil:
		u.saveErr = ""
	}
}

func (u *Updater) active(ctx context.Context) bool {
	if u.Active != nil {
		return u.Active(ctx)
	}
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", update.Unit+".service").Output()
	s := strings.TrimSpace(string(out))
	return s == "active" || s == "activating"
}

// Status is the Güncelleme page's view; brief (the dashboard's card, every few seconds)
// leaves out the log and asks systemd nothing.
func (u *Updater) Status(ctx context.Context, brief bool) UpdateStatus {
	st := u.state()
	out := UpdateStatus{Running: u.Running, Latest: st.Latest, Ready: st.Ready, Checked: st.Checked,
		CheckErr: st.CheckErr, Unverified: st.Unverified, Prev: update.PrevVersion(u.Dir)}
	if out.Prev == u.Running {
		out.Prev = "" // going back would change nothing
	}
	if st.Latest != nil {
		out.Newer = release.Newer(st.Latest.Version, u.Running)
	}
	u.mu.Lock()
	out.Checking, out.Next = u.checking, u.next
	u.mu.Unlock()
	if r, err := update.ReadRun(u.Dir); err == nil && r != nil {
		out.Run = r
		out.Applying = !r.Final()
		if !brief {
			out.Log = update.Tail(r.Log, updateLogLines)
		}
	}
	if !brief {
		out.Applying = out.Applying || u.active(ctx)
	}
	return out
}

// CheckNow asks for a check at once; false if one runs.
func (u *Updater) CheckNow() bool {
	u.init()
	u.mu.Lock()
	checking := u.checking
	u.mu.Unlock()
	if checking {
		return false
	}
	select {
	case u.kick <- struct{}{}:
	default:
	}
	return true
}

// Run settles the last update, then checks until ctx is done.
func (u *Updater) Run(ctx context.Context) {
	u.init()
	u.Recover(ctx)
	wait := updateFirst
	for {
		u.mu.Lock()
		u.next = u.clock().Add(wait)
		u.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		case <-u.kick:
			t.Stop()
		}
		u.Check(ctx)
		wait = u.nextWake(u.clock())
	}
}

// nextWake: the daily check at updateHour and a minute of the server's own (servers do
// not ask GitHub at the same moment), sooner after a failure.
func (u *Updater) nextWake(now time.Time) time.Duration {
	h := fnv.New32a()
	h.Write([]byte(u.Server))
	local := now.Local()
	daily := time.Date(local.Year(), local.Month(), local.Day(), updateHour, int(h.Sum32()%60), 0, 0, local.Location())
	if !daily.After(local) {
		daily = daily.AddDate(0, 0, 1)
	}
	wait := daily.Sub(local)
	if st := u.state(); st.CheckErr != "" {
		wait = min(wait, updateRetry)
	}
	return wait
}

// Check looks for a newer release and readies it.
func (u *Updater) Check(ctx context.Context) {
	u.mu.Lock()
	if u.checking {
		u.mu.Unlock()
		return
	}
	u.checking = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.checking = false
		u.mu.Unlock()
	}()
	cctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	r, err := u.Source.Latest(cctx)
	if ctx.Err() != nil {
		return
	}
	now := u.clock()
	switch {
	case errors.Is(err, update.ErrNotVerified):
		u.logf("güncelleme: %v", err)
		u.mutate(func(st *updateState) { st.Checked, st.Unverified, st.CheckErr = now, clipMsg(err.Error(), 300), "" })
		return
	case err != nil:
		u.mutate(func(st *updateState) { st.Checked, st.CheckErr = now, clipMsg(err.Error(), 300) })
		return
	}
	m := r.Manifest
	if !release.Newer(m.Version, u.Running) {
		u.mutate(func(st *updateState) {
			st.Checked, st.Unverified, st.CheckErr, st.Latest, st.Ready = now, "", "", &m, nil
		})
		u.cleanup("")
		return
	}
	// Downloaded again when it is not there and right; planned again when its plan
	// failed (a time or memory limit, a service left from a restart).
	ready := u.state().Ready
	binOK := ready != nil && ready.Version == m.Version && m.Files[release.BinaryName(u.Arch)].Check(ready.Bin) == nil
	if !binOK || ready.PlanErr != "" {
		bin := ""
		if binOK {
			bin = ready.Bin
		} else {
			bin, err = u.Source.Download(cctx, r, u.Arch, u.Dir)
			if err == nil {
				err = u.Verify(cctx, bin, m.Version)
			}
			if err != nil {
				u.mutate(func(st *updateState) {
					st.Checked, st.Unverified, st.Latest, st.CheckErr = now, "", &m, clipMsg("indirilemedi: "+err.Error(), 300)
				})
				return
			}
		}
		next := &UpdateReady{Version: m.Version, Bin: bin, At: u.clock()}
		if plan, err := u.Plan(cctx, bin); err != nil {
			next.PlanErr = clipMsg(err.Error(), 300)
		} else {
			next.Plan = plan
		}
		ready = next
		u.cleanup(m.Version)
	}
	told := ""
	u.mutate(func(st *updateState) {
		st.Checked, st.Unverified, st.CheckErr, st.Latest, st.Ready = now, "", "", &m, ready
		told = st.Told
	})
	if told != m.Version && u.Notify != nil {
		err := u.Notify.SendNotice(ctx, "🆕", u.newsText(m, ready), "/server/update")
		if err == nil || errors.Is(err, errNoNotify) {
			u.mutate(func(st *updateState) { st.Told = m.Version })
		}
	}
}

func (u *Updater) newsText(m release.Manifest, ready *UpdateReady) string {
	var b strings.Builder
	fmt.Fprintf(&b, "FootOps %s hazır (şu an %s).", m.Version, u.Running)
	if m.Notes != "" {
		b.WriteString("\n" + clipMsg(m.Notes, 1500))
	}
	switch {
	case ready == nil:
	case ready.PlanErr != "":
		b.WriteString("\nPlanı çıkarılamadı: " + ready.PlanErr)
	case len(ready.Plan.Interactive) > 0:
		b.WriteString("\nBu sürüm SSH'den kurulur (" + strings.Join(ready.Plan.Interactive, ", ") + " değişiyor): sudo footops self-update")
	default:
		for _, l := range ready.Plan.Impact {
			b.WriteString("\n• " + l)
		}
		b.WriteString("\nKurmak için panelde Sunucu → Güncelleme → Güncelle.")
	}
	return b.String()
}

// cleanup removes downloaded releases but keep's and the last apply's (its log).
func (u *Updater) cleanup(keep string) {
	ents, err := os.ReadDir(u.Dir)
	if err != nil {
		return
	}
	last := ""
	if r, _ := update.ReadRun(u.Dir); r != nil {
		last = r.To
	}
	for _, e := range ents {
		v := e.Name()
		if _, ok := release.ParseVersion(v); !ok || !e.IsDir() || v == keep || v == last {
			continue
		}
		if err := os.RemoveAll(filepath.Join(u.Dir, v)); err != nil {
			u.logf("güncelleme: %s silinemedi: %v", v, err)
		}
	}
}

// Apply starts the update to version (it must be the ready one). The job ends with
// this agent; the next one settles it.
func (u *Updater) Apply(ctx context.Context, version string, actor Actor) (string, error) {
	st := u.state()
	r := st.Ready
	switch {
	case r == nil || r.Version != version || st.Latest == nil || st.Latest.Version != version:
		return "", errNotReady
	case !release.Newer(version, u.Running):
		return "", fmt.Errorf("%s, çalışan sürümden (%s) yeni değil", version, u.Running)
	case r.PlanErr != "":
		return "", fmt.Errorf("güncellemenin planı çıkarılamadı: %s", r.PlanErr)
	case len(r.Plan.Interactive) > 0:
		return "", fmt.Errorf("bu sürüm SSH'den kurulur (%s değişiyor): sudo footops self-update", strings.Join(r.Plan.Interactive, ", "))
	case u.active(ctx):
		return "", errUpdateBusy
	}
	// The binary waited on disk since it was checked: checked again.
	if err := st.Latest.Files[release.BinaryName(u.Arch)].Check(r.Bin); err != nil {
		return "", fmt.Errorf("indirilen ikili değişmiş, yeniden denetle: %v", err)
	}
	return u.start(actor, r.Bin, version, false)
}

// Back starts going back to the kept previous binary.
func (u *Updater) Back(ctx context.Context, actor Actor) (string, error) {
	prev := update.PrevVersion(u.Dir)
	switch {
	case prev == "" || prev == u.Running:
		return "", errors.New("dönülecek önceki sürüm yok")
	case u.active(ctx):
		return "", errUpdateBusy
	}
	return u.start(actor, filepath.Join(update.PrevDir(u.Dir), "footops"), prev, true)
}

// start runs the update's service for target (the installed binary oversees it) in a
// job, which holds off other work meanwhile.
func (u *Updater) start(actor Actor, target, to string, back bool) (string, error) {
	from := u.Running
	id, err := u.Jobs.Start(JobSpec{Kind: "server.update", Target: to, Actor: actor}, func(ctx context.Context, t *Task) error {
		if back {
			t.Step(fmt.Sprintf("footops %s → %s: önceki sürüme dönülüyor", from, to))
		} else {
			t.Step(fmt.Sprintf("footops %s → %s: ikili doğrulandı", from, to))
		}
		if err := u.Start(ctx, update.UnitArgs(target, from, to, t.ID(), back)); err != nil {
			return fmt.Errorf("güncelleme servisi başlatılamadı: %w", err)
		}
		t.Step("Güncelleme servisi çalışıyor (footops-update); panel birkaç saniye yeniden başlayacak, sayfa kendini yeniler")
		err := u.wait(ctx, t.ID())
		// An apply that ended without restarting this agent (refused, rolled back before
		// the panel was touched, or its service died) is told now: no later start would.
		if r, _ := update.ReadRun(u.Dir); r != nil && r.Job == t.ID() && r.Final() {
			u.settle(context.Background(), *r)
		}
		return err
	}, nil)
	if err == nil && u.Audit != nil {
		_ = u.Audit.Record(AuditEntry{Actor: actor, Action: "server.update.start", Target: from + " → " + to})
	}
	return id, err
}

// wait follows an apply this agent started: normally the agent is stopped first (the
// next one settles the job); an apply that ends before (refused, rolled back early)
// ends it here, and a service that died leaving its record open is closed here — the
// page would otherwise say "sürüyor" until the agent restarts.
func (u *Updater) wait(ctx context.Context, job string) error {
	quiet := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		r, _ := update.ReadRun(u.Dir)
		if r != nil && r.Job == job && r.Final() {
			return runOutcome(*r)
		}
		if u.active(ctx) {
			quiet = 0
			continue
		}
		if quiet++; quiet < 15 {
			continue
		}
		if r == nil || r.Job != job {
			return errors.New("güncelleme servisi başlamadı ya da kayıt bırakmadan bitti: sudo journalctl -u footops-update")
		}
		closed := u.closeCut(*r, update.PhaseFailed, "güncelleme servisi kayıt bırakmadan bitti (sudo journalctl -u footops-update)")
		return runOutcome(closed)
	}
}

// closeCut ends a record its service left open, and puts the way back right: prev.new
// holds the binary that ran before; if the installed one is another, the update got
// that far and prev.new is now the way back, else nothing was replaced.
func (u *Updater) closeCut(r update.Run, phase, why string) update.Run {
	r.Phase, r.Finished, r.Err = phase, u.clock().UTC(), why
	pending := update.PendingDir(u.Dir)
	if same, err := sameContent(filepath.Join(pending, "footops"), u.Installed); err == nil {
		if same || (phase == update.PhaseDone && r.Back) {
			_ = os.RemoveAll(pending)
		} else if err := update.Promote(u.Dir); err != nil {
			u.logf("güncelleme: geri dönüş yolu saklanamadı: %v", err)
		}
	}
	if err := update.WriteRun(u.Dir, r); err != nil {
		u.logf("güncelleme kaydı yazılamadı: %v", err)
	}
	return r
}

// sameContent reports whether two files hold the same bytes.
func sameContent(a, b string) (bool, error) {
	fa, err := release.FileOf(a)
	if err != nil {
		return false, err
	}
	fb, err := release.FileOf(b)
	if err != nil {
		return false, err
	}
	return fa == fb, nil
}

// runOutcome is an apply's end as a job's error.
func runOutcome(r update.Run) error {
	switch r.Phase {
	case update.PhaseDone:
		return nil
	case update.PhaseRolledBack:
		return fmt.Errorf("%s açılmadı; önceki sürüm (%s) yeniden çalışıyor: %s", r.To, r.From, r.Err)
	case update.PhaseRefused:
		return fmt.Errorf("güncelleme yapılmadı (hiçbir şey değişmedi): %s", r.Err)
	}
	return fmt.Errorf("güncelleme yarıda kaldı, geri dönülemedi: %s — SSH'den bakılmalı (%s)", r.Err, r.Log)
}

// Recover settles the last apply after a start: the job of the agent it restarted, the
// audit log, the chat. An apply still running is waited for in a job of its own (no
// other work starts meanwhile).
func (u *Updater) Recover(ctx context.Context) {
	r, err := update.ReadRun(u.Dir)
	if err != nil {
		u.logf("güncelleme kaydı okunamadı: %v", err)
		return
	}
	if r == nil {
		return
	}
	if !r.Final() {
		if u.active(ctx) {
			go u.await(ctx, *r)
			return
		}
		// The service is gone without an end (a reboot under it): what runs now says how
		// far it got.
		why := "güncelleme servisi yarıda kesildi (sunucu yeniden mi başladı?)"
		phase := update.PhaseFailed
		switch u.Running {
		case r.To:
			phase, why = update.PhaseDone, why+"; "+r.To+" çalışıyor"
		case r.From:
			phase, why = update.PhaseRolledBack, why+"; "+r.From+" çalışıyor — sudo footops doctor ile denetle"
		}
		closed := u.closeCut(*r, phase, why)
		r = &closed
	}
	u.settle(ctx, *r)
}

// await waits, in a job of its own (no other work starts meanwhile), for an apply that
// is still running after a start; while another job holds the queue (the recovery of
// an interrupted restore runs first), it asks again.
func (u *Updater) await(ctx context.Context, run update.Run) {
	for {
		_, err := u.Jobs.Start(JobSpec{Kind: "server.update", Target: run.To, Actor: Actor{User: "footops"}}, func(jctx context.Context, t *Task) error {
			t.Step(fmt.Sprintf("footops %s → %s güncellemesi sürüyor; bitmesi bekleniyor", run.From, run.To))
			for u.active(jctx) {
				select {
				case <-jctx.Done():
					return jctx.Err()
				case <-time.After(2 * time.Second):
				}
			}
			r, err := update.ReadRun(u.Dir)
			if err != nil || r == nil {
				return errors.New("güncelleme servisi bitti ama kaydı okunamadı")
			}
			if !r.Final() {
				closed := u.closeCut(*r, update.PhaseFailed, "güncelleme servisi kayıt bırakmadan bitti (sudo journalctl -u footops-update)")
				r = &closed
			}
			u.settle(context.Background(), *r)
			return runOutcome(*r)
		}, nil)
		if !errors.Is(err, ErrBusy) {
			if err != nil {
				u.logf("süren güncellemenin işi başlatılamadı: %v", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// settle tells the outcome once: the starting job (and a waiting one a second restart
// cut off), the audit log, the chat.
func (u *Updater) settle(ctx context.Context, r update.Run) {
	key := r.Started.UTC().Format(time.RFC3339Nano)
	if u.state().Reported == key {
		return
	}
	actor := Actor{User: "footops"}
	if job, ok := u.Jobs.Get(r.Job); ok {
		actor = job.Actor
	}
	outcome := runOutcome(r)
	msg, step := "", ""
	switch r.Phase {
	case update.PhaseDone:
		step = fmt.Sprintf("✓ footops %s çalışıyor", r.To)
	default:
		msg = outcome.Error()
		step = "✗ " + msg
	}
	u.Jobs.Settle(r.Job, msg, step)
	for _, id := range u.Jobs.InterruptedSince("server.update", r.Started) {
		u.Jobs.Settle(id, msg, step)
	}
	if u.Audit != nil {
		_ = u.Audit.Record(AuditEntry{Actor: actor, Action: "server.update", Target: r.From + " → " + r.To, Detail: msg})
	}
	if u.Notify != nil {
		icon, text := "✅", fmt.Sprintf("FootOps güncellendi: %s → %s.", r.From, r.To)
		if r.Back {
			text = fmt.Sprintf("FootOps önceki sürüme döndü: %s → %s.", r.From, r.To)
		}
		if r.Phase != update.PhaseDone {
			icon, text = "⚠️", msg
		}
		if err := u.Notify.SendNotice(ctx, icon, text, "/server/update"); err != nil && !errors.Is(err, errNoNotify) {
			u.logf("güncellemenin sonucu Telegram'a gönderilemedi: %v", err)
		}
	}
	u.mutate(func(st *updateState) { st.Reported = key })
}

// Alerts: a release whose signature does not hold, and an update that did not go well
// (for a few days, or until one does).
func (u *Updater) Alerts(now time.Time) []Alert {
	st := u.state()
	var out []Alert
	if st.Unverified != "" {
		out = append(out, Alert{Key: "update:verify", Level: "bad", Title: "En yeni FootOps sürümünün imzası tutmuyor",
			Detail: st.Unverified + ". Kurulmadı; GitHub'daki sürüme bakılmalı.", Link: "/server/update", Since: st.Checked})
	}
	if r, err := update.ReadRun(u.Dir); err == nil && r != nil && r.Final() && r.Phase != update.PhaseDone && now.Sub(r.Finished) < updateFailedFor {
		level := "warn"
		if r.Phase == update.PhaseFailed {
			level = "bad"
		}
		out = append(out, Alert{Key: "update:failed", Level: level, Title: "FootOps güncellemesi olmadı",
			Detail: runOutcome(*r).Error(), Link: "/server/update", Since: r.Finished})
	}
	return out
}

// StatusPart is the status file's: a newer release out, and whether it is ready to
// install from the panel (downloaded, checked, planned, asking for no terminal).
func (u *Updater) StatusPart() *monitor.StatusUpdate {
	st := u.state()
	if st.Latest == nil || !release.Newer(st.Latest.Version, u.Running) {
		return nil
	}
	r := st.Ready
	return &monitor.StatusUpdate{Available: st.Latest.Version,
		Ready: r != nil && r.Version == st.Latest.Version && r.PlanErr == "" && len(r.Plan.Interactive) == 0}
}

// systemdPlan asks a release's binary for its plan in a read-only transient service
// (the doctor's sandbox: it reads what the doctor reads).
func systemdPlan(ctx context.Context, bin string) (update.Plan, error) {
	var p update.Plan
	if f, err := os.OpenFile(ufwLock, os.O_CREATE|os.O_RDONLY, 0o644); err == nil {
		f.Close()
	}
	args := []string{"--unit=footops-update-plan", "--description=FootOps güncellemesinin planı", "--quiet", "--collect",
		"--wait", "--pipe", "--setenv=HOME=/tmp"}
	for _, prop := range doctorProps {
		args = append(args, "--property="+prop)
	}
	args = append(args, "--", bin, "install", "--plan")
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &capWriter{w: &stdout, n: 1 << 20}
	cmd.Stderr = &capWriter{w: &stderr, n: 64 << 10}
	if err := cmd.Run(); err != nil {
		return p, fmt.Errorf("%v: %s", err, lastLine(stderr.String()))
	}
	if err := json.Unmarshal(stdout.Bytes(), &p); err != nil {
		return p, fmt.Errorf("plan okunamadı: %v", err)
	}
	return p, nil
}

// verifyBinary runs a downloaded binary's `version`: it must say the release's.
func verifyBinary(ctx context.Context, bin, want string) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "version").Output()
	if err != nil {
		return fmt.Errorf("indirilen ikili çalışmadı: %v", err)
	}
	if f := strings.Fields(string(out)); len(f) < 2 || f[1] != want {
		return fmt.Errorf("indirilen ikili başka bir sürüm söylüyor: %q", clipMsg(strings.TrimSpace(string(out)), 100))
	}
	return nil
}

// startUnit starts the update's transient service (systemd-run returns once it runs).
func startUnit(ctx context.Context, args []string) error {
	out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, lastLine(string(out)))
	}
	return nil
}
