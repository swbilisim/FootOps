package agent

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Alert is a condition that needs the operator's attention.
type Alert struct {
	Key    string    `json:"key"`   // stable identity, e.g. "disk" or "restarts:web"
	Level  string    `json:"level"` // "warn" or "bad"
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Link   string    `json:"link,omitempty"` // panel page that shows more
	Since  time.Time `json:"since"`
}

// Thresholds. Short spikes are ignored: a condition must hold for alertHold before it
// becomes an alert, except for slow-moving ones (disk, certificates) that are checked
// on the same schedule anyway.
const (
	alertHold       = 30 * time.Second
	diskWarn        = 80.0
	diskBad         = 90.0
	memWarn         = 90.0
	memBad          = 95.0
	swapWarn        = 50.0
	swapBad         = 80.0
	cpuWarn         = 90.0
	cpuWindow       = 5 * time.Minute
	limitWarn       = 90.0 // container memory, percent of its limit
	restartWindow   = 15 * time.Minute
	restartWarn     = 2 // restarts within restartWindow
	certWarnDays    = 30
	certBadDays     = 7
	certEvery       = 10 * time.Minute // certificates change rarely; parsing them is the costly part
	restartKeepSpan = restartWindow + time.Minute
)

// Alerter evaluates the latest overview against the thresholds and remembers since when
// each alert has been active.
type Alerter struct {
	// Certs lists installed certificates; nil means certificates are not checked.
	Certs func() ([]sites.Cert, error)
	// OnChange, if set, is called (outside the lock) with alerts that opened or resolved.
	OnChange func(opened, resolved []Alert)

	mu       sync.Mutex
	pending  map[string]time.Time // condition first seen
	active   map[string]Alert
	cpu      []timedValue
	restarts map[string][]timedCount

	certs       []sites.Cert // last read, refreshed every certEvery
	certErr     error
	certsReadAt time.Time
}

type timedValue struct {
	t time.Time
	v float64
}

type timedCount struct {
	t time.Time
	n int
}

func NewAlerter() *Alerter {
	return &Alerter{pending: map[string]time.Time{}, active: map[string]Alert{}, restarts: map[string][]timedCount{}}
}

// Active returns the current alerts, most severe first.
func (a *Alerter) Active() []Alert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Alert, 0, len(a.active))
	for _, al := range a.active {
		out = append(out, al)
	}
	sortAlerts(out)
	return out
}

func sortAlerts(list []Alert) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Level != list[j].Level {
			return list[i].Level == "bad"
		}
		return list[i].Key < list[j].Key
	})
}

// Evaluate checks one overview taken at now.
func (a *Alerter) Evaluate(o Overview, now time.Time) {
	a.mu.Lock()
	refresh := a.Certs != nil && (a.certsReadAt.IsZero() || now.Sub(a.certsReadAt) >= certEvery || now.Before(a.certsReadAt))
	a.mu.Unlock()
	if refresh {
		certs, err := a.Certs()
		a.mu.Lock()
		a.certs, a.certErr, a.certsReadAt = certs, err, now
		a.mu.Unlock()
	}

	a.mu.Lock()
	found := a.conditions(o, a.certs, a.certErr, now)
	var opened, resolved []Alert
	seen := map[string]bool{}
	for _, c := range found {
		seen[c.Key] = true
		first, ok := a.pending[c.Key]
		if !ok {
			first = now
			a.pending[c.Key] = now
		}
		if prev, ok := a.active[c.Key]; ok {
			c.Since = prev.Since // keep the start, refresh level and text
			a.active[c.Key] = c
			continue
		}
		if c.Since.IsZero() && now.Sub(first) < alertHold {
			continue
		}
		c.Since = first
		a.active[c.Key] = c
		opened = append(opened, c)
	}
	for k := range a.pending {
		if !seen[k] {
			delete(a.pending, k)
		}
	}
	for k, al := range a.active {
		if !seen[k] {
			delete(a.active, k)
			resolved = append(resolved, al)
		}
	}
	notify := a.OnChange
	a.mu.Unlock()
	if notify != nil && (len(opened) > 0 || len(resolved) > 0) {
		sortAlerts(opened)
		sortAlerts(resolved)
		notify(opened, resolved)
	}
}

