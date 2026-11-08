package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- end dates and parking (Faz P1b) ----

// ExpiryRequest gives a site an end date ("" : none); for an unpark, the one it comes
// back with. Was, if set, is the end date the change was made from: the change is
// refused if the site no longer has it (a second click).
type ExpiryRequest struct {
	Expires string  `json:"expires"`
	Was     *string `json:"was,omitempty"`
	Actor   Actor   `json:"actor"`
}

// setExpiry changes a site's end date at once (no job: only its record changes).
func (s *Server) setExpiry(w http.ResponseWriter, r *http.Request) {
	var req ExpiryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	err := s.Expiry.SetExpiry(r.PathValue("name"), req.Expires, req.Was, req.Actor)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"expires": req.Expires})
	case errors.Is(err, sites.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errSiteBusy), errors.Is(err, errExpiryChanged):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeProblems(w, err)
	}
}

func (s *Server) parkSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	// Parking a parked site again finishes what an earlier try left (a container still
	// running).
	if _, err := s.Sites.Store.Get(name); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	id, err := s.Expiry.StartPark(name, req.Actor, nil)
	s.jobResponse(w, id, err)
}

func (s *Server) unparkSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req ExpiryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, err := s.Sites.Store.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !spec.IsParked() {
		writeErr(w, http.StatusConflict, "site parkta değil")
		return
	}
	if err := UnparkExpiry(req.Expires, time.Now()); err != nil {
		writeProblems(w, err)
		return
	}
	id, err := s.Expiry.StartUnpark(name, req.Expires, req.Actor, nil)
	s.jobResponse(w, id, err)
}
