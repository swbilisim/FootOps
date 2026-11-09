package mac

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"
)

// The Terminal's view of durum and yokla (FootOps.app reads their --json instead).

var dot = map[string]string{green: "🟢", gray: "⚪", yellow: "🟡", red: "🔴"}

var overall = map[string]string{green: "hepsi yolunda", gray: "bilgi yok", yellow: "bakılmalı", red: "sorun var"}

func printStatus(w io.Writer, st Status) {
	now := st.Time
	fmt.Fprintf(w, "FootOps · %s · %s %s\n", now.Format("02.01.2006 15:04"), dot[st.Color], overall[st.Color])
	for _, s := range st.Servers {
		fmt.Fprintf(w, "\n%s %s", dot[s.Color], s.Name)
		if f := s.File; f != nil {
			fmt.Fprintf(w, " · %d / %d site çalışıyor", s.Running, s.Total)
			switch last := f.Backup.LastOK; {
			case !f.Backup.Ready:
				fmt.Fprint(w, " · yedek hazır değil")
			case last.IsZero():
				fmt.Fprint(w, " · yedek henüz yok")
			default:
				fmt.Fprintf(w, " · son yedek %s", clock(last, now))
			}
			fmt.Fprintf(w, " · FootOps %s", clean(f.FootOps))
		}
		fmt.Fprintln(w)
		if f := s.File; f != nil {
			if y := f.Sys; y != nil {
				fmt.Fprintf(w, "   İşlemci %%%s · yük %s / %s / %s · bellek %s / %s (%%%d)", trNum(y.CPUPercent, 0),
					trNum(y.Load1, 2), trNum(y.Load5, 2), trNum(y.Load15, 2), sizeTR(y.MemUsed), sizeTR(y.MemTotal), percent(y.MemUsed, y.MemTotal))
				if y.SwapTotal > 0 {
					fmt.Fprintf(w, " · takas %s", sizeTR(y.SwapUsed))
				}
				fmt.Fprintf(w, " · disk %s / %s (%%%d)", sizeTR(y.DiskUsed), sizeTR(y.DiskTotal), percent(y.DiskUsed, y.DiskTotal))
				if y.Uptime > 0 {
					fmt.Fprintf(w, " · açık %s", spanTR(time.Duration(y.Uptime)*time.Second))
				}
				fmt.Fprintln(w)
			} else {
				fmt.Fprintln(w, "   Sistem bilgisi yok (sunucudaki FootOps bu sürümden eski)")
			}
		}
		var parts []string
		if s.File != nil {
			at := s.FileAt
			if at.IsZero() {
				at = s.File.Time
			}
			parts = append(parts, "durum dosyası "+ago(at, st.Time))
		}
		// A file that could not be read is among the problems below.
		if b := s.Watcher; b != nil {
			ok := 0
			for _, d := range b.Domains {
				if d.OK {
					ok++
				}
			}
			at := s.WatcherAt
			if at.IsZero() {
				at = b.Time
			}
			parts = append(parts, fmt.Sprintf("nöbetçi %s: %d / %d alan adı açık", ago(at, st.Time), ok, len(b.Domains)))
		}
		if len(parts) > 0 {
			fmt.Fprintf(w, "   %s\n", upperFirst(strings.Join(parts, " · ")))
		}
		if off := offSites(s); off != "" {
			fmt.Fprintf(w, "   %s\n", off)
		}
		for _, p := range s.Problems {
			fmt.Fprintf(w, "   %s %s\n", dot[p.Color], p.Text)
		}
	}
}

// offSites names the sites switched off on purpose (or frozen for a backup's moment),
// which do not colour their server.
func offSites(s ServerStatus) string {
	by := map[string][]string{}
	var order []string
	for _, site := range s.Sites {
		switch site.State {
		case "parked", "paused":
			if by[site.State] == nil {
				order = append(order, site.State)
			}
			by[site.State] = append(by[site.State], site.Name)
		}
	}
	var parts []string
	for _, st := range order {
		parts = append(parts, stateTR[st]+": "+strings.Join(by[st], ", "))
	}
	return upperFirst(strings.Join(parts, " · "))
}

var watcherTR = map[string]string{"ok": "✓ açık", "blind": "✓ açık (Cloudflare doğrulaması)", "failing": "✗ ulaşamadı",
	"down": "✗ kapalı", "unknown": "—"}

