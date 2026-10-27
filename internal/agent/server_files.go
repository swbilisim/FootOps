package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- file manager (F3) ----

// PlaceRequest puts a finished upload into a folder of the data directory.
type PlaceRequest struct {
	Upload    string `json:"upload"`
	Dir       string `json:"dir"`
	Overwrite bool   `json:"overwrite"`
	Actor     Actor  `json:"actor"`
}

// UnpackRequest opens an archive of the data directory in its folder.
type UnpackRequest struct {
	Path         string `json:"path"`
	TrashArchive bool   `json:"trash_archive"`
	Actor        Actor  `json:"actor"`
}

// DataURLRequest sets where the site serves its data directory's files.
type DataURLRequest struct {
	URL   string `json:"url"`
	Actor Actor  `json:"actor"`
}

// EmptyTrashResult says what emptying the trash removed.
type EmptyTrashResult struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// filesErr answers with the status that fits a file manager error.
func filesErr(w http.ResponseWriter, err error) {
	var fe *FilesError
	switch {
	case errors.As(err, &fe):
		status := map[string]int{"notfound": http.StatusNotFound, "exists": http.StatusConflict,
			"invalid": http.StatusUnprocessableEntity}[fe.Code]
		if status == 0 {
			status = http.StatusBadRequest
		}
		writeErr(w, status, fe.Msg)
	case errors.Is(err, ErrBusy), errors.Is(err, ErrZipBusy):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, sites.ErrNotFound), errors.Is(err, ErrNoUpload):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// filesSite returns a site with a data directory, or writes the error.
func (s *Server) filesSite(w http.ResponseWriter, name string) (sites.Spec, bool) {
	spec, err := s.Sites.Store.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return spec, false
	}
	if spec.DataMount == "" {
		writeErr(w, http.StatusBadRequest, "bu sitenin kalıcı klasörü yok")
		return spec, false
	}
	return spec, true
}

// hold reserves the site for a change, or answers 423 (Locked) while a job works on
// it: unlike a taken name (409), it is worth trying again a little later.
func (s *Server) hold(w http.ResponseWriter, site string) (func(), bool) {
	release, err := s.Jobs.Hold(site)
	if err != nil {
		writeErr(w, http.StatusLocked, "bu sitede bir işlem sürüyor; bitince tekrar dene")
		return nil, false
	}
	return release, true
}

// slash shows a path of the data directory as the panel does: /fotolar/a.jpg.
func slash(parts ...string) string {
	return "/" + strings.TrimPrefix(path.Join(parts...), "/")
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	q := r.URL.Query()
	off, _ := strconv.Atoi(q.Get("offset"))
	lim, _ := strconv.Atoi(q.Get("limit"))
	l, err := s.Data.ListFiles(r.Context(), spec.Name, q.Get("dir"),
		datafs.ListOptions{Sort: q.Get("sort"), Offset: off, Limit: lim, DirsOnly: q.Get("dirs") == "1"})
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) fileDirs(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	dirs, err := s.Data.FileDirs(r.Context(), spec.Name)
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dirs)
}

// downloadFile serves a file of the data directory (ranges: a broken download resumes).
// It is always an attachment: the panel never shows a site's files inline.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	f, info, err := s.Data.OpenFile(r.Context(), spec.Name, p)
	if err != nil {
		filesErr(w, err)
		return
	}
	defer f.Close()
	s.serveFile(w, r, f, download{Name: info.Name, Type: "application/octet-stream",
		Action: "files.download", Target: spec.Name + " " + slash(p)})
}

// headerOnce sends the zip's headers with its first bytes, so an error before them can
// still be answered properly.
type headerOnce struct {
	w       http.ResponseWriter
	name    string
	started bool
}

func (h *headerOnce) Write(p []byte) (int, error) {
	if !h.started {
		h.started = true
		h.w.Header().Set("Content-Type", "application/zip")
		h.w.Header().Set("Content-Disposition", attachment(h.name))
		h.w.WriteHeader(http.StatusOK)
	}
	return h.w.Write(p)
}

// zipFiles streams a folder, or some of its items, as a zip.
func (s *Server) zipFiles(w http.ResponseWriter, r *http.Request) {
	var req FilesChange
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	name := spec.Name
	switch {
	case len(req.Names) == 1:
		name = req.Names[0]
	case req.Dir != "":
		name = path.Base(req.Dir)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadTimeout))
	out := &headerOnce{w: w, name: name + ".zip"}
	st, u, err := s.Data.Zip(r.Context(), spec.Name, req.Dir, req.Names, out)
	target := spec.Name + " " + slash(req.Dir)
	if len(req.Names) > 0 {
		target += ": " + strings.Join(req.Names, ", ")
	}
	detail := fmt.Sprintf("%d dosya, %s, %s", st.Files, humanBytes(st.Bytes), usageNote(u))
	if err != nil {
		detail = "hata: " + err.Error()
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "files.zip", Target: target, Detail: detail}, err)
	switch {
	case err != nil && !out.started:
		filesErr(w, err)
	case err != nil:
		// Part of the zip is out: end the response without its last chunk, so the
		// browser reports a failed download instead of saving a broken zip.
		panic(http.ErrAbortHandler)
	}
}

