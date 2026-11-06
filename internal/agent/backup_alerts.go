package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Backup alerts (§27.8) join the panel's alert list: the backups not set up or paused,
// parts that failed or are overdue, the repository past its size threshold or failing
// its weekly check. One alert of each kind names the sites, so a repository that cannot
// be reached does not flood the list; each clears itself once the next backup puts it
// right.
const (
	// staleAfter: a part with no successful backup for this long is overdue — the night
	// run is daily, so one missed night plus a little. Files backed up weekly get a week.
	staleAfter  = 26 * time.Hour
	staleWeekly = 7*24*time.Hour + 2*time.Hour
	// alertNames is how many parts an alert's title names before "and N more".
	alertNames = 3
)

// partOrder sorts a site's parts as a run takes them.
var partOrder = map[string]string{PartDB: "1", PartFiles: "2", PartDef: "3", PartServer: "4"}

// Alerts lists what is wrong with the backups at now. The Alerter asks for them with
// every sample (about 60 µs: a few small files read, no restic, no network).
func (b *Backups) Alerts(now time.Time) []Alert {
	b.init()
	alert := func(key, level, title, detail string, since time.Time) Alert {
		if since.IsZero() || since.After(now) {
			since = now
		}
		return Alert{Key: "backup:" + key, Level: level, Title: title, Detail: detail, Link: "/backup", Since: since}
	}
	// A file that cannot be read for a moment does not resolve what was wrong: the last
	// alerts stand, with the read error beside them (no Since: the Alerter's hold time
	// lets a passing one go unseen).
	unreadable := func(key, title string, err error) []Alert {
		b.mu.Lock()
		out := append([]Alert(nil), b.lastAlerts...)
		b.mu.Unlock()
		return append(out, Alert{Key: "backup:" + key, Level: "warn", Title: title, Detail: err.Error(), Link: "/backup"})
	}
	s, err := b.Settings()
	if err != nil {
		return unreadable("settings", "Yedek ayarları okunamadı", err)
	}
	if _, err := b.secret(); errors.Is(err, errNoBackup) {
		return []Alert{alert("setup", "warn", "Uzak yedekleme kurulmadı",
			"Sunucu giderse sitelerin verisi de gider: Yedekleme sayfasından bir S3 deposu (ör. Cloudflare R2) bağla.", now)}
	} else if err != nil {
		return []Alert{alert("setup", "bad", "Yedek deposu ayarları okunamadı", err.Error(), now)}
	}
	if s.Confirmed.IsZero() {
		return []Alert{alert("setup", "warn", "Yedek deposunun şifresi onaylanmadı",
			"Şifreyi kaydettiğini onaylayana kadar yedek alınmaz.", now)}
	}
	st := b.state()
	var out []Alert
	if s.Paused {
		out = append(out, alert("paused", "warn", "Zamanlanmış yedekleme duraklatıldı",
			"Gece ve saatlik yedekler alınmıyor; Yedekleme sayfasından sürdür.", now))
	}

	// What should have a recent backup, and how recent.
	all, err := b.Data.Store.List()
	if err != nil {
		return unreadable("sites", "Yedeklenecek siteler okunamadı", err)
	}
	due := map[string]time.Duration{serverPart + "/" + PartServer: staleAfter}
	since := map[string]time.Time{}
	for _, spec := range all {
		pol := s.For(spec.Name)
		if pol.Off {
			continue
		}
		due[spec.Name+"/"+PartDef] = staleAfter
		if spec.Database {
			due[spec.Name+"/"+PartDB] = staleAfter
		}
		if spec.DataMount != "" && pol.Files != FilesOff {
			due[spec.Name+"/"+PartFiles] = staleAfter
			if pol.Files == FilesWeekly {
				due[spec.Name+"/"+PartFiles] = staleWeekly
			}
		}
		since[spec.Name] = spec.CreatedAt
	}
	// Sites in name order, each part in the order a run takes them, the server last.
	keys := make([]string, 0, len(due))
	for k := range due {
		keys = append(keys, k)
	}
	order := func(k string) string {
		site, part, _ := strings.Cut(k, "/")
		if site == serverPart {
			site = "\uffff"
		}
		return site + "/" + partOrder[part]
	}
	sort.Slice(keys, func(i, j int) bool { return order(keys[i]) < order(keys[j]) })

	var failed, warned, stale []string
	var failedAt, warnedAt, staleAt time.Time
	var failMsg, warnMsg string
	var failLast, warnLast time.Time
	earliest := func(t *time.Time, v time.Time) {
		if t.IsZero() || v.Before(*t) {
			*t = v
		}
	}
	for _, k := range keys {
		site, _, _ := strings.Cut(k, "/")
		// A site made again under the same name does not inherit the old one's failure.
		if f, ok := st.Failed[k]; ok && !f.Time.Before(since[site]) {
			if f.Warning {
				warned = append(warned, partLabel(k))
				earliest(&warnedAt, f.Time)
				if !f.Time.Before(warnLast) {
					warnLast, warnMsg = f.Time, f.Err
				}
			} else {
				failed = append(failed, partLabel(k))
				earliest(&failedAt, f.Time)
				if !f.Time.Before(failLast) {
					failLast, failMsg = f.Time, f.Err
				}
				continue // overdue too, sooner or later: one alert is enough
			}
		}
		if s.Paused {
			continue
		}
		ref := st.Last[k].Time
		for _, t := range []time.Time{since[site], s.Confirmed} {
			if t.After(ref) {
				ref = t // nothing is owed from before the site, or the backups, existed
			}
		}
		if now.Sub(ref) > due[k] {
			stale = append(stale, partLabel(k))
			earliest(&staleAt, ref.Add(due[k]))
		}
	}
	if rf := st.RunFailed; rf != nil {
		earliest(&failedAt, rf.Time)
		if !rf.Time.Before(failLast) {
			failLast, failMsg = rf.Time, rf.Err
		}
	}
	if len(failed) > 0 || st.RunFailed != nil {
		title := "Yedek alınamadı"
		if len(failed) > 0 {
			title += ": " + nameList(failed)
		}
		out = append(out, alert("failed", "bad", title, failMsg, failedAt))
	}
	if len(stale) > 0 {
		out = append(out, alert("stale", "bad", "Yedek gecikti: "+nameList(stale),
			"Son başarılı yedekleri beklenenden eski (gece yedeği her gün; haftalık dosyalar 7 günde bir).", staleAt))
	}
	if len(warned) > 0 {
		out = append(out, alert("partial", "warn", "Bazı dosyalar yedeklenemedi: "+nameList(warned), warnMsg, warnedAt))
	}
	switch {
	case st.ForgetFailed != nil:
		out = append(out, alert("maint", "warn", "Saklama kuralı uygulanamadı",
			st.ForgetFailed.Err+" — eski yedekler silinmiyor, depo büyüyor.", st.ForgetFailed.Time))
	case st.PruneFailed != nil:
		out = append(out, alert("maint", "warn", "Yedek deposu temizlenemedi",
			st.PruneFailed.Err+" — silinen yedeklerin yeri boşalmadı; sonraki gece yeniden denenir.", st.PruneFailed.Time))
	}
	if st.CheckFailed != "" {
		out = append(out, alert("check", "bad", "Yedek deposunun haftalık denetimi sorun buldu", st.CheckFailed, st.LastCheck))
	}
	if st.RepoSize >= s.Warn() {
		out = append(out, alert("size", "warn", fmt.Sprintf("Yedek deposu %s (uyarı eşiği %s)", humanBytes(st.RepoSize), humanBytes(s.Warn())),
			"Sağlayıcının ücretsiz katmanı bitince kullanım ücretlenir (Cloudflare R2: hesap başına 10 GB). Saklamayı kısalt ya da eşiği yükselt.",
			st.RepoSized))
	}
	b.mu.Lock()
	b.lastAlerts = out
	b.mu.Unlock()
	return out
}

// partLabel names a "site/part" key for an alert: "magaza (veritabanı)".
func partLabel(key string) string {
	site, part, _ := strings.Cut(key, "/")
	if site == serverPart {
		return partName(PartServer)
	}
	return site + " (" + partName(part) + ")"
}

// nameList joins the first few names, and counts the rest.
func nameList(names []string) string {
	if len(names) <= alertNames {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s ve %d tane daha", strings.Join(names[:alertNames], ", "), len(names)-alertNames)
}
