package web

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/history"
)

// Charts are rendered on the server: the plot is inline SVG stretched to its box (lines
// keep their width with non-scaling strokes), axes and legend are HTML positioned by
// classes. No JavaScript and no inline styles, so the strict CSP stays as it is. Hovering
// a time column shows the values at that time (SVG <title>); the legend table carries
// the same numbers for anyone who does not hover.

const (
	unitPercent = "%"
	unitBytes   = "B"

	maxLines = 8 // categorical colors; more series are folded into "Diğer"
	plotW    = 1000.0
	plotH    = 100.0
)

type pt struct {
	T       int64
	V, Peak float64
}

type chartLine struct {
	Name string
	Pts  []pt
}

type chartSpec struct {
	Title    string
	Unit     string
	Lines    []chartLine // colors follow this order
	From, To time.Time
	Step     int64   // seconds per point
	YMax     float64 // fixed top of the scale (CPU 100%, RAM total); 0 fits the data
	Limit    float64 // reference line, e.g. a container's memory limit; 0 for none
	LimitTag string  // legend name of the reference line ("sınır" if empty)
	Note     string  // shown under the title
	Empty    string  // message when there are no points
}

func cpuPts(ps []history.Point) []pt {
	out := make([]pt, len(ps))
	for i, p := range ps {
		out[i] = pt{p.T, p.CPU, p.CPUMax}
	}
	return out
}

func memPts(ps []history.Point) []pt {
	out := make([]pt, len(ps))
	for i, p := range ps {
		out[i] = pt{p.T, float64(p.Mem), float64(p.MemMax)}
	}
	return out
}

func swapPts(ps []history.Point) []pt {
	out := make([]pt, len(ps))
	for i, p := range ps {
		out[i] = pt{p.T, float64(p.Swap), float64(p.Swap)}
	}
	return out
}

// containerLines picks the containers to draw: sorted by name so a container keeps its
// color across charts and reloads; past maxLines the smallest (by peak memory) are
// summed into "Diğer".
func containerLines(h agent.History, conv func([]history.Point) []pt) []chartLine {
	names := make([]string, 0, len(h.Containers))
	for n := range h.Containers {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) <= maxLines {
		out := make([]chartLine, len(names))
		for i, n := range names {
			out[i] = chartLine{n, conv(h.Containers[n])}
		}
		return out
	}
	peak := func(n string) uint64 {
		var m uint64
		for _, p := range h.Containers[n] {
			m = max(m, p.MemMax)
		}
		return m
	}
	byPeak := append([]string(nil), names...)
	sort.SliceStable(byPeak, func(i, j int) bool { return peak(byPeak[i]) > peak(byPeak[j]) })
	keep := map[string]bool{}
	for _, n := range byPeak[:maxLines-1] {
		keep[n] = true
	}
	var out []chartLine
	rest := map[int64]*pt{}
	for _, n := range names {
		pts := conv(h.Containers[n])
		if keep[n] {
			out = append(out, chartLine{n, pts})
			continue
		}
		for _, p := range pts {
			r := rest[p.T]
			if r == nil {
				r = &pt{T: p.T}
				rest[p.T] = r
			}
			r.V += p.V
			r.Peak += p.Peak
		}
	}
	other := chartLine{Name: fmt.Sprintf("Diğer (%d)", len(names)-len(out))}
	for _, p := range rest {
		other.Pts = append(other.Pts, *p)
	}
	sort.Slice(other.Pts, func(i, j int) bool { return other.Pts[i].T < other.Pts[j].T })
	return append(out, other)
}

