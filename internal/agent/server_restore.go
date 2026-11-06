package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- restoring from backups (Faz Y3) ----

// BackupOpenRequest opens a backup's database for a look (or closes it).
type BackupOpenRequest struct {
	Actor Actor `json:"actor"`
}

// BackupListRequest reads the backup list again; Site, if given, is the page it was
// asked from (the job's target, for its way back).
type BackupListRequest struct {
	Site  string `json:"site,omitempty"`
	Actor Actor  `json:"actor"`
}

// BackupRestoreRequest restores a backup; Confirm is the site's name (rule K6).
type BackupRestoreRequest struct {
	BackupRestore
	Confirm string `json:"confirm"`
	Actor   Actor  `json:"actor"`
}

// BackupExportRequest makes a backup into a FootOps archive.
type BackupExportRequest struct {
	BackupExport
	Actor Actor `json:"actor"`
}

// BackupCompare is a backup beside the site as it is now: a restore's preview.
type BackupCompare struct {
	Backup  BackupDetail  `json:"backup"`
	Current BackupCurrent `json:"current"`
}

// backupErr answers with the status that fits a restore-side error.
func backupErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoBackup):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, errBadPath):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, errBackupGone), errors.Is(err, errSnapshotGone), errors.Is(err, errNoBackupPath), errors.Is(err, errNoFilesPart),
		errors.Is(err, sites.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, errViewGone):
		writeErr(w, http.StatusGone, err.Error())
	case errors.Is(err, ErrReadBusy), errors.Is(err, ErrDBBusy):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeErr(w, http.StatusBadGateway, err.Error())
	}
}

// backupSite is the site a restore-side request is about.
func (s *Server) backupSite(w http.ResponseWriter, r *http.Request) (sites.Spec, bool) {
	spec, err := s.Sites.Store.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return spec, false
	}
	return spec, true
}

func (s *Server) backupList(w http.ResponseWriter, r *http.Request) {
	var req BackupListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if _, err := s.Backups.secret(); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	target := "yedek listesi"
	if req.Site != "" {
		spec, err := s.Sites.Store.Get(req.Site)
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		target = spec.Name
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "backup.list", Target: target, Actor: req.Actor, Cancellable: true},
		s.Backups.ListBackups, func(j Job) { s.recordJobDetail(j, orDash(j.Err+j.Result)) })
	s.jobResponse(w, id, err)
}

