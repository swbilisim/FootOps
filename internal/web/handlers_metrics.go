package web

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/history"
)

// liveEvery is how often (seconds) live pages update; the agent samples every 10 s.
const liveEvery = 10

// historyView is what the "history" template block shows.
type historyView struct {
	Title  string
	Ranges []rangeOption
	Charts []template.HTML
	Err    string
	Skip   bool // a light live refresh: charts are left out and stay as they are
}

// selectedRange reads ?range=, falling back to the last 24 hours.
func selectedRange(r *http.Request) string {
	rng, _ := history.RangeByName(r.URL.Query().Get("range"))
	return rng.Name
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	o, err := s.agent.Overview(r.Context())
	rng := selectedRange(r)
	hv := historyView{Title: "Geçmiş", Ranges: rangeOptions(rng), Skip: lightRefresh(r)}
	if !hv.Skip {
		h, herr := s.agent.History(r.Context(), rng, "")
		hv.Err = errString(herr)
		if herr == nil {
			hv.Charts = serverCharts(o.System, h)
		}
	}
	s.render(w, r, "dashboard", map[string]any{"Title": "Sunucu", "O": o, "Err": errString(err), "History": hv,
		"Live": liveEvery, "Refresh": 15})
}

func serverCharts(sys agent.SystemMetrics, h agent.History) []template.HTML {
	empty := "Henüz veri yok: geçmiş dakikada bir birikir."
	// The scale always reaches the total, so the chart shows how full the server is.
	mem := chartSpec{Title: "Sunucu bellek", Unit: unitBytes, From: h.From, To: h.To, Step: h.Step,
		YMax: niceMax(float64(sys.MemTotal), unitBytes), Limit: float64(sys.MemTotal), LimitTag: "toplam RAM",
		Empty: empty, Lines: []chartLine{{"RAM", memPts(h.Host)}}}
	if sys.SwapTotal > 0 {
		mem.Lines = append(mem.Lines, chartLine{"Swap", swapPts(h.Host)})
		mem.Note = "swap " + shortBytes(float64(sys.SwapTotal))
	}
	return []template.HTML{
		renderChart(chartSpec{Title: "Sunucu CPU", Unit: unitPercent, From: h.From, To: h.To, Step: h.Step,
			YMax: 100, Empty: empty, Lines: []chartLine{{"CPU", cpuPts(h.Host)}}}),
		renderChart(mem),
		renderChart(chartSpec{Title: "Container bellek", Unit: unitBytes, From: h.From, To: h.To, Step: h.Step,
			Note: "hangi container ne kadar kullanıyor", Empty: empty, Lines: containerLines(h, memPts)}),
		renderChart(chartSpec{Title: "Container CPU", Unit: unitPercent, From: h.From, To: h.To, Step: h.Step,
			Note: "%100 = bir çekirdek", Empty: empty, Lines: containerLines(h, cpuPts)}),
	}
}

// containerHistory renders one container's CPU and memory charts. limit is its memory
// limit in bytes, 0 if it has none.
func (s *Server) containerHistory(r *http.Request, name string, limit uint64) historyView {
	rng := selectedRange(r)
	hv := historyView{Title: "Geçmiş", Ranges: rangeOptions(rng), Skip: lightRefresh(r)}
	if hv.Skip {
		return hv
	}
	h, err := s.agent.History(r.Context(), rng, name)
	if err != nil {
		hv.Err = err.Error()
		return hv
	}
	empty := "Bu aralıkta veri yok (container çalışmıyordu ya da geçmiş henüz birikmedi)."
	mem := chartSpec{Title: "Bellek", Unit: unitBytes, From: h.From, To: h.To, Step: h.Step,
		Limit: float64(limit), Empty: empty, Lines: []chartLine{{name, memPts(h.Containers[name])}}}
	if limit > 0 {
		mem.Note = "sınır " + shortBytes(float64(limit)) + "; aşarsa container yeniden başlatılır"
	}
	hv.Charts = []template.HTML{
		renderChart(chartSpec{Title: "CPU", Unit: unitPercent, From: h.From, To: h.To, Step: h.Step,
			Note: "%100 = bir çekirdek", Empty: empty, Lines: []chartLine{{name, cpuPts(h.Containers[name])}}}),
		renderChart(mem),
	}
	return hv
}

func (s *Server) container(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	o, err := s.agent.Overview(r.Context())
	var c *agent.ContainerInfo
	for i := range o.Containers {
		if o.Containers[i].Name == name {
			c = &o.Containers[i]
		}
	}
	var limit uint64
	if c != nil && c.MemLimit > 0 && c.MemLimit < o.System.MemTotal {
		limit = c.MemLimit // without a limit Docker reports the host's memory
	}
	site := ""
	if list, err := s.agent.Sites(r.Context()); err == nil {
		for _, st := range list {
			if st.Spec.Container() == name {
				site = st.Spec.Name
			}
		}
	}
	data := map[string]any{"Title": name, "Name": name, "Site": site, "Err": errString(err),
		"History": s.containerHistory(r, name, limit), "Live": liveEvery, "Refresh": 15}
	if c != nil {
		data["C"] = c
	}
	s.render(w, r, "container", data)
}

// meter is a small usage bar; an SVG rect sized by attribute, since the CSP allows no
// inline styles.
func meter(p float64) template.HTML {
	p = max(0, min(p, 100))
	return template.HTML(fmt.Sprintf(`<svg class="meter" viewBox="0 0 100 6" preserveAspectRatio="none" aria-hidden="true"><rect width="%.1f" height="6" class="%s"/></svg>`,
		p, levelOf(p)))
}

func levelOf(p float64) string {
	switch {
	case p >= 90:
		return "bad"
	case p >= 75:
		return "warn"
	default:
		return "ok"
	}
}