func printProbe(w io.Writer, r ProbeRun) {
	fmt.Fprintf(w, "Yoklama · %s · bu Mac'ten %d alan adı · %s\n", r.Time.Format("02.01.2006 15:04"), r.Total,
		secondsTR(time.Duration(r.TookMS)*time.Millisecond))
	for _, n := range r.Notes {
		fmt.Fprintf(w, "⚠ %s\n", n)
	}
	// Each server's rows aligned on their own; a verdict's line starts a new alignment.
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	server := ""
	for _, p := range r.Domains {
		if p.Server != server {
			tw.Flush()
			server = p.Server
			fmt.Fprintf(w, "\n%s\n", p.ServerName)
		}
		mark, what := "✓", p.Code+" · "+strconv.FormatInt(p.MS, 10)+" ms"
		if p.Cloudflare {
			what += " · CF"
		}
		if p.Note != "" {
			what += " · Cloudflare doğrulaması"
		}
		if !p.OK {
			mark, what = "✗", p.Reason
		}
		fmt.Fprintf(tw, "  %s %s\t%s\tnöbetçi: %s\n", mark, p.Domain, what, watcherTR[p.Watcher])
		if p.Verdict != "" {
			fmt.Fprintf(tw, "      → %s\n", p.Verdict)
		}
	}
	tw.Flush()
	if len(r.Skipped) > 0 {
		var parts []string
		for _, s := range r.Skipped {
			parts = append(parts, fmt.Sprintf("%s (%s, %s)", s.Site, s.ServerName, s.Label))
		}
		fmt.Fprintf(w, "\nAtlananlar: %s\n", strings.Join(parts, ", "))
	}
	if r.Total > 0 {
		fmt.Fprintf(w, "\nSonuç: %d / %d açık", r.Open, r.Total)
		if r.Slowest != "" {
			fmt.Fprintf(w, " · en yavaş %s (%s)", r.Slowest, secondsTR(time.Duration(r.SlowestMS)*time.Millisecond))
		}
		fmt.Fprintln(w)
	}
}

func printSettings(w io.Writer, v SettingsView) {
	fmt.Fprintf(w, "Ayarlar: %s\n", v.Path)
	if len(v.Servers) == 0 {
		fmt.Fprintln(w, "Bilinen sunucu yok.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nSUNUCU\tPANEL\tSSH\tKOVA")
	for _, s := range v.Servers {
		bucket := s.Bucket
		if bucket == "" {
			bucket = s.DefaultBucket + " (varsayılan)"
		}
		code := s.Code
		if !s.Keychain {
			code += " (yalnız ayarlarda)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", code, orDash(s.Panel), orDash(s.SSH), bucket)
	}
	tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// trNum is v with prec decimals and a decimal comma.
func trNum(v float64, prec int) string {
	return strings.Replace(strconv.FormatFloat(v, 'f', prec, 64), ".", ",", 1)
}

// sizeTR is a size in GB (one decimal) or MB.
func sizeTR(n uint64) string {
	if n >= 1<<30 {
		return trNum(float64(n)/(1<<30), 1) + " GB"
	}
	return trNum(float64(n)/(1<<20), 0) + " MB"
}

// bytesTR is a size in B, KB, MB or GB (of 1024; a decimal comma): a backup's parts
// are often a few hundred KB.
func bytesTR(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1<<20:
		return trNum(float64(n)/(1<<10), 0) + " KB"
	case n < 10<<20:
		return trNum(float64(n)/(1<<20), 1) + " MB"
	case n < 1<<30:
		return trNum(float64(n)/(1<<20), 0) + " MB"
	}
	return trNum(float64(n)/(1<<30), 1) + " GB"
}

func percent(used, total uint64) int {
	if total == 0 {
		return 0
	}
	return int(math.Round(100 * float64(used) / float64(total)))
}

// limitTR is a time limit as the watcher says it: "10 sn" (or "300 ms").
func limitTR(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	}
	return strconv.FormatInt(int64(d.Round(time.Second)/time.Second), 10) + " sn"
}

func secondsTR(d time.Duration) string {
	if d < time.Second {
		return strconv.FormatInt(d.Milliseconds(), 10) + " ms"
	}
	return trNum(d.Seconds(), 1) + " sn"
}

// spanTR is a duration in Turkish: "3 dk", "2 sa 5 dk", "3 gün 4 sa" (the agent's).
func spanTR(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%d dk", max(m, 1))
	case m < 24*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d sa", m/60)
		}
		return fmt.Sprintf("%d sa %d dk", m/60, m%60)
	}
	h := m / 60
	if h%24 == 0 {
		return fmt.Sprintf("%d gün", h/24)
	}
	return fmt.Sprintf("%d gün %d sa", h/24, h%24)
}

// ago is "az önce" or "3 dk önce".
func ago(t, now time.Time) string {
	if now.Sub(t) < time.Minute {
		return "az önce"
	}
	return spanTR(now.Sub(t)) + " önce"
}

// clock is "15:04" for today, "02.01 15:04" otherwise, in now's zone.
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("02.01 15:04")
}

// clean keeps text from a file free of control and format characters: nothing in a
// status file reaches the Terminal as an escape sequence, or turns a name around
// (U+202E) in the menu.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "?")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return '?'
		}
		return r
	}, s)
}

// clip keeps at most n bytes of a message (whole characters).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	// Turkish: i is İ.
	if r == 'i' {
		return "İ" + s[n:]
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
