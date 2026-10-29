package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/mongo"
)

// The database browser (F5): a site's collections, pages of documents, one document,
// and single-document edits. Everything is a form or a link; the agent reads and
// writes as the site's own MongoDB user and keeps the previous version of what it
// changes, so every change can be undone for a week.

const (
	// dbEditForm is the body limit of the edit form: a document's text. The form is
	// multipart, so Turkish letters are not inflated threefold as in URL encoding.
	dbEditForm = 4 << 20
	// dbEditRead replaces the 15-second read timeout for that form.
	dbEditRead = 2 * time.Minute
)

// isDBEditPost reports the requests whose bodies carry a document.
func isDBEditPost(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sites/") && strings.HasSuffix(r.URL.Path, "/db/edit")
}

// dbURL is the address of a database browser page: dbURL("magaza", "/coll", "c", "users").
func dbURL(site, page string, kv ...string) string {
	u := "/sites/" + site + "/db" + page
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			v.Set(kv[i], kv[i+1])
		}
	}
	if len(v) > 0 {
		u += "?" + v.Encode()
	}
	return u
}

var reOIDText = regexp.MustCompile(`^\{"\$oid": "([0-9a-f]{24})"\}$`)

// idText shows an _id as the shell does: ObjectId("…") rather than {"$oid": "…"};
// a long one is shortened (links keep it whole).
func idText(id string) string {
	if m := reOIDText.FindStringSubmatch(id); m != nil {
		return `ObjectId("` + m[1] + `")`
	}
	return mongo.Clip(id, 96)
}

// dbParam reads a collection name or document id from a query or form: short, no
// control characters (the agent checks the rest).
func dbParam(v string, max int) (string, bool) {
	if v == "" || len(v) > max || strings.ContainsAny(v, "\x00\r") {
		return "", false
	}
	return v, true
}

