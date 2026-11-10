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
	"encoding/json"
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
	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/mongo"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/update"
)

type previewAgent struct{ fakeAgent }

var previewContainers = []agent.ContainerInfo{
	{Name: "magaza", Image: "ghcr.io/ornek/magaza:main", State: "running", Project: "magaza", CPUPercent: 1.2, MemUsage: 180 << 20, MemLimit: 256 << 20, StartedAt: time.Now().Add(-50 * time.Hour)},
	{Name: "footops-caddy", Image: "caddy:2.11.7-alpine", State: "running", Health: "healthy", Project: "footops-core", CPUPercent: 0.3, MemUsage: 38 << 20, MemLimit: 4 << 30, StartedAt: time.Now().Add(-80 * time.Hour)},
	{Name: "footops-mongo", Image: "mongo:7.0.45", State: "running", Health: "healthy", Project: "footops-core", CPUPercent: 2.4, MemUsage: 610 << 20, MemLimit: 4 << 30, StartedAt: time.Now().Add(-80 * time.Hour)},
	{Name: "blog", Image: "ghcr.io/ornek/blog:main", State: "restarting", Project: "blog", RestartCount: 14, OOMKilled: true, MemLimit: 512 << 20, StartedAt: time.Now().Add(-3 * time.Minute)},
	{Name: "kurumsal", Image: "ghcr.io/ornek/kurumsal:main", State: "running", Health: "starting", Project: "kurumsal", CPUPercent: 7.8, MemUsage: 300 << 20, MemLimit: 512 << 20, StartedAt: time.Now().Add(-20 * time.Minute)},
	// Edge cases for the layout: four domains, the longest name and long domains.
	{Name: "klinik", Image: "ghcr.io/ornek/klinik:main", State: "running", Project: "klinik", CPUPercent: 0.6, MemUsage: 96 << 20, MemLimit: 384 << 20, StartedAt: time.Now().Add(-30 * time.Hour)},
	{Name: "buyuksehir-belediyesi-kultur-mrk", Image: "ghcr.io/buyuksehir-belediyesi/buyuksehir-belediyesi-kultur-merkezi:2026-10-30-main", State: "running", Health: "healthy", Project: "buyuksehir-belediyesi-kultur-mrk", CPUPercent: 3.1, MemUsage: 420 << 20, MemLimit: 1024 << 20, StartedAt: time.Now().Add(-6 * time.Hour)},
	{Name: "randevu", Image: "ghcr.io/ornek/randevu:main", State: "running", Project: "randevu", CPUPercent: 0.2, MemUsage: 64 << 20, MemLimit: 128 << 20, StartedAt: time.Now().Add(-90 * time.Hour)},
	{Name: "haber", Image: "ghcr.io/ornek/haber:main", State: "exited", Project: "haber", MemLimit: 256 << 20, StartedAt: time.Now().Add(-9 * time.Hour)},
	{Name: "footops-cloudflared", Image: "cloudflare/cloudflared:2026.10.3", State: "running", Project: "footops-core", CPUPercent: 0.1, MemUsage: 22 << 20, MemLimit: 4 << 30, StartedAt: time.Now().Add(-80 * time.Hour)},
}

