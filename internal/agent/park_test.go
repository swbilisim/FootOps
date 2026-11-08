package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

// parkOps is a site in service, its page answered as Caddy would: the maintenance page
// (503) while parked, the app otherwise.
func parkOps(t *testing.T) *opsEnv {
	t.Helper()
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("enisparta"), map[string]string{"NODE_ENV": "production"}, e.progress); err != nil {
		t.Fatal(err)
	}
	e.ops.page = func(context.Context, string) (int, int64, error) {
		if s, _ := e.ops.Store.Get("enisparta"); s.IsParked() {
			return 503, int64(len(sites.ParkPage)), nil
		}
		return 200, 1000, nil
	}
	e.steps, e.run.calls = nil, nil
	return e
}

func (e *opsEnv) block(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"))
}

// after reports whether a runs after b in the commands.
func (r *cmdRunner) after(a, b string) bool {
	ia, ib := -1, -1
	for i, c := range r.calls {
		if strings.Contains(c, a) && ia < 0 {
			ia = i
		}
		if strings.Contains(c, b) {
			ib = i
		}
	}
	return ia > ib && ib >= 0
}

func TestParkAndUnpark(t *testing.T) {
	e := parkOps(t)
	ctx := context.Background()
	normal := e.block(t)
	if err := e.ops.Park(ctx, "enisparta", "furkan", e.progress); err != nil {
		t.Fatalf("park: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	spec, _ := e.ops.Store.Get("enisparta")
	if !spec.IsParked() || spec.Parked.By != "furkan" || spec.Parked.At.IsZero() {
		t.Fatalf("record: %+v", spec.Parked)
	}
	if b := e.block(t); !strings.Contains(b, "respond `") || strings.Contains(b, "reverse_proxy") || !strings.Contains(b, "https://enisparta.com, https://www.enisparta.com {") {
		t.Fatalf("park block:\n%s", b)
	}
	// The page first, then the container: visitors never see a gateway error.
	if e.run.ran("caddy validate") != 1 || e.run.ran("caddy reload") != 1 || !e.run.after("enisparta/compose.yaml stop", "caddy reload") {
		t.Fatalf("order: %v", e.run.calls)
	}
	if steps := strings.Join(e.steps, "\n"); !strings.Contains(steps, "bakım sayfasını veriyor") || !strings.Contains(steps, "Container durduruldu") {
		t.Errorf("steps:\n%s", steps)
	}
	if !e.ops.ParkedContainers()["enisparta"] {
		t.Error("alerts do not know it is parked")
	}

	// Parking again finishes what was left: a container still running is stopped; a
	// block already right is not reloaded.
	e.run.calls, e.steps = nil, nil
	if err := e.ops.Park(ctx, "enisparta", "furkan", e.progress); err != nil || e.run.ran("caddy reload") != 0 || e.run.ran(" stop") != 1 {
		t.Fatalf("park again: %v %v", err, e.run.calls)
	}
	e.net.running = false
	e.run.calls = nil
	if err := e.ops.Park(ctx, "enisparta", "furkan", e.progress); err != nil || e.run.ran(" stop") != 0 || !strings.Contains(strings.Join(e.steps, "\n"), "zaten duruyor") {
		t.Fatalf("park a stopped one: %v %v", err, e.run.calls)
	}
	// A block put back by hand (or a crash between record and block): the record decides,
	// when Caddy's blocks are brought up to date.
	os.WriteFile(filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"), []byte(normal), 0o644)
	certs, _ := e.ops.Store.Certs()
	if err := e.ops.rerenderCaddy(ctx, certs); err != nil || !strings.Contains(e.block(t), "respond `") {
		t.Fatalf("rerender: %v\n%s", err, e.block(t))
	}

	// What would start the container is refused; Start leaves it stopped.
	if err := e.ops.Redeploy(ctx, "enisparta", e.progress); !errors.Is(err, errParked) {
		t.Errorf("redeploy: %v", err)
	}
	if _, err := e.ops.CheckUpdate("enisparta", SiteUpdate{Domains: spec.Domains, Image: spec.Image, Port: 3000, MemoryMB: 256}); !errors.Is(err, errParked) {
		t.Errorf("update: %v", err)
	}
	if _, _, err := e.ops.CheckMove("enisparta", "kisisel"); !errors.Is(err, errParked) {
		t.Errorf("move: %v", err)
	}
	e.run.calls = nil
	if err := e.ops.Start(ctx, "enisparta"); err != nil || e.run.ran(" up ") != 0 {
		t.Errorf("start: %v %v", err, e.run.calls)
	}

	// Back with a new end date: the container first, then the record and the block.
	e.net.running = true
	e.run.calls, e.steps = nil, nil
	if err := e.ops.Unpark(ctx, "enisparta", "2027-01-15", e.progress); err != nil {
		t.Fatalf("unpark: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	spec, _ = e.ops.Store.Get("enisparta")
	if spec.IsParked() || spec.Expires != "2027-01-15" || e.block(t) != normal {
		t.Fatalf("after unpark: %+v\n%s", spec, e.block(t))
	}
	if !e.run.after("caddy reload", "compose.yaml up --detach") || !strings.Contains(strings.Join(e.steps, "\n"), "yeniden yayında (bitiş 15.01.2027)") {
		t.Errorf("unpark order / steps: %v\n%s", e.run.calls, strings.Join(e.steps, "\n"))
	}
	if e.ops.ParkedContainers()["enisparta"] {
		t.Error("still parked for the alerts")
	}
	if err := e.ops.Unpark(ctx, "enisparta", "", e.progress); err == nil || !strings.Contains(err.Error(), "parkta değil") {
		t.Errorf("unpark a site in service: %v", err)
	}
}

func TestUnparkFailsBackToParked(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		spoil func(e *opsEnv)
	}{
		{"app does not start", func(e *opsEnv) {
			e.run.hook = func(cmd string) { e.net.running = !strings.Contains(cmd, " up ") && e.net.running }
		}},
		{"no answer through Caddy", func(e *opsEnv) { e.ops.https = func(context.Context, string) (int, error) { return 502, nil } }},
		{"Caddy refuses the block", func(e *opsEnv) {
			e.run.hook = func(cmd string) {
				if strings.Contains(cmd, "caddy validate") {
					e.run.fail["caddy validate"] = errors.New("exit status 1")
				}
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := parkOps(t)
			if err := e.ops.Park(ctx, "enisparta", "furkan", e.progress); err != nil {
				t.Fatal(err)
			}
			parked := e.block(t)
			c.spoil(e)
			e.run.calls, e.steps = nil, nil
			err := e.ops.Unpark(ctx, "enisparta", "2027-01-15", e.progress)
			spec, _ := e.ops.Store.Get("enisparta")
			if err == nil || !spec.IsParked() || spec.Expires != "" || e.block(t) != parked {
				t.Fatalf("err %v, parked %v, expires %q\n%s", err, spec.IsParked(), spec.Expires, e.block(t))
			}
			if !e.run.after(" stop", " up ") || !strings.Contains(strings.Join(e.steps, "\n"), "site parkta kaldı") {
				t.Errorf("container not stopped again: %v\n%s", e.run.calls, strings.Join(e.steps, "\n"))
			}
		})
	}
}

func TestParkRefusedByCaddy(t *testing.T) {
	e := parkOps(t)
	normal := e.block(t)
	e.run.fail["caddy validate"] = errors.New("exit status 1")
	err := e.ops.Park(context.Background(), "enisparta", "furkan", e.progress)
	spec, _ := e.ops.Store.Get("enisparta")
	if err == nil || !strings.Contains(err.Error(), "site değişmedi") || spec.IsParked() || e.block(t) != normal || e.run.ran(" stop") != 0 {
		t.Fatalf("err %v, parked %v, stops %d\n%s", err, spec.IsParked(), e.run.ran(" stop"), e.block(t))
	}
}

func TestSetExpiry(t *testing.T) {
	e := parkOps(t)
	if _, err := e.ops.SetExpiry("enisparta", " 2026-12-31 ", nil); err != nil {
		t.Fatal(err)
	}
	if spec, _ := e.ops.Store.Get("enisparta"); spec.Expires != "2026-12-31" || len(e.run.calls) != 0 {
		t.Fatalf("expires %q, commands %v", spec.Expires, e.run.calls)
	}
	var ve *sites.ValidationError
	if _, err := e.ops.SetExpiry("enisparta", "31.12.2026", nil); !errors.As(err, &ve) {
		t.Errorf("bad date: %v", err)
	}
	if _, err := e.ops.SetExpiry("yok", "2026-12-31", nil); !errors.Is(err, sites.ErrNotFound) {
		t.Errorf("unknown site: %v", err)
	}
	// From the end date the page showed: a second click does not extend twice.
	was := "2026-12-31"
	if _, err := e.ops.SetExpiry("enisparta", "2027-01-31", &was); err != nil {
		t.Fatal(err)
	}
	_, err := e.ops.SetExpiry("enisparta", "2027-01-31", &was)
	if !errors.Is(err, errExpiryChanged) || !strings.Contains(err.Error(), "şu an 31.01.2027") {
		t.Errorf("second click: %v", err)
	}
	if spec, _ := e.ops.Store.Get("enisparta"); spec.Expires != "2027-01-31" {
		t.Errorf("after a second click: %q", spec.Expires)
	}
	// A site being made or changed holds the lock for minutes: a quick change does not wait.
	old := quickLock
	quickLock = 30 * time.Millisecond
	t.Cleanup(func() { quickLock = old })
	e.ops.mu.Lock()
	_, err = e.ops.SetExpiry("enisparta", "", nil)
	e.ops.mu.Unlock()
	if !errors.Is(err, errSiteBusy) {
		t.Errorf("busy: %v", err)
	}
	if _, err := e.ops.SetExpiry("enisparta", "", nil); err != nil {
		t.Fatal(err)
	}
	if spec, _ := e.ops.Store.Get("enisparta"); spec.Expires != "" {
		t.Errorf("no end date: %q", spec.Expires)
	}
}

func TestCreateParked(t *testing.T) {
	ctx := context.Background()
	// A recovery brings a parked site back parked: no container, the maintenance page.
	e := newOps(t)
	spec := specFor("enisparta")
	spec.CreatedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	spec.Parked = &sites.ParkState{At: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), By: "furkan"}
	if _, err := e.ops.CreateWith(ctx, spec, nil, e.progress, CreateOptions{Keep: true, Lenient: true}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(e.steps, "\n"))
	}
	got, _ := e.ops.Store.Get("enisparta")
	if !got.IsParked() || e.run.ran(" up ") != 0 || e.run.ran(" pull ") != 1 || !strings.Contains(e.block(t), "respond `") {
		t.Fatalf("parked %v, commands %v\n%s", got.IsParked(), e.run.calls, e.block(t))
	}
	// A new site from the panel is never made parked, whatever its form said.
	e2 := newOps(t)
	spec.CreatedAt = time.Time{}
	if err := e2.ops.Create(ctx, spec, nil, e2.progress); err != nil {
		t.Fatal(err)
	}
	if got, _ := e2.ops.Store.Get("enisparta"); got.IsParked() || e2.run.ran(" up ") != 1 || strings.Contains(e2.block(t), "respond `") {
		t.Fatalf("new site parked: %+v", got.Parked)
	}
}

func TestParkedNotDown(t *testing.T) {
	a := NewAlerter()
	a.Parked = func() map[string]bool { return map[string]bool{"magaza": true} }
	now := time.Now()
	o := Overview{SampledAt: now, Containers: []ContainerInfo{
		{Name: "magaza", State: "exited", Project: "magaza"}, {Name: "blog", State: "exited", Project: "blog"}}}
	a.Evaluate(o, now)
	a.Evaluate(o, now.Add(time.Minute))
	keys := ""
	for _, al := range a.Active() {
		keys += al.Key + " "
	}
	if keys != "down:blog " {
		t.Fatalf("alerts: %s", keys)
	}
}

func TestParkedInStatusAndReport(t *testing.T) {
	parked := sites.Spec{Name: "magaza", Domains: []string{"magaza.com"}, Parked: &sites.ParkState{At: time.Now()}}
	w := &StatusWriter{Sites: func() ([]sites.Spec, error) {
		return []sites.Spec{parked, {Name: "blog", Domains: []string{"blog.com"}}}, nil
	},
		Containers: func(context.Context) []ContainerInfo {
			return []ContainerInfo{{Name: "magaza", State: "exited"}, {Name: "blog", State: "running"}}
		}}
	f := w.Build(context.Background())
	if len(f.Sites) != 2 || f.Sites[0].Name != "blog" || f.Sites[0].State != "running" || f.Sites[1].State != "parked" {
		t.Fatalf("status file: %+v", f.Sites)
	}
	r := reportSites(ServerReport{Sites: []sites.Spec{parked, {Name: "blog"}},
		Overview: Overview{Containers: []ContainerInfo{{Name: "magaza", State: "exited"}, {Name: "blog", State: "running"}}}})
	if r != "Siteler: 1 / 2 çalışıyor\n  parkta: magaza" {
		t.Fatalf("report: %q", r)
	}
}

func TestParkedBackups(t *testing.T) {
	e := newBackups(t)
	spec, _ := e.data.Store.Get("magaza")
	e.now = spec.CreatedAt.Add(time.Hour) // set up after the site was made
	e.configure(t)
	spec.Parked = &sites.ParkState{At: e.now}
	if err := e.data.Store.WriteMeta(spec); err != nil {
		t.Fatal(err)
	}
	s, _ := e.b.Settings()
	s.Sites = map[string]BackupPolicy{"magaza": {DB: DBHourly}}
	if err := e.b.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	// Its data do not change while parked: the night takes its definition only, the
	// hours nothing; by hand, everything.
	if p, _ := e.b.Plan(RunDaily, "", at); len(p.Sites) != 1 || p.Sites[0].DB || p.Sites[0].Files || !p.Sites[0].Def {
		t.Errorf("night: %+v", p.Sites)
	}
	if p, _ := e.b.Plan(RunHourly, "", at.Add(5*time.Hour)); len(p.Sites) != 0 {
		t.Errorf("hourly: %+v", p.Sites)
	}
	if p, _ := e.b.Plan(RunManual, "magaza", at); len(p.Sites) != 1 || !p.Sites[0].DB || !p.Sites[0].Files || !p.Sites[0].Def {
		t.Errorf("by hand: %+v", p.Sites)
	}
	// Only the definition is owed: old database and file backups are no delay.
	got := e.b.Alerts(e.now.Add(27 * time.Hour))
	if alertKeys(got) != "backup:stale=bad" || got[0].Title != "Yedek gecikti: magaza (site tanımı), sunucu ayarları" {
		t.Fatalf("stale: %+v", got)
	}
}

func TestFinishParks(t *testing.T) {
	e := parkOps(t)
	ctx := context.Background()
	// Parked, its container still running (the stop failed, or the agent was cut off).
	spec, _ := e.ops.Store.Get("enisparta")
	spec.Parked = &sites.ParkState{At: time.Now()}
	if err := e.ops.Store.WriteMeta(spec); err != nil {
		t.Fatal(err)
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	if err := e.ops.FinishParks(ctx, logf); err != nil || e.run.ran("enisparta/compose.yaml stop") != 1 || len(logs) != 1 {
		t.Fatalf("running: %v %v %v", err, e.run.calls, logs)
	}
	// Stopped already, or not parked: nothing to do.
	e.net.running = false
	if err := e.ops.FinishParks(ctx, logf); err != nil || e.run.ran(" stop") != 1 {
		t.Fatalf("stopped: %v %v", err, e.run.calls)
	}
	e.net.running = true
	spec.Parked = nil
	e.ops.Store.WriteMeta(spec)
	if err := e.ops.FinishParks(ctx, logf); err != nil || e.run.ran(" stop") != 1 {
		t.Fatalf("not parked: %v %v", err, e.run.calls)
	}
	// A stop that fails is said, and tried again by the caller.
	spec.Parked = &sites.ParkState{At: time.Now()}
	e.ops.Store.WriteMeta(spec)
	e.run.fail[" stop"] = errors.New("exit status 1")
	if err := e.ops.FinishParks(ctx, logf); err == nil || !strings.Contains(err.Error(), "enisparta: container durdurulamadı") {
		t.Fatalf("failing stop: %v", err)
	}
}

func TestUnparkEndDate(t *testing.T) {
	e := parkOps(t)
	ctx := context.Background()
	if err := e.ops.Park(ctx, "enisparta", "furkan", e.progress); err != nil {
		t.Fatal(err)
	}
	// Back with a date gone or today: it would ask at once to park it again.
	var ve *sites.ValidationError
	for _, d := range []int{-1, 0} {
		day := time.Now().AddDate(0, 0, d).Format(sites.DayLayout)
		if err := e.ops.Unpark(ctx, "enisparta", day, e.progress); !errors.As(err, &ve) || !strings.Contains(err.Error(), "bugünden sonra") {
			t.Fatalf("%s: %v", day, err)
		}
	}
	if s, _ := e.ops.Store.Get("enisparta"); !s.IsParked() || e.run.ran(" up ") != 0 {
		t.Fatal("refused, yet started")
	}
	before := time.Now().UTC().Truncate(time.Second)
	if err := e.ops.Unpark(ctx, "enisparta", time.Now().AddDate(0, 0, 1).Format(sites.DayLayout), e.progress); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.ops.Store.Get("enisparta"); s.Resumed.Before(before) {
		t.Errorf("resumed %v, before %v", s.Resumed, before)
	}
}

func TestResumedBackupsOwed(t *testing.T) {
	e := newBackups(t)
	spec, _ := e.data.Store.Get("magaza")
	e.now = spec.CreatedAt.Add(time.Hour)
	e.configure(t)
	// Back from five days parked: its database and files were not backed up meanwhile, by
	// design — owed from the moment it came back, not from before.
	spec.Resumed = e.now.Add(5 * 24 * time.Hour)
	e.data.Store.WriteMeta(spec)
	got := e.b.Alerts(spec.Resumed.Add(time.Hour))
	for _, a := range got {
		if strings.Contains(a.Title, "veritabanı") || strings.Contains(a.Title, "dosyalar") {
			t.Fatalf("owed from before it came back: %+v", got)
		}
	}
	got = e.b.Alerts(spec.Resumed.Add(27 * time.Hour))
	if alertKeys(got) != "backup:stale=bad" || !strings.Contains(got[0].Title, "magaza (veritabanı)") {
		t.Fatalf("a day after: %+v", got)
	}
}

func TestParkBacksUpDefinition(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	e.ops.page = func(context.Context, string) (int, int64, error) { return 503, 700, nil }
	x := &Expiry{Sites: e.ops, Jobs: e.b.Jobs, Backups: e.b}
	if _, err := x.StartPark("magaza", Actor{User: "it"}, nil); err != nil {
		t.Fatal(err)
	}
	// The park, then a backup of the site's definition alone.
	waitFor(t, func() bool {
		for _, c := range e.restic.ran("backup") {
			if strings.Contains(strings.Join(c, " "), partPath("magaza", PartDef)) {
				return true
			}
		}
		return false
	})
	waitFor(t, func() bool { return !e.b.Jobs.Busy() })
	for _, c := range e.restic.ran("backup") {
		if a := strings.Join(c, " "); strings.Contains(a, partPath("magaza", PartDB)) || strings.Contains(a, "/files") {
			t.Fatalf("more than the definition: %s", a)
		}
	}
	if s, _ := e.data.Store.Get("magaza"); !s.IsParked() {
		t.Fatal("not parked")
	}
}

func TestDefBackupBounded(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	// A repository out of reach: restic waits (its retries) until it is stopped.
	e.restic.reply = func(ctx context.Context, args []string, c run.Cmd) error {
		<-ctx.Done()
		return ctx.Err()
	}
	p, err := e.b.DefPlan("magaza")
	if err != nil || p.Timeout != defTimeout || len(p.Sites) != 1 || !p.Sites[0].Def || p.Sites[0].DB || p.Sites[0].Files {
		t.Fatalf("plan: %+v %v", p, err)
	}
	p.Timeout = 50 * time.Millisecond
	id, err := e.b.Start(p, parkActor)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { j, _ := e.b.Jobs.Get(id); return j.Done })
	if j, _ := e.b.Jobs.Get(id); !strings.Contains(j.Err, "içinde bitmedi") || strings.Contains(j.Err, "zaman aşımı (") {
		t.Fatalf("job: %q", j.Err)
	}
}
