//go:build preview

package web

// A local preview of the panel with made-up data, for checking the design in a browser:
//
//	go test -tags preview -run TestPreview -timeout 0 ./internal/web
//
// then open http://localhost:7799/dev-login. Never part of the binary.

import (
	"context"
	"fmt"
	"html"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

type previewAgent struct{ fakeAgent }

var previewContainers = []agent.ContainerInfo{
	{Name: "magaza", Image: "ghcr.io/ornek/magaza:main", State: "running", Project: "magaza", CPUPercent: 1.2, MemUsage: 180 << 20, MemLimit: 256 << 20, StartedAt: time.Now().Add(-50 * time.Hour)},
	{Name: "footops-caddy", Image: "caddy:2.11.7-alpine", State: "running", Health: "healthy", Project: "footops-core", CPUPercent: 0.3, MemUsage: 38 << 20, MemLimit: 4 << 30, StartedAt: time.Now().Add(-80 * time.Hour)},
	{Name: "footops-mongo", Image: "mongo:7.0.45", State: "running", Health: "healthy", Project: "footops-core", CPUPercent: 2.4, MemUsage: 610 << 20, MemLimit: 4 << 30, StartedAt: time.Now().Add(-80 * time.Hour)},
	{Name: "blog", Image: "ghcr.io/ornek/blog:main", State: "restarting", Project: "blog", RestartCount: 14, OOMKilled: true, MemLimit: 512 << 20, StartedAt: time.Now().Add(-3 * time.Minute)},
	{Name: "kurumsal", Image: "ghcr.io/ornek/kurumsal:main", State: "running", Health: "starting", Project: "kurumsal", CPUPercent: 7.8, MemUsage: 300 << 20, MemLimit: 512 << 20, StartedAt: time.Now().Add(-20 * time.Minute)},
}

func (p *previewAgent) Overview(context.Context) (agent.Overview, error) {
	return agent.Overview{
		SampledAt: time.Now().Add(-4 * time.Second),
		System: agent.SystemMetrics{Hostname: "h1", CPUPercent: 23.4, Load1: 0.42, Load5: 0.38, Load15: 0.31,
			MemTotal: 3900 << 20, MemUsed: 2950 << 20, SwapTotal: 2 << 30, SwapUsed: 180 << 20,
			DiskTotal: 38 << 30, DiskUsed: 31 << 30, Uptime: 74 * time.Hour},
		Containers: previewContainers,
	}, nil
}

func wave(i int, period, lo, hi float64) float64 {
	return lo + (hi-lo)*(0.5+0.5*math.Sin(float64(i)/period))
}

func (p *previewAgent) History(_ context.Context, rng, name string) (agent.History, error) {
	r, _ := history.RangeByName(rng)
	now := time.Now()
	step := int64(r.Step / time.Second)
	n := int(r.Span / r.Step)
	h := agent.History{Range: r.Name, Step: step, From: now.Add(-r.Span), To: now, Containers: map[string][]history.Point{}}
	start := now.Add(-r.Span).Unix()
	start -= start % step
	for i := 0; i < n; i++ {
		t := start + int64(i)*step
		cpu := wave(i, 9, 5, 35)
		h.Host = append(h.Host, history.Point{T: t, CPU: cpu, CPUMax: cpu * 1.6, Mem: uint64(wave(i, 40, 2400, 3000)) << 20, MemMax: uint64(wave(i, 40, 2500, 3100)) << 20, Swap: uint64(wave(i, 60, 100, 200)) << 20})
		for j, c := range previewContainers {
			if c.Name == "blog" && i > n*2/3 {
				continue // stopped: a gap
			}
			if c.Name == "kurumsal" && i < n/3 {
				continue // created later
			}
			grow := float64(i) / float64(n)
			mem := float64(c.MemUsage>>20) * (0.6 + 0.4*grow)
			if c.Name == "blog" {
				mem = 200 + 300*grow*1.4
			}
			cpu := wave(i+j*7, 5+float64(j), 0.2, 3+float64(j)*2)
			h.Containers[c.Name] = append(h.Containers[c.Name], history.Point{T: t, CPU: cpu, CPUMax: cpu * 2, Mem: uint64(mem) << 20, MemMax: uint64(mem*1.1) << 20})
		}
	}
	if name != "" {
		h.Host = nil
		h.Containers = map[string][]history.Point{name: h.Containers[name]}
	}
	return h, nil
}

func (p *previewAgent) Sites(context.Context) ([]agent.SiteStatus, error) {
	var out []agent.SiteStatus
	for _, n := range []string{"magaza", "blog", "kurumsal"} {
		var c *agent.ContainerInfo
		for i := range previewContainers {
			if previewContainers[i].Name == n {
				c = &previewContainers[i]
			}
		}
		out = append(out, agent.SiteStatus{Spec: sites.Spec{Name: n, Group: "musteri", Domains: []string{n + ".example.com", "www." + n + ".example.com"},
			Image: c.Image, Port: 3000, MemoryMB: int(c.MemLimit >> 20), Database: n != "blog", DBVar: "MONGODB_URI",
			DataMount: "/app/uploads", EnvKeys: []string{"NODE_ENV", "SESSION_SECRET", "RESEND_API_KEY"},
			CreatedAt: time.Now().Add(-72 * time.Hour), UpdatedAt: time.Now().Add(-2 * time.Hour)}, Container: c})
	}
	return out, nil
}

func (p *previewAgent) Snapshots(_ context.Context, site string) ([]agent.Snapshot, error) {
	if site != "magaza" {
		return nil, nil
	}
	var out []agent.Snapshot
	for i, h := range []int{2, 26, 120} {
		t := time.Now().Add(-time.Duration(h) * time.Hour)
		out = append(out, agent.Snapshot{ID: t.UTC().Format("20060102-150405"), Site: site, Created: t, Actor: "admin",
			Bytes: int64(8806 + i*1200), Documents: int64(89 + i*3), Collections: map[string]int64{"a": 1, "b": 2, "c": 3},
			Usage: run.Usage{PeakMem: 42 << 20, Wall: 1800 * time.Millisecond, CPU: 300 * time.Millisecond}})
	}
	return out, nil
}

// Job shows a database copy: /jobs/running (with progress and cancel) or /jobs/done.
func (p *previewAgent) Job(_ context.Context, id string) (agent.Job, error) {
	j := agent.Job{ID: id, Kind: "db.snapshot", Target: "magaza", Cancellable: true, Started: time.Now().Add(-4 * time.Second),
		Steps: []string{"✓ Veritabanı magaza: 10 koleksiyon, 89 belge, 41.2 KB veri", "… mongodump çalışıyor (sınır: 512 MB bellek, %50 CPU)"}, Bytes: 3 << 20}
	if id == "done" {
		j.Done, j.Finished, j.Result = true, time.Now(), "20261027-110000"
		j.Usage = run.Usage{PeakMem: 38 << 20, CPU: 420 * time.Millisecond}
		j.Steps = append(j.Steps, "✓ mongodump bitti: 10 koleksiyon, 89 belge → 8.6 KB (1.8s, en çok 38.0 MB bellek, 420ms CPU)",
			"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 20261027-110000 (sunucuda 7 gün saklanır)")
	}
	return j, nil
}

func (p *previewAgent) Certs(context.Context) ([]sites.Cert, error) {
	return []sites.Cert{
		{ID: "0123456789abcdef", Names: []string{"*.example.com", "example.com"}, Issuer: "CloudFlare Origin SSL Certificate Authority", NotAfter: time.Now().AddDate(15, 0, 0)},
		{ID: "fedcba9876543210", Names: []string{"*.example.org", "example.org"}, Issuer: "CloudFlare Origin SSL Certificate Authority", NotAfter: time.Now().AddDate(0, 0, 12)},
	}, nil
}

func (p *previewAgent) Logs(context.Context, string, int) ([]string, error) {
	var out []string
	for i := 0; i < 120; i++ {
		ts := time.Now().Add(time.Duration(i-120) * 7 * time.Second).UTC().Format(time.RFC3339)
		switch i % 9 {
		case 4:
			out = append(out, ts+" ERROR MongoServerError: connection refused (attempt "+string(rune('0'+i%10))+")")
		case 7:
			out = append(out, ts+" WARN slow request GET /api/products 1840ms")
		default:
			out = append(out, ts+" INFO GET /urunler 200 12ms")
		}
	}
	return out, nil
}

func (p *previewAgent) AuditTail(context.Context, int) ([]agent.AuditEntry, error) {
	a := agent.Actor{User: "admin", IP: "203.0.113.7", Via: "tunnel"}
	return []agent.AuditEntry{
		{Time: time.Now().Add(-time.Hour), Actor: a, Action: "site.create", Target: "magaza", OK: true},
		{Time: time.Now().Add(-50 * time.Minute), Actor: a, Action: "container.restart", Target: "blog", OK: false, Detail: "docker: 500 container is restarting"},
		{Time: time.Now().Add(-2 * time.Minute), Actor: a, Action: "auth.login", OK: true, Detail: "totp (access: admin@example.com)"},
	}, nil
}

type frameable struct{ http.ResponseWriter }

func (f frameable) WriteHeader(code int) {
	f.Header().Del("X-Frame-Options")
	f.Header().Set("Content-Security-Policy", strings.Replace(f.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'", "frame-ancestors 'self'", 1))
	f.ResponseWriter.WriteHeader(code)
}

func (f frameable) Write(b []byte) (int, error) {
	if f.Header().Get("X-Frame-Options") != "" {
		f.WriteHeader(http.StatusOK)
	}
	return f.ResponseWriter.Write(b)
}

func (p *previewAgent) Alerts(context.Context) ([]agent.Alert, error) {
	return []agent.Alert{
		{Key: "down:blog", Level: "bad", Title: "blog sürekli yeniden başlıyor", Detail: "Uygulama açılırken çöküyor; loglara bak.", Link: "/containers/blog/logs", Since: time.Now().Add(-4 * time.Minute)},
		{Key: "disk", Level: "warn", Title: "Disk %82 dolu", Detail: "Eski imajlar, loglar ve yedekler yer kaplıyor olabilir.", Link: "/", Since: time.Now().Add(-3 * time.Hour)},
		{Key: "cert:fedcba9876543210", Level: "warn", Title: "blog.example.org sertifikasının bitmesine 12 gün var", Link: "/certs", Since: time.Now().Add(-26 * time.Hour)},
	}, nil
}

func TestPreview(t *testing.T) {
	store := auth.NewStore(filepath.Join(t.TempDir(), "auth.json"))
	cfg := config.Panel{ServerCode: "H1", PanelHost: "panel.example.com"}
	s, err := NewServer(cfg, &previewAgent{}, store, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler(viaSSH)
	mux := http.NewServeMux()
	mux.HandleFunc("/dev-login", func(w http.ResponseWriter, r *http.Request) {
		sess := s.sessions.create("admin", viaSSH, "")
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sess.ID, Path: "/", HttpOnly: true, MaxAge: 7200})
		next := r.URL.Query().Get("next")
		if !strings.HasPrefix(next, "/") {
			next = "/"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
	})
	// /frame?src=/sites shows a page at phone width; the CSP forbids framing, so the
	// preview drops it for framed pages.
	mux.HandleFunc("/frame", func(w http.ResponseWriter, r *http.Request) {
		src := r.URL.Query().Get("src")
		if !strings.HasPrefix(src, "/") {
			src = "/"
		}
		width, err := strconv.Atoi(r.URL.Query().Get("w"))
		if err != nil || width < 200 || width > 2000 {
			width = 390
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		height, err := strconv.Atoi(r.URL.Query().Get("h"))
		if err != nil || height < 200 || height > 8000 {
			height = 4000
		}
		_, _ = fmt.Fprintf(w, `<!doctype html><body style="margin:0;background:#888"><iframe src="%s" width="%d" height="%d" style="border:0;background:#fff"></iframe>`, html.EscapeString(src), width, height)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(frameable{w}, r)
	}))
	t.Log("http://localhost:7799/dev-login")
	t.Fatal(http.ListenAndServe("127.0.0.1:7799", mux))
}
