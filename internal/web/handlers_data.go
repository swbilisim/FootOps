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

// snapshotDownload streams a copy from the agent.
func (s *Server) snapshotDownload(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !reSnapshotID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.passDownload(w, r, func() (*http.Response, error) {
		return s.agent.DownloadSnapshot(r.Context(), st.Spec.Name, id, s.actor(r), r.Header)
	})
}

// passDownload streams a file from the agent. Range requests pass through, so a browser
// can resume a broken download; nothing is buffered or written to disk here. A refusal
// of the agent keeps its status (404, 409, 416, 422…).
func (s *Server) passDownload(w http.ResponseWriter, r *http.Request, open func() (*http.Response, error)) {
	if r.Method == http.MethodHead { // GET patterns also match HEAD; a HEAD would pull the whole file
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := open()
	if err != nil {
		var ae *agent.Error
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
			http.Error(w, ae.Message, ae.Status)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	relay(w, resp)
}

// relay passes an agent's download on to the browser. If the stream breaks on either
// side, the response ends without its last chunk, so the browser reports a failed
// download instead of keeping a truncated file as if it were whole.
func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadTimeout))
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
		"Content-Disposition", "Last-Modified", "X-Content-Sha256"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if _, err := io.Copy(w, resp.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
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
