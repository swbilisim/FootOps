package agent

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ---- updates (Faz U3) ----

// UpdateRequest applies a ready release (Version) or goes back (no version).
type UpdateRequest struct {
	Version string `json:"version,omitempty"`
	Actor   Actor  `json:"actor"`
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		writeErr(w, http.StatusNotFound, "güncelleme kapalı")
		return
	}
	writeJSON(w, http.StatusOK, s.Updater.Status(r.Context(), r.URL.Query().Get("brief") == "1"))
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	if s.Updater == nil {
		writeErr(w, http.StatusNotFound, "güncelleme kapalı")
		return
	}
	if !s.Updater.CheckNow() {
		writeErr(w, http.StatusConflict, "denetim zaten sürüyor")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	s.updateStart(w, r, false)
}

func (s *Server) updateBack(w http.ResponseWriter, r *http.Request) {
	s.updateStart(w, r, true)
}

func (s *Server) updateStart(w http.ResponseWriter, r *http.Request, back bool) {
	if s.Updater == nil {
		writeErr(w, http.StatusNotFound, "güncelleme kapalı")
		return
	}
	var req UpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	var id string
	var err error
	if back {
		id, err = s.Updater.Back(r.Context(), req.Actor)
	} else {
		id, err = s.Updater.Apply(r.Context(), req.Version, req.Actor)
	}
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, JobResponse{JobID: id})
	case errors.Is(err, ErrBusy), errors.Is(err, errUpdateBusy):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	}
}