// changeFiles makes a folder, renames, moves or deletes to the trash.
func (s *Server) changeFiles(w http.ResponseWriter, r *http.Request) {
	var c FilesChange
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	entry, err := s.Data.ChangeFiles(r.Context(), spec.Name, c)
	target, detail := spec.Name+" ", ""
	switch c.Op {
	case "mkdir":
		target += slash(c.Dir, c.Name)
	case "rename":
		target += slash(c.Dir, c.Name) + " → " + c.To
	case "move":
		target += slash(c.Dir) + ": " + strings.Join(c.Names, ", ") + " → " + slash(c.To)
	case "trash":
		target += slash(c.Dir) + ": " + strings.Join(c.Names, ", ")
		if entry != nil {
			detail = fmt.Sprintf("%d dosya, %s; çöp kutusu %s", entry.Files, humanBytes(entry.Bytes), entry.ID)
		}
	}
	if err != nil {
		detail = err.Error()
	}
	s.record(AuditEntry{Actor: c.Actor, Action: "files." + c.Op, Target: target, Detail: detail}, err)
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry})
}

func (s *Server) placeUpload(w http.ResponseWriter, r *http.Request) {
	extendDeadlines(w, singleTimeout) // an upload that cannot be linked is copied
	var req PlaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	p, err := s.Data.PlaceUpload(r.Context(), spec.Name, req.Upload, req.Dir, req.Overwrite, req.Actor)
	if err != nil {
		s.record(AuditEntry{Actor: req.Actor, Action: "files.upload", Target: spec.Name + " " + slash(req.Dir), Detail: err.Error()}, err)
		filesErr(w, err)
		return
	}
	detail := humanBytes(p.Size) + map[bool]string{true: ", bağlantıyla (kopyasız)", false: ", kopyalanarak"}[p.Linked]
	if p.Trashed != "" {
		detail += "; eskisi çöp kutusunda (" + p.Trashed + ")"
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "files.upload", Target: spec.Name + " " + slash(req.Dir, p.Name), Detail: detail}, nil)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) unpackFile(w http.ResponseWriter, r *http.Request) {
	var req UnpackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	p, err := datafs.CleanPath(req.Path)
	if err != nil || p == "" {
		writeErr(w, http.StatusUnprocessableEntity, "geçersiz yol")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "files.unpack", Target: spec.Name, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.Unpack(ctx, t, spec.Name, p, req.TrashArchive, req.Actor)
			return err
		}, func(j Job) {
			detail := slash(p)
			if j.Err != "" {
				detail += " — hata: " + j.Err
			} else {
				detail += fmt.Sprintf(" açıldı, %s", humanBytes(j.Bytes))
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) listTrash(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	list, err := s.Data.TrashList(r.Context(), spec.Name)
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) restoreTrash(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	id := r.PathValue("id")
	res, err := s.Data.Untrash(r.Context(), spec.Name, id)
	var parts []string
	for _, it := range res.Items {
		if it.As != it.Name {
			parts = append(parts, it.Name+" → "+it.As)
		} else {
			parts = append(parts, it.Name)
		}
	}
	detail := slash(res.Dir) + ": " + strings.Join(parts, ", ")
	if err != nil {
		detail += " — hata: " + err.Error()
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "files.untrash", Target: spec.Name + " " + id, Detail: detail}, err)
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) emptyTrash(w http.ResponseWriter, r *http.Request) {
	var req ConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	if req.Confirm != spec.Name { // rule K6: deleting for good is confirmed by the name
		writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	n, size, err := s.Data.EmptyTrash(spec.Name)
	s.record(AuditEntry{Actor: req.Actor, Action: "files.empty-trash", Target: spec.Name,
		Detail: fmt.Sprintf("%d kayıt, %s kalıcı olarak silindi", n, humanBytes(size))}, err)
	if err != nil {
		filesErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, EmptyTrashResult{Entries: n, Bytes: size})
}

func (s *Server) setDataURL(w http.ResponseWriter, r *http.Request) {
	var req DataURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.filesSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	release, ok := s.hold(w, spec.Name)
	if !ok {
		return
	}
	defer release()
	spec, err := s.Sites.SetDataURL(spec.Name, req.URL)
	s.record(AuditEntry{Actor: req.Actor, Action: "files.url", Target: spec.Name, Detail: orDash(spec.DataURL)}, err)
	if err != nil {
		writeProblems(w, err)
		return
	}
	writeJSON(w, http.StatusOK, spec)
}
