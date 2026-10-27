package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Uploads and long downloads replace the server's read and write timeouts with these.
const (
	chunkTimeout  = 10 * time.Minute
	singleTimeout = 30 * time.Minute
	// ChunkSumHeader carries a chunk's hex SHA-256.
	ChunkSumHeader = "X-Chunk-Sha256"
)

// bodyLimit is the most a request body may hold: chunks and single uploads carry files,
// everything else is small JSON.
func bodyLimit(r *http.Request) int64 {
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/uploads/"):
		return MaxUploadChunk + 1
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads/single"):
		return MaxSingleUpload + 1
	}
	return maxBody
}

func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(d))
	_ = rc.SetWriteDeadline(time.Now().Add(d))
}

func uploadErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoUpload), errors.Is(err, sites.ErrNotFound), errors.Is(err, ErrNoCheck):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errBadChunk):
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, errUploadState):
		writeErr(w, http.StatusConflict, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

// ---- uploads ----

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	var req CreateUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	st, err := s.Data.CreateUpload(r.PathValue("name"), req.Name, req.Size, req.Actor)
	if err != nil {
		uploadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	st, err := s.Data.GetUpload(r.PathValue("id"))
	if err != nil {
		uploadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request) {
	extendDeadlines(w, chunkTimeout)
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz parça numarası")
		return
	}
	st, err := s.Data.PutChunk(r.PathValue("id"), n, r.Header.Get(ChunkSumHeader), r.Body)
	if err != nil {
		uploadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) finishUpload(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	before, _ := s.Data.GetUpload(r.PathValue("id"))
	st, err := s.Data.FinishUpload(r.PathValue("id"))
	if err != nil {
		uploadErr(w, err)
		return
	}
	if !before.Finished {
		s.record(AuditEntry{Actor: req.Actor, Action: "upload", Target: st.Site,
			Detail: fmt.Sprintf("%s, %s, tür %s", st.Name, humanBytes(st.Size), orDash(string(st.Kind)))}, nil)
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) singleUpload(w http.ResponseWriter, r *http.Request) {
	extendDeadlines(w, singleTimeout)
	var a Actor
	_ = json.Unmarshal([]byte(r.Header.Get(ActorHeader)), &a)
	st, err := s.Data.SingleUpload(r.PathValue("name"), r.URL.Query().Get("name"), r.Body, a)
	if err != nil {
		uploadErr(w, err)
		return
	}
	s.record(AuditEntry{Actor: a, Action: "upload", Target: st.Site,
		Detail: fmt.Sprintf("%s, %s, tür %s (tek parça)", st.Name, humanBytes(st.Size), orDash(string(st.Kind)))}, nil)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) removeUpload(w http.ResponseWriter, r *http.Request) {
	if err := s.Data.RemoveUpload(r.PathValue("id")); err != nil {
		uploadErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- imports and restores ----

// dataSite returns a site that has a FootOps database or a data directory.
func (s *Server) dataSite(w http.ResponseWriter, name string) (sites.Spec, bool) {
	spec, err := s.Sites.Store.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return spec, false
	}
	if !spec.Database && spec.DataMount == "" {
		writeErr(w, http.StatusBadRequest, "bu sitenin ne veritabanı ne kalıcı klasörü var")
		return spec, false
	}
	return spec, true
}

// dbSite returns a site that has a FootOps database, or writes the error.
func (s *Server) dbSite(w http.ResponseWriter, name string) (sites.Spec, bool) {
	spec, err := s.Sites.Store.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return spec, false
	}
	if !spec.Database {
		writeErr(w, http.StatusBadRequest, "bu sitenin veritabanı yok")
		return spec, false
	}
	return spec, true
}

func (s *Server) checkImport(w http.ResponseWriter, r *http.Request) {
	var req ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dataSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	if _, _, err := s.Data.uploadFile(spec.Name, req.Upload); err != nil {
		uploadErr(w, err)
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "import.check", Target: spec.Name, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.CheckImport(ctx, t, spec.Name, req.Upload, req.SourceDB)
			return err
		}, func(j Job) {
			detail := j.Err
			if ic, err := s.Data.LoadCheck(spec.Name, req.Upload); j.Err == "" && err == nil {
				detail = fmt.Sprintf("%s (sha256 %s…)%s", ic.File, short(ic.SHA256), partsNote(ic))
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

func (s *Server) getImport(w http.ResponseWriter, r *http.Request) {
	ic, err := s.Data.LoadCheck(r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		uploadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ic)
}

func (s *Server) applyImport(w http.ResponseWriter, r *http.Request) {
	var req ApplyImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dataSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	upload, opt := r.PathValue("id"), req.Options
	ic, err := s.Data.LoadCheck(spec.Name, upload)
	if err != nil {
		uploadErr(w, err)
		return
	}
	validMode := func(m string) bool { return m == ModeReplace || m == ModeAdd }
	switch {
	case req.Confirm != spec.Name: // rule K6, checked here as well as in the panel
		writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
		return
	case !opt.DB && !opt.Files:
		writeErr(w, http.StatusUnprocessableEntity, "aktarılacak bir şey seçilmedi (veritabanı ya da dosyalar)")
		return
	case opt.DB && !validMode(opt.DBMode), opt.Files && !validMode(opt.FilesMode):
		writeErr(w, http.StatusBadRequest, "geçersiz mod")
		return
	case opt.DB && (!ic.HasDB || ic.SourceDB == "" || ic.Archive == nil):
		writeErr(w, http.StatusConflict, "veritabanı kısmı hazır değil (veritabanı seçilmedi ya da arşivde yok)")
		return
	case opt.Files && ic.Files == nil:
		writeErr(w, http.StatusConflict, "arşivde bu siteye aktarılacak dosya yok")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "import.apply", Target: spec.Name, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.ApplyImport(ctx, t, spec.Name, upload, opt, req.Actor)
			return err
		}, func(j Job) {
			var parts []string
			if opt.DB {
				parts = append(parts, fmt.Sprintf("veritabanı (%s) %s → %s: %d koleksiyon, %d belge", opt.DBMode,
					ic.SourceDB, ic.Target, len(ic.Archive), ic.ArchiveDocs))
			}
			if opt.Files {
				e := ic.Files.Expect()
				parts = append(parts, fmt.Sprintf("dosyalar (%s%s): %d dosya, %s", opt.FilesMode,
					map[bool]string{true: ", üst klasör atıldı", false: ""}[opt.Strip], e.Files, humanBytes(e.Bytes)))
			}
			detail := fmt.Sprintf("%s (sha256 %s…): %s; geri alma: %s", ic.File, short(ic.SHA256), strings.Join(parts, "; "), orDash(j.Result))
			if j.Err != "" {
				detail += " — hata: " + j.Err
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

// partsNote summarises what a check found, for the audit log.
func partsNote(ic ImportCheck) string {
	var out string
	if ic.HasDB {
		out += fmt.Sprintf(", veritabanı %s: %d koleksiyon, %d belge", orDash(ic.SourceDB), len(ic.Archive), ic.ArchiveDocs)
	}
	if ic.Files != nil {
		e := ic.Files.Expect()
		out += fmt.Sprintf(", dosyalar: %d dosya, %s", e.Files, humanBytes(e.Bytes))
	}
	return out
}

func (s *Server) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	var req ConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	snapID := r.PathValue("id")
	if _, _, err := s.Data.SnapshotFile(spec.Name, snapID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if req.Confirm != spec.Name {
		writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "db.restore", Target: spec.Name, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.RestoreSnapshot(ctx, t, spec.Name, snapID, req.Actor)
			return err
		}, func(j Job) {
			detail := fmt.Sprintf("kopya %s geri yüklendi; öncesinin kopyası %s", snapID, orDash(j.Result))
			if j.Err != "" {
				detail = fmt.Sprintf("kopya %s — hata: %s", snapID, j.Err)
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) listFilesBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.Data.FilesBackups(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) restoreFiles(w http.ResponseWriter, r *http.Request) {
	var req ConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, err := s.Sites.Store.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	id := r.PathValue("id")
	list, _ := s.Data.FilesBackups(spec.Name)
	found := false
	for _, b := range list {
		found = found || b.ID == id
	}
	if !found {
		writeErr(w, http.StatusNotFound, "dosya kopyası bulunamadı")
		return
	}
	if req.Confirm != spec.Name {
		writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
		return
	}
	jid, err := s.Jobs.Start(JobSpec{Kind: "files.restore", Target: spec.Name, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.RestoreFiles(ctx, t, spec.Name, id, req.Actor)
			return err
		}, func(j Job) {
			detail := fmt.Sprintf("dosya kopyası %s geri yüklendi; öncesi %s", id, orDash(j.Result))
			if j.Err != "" {
				detail = fmt.Sprintf("dosya kopyası %s — hata: %s", id, j.Err)
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, jid, err)
}

// ---- exports ----

func (s *Server) startExport(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dataSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "site.export", Target: spec.Name, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.Export(ctx, t, spec.Name)
			return err
		}, func(j Job) {
			detail := j.Err
			if j.Err == "" {
				detail = "FootOps arşivi, " + humanBytes(j.Bytes)
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) listExports(w http.ResponseWriter, r *http.Request) {
	list, err := s.Data.Exports(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) downloadExport(w http.ResponseWriter, r *http.Request) {
	info, path, err := s.Data.ExportFile(r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.serveDownload(w, r, path, download{Name: info.File, Type: "application/x-tar", SHA256: info.SHA256,
		Action: "site.export.download", Target: info.Site + " " + info.File})
}

// download describes a file served to the panel.
type download struct {
	Name, Type, SHA256 string
	Action, Target     string // audit record
}

// serveDownload serves a file with Range support, so a broken download resumes. Only
// downloads from the start are audited; a resumed one is the same download.
func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request, path string, d download) {
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "dosya bulunamadı")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.Method != http.MethodHead && !resumes(r.Header.Get("Range")) {
		var a Actor
		_ = json.Unmarshal([]byte(r.Header.Get(ActorHeader)), &a)
		s.record(AuditEntry{Actor: a, Action: d.Action, Target: d.Target, Detail: humanBytes(st.Size())}, nil)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(downloadTimeout))
	h := w.Header()
	h.Set("Content-Type", d.Type)
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, d.Name))
	h.Set("X-Content-Sha256", d.SHA256)
	http.ServeContent(w, r, "", st.ModTime(), f)
}
