package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

// Imports and exports (F2). The browser sends a file in chunks with upload.js; each
// chunk streams through this process to the agent without touching memory or disk
// here. Without JavaScript the import form posts the whole file in one request.

var reUploadID = regexp.MustCompile(`^[A-Za-z0-9]{24}$`)

const (
	// csrfHeader carries the session's CSRF token on requests made by upload.js.
	csrfHeader    = "X-CSRF-Token"
	chunkDeadline = 10 * time.Minute
	formDeadline  = 30 * time.Minute
)

func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(d))
	_ = rc.SetWriteDeadline(time.Now().Add(d))
}

// jsonOut answers upload.js.
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// jsonErr passes the agent's status on (422 tells upload.js to send a chunk again).
func jsonErr(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var ae *agent.Error
	if errors.As(err, &ae) {
		status = ae.Status
	}
	jsonOut(w, status, map[string]string{"error": err.Error()})
}

// dbSiteOr404 finds a site with a database for the import pages.
func (s *Server) dbSiteOr404(w http.ResponseWriter, r *http.Request) (agent.SiteStatus, bool) {
	st, ok := s.findSite(w, r)
	if ok && !st.Spec.Database {
		http.Error(w, "bu sitenin veritabanı yok", http.StatusNotFound)
		return st, false
	}
	return st, ok
}

func (s *Server) importPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	s.render(w, r, "import", map[string]any{"Title": st.Spec.Name + " içe aktar", "S": st.Spec, "Upload": true,
		"MaxSize": int64(agent.MaxUploadSize), "MaxSingle": int64(agent.MaxSingleUpload)})
}

func (s *Server) uploadCreate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "geçersiz istek"})
		return
	}
	up, err := s.agent.CreateUpload(r.Context(), st.Spec.Name, req.Name, req.Size, s.actor(r))
	if err != nil {
		jsonErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, up)
}

func (s *Server) uploadStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	up, err := s.agent.Upload(r.Context(), id)
	if err != nil {
		jsonErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, up)
}

// uploadChunk streams one chunk to the agent.
func (s *Server) uploadChunk(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := strconv.Atoi(r.PathValue("n"))
	if !reUploadID.MatchString(id) || err != nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	extendDeadlines(w, chunkDeadline)
	r.Body = http.MaxBytesReader(w, r.Body, agent.MaxUploadChunk+1)
	up, err := s.agent.PutChunk(r.Context(), id, n, r.Header.Get(agent.ChunkSumHeader), r.Body, r.ContentLength)
	if err != nil {
		jsonErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, up)
}

// uploadFinish closes the upload and starts checking it; upload.js then opens the job.
func (s *Server) uploadFinish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	up, err := s.agent.FinishUpload(r.Context(), id, s.actor(r))
	if err != nil {
		jsonErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"next": s.startCheck(r, up.Site, id, "")})
}

// startCheck starts checking an upload and returns where to go: the job, or the preview
// page (with a message) if the check could not start.
func (s *Server) startCheck(r *http.Request, site, upload, sourceDB string) string {
	job, err := s.agent.CheckImport(r.Context(), site, upload, sourceDB, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Kontrol başlatılamadı: "+err.Error())
		return "/sites/" + site + "/import/" + upload
	}
	return "/jobs/" + job
}