// renderChart returns the chart's HTML. Every piece of text is escaped here, since the
// result bypasses the template's escaping.
func renderChart(c chartSpec) template.HTML {
	var b strings.Builder
	esc := html.EscapeString
	b.WriteString(`<figure class="chart">`)
	fmt.Fprintf(&b, `<figcaption><strong>%s</strong>`, esc(c.Title))
	if c.Note != "" {
		fmt.Fprintf(&b, ` <span class="muted small">%s</span>`, esc(c.Note))
	}
	b.WriteString(`</figcaption>`)

	var maxV float64
	n := 0
	for _, l := range c.Lines {
		for _, p := range l.Pts {
			maxV = max(maxV, p.V)
			n++
		}
	}
	if n == 0 {
		msg := c.Empty
		if msg == "" {
			msg = "Bu aralıkta veri yok."
		}
		fmt.Fprintf(&b, `<p class="chart-empty muted">%s</p></figure>`, esc(msg))
		return template.HTML(b.String())
	}

	top := c.YMax
	if top <= 0 {
		v := maxV
		if c.Limit > 0 && maxV >= c.Limit/2 {
			v = max(v, c.Limit) // close enough to the limit for it to matter: show it
		}
		top = niceMax(v, c.Unit)
	}
	from, to := c.From.Unix(), c.To.Unix()
	if to <= from {
		to = from + 1
	}
	x := func(t int64) float64 { return float64(t-from) / float64(to-from) * plotW }
	y := func(v float64) float64 { return plotH - math.Min(v/top, 1)*plotH }
	f := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

	b.WriteString(`<div class="plot"><div class="yaxis" aria-hidden="true">`)
	for i := 0; i <= 4; i++ {
		fmt.Fprintf(&b, `<span class="y%d">%s</span>`, i, esc(formatValue(top*float64(i)/4, c.Unit)))
	}
	fmt.Fprintf(&b, `</div><div class="canvas"><svg viewBox="0 0 %g %g" preserveAspectRatio="none" role="img" aria-label="%s">`,
		plotW, plotH, esc(c.Title))
	b.WriteString(`<g class="grid">`)
	for i := 0; i <= 4; i++ {
		yy := f(plotH * float64(i) / 4)
		fmt.Fprintf(&b, `<line x1="0" x2="%g" y1="%s" y2="%s" vector-effect="non-scaling-stroke"/>`, plotW, yy, yy)
	}
	b.WriteString(`</g>`)

	w := float64(c.Step) / float64(to-from) * plotW     // width of one time step
	px := func(t int64) string { return f(x(t) + w/2) } // points sit in the middle of their step
	for i, l := range c.Lines {
		var line, area strings.Builder
		for _, seg := range segments(l.Pts, c.Step) {
			first, last := seg[0], seg[len(seg)-1]
			if len(seg) == 1 { // a lone reading: a short dash, or it would be invisible
				fmt.Fprintf(&line, "M%s %sh%s", f(x(first.T)+w/4), f(y(first.V)), f(w/2))
				continue
			}
			fmt.Fprintf(&area, "M%s %g", px(first.T), plotH)
			for k, p := range seg {
				cmd := "L"
				if k == 0 {
					cmd = "M"
				}
				fmt.Fprintf(&line, "%s%s %s", cmd, px(p.T), f(y(p.V)))
				fmt.Fprintf(&area, "L%s %s", px(p.T), f(y(p.V)))
			}
			fmt.Fprintf(&area, "L%s %gZ", px(last.T), plotH)
		}
		slot := i + 1
		if len(c.Lines) == 1 && area.Len() > 0 {
			fmt.Fprintf(&b, `<path class="wash s%d" d="%s"/>`, slot, area.String())
		}
		fmt.Fprintf(&b, `<path class="ln s%d" d="%s" vector-effect="non-scaling-stroke"/>`, slot, line.String())
	}
	if c.Limit > 0 && c.Limit <= top {
		ly := f(y(c.Limit))
		fmt.Fprintf(&b, `<line class="limit" x1="0" x2="%g" y1="%s" y2="%s" vector-effect="non-scaling-stroke"/>`, plotW, ly, ly)
	}

	// Hover columns: one per time step, listing every series at that time.
	byT := map[int64][]string{}
	var times []int64
	for _, l := range c.Lines {
		for _, p := range l.Pts {
			if _, ok := byT[p.T]; !ok {
				times = append(times, p.T)
			}
			v := l.Name + ": " + formatValue(p.V, c.Unit)
			if len(c.Lines) == 1 && p.Peak > p.V {
				v += " (tepe " + formatValue(p.Peak, c.Unit) + ")"
			}
			byT[p.T] = append(byT[p.T], v)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	b.WriteString(`<g class="hit">`)
	for _, t := range times {
		fmt.Fprintf(&b, `<rect x="%s" width="%s" height="%g"><title>%s&#10;%s</title></rect>`,
			f(x(t)), f(w), plotH, esc(formatTime(t, c.To.Sub(c.From))), esc(strings.Join(byT[t], "\n")))
	}
	b.WriteString(`</g></svg><div class="xaxis" aria-hidden="true">`)
	for i := 0; i <= 4; i++ {
		t := from + (to-from)*int64(i)/4
		fmt.Fprintf(&b, `<span class="x%d">%s</span>`, i, esc(formatTime(t, c.To.Sub(c.From))))
	}
	b.WriteString(`</div></div></div>`)

	// Legend doubling as the table view: now, average and peak for each series.
	b.WriteString(`<table class="legend"><thead><tr><th></th><th>şimdi</th><th>ortalama</th><th>en yüksek</th></tr></thead><tbody>`)
	for i, l := range c.Lines {
		if len(l.Pts) == 0 {
			continue
		}
		var sum, peak float64
		for _, p := range l.Pts {
			sum += p.V
			peak = max(peak, p.Peak)
		}
		// "Now" only if the series is still reporting; a stopped container has no current value.
		now := "—"
		if last := l.Pts[len(l.Pts)-1]; last.T >= to-3*c.Step {
			now = formatValue(last.V, c.Unit)
		}
		fmt.Fprintf(&b, `<tr><td><span class="key s%d"></span>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			i+1, esc(l.Name), esc(now), esc(formatValue(sum/float64(len(l.Pts)), c.Unit)), esc(formatValue(peak, c.Unit)))
	}
	if c.Limit > 0 {
		tag := c.LimitTag
		if tag == "" {
			tag = "sınır"
		}
		fmt.Fprintf(&b, `<tr><td><span class="key limit"></span>%s</td><td colspan="3">%s</td></tr>`, esc(tag), esc(formatValue(c.Limit, c.Unit)))
	}
	b.WriteString(`</tbody></table></figure>`)
	return template.HTML(b.String())
}

// segments splits points where readings are missing (a stopped container, the agent
// down), so the line shows a gap instead of bridging it.
func segments(ps []pt, step int64) [][]pt {
	var out [][]pt
	for i, p := range ps {
		if i == 0 || p.T-ps[i-1].T > step {
			out = append(out, nil)
		}
		out[len(out)-1] = append(out[len(out)-1], p)
	}
	return out
}

// niceMax rounds v up to a scale top whose quarters are round numbers: 1-2-2.5-5 steps
// for percentages, powers of two (in MiB) for bytes.
func niceMax(v float64, unit string) float64 {
	if unit == unitBytes {
		tick := float64(1 << 20)
		for tick*4 < v {
			tick *= 2
		}
		return tick * 4
	}
	v = math.Max(v, 1)
	raw := v / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if m*mag >= raw {
			return m * mag * 4
		}
	}
	return v
}

func formatValue(v float64, unit string) string {
	if unit == unitBytes {
		return shortBytes(v)
	}
	return "%" + trimZeros(strconv.FormatFloat(v, 'f', decimals(v), 64))
}

func decimals(v float64) int {
	switch {
	case v >= 10:
		return 0
	case v >= 1:
		return 1
	default:
		return 2
	}
}

// shortBytes is the compact form for axes and legends: "512 MiB", "1.5 GiB".
func shortBytes(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v == 0 {
		return "0"
	}
	return trimZeros(strconv.FormatFloat(v, 'f', decimals(v), 64)) + " " + units[i]
}

func trimZeros(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func formatTime(unix int64, span time.Duration) string {
	t := time.Unix(unix, 0).Local()
	if span > 24*time.Hour {
		return t.Format("02.01 15:04")
	}
	return t.Format("15:04")
}

// rangeOption is one entry of the time-range selector.
type rangeOption struct {
	Name, Label string
	Selected    bool
}

var rangeLabels = map[string]string{"1h": "1 saat", "6h": "6 saat", "24h": "24 saat", "7d": "7 gün"}

func rangeOptions(selected string) []rangeOption {
	out := make([]rangeOption, len(history.Ranges))
	for i, r := range history.Ranges {
		out[i] = rangeOption{r.Name, rangeLabels[r.Name], r.Name == selected}
	}
	return out
}
