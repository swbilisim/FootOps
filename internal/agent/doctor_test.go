package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/doctor"
)

func doctorReport(t *testing.T, at time.Time, problems ...doctor.Finding) []byte {
	t.Helper()
	r := doctor.Report{Format: doctor.Format, Host: "h1", FootOps: "v0.1.0", Time: at,
		Findings: append([]doctor.Finding{{ID: "reboot", Section: "system", Level: doctor.OK, Title: "Yeniden başlatma gerekmiyor"}}, problems...)}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestDoctorConfirmsProblems(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	clock := start
	var out []byte
	var execErr error
	path := filepath.Join(t.TempDir(), "doctor.json")
	d := &Doctor{Path: path, Exec: func(context.Context) ([]byte, error) { return out, execErr }}
	d.now = func() time.Time { return clock }
	changed := 0
	d.Changed = func() { changed++ }

	ports := doctor.Finding{ID: "ports", Section: "network", Level: doctor.Bad, Title: "Docker dışarıya port açmış: tcp 8080 → elle"}
	images := doctor.Finding{ID: "images", Section: "disk", Level: doctor.Info, Title: "2 imajı hiçbir container kullanmıyor"}
	out = doctorReport(t, clock, ports, images)
	d.Check(ctx)
	if a := d.Alerts(clock); len(a) != 0 {
		t.Fatalf("first sighting is an alert: %+v", a)
	}
	if w := d.nextWake(clock); w != doctorConfirm {
		t.Errorf("confirm run in %v, want %v", w, doctorConfirm)
	}
	if st := d.Status(); st.Report == nil || len(st.Confirmed) != 0 || !st.Tried.Equal(start) {
		t.Errorf("status = %+v", st)
	}

	clock = clock.Add(doctorConfirm)
	out = doctorReport(t, clock, ports, images)
	d.Check(ctx)
	a := d.Alerts(clock)
	if len(a) != 1 || a[0].Key != "doctor:ports" || a[0].Level != "bad" || a[0].Title != "Kontrol: "+ports.Title ||
		a[0].Link != "/server/check" || !a[0].Since.Equal(start) {
		t.Fatalf("confirmed = %+v", a)
	}
	// Nothing left to confirm: the daily run, tomorrow 08:30.
	if w := d.nextWake(clock); w != 23*time.Hour+20*time.Minute {
		t.Errorf("next = %v", w)
	}
	// What the agent knew survives a restart.
	if a := (&Doctor{Path: path}).Alerts(clock); len(a) != 1 {
		t.Errorf("after a restart: %+v", a)
	}
	if p := d.StatusPart(); p == nil || p.Bad != 1 || p.Warn != 0 || len(p.Problems) != 1 || !p.Time.Equal(clock) {
		t.Errorf("status part = %+v", p)
	}

	// Put right: the alert goes at the next run.
	out = doctorReport(t, clock)
	d.Check(ctx)
	if a := d.Alerts(clock); len(a) != 0 {
		t.Errorf("solved, still: %+v", a)
	}

	// The check itself failing: an alert from the second failure, gone with a run that works.
	execErr = errors.New("exit status 1: Unit footops-doctor.service already exists")
	d.Check(ctx)
	if a := d.Alerts(clock); len(a) != 0 || d.nextWake(clock) != doctorRetry {
		t.Errorf("one failure: %+v", a)
	}
	clock = clock.Add(doctorRetry)
	execErr = nil
	out = []byte("not json")
	d.Check(ctx)
	a = d.Alerts(clock)
	if len(a) != 1 || a[0].Key != "doctor:run" || a[0].Detail != "rapor okunamadı: invalid character 'o' in literal null (expecting 'u')" ||
		!a[0].Since.Equal(clock.Add(-doctorRetry)) {
		t.Errorf("two failures: %+v", a)
	}
	// The last report stays on the page meanwhile.
	if st := d.Status(); st.Report == nil || st.Err == "" {
		t.Errorf("status while failing = %+v", st)
	}
	out = doctorReport(t, clock)
	d.Check(ctx)
	if a := d.Alerts(clock); len(a) != 0 || d.Status().Err != "" {
		t.Errorf("recovered: %+v", a)
	}
	if changed != 6 {
		t.Errorf("changed %d times, want 6", changed)
	}
}

func TestDoctorAPI(t *testing.T) {
	block := make(chan struct{})
	d := &Doctor{Exec: func(context.Context) ([]byte, error) {
		<-block
		return doctorReport(t, time.Now()), nil
	}}
	c, _ := startServer(t, 0, func(s *Server) { s.Doctor = d })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	if err := c.DoctorRun(ctx, Actor{User: "furkan"}); err != nil {
		t.Fatal(err)
	}
	waitFor := func(what string, ok func(DoctorStatus) bool) {
		t.Helper()
		for i := 0; i < 200; i++ {
			st, err := c.DoctorStatus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if ok(st) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	waitFor("the run", func(st DoctorStatus) bool { return st.Running })
	var ae *Error
	if err := c.DoctorRun(ctx, Actor{User: "furkan"}); !errors.As(err, &ae) || ae.Status != 409 {
		t.Errorf("second run while one runs: %v", err)
	}
	close(block)
	waitFor("the report", func(st DoctorStatus) bool { return !st.Running && st.Report != nil && st.Report.Host == "h1" })
	if st, _ := c.DoctorStatus(ctx); st.Next.IsZero() {
		t.Errorf("no next run: %+v", st)
	}
}
