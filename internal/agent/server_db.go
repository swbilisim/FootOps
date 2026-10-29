package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/mongo"
	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- database browser (F5) ----

// dbEditBody is the body limit of edits: a document's text may be larger than its BSON.
const dbEditBody = 4 << 20

// dbErr answers with the status that fits a database browser error.
func dbErr(w http.ResponseWriter, err error) {
	var de *DBError
	var me *mongo.Error
	switch {
	case errors.As(err, &de):
		writeErr(w, http.StatusUnprocessableEntity, de.Msg)
	case errors.Is(err, ErrDBConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrDBNotFound), errors.Is(err, sites.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrDBBusy):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, ErrDBUnknown):
		writeErr(w, http.StatusGatewayTimeout, err.Error())
	case errors.As(err, &me):
		writeErr(w, http.StatusBadGateway, "MongoDB: "+me.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// dbTarget names a document in the audit log: site, collection and a short _id.
func dbTarget(site, coll, id string) string {
	return site + " " + mongo.Clip(coll, 40) + " " + mongo.Clip(id, 48)
}

// changeNames lists changed fields for the audit log: names, never values (K6).
func changeNames(cs []DBChange, more int) string {
	var parts []string
	for _, c := range cs {
		p := map[string]string{"added": "+", "removed": "−"}[c.Kind] + c.Field
		if c.OldType != "" && c.NewType != "" && c.OldType != c.NewType {
			p += " (" + c.OldType + "→" + c.NewType + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return "alan değişmedi"
	}
	if more > 0 {
		parts = append(parts, fmt.Sprintf("+%d alan daha", more))
	}
	return "alanlar: " + mongo.Clip(strings.Join(parts, ", "), 300)
}

func (s *Server) dbCollections(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	out, err := s.Data.DBCollections(r.Context(), spec.Name)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dbFind(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	per, _ := strconv.Atoi(q.Get("n"))
	out, err := s.Data.DBFind(r.Context(), spec.Name, DBQuery{Collection: q.Get("c"), Filter: q.Get("filter"),
		Sort: q.Get("sort"), Page: page, PerPage: per})
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dbDoc(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	q := r.URL.Query()
	out, err := s.Data.DBDoc(r.Context(), spec.Name, q.Get("c"), q.Get("id"))
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) dbPreview(w http.ResponseWriter, r *http.Request) {
	var req DBEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	out, err := s.Data.DBPreview(r.Context(), spec.Name, req)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// recordDB writes a database change to the audit log. On failure the detail says what
// kind of failure, never the error's text: MongoDB's messages quote values (rule K6).
func (s *Server) recordDB(e AuditEntry, rec DBEdit, err error, prefix string) {
	e.OK = err == nil
	if err != nil {
		e.Detail = prefix + auditFailure(err)
		if rec.Pending { // the change may have happened: its record is the way back
			e.Detail += " · kayıt " + rec.ID
		}
	}
	if aerr := s.Audit.Record(e); aerr != nil {
		s.logf("audit: %v", aerr)
	}
}

// dbChange runs an edit or a delete held against jobs on the site, and audits it.
func (s *Server) dbChange(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req DBEditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "geçersiz istek")
			return
		}
		spec, ok := s.dbSite(w, r.PathValue("name"))
		if !ok {
			return
		}
		release, ok := s.hold(w, spec.Name)
		if !ok {
			return
		}
		defer release()
		// A change is not cut off when the browser goes away: it finishes within its own
		// time limit (otherwise its record would be left pending for nothing).
		ctx := context.WithoutCancel(r.Context())
		var rec DBEdit
		var err error
		if op == "edit" {
			rec, err = s.Data.DBReplace(ctx, spec.Name, req)
		} else {
			rec, err = s.Data.DBDelete(ctx, spec.Name, req)
		}
		detail := "kayıt " + orDash(rec.ID)
		if op == "edit" {
			detail = changeNames(rec.Changes, rec.More) + " · " + detail
		}
		if rec.Pending && err == nil {
			detail += " (kayıt kesinleştirilemedi)"
		}
		s.recordDB(AuditEntry{Actor: req.Actor, Action: "db." + op, Target: dbTarget(spec.Name, req.Collection, req.ID), Detail: detail}, rec, err, "")
		if err != nil {
			dbErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func (s *Server) dbEdits(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	list, err := s.Data.DBEdits(spec.Name)
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) dbEdit(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	v, err := s.Data.DBEditView(spec.Name, r.PathValue("id"))
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) dbUndo(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	id := r.PathValue("id")
	rec, orig, err := s.Data.DBUndo(context.WithoutCancel(r.Context()), spec.Name, id, req.Actor)
	detail := fmt.Sprintf("%s geri alındı · kayıt %s", id, orDash(rec.ID))
	s.recordDB(AuditEntry{Actor: req.Actor, Action: "db.undo", Target: dbTarget(spec.Name, orig.Collection, orig.DocID), Detail: detail}, rec, err, id+" — ")
	if err != nil {
		dbErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}
