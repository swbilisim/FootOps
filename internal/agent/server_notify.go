package agent

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ---- notifications (Faz Y4) ----

// NotifyLinkRequest starts linking a Telegram chat. Token is a secret: it is kept in
// memory until the link is made, then in a root-only file, and never logged. Keep
// links another chat with the saved token.
type NotifyLinkRequest struct {
	Token string `json:"token,omitempty"`
	Keep  bool   `json:"keep,omitempty"`
	Actor Actor  `json:"actor"`
}

func (s *Server) notifyStatus(w http.ResponseWriter, r *http.Request) {
	if s.Notify == nil {
		writeErr(w, http.StatusNotFound, "bildirimler bu sunucuda kapalı")
		return
	}
	st := s.Notify.Status()
	if s.StatusFile != nil {
		fs := s.StatusFile.State()
		st.StatusFile = &fs
	}
	writeJSON(w, http.StatusOK, st)
}

// notifyStart checks the bot token with Telegram and returns the code to send.
func (s *Server) notifyStart(w http.ResponseWriter, r *http.Request) {
	var req NotifyLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Notify == nil {
		writeErr(w, http.StatusNotFound, "bildirimler bu sunucuda kapalı")
		return
	}
	l, err := s.Notify.StartLink(r.Context(), req.Token, req.Keep)
	target := "Telegram"
	if err == nil {
		target = "Telegram @" + l.Bot
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "notify.start", Target: target}, err)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// notifyLink finishes a link once the code reached the bot.
func (s *Server) notifyLink(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Notify == nil {
		writeErr(w, http.StatusNotFound, "bildirimler bu sunucuda kapalı")
		return
	}
	st, err := s.Notify.FinishLink(r.Context(), req.Actor.User)
	if errors.Is(err, errCodeNotSeen) {
		// Not a failure worth a record: the operator just has not sent it yet.
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	target := "Telegram"
	if err == nil {
		target = "Telegram @" + st.Bot + " → " + st.Chat
		if c := st.Commands; c != nil && c.User != "" {
			target += " · komut: " + c.User
		}
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "notify.link", Target: target}, err)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) notifyCancel(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Notify != nil {
		s.Notify.CancelLink()
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) notifyTest(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Notify == nil {
		writeErr(w, http.StatusNotFound, "bildirimler bu sunucuda kapalı")
		return
	}
	err := s.Notify.Test(r.Context())
	s.record(AuditEntry{Actor: req.Actor, Action: "notify.test", Target: "Telegram"}, err)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) notifyRemove(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if s.Notify == nil {
		writeErr(w, http.StatusNotFound, "bildirimler bu sunucuda kapalı")
		return
	}
	target := "Telegram"
	if st := s.Notify.Status(); st.Linked {
		target = "Telegram @" + st.Bot + " → " + st.Chat
	}
	err := s.Notify.Remove()
	s.record(AuditEntry{Actor: req.Actor, Action: "notify.remove", Target: target}, err)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
