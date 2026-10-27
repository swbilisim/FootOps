package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/datafs"
)

// The file manager (F3): a site's data directory, folder by folder. Everything is a
// form or a link (JavaScript only speeds uploads up, as on the import page); every
// change goes to the agent, which runs it as the site's user.

const filesPerPage = 200

// filesSiteOr404 finds a site with a data directory.
func (s *Server) filesSiteOr404(w http.ResponseWriter, r *http.Request) (agent.SiteStatus, bool) {
	st, ok := s.findSite(w, r)
	if ok && st.Spec.DataMount == "" {
		http.Error(w, "bu sitenin kalıcı klasörü yok", http.StatusNotFound)
		return st, false
	}
	return st, ok
}

// cleanDir reads a folder path from a form or query ("" is the data directory).
func cleanDir(v string) (string, bool) {
	d, err := datafs.CleanPath(v)
	return d, err == nil
}

// filesURL is the address of a folder's page.
func filesURL(site, dir string) string {
	u := "/sites/" + site + "/files"
	if dir != "" {
		u += "?" + url.Values{"dir": {dir}}.Encode()
	}
	return u
}

// crumb is one step of the folder path above the listing.
type crumb struct{ Name, Dir string }

func crumbsOf(dir string) []crumb {
	if dir == "" {
		return nil
	}
	var out []crumb
	parts := strings.Split(dir, "/")
	for i, p := range parts {
		out = append(out, crumb{p, strings.Join(parts[:i+1], "/")})
	}
	return out
}

// parentDir is the folder above dir ("" for the data directory).
func parentDir(dir string) string {
	if d := path.Dir(dir); d != "." {
		return d
	}
	return ""
}

// sortOf keeps the sort parameter to the known ones.
func sortOf(v string) string {
	if v == "time" || v == "size" {
		return v
	}
	return "name"
}

func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	dir, ok := cleanDir(q.Get("dir"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	pg, _ := strconv.Atoi(q.Get("page"))
	pg = max(pg, 1)
	sort := sortOf(q.Get("sort"))
	data := map[string]any{"Title": st.Spec.Name + " dosyaları", "S": st.Spec, "Dir": dir, "Crumbs": crumbsOf(dir),
		"Sort": sort, "Page": pg, "Upload": true, "MaxSize": int64(agent.MaxUploadSize), "MaxSingle": int64(agent.MaxSingleUpload),
		"KeepDays": int(agent.TrashKeep.Hours() / 24), "Parent": parentDir(dir)}
	l, err := s.agent.Files(r.Context(), st.Spec.Name, dir, datafs.ListOptions{Sort: sort, Offset: (pg - 1) * filesPerPage, Limit: filesPerPage})
	status := http.StatusOK
	if err != nil {
		var ae *agent.Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			status = http.StatusNotFound
		}
		data["Err"] = err.Error()
	} else {
		data["L"] = l
		data["Pages"] = (l.Total + filesPerPage - 1) / filesPerPage
	}
	s.renderStatus(w, r, status, "files", data)
}

// passFiles is passDownload for the file manager: a refusal (a folder instead of a file,
// a zip already being made…) goes back to the folder with a message rather than to a
// bare error page.
func (s *Server) passFiles(w http.ResponseWriter, r *http.Request, site, dir string, open func() (*http.Response, error)) {
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp, err := open()
	if err != nil {
		s.back(w, r, site, dir, "İndirilemedi: "+err.Error())
		return
	}
	relay(w, resp)
}

// back returns to a folder's page with a message.
func (s *Server) back(w http.ResponseWriter, r *http.Request, site, dir, msg string) {
	if msg != "" {
		s.sessions.setFlash(current(r).ID, msg)
	}
	http.Redirect(w, r, filesURL(site, dir), http.StatusSeeOther)
}

// selection reads the items chosen on a folder's page.
func selection(r *http.Request) (string, []string, bool) {
	dir, ok := cleanDir(r.PostFormValue("dir"))
	names := r.PostForm["n"]
	if !ok || len(names) > 1000 {
		return "", nil, false
	}
	for _, n := range names {
		if datafs.CheckName(n) != nil {
			return "", nil, false
		}
	}
	return dir, names, true
}

