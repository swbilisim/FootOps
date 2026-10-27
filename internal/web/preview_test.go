//go:build preview

package web

// A local preview of the panel with made-up data, for checking the design in a browser:
//
//	go test -tags preview -run TestPreview -timeout 0 ./internal/web
//
// then open http://localhost:7799/dev-login. Never part of the binary.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"log"
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
	"github.com/swbilisim/FootOps/internal/datafs"
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
			DataMount: "/app/uploads", DataURL: map[bool]string{true: "/uploads"}[n == "magaza"], EnvKeys: []string{"NODE_ENV", "SESSION_SECRET", "RESEND_API_KEY"},
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
	if id == "import" {
		return agent.Job{ID: id, Kind: "import.apply", Target: "magaza", Started: time.Now().Add(-9 * time.Second), Finished: time.Now(),
			Done: true, Result: "20261027-110000", Usage: run.Usage{PeakMem: 96 << 20, CPU: 2 * time.Second},
			Steps: []string{"✓ Veritabanı magaza: 4 koleksiyon, 2759 belge, 1.1 MB veri", "… mongodump çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ mongodump bitti: 4 koleksiyon, 2759 belge → 240.1 KB (0.9 sn, en çok 61.0 MB bellek, 210 ms CPU)",
				"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 20261027-110000 (sunucuda 7 gün saklanır)",
				"✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)", "… yüklenen dosya magaza-20261027-091500.footops.tar yükleniyor (tamamen değiştir)",
				"✓ Arşivde olmayan 1 koleksiyon silindi: oturumlar", "… mongorestore çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ Yüklendi ve sayıldı: 4 koleksiyon, 2641 belge (önce 2759 belge) — 2.1 sn, en çok 96.0 MB bellek, 1.4 sn CPU",
				"✓ Site başlatıldı", "✓ Site sağlıklı cevap veriyor", "✓ Yüklenen dosya sunucudan silindi",
				"ℹ Geri almak için: site sayfası → Veritabanı kopyaları → 20261027-110000 → Geri yükle"}}, nil
	}
	if id == "done" {
		j.Done, j.Finished, j.Result = true, time.Now(), "20261027-110000"
		j.Usage = run.Usage{PeakMem: 38 << 20, CPU: 420 * time.Millisecond}
		j.Steps = append(j.Steps, "✓ mongodump bitti: 10 koleksiyon, 89 belge → 8.6 KB (1.8s, en çok 38.0 MB bellek, 420ms CPU)",
			"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 20261027-110000 (sunucuda 7 gün saklanır)")
	}
	return j, nil
}

// Imports: /sites/magaza/import/<testUpload> shows a checked upload; /jobs/import shows
// a finished import.
func (p *previewAgent) Upload(_ context.Context, id string) (agent.UploadStatus, error) {
	return agent.UploadStatus{Upload: agent.Upload{ID: id, Site: "magaza", Name: "magaza-20261027-091500.footops.tar",
		Size: 9 << 20, Finished: true, Created: time.Now().Add(-3 * time.Minute)}}, nil
}

// CreateUpload and PutChunk check what upload.js sends: every chunk's SHA-256.
func (p *previewAgent) CreateUpload(_ context.Context, site, name string, size int64, a agent.Actor) (agent.UploadStatus, error) {
	st := agent.UploadStatus{Upload: agent.Upload{ID: testUpload, Site: site, Name: name, Size: size, Chunk: agent.UploadChunk}}
	for i := 0; i < st.Chunks(); i++ {
		st.Got, st.Missing = append(st.Got, false), append(st.Missing, i)
	}
	return st, nil
}

func (p *previewAgent) PutChunk(_ context.Context, id string, n int, sum string, body io.Reader, length int64) (agent.UploadStatus, error) {
	h := sha256.New()
	got, err := io.Copy(h, body)
	if err != nil {
		return agent.UploadStatus{}, err
	}
	ok := hex.EncodeToString(h.Sum(nil)) == sum
	log.Printf("preview: chunk %d, %d bytes, sha256 ok=%v", n, got, ok)
	if !ok {
		return agent.UploadStatus{}, &agent.Error{Status: 422, Message: "parça bozuk geldi"}
	}
	return agent.UploadStatus{}, nil
}

func (p *previewAgent) FinishUpload(_ context.Context, id string, a agent.Actor) (agent.UploadStatus, error) {
	log.Printf("preview: upload %s finished", id)
	return agent.UploadStatus{Upload: agent.Upload{ID: id, Site: "magaza", Finished: true}}, nil
}

