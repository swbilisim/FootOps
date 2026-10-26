package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

var a0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func healthy() Overview {
	return Overview{
		System: SystemMetrics{CPUPercent: 10, MemTotal: 4 << 30, MemUsed: 1 << 30, SwapTotal: 2 << 30, DiskTotal: 40 << 30, DiskUsed: 10 << 30},
		Containers: []ContainerInfo{
			{Name: "footops-mongo", Project: "footops-core", State: "running", Health: "healthy", MemUsage: 500 << 20, MemLimit: 4 << 30},
			{Name: "web", Project: "web", State: "running", MemUsage: 100 << 20, MemLimit: 256 << 20},
			{Name: "manual", State: "exited"}, // not managed by FootOps: ignored
		},
	}
}

func keys(list []Alert) string {
	var k []string
	for _, a := range list {
		k = append(k, a.Level+" "+a.Key)
	}
	return strings.Join(k, ", ")
}

func TestAlertsQuietWhenHealthy(t *testing.T) {
	a := NewAlerter()
	for i := 0; i < 60; i++ {
		a.Evaluate(healthy(), a0.Add(time.Duration(i)*10*time.Second))
	}
	if got := a.Active(); len(got) != 0 {
		t.Errorf("alerts on a healthy server: %s", keys(got))
	}
}

func TestAlertsHoldAndResolve(t *testing.T) {
	var opened, resolved []Alert
	a := NewAlerter()
	a.OnChange = func(o, r []Alert) { opened, resolved = append(opened, o...), append(resolved, r...) }

	o := healthy()
	o.System.MemUsed = o.System.MemTotal * 96 / 100
	o.System.DiskUsed = o.System.DiskTotal * 85 / 100
	a.Evaluate(o, a0)
	// Disk is stable and alerts at once; memory has to stay high for alertHold.
	if got := keys(a.Active()); got != "warn disk" {
		t.Errorf("first round: %s", got)
	}
	a.Evaluate(o, a0.Add(alertHold))
	if got := keys(a.Active()); got != "bad memory, warn disk" {
		t.Errorf("after hold: %s", got)
	}
	if mem := a.Active()[0]; !mem.Since.Equal(a0) || mem.Link == "" {
		t.Errorf("memory alert = %+v", mem)
	}
	// A short spike that disappears never opens.
	a.Evaluate(healthy(), a0.Add(alertHold+10*time.Second))
	if len(a.Active()) != 0 || len(resolved) != 2 || len(opened) != 2 {
		t.Errorf("active %s, opened %s, resolved %s", keys(a.Active()), keys(opened), keys(resolved))
	}
}

func TestAlertsContainers(t *testing.T) {
	a := NewAlerter()
	o := healthy()
	o.Containers[1].RestartCount = 0
	a.Evaluate(o, a0)
	o = healthy()
	o.Containers[1].RestartCount = 3
	o.Containers[1].MemUsage = 240 << 20
	o.Containers[0].Health = "unhealthy"
	o.Containers = append(o.Containers, ContainerInfo{Name: "site2", Project: "site2", State: "restarting", OOMKilled: true})
	for i := 1; i <= 4; i++ {
		a.Evaluate(o, a0.Add(time.Duration(i)*10*time.Second))
	}
	want := "bad down:site2, bad health:footops-mongo, bad oom:site2, warn limit:web, warn restarts:web"
	if got := keys(a.Active()); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Restarts age out of the window.
	a.Evaluate(healthyWithRestarts(3), a0.Add(restartWindow+time.Minute))
	a.Evaluate(healthyWithRestarts(3), a0.Add(restartWindow+2*time.Minute))
	if got := keys(a.Active()); got != "" {
		t.Errorf("after the window: %s", got)
	}
}

func healthyWithRestarts(n int) Overview {
	o := healthy()
	o.Containers[1].RestartCount = n
	return o
}

func TestAlertsCPUNeedsSustainedLoad(t *testing.T) {
	a := NewAlerter()
	o := healthy()
	o.System.CPUPercent = 99
	for i := 0; i < 20; i++ { // 3 minutes 20 seconds
		a.Evaluate(o, a0.Add(time.Duration(i)*10*time.Second))
	}
	if len(a.Active()) != 0 {
		t.Fatalf("CPU alert before the window filled: %s", keys(a.Active()))
	}
	for i := 20; i < 31; i++ {
		a.Evaluate(o, a0.Add(time.Duration(i)*10*time.Second))
	}
	if got := keys(a.Active()); got != "warn cpu" {
		t.Errorf("sustained CPU: %s", got)
	}
}

func TestAlertsCertificates(t *testing.T) {
	a := NewAlerter()
	a.Certs = func() ([]sites.Cert, error) {
		return []sites.Cert{
			{ID: "aa", Names: []string{"*.a.com", "a.com"}, NotAfter: a0.AddDate(0, 0, 20)},
			{ID: "bb", Names: []string{"b.com"}, NotAfter: a0.AddDate(0, 0, 3)},
			{ID: "cc", Names: []string{"c.com"}, NotAfter: a0.Add(-time.Hour)},
			{ID: "dd", Names: []string{"d.com"}, NotAfter: a0.AddDate(10, 0, 0)},
		}, nil
	}
	a.Evaluate(healthy(), a0)
	if got := keys(a.Active()); got != "bad cert:bb, bad cert:cc, warn cert:aa" {
		t.Errorf("got %s", got)
	}
	if t0 := a.Active()[2].Title; !strings.Contains(t0, "a.com") || !strings.Contains(t0, "20 gün") {
		t.Errorf("title = %q", t0)
	}
	// Certificates are read again only every certEvery.
	a.Certs = func() ([]sites.Cert, error) { return nil, errors.New("izin yok") }
	a.Evaluate(healthy(), a0.Add(time.Minute))
	if got := keys(a.Active()); got != "bad cert:bb, bad cert:cc, warn cert:aa" {
		t.Errorf("before the refresh: %s", got)
	}
	a.Evaluate(healthy(), a0.Add(certEvery))
	if got := keys(a.Active()); got != "warn certs" {
		t.Errorf("unreadable certificates: %s", got)
	}
}

func TestAlertsEndpoint(t *testing.T) {
	c, _ := startServer(t, 999)
	if list, err := c.Alerts(ctx); err != nil || len(list) != 0 {
		t.Fatalf("without an alerter: %v, %v", list, err)
	}
	al := NewAlerter()
	o := healthy()
	o.System.DiskUsed = o.System.DiskTotal * 95 / 100
	al.Evaluate(o, time.Now())
	c, _ = startServer(t, 999, func(s *Server) { s.Sampler.Alerts = al })
	list, err := c.Alerts(ctx)
	if err != nil || len(list) != 1 || list[0].Key != "disk" || list[0].Level != "bad" {
		t.Errorf("alerts = %+v, %v", list, err)
	}
}
