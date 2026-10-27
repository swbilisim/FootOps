package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Moving sites from other servers (F4): source servers and each site's move.

// SourceView is a source with what the operator needs to let this server in.
type SourceView struct {
	remote.Source
	// Authorized is the line for ~/.ssh/authorized_keys on the source; Install is a
	// command that adds it there.
	Authorized string `json:"authorized"`
	Install    string `json:"install"`
	// UsedBy lists the sites whose move uses this source.
	UsedBy []string `json:"used_by,omitempty"`
}

// MigrationView is everything the wizard page shows for a site.
type MigrationView struct {
	Migration *Migration         `json:"migration,omitempty"`
	Source    *SourceView        `json:"source,omitempty"`
	Discovery *remote.Discovery  `json:"discovery,omitempty"`
	Scan      *remote.Scan       `json:"scan,omitempty"`
	Suggest   *remote.Suggestion `json:"suggest,omitempty"`
	Sources   []SourceView       `json:"sources"`
}

type CreateSourceRequest struct {
	Label string `json:"label"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
	User  string `json:"user"`
	Actor Actor  `json:"actor"`
}

type RemoveSourceRequest struct {
	// Cleanup removes this server's line from the source's authorized_keys first.
	Cleanup bool  `json:"cleanup"`
	Actor   Actor `json:"actor"`
}

type SetMigrationRequest struct {
	Choice MigrationChoice `json:"choice"`
	Actor  Actor           `json:"actor"`
}

func (s *Server) sourceView(src remote.Source, used map[string][]string) SourceView {
	info := publicAddrs()
	line := remote.AuthorizedLine(src, append(info.IPv4, info.IPv6...))
	return SourceView{Source: src, Authorized: line, Install: remote.InstallCommand(line), UsedBy: used[src.ID]}
}

// sourceUse maps each source to the sites whose move uses it.
func (s *Server) sourceUse() map[string][]string {
	used := map[string][]string{}
	list, _ := s.Sites.Store.List()
	for _, spec := range list {
		if m, err := s.Data.Migration(spec.Name); err == nil {
			used[m.Source] = append(used[m.Source], spec.Name)
		}
	}
	return used
}

func (s *Server) sourceViews() []SourceView {
	list, _ := s.Data.Sources.List()
	used := s.sourceUse()
	out := []SourceView{}
	for _, src := range list {
		out = append(out, s.sourceView(src, used))
	}
	return out
}

func sourceErr(w http.ResponseWriter, err error) {
	var ve *remote.ValidationError
	var ce *remote.ConnError
	switch {
	case errors.As(err, &ve):
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "kaynak bilgilerinde sorunlar var", Problems: ve.Problems})
	case errors.Is(err, remote.ErrNotFound), errors.Is(err, ErrNoMigration), errors.Is(err, sites.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.As(err, &ce), errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		writeErr(w, http.StatusBadRequest, err.Error())
	}
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.sourceViews())
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var req CreateSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	src, err := s.Data.Sources.Create(r.Context(), req.Label, req.Host, req.Port, req.User, req.Actor.User)
	s.record(AuditEntry{Actor: req.Actor, Action: "source.create", Target: req.Label,
		Detail: fmt.Sprintf("%s@%s:%d", req.User, req.Host, req.Port)}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.sourceView(src, nil))
}

// testSource connects to the source once (pinning its host key the first time).
func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	before, err := s.Data.Sources.Get(r.PathValue("id"))
	if err != nil {
		sourceErr(w, err)
		return
	}
	src, err := s.Data.Sources.Test(r.Context(), before.ID)
	detail := ""
	if err == nil {
		detail = strings.Join(src.HostKeys, ", ") + " · " + src.System
		if !before.Pinned() {
			detail = "sunucu anahtarı kaydedildi: " + detail
		}
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "source.test", Target: before.Label, Detail: detail}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.sourceView(src, s.sourceUse()))
}

func (s *Server) resetHostKey(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	before, err := s.Data.Sources.Get(r.PathValue("id"))
	if err != nil {
		sourceErr(w, err)
		return
	}
	src, err := s.Data.Sources.ResetHostKey(before.ID)
	s.record(AuditEntry{Actor: req.Actor, Action: "source.reset-hostkey", Target: before.Label,
		Detail: "önceki: " + strings.Join(before.HostKeys, ", ")}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.sourceView(src, s.sourceUse()))
}

// removeSource forgets a source and its key pair; with Cleanup it first removes this
// server's line from the source (if that fails, nothing is removed).
func (s *Server) removeSource(w http.ResponseWriter, r *http.Request) {
	var req RemoveSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	src, err := s.Data.Sources.Get(r.PathValue("id"))
	if err != nil {
		sourceErr(w, err)
		return
	}
	detail := "anahtar kaynakta bırakıldı (elle silinmeli)"
	if req.Cleanup {
		err = s.removeRemoteKey(r.Context(), src)
		detail = "anahtar kaynaktan silindi"
	}
	if err == nil {
		err = s.Data.Sources.Remove(src.ID)
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "source.remove", Target: src.Label, Detail: detail}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeRemoteKey(ctx context.Context, src remote.Source) error {
	if !src.Pinned() {
		return errors.New("bu kaynağa hiç bağlanılmadı; anahtar kaynakta eklendiyse elle sil (ya da kutuyu boş bırak)")
	}
	cmd, err := remote.RemoveCommand(src)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := s.Data.Sources.Output(ctx, src, 4096, cmd); err != nil {
		return fmt.Errorf("anahtar kaynaktan silinemedi: %w", err)
	}
	return nil
}

func (s *Server) getMigration(w http.ResponseWriter, r *http.Request) {
	spec, err := s.Sites.Store.Get(r.PathValue("name"))
	if errors.Is(err, sites.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := MigrationView{Sources: s.sourceViews()}
	m, err := s.Data.Migration(spec.Name)
	switch {
	case errors.Is(err, ErrNoMigration):
		writeJSON(w, http.StatusOK, v)
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v.Migration = &m
	for i := range v.Sources {
		if v.Sources[i].ID == m.Source {
			v.Source = &v.Sources[i]
		}
	}
	v.Discovery, v.Scan = s.Data.MigrationSaved(spec.Name)
	if v.Discovery != nil {
		sg := remote.Suggest(*v.Discovery, v.Scan, spec.Name, spec.Domains)
		v.Suggest = &sg
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) setMigration(w http.ResponseWriter, r *http.Request) {
	var req SetMigrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	site := r.PathValue("name")
	// Not while the site's data is being worked on (the copy reads the choice).
	release, err := s.Jobs.Hold(site)
	if err != nil {
		writeErr(w, http.StatusConflict, "bu sitede bir işlem sürüyor; bitince tekrar dene")
		return
	}
	defer release()
	m, err := s.Data.SetMigration(site, req.Choice, req.Actor.User)
	detail := ""
	if err == nil {
		detail = migrationText(m)
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "migrate.set", Target: site, Detail: detail}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// migrationText describes the picks for the audit log.
func migrationText(m Migration) string {
	parts := []string{"kaynak " + m.Source}
	if m.DB != "" {
		where := "sunucuda"
		if m.Container != "" {
			where = "container " + m.Container
		}
		parts = append(parts, fmt.Sprintf("veritabanı %s (%s)", m.DB, where))
	}
	if m.FilesDir != "" {
		parts = append(parts, "dosyalar "+m.FilesDir)
	}
	if m.AppDir != "" {
		parts = append(parts, "uygulama "+m.AppDir)
	}
	return strings.Join(parts, "; ")
}

func (s *Server) forgetMigration(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	site := r.PathValue("name")
	release, err := s.Jobs.Hold(site)
	if err != nil {
		writeErr(w, http.StatusConflict, "bu sitede bir işlem sürüyor; bitince tekrar dene")
		return
	}
	defer release()
	err = s.Data.ForgetMigration(site)
	s.record(AuditEntry{Actor: req.Actor, Action: "migrate.forget", Target: site}, err)
	if err != nil {
		sourceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// migrationJob starts one of the move's jobs on a site that has a move.
func (s *Server) migrationJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		if _, _, err := s.Data.migrationSource(spec.Name); err != nil {
			sourceErr(w, err)
			return
		}
		site := spec.Name
		var run func(ctx context.Context, t *Task) error
		detail := func(j Job) string { return j.Err }
		switch kind {
		case "migrate.discover":
			run = func(ctx context.Context, t *Task) error {
				_, err := s.Data.Discover(ctx, t, site)
				return err
			}
		case "migrate.scan":
			run = func(ctx context.Context, t *Task) error {
				_, err := s.Data.ScanApp(ctx, t, site)
				return err
			}
		case "migrate.copy":
			if req.Confirm != site { // rule K6, checked here as well as in the panel
				writeErr(w, http.StatusUnprocessableEntity, "onay için site adı aynen yazılmalı")
				return
			}
			var res CopyResult
			run = func(ctx context.Context, t *Task) error {
				var err error
				res, err = s.Data.MigrateCopy(ctx, t, site, req.Actor)
				return err
			}
			detail = func(j Job) string { return copyText(res, j) }
		}
		id, err := s.Jobs.Start(JobSpec{Kind: kind, Target: site, Actor: req.Actor, Cancellable: kind != "migrate.copy"}, run,
			func(j Job) { s.recordJobDetail(j, detail(j)) })
		s.jobResponse(w, id, err)
	}
}

// copyText describes a copy for the audit log: counts, never values.
func copyText(res CopyResult, j Job) string {
	var parts []string
	if res.DB != "" {
		parts = append(parts, fmt.Sprintf("veritabanı %s: kaynakta %d koleksiyon / %d belge, sitede %d belge",
			res.DB, len(res.SourceDocs), res.SourceDocTotal(), res.SiteDocTotal()))
	}
	if res.FilesDir != "" && res.SourceFiles != nil {
		got := "?"
		if res.SiteFiles != nil {
			got = fmt.Sprintf("%d / %s", res.SiteFiles.Files, humanBytes(res.SiteFiles.Bytes))
		}
		parts = append(parts, fmt.Sprintf("dosyalar %s: kaynakta %d / %s, sitede %s", res.FilesDir,
			res.SourceFiles.Files, humanBytes(res.SourceFiles.Bytes), got))
	}
	out := strings.Join(parts, "; ") + "; geri alma: " + orDash(j.Result)
	if j.Err != "" {
		out += " — hata: " + j.Err
	}
	return out
}