// conditions lists what is wrong right now. A non-zero Since marks a condition that
// needs no hold time (it does not flap). Callers hold a.mu.
func (a *Alerter) conditions(o Overview, certs []sites.Cert, certErr error, now time.Time) []Alert {
	var out []Alert
	stable := now // Since set: skip the hold
	add := func(al Alert) { out = append(out, al) }
	sys := o.System

	if sys.DiskTotal > 0 {
		p := pctOf(sys.DiskUsed, sys.DiskTotal)
		if p >= diskWarn {
			add(Alert{Key: "disk", Level: level(p, diskBad), Title: fmt.Sprintf("Disk %%%.0f dolu", p),
				Detail: "Eski imajlar, loglar ve yedekler yer kaplıyor olabilir.", Link: "/", Since: stable})
		}
	}
	if sys.MemTotal > 0 {
		if p := pctOf(sys.MemUsed, sys.MemTotal); p >= memWarn {
			add(Alert{Key: "memory", Level: level(p, memBad), Title: fmt.Sprintf("RAM %%%.0f dolu", p),
				Detail: "Container bellek grafiğinde hangi container'ın büyüdüğüne bak.", Link: "/"})
		}
	}
	if sys.SwapTotal > 0 {
		if p := pctOf(sys.SwapUsed, sys.SwapTotal); p >= swapWarn {
			add(Alert{Key: "swap", Level: level(p, swapBad), Title: fmt.Sprintf("Swap %%%.0f kullanımda", p),
				Detail: "RAM yetmiyor; sunucu yavaşlayabilir.", Link: "/"})
		}
	}

	// CPU: only a sustained average counts, not a spike.
	if sys.MemTotal > 0 {
		a.cpu = append(a.cpu, timedValue{now, sys.CPUPercent})
	}
	for len(a.cpu) > 0 && now.Sub(a.cpu[0].t) > cpuWindow {
		a.cpu = a.cpu[1:]
	}
	if len(a.cpu) > 0 && now.Sub(a.cpu[0].t) >= cpuWindow-30*time.Second {
		var sum float64
		for _, v := range a.cpu {
			sum += v.v
		}
		if avg := sum / float64(len(a.cpu)); avg >= cpuWarn {
			add(Alert{Key: "cpu", Level: "warn", Title: fmt.Sprintf("CPU 5 dakikadır %%%.0f", avg), Link: "/", Since: stable})
		}
	}

	present := map[string]bool{}
	for _, c := range o.Containers {
		present[c.Name] = true
		link := "/containers/" + c.Name
		// Only containers FootOps manages (compose projects) are expected to run.
		if c.Project != "" {
			switch {
			case c.State == "restarting":
				add(Alert{Key: "down:" + c.Name, Level: "bad", Title: c.Name + " sürekli yeniden başlıyor",
					Detail: "Uygulama açılırken çöküyor; loglara bak.", Link: link + "/logs"})
			case c.State != "running":
				add(Alert{Key: "down:" + c.Name, Level: "bad", Title: c.Name + " çalışmıyor", Detail: "Durum: " + c.State, Link: link})
			case c.Health == "unhealthy":
				add(Alert{Key: "health:" + c.Name, Level: "bad", Title: c.Name + " sağlıksız", Detail: "Sağlık kontrolü başarısız.", Link: link + "/logs"})
			}
		}
		if c.OOMKilled {
			add(Alert{Key: "oom:" + c.Name, Level: "bad", Title: c.Name + " belleği doldurdu (OOM)",
				Detail: "Bellek sınırına ulaştı ve kapatıldı; sınırı artırmayı ya da sızıntıyı düşün.", Link: link})
		}
		if c.State == "running" && c.MemLimit > 0 && (sys.MemTotal == 0 || c.MemLimit < sys.MemTotal) {
			if p := pctOf(c.MemUsage, c.MemLimit); p >= limitWarn {
				add(Alert{Key: "limit:" + c.Name, Level: "warn", Title: fmt.Sprintf("%s bellek sınırının %%%.0f'inde", c.Name, p),
					Detail: "Sınırı aşarsa container yeniden başlatılır.", Link: link})
			}
		}

		// Restarts: count increases within the window (a recreated container starts at 0).
		hist := append(a.restarts[c.Name], timedCount{now, c.RestartCount})
		for len(hist) > 0 && now.Sub(hist[0].t) > restartKeepSpan {
			hist = hist[1:]
		}
		a.restarts[c.Name] = hist
		n := 0
		for i := 1; i < len(hist); i++ {
			if d := hist[i].n - hist[i-1].n; d > 0 && now.Sub(hist[i].t) <= restartWindow {
				n += d
			}
		}
		if n >= restartWarn && c.State != "restarting" {
			add(Alert{Key: "restarts:" + c.Name, Level: "warn", Title: fmt.Sprintf("%s son 15 dakikada %d kez yeniden başladı", c.Name, n),
				Link: link + "/logs", Since: stable})
		}
	}
	for name := range a.restarts {
		if !present[name] {
			delete(a.restarts, name)
		}
	}

	if certErr != nil {
		add(Alert{Key: "certs", Level: "warn", Title: "Sertifikalar okunamadı", Detail: certErr.Error(), Link: "/certs", Since: stable})
	}
	for _, c := range certs {
		days := int(c.NotAfter.Sub(now).Hours() / 24)
		name := c.ID
		if len(c.Names) > 0 {
			name = c.Names[len(c.Names)-1]
		}
		switch {
		case !now.Before(c.NotAfter):
			add(Alert{Key: "cert:" + c.ID, Level: "bad", Title: name + " sertifikasının süresi doldu", Link: "/certs", Since: stable})
		case days < certWarnDays:
			lv := "warn"
			if days < certBadDays {
				lv = "bad"
			}
			add(Alert{Key: "cert:" + c.ID, Level: lv, Title: fmt.Sprintf("%s sertifikasının bitmesine %d gün var", name, days),
				Detail: "Cloudflare'de yeni Origin sertifikası oluşturup Sertifikalar sayfasından yükle.", Link: "/certs", Since: stable})
		}
	}
	return out
}

func pctOf(used, total uint64) float64 { return float64(used) / float64(total) * 100 }

func level(p, bad float64) string {
	if p >= bad {
		return "bad"
	}
	return "warn"
}
