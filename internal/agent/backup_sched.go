package agent

import (
	"context"
	"errors"
	"time"
)

// The timetable (§27.3): the night run once a day at Night (server time) — every site's
// database, files (daily, or weekly on filesWeekday) and definition, the server's
// settings, retention, and weekly prune and check — and between nights, every hour at
// the same minute, the databases of sites backed up hourly or every six hours. One
// timer in the agent; nothing else runs while it waits.
//
// Backups are jobs like any other, one at a time: an hourly run that finds the agent
// busy waits for the next hour; the night run tries again every few minutes, one that
// could not reach the repository at the next hour, and one the server missed (off,
// restarting) runs once the agent is back.
const (
	nightRetry = 5 * time.Minute
	// startGrace lets the agent's start (recovery, MongoDB coming up) pass before a
	// missed night run is caught up.
	startGrace = 2 * time.Minute
	// nightOwedFor: a night run missed longer ago than this is not caught up — the next
	// night is then less than three hours away.
	nightOwedFor = 21 * time.Hour
)

// schedActor is who the timetable's runs are recorded as.
var schedActor = Actor{User: "footops", Via: "zamanlayıcı"}

// Schedule runs the timetable until ctx ends.
func (b *Backups) Schedule(ctx context.Context) {
	b.init()
	for {
		timer := time.NewTimer(b.nextWake(b.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-b.wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		if _, err := b.Tick(b.now()); err != nil {
			b.logf("yedek zamanlayıcısı: %v", err)
		}
	}
}

func (b *Backups) location() *time.Location {
	if b.loc != nil {
		return b.loc
	}
	return time.Local
}

// active reports whether the timetable runs: a repository, its password confirmed, not
// paused.
func (b *Backups) active() (BackupSettings, bool) {
	s, err := b.Settings()
	if err != nil || s.Confirmed.IsZero() || s.Paused || b.Rehearsal.Active() {
		return s, false
	}
	if _, err := b.secret(); err != nil {
		return s, false
	}
	return s, true
}

// nightOwed returns the night run due at now that has not run: the latest night time
// at or before now, unless that run is done or too long ago.
func (b *Backups) nightOwed(s BackupSettings, now time.Time) (time.Time, bool) {
	local := now.In(b.location())
	n := s.nightAt(local)
	if n.After(local) {
		n = s.nightAt(local.AddDate(0, 0, -1))
	}
	if local.Sub(n) >= nightOwedFor {
		return n, false
	}
	return n, b.state().NightDone < n.Format("2006-01-02")
}

// nextSlot is the next hourly slot after now: the night time's minute of some hour.
func (b *Backups) nextSlot(s BackupSettings, now time.Time) time.Time {
	local := now.In(b.location())
	minute := s.nightAt(local).Minute()
	next := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), minute, 0, 0, local.Location())
	for !next.After(local) {
		next = next.Add(time.Hour)
	}
	return next
}

// nextWake is how long the timetable sleeps: until the next slot, or sooner while a
// night run is owed (after the start grace and any retry delay).
func (b *Backups) nextWake(now time.Time) time.Duration {
	s, ok := b.active()
	wait := b.nextSlot(s, now).Sub(now)
	if _, owed := b.nightOwed(s, now); !ok || !owed {
		return wait
	}
	at := b.started.Add(startGrace)
	b.mu.Lock()
	if b.nightNotBefore.After(at) {
		at = b.nightNotBefore
	}
	b.mu.Unlock()
	if d := at.Sub(now); d < wait {
		wait = max(d, 0)
	}
	return wait
}

// Tick starts what is due at now: an owed night run, or else, at a slot, the hourly
// run. It returns the kind it started (or tried to).
func (b *Backups) Tick(now time.Time) (string, error) {
	b.init()
	s, ok := b.active()
	if !ok {
		return "", nil
	}
	if night, owed := b.nightOwed(s, now); owed {
		b.mu.Lock()
		wait := now.Before(b.started.Add(startGrace)) || now.Before(b.nightNotBefore)
		if !wait {
			// Running, refused or failed to start: it is not tried again for a while
			// (the run itself pushes this further when the repository was unreachable).
			b.nightNotBefore = now.Add(nightRetry)
		}
		b.mu.Unlock()
		if wait {
			return "", nil
		}
		// Planned for its night (the weekly work, the date it is recorded under), even
		// when it starts later.
		p, err := b.Plan(RunDaily, "", night)
		if err == nil {
			_, err = b.Start(p, schedActor)
		}
		if errors.Is(err, ErrBusy) {
			return RunDaily, nil // again in a few minutes
		}
		return RunDaily, err
	}
	local := now.In(b.location())
	slot := b.nextSlot(s, now.Add(-time.Minute))
	b.mu.Lock()
	handled := b.lastSlot.Equal(slot)
	if !local.Before(slot) && local.Sub(slot) < time.Minute {
		b.lastSlot = slot
	}
	b.mu.Unlock()
	if local.Before(slot) || local.Sub(slot) >= time.Minute || handled {
		return "", nil // woken early (settings changed) or this slot already handled
	}
	p, err := b.Plan(RunHourly, "", now)
	if err != nil || p.empty() {
		return "", err
	}
	_, err = b.Start(p, schedActor)
	if errors.Is(err, ErrBusy) {
		b.logf("saatlik yedek atlandı: başka bir işlem sürüyordu")
		return RunHourly, nil
	}
	return RunHourly, err
}

// Start runs a plan as a job.
func (b *Backups) Start(p runPlan, actor Actor) (string, error) {
	b.init()
	if b.Rehearsal.Active() {
		return "", errRehearsal
	}
	if p.empty() {
		return "", errors.New("yedeklenecek bir şey yok (site ayarlarında yedekleme kapalı olabilir)")
	}
	return b.Jobs.Start(JobSpec{Kind: "backup.run", Target: p.Target(), Actor: actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := b.Run(ctx, t, p, actor)
			return err
		}, func(j Job) {
			detail := j.Err
			if detail == "" {
				detail = j.Result
			}
			if b.Audit != nil {
				if err := b.Audit.Record(AuditEntry{Actor: j.Actor, Action: j.Kind, Target: j.Target, OK: j.Err == "", Detail: detail}); err != nil {
					b.logf("audit: %v", err)
				}
			}
			if j.Err != "" {
				b.logf("yedek başarısız (%s): %s", j.Target, j.Err)
			}
			if p.Kind == RunDaily {
				b.afterNight(j)
			}
		})
}
