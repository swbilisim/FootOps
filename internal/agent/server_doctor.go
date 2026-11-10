package agent

import (
	"net/http"
)

// ---- the server check (Faz U1) ----

func (s *Server) doctorStatus(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		writeErr(w, http.StatusNotFound, "sunucu kontrolü yok")
		return
	}
	writeJSON(w, http.StatusOK, s.Doctor.Status())
}

// doctorRun starts a check at once; the page follows it (it takes a few seconds).
func (s *Server) doctorRun(w http.ResponseWriter, r *http.Request) {
	if s.Doctor == nil {
		writeErr(w, http.StatusNotFound, "sunucu kontrolü yok")
		return
	}
	if !s.Doctor.RunNow() {
		writeErr(w, http.StatusConflict, "kontrol zaten sürüyor")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}
