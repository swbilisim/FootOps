package web

import (
	"net/http"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
)

// The notifications page (Faz Y4, panel-tasarim §27.8): linking a Telegram chat with a
// bot token and a one-time code, a test message, and which alerts go there. The token
// passes through here only inside the request that carries it; the agent never gives
// it back.

// notifyRow is an active alert and whether the chat hears about it.
type notifyRow struct {
	agent.Alert
	Sent bool
}

func (s *Server) notifyPage(w http.ResponseWriter, r *http.Request) {
	s.renderNotify(w, r, http.StatusOK, nil)
}

func (s *Server) renderNotify(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	st, err := s.agent.NotifyStatus(r.Context())
	data := map[string]any{"Title": "Bildirimler", "St": st, "Err": errString(err)}
	list, aerr := s.agent.Alerts(r.Context())
	rows := make([]notifyRow, 0, len(list))
	for _, a := range list {
		rows = append(rows, notifyRow{Alert: a, Sent: agent.Notifiable(a)})
	}
	data["Rows"], data["AlertsErr"] = rows, errString(aerr)
	for k, v := range extra {
		data[k] = v
	}
	s.renderStatus(w, r, status, "notify", data)
}

// notifyStart checks the bot token (or, keep, the saved one, for another chat) and
// shows the code to send.
func (s *Server) notifyStart(w http.ResponseWriter, r *http.Request) {
	keep := r.PostFormValue("keep") == "1"
	token := strings.TrimSpace(r.PostFormValue("token"))
	if !keep && token == "" {
		s.renderNotify(w, r, http.StatusUnprocessableEntity, map[string]any{"Problems": []string{"Bot token'ını yapıştır."}})
		return
	}
	if _, err := s.agent.NotifyStart(r.Context(), token, keep, s.actor(r)); err != nil {
		// The form comes back empty: the token is never written into a page.
		s.renderNotify(w, r, agentStatus(err), map[string]any{"Problems": []string{err.Error()}})
		return
	}
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

// notifyLink finishes linking once the code reached the bot.
func (s *Server) notifyLink(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.NotifyLink(r.Context(), s.actor(r))
	if err != nil {
		s.renderNotify(w, r, agentStatus(err), map[string]any{"LinkErr": err.Error()})
		return
	}
	s.sessions.setFlash(current(r).ID, "Telegram bağlandı: @"+st.Bot+" → "+st.Chat+". Sohbete bir karşılama mesajı gönderildi; şu an açık uyarılar da birazdan gelir.")
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

func (s *Server) notifyCancel(w http.ResponseWriter, r *http.Request) {
	if err := s.agent.NotifyCancel(r.Context(), s.actor(r)); err != nil {
		s.sessions.setFlash(current(r).ID, "Vazgeçilemedi: "+err.Error())
	}
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

func (s *Server) notifyTest(w http.ResponseWriter, r *http.Request) {
	msg := "Deneme mesajı gönderildi; Telegram'da görünmesi gerekir."
	if err := s.agent.NotifyTest(r.Context(), s.actor(r)); err != nil {
		msg = "Deneme mesajı gönderilemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}

func (s *Server) notifyRemove(w http.ResponseWriter, r *http.Request) {
	msg := "Telegram bağlantısı kaldırıldı; bot token'ı bu sunucudan silindi. Uyarılar yalnız panelde görünür."
	if err := s.agent.NotifyRemove(r.Context(), s.actor(r)); err != nil {
		msg = "Kaldırılamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/notify", http.StatusSeeOther)
}
