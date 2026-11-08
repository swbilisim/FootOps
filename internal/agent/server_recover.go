package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ---- recovering a server from its backups (Faz Y5) ----

// RehearsalEndRequest ends the rehearsal (footops go-live).
type RehearsalEndRequest struct {
	Actor Actor `json:"actor"`
}

// rootOnly answers 403 unless root asks: a recovery is the installer's, going live
// go-live's, both run with sudo on the server.
func rootOnly(w http.ResponseWriter, r *http.Request) bool {
	if uid, _ := r.Context().Value(keyUID).(uint32); uid != 0 {
		writeErr(w, http.StatusForbidden, "yalnız sunucuda root (sudo footops install / go-live) yapabilir")
		return false
	}
	return true
}

func (s *Server) recoverStatus(w http.ResponseWriter, r *http.Request) {
	if s.Recovery == nil {
		writeJSON(w, http.StatusOK, RecoverStatus{})
		return
	}
	st := s.Recovery.Status()
	if j, ok := s.Jobs.Running(); ok && j.Kind == "server.recover" {
		st.Running = j.ID
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) recoverStart(w http.ResponseWriter, r *http.Request) {
	if !rootOnly(w, r) {
		return
	}
	if s.Recovery == nil {
		writeErr(w, http.StatusNotFound, "yedekten kurtarma bu sunucuda kapalı")
		return
	}
	var req RecoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if err := req.Check(); err != nil {
		var ve *validationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "kurtarma bilgilerinde sorunlar var", Problems: ve.problems})
			return
		}
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	label := repoLabel(req.Repository)
	if err := s.Recovery.MayRecover(req); err != nil {
		s.record(AuditEntry{Actor: req.Actor, Action: "server.recover", Target: label}, err)
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "server.recover", Target: label, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error { return s.Recovery.Recover(ctx, t, req) },
		func(j Job) { s.recordJobDetail(j, orDash(j.Err+j.Result)) })
	s.jobResponse(w, id, err)
}

func (s *Server) rehearsalEnd(w http.ResponseWriter, r *http.Request) {
	if !rootOnly(w, r) {
		return
	}
	var req RehearsalEndRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Rehearsal == nil || !s.Rehearsal.Active() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	err := s.Rehearsal.End(r.Context())
	s.record(AuditEntry{Actor: req.Actor, Action: "server.go-live", Target: "prova kipi bitti"}, err)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