func (p *previewAgent) Import(context.Context, string, string) (agent.ImportCheck, error) {
	return agent.ImportCheck{Upload: testUpload, Site: "magaza", Format: agent.FormatFootOps,
		SHA256:        "9f2c4e0b7d1a5c3e8f6b2a4d0c9e7f1b3a5d8c2e6f0b4a7d9c1e3f5a8b2d6c0e",
		Manifest:      &agent.ExportManifest{Site: "magaza", Created: time.Now().Add(-26 * time.Hour)},
		ServerVersion: "7.0.45", ToolVersion: "100.10.0", Databases: []string{"magaza"}, SourceDB: "magaza", Target: "magaza",
		Archive: map[string]int64{"urunler": 412, "kategoriler": 18, "siparisler": 2210, "ayarlar": 1}, ArchiveDocs: 2641, Views: []string{"ucuz_urunler"},
		Current: map[string]int64{"urunler": 415, "kategoriler": 18, "siparisler": 2230, "oturumlar": 96}, CurrentDocs: 2759,
		Checked: time.Now().Add(-time.Minute)}, nil
}

func (p *previewAgent) Exports(_ context.Context, site string) ([]agent.ExportInfo, error) {
	if site != "magaza" {
		return nil, nil
	}
	return []agent.ExportInfo{{ID: "exp1", Site: site, File: "magaza-20261027-091500.footops.tar", Bytes: 9 << 20,
		Created: time.Now().Add(-40 * time.Minute), Collections: 4, Documents: 2641}}, nil
}

func (p *previewAgent) Files(_ context.Context, site, dir string, opt datafs.ListOptions) (datafs.Listing, error) {
	now := time.Now()
	var all []datafs.Entry
	if dir == "" {
		all = []datafs.Entry{{Name: "urunler", Type: "dir", MTime: now.Add(-3 * time.Hour)}, {Name: "slider", Type: "dir", MTime: now.Add(-50 * time.Hour)},
			{Name: "kampanya-2026.zip", Type: "file", Size: 48 << 20, MTime: now.Add(-20 * time.Minute)},
			{Name: "katalog.pdf", Type: "file", Size: 3 << 20, MTime: now.Add(-400 * time.Hour)},
			{Name: "Çiçekli kapak görseli — çok uzun bir dosya adı örneği.jpg", Type: "file", Size: 820 << 10, MTime: now.Add(-26 * time.Hour)},
			{Name: "eski-site", Type: "link", MTime: now.Add(-900 * time.Hour)}, {Name: "bozuk?ad", Type: "file", Odd: true, Size: 12, MTime: now.Add(-900 * time.Hour)}}
	} else {
		for i := 0; i < 23; i++ {
			all = append(all, datafs.Entry{Name: fmt.Sprintf("urun-%03d.jpg", i+1), Type: "file", Size: int64(180+i*37) << 10, MTime: now.Add(-time.Duration(i) * time.Hour)})
		}
	}
	l := datafs.Listing{Dir: dir, Total: len(all), Entries: all, Hidden: 1}
	for _, e := range all {
		if e.Type == "dir" {
			l.Dirs++
		} else if e.Type == "file" {
			l.Files++
			l.Bytes += e.Size
		}
	}
	return l, nil
}

func (p *previewAgent) FileDirs(context.Context, string) ([]string, error) {
	return []string{"slider", "urunler", "urunler/2025", "urunler/2026"}, nil
}

func (p *previewAgent) Trash(context.Context, string) ([]datafs.TrashEntry, error) {
	now := time.Now()
	return []datafs.TrashEntry{
		{ID: "20261028-093000-abcdef", Dir: "urunler", Names: []string{"urun-001.jpg", "urun-002.jpg", "urun-003.jpg", "urun-004.jpg", "urun-005.jpg", "urun-006.jpg", "urun-007.jpg"},
			Files: 7, Bytes: 1900 << 10, Deleted: now.Add(-2 * time.Hour), Actor: "admin"},
		{ID: "20261027-120000-123456", Dir: "", Names: []string{"katalog.pdf"}, Files: 1, Bytes: 3 << 20, Deleted: now.Add(-30 * time.Hour), Actor: "admin", Reason: "overwrite"},
		{ID: "20261026-120000-654321", Dir: "slider", Names: []string{"eski"}, Files: 12, Dirs: 2, Bytes: 9 << 20, Deleted: now.Add(-50 * time.Hour), Actor: "admin"},
	}, nil
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
