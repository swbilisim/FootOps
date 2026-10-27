package web

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

var reSnapshotID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

// downloadTimeout replaces the server's 60-second write timeout for downloads.
const downloadTimeout = 2 * time.Hour

func (s *Server) snapshotTake(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	id, err := s.agent.TakeSnapshot(r.Context(), st.Spec.Name, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Kopya başlatılamadı: "+err.Error())
		http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// snapshotDownload streams a copy from the agent. Range requests pass through, so a
// browser can resume a broken download; nothing is buffered or written to disk here.
func (s *Server) snapshotDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet { // GET patterns also match HEAD; a HEAD would pull the whole file
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !reSnapshotID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	resp, err := s.agent.DownloadSnapshot(r.Context(), st.Spec.Name, id, s.actor(r), r.Header)
	if err != nil {
		var ae *agent.Error
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusRequestedRangeNotSatisfiable) {
			http.Error(w, ae.Message, ae.Status)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadTimeout))
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Content-Disposition", "Last-Modified", "X-Content-Sha256"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *Server) jobCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reJobID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	msg := "İptal istendi; çalışan araç durduruluyor."
	if err := s.agent.CancelJob(r.Context(), id, s.actor(r)); err != nil {
		msg = "İptal edilemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
