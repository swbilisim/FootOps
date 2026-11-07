package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// ---- removed sites ----

// PurgeRequest removes a removed site for good; Confirm is the site's name.
type PurgeRequest struct {
	Confirm string `json:"confirm"`
	Actor   Actor  `json:"actor"`
}

// listRemoved lists the removed sites; ?detail=1 adds the sizes (walks the folders and
// asks MongoDB: for the page that shows them, not for every sites page).
func (s *Server) listRemoved(w http.ResponseWriter, r *http.Request) {
	list, err := s.Data.Removed(r.Context(), r.URL.Query().Get("detail") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []RemovedSite{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) purgeRemoved(w http.ResponseWriter, r *http.Request) {
	var req PurgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	id := r.PathValue("id")
	if !reRemovedID.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "geçersiz kayıt")
		return
	}
	rs, err := s.Data.FindRemoved(r.Context(), id)
	if errors.Is(err, errRemovedGone) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Confirm != rs.Site {
		writeErr(w, http.StatusUnprocessableEntity, "Onay için site adını aynen yaz.")
		return
	}
	var detail string
	jobID, err := s.Jobs.Start(JobSpec{Kind: "site.purge", Target: rs.Site, Actor: req.Actor}, func(ctx context.Context, t *Task) error {
		var err error
		detail, err = s.Data.Purge(ctx, t, rs, req.Actor)
		return err
	}, func(j Job) {
		d := rs.ID
		if detail != "" {
			d += ": " + detail
		}
		if j.Err != "" {
			d += " — hata: " + j.Err
		}
		s.recordJobDetail(j, d)
	})
	s.jobResponse(w, jobID, err)
}
