package web

import (
	"net/http"
	"regexp"

	"github.com/swbilisim/FootOps/internal/agent"
)

// Removed sites (Faz D "Kaldırılan siteyi kalıcı sil"): what removing a site kept — its
// folder under /srv/<group>/_removed/ and its database — and removing it for good,
// confirmed with the site's name.

var reRemovedID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}-[0-9]{8}-[0-9]{6}$`)

func (s *Server) removedPage(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.Removed(r.Context(), true)
	s.render(w, r, "removed", map[string]any{"Title": "Kaldırılan siteler", "List": list, "Err": errString(err)})
}

// removedOf finds a removed site by the path's id, with its sizes.
func (s *Server) removedOf(w http.ResponseWriter, r *http.Request) (agent.RemovedSite, bool) {
	id := r.PathValue("id")
	if !reRemovedID.MatchString(id) {
		http.NotFound(w, r)
		return agent.RemovedSite{}, false
	}
	list, err := s.agent.Removed(r.Context(), true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return agent.RemovedSite{}, false
	}
	for _, x := range list {
		if x.ID == id {
			return x, true
		}
	}
	s.sessions.setFlash(current(r).ID, "Bu kaldırılan site artık yok (kalıcı silinmiş olabilir).")
	http.Redirect(w, r, "/removed", http.StatusSeeOther)
	return agent.RemovedSite{}, false
}

func (s *Server) removedConfirm(w http.ResponseWriter, r *http.Request) {
	x, ok := s.removedOf(w, r)
	if !ok {
		return
	}
	s.render(w, r, "removed_confirm", map[string]any{"Title": "Kalıcı sil · " + x.Site, "R": x, "KeepDays": keepDays()})
}

func (s *Server) removedPurge(w http.ResponseWriter, r *http.Request) {
	x, ok := s.removedOf(w, r)
	if !ok {
		return
	}
	confirm := r.PostFormValue("confirm")
	problem := ""
	if confirm != x.Site {
		problem = "Onay için site adını aynen yaz."
	} else if id, err := s.agent.PurgeRemoved(r.Context(), x.ID, confirm, s.actor(r)); err != nil {
		problem = err.Error()
	} else {
		http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
		return
	}
	s.renderStatus(w, r, http.StatusUnprocessableEntity, "removed_confirm", map[string]any{"Title": "Kalıcı sil · " + x.Site, "R": x,
		"KeepDays": keepDays(), "Problem": problem})
}

func keepDays() int { return int(agent.SnapshotKeep.Hours() / 24) }
