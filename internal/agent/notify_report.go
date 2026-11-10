package agent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/version"
)

// ServerReport is what /durum is made of (Faz P1a): the latest sample, the sites, the
// backups and the open alerts — all of it at hand already, nothing is measured anew.
type ServerReport struct {
	Overview Overview
	Sites    []sites.Spec
	SitesErr string
	Backup   *BackupStatus // nil: no backups on this server
	Alerts   []Alert
	// Available is a newer FootOps release out (Faz U3), if any; AvailableReady: it can
	// be installed from the panel.
	Available      string
	AvailableReady bool
}

// NewReport puts the report together from the agent's parts (backups may be nil).
func NewReport(sampler *Sampler, list func() ([]sites.Spec, error), backups *Backups) func(ctx context.Context) ServerReport {
	return func(ctx context.Context) ServerReport {
		r := ServerReport{Overview: sampler.Latest(ctx)}
		if sampler.Alerts != nil {
			r.Alerts = sampler.Alerts.Active()
		}
		if specs, err := list(); err != nil {
			r.SitesErr = err.Error()
		} else {
			r.Sites = specs
		}
		if backups != nil {
			bs := backups.Status()
			r.Backup = &bs
		}
		return r
	}
}

// reportAlertsMax is how many open alerts the report names.
const reportAlertsMax = 8

// siteStateTR names a site's state (StatusSite.State) in the report.
var siteStateTR = map[string]string{
	"unhealthy":  "sağlıksız",
	"restarting": "yeniden başlıyor",
	"paused":     "yedek için duraklatılmış",
	"stopped":    "duruyor",
	"missing":    "container yok",
	"parked":     "parkta",
}

// renderReport is the /durum message (plain text, local time).
func renderReport(prefix string, r ServerReport, now time.Time) string {
	now = now.Local()
	var b strings.Builder
	fmt.Fprintf(&b, "🖥 %s Sunucu durumu · %s", prefix, now.Format("02.01.2006 15:04:05"))
	if o := r.Overview; o.SampledAt.IsZero() {
		b.WriteString("\nÖlçüm henüz yok (agent yeni açıldı).")
	} else {
		s := o.System
		fmt.Fprintf(&b, "\nİşlemci: %%%s · yük %s / %s / %s", trNum(s.CPUPercent, 0), trNum(s.Load1, 2), trNum(s.Load5, 2), trNum(s.Load15, 2))
		fmt.Fprintf(&b, "\nBellek: %s / %s (%%%s)", trSize(s.MemUsed), trSize(s.MemTotal), trPct(s.MemUsed, s.MemTotal))
		if s.SwapTotal > 0 {
			fmt.Fprintf(&b, " · takas %s / %s", trSize(s.SwapUsed), trSize(s.SwapTotal))
		} else {
			b.WriteString(" · takas yok")
		}
		fmt.Fprintf(&b, "\nDisk: %s / %s (%%%s)", trSize(s.DiskUsed), trSize(s.DiskTotal), trPct(s.DiskUsed, s.DiskTotal))
		if s.Uptime > 0 {
			b.WriteString("\nAçık kalma: " + spanTR(s.Uptime))
		}
	}
	b.WriteString("\n" + reportSites(r))
	b.WriteString("\n" + reportBackup(r.Backup, now))
	if len(r.Alerts) == 0 {
		b.WriteString("\nAçık uyarı yok ✓")
	} else {
		fmt.Fprintf(&b, "\nAçık uyarılar (%d):", len(r.Alerts))
		for i, a := range r.Alerts {
			if i == reportAlertsMax {
				fmt.Fprintf(&b, "\n… ve %d tane daha (panelde)", len(r.Alerts)-reportAlertsMax)
				break
			}
			mark := "🟡"
			if a.Level == "bad" {
				mark = "🔴"
			}
			b.WriteString("\n" + mark + " " + clipMsg(strings.ToValidUTF8(a.Title, "?"), 160))
		}
	}
	if errs := r.Overview.Errors; len(errs) > 0 {
		b.WriteString("\n⚠ Ölçülemeyen: " + clipMsg(strings.ToValidUTF8(strings.Join(errs, "; "), "?"), 300))
	}
	b.WriteString("\nFootOps " + version.Version)
	switch {
	case r.Available != "" && r.AvailableReady:
		b.WriteString(" · " + r.Available + " hazır (panel: Sunucu → Güncelleme)")
	case r.Available != "":
		b.WriteString(" · " + r.Available + " çıktı (panel: Sunucu → Güncelleme)")
	}
	return b.String()
}

func reportSites(r ServerReport) string {
	if r.SitesErr != "" {
		return "Siteler okunamadı: " + clipMsg(r.SitesErr, 200)
	}
	if len(r.Sites) == 0 {
		return "Site yok"
	}
	byName := map[string]ContainerInfo{}
	for _, c := range r.Overview.Containers {
		byName[c.Name] = c
	}
	running := 0
	other := map[string][]string{}
	for _, spec := range r.Sites {
		st := siteState(spec, byName)
		if st == "running" {
			running++
			continue
		}
		other[st] = append(other[st], spec.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Siteler: %d / %d çalışıyor", running, len(r.Sites))
	states := make([]string, 0, len(other))
	for st := range other {
		states = append(states, st)
	}
	sort.Strings(states)
	for _, st := range states {
		names := other[st]
		sort.Strings(names)
		label := siteStateTR[st]
		if label == "" {
			label = st
		}
		fmt.Fprintf(&b, "\n  %s: %s", label, clipMsg(strings.Join(names, ", "), 300))
	}
	return b.String()
}

func reportBackup(bs *BackupStatus, now time.Time) string {
	switch {
	case bs == nil:
		return "Yedek: bu sunucuda yedekleme yok"
	case bs.Rehearsal:
		return "Yedek: prova kipi (canlıya alınınca)"
	case !bs.Configured:
		return "Yedek: depo bağlı değil"
	case bs.Settings.Confirmed.IsZero():
		return "Yedek: kurulum tamamlanmadı (panelde onay bekliyor)"
	}
	var last time.Time
	for _, l := range bs.Last {
		if l.Time.After(last) {
			last = l.Time
		}
	}
	var parts []string
	if last.IsZero() {
		parts = append(parts, "henüz alınmadı")
	} else {
		parts = append(parts, "son "+shortTime(last, now))
	}
	if d, err := time.ParseInLocation("2006-01-02", bs.NightDone, now.Location()); err == nil {
		parts = append(parts, "son gece yedeği "+d.Format("02.01"))
	}
	if bs.Settings.Paused {
		parts = append(parts, "zamanlanmış yedekler duraklatıldı")
	}
	if bs.Running != nil {
		parts = append(parts, "şu an alınıyor")
	}
	return "Yedek: " + strings.Join(parts, " · ")
}

// shortTime is "15:04" for today, "02.01 15:04" otherwise (local time).
func shortTime(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

// trNum is v with prec decimals and a decimal comma.
func trNum(v float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', prec, 64), ".", ",", 1)
}

// trSize is a size in GB (one decimal) or MB, with a decimal comma.
func trSize(n uint64) string {
	if n >= 1<<30 {
		return trNum(float64(n)/(1<<30), 1) + " GB"
	}
	return trNum(float64(n)/(1<<20), 0) + " MB"
}

func trPct(used, total uint64) string {
	if total == 0 {
		return "0"
	}
	return trNum(100*float64(used)/float64(total), 0)
}