func (s *Server) filesMkdir(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, ok := cleanDir(r.PostFormValue("dir"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	_, err := s.agent.ChangeFiles(r.Context(), st.Spec.Name, agent.FilesChange{Op: "mkdir", Dir: dir, Name: name, Actor: s.actor(r)})
	if err != nil {
		s.back(w, r, st.Spec.Name, dir, "Klasör oluşturulamadı: "+err.Error())
		return
	}
	s.back(w, r, st.Spec.Name, dir, "“"+name+"” klasörü oluşturuldu.")
}

// act is the selection form: its buttons open the move dialog or the trash
// confirmation, or download the selected items as a zip.
func (s *Server) filesAct(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, names, ok := selection(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if len(names) == 0 {
		s.back(w, r, st.Spec.Name, dir, "Önce listeden bir ya da daha çok öğe seç.")
		return
	}
	data := map[string]any{"S": st.Spec, "Dir": dir, "Names": names, "KeepDays": int(agent.TrashKeep.Hours() / 24)}
	switch r.PostFormValue("do") {
	case "zip":
		s.passFiles(w, r, st.Spec.Name, dir, func() (*http.Response, error) {
			return s.agent.ZipFiles(r.Context(), st.Spec.Name, dir, names, s.actor(r))
		})
	case "trash":
		data["Title"] = st.Spec.Name + " çöpe at"
		s.render(w, r, "files_trash", data)
	case "move":
		s.renderMove(w, r, st, data, http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

// renderMove shows the move dialog: every folder the selection may go to.
func (s *Server) renderMove(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, data map[string]any, status int) {
	dir, names := data["Dir"].(string), data["Names"].([]string)
	all, err := s.agent.FileDirs(r.Context(), st.Spec.Name)
	targets := []string{}
	if dir != "" {
		targets = append(targets, "")
	}
	for _, d := range all {
		inside := d == dir
		for _, n := range names {
			p := path.Join(dir, n)
			inside = inside || d == p || strings.HasPrefix(d, p+"/")
		}
		if !inside {
			targets = append(targets, d)
		}
	}
	data["Title"], data["Targets"], data["Err"] = st.Spec.Name+" taşı", targets, errString(err)
	s.renderStatus(w, r, status, "files_move", data)
}

func (s *Server) filesMove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, names, ok := selection(r)
	to, okTo := cleanDir(r.PostFormValue("to"))
	if !ok || !okTo || len(names) == 0 {
		http.NotFound(w, r)
		return
	}
	_, err := s.agent.ChangeFiles(r.Context(), st.Spec.Name, agent.FilesChange{Op: "move", Dir: dir, Names: names, To: to, Actor: s.actor(r)})
	if err != nil {
		data := map[string]any{"S": st.Spec, "Dir": dir, "Names": names, "Problem": err.Error()}
		s.renderMove(w, r, st, data, http.StatusUnprocessableEntity)
		return
	}
	s.back(w, r, st.Spec.Name, to, fmt.Sprintf("%d öğe buraya taşındı.", len(names)))
}

func (s *Server) filesTrash(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, names, ok := selection(r)
	if !ok || len(names) == 0 {
		http.NotFound(w, r)
		return
	}
	e, err := s.agent.ChangeFiles(r.Context(), st.Spec.Name, agent.FilesChange{Op: "trash", Dir: dir, Names: names, Actor: s.actor(r)})
	if err != nil {
		s.back(w, r, st.Spec.Name, dir, "Çöpe taşınamadı: "+err.Error())
		return
	}
	msg := fmt.Sprintf("%d öğe çöp kutusuna taşındı", len(names))
	if e != nil {
		msg += fmt.Sprintf(" (%d dosya, %s)", e.Files, humanSize(e.Bytes))
	}
	s.back(w, r, st.Spec.Name, dir, msg+"; Çöp kutusu sayfasından geri alınabilir.")
}

func (s *Server) filesRenameForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, ok := cleanDir(r.URL.Query().Get("dir"))
	name := r.URL.Query().Get("name")
	if !ok || datafs.CheckName(name) != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "files_rename", map[string]any{"Title": st.Spec.Name + " ad değiştir", "S": st.Spec, "Dir": dir, "Name": name, "To": name})
}

func (s *Server) filesRename(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, ok := cleanDir(r.PostFormValue("dir"))
	name, to := r.PostFormValue("name"), strings.TrimSpace(r.PostFormValue("to"))
	if !ok || datafs.CheckName(name) != nil {
		http.NotFound(w, r)
		return
	}
	_, err := s.agent.ChangeFiles(r.Context(), st.Spec.Name, agent.FilesChange{Op: "rename", Dir: dir, Name: name, To: to, Actor: s.actor(r)})
	if err != nil {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "files_rename", map[string]any{"Title": st.Spec.Name + " ad değiştir",
			"S": st.Spec, "Dir": dir, "Name": name, "To": to, "Problem": err.Error()})
		return
	}
	s.back(w, r, st.Spec.Name, dir, "“"+name+"” → “"+to+"”")
}

func (s *Server) filesDownload(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	p, err := datafs.CleanPath(r.URL.Query().Get("path"))
	if err != nil || p == "" {
		http.NotFound(w, r)
		return
	}
	s.passFiles(w, r, st.Spec.Name, parentDir(p), func() (*http.Response, error) {
		return s.agent.DownloadFile(r.Context(), st.Spec.Name, p, s.actor(r), r.Header)
	})
}

// filesZip downloads a whole folder as a zip.
func (s *Server) filesZip(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, ok := cleanDir(r.URL.Query().Get("dir"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.passFiles(w, r, st.Spec.Name, dir, func() (*http.Response, error) {
		return s.agent.ZipFiles(r.Context(), st.Spec.Name, dir, nil, s.actor(r))
	})
}

func (s *Server) filesUnpackConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	p, err := datafs.CleanPath(r.URL.Query().Get("path"))
	if err != nil || p == "" {
		http.NotFound(w, r)
		return
	}
	dir := parentDir(p)
	s.render(w, r, "files_unpack", map[string]any{"Title": st.Spec.Name + " arşivi aç", "S": st.Spec, "Path": p, "Dir": dir,
		"Name": path.Base(p), "KeepDays": int(agent.TrashKeep.Hours() / 24)})
}

func (s *Server) filesUnpack(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	p, err := datafs.CleanPath(r.PostFormValue("path"))
	if err != nil || p == "" {
		http.NotFound(w, r)
		return
	}
	job, err := s.agent.Unpack(r.Context(), st.Spec.Name, agent.UnpackRequest{Path: p, TrashArchive: r.PostFormValue("trash_archive") == "on", Actor: s.actor(r)})
	if err != nil {
		s.back(w, r, st.Spec.Name, parentDir(p), "Açılamadı: "+err.Error())
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

// filesPlace finishes an upload made by upload.js and puts it into a folder.
func (s *Server) filesPlace(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	extendDeadlines(w, formDeadline) // an upload that cannot be linked is copied
	var req struct {
		Upload    string `json:"upload"`
		Dir       string `json:"dir"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !reUploadID.MatchString(req.Upload) {
		jsonOut(w, http.StatusBadRequest, map[string]string{"error": "geçersiz istek"})
		return
	}
	dir, ok := cleanDir(req.Dir)
	if !ok {
		jsonOut(w, http.StatusUnprocessableEntity, map[string]string{"error": "geçersiz klasör"})
		return
	}
	p, err := s.place(r, st.Spec.Name, req.Upload, dir, req.Overwrite)
	if err != nil {
		jsonErr(w, err)
		return
	}
	s.noteUpload(r, p)
	jsonOut(w, http.StatusOK, p)
}

// place finishes an upload (if it is not yet) and puts it into dir.
func (s *Server) place(r *http.Request, site, upload, dir string, overwrite bool) (datafs.Placed, error) {
	up, err := s.agent.Upload(r.Context(), upload)
	if err != nil {
		return datafs.Placed{}, err
	}
	if up.Site != site {
		return datafs.Placed{}, &agent.Error{Status: http.StatusNotFound, Message: "yükleme bulunamadı"}
	}
	if !up.Finished {
		if _, err := s.agent.FinishUpload(r.Context(), upload, s.actor(r)); err != nil {
			return datafs.Placed{}, err
		}
	}
	return s.agent.PlaceUpload(r.Context(), site, agent.PlaceRequest{Upload: upload, Dir: dir, Overwrite: overwrite, Actor: s.actor(r)})
}

// noteUpload adds an uploaded file to the message shown on the folder's page; several
// uploads in a row add up (upload.js loads the page only after the last one).
func (s *Server) noteUpload(r *http.Request, p datafs.Placed) {
	msg := "“" + p.Name + "” yüklendi"
	if p.Trashed != "" {
		msg += " (eskisi çöp kutusunda)"
	}
	s.sessions.addFlash(current(r).ID, msg+".")
}

// filesUploadForm is the upload without JavaScript: a multipart form read as a stream
// (CSRF token first, then the folder and the overwrite choice, then the files), each
// file at most MaxSingleUpload.
func (s *Server) filesUploadForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	extendDeadlines(w, formDeadline)
	r.Body = http.MaxBytesReader(w, r.Body, agent.MaxSingleUpload+64<<10)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "form okunamadı", http.StatusBadRequest)
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
	dir, overwrite, n := "", false, 0
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.back(w, r, st.Spec.Name, dir, "Form okunamadı: "+err.Error())
			return
		}
		switch p.FormName() {
		case "dir":
			v, _ := io.ReadAll(io.LimitReader(p, 2048))
			var ok bool
			if dir, ok = cleanDir(string(v)); !ok {
				http.NotFound(w, r)
				return
			}
		case "overwrite":
			overwrite = true
		case "file":
			if p.FileName() == "" {
				continue
			}
			up, err := s.agent.SingleUpload(r.Context(), st.Spec.Name, p.FileName(), p, s.actor(r))
			if err == nil {
				var placed datafs.Placed
				if placed, err = s.place(r, st.Spec.Name, up.ID, dir, overwrite); err == nil {
					s.noteUpload(r, placed)
					n++
					continue
				}
				_ = s.agent.RemoveUpload(r.Context(), up.ID)
			}
			s.sessions.addFlash(current(r).ID, "“"+p.FileName()+"” yüklenemedi: "+err.Error())
			s.back(w, r, st.Spec.Name, dir, "")
			return
		}
	}
	if n == 0 {
		s.sessions.setFlash(current(r).ID, "Dosya seçilmedi.")
	}
	s.back(w, r, st.Spec.Name, dir, "")
}

func (s *Server) filesURLSet(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	dir, _ := cleanDir(r.PostFormValue("dir"))
	u := strings.TrimSpace(r.PostFormValue("url"))
	if err := s.agent.SetDataURL(r.Context(), st.Spec.Name, u, s.actor(r)); err != nil {
		s.back(w, r, st.Spec.Name, dir, "Kaydedilemedi: "+err.Error())
		return
	}
	msg := "Web adresi kaydedildi; dosyaların “Sitede aç” bağlantısı var."
	if u == "" {
		msg = "Web adresi kaldırıldı."
	}
	s.back(w, r, st.Spec.Name, dir, msg)
}

// ---- trash ----

func (s *Server) trash(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	list, err := s.agent.Trash(r.Context(), st.Spec.Name)
	var size int64
	for _, e := range list {
		size += e.Bytes
	}
	s.render(w, r, "trash", map[string]any{"Title": st.Spec.Name + " çöp kutusu", "S": st.Spec, "List": list, "Bytes": size,
		"Err": errString(err), "KeepDays": int(agent.TrashKeep.Hours() / 24), "MaxListed": datafs.MaxTrashListed})
}

func (s *Server) trashRestore(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !datafs.TrashIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	res, err := s.agent.RestoreTrash(r.Context(), st.Spec.Name, id, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Geri alınamadı: "+err.Error())
		http.Redirect(w, r, "/sites/"+st.Spec.Name+"/trash", http.StatusSeeOther)
		return
	}
	var renamed []string
	for _, it := range res.Items {
		if it.As != it.Name {
			renamed = append(renamed, "“"+it.Name+"” → “"+it.As+"”")
		}
	}
	msg := fmt.Sprintf("%d öğe geri alındı.", len(res.Items))
	if len(renamed) > 0 {
		msg += " Aynı adda bir öğe olduğu için yeni adla konanlar: " + strings.Join(renamed, ", ") + "."
	}
	s.back(w, r, st.Spec.Name, res.Dir, msg)
}

func (s *Server) trashEmptyConfirm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	s.render(w, r, "trash_empty", map[string]any{"Title": st.Spec.Name + " çöpü boşalt", "S": st.Spec})
}

func (s *Server) trashEmpty(w http.ResponseWriter, r *http.Request) {
	st, ok := s.filesSiteOr404(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != st.Spec.Name {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "trash_empty", map[string]any{"Title": st.Spec.Name + " çöpü boşalt",
			"S": st.Spec, "Problem": "Onay için site adını aynen yaz."})
		return
	}
	res, err := s.agent.EmptyTrash(r.Context(), st.Spec.Name, r.PostFormValue("confirm"), s.actor(r))
	msg := fmt.Sprintf("Çöp kutusu boşaltıldı: %d kayıt, %s.", res.Entries, humanSize(res.Bytes))
	if err != nil {
		msg = "Boşaltılamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/sites/"+st.Spec.Name+"/trash", http.StatusSeeOther)
}
