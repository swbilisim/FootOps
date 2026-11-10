package web

import (
	"net/http"
	"strings"
)

// The Güncelleme page (Faz U3, panel-tasarim §31.3): the running version, a newer one
// downloaded, checked and planned by the agent, the button that installs it, the last
// update's outcome and log, and going back to the kept previous version.

func (s *Server) updatePage(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.UpdateStatus(r.Context(), false)
	data := map[string]any{"Title": "Güncelleme", "Nav": "server", "St": st, "Err": errString(err)}
	if st.Applying || st.Checking {
		data["Live"], data["Refresh"] = 3, 5
	}
	s.render(w, r, "update", data)
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.UpdateCheck(r.Context(), s.actor(r)); err != nil && agentStatus(err) != http.StatusConflict {
		s.sessions.setFlash(current(r).ID, "Denetlenemedi: "+err.Error())
	}
	http.Redirect(w, r, "/server/update", http.StatusSeeOther)
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	v := strings.TrimSpace(r.PostFormValue("version"))
	msg := "Güncelleme başladı: panel birkaç saniye kapanıp " + v + " ile açılacak; bu sayfa kendini yeniler."
	if _, err := s.agent.UpdateApply(r.Context(), v, s.actor(r)); err != nil {
		msg = "Güncelleme başlatılamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/server/update", http.StatusSeeOther)
}

func (s *Server) updateBack(w http.ResponseWriter, r *http.Request) {
	msg := "Önceki sürüme dönülüyor: panel birkaç saniye kapanacak; bu sayfa kendini yeniler."
	if _, err := s.agent.UpdateBack(r.Context(), s.actor(r)); err != nil {
		msg = "Önceki sürüme dönülemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/server/update", http.StatusSeeOther)
}
