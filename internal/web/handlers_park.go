package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- end dates and parking (Faz P1b) ----

// termView is a site's end date as the pages show it.
type termView struct {
	Has   bool   // it has one
	Day   string // "15.12.2026"
	Left  int    // days left: 0 on the day, negative once passed
	Class string // badge: "" (more than a week), "warn" (a week or less), "bad" (the day or past)
	Text  string // "36 gün kaldı", "bugün doluyor", "2 gün önce doldu"
}

func termOf(s sites.Spec) termView {
	left, ok := sites.DaysLeft(s.Expires, time.Now())
	if !ok {
		return termView{}
	}
	v := termView{Has: true, Day: dayTR(s.Expires), Left: left}
	switch {
	case left > 0:
		v.Text = fmt.Sprintf("%d gün kaldı", left)
	case left == 0:
		v.Text = "bugün doluyor"
	default:
		v.Text = fmt.Sprintf("%d gün önce doldu", -left)
	}
	switch {
	case left <= 0:
		v.Class = "bad"
	case left <= 7:
		v.Class = "warn"
	}
	return v
}

// dayTR writes an end date ("2026-12-31") as "31.12.2026".
func dayTR(day string) string {
	t, err := time.Parse(sites.DayLayout, day)
	if err != nil {
		return day
	}
	return t.Format("02.01.2006")
}

// siteExpiry changes a site's end date: to a date, a month or a year on, or none —
// from the end date the page showed (was): the agent refuses if it has changed since,
// so a second click does not extend twice.
func (s *Server) siteExpiry(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	cur := st.Spec.Expires
	if _, ok := r.PostForm["was"]; ok {
		cur = r.PostFormValue("was")
	}
	var to string
	switch r.PostFormValue("act") {
	case "set":
		to = strings.TrimSpace(r.PostFormValue("expires"))
		if to == "" {
			s.sessions.setFlash(current(r).ID, "Bir tarih seç (ya da “Süresiz yap”a bas).")
			http.Redirect(w, r, "/sites/"+st.Spec.Name+"#sure", http.StatusSeeOther)
			return
		}
	case "month":
		to = sites.Extend(cur, 1, s.now())
	case "year":
		to = sites.Extend(cur, 12, s.now())
	case "none":
	default:
		http.Error(w, "geçersiz istek", http.StatusBadRequest)
		return
	}
	msg := "Site artık süresiz."
	if to != "" {
		msg = "Bitiş tarihi: " + dayTR(to) + "."
	}
	if err := s.agent.SetExpiry(r.Context(), st.Spec.Name, to, &cur, s.actor(r)); err != nil {
		msg = "Bitiş tarihi değiştirilemedi: " + strings.Join(problemsOf(err), "; ")
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/sites/"+st.Spec.Name+"#sure", http.StatusSeeOther)
}

func (s *Server) sitePark(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	id, err := s.agent.ParkSite(r.Context(), st.Spec.Name, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, err.Error())
		http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// unparkChoices are the end dates a parked site can come back with.
type unparkChoice struct {
	Value, Label string
}

func (s *Server) unparkChoices(sp sites.Spec) []unparkChoice {
	now := s.now()
	month, year := sites.Extend(sp.Expires, 1, now), sites.Extend(sp.Expires, 12, now)
	return []unparkChoice{
		{"year", "+1 yıl → " + dayTR(year)},
		{"month", "+1 ay → " + dayTR(month)},
		{"set", "Başka bir tarih"},
		{"none", "Süresiz"},
	}
}

func (s *Server) siteUnparkForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	s.render(w, r, "site_unpark", map[string]any{"Title": st.Spec.Name + " yayına al", "S": st,
		"Choices": s.unparkChoices(st.Spec), "Choice": "year"})
}

func (s *Server) siteUnpark(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	choice := r.PostFormValue("choice")
	again := func(problems ...string) {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_unpark", map[string]any{"Title": st.Spec.Name + " yayına al",
			"S": st, "Choices": s.unparkChoices(st.Spec), "Choice": choice, "Date": r.PostFormValue("expires"), "Problems": problems})
	}
	var to string
	switch choice {
	case "year":
		to = sites.Extend(st.Spec.Expires, 12, s.now())
	case "month":
		to = sites.Extend(st.Spec.Expires, 1, s.now())
	case "set":
		to = strings.TrimSpace(r.PostFormValue("expires"))
		if to == "" {
			again("Bir tarih seç.")
			return
		}
	case "none":
	default:
		again("Yeni süreyi seç.")
		return
	}
	id, err := s.agent.UnparkSite(r.Context(), st.Spec.Name, to, s.actor(r))
	if err != nil {
		again(problemsOf(err)...)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
