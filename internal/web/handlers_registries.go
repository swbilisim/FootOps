package web

import (
	"net/http"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
)

// registryForm keeps what was typed when a login is refused; never the token.
type registryForm struct{ Host, User string }

func (s *Server) registries(w http.ResponseWriter, r *http.Request) {
	s.renderRegistries(w, r, http.StatusOK, registryForm{Host: "ghcr.io"}, nil)
}

func (s *Server) renderRegistries(w http.ResponseWriter, r *http.Request, status int, f registryForm, problems []string) {
	list, err := s.agent.Registries(r.Context())
	s.renderStatus(w, r, status, "registries", map[string]any{"Title": "İmaj girişleri", "List": list, "Err": errString(err),
		"F": f, "Problems": problems, "Now": s.now()})
}

// registryLogin hands the token to the agent; it is not kept here, not even to show the
// form again.
func (s *Server) registryLogin(w http.ResponseWriter, r *http.Request) {
	f := registryForm{Host: strings.TrimSpace(r.PostFormValue("host")), User: strings.TrimSpace(r.PostFormValue("user"))}
	res, err := s.agent.RegistryLogin(r.Context(), f.Host, f.User, r.PostFormValue("token"), s.actor(r))
	if err != nil {
		s.renderRegistries(w, r, http.StatusUnprocessableEntity, f, problemsOf(err))
		return
	}
	msg := "Giriş kaydedildi: " + res.Login.Host + " (" + res.Login.User + ")."
	if len(res.Notes) > 0 {
		msg += " " + strings.Join(res.Notes, " · ")
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/registries", http.StatusSeeOther)
}

// registryOf finds a saved login by the host in the request, or answers 404.
func (s *Server) registryOf(w http.ResponseWriter, r *http.Request, host string) (agent.RegistryLogin, bool) {
	list, err := s.agent.Registries(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return agent.RegistryLogin{}, false
	}
	for _, l := range list {
		if l.Host == host {
			return l, true
		}
	}
	http.NotFound(w, r)
	return agent.RegistryLogin{}, false
}

func (s *Server) registryRemoveConfirm(w http.ResponseWriter, r *http.Request) {
	l, ok := s.registryOf(w, r, r.URL.Query().Get("host"))
	if !ok {
		return
	}
	s.render(w, r, "registry_remove", map[string]any{"Title": "Girişi kaldır", "Host": l.Host})
}

func (s *Server) registryRemove(w http.ResponseWriter, r *http.Request) {
	l, ok := s.registryOf(w, r, r.PostFormValue("host"))
	if !ok {
		return
	}
	msg := l.Host + " girişi kaldırıldı."
	if err := s.agent.RegistryLogout(r.Context(), l.Host, s.actor(r)); err != nil {
		msg = "Kaldırılamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/registries", http.StatusSeeOther)
}