func (s *Server) uploadCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	up, err := s.agent.Upload(r.Context(), id)
	if err == nil {
		err = s.agent.RemoveUpload(r.Context(), id)
	}
	if r.Header.Get(csrfHeader) != "" {
		if err != nil {
			jsonErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	msg := "Yüklenen dosya silindi."
	if err != nil {
		msg = "Silinemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	to := "/sites"
	if up.Site != "" {
		to = "/sites/" + up.Site
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// importForm is the upload without JavaScript: a multipart form whose first field is
// the CSRF token and whose second is the file, read as a stream (never parsed into
// memory or temporary files).
func (s *Server) importForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	extendDeadlines(w, formDeadline)
	r.Body = http.MaxBytesReader(w, r.Body, agent.MaxSingleUpload+64<<10)
	back := func(msg string) {
		s.sessions.setFlash(current(r).ID, msg)
		http.Redirect(w, r, "/sites/"+st.Spec.Name+"/import", http.StatusSeeOther)
	}
	mr, err := r.MultipartReader()
	if err != nil {
		back("Form okunamadı.")
		return
	}
	p, err := mr.NextPart()
	if err != nil || p.FormName() != "csrf" {
		http.Error(w, "geçersiz form (CSRF); sayfayı yenileyip tekrar dene", http.StatusForbidden)
		return
	}
	tok, _ := io.ReadAll(io.LimitReader(p, 256))
	if subtle.ConstantTimeCompare(tok, []byte(current(r).CSRF)) != 1 {
		http.Error(w, "geçersiz form (CSRF); sayfayı yenileyip tekrar dene", http.StatusForbidden)
		return
	}
	p, err = mr.NextPart()
	if err != nil || p.FormName() != "file" || p.FileName() == "" {
		back("Dosya seçilmedi.")
		return
	}
	up, err := s.agent.SingleUpload(r.Context(), st.Spec.Name, p.FileName(), p, s.actor(r))
	if err != nil {
		back("Yüklenemedi: " + err.Error())
		return
	}
	http.Redirect(w, r, s.startCheck(r, st.Spec.Name, up.ID, ""), http.StatusSeeOther)
}

// collRow is one line of the preview table.
type collRow struct {
	Name            string
	Archive, Now    int64
	InArchive, Here bool
	View            bool
}

// Note says what happens to the collection.
func (c collRow) Note() string {
	switch {
	case c.InArchive && !c.Here:
		return "yeni"
	case !c.InArchive && c.Here:
		return "yalnız sitede: “Tamamen değiştir” siler"
	case c.View:
		return "görünüm"
	case c.Archive != c.Now:
		return "farklı"
	}
	return "aynı"
}

func rowsOf(ic agent.ImportCheck) []collRow {
	m := map[string]*collRow{}
	row := func(name string) *collRow {
		if m[name] == nil {
			m[name] = &collRow{Name: name}
		}
		return m[name]
	}
	for k, v := range ic.Archive {
		r := row(k)
		r.Archive, r.InArchive = v, true
	}
	for _, k := range ic.Views {
		r := row(k)
		r.InArchive, r.View = true, true
	}
	for k, v := range ic.Current {
		r := row(k)
		r.Now, r.Here = v, true
	}
	for _, k := range ic.CurrentViews {
		r := row(k)
		r.Here, r.View = true, true
	}
	out := make([]collRow, 0, len(m))
	for _, r := range m {
		out = append(out, *r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (s *Server) importPreview(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	s.renderPreview(w, r, st, http.StatusOK, "", "")
}

func (s *Server) renderPreview(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, status int, problem, mode string) {
	id := r.PathValue("id")
	data := map[string]any{"Title": st.Spec.Name + " içe aktarma", "S": st.Spec, "ID": id, "Problem": problem, "Mode": mode}
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	up, err := s.agent.Upload(r.Context(), id)
	if err != nil || up.Site != st.Spec.Name {
		data["Gone"] = true
		s.renderStatus(w, r, http.StatusNotFound, "import_preview", data)
		return
	}
	data["U"] = up
	ic, err := s.agent.Import(r.Context(), st.Spec.Name, id)
	var ae *agent.Error
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		data["Unchecked"] = true
	case err != nil:
		data["Err"] = err.Error()
	default:
		data["C"], data["Rows"] = ic, rowsOf(ic)
	}
	if mode == "" {
		data["Mode"] = agent.ModeReplace
	}
	s.renderStatus(w, r, status, "import_preview", data)
}

func (s *Server) importRecheck(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, s.startCheck(r, st.Spec.Name, id, r.PostFormValue("source_db")), http.StatusSeeOther)
}

func (s *Server) importApply(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	id, mode := r.PathValue("id"), r.PostFormValue("mode")
	if !reUploadID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	if r.PostFormValue("confirm") != st.Spec.Name {
		s.renderPreview(w, r, st, http.StatusUnprocessableEntity, "Onay için site adını aynen yaz.", mode)
		return
	}
	job, err := s.agent.ApplyImport(r.Context(), st.Spec.Name, id, mode, r.PostFormValue("confirm"), s.actor(r))
	if err != nil {
		s.renderPreview(w, r, st, http.StatusUnprocessableEntity, err.Error(), mode)
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

// snapshotOf finds one of the site's copies.
func (s *Server) snapshotOf(w http.ResponseWriter, r *http.Request, site string) (agent.Snapshot, bool) {
	id := r.PathValue("id")
	if !reSnapshotID.MatchString(id) {
		http.NotFound(w, r)
		return agent.Snapshot{}, false
	}
	list, err := s.agent.Snapshots(r.Context(), site)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return agent.Snapshot{}, false
	}
	for _, sn := range list {
		if sn.ID == id {
			return sn, true
		}
	}
	http.NotFound(w, r)
	return agent.Snapshot{}, false
}

func (s *Server) restoreConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	sn, ok := s.snapshotOf(w, r, st.Spec.Name)
	if !ok {
		return
	}
	s.render(w, r, "restore_confirm", map[string]any{"Title": st.Spec.Name + " geri yükle", "S": st.Spec, "Snap": sn})
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	sn, ok := s.snapshotOf(w, r, st.Spec.Name)
	if !ok {
		return
	}
	again := func(problem string) {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "restore_confirm",
			map[string]any{"Title": st.Spec.Name + " geri yükle", "S": st.Spec, "Snap": sn, "Problem": problem})
	}
	if r.PostFormValue("confirm") != st.Spec.Name {
		again("Onay için site adını aynen yaz.")
		return
	}
	job, err := s.agent.RestoreSnapshot(r.Context(), st.Spec.Name, sn.ID, r.PostFormValue("confirm"), s.actor(r))
	if err != nil {
		again(err.Error())
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

func (s *Server) exportStart(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	job, err := s.agent.Export(r.Context(), st.Spec.Name, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Dışa aktarma başlatılamadı: "+err.Error())
		http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

func (s *Server) exportDownload(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !reJobID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.passDownload(w, r, func() (*http.Response, error) {
		return s.agent.DownloadExport(r.Context(), st.Spec.Name, id, s.actor(r), r.Header)
	})
}