// dbBack goes to a page with a message.
func (s *Server) dbBack(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		s.sessions.setFlash(current(r).ID, msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func agentStatus(err error) int {
	var ae *agent.Error
	if errors.As(err, &ae) && (ae.Status >= 400 && ae.Status < 500 || ae.Status == http.StatusServiceUnavailable) {
		return ae.Status
	}
	return http.StatusBadGateway
}

// unknown reports a write whose outcome the agent could not learn (504).
func unknown(err error) bool {
	var ae *agent.Error
	return errors.As(err, &ae) && ae.Status == http.StatusGatewayTimeout
}

// docGone sends a link to a missing document back to its collection with a message,
// rather than to a bare error page.
func (s *Server) docGone(w http.ResponseWriter, r *http.Request, site, coll string, err error) bool {
	if agentStatus(err) != http.StatusNotFound {
		return false
	}
	s.dbBack(w, r, dbURL(site, "/coll", "c", coll), "Belge bulunamadı (silinmiş olabilir).")
	return true
}

func (s *Server) dbPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	data := map[string]any{"Title": st.Spec.Name + " veritabanı", "S": st.Spec, "KeepDays": int(agent.DBEditKeep.Hours() / 24)}
	status := http.StatusOK
	if ov, err := s.agent.DBCollections(r.Context(), st.Spec.Name); err != nil {
		data["Err"], status = err.Error(), agentStatus(err)
	} else {
		data["O"] = ov
	}
	if edits, err := s.agent.DBEdits(r.Context(), st.Spec.Name); err != nil {
		data["EditsErr"] = err.Error()
	} else {
		data["Edits"] = edits
	}
	s.renderStatus(w, r, status, "db", data)
}

func (s *Server) dbColl(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	coll, ok := dbParam(q.Get("c"), 255)
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg, _ := strconv.Atoi(q.Get("page"))
	per, _ := strconv.Atoi(q.Get("n"))
	sort := q.Get("sort")
	if o := strings.TrimSpace(q.Get("osort")); o != "" { // the custom sort field wins
		sort = o
	}
	query := agent.DBQuery{Collection: coll, Filter: q.Get("filter"), Sort: sort, Page: max(pg, 1), PerPage: per}
	data := map[string]any{"Title": coll + " · " + st.Spec.Name, "S": st.Spec, "C": coll, "Q": query,
		"CustomSort": query.Sort != "" && query.Sort != "-_id" && query.Sort != "_id"}
	status := http.StatusOK
	p, err := s.agent.DBFind(r.Context(), st.Spec.Name, query)
	if err != nil {
		data["Err"], status = err.Error(), agentStatus(err)
		if status == http.StatusUnprocessableEntity {
			status = http.StatusOK // a mistake in the filter: the form is shown again
		}
	} else {
		data["P"] = p
		first := (p.Page-1)*p.PerPage + 1
		data["First"], data["Last"] = first, first+len(p.Docs)-1
	}
	s.renderStatus(w, r, status, "db_coll", data)
}

func (s *Server) dbDocPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	coll, ok1 := dbParam(q.Get("c"), 255)
	id, ok2 := dbParam(q.Get("id"), 1024)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	d, err := s.agent.DBDoc(r.Context(), st.Spec.Name, coll, id)
	if err != nil {
		if !s.docGone(w, r, st.Spec.Name, coll, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	s.render(w, r, "db_doc", map[string]any{"Title": coll + " · " + st.Spec.Name, "S": st.Spec, "C": coll, "D": d})
}

// dbEditForm shows a document in an editor. With from=<record>, the editor starts from
// that change's previous version (when an undo could not run).
func (s *Server) dbEditForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	coll, ok1 := dbParam(q.Get("c"), 255)
	id, ok2 := dbParam(q.Get("id"), 1024)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	d, err := s.agent.DBDoc(r.Context(), st.Spec.Name, coll, id)
	if err != nil {
		if !s.docGone(w, r, st.Spec.Name, coll, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	if !d.Editable {
		s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Bu belge panelde düzenlenemez: "+d.Why)
		return
	}
	text := d.Text
	if from := q.Get("from"); from != "" {
		// Only an edit's previous version, and only one whose text reads back as the same
		// document (a lossy text would change it when saved).
		v, err := s.agent.DBEdit(r.Context(), st.Spec.Name, from)
		if err != nil || v.Edit.Collection != coll || v.Edit.DocID != id || !v.Edit.Before || !v.Edit.After || !v.BeforeExact {
			s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Değişikliğin önceki hali düzenleme formunda açılamıyor.")
			return
		}
		text = v.Before
	}
	s.renderEdit(w, r, st, coll, id, d.Hash, text, nil, "")
}

func (s *Server) renderEdit(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, coll, id, hash, text string, pv *agent.DBPreview, problem string) {
	data := map[string]any{"Title": "Belgeyi düzenle · " + st.Spec.Name, "S": st.Spec, "C": coll, "ID": id, "Hash": hash,
		"Text": text, "Rows": min(max(strings.Count(text, "\n")+3, 8), 40), "Problem": problem,
		"EditMax": int64(agent.DBEditMax)}
	if pv != nil {
		data["PV"] = pv
	}
	s.render(w, r, "db_edit", data)
}

// dbEditPost previews (do=preview), saves (do=save) or goes back to the editor
// (do=back). The text never gets lost: on any refusal the editor shows it again.
func (s *Server) dbEditPost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	coll, ok1 := dbParam(r.PostFormValue("c"), 255)
	id, ok2 := dbParam(r.PostFormValue("id"), 1024)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	req := agent.DBEditRequest{Collection: coll, ID: id, Hash: r.PostFormValue("h"),
		Text: strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n"), Actor: s.actor(r)}
	switch r.PostFormValue("do") {
	case "back":
		s.renderEdit(w, r, st, coll, id, req.Hash, req.Text, nil, "")
	case "save":
		rec, err := s.agent.DBReplace(r.Context(), st.Spec.Name, req)
		switch {
		case unknown(err):
			s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Sonuç belirsiz: "+err.Error())
			return
		case agentStatus(err) == http.StatusConflict:
			s.rebase(w, r, st, coll, id, req.Text)
			return
		case err != nil:
			s.renderEdit(w, r, st, coll, id, req.Hash, req.Text, nil, "Kaydedilmedi: "+err.Error())
			return
		}
		s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id),
			"Kaydedildi ("+changeSummary(rec.Changes)+"). Önceki hali "+strconv.Itoa(int(agent.DBEditKeep.Hours()/24))+
				" gün saklanır; Veritabanı → Son değişiklikler → Geri al.")
	default:
		pv, err := s.agent.DBPreview(r.Context(), st.Spec.Name, req)
		switch {
		case agentStatus(err) == http.StatusConflict:
			s.rebase(w, r, st, coll, id, req.Text)
		case err != nil:
			s.renderEdit(w, r, st, coll, id, req.Hash, req.Text, nil, err.Error())
		default:
			s.renderEdit(w, r, st, coll, id, req.Hash, req.Text, &pv, "")
		}
	}
}

// rebase answers a conflict (the document changed since the editor opened): the
// operator's text stays, now against the document as it is, and the next preview shows
// the difference from that version — including undoing whatever changed meanwhile.
func (s *Server) rebase(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, coll, id, text string) {
	d, err := s.agent.DBDoc(r.Context(), st.Spec.Name, coll, id)
	switch {
	case err != nil:
		if !s.docGone(w, r, st.Spec.Name, coll, err) {
			s.renderEdit(w, r, st, coll, id, "", text, nil, "Belge sen açtıktan sonra değişti ve şimdi okunamıyor: "+err.Error())
		}
	case !d.Editable:
		s.renderEdit(w, r, st, coll, id, "", text, nil, "Belge sen açtıktan sonra değişti ve artık panelde düzenlenemiyor: "+d.Why)
	default:
		s.renderEdit(w, r, st, coll, id, d.Hash, text, nil, "Belge sen açtıktan sonra değişti (uygulama ya da başka biri yazdı); hiçbir şey kaydedilmedi. "+
			"Metnin duruyor: “Değişiklikleri gör” artık farkı belgenin güncel haline göre gösterecek, arada değişen alanlar da orada görünür.")
	}
}

// changeSummary is "3 alan değişti" style text.
func changeSummary(cs []agent.DBChange) string {
	var a, rm, ch int
	for _, c := range cs {
		switch c.Kind {
		case "added":
			a++
		case "removed":
			rm++
		default:
			ch++
		}
	}
	var parts []string
	if ch > 0 {
		parts = append(parts, strconv.Itoa(ch)+" alan değişti")
	}
	if a > 0 {
		parts = append(parts, strconv.Itoa(a)+" alan eklendi")
	}
	if rm > 0 {
		parts = append(parts, strconv.Itoa(rm)+" alan silindi")
	}
	return strings.Join(parts, ", ")
}

func (s *Server) dbDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	coll, ok1 := dbParam(q.Get("c"), 255)
	id, ok2 := dbParam(q.Get("id"), 1024)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	d, err := s.agent.DBDoc(r.Context(), st.Spec.Name, coll, id)
	if err != nil {
		if !s.docGone(w, r, st.Spec.Name, coll, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	if !d.Deletable || d.TooBig {
		s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Bu belge panelden silinemez.")
		return
	}
	s.render(w, r, "db_delete", map[string]any{"Title": "Belgeyi sil · " + st.Spec.Name, "S": st.Spec, "C": coll, "D": d,
		"KeepDays": int(agent.DBEditKeep.Hours() / 24)})
}

func (s *Server) dbDeletePost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	coll, ok1 := dbParam(r.PostFormValue("c"), 255)
	id, ok2 := dbParam(r.PostFormValue("id"), 1024)
	if !ok1 || !ok2 {
		http.NotFound(w, r)
		return
	}
	_, err := s.agent.DBDelete(r.Context(), st.Spec.Name, agent.DBEditRequest{Collection: coll, ID: id,
		Hash: r.PostFormValue("h"), Actor: s.actor(r)})
	switch {
	case unknown(err):
		s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Sonuç belirsiz: "+err.Error())
		return
	case err != nil:
		s.dbBack(w, r, dbURL(st.Spec.Name, "/doc", "c", coll, "id", id), "Silinmedi: "+err.Error())
		return
	}
	s.dbBack(w, r, dbURL(st.Spec.Name, "/coll", "c", coll), "Belge silindi. "+strconv.Itoa(int(agent.DBEditKeep.Hours()/24))+
		" gün içinde Veritabanı → Son değişiklikler → Geri al ile geri getirilebilir.")
}

func (s *Server) dbUndoConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	v, err := s.agent.DBEdit(r.Context(), st.Spec.Name, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), agentStatus(err))
		return
	}
	s.renderUndo(w, r, st, v, "")
}

func (s *Server) renderUndo(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, v agent.DBEditView, problem string) {
	s.render(w, r, "db_undo", map[string]any{"Title": "Değişikliği geri al · " + st.Spec.Name, "S": st.Spec, "V": v, "E": v.Edit,
		"Problem": problem})
}

func (s *Server) dbUndoPost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	rec, err := s.agent.DBUndo(r.Context(), st.Spec.Name, id, s.actor(r))
	if err != nil {
		msg := "Geri alınamadı: " + err.Error()
		if unknown(err) {
			msg = "Sonuç belirsiz: " + err.Error()
		}
		v, verr := s.agent.DBEdit(r.Context(), st.Spec.Name, id)
		if verr != nil {
			s.dbBack(w, r, dbURL(st.Spec.Name, ""), msg)
			return
		}
		s.renderUndo(w, r, st, v, msg)
		return
	}
	to := dbURL(st.Spec.Name, "/doc", "c", rec.Collection, "id", rec.DocID)
	if !rec.After { // the document is gone again
		to = dbURL(st.Spec.Name, "/coll", "c", rec.Collection)
	}
	s.dbBack(w, r, to, "Geri alındı. Bu da bir değişiklik olarak kaydedildi; gerekirse o da geri alınabilir.")
}