func (s *Server) siteBackups(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	out, err := s.Backups.SiteBackups(spec.Name)
	if err != nil {
		backupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) siteBackup(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	d, err := s.Backups.SiteBackup(spec.Name, r.PathValue("run"))
	if err != nil {
		backupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) backupCompare(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	d, err := s.Backups.SiteBackup(spec.Name, r.PathValue("run"))
	if err != nil {
		backupErr(w, err)
		return
	}
	cur, err := s.Backups.Current(r.Context(), spec.Name)
	if err != nil {
		backupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, BackupCompare{Backup: d, Current: cur})
}

func (s *Server) backupOpen(w http.ResponseWriter, r *http.Request) {
	var req BackupOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dbSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	run := r.PathValue("run")
	d, err := s.Backups.SiteBackup(spec.Name, run)
	if err != nil {
		backupErr(w, err)
		return
	}
	if _, ok := d.Parts[PartDB]; !ok {
		writeErr(w, http.StatusConflict, "bu yedekte veritabanı yok")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "backup.open", Target: spec.Name, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := s.Backups.OpenBackup(ctx, t, spec.Name, run, req.Actor)
			return err
		}, func(j Job) {
			detail := fmt.Sprintf("yedek %s (%s) incelemeye açıldı", localTime(d.Time), run)
			if v, err := s.Backups.loadView(j.ID); err == nil && v.Ready {
				detail += fmt.Sprintf(": %d koleksiyon, %d belge", len(v.Collections), v.Documents)
				if v.RecordDiff != "" {
					detail += "; kayıtla farklı: " + clipMsg(v.RecordDiff, 200)
				}
			}
			if j.Err != "" {
				detail = fmt.Sprintf("yedek %s (%s) açılamadı: %s", localTime(d.Time), run, j.Err)
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) viewClose(w http.ResponseWriter, r *http.Request) {
	var req BackupOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	err := s.Backups.CloseView(spec.Name, r.PathValue("id"))
	s.record(AuditEntry{Actor: req.Actor, Action: "backup.close", Target: spec.Name, Detail: "açılan yedek kapatıldı"}, err)
	if err != nil {
		backupErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// inView runs a database browser read on an opened backup: the same handler, logged in
// with the view's temporary user. Only reads are wrapped (rule: a backup is looked at,
// never changed).
func (s *Server) inView(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ctx, err := s.Backups.ViewContext(r.Context(), r.PathValue("name"), r.PathValue("id"))
		if err != nil {
			backupErr(w, err)
			return
		}
		h(w, r.WithContext(ctx))
	}
}

func (s *Server) backupRestore(w http.ResponseWriter, r *http.Request) {
	var req BackupRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.dataSite(w, r.PathValue("name"))
	if !ok {
		return
	}
	req.Run = r.PathValue("run")
	d, err := s.Backups.SiteBackup(spec.Name, req.Run)
	if err != nil {
		backupErr(w, err)
		return
	}
	_, hasDB := d.Parts[PartDB]
	_, hasFiles := d.Parts[PartFiles]
	switch {
	case req.Confirm != spec.Name: // rule K6, checked here as well as in the panel
		writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
		return
	case !req.DB && !req.Files:
		writeErr(w, http.StatusUnprocessableEntity, "geri yüklenecek bir şey seçilmedi (veritabanı ya da dosyalar)")
		return
	case req.DB && (!hasDB || !spec.Database):
		writeErr(w, http.StatusConflict, "bu yedekte (ya da sitede) veritabanı yok")
		return
	case req.Files && (!hasFiles || spec.DataMount == ""):
		writeErr(w, http.StatusConflict, "bu yedekte (ya da sitede) kalıcı klasör yok")
		return
	}
	var view viewRecord
	if req.DB {
		v, err := s.Backups.viewFor(spec.Name, req.View)
		if err != nil || v.Run != req.Run {
			writeErr(w, http.StatusConflict, "veritabanının önizlemesi hazır değil: önce bu yedeği aç (incelemeye aç, sayımlar orada çıkar)")
			return
		}
		if v.RecordDiff != "" && !req.AcceptDiff {
			writeErr(w, http.StatusConflict, "açılan yedeğin sayımları yedek alınırken sayılanla tutmuyor: önizlemede farkı onayla")
			return
		}
		view = v
	}
	restore := req.BackupRestore
	id, err := s.Jobs.Start(JobSpec{Kind: "backup.restore", Target: spec.Name, Actor: req.Actor},
		func(ctx context.Context, t *Task) error {
			_, err := s.Backups.Restore(ctx, t, spec.Name, restore, req.Actor)
			return err
		}, func(j Job) {
			var parts []string
			if restore.DB {
				p := fmt.Sprintf("veritabanı (%d koleksiyon, %d belge)", len(view.Collections), view.Documents)
				if restore.AcceptDiff {
					p += " — kayıtla farkı onaylandı"
				}
				parts = append(parts, p)
			}
			if restore.Files {
				f := d.Parts[PartFiles]
				parts = append(parts, fmt.Sprintf("dosyalar (%d dosya, %s)", f.Files, humanBytes(f.Bytes)))
			}
			detail := fmt.Sprintf("yedek %s (%s): %s; geri alma: %s", localTime(d.Time), restore.Run, strings.Join(parts, ", "), orDash(j.Result))
			if j.Err != "" {
				detail += " — hata: " + j.Err
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) backupExport(w http.ResponseWriter, r *http.Request) {
	var req BackupExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	req.Run = r.PathValue("run")
	d, err := s.Backups.SiteBackup(spec.Name, req.Run)
	if err != nil {
		backupErr(w, err)
		return
	}
	if !req.DB && !req.Files {
		writeErr(w, http.StatusUnprocessableEntity, "arşive veritabanı ya da dosyalar seçilmeli")
		return
	}
	exp := req.BackupExport
	id, err := s.Jobs.Start(JobSpec{Kind: "backup.export", Target: spec.Name, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := s.Backups.ExportBackup(ctx, t, spec.Name, exp)
			return err
		}, func(j Job) {
			var parts []string
			for _, p := range []struct {
				on   bool
				name string
			}{{exp.DB, "veritabanı"}, {exp.Files, "dosyalar"}, {exp.Env, "ortam değişkenleri (sırlar dahil)"}} {
				if p.on {
					parts = append(parts, p.name)
				}
			}
			detail := fmt.Sprintf("yedek %s (%s) arşive çevrildi: %s", localTime(d.Time), exp.Run, strings.Join(parts, ", "))
			if j.Err != "" {
				detail += " — hata: " + j.Err
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

func (s *Server) backupFiles(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	out, err := s.Backups.BackupFiles(r.Context(), spec.Name, r.PathValue("run"), r.URL.Query().Get("path"))
	if err != nil {
		backupErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// backupFileDownload streams one file of a backup. Nothing is written before restic has
// found it; a failure after that cuts the response short of its length, so a browser
// never keeps a short file as whole.
func (s *Server) backupFileDownload(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.backupSite(w, r)
	if !ok {
		return
	}
	var actor Actor
	_ = json.Unmarshal([]byte(r.Header.Get(ActorHeader)), &actor)
	p := r.URL.Query().Get("path")
	run := r.PathValue("run")
	var size int64
	started := false
	err := s.Backups.StreamBackupFile(r.Context(), spec.Name, run, p, func(f BackupFile) io.Writer {
		size, started = f.Size, true
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Length", strconv.FormatInt(f.Size, 10))
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
		h.Set("Last-Modified", f.MTime.UTC().Format(http.TimeFormat))
		// A large file may take longer than the server's write timeout.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Hour))
		w.WriteHeader(http.StatusOK)
		return w
	})
	s.record(AuditEntry{Actor: actor, Action: "backup.download", Target: spec.Name,
		Detail: fmt.Sprintf("yedek %s: %s (%s)", run, p, humanBytes(size))}, err)
	if err == nil {
		return
	}
	if started {
		// The length was promised: a cut-off connection tells the browser.
		panic(http.ErrAbortHandler)
	}
	backupErr(w, err)
}
