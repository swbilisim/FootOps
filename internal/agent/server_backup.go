package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- backups (Faz Y) ----

// BackupSetupRequest connects the server to a repository. KeyID, SecretKey and
// Password are secrets: they go to a root-only file and are never logged.
type BackupSetupRequest struct {
	BackupSetup
	Actor Actor `json:"actor"`
}

// BackupSettingsRequest replaces the schedule and policies.
type BackupSettingsRequest struct {
	Settings BackupSettings `json:"settings"`
	Actor    Actor          `json:"actor"`
}

// BackupRunRequest backs up now: one site, or (Site empty) every site and the server.
type BackupRunRequest struct {
	Site  string `json:"site,omitempty"`
	Actor Actor  `json:"actor"`
}

// BackupKey is the repository password.
type BackupKey struct {
	Password string `json:"password"`
}

func (s *Server) backupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Backups.Status())
}

func (s *Server) backupSetup(w http.ResponseWriter, r *http.Request) {
	var req BackupSetupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if err := req.Check(); err != nil {
		var ve *validationError
		if errors.As(err, &ve) {
			writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "yedek deposu bilgilerinde sorunlar var", Problems: ve.problems})
			return
		}
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	setup := req.BackupSetup
	label := repoLabel(setup.Repository)
	uid, _ := r.Context().Value(keyUID).(uint32)
	root := uid == 0
	if err := s.Backups.MayReplace(setup, root); err != nil {
		s.record(AuditEntry{Actor: req.Actor, Action: "backup.setup", Target: label}, err)
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "backup.setup", Target: label, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			outcome, err := s.Backups.Setup(ctx, t, setup, root)
			if err == nil {
				t.SetResult(outcome)
			}
			return err
		}, func(j Job) {
			detail := j.Err
			if j.Err == "" {
				detail = map[string]string{"created": "yeni depo oluşturuldu", "connected": "var olan depoya bağlandı"}[j.Result]
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

// backupKey returns the repository password: to root (footops backup show-key) at any
// time, to the panel only until the operator confirmed keeping it — the one time the
// panel shows it. Every read is audited.
func (s *Server) backupKey(w http.ResponseWriter, r *http.Request) {
	uid, _ := r.Context().Value(keyUID).(uint32)
	var actor Actor
	_ = json.Unmarshal([]byte(r.Header.Get(ActorHeader)), &actor)
	if uid == 0 {
		actor = Actor{User: "root", Via: "ssh"}
	}
	entry := AuditEntry{Actor: actor, Action: "backup.key", Target: "yedek deposu şifresi"}
	if uid != 0 {
		st, err := s.Backups.Settings()
		if err != nil || !st.Confirmed.IsZero() {
			err := errors.New("şifre onaylandıktan sonra yalnız sunucuda gösterilir: sudo footops backup show-key")
			s.record(entry, err)
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
	}
	pw, err := s.Backups.Key()
	s.record(entry, err)
	if errors.Is(err, errNoBackup) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, BackupKey{Password: pw})
}

func (s *Server) backupConfirm(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	err := s.Backups.Confirm()
	s.record(AuditEntry{Actor: req.Actor, Action: "backup.confirm", Target: "yedek deposu şifresi kaydedildi"}, err)
	if errors.Is(err, errNoBackup) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backupSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.Backups.Settings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) setBackupSettings(w http.ResponseWriter, r *http.Request) {
	var req BackupSettingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	err := s.Backups.SetSettings(req.Settings)
	s.record(AuditEntry{Actor: req.Actor, Action: "backup.settings", Target: "yedek ayarları"}, err)
	var ve *validationError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "yedek ayarlarında sorunlar var", Problems: ve.problems})
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) backupRun(w http.ResponseWriter, r *http.Request) {
	var req BackupRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	p, err := s.Backups.Plan(RunManual, req.Site, s.Backups.now())
	if errors.Is(err, sites.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.Backups.secret(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if st, err := s.Backups.Settings(); err == nil && st.Confirmed.IsZero() {
		writeErr(w, http.StatusConflict, errNotConfirmed.Error())
		return
	}
	id, err := s.Backups.Start(p, req.Actor)
	if err != nil && !errors.Is(err, ErrBusy) {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.jobResponse(w, id, err)
}

func (s *Server) backupRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeErr(w, http.StatusBadRequest, "limit 1-1000 arasında olmalı")
			return
		}
		limit = n
	}
	list, err := s.Backups.Runs(r.URL.Query().Get("site"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}