// previewDomains gives a site domains other than <name>.example.com and its www.
var previewDomains = map[string][]string{
	"klinik": {"klinik.example.com", "www.klinik.example.com", "ozel-klinik.example.org", "www.ozel-klinik.example.org"},
	"buyuksehir-belediyesi-kultur-mrk": {"kultur.buyuksehir-belediyesi.example.com", "www.kultur.buyuksehir-belediyesi.example.com",
		"kulturmerkezi.example.org", "www.kulturmerkezi.example.org", "rezervasyon.kulturmerkezi.example.org", "bilet.kulturmerkezi.example.org"},
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
	for i := range previewContainers {
		c := &previewContainers[i]
		n := c.Name
		if c.Project == "footops-core" {
			continue
		}
		domains := previewDomains[n]
		if domains == nil {
			domains = []string{n + ".example.com", "www." + n + ".example.com"}
		}
		mount := "/app/uploads"
		if n == "randevu" {
			mount = "" // neither a database nor a persistent folder
		}
		out = append(out, agent.SiteStatus{Spec: sites.Spec{Name: n, Group: "musteri", Domains: domains,
			Image: c.Image, Port: 3000, MemoryMB: int(c.MemLimit >> 20), Database: n != "blog" && n != "randevu", DBVar: "MONGODB_URI",
			DataMount: mount, DataURL: map[bool]string{true: "/uploads"}[n == "magaza"], EnvKeys: []string{"NODE_ENV", "SESSION_SECRET", "RESEND_API_KEY"},
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

// Job shows a database copy: /jobs/running (with progress and cancel) or /jobs/done;
// /jobs/backup a night run, /jobs/backupsetup a new repository.
func (p *previewAgent) Job(_ context.Context, id string) (agent.Job, error) {
	switch id {
	case "backup":
		return agent.Job{ID: id, Kind: "backup.run", Target: "gece yedeği", Started: time.Now().Add(-74 * time.Second), Finished: time.Now(), Done: true,
			Err:   "1 işlem başarısız: kurumsal (veritabanı): mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused",
			Usage: run.Usage{PeakMem: 174 << 20, CPU: 21 * time.Second},
			Steps: []string{"… yedek deposu: footops-h1 · sınır: 512 MB bellek, %50 CPU",
				"✓ magaza · veritabanı: 3 koleksiyon, 3401 belge (4.0 MB döküm) → depoya 310.0 KB eklendi (3.2 sn, en çok 81.0 MB bellek)",
				"✓ magaza · dosyalar: 1240 dosya, 356.0 MB (yeni 3, değişen 0) → depoya 2.0 MB eklendi (4.1 sn, en çok 81.0 MB bellek)",
				"✗ kurumsal · veritabanı: mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused",
				"✓ Saklama kuralı uygulandı: 14 eski anlık görüntü silindi", "✓ Depoda yedeklerin kapladığı: 6.7 GB"}}, nil
	case "purge":
		return agent.Job{ID: id, Kind: "site.purge", Target: "pos", Started: time.Now().Add(-9 * time.Second), Finished: time.Now(), Done: true,
			Result: "", Usage: run.Usage{PeakMem: 61 << 20, CPU: 900 * time.Millisecond},
			Steps: []string{"ℹ Kalıcı silinecek: pos (musteri grubu, 2026-11-02 13:15 kaldırıldı) — klasör 3.0 MB, 12 dosya",
				"✓ Veritabanı pos: 4 koleksiyon, 1210 belge, 8.0 MB veri", "… mongodump çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ mongodump bitti: 4 koleksiyon, 1210 belge → 412.0 KB (0.6 sn, en çok 61.0 MB bellek, 0.4 sn CPU)",
				"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 2026-11-07 14:02:11 (sunucuda 7 gün saklanır)",
				"✓ MongoDB: pos veritabanı ve kullanıcıları silindi", "✓ Klasör silindi: /srv/musteri/_removed/pos-20261102-101500 (3.0 MB)",
				"ℹ İmaj silinmedi (başka bir container kullanıyor ya da zaten yok): ghcr.io/ornek/pos:main"}}, nil
	case "bopen":
		return agent.Job{ID: id, Kind: "backup.open", Target: "magaza", Started: time.Now().Add(-8 * time.Second), Finished: time.Now(), Done: true,
			Result: "RunGece01", Usage: run.Usage{PeakMem: 104 << 20, CPU: 1900 * time.Millisecond},
			Steps: []string{"… Yedekteki veritabanı indiriliyor: 06.11.2026 04:00, 4.0 MB döküm", "✓ İndirildi: 4.0 MB (2.1 sn)",
				"… Geçici veritabanına yükleniyor (site etkilenmez)", "… mongorestore çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ Deneme yüklemesi tamam: 6 koleksiyon, 1860 belge, 1 görünüm (view) (1.9 sn, en çok 104.0 MB bellek, 1.1 sn CPU)",
				"✓ Sayımlar yedek alınırken sayılanla aynı",
				"✓ Yedek açıldı: 6 koleksiyon, 1860 belge, 1 görünüm (view) — salt okunur incelenebilir; 05:12 kendiliğinden kapanır"}}, nil
	case "brestore":
		return agent.Job{ID: id, Kind: "backup.restore", Target: "magaza", Started: time.Now().Add(-31 * time.Second), Finished: time.Now(), Done: true,
			Result: "20261106-091502 files-20261106-091504", Usage: run.Usage{PeakMem: 121 << 20, CPU: 6 * time.Second},
			Steps: []string{"ℹ Geri yüklenecek yedek: 06.11.2026 04:00 (gece yedeği) — veritabanı (6 koleksiyon, 1860 belge), dosyalar (1240 dosya, 356.0 MB)",
				"… Dosyalar yedekten açılıyor: 1240 dosya, 356.0 MB (site bu sırada çalışmaya devam eder)",
				"✓ Dosyalar açıldı: 1240 dosya, 31 klasör, 356.0 MB; sahip ve izinler yedekteki gibi (14.2 sn, en çok 92.0 MB bellek)",
				"✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)", "✓ Kopya hazır: 2026-11-06 12:15:02 (sunucuda 7 gün saklanır)",
				"… Veritabanı: yedek 06.11.2026 04:00 yükleniyor (tamamen değiştir)", "✓ Arşivde olmayan 1 koleksiyon silindi: oturum_kayitlari_yeni_eklenen_koleksiyon",
				"✓ Yüklendi ve sayıldı: 6 koleksiyon, 1860 belge (önce 1799 belge) — 2.0 sn, en çok 121.0 MB bellek, 1.2 sn CPU",
				"✓ Kalıcı klasör değiştirildi (tamamen değiştir); önceki hali data.old-20261106-091504, 7 gün saklanır",
				"✓ Site başlatıldı", "✓ Site sağlıklı cevap veriyor", "✓ Açılan yedek kapatıldı (geçici veritabanı silindi)",
				"ℹ Veritabanını geri almak için: site sayfası → Veritabanı kopyaları → 2026-11-06 12:15:02 → Geri yükle",
				"ℹ Dosyaları geri almak için: site sayfası → Dosya kopyaları → 2026-11-06 12:15:04 → Geri yükle"}}, nil
	case "bexport":
		return agent.Job{ID: id, Kind: "backup.export", Target: "magaza", Started: time.Now().Add(-19 * time.Second), Finished: time.Now(), Done: true,
			Result: "bexport", Usage: run.Usage{PeakMem: 88 << 20, CPU: 7 * time.Second},
			Steps: []string{"ℹ Yedek: 06.11.2026 04:00 (gece yedeği)", "… Veritabanı yedekten alınıyor: 4.0 MB döküm, sıkıştırılarak",
				"✓ Veritabanı: 4.0 MB → 610.0 KB sıkıştırılmış", "… Dosyalar yedekten alınıyor: 1240 dosya, 356.0 MB",
				"✓ Dosyalar: 1239 dosya, 31 klasör, 355.9 MB → 352.0 MB — 1 öğe alınmadı (bağlantı, özel dosya ya da içe aktarılamayacak ad)",
				"✓ Ortam değişkenleri: 9 değişken (değerleri arşivde, sırlar dahil)",
				"✓ Arşiv hazır: magaza-20261106-040000.footops.tar (352.7 MB); sunucuda 24 saat indirilebilir"}}, nil
	case "backupsetup":
		return agent.Job{ID: id, Kind: "backup.setup", Target: "footops-h1", Started: time.Now().Add(-6 * time.Second), Finished: time.Now(), Done: true, Result: "created",
			Steps: []string{"✓ restic 0.19.1 hazır (özet doğrulandı)", "… yedek deposu deneniyor: footops-h1", "… depo yok, yeni depo oluşturuluyor",
				"✓ Yeni depo oluşturuldu (şifreli; şifre panelde bir kez gösterilir)"}}, nil
	}
	j := agent.Job{ID: id, Kind: "db.snapshot", Target: "magaza", Cancellable: true, Started: time.Now().Add(-4 * time.Second),
		Steps: []string{"✓ Veritabanı magaza: 10 koleksiyon, 89 belge, 41.2 KB veri", "… mongodump çalışıyor (sınır: 512 MB bellek, %50 CPU)"}, Bytes: 3 << 20}
	if id == "import" {
		return agent.Job{ID: id, Kind: "import.apply", Target: "magaza", Started: time.Now().Add(-9 * time.Second), Finished: time.Now(),
			Done: true, Result: "20261027-110000", Usage: run.Usage{PeakMem: 96 << 20, CPU: 2 * time.Second},
			Steps: []string{"✓ Veritabanı magaza: 4 koleksiyon, 2759 belge, 1.1 MB veri", "… mongodump çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ mongodump bitti: 4 koleksiyon, 2759 belge → 240.1 KB (0.9 sn, en çok 61.0 MB bellek, 210 ms CPU)",
				"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 2026-10-27 14:00:00 (sunucuda 7 gün saklanır)",
				"✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)", "… yüklenen dosya magaza-20261027-091500.footops.tar yükleniyor (tamamen değiştir)",
				"✓ Arşivde olmayan 1 koleksiyon silindi: oturumlar", "… mongorestore çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ Yüklendi ve sayıldı: 4 koleksiyon, 2641 belge (önce 2759 belge) — 2.1 sn, en çok 96.0 MB bellek, 1.4 sn CPU",
				"✓ Site başlatıldı", "✓ Site sağlıklı cevap veriyor", "✓ Yüklenen dosya sunucudan silindi",
				"ℹ Geri almak için: site sayfası → Veritabanı kopyaları → 2026-10-27 14:00:00 → Geri yükle"}}, nil
	}
	if id == "migcopy" {
		return agent.Job{ID: id, Kind: "migrate.copy", Target: "magaza", Started: time.Now().Add(-42 * time.Second), Finished: time.Now(),
			Done: true, Result: "20261028-090000 files-20261028-090001", Usage: run.Usage{PeakMem: 131 << 20, CPU: 2100 * time.Millisecond},
			Steps: []string{"ℹ Kaynak: eski sunucu (root@203.0.113.20:22) — oradaki site çalışmaya devam eder, yalnız okunur",
				"✓ Kaynakta /home/magaza/www/public/uploads: 512 dosya, 14 klasör, 230.0 MB; alınmayacak: 2 gizli, 0 bağlantı, 0 özel öğe",
				"… Kaynakta mongodump çalışıyor (sunucuda, veritabanı magazadb) → SSH → bu sunucu",
				"✓ Döküm geldi: 5 koleksiyon, 804 belge → 310.0 KB (1.2 sn, en çok 9.0 MB bellek, 40 ms CPU)",
				"… Deneme yüklemesi: magazadb → geçici veritabanı (site etkilenmez)", "… mongorestore çalışıyor (sınır: 512 MB bellek, %50 CPU)",
				"✓ Deneme yüklemesi tamam: 5 koleksiyon, 804 belge (0.9 sn, en çok 101.0 MB bellek, 300 ms CPU)",
				"… Dosyalar kaynaktan doğrudan bu sunucuya akıyor (SSH, tar → yardımcı; site burada çalışırken data.new'a)",
				"… Dosyalar açılıyor (site kullanıcısı 1001, sınır: 256 MB bellek, %50 CPU)",
				"✓ Dosyalar açıldı: 510 dosya, 14 klasör, 229.0 MB — 6.8 sn, en çok 22.0 MB bellek, 1.1 sn CPU",
				"✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)", "✓ Kopya hazır: 2026-10-28 12:00:00 (sunucuda 7 gün saklanır)",
				"✓ Yüklendi ve sayıldı: 5 koleksiyon, 804 belge (önce 0 belge) — 1.1 sn, en çok 96.0 MB bellek, 0.4 sn CPU",
				"✓ Kalıcı klasör değiştirildi (tamamen değiştir); önceki hali data.old-20261028-090001, 7 gün saklanır",
				"✓ Site başlatıldı", "✓ Site sağlıklı cevap veriyor",
				"✓ Ön kopya tamam: site bu sunucuda kaynaktaki verinin kopyasıyla çalışıyor; DNS hâlâ eski sunucuyu gösteriyor"}}, nil
	}
	if id == "imgcheck" || id == "imgfail" {
		info := agent.ImageInfo{Image: "ghcr.io/ornek/magaza:main", ID: "4f2a9c1b7d3e", Size: 312 << 20, Platform: "linux/amd64",
			Server: "linux/amd64", User: "nextjs", UID: 1001, GID: 1001, Ports: []int{3000}, Volumes: []string{"/app/uploads"},
			EnvPort: "3000", EnvHostname: "0.0.0.0"}
		b, _ := json.Marshal(info)
		j := agent.Job{ID: id, Kind: "image.check", Target: info.Image, Started: time.Now().Add(-14 * time.Second), Finished: time.Now(), Done: true,
			Steps: []string{"İmaj indiriliyor (ya da güncel mi diye bakılıyor): " + info.Image, "✓ İmaj hazır: 312.0 MB, linux/amd64, 4f2a9c1b7d3e (12.8 sn)",
				"✓ Kullanıcı: nextjs (uid 1001, gid 1001)", "✓ Port önerisi: 3000 (EXPOSE)", "ℹ İmajın veri klasörleri (VOLUME): /app/uploads"},
			Result: string(b)}
		if id == "imgfail" {
			j.Steps, j.Result = j.Steps[:1], ""
			j.Err = "imaja erişim yok: özel bir imajsa bu sunucuda kayıt defterine giriş yapılmamış ya da token'ın bu pakete okuma izni yok (İmaj girişleri sayfası) (Error response from daemon: unauthorized)"
			j.Steps = append(j.Steps, "✗ "+j.Err)
		}
		return j, nil
	}
	if id == "done" {
		j.Done, j.Finished, j.Result = true, time.Now(), "20261027-110000"
		j.Usage = run.Usage{PeakMem: 38 << 20, CPU: 420 * time.Millisecond}
		j.Steps = append(j.Steps, "✓ mongodump bitti: 10 koleksiyon, 89 belge → 8.6 KB (1.8s, en çok 38.0 MB bellek, 420ms CPU)",
			"✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)", "✓ Kopya hazır: 2026-10-27 14:00:00 (sunucuda 7 gün saklanır)")
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
		Manifest:      &archive.Manifest{Site: "magaza", Created: time.Now().Add(-26 * time.Hour)},
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
		{Key: "backup:failed", Level: "bad", Title: "Yedek alınamadı: kurumsal (veritabanı)", Detail: "mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused", Link: "/backup", Since: time.Now().Add(-8 * time.Hour)},
		{Key: "backup:partial", Level: "warn", Title: "Bazı dosyalar yedeklenemedi: blog (dosyalar)", Detail: "bazı dosyalar okunamadı: open /footops/blog/files/cache/oturum-1: permission denied", Link: "/backup", Since: time.Now().Add(-8 * time.Hour)},
	}, nil
}

// previewBackup is the backup page's state: setup, key (the password shown), ready or
// running; /dev-backup?s=… switches it.
var previewBackup = "ready"

const previewKey = "Xq7pV2mK9tR4wZ8nB3cH6jL1sD5fG0yA2eU7iO4k"

func (p *previewAgent) BackupStatus(context.Context) (agent.BackupStatus, error) {
	s := agent.DefaultBackupSettings()
	s.Sites = map[string]agent.BackupPolicy{"magaza": {DB: agent.DBHourly}, "kurumsal": {Freeze: true, KeepDays: 90},
		"haber": {Off: true}, "buyuksehir-belediyesi-kultur-mrk": {DB: agent.DB6h, Files: agent.FilesWeekly}}
	switch previewBackup {
	case "setup":
		return agent.BackupStatus{Settings: s}, nil
	case "rehearsal":
		return agent.BackupStatus{Settings: s, Rehearsal: true}, nil
	case "key":
		return agent.BackupStatus{Configured: true, Repository: "footops-h1", Settings: s}, nil
	}
	now := time.Now()
	s.Confirmed = now.Add(-9 * 24 * time.Hour)
	night := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, time.Local)
	if night.After(now) {
		night = night.AddDate(0, 0, -1)
	}
	st := agent.BackupStatus{Configured: true, Repository: "footops-h1", Settings: s, NightDone: night.Format("2006-01-02"),
		NextNight: night.AddDate(0, 0, 1), LastPrune: night.AddDate(0, 0, -3), LastCheck: night.AddDate(0, 0, -3),
		RepoSize: 6900 << 20, RepoSized: night.Add(3 * time.Minute), Last: map[string]agent.BackupLast{}}
	sites, _ := p.Sites(context.Background())
	for _, ss := range sites {
		if ss.Spec.Name == "haber" {
			for _, part := range []string{"db", "files", "def"} {
				st.Last["haber/"+part] = agent.BackupLast{Time: night.AddDate(0, 0, -12)}
			}
			continue
		}
		st.Last[ss.Spec.Name+"/def"] = agent.BackupLast{Time: night}
		if ss.Spec.Database && ss.Spec.Name != "kurumsal" {
			st.Last[ss.Spec.Name+"/db"] = agent.BackupLast{Time: night}
		}
		if ss.Spec.DataMount != "" {
			st.Last[ss.Spec.Name+"/files"] = agent.BackupLast{Time: night}
		}
	}
	st.Last["magaza/db"] = agent.BackupLast{Time: now.Add(-25 * time.Minute)}
	st.Last["_server/server"] = agent.BackupLast{Time: night}
	st.Failed = map[string]agent.BackupFailure{
		"kurumsal/db": {Time: night, Err: "mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused"},
		"blog/files":  {Time: night, Err: "bazı dosyalar okunamadı: open /footops/blog/files/cache/oturum-1: permission denied", Warning: true},
	}
	st.Trial = &agent.BackupTrial{Time: night.Add(-3*24*time.Hour + 6*time.Minute), Site: "buyuksehir-belediyesi-kultur-mrk", OK: true,
		Detail: "veritabanı 14 koleksiyon / 128455 belge aynı · 20 dosyanın 17 tanesi aynı (3 tanesi yedekten beri değiştiği için yalnız boyutu)"}
	if previewBackup == "trialfail" {
		st.Trial = &agent.BackupTrial{Time: night.Add(6 * time.Minute), Site: "kurumsal", Err: "veritabanı: sayımlar yedek alınırken sayılanla tutmuyor: siparisler: kayıtta 18342, yüklenen 18341"}
	}
	if previewBackup == "running" {
		st.Running = &agent.Job{ID: "running", Kind: "backup.run", Target: "saatlik yedek: magaza", Started: now.Add(-6 * time.Second)}
	}
	return st, nil
}

func (p *previewAgent) BackupRuns(_ context.Context, site string, limit int) ([]agent.BackupRun, error) {
	now := time.Now()
	night := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, time.Local)
	if night.After(now) {
		night = night.AddDate(0, 0, -1)
	}
	use := run.Usage{PeakMem: 81 << 20}
	nightParts := []agent.BackupPart{
		{Site: "magaza", Part: "db", Snapshot: "a1", Collections: map[string]int64{"orders": 1, "products": 2, "users": 3}, Documents: 3401, Bytes: 4 << 20, AddedPacked: 310 << 10, Took: 3200 * time.Millisecond, Usage: use},
		{Site: "magaza", Part: "files", Snapshot: "a2", Files: 1240, Bytes: 356 << 20, AddedPacked: 2 << 20, Took: 4100 * time.Millisecond, Usage: use},
		{Site: "magaza", Part: "def", Snapshot: "a3", Files: 2, Bytes: 3 << 10, AddedPacked: 900, Took: 800 * time.Millisecond, Usage: use},
		{Site: "blog", Part: "files", Snapshot: "a4", Files: 88, Bytes: 12 << 20, Warning: "bazı dosyalar okunamadı: open /footops/blog/files/cache/oturum-1: permission denied", Took: 1200 * time.Millisecond, Usage: use},
		{Site: "kurumsal", Part: "db", Err: "mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused", Took: 900 * time.Millisecond},
		{Site: "_server", Part: "server", Snapshot: "a5", Files: 41, Bytes: 180 << 10, AddedPacked: 2 << 10, Took: 900 * time.Millisecond, Usage: use},
	}
	all := []agent.BackupRun{
		{ID: "h3", Kind: agent.RunHourly, Started: now.Add(-25 * time.Minute), Finished: now.Add(-25*time.Minute + 4*time.Second),
			Parts: []agent.BackupPart{{Site: "magaza", Part: "db", Snapshot: "b1", Collections: map[string]int64{"orders": 1}, Documents: 3402, Bytes: 4 << 20, AddedPacked: 120 << 10, Took: 3 * time.Second, Usage: use}}},
		{ID: "m1", Kind: agent.RunManual, Actor: "admin", Started: now.Add(-3 * time.Hour), Finished: now.Add(-3*time.Hour + 9*time.Second),
			Parts: []agent.BackupPart{{Site: "magaza", Part: "db", Snapshot: "c1", Collections: map[string]int64{"orders": 1}, Documents: 3399, Bytes: 4 << 20, AddedPacked: 98 << 10, Took: 3 * time.Second, Usage: use}}},
		{ID: "n1", Kind: agent.RunDaily, Started: night, Finished: night.Add(74 * time.Second), Parts: nightParts, Removed: 14, RepoSize: 6900 << 20,
			Err: "2 işlem başarısız: kurumsal (veritabanı): mongodump başarısız, yedek alınmadı: Failed: can't create session: connection refused"},
		{ID: "x1", Kind: agent.RunManual, Actor: "admin", Started: night.Add(-5 * time.Hour), Finished: night.Add(-5*time.Hour + 40*time.Second), Err: "context canceled",
			Parts: []agent.BackupPart{{Site: "magaza", Part: "db", Snapshot: "d1", Documents: 3390, Bytes: 4 << 20, Took: 3 * time.Second, Usage: use}}},
		{ID: "n0", Kind: agent.RunDaily, Started: night.AddDate(0, 0, -1), Finished: night.AddDate(0, 0, -1).Add(66 * time.Second), Parts: nightParts[:3], Pruned: true, Checked: true, RepoSize: 6800 << 20},
	}
	var out []agent.BackupRun
	for _, r := range all {
		for _, part := range r.Parts {
			if site == "" || part.Site == site {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// previewSource is a connected source server; /sources also lists one not connected yet.
var previewSource = agent.SourceView{Source: remote.Source{ID: "kaynak000001", Label: "eski sunucu", Host: "203.0.113.20", Port: 22, User: "root",
	PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOrnek footops-h1-kaynak000001", HostKeys: []string{"ED25519 SHA256:3fQkV0mF6c1x2pXq9Zr0b8yWk4u7Lh1sQe2nTt5aBcD"},
	Checked: time.Now().Add(-40 * time.Minute), System: "CentOS Linux 7 (Core) · 3.10.0-1160.el7.x86_64", Created: time.Now().Add(-2 * time.Hour)},
	UsedBy: []string{"magaza"}}

func (p *previewAgent) Registries(context.Context) ([]agent.RegistryLogin, error) {
	return []agent.RegistryLogin{
		{Host: "ghcr.io", User: "ornek", Saved: time.Now().Add(-80 * 24 * time.Hour), Actor: "admin", Expires: time.Now().Add(10 * 24 * time.Hour), Scopes: []string{"read:packages"}},
		{Host: "registry.example.com:5000", User: "deploy"},
	}, nil
}

func (p *previewAgent) SiteDNS(_ context.Context, site string) ([]agent.DomainCheck, error) {
	return []agent.DomainCheck{
		{Domain: site + ".example.com", DNS: "cloudflare", IPs: []string{"104.21.32.1", "172.67.150.2"}, Public: 200, CFRay: true, Local: 200},
		{Domain: "www." + site + ".example.com", DNS: "elsewhere", IPs: []string{"203.0.113.20"}, Public: 502, CFRay: true, Local: 200},
	}, nil
}

func (p *previewAgent) Sources(context.Context) ([]agent.SourceView, error) {
	fresh := agent.SourceView{Source: remote.Source{ID: "kaynak000002", Label: "arkadaşın sunucusu", Host: "eski.example.net", Port: 2222, User: "root",
		PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIYeni footops-h1-kaynak000002"}}
	fresh.Authorized = `restrict,from="203.0.113.10" ` + fresh.PublicKey
	fresh.Install = remote.InstallCommand(fresh.Authorized)
	return []agent.SourceView{previewSource, fresh}, nil
}

// Migration shows the wizard for magaza with a discovery, a scan and a finished copy;
// /sites/blog/migrate is a site whose move has not started.
func (p *previewAgent) Migration(_ context.Context, site string) (agent.MigrationView, error) {
	srcs, _ := p.Sources(context.Background())
	v := agent.MigrationView{Sources: srcs}
	if site != "magaza" {
		return v, nil
	}
	now := time.Now()
	disc := &remote.Discovery{Taken: now.Add(-25 * time.Minute), Complete: true,
		System: remote.System{OS: "CentOS Linux 7 (Core)", CPUs: 4, MemTotal: 8 << 30, MemAvail: 2 << 30,
			Disks: []remote.Disk{{Mount: "/", Size: 200 << 30, Used: 96 << 30, Avail: 104 << 30}}},
		Tools: []remote.Tool{{Name: "mongodump"}, {Name: "mongo"}, {Name: "node"}, {Name: "nginx"}, {Name: "tar"}, {Name: "find"}},
		Mongo: []remote.MongoSource{{Via: "host", Shell: "mongo", Version: "4.0.28", DBs: []remote.MongoDB{
			{Name: "magazadb", Collections: 12, Objects: 804, DataSize: 1400 << 10},
			{Name: "eskiblog", Collections: 7, Objects: 120, DataSize: 300 << 10}}}},
		Web: []remote.WebServer{{File: "/etc/nginx/conf.d/magaza.conf", Names: []string{"magaza.example.com", "www.magaza.example.com"},
			Locations: []remote.Location{{Path: "/", Proxy: "http://127.0.0.1:8196"}}}},
		Procs: []remote.Proc{{PID: 1001, User: "root", Daemon: true, PM2Home: "/root/.pm2", Cmd: "PM2 v4.5.6: God Daemon (/root/.pm2)"},
			{PID: 2001, PPID: 1001, User: "root", RSS: 180 << 20, Cwd: "/home/magaza/www", PM2Name: "magaza", PM2ID: "3", Ports: []int{8196}}},
		Containers: []remote.Container{{Name: "eski-redis", Image: "redis:5", Status: "Up 3 months"}},
		Apps:       []remote.AppDir{{Dir: "/home/magaza/www", KB: 480000}},
		Candidates: []remote.Candidate{{Dir: "/home/magaza/www/public/uploads", Counts: remote.Counts{Files: 512, Bytes: 230 << 20, Dirs: 14, Hidden: 2}}}}
	sc := &remote.Scan{Taken: now.Add(-20 * time.Minute), Dir: "/home/magaza/www", Complete: true,
		Total: remote.Counts{Files: 41210, Bytes: 480 << 20, Hidden: 6},
		Subs: []remote.SubDir{{Name: "node_modules", Counts: remote.Counts{Files: 40500, Bytes: 210 << 20}},
			{Name: "public", Counts: remote.Counts{Files: 690, Bytes: 268 << 20}}, {Name: "lib", Counts: remote.Counts{Files: 20, Bytes: 80 << 10}}},
		Files:   []string{"package.json", "app.js"},
		Package: &remote.Package{Name: "magaza", Main: "app.js", Node: ">=14", Deps: []string{"express", "mongoose", "multer", "nodemailer"}},
		Env:     []remote.EnvFile{{File: ".env", Keys: []string{"DB_URI", "PORT", "SECRET_KEY"}}},
		Secrets: []remote.Finding{{Kind: "password", Where: "lib/mailConf.js:7"}, {Kind: "smtp", Where: "lib/mailConf.js:3"}},
		DBRefs:  []remote.Ref{{Value: "magazadb", Count: 1}},
		URLs:    []remote.Ref{{Value: "http://203.0.113.20:8196", Count: 3}}}
	sg := remote.Suggest(*disc, sc, "magaza", []string{"magaza.example.com"})
	v.Discovery, v.Scan, v.Suggest = disc, sc, &sg
	v.Source = &previewSource
	m := &agent.Migration{Site: site, Source: previewSource.ID, AppDir: "/home/magaza/www", FilesDir: "/home/magaza/www/public/uploads",
		DB: "magazadb", Created: now.Add(-time.Hour), Updated: now.Add(-15 * time.Minute)}
	files := datafs.Stats{Files: 510, Bytes: 229 << 20}
	m.Copies = []agent.CopyResult{{Job: "migcopy", Started: now.Add(-10 * time.Minute), Finished: now.Add(-9*time.Minute - 18*time.Second),
		DB: "magazadb", SourceDocs: map[string]int64{"urunler": 412, "kategoriler": 18, "siparisler": 344, "ayarlar": 1, "sayfalar": 29},
		SiteDocs: map[string]int64{"urunler": 412, "kategoriler": 18, "siparisler": 344, "ayarlar": 1, "sayfalar": 29}, DumpBytes: 310 << 10,
		FilesDir: m.FilesDir, SourceFiles: &remote.Counts{Files: 512, Bytes: 230 << 20, Dirs: 14, Hidden: 2}, SiteFiles: &files,
		Notes: []string{"Dosyalar: kaynakta 512 / 230.0 MB sayılmıştı, sitede 510 / 229.0 MB (kaynak arada değişmiş olabilir)."},
		Undo:  now.Add(-10*time.Minute).UTC().Format("20060102-150405") + " files-" + now.Add(-10*time.Minute).UTC().Format("20060102-150405"),
		Usage: run.Usage{PeakMem: 131 << 20, CPU: 2100 * time.Millisecond}}}
	v.Migration = m
	return v, nil
}

// previewNotify is the notifications page's state: none, link (the code shown), linked
// or failing; /dev-notify?s=… switches it.
var previewNotify = "linked"

func (p *previewAgent) NotifyStatus(context.Context) (agent.NotifyStatus, error) {
	now := time.Now()
	file := &agent.StatusFileState{Key: "footops-h1/izleme/durum.json", LastOK: now.Add(-2 * time.Minute)}
	linked := agent.NotifyStatus{Linked: true, Bot: "OrnekBot", Chat: "Ayşe Yılmaz (@ayse_yilmaz_uzun_kullanici_adi)", Since: now.Add(-50 * time.Hour),
		By: "furkan", Told: 3, LastSent: now.Add(-7 * time.Hour), StatusFile: file}
	switch previewNotify {
	case "none":
		return agent.NotifyStatus{StatusFile: &agent.StatusFileState{}}, nil
	case "rehearsal":
		return agent.NotifyStatus{StatusFile: &agent.StatusFileState{}, Rehearsal: true}, nil
	case "link":
		return agent.NotifyStatus{Link: &agent.NotifyLink{Bot: "OrnekBot", Code: "K7Q2M9XDRA", URL: "https://t.me/OrnekBot?start=K7Q2M9XDRA",
			Expires: now.Add(29 * time.Minute)}, StatusFile: file}, nil
	case "failing":
		linked.LastErr, linked.LastErrAt, linked.Failing, linked.RetryAt = "Telegram'a ulaşılamadı: dial tcp: lookup api.telegram.org: i/o timeout",
			now.Add(-time.Minute), now.Add(-25*time.Minute), now.Add(4*time.Minute)
		linked.StatusFile = &agent.StatusFileState{Key: file.Key, LastOK: now.Add(-31 * time.Minute), Failing: now.Add(-26 * time.Minute),
			LastErr: "depo yazmayı reddetti (HTTP 403): AccessDenied Access Denied", LastErrAt: now.Add(-time.Minute)}
		return linked, nil
	}
	return linked, nil
}

func (p *previewAgent) Removed(_ context.Context, detail bool) ([]agent.RemovedSite, error) {
	list := []agent.RemovedSite{
		{ID: "pos-20261102-101500", Site: "pos", Group: "musteri", Removed: time.Now().Add(-120 * time.Hour), Database: true,
			Bytes: 3 << 20, Files: 12, DBBytes: 8 << 20, Image: "ghcr.io/ornek/pos:main"},
		{ID: "buyuksehir-belediyesi-kultur-mrk-20261001-090000", Site: "buyuksehir-belediyesi-kultur-mrk", Group: "musteri",
			Removed: time.Now().Add(-900 * time.Hour), Database: true, Current: true, Bytes: 356 << 20, Files: 2481},
		{ID: "eski-20260915-120000", Site: "eski", Group: "musteri", Removed: time.Now().Add(-1300 * time.Hour), Database: true, DBBytes: -1, Bytes: 4 << 10, Files: 2},
	}
	if !detail {
		for i := range list {
			list[i].Bytes, list[i].Files, list[i].DBBytes = 0, 0, 0
		}
	}
	return list, nil
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
	// /dev-notify?s=none|link|linked|failing switches the notifications page's state.
	mux.HandleFunc("/dev-notify", func(w http.ResponseWriter, r *http.Request) {
		previewNotify = r.URL.Query().Get("s")
		http.Redirect(w, r, "/notify", http.StatusSeeOther)
	})
	// /dev-backup?s=setup|key|ready|running|rehearsal|trialfail switches the backup page's state.
	mux.HandleFunc("/dev-backup", func(w http.ResponseWriter, r *http.Request) {
		previewBackup = r.URL.Query().Get("s")
		if previewBackup == "key" {
			http.Redirect(w, r, "/dev-backup-key", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
	})
	mux.Handle("/dev-backup-key", s.localGate(s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		s.renderBackup(frameable{w}, r, http.StatusOK, map[string]any{"Key": previewKey})
	})))
	t.Log("http://localhost:7799/dev-login")
	t.Fatal(http.ListenAndServe("127.0.0.1:7799", mux))
}

// ---- database browser (F5): /sites/magaza/db, /db/coll?c=products, /db/doc?c=products&id=… ----

const previewDoc = `{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "ad": "Kurşun kalem, 2B (12'li kutu)", "slug": "kursun-kalem-2b-12li",
	"fiyat": 129.9, "stok": 42, "aktif": true, "kategori": {"$oid": "6ae1c4a7ec711d7125395000"},
	"etiketler": ["kırtasiye", "okul", "çizim"], "ölçü": {"en": 18.5, "boy": 4.0, "birim": "cm"},
	"aciklama": "Yumuşak uçlu, kolay silinen grafit kalemler. Çizim ve eskiz için idealdir; ahşap gövde, kırılmaya dayanıklı uç.",
	"gorseller": [{"url": "/uploads/kalem-1.jpg", "alt": "Kutu"}, {"url": "/uploads/kalem-2.jpg", "alt": "Kalem"}],
	"olusturma": {"$date": "2026-09-14T10:22:31.512Z"}, "guncelleme": {"$date": "2026-10-27T18:03:09.004Z"},
	"satis": {"$numberLong": "1843"}, "kdv": {"$numberDecimal": "0.20"}, "__v": 3}`

func (p *previewAgent) DBCollections(context.Context, string) (agent.DBOverview, error) {
	return agent.DBOverview{DB: "magaza", Took: 9 * time.Millisecond, Collections: []agent.DBCollection{
		{Name: "aktif_urunler", Type: "view", Count: -1},
		{Name: "contactrequests", Type: "collection", Count: 3, Size: 1200, Indexes: 2, IndexSize: 36 << 10},
		{Name: "customers", Type: "collection", Count: 128, Size: 96 << 10, Indexes: 3, IndexSize: 72 << 10},
		{Name: "payments", Type: "collection", Count: 412, Size: 310 << 10, Indexes: 2, IndexSize: 64 << 10},
		{Name: "products", Type: "collection", Count: 1240, Size: 2 << 20, Indexes: 4, IndexSize: 220 << 10},
		{Name: "sessions", Type: "collection", Count: 12, Size: 8 << 10, Indexes: 2, IndexSize: 36 << 10, Capped: true},
	}}, nil
}

func (p *previewAgent) DBFind(_ context.Context, site string, q agent.DBQuery) (agent.DBPage, error) {
	page := agent.DBPage{Collection: agent.DBCollection{Name: q.Collection, Type: "collection"}, Page: max(q.Page, 1), PerPage: 20,
		Total: 1240, More: true, Took: 6 * time.Millisecond, Sort: "-_id",
		Indexes: []agent.DBIndex{{Name: "_id_", Keys: `{"_id": 1}`, TTL: -1}, {Name: "slug_1", Keys: `{"slug": 1}`, Unique: true, TTL: -1},
			{Name: "kategori_1_fiyat_-1", Keys: `{"kategori": 1, "fiyat": -1}`, TTL: -1}, {Name: "olusturma_1", Keys: `{"olusturma": 1}`, TTL: 2592000}}}
	if q.Collection == "aktif_urunler" {
		page.Collection.Type, page.Total, page.Indexes = "view", 812, nil
	}
	if q.Filter != "" {
		page.Filter, page.Total, page.More = q.Filter, 3, false
	}
	names := []string{"Kurşun kalem, 2B (12'li kutu)", "Defter A4 kareli", "Silgi", "Cetvel 30 cm", "Boya kalemi seti 24 renk"}
	for i := range 20 {
		if q.Filter != "" && i >= 3 {
			break
		}
		id := fmt.Sprintf(`{"$oid": "6ae1c4a7ec711d7125395%03d"}`, 999-i)
		page.Docs = append(page.Docs, agent.DBDocSummary{ID: id, IDText: id, Size: int64(600 + i*37), Fields: 16, Rest: 8,
			Preview: []agent.DBField{{Key: "ad", Text: strconv.Quote(names[i%len(names)])}, {Key: "slug", Text: `"urun-` + strconv.Itoa(i) + `"`},
				{Key: "fiyat", Text: strconv.FormatFloat(19.9+float64(i)*10, 'f', 1, 64)}, {Key: "stok", Text: strconv.Itoa(40 - i)},
				{Key: "aktif", Text: "true"}, {Key: "kategori", Text: `{"$oid": "6ae1c4a7ec711d7125395000"}`},
				{Key: "etiketler", Text: "[…] 3 öğe"}, {Key: "aciklama", Text: `"Yumuşak uçlu, kolay silinen grafit kalemler. Çizim ve eskiz için idealdir; ahşap gö…"`}}})
	}
	return page, nil
}

func (p *previewAgent) DBDoc(_ context.Context, site, coll, id string) (agent.DBDocument, error) {
	d, err := mongo.ParseDoc(previewDoc)
	if err != nil {
		return agent.DBDocument{}, err
	}
	text, _ := mongo.DocText(d, true)
	sum := sha256.Sum256(d)
	return agent.DBDocument{Collection: agent.DBCollection{Name: coll, Type: "collection"}, ID: id, Text: text, Size: int64(len(d)),
		Fields: 16, Hash: hex.EncodeToString(sum[:]), Editable: coll != "aktif_urunler", Deletable: coll != "aktif_urunler",
		Why: map[bool]string{true: "görünüm ve time-series koleksiyonları yalnız okunur"}[coll == "aktif_urunler"], Took: 4 * time.Millisecond}, nil
}

func (p *previewAgent) DBPreview(_ context.Context, site string, req agent.DBEditRequest) (agent.DBPreview, error) {
	return agent.DBPreview{Size: 742, Changes: []agent.DBChange{
		{Field: "fiyat", Kind: "changed", Old: "129.9", New: "139.9"},
		{Field: "stok", Kind: "changed", Old: "42", New: "40.0", OldType: "int", NewType: "double"},
		{Field: "indirim", Kind: "added", New: `{"oran": 10, "bitis": {"$date": "2026-11-30T00:00:00.000Z"}}`, NewType: "object"},
		{Field: "__v", Kind: "removed", Old: "3", OldType: "int"}}}, nil
}

func (p *previewAgent) DBEdits(context.Context, string) ([]agent.DBEdit, error) {
	now := time.Now()
	return []agent.DBEdit{
		{ID: "20261028-120501-000001-abcd", Collection: "products", DocID: `{"$oid": "6ae1c4a7ec711d712539599d"}`, Op: "edit", Time: now.Add(-20 * time.Minute),
			Actor: "admin", Changes: []agent.DBChange{{Field: "fiyat", Kind: "changed"}, {Field: "indirim", Kind: "added"}}, Before: true, After: true},
		{ID: "20261028-110000-000001-abcd", Collection: "contactrequests", DocID: `{"$oid": "6ae1c4a7ec711d7125391111"}`, Op: "delete", Time: now.Add(-2 * time.Hour),
			Actor: "admin", Before: true},
		{ID: "20261027-090000-000001-abcd", Collection: "customers", DocID: `"musteri-42"`, Op: "edit", Time: now.Add(-27 * time.Hour),
			Actor: "admin", Changes: []agent.DBChange{{Field: "telefon", Kind: "changed"}}, Before: true, After: true, UndoneBy: "20261027-091000-000001-abcd"},
		{ID: "20261027-091000-000001-abcd", Collection: "customers", DocID: `"musteri-42"`, Op: "undo", Time: now.Add(-26 * time.Hour),
			Actor: "admin", Changes: []agent.DBChange{{Field: "telefon", Kind: "changed"}}, Before: true, After: true, Pending: true},
	}, nil
}

func (p *previewAgent) DBEdit(_ context.Context, site, id string) (agent.DBEditView, error) {
	edits, _ := p.DBEdits(context.Background(), site)
	d, _ := mongo.ParseDoc(previewDoc)
	text, _ := mongo.DocText(d, true)
	for _, e := range edits {
		if e.ID == id {
			return agent.DBEditView{Edit: e, Before: text, After: text, BeforeSize: int64(len(d)), AfterSize: int64(len(d))}, nil
		}
	}
	return agent.DBEditView{}, &agent.Error{Status: 404, Message: "kayıt bulunamadı"}
}

// ---- restoring from backups (Y3): /sites/<site>/backups/RunGece01 (the night, its
// database open for a look), RunSaat02 (an hourly database backup, not opened) ----

func previewPoints(now time.Time) []agent.BackupPoint {
	night := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, time.Local)
	if night.After(now) {
		night = night.AddDate(0, 0, -1)
	}
	snap := func(n int, part string, t time.Time, files, bytes int64) agent.BackupSnapshot {
		return agent.BackupSnapshot{ID: fmt.Sprintf("%064x", n), Time: t, Part: part, Files: files, Bytes: bytes}
	}
	pts := []agent.BackupPoint{{Run: "RunSaat02", Time: now.Add(-25 * time.Minute), Kind: agent.RunHourly,
		Parts: map[string]agent.BackupSnapshot{"db": snap(9, "db", now.Add(-25*time.Minute), 1, 4<<20)}}}
	for d := 0; d < 18; d++ {
		t := night.AddDate(0, 0, -d)
		run := "RunGece01"
		if d > 0 {
			run = fmt.Sprintf("RunGece%02d", d+10)
		}
		pts = append(pts, agent.BackupPoint{Run: run, Time: t, Kind: agent.RunDaily, Parts: map[string]agent.BackupSnapshot{
			"db": snap(100+d, "db", t, 1, 4<<20), "files": snap(200+d, "files", t, 1240, 356<<20), "def": snap(300+d, "def", t, 2, 3<<10)}})
	}
	return pts
}

func previewView(now time.Time, pt agent.BackupPoint) *agent.BackupView {
	return &agent.BackupView{ID: "ViewOrnek1", Run: pt.Run, Taken: pt.Time, Ready: true, DB: "magaza", Opened: now.Add(-12 * time.Minute),
		Expires: now.Add(48 * time.Minute), Collections: map[string]int64{"contactrequests": 3, "customers": 126, "payments": 405, "products": 1238,
			"sessions": 11, "kampanya_kayitlari_2026_ilkbahar_donemi": 77}, Views: []string{"aktif_urunler"}, Documents: 1860}
}

func (p *previewAgent) SiteBackups(_ context.Context, site string) (agent.BackupList, error) {
	now := time.Now()
	pts := previewPoints(now)
	return agent.BackupList{Updated: now.Add(-25 * time.Minute), Points: pts, Views: []agent.BackupView{*previewView(now, pts[1])}}, nil
}

func (p *previewAgent) SiteBackup(_ context.Context, site, run string) (agent.BackupDetail, error) {
	now := time.Now()
	for _, pt := range previewPoints(now) {
		if pt.Run != run {
			continue
		}
		d := agent.BackupDetail{BackupPoint: pt, Repository: "footops-h1"}
		if run == "RunGece01" {
			d.View = previewView(now, pt)
			d.Record = []agent.BackupPart{{Site: site, Part: "db", Snapshot: pt.Parts["db"].ID,
				Collections: map[string]int64{"contactrequests": 3, "customers": 126, "payments": 405, "products": 1238, "sessions": 11, "system.js": 1}}}
		}
		return d, nil
	}
	return agent.BackupDetail{}, &agent.Error{Status: 404, Message: "bu yedek listede yok: saklama kuralıyla silinmiş olabilir (listeyi yenile)"}
}

func (p *previewAgent) BackupCompare(ctx context.Context, site, run string) (agent.BackupCompare, error) {
	d, err := p.SiteBackup(ctx, site, run)
	files := datafsStats(1251, 359<<20)
	return agent.BackupCompare{Backup: d, Current: agent.BackupCurrent{DB: "magaza", Collections: map[string]int64{"contactrequests": 3,
		"customers": 128, "payments": 412, "products": 1240, "sessions": 12, "oturum_kayitlari_yeni_eklenen_koleksiyon": 4},
		Views: []string{"aktif_urunler"}, Documents: 1799, Files: &files, Running: true}}, err
}

func datafsStats(files int, bytes int64) datafs.Stats {
	return datafs.Stats{Files: files, Bytes: bytes}
}

func (p *previewAgent) BackupFiles(_ context.Context, site, run, dir string) (agent.BackupFolder, error) {
	t := time.Now().Add(-26 * time.Hour)
	l := agent.BackupFolder{Path: dir, Took: 820 * time.Millisecond, Entries: []agent.BackupEntry{
		{Name: "2026-kampanya-gorselleri-ilkbahar-yaz-donemi-urun-fotograflari", Type: "dir", Mode: "drwxr-xr-x", MTime: t},
		{Name: "urunler", Type: "dir", Mode: "drwxr-xr-x", MTime: t},
		{Name: ".htaccess", Type: "file", Size: 120, Mode: "-rw-r--r--", MTime: t},
		{Name: "katalog-2026-sonbahar-kis-koleksiyonu-tum-urunler-fiyat-listesi.pdf", Type: "file", Size: 18 << 20, Mode: "-rw-r--r--", MTime: t},
		{Name: "logo.png", Type: "file", Size: 42 << 10, Mode: "-rw-r--r--", MTime: t},
		{Name: "son-katalog", Type: "symlink", Mode: "Lrwxrwxrwx", MTime: t},
	}}
	return l, nil
}

func (p *previewAgent) ViewCollections(ctx context.Context, site, view string) (agent.DBOverview, error) {
	return p.DBCollections(ctx, site)
}

func (p *previewAgent) ViewFind(ctx context.Context, site, view string, q agent.DBQuery) (agent.DBPage, error) {
	return p.DBFind(ctx, site, q)
}

func (p *previewAgent) ViewDoc(ctx context.Context, site, view, coll, id string) (agent.DBDocument, error) {
	return p.DBDoc(ctx, site, coll, id)
}

// The server check and updates (Faz U1, U3): a report with one of each level, a release
// ready, the last update rolled back.
func (p *previewAgent) DoctorStatus(context.Context) (agent.DoctorStatus, error) {
	f := func(id, section, level, title, detail, fix string) doctor.Finding {
		return doctor.Finding{ID: id, Section: section, Level: level, Title: title, Detail: detail, Fix: fix}
	}
	findings := []doctor.Finding{}
	for _, n := range []string{"Temel paketler", "Sunucu adı", "Saat dilimi", "Takas alanı (swap)", "Güvenlik duvarı (UFW)", "fail2ban", "Docker", "Docker ayarları", "Çekirdek container'lar", "Panel servisleri"} {
		findings = append(findings, f("step:"+n, "standard", doctor.OK, n, "", ""))
	}
	findings = append(findings,
		f("step:ssh-hardening", "standard", doctor.Bad, "Standarttan farklı: SSH ayarları", "Kurulumun bu adımı: SSH: root ve şifreyle giriş kapalı, yalnız furkan anahtarla.", "sudo footops install (önceki cevaplarla; yalnız farklı olanı düzeltir)"),
		f("reboot", "system", doctor.Info, "Yeniden başlatma bekliyor; sunucu kendiliğinden yeniden başlayacak (saat 04:00)", "Yeniden başlatma 5 saat önce istendi (linux-image-6.8.0-150-generic).", ""),
		f("updates", "system", doctor.Info, "3 paket güncellemesi bekliyor", "Diğer: docker-ce, docker-ce-cli, containerd.io — kendiliğinden kurulmaz.", "SSH'den: sudo apt upgrade (Docker güncellenirken container'lar çalışmaya devam eder)"),
		f("unattended", "system", doctor.OK, "Otomatik güvenlik güncellemeleri çalışıyor (son: 2 saat önce)", "", ""),
		f("time", "network", doctor.OK, "Saat senkron (NTP)", "", ""),
		f("ports", "network", doctor.Warn, "Beklenmeyen dinleyen port: tcp 8080 (node)", "Güvenlik duvarı (UFW) içeri girişi kapalı tuttuğu sürece dışarıdan ulaşılamaz; yine de ne olduğuna bak.", "sudo ss -tulnp ile süreci gör"),
		f("images", "disk", doctor.Info, "7 imajı hiçbir container kullanmıyor (812,4 MB ile 2,1 GB arası)", "ghcr.io/ornek/magaza:<eski>, ghcr.io/buyuksehir-belediyesi/buyuksehir-belediyesi-kultur-merkezi:2026-10-30-main, etiketsiz 4f2c1d9e8a7b ve 4 tane daha.", ""),
		f("accounts", "accounts", doctor.OK, "Yönetici hesabı, bulut hesapları ve sudo kuralları standartta", "", ""),
		f("footops", "versions", doctor.OK, "FootOps v0.1.0", "", ""),
		f("docker", "versions", doctor.Info, "Docker 29.9.3 — 29.9.5 çıkmış", "", "SSH'den: sudo apt upgrade (container'lar çalışmaya devam eder)"),
		f("cloudflared", "versions", doctor.OK, "cloudflared 2026.10.3 (Cloudflare desteği bitişi: 2027-10)", "", ""))
	at := time.Now().Add(-35 * time.Minute)
	return agent.DoctorStatus{Report: &doctor.Report{Format: 1, Host: "h1", FootOps: "v0.1.0", Time: at, TookMS: 1840, Findings: findings},
		Confirmed: map[string]bool{"step:ssh-hardening": true}, Next: time.Now().Add(20 * time.Hour), Tried: at}, nil
}

func (p *previewAgent) UpdateStatus(context.Context, bool) (agent.UpdateStatus, error) {
	m := &release.Manifest{Version: "v0.2.0", Commit: "def5678", Date: "2026-11-12T10:00:00Z",
		Notes: "Eski imajlar dağıtımdan sonra silinir.\nÇekirdeğin sağlık kontrolleri seyreldi: Docker'ın yükü azalır."}
	return agent.UpdateStatus{Running: "v0.1.0", Latest: m, Newer: true, Checked: time.Now().Add(-2 * time.Hour), Next: time.Now().Add(22 * time.Hour),
		Ready: &agent.UpdateReady{Version: "v0.2.0", At: time.Now().Add(-2 * time.Hour), Plan: update.Plan{
			Steps: []update.PlanStep{{Name: "core-stack", Title: "Çekirdek container'lar", Change: true, Detail: "Çekirdek container'ları (Caddy, MongoDB, cloudflared) güncel ayarlarla çalıştır"},
				{Name: "panel-binary", Title: "FootOps programı", Change: true, Detail: "footops programını /usr/local/bin/footops konumuna kur"}},
			Impact: []string{"Caddy ve/veya MongoDB yeniden oluşturulur: siteler birkaç saniye (en çok ~10 sn) erişilemez, veritabanlı siteler veritabanına birkaç saniye ulaşamaz.",
				"Panel birkaç saniye yeniden başlar; siteler etkilenmez."}}},
		Run: &update.Run{From: "v0.1.0", To: "v0.1.1", Started: time.Now().Add(-26 * time.Hour), Finished: time.Now().Add(-26*time.Hour + 95*time.Second),
			Phase: update.PhaseRolledBack, Err: "v0.1.1 90 sn içinde açılmadı (panel (footops-web) çalışmıyor)"},
		Log:  []string{"== 2026-11-09 10:05:12 · footops v0.1.0 → v0.1.1", "[1/19] packages: already done", "[17/19] panel-binary: ok (412ms)", "[19/19] panel-services: ok (2.1s)", "✗ v0.1.1 90 sn içinde açılmadı (panel (footops-web) çalışmıyor)", "Önceki sürüme dönülüyor (v0.1.0)…", "✓ önceki sürüm (v0.1.0) yeniden çalışıyor"},
		Prev: "v0.0.9"}, nil
}
