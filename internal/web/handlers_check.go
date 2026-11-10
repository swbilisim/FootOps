package web

import (
	"net/http"
	"sort"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/doctor"
)

// The server check page (Faz U1, panel-tasarim §31.1): the last `footops doctor`
// report by section, and a button that runs it now.

// checkSection is a section of the report as the page shows it.
type checkSection struct {
	Title string
	Items []checkItem // what is not fine, most serious first
	OK    []string
	// Fold: the many fine steps of the standard are folded away.
	Fold bool
}

// checkItem is a finding; New marks a problem seen once so far (no alert yet).
type checkItem struct {
	doctor.Finding
	New bool
}

func checkSections(st agent.DoctorStatus) []checkSection {
	if st.Report == nil {
		return nil
	}
	rank := map[string]int{doctor.Bad: 0, doctor.Warn: 1, doctor.Info: 2}
	var out []checkSection
	for _, sec := range doctor.Sections {
		cs := checkSection{Title: sec.Title}
		for _, f := range st.Report.Findings {
			if f.Section != sec.ID {
				continue
			}
			if f.Level == doctor.OK {
				cs.OK = append(cs.OK, f.Title)
				continue
			}
			cs.Items = append(cs.Items, checkItem{Finding: f, New: f.Problem() && !st.Confirmed[f.ID]})
		}
		if len(cs.Items) == 0 && len(cs.OK) == 0 {
			continue
		}
		sort.SliceStable(cs.Items, func(i, j int) bool { return rank[cs.Items[i].Level] < rank[cs.Items[j].Level] })
		cs.Fold = sec.ID == "standard" && len(cs.OK) > 3
		out = append(out, cs)
	}
	return out
}

func (s *Server) checkPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.DoctorStatus(r.Context())
	data := map[string]any{"Title": "Sunucu kontrolü", "Nav": "server", "St": st, "Err": errString(err), "Sections": checkSections(st)}
	if st.Running {
		data["Live"], data["Refresh"] = 3, 5
	}
	s.render(w, r, "check", data)
}

// checkRun starts a check; the page then follows it.
func (s *Server) checkRun(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.DoctorRun(r.Context(), s.actor(r)); err != nil && agentStatus(err) != http.StatusConflict {
		s.sessions.setFlash(current(r).ID, "Kontrol başlatılamadı: "+err.Error())
	}
	http.Redirect(w, r, "/server/check", http.StatusSeeOther)
}

// checkSummary is the dashboard's card.
type checkSummary struct {
	Known     bool // a report exists
	Running   bool
	Bad, Warn int // problems that are alerts
	New       int // problems seen once so far
	Time      time.Time
	Failing   bool // the last run failed
}

func checkSummaryOf(st agent.DoctorStatus) checkSummary {
	cs := checkSummary{Running: st.Running, Failing: st.Err != ""}
	if st.Report == nil {
		return cs
	}
	cs.Known, cs.Time = true, st.Report.Time
	for _, f := range st.Report.Problems() {
		switch {
		case !st.Confirmed[f.ID]:
			cs.New++
		case f.Level == doctor.Bad:
			cs.Bad++
		default:
			cs.Warn++
		}
	}
	return cs
}

// imagesPrune starts removing the images no container uses (Faz U4); the job's page
// follows it.
func (s *Server) imagesPrune(w http.ResponseWriter, r *http.Request) {
	id, err := s.agent.PruneImages(r.Context(), s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "İmajlar silinemedi: "+err.Error())
		http.Redirect(w, r, "/server/check", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
