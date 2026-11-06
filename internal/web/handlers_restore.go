package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
)

// Restoring from backups (Faz Y3, panel-tasarim §27.4): a site's backups, listed from
// the agent's index of the repository; one backup's page; its database opened for a
// look (the database browser's pages, read only); a restore with its preview (the site
// now beside the backup) and the site's name to confirm; an archive of it to download;
// its files a folder at a time, one file to download. The agent does the work.

// backupPointsShown is how many backups a site's page lists before "all".
const backupPointsShown = 15

// backupRun reads the {run} of a backup's address: a run's job, or a snapshot's id.
func backupRun(w http.ResponseWriter, r *http.Request) (string, bool) {
	run := r.PathValue("run")
	if !reJobID.MatchString(run) {
		http.NotFound(w, r)
		return "", false
	}
	return run, true
}

func backupURL(site, run, rest string) string { return "/sites/" + site + "/backups/" + run + rest }

// backupGone sends a request about a backup the agent no longer lists back to the
// site's backups, with why.
func (s *Server) backupGone(w http.ResponseWriter, r *http.Request, site string, err error) bool {
	if agentStatus(err) != http.StatusNotFound {
		return false
	}
	s.sessions.setFlash(current(r).ID, err.Error())
	http.Redirect(w, r, "/sites/"+site+"/backup#yedekler", http.StatusSeeOther)
	return true
}

// backupPart is a backup's part as the pages show it. Documents are what mongodump
// counted (its record lists views as empty collections: only the opened backup tells
// collections from views).
type backupPart struct {
	Part      string
	Snap      agent.BackupSnapshot
	Rec       *agent.BackupPart // the run's record, while kept
	Documents int64
}

func backupParts(d agent.BackupDetail) []backupPart {
	var out []backupPart
	for _, p := range []string{agent.PartDB, agent.PartFiles, agent.PartDef} {
		snap, ok := d.Parts[p]
		if !ok {
			continue
		}
		bp := backupPart{Part: p, Snap: snap}
		for i := range d.Record {
			if rec := d.Record[i]; rec.Part == p && rec.Snapshot == snap.ID {
				bp.Rec = &d.Record[i]
				for c, n := range rec.Collections {
					if !strings.HasPrefix(c, "system.") {
						bp.Documents += n
					}
				}
			}
		}
		out = append(out, bp)
	}
	return out
}

func (s *Server) backupListRefresh(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	job, err := s.agent.ListBackups(r.Context(), st.Spec.Name, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Yedek listesi okunamadı: "+err.Error())
		http.Redirect(w, r, "/sites/"+st.Spec.Name+"/backup#yedekler", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

// backupPoint is one backup's page: what it holds and what can be done with it.
func (s *Server) backupPoint(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	d, err := s.agent.SiteBackup(r.Context(), st.Spec.Name, run)
	if err != nil {
		if !s.backupGone(w, r, st.Spec.Name, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	s.render(w, r, "backup_point", map[string]any{"Title": st.Spec.Name + " yedeği", "S": st.Spec, "D": d, "Parts": backupParts(d)})
}

func (s *Server) backupOpen(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	job, err := s.agent.OpenBackup(r.Context(), st.Spec.Name, run, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Yedek açılamadı: "+err.Error())
		http.Redirect(w, r, backupURL(st.Spec.Name, run, ""), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

// backupClose closes the backup's opened database (its temporary database is removed).
func (s *Server) backupClose(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	msg := "İnceleme kapatıldı; geçici veritabanı silindi."
	d, err := s.agent.SiteBackup(r.Context(), st.Spec.Name, run)
	switch {
	case err != nil:
		msg = "Kapatılamadı: " + err.Error()
	case d.View == nil:
		msg = "Bu yedek zaten açık değil."
	default:
		if err := s.agent.CloseView(r.Context(), st.Spec.Name, d.View.ID, s.actor(r)); err != nil {
			msg = "Kapatılamadı: " + err.Error()
		}
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, backupURL(st.Spec.Name, run, ""), http.StatusSeeOther)
}

// restoreRow is a collection on the restore page: in the backup, now, and what a
// restore does to it.
type restoreRow struct {
	Name            string
	Backup, Now     int64
	InBackup, InNow bool
	Note            string
	Warn            bool
}

func restoreRows(v *agent.BackupView, cur agent.BackupCurrent) []restoreRow {
	if v == nil {
		return nil
	}
	names := map[string]bool{}
	for c := range v.Collections {
		names[c] = true
	}
	for c := range cur.Collections {
		names[c] = true
	}
	var out []restoreRow
	for c := range names {
		b, inB := v.Collections[c]
		n, inN := cur.Collections[c]
		row := restoreRow{Name: c, Backup: b, Now: n, InBackup: inB, InNow: inN}
		switch {
		case inN && n < 0:
			row.Note = "şu anki sayı okunamadı"
		case !inN:
			row.Note = "yeni: yedekten gelir"
		case !inB:
			row.Note, row.Warn = "yalnız sitede: silinir", true
		case b != n:
			row.Note, row.Warn = "farklı: yedekteki hali gelir", true
		default:
			row.Note = "aynı sayıda"
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// backupRestoreForm is the restore form as sent. AcceptDiff accepts that the opened
// backup differs from what was counted when it was taken.
type backupRestoreForm struct {
	DB, Files, AcceptDiff bool
	Confirm               string
}

func (s *Server) backupRestorePage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	cmp, err := s.agent.BackupCompare(r.Context(), st.Spec.Name, run)
	if err != nil {
		if !s.backupGone(w, r, st.Spec.Name, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	// What was chosen: the database once it is open for a look, the files when asked.
	f := backupRestoreForm{DB: cmp.Backup.View != nil, Files: r.URL.Query().Get("part") == "files"}
	s.renderRestore(w, r, http.StatusOK, st, cmp, f, nil)
}

func (s *Server) renderRestore(w http.ResponseWriter, r *http.Request, status int, st agent.SiteStatus, cmp agent.BackupCompare,
	f backupRestoreForm, problems []string) {
	_, hasDB := cmp.Backup.Parts[agent.PartDB]
	files, hasFiles := cmp.Backup.Parts[agent.PartFiles]
	s.renderStatus(w, r, status, "backup_restore", map[string]any{"Title": st.Spec.Name + " — yedekten geri yükle", "S": st.Spec,
		"B": cmp.Backup, "Cur": cmp.Current, "Rows": restoreRows(cmp.Backup.View, cmp.Current), "F": f, "Problems": problems,
		"CanDB": hasDB && st.Spec.Database, "CanFiles": hasFiles && st.Spec.DataMount != "", "FilesSnap": files})
}

func (s *Server) backupRestorePost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	f := backupRestoreForm{DB: r.PostFormValue("db") == "on", Files: r.PostFormValue("files") == "on",
		AcceptDiff: r.PostFormValue("accept_diff") == "on", Confirm: r.PostFormValue("confirm")}
	// The backup alone is enough to check the form: the site's own counts are read again
	// only when the form has to be shown again.
	d, err := s.agent.SiteBackup(r.Context(), st.Spec.Name, run)
	if err != nil {
		if !s.backupGone(w, r, st.Spec.Name, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	again := func(status int, problems []string) {
		cmp, err := s.agent.BackupCompare(r.Context(), st.Spec.Name, run)
		if err != nil {
			cmp = agent.BackupCompare{Backup: d, Current: agent.BackupCurrent{DBErr: err.Error(), FilesErr: err.Error()}}
		}
		s.renderRestore(w, r, status, st, cmp, f, problems)
	}
	var problems []string
	switch {
	case !f.DB && !f.Files:
		problems = append(problems, "Geri yüklenecek bir şey seç: veritabanı, dosyalar ya da ikisi.")
	case f.DB && d.View == nil:
		problems = append(problems, "Veritabanı için önce yedeği aç: sayımlar (önizleme) orada çıkar.")
	case f.DB && d.View.RecordDiff != "" && !f.AcceptDiff:
		problems = append(problems, "Açılan yedeğin sayımları yedek alınırken sayılanla tutmuyor: yine de geri yüklemek için farkı onayla.")
	}
	if strings.TrimSpace(f.Confirm) != st.Spec.Name {
		problems = append(problems, "Onay için site adını aynen yaz.")
	}
	if len(problems) > 0 {
		again(http.StatusUnprocessableEntity, problems)
		return
	}
	req := agent.BackupRestore{Run: run, DB: f.DB, Files: f.Files, AcceptDiff: f.DB && f.AcceptDiff}
	if f.DB {
		req.View = d.View.ID
	}
	job, err := s.agent.RestoreBackup(r.Context(), st.Spec.Name, req, st.Spec.Name, s.actor(r))
	if err != nil {
		again(agentStatus(err), problemsOf(err))
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

func (s *Server) backupExportPost(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	req := agent.BackupExport{Run: run, DB: r.PostFormValue("db") == "on", Files: r.PostFormValue("files") == "on",
		Env: r.PostFormValue("env") == "on"}
	back := backupURL(st.Spec.Name, run, "#indir")
	if !req.DB && !req.Files {
		s.sessions.setFlash(current(r).ID, "Arşive veritabanı ya da dosyalar seç.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	job, err := s.agent.ExportBackup(r.Context(), st.Spec.Name, req, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Arşiv hazırlanamadı: "+err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
}

// backupCrumb is one folder of the way to a folder of a backup's files.
type backupCrumb struct{ Name, Path string }

func backupCrumbs(dir string) []backupCrumb {
	var out []backupCrumb
	if dir == "" {
		return out
	}
	acc := ""
	for _, part := range strings.Split(dir, "/") {
		acc = strings.TrimPrefix(acc+"/"+part, "/")
		out = append(out, backupCrumb{Name: part, Path: acc})
	}
	return out
}

func (s *Server) backupFilesPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	dir := strings.Trim(r.URL.Query().Get("path"), "/")
	if len(dir) > 4096 || strings.ContainsAny(dir, "\x00\r\n") {
		http.NotFound(w, r)
		return
	}
	d, err := s.agent.SiteBackup(r.Context(), st.Spec.Name, run)
	if err != nil {
		if !s.backupGone(w, r, st.Spec.Name, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return
	}
	data := map[string]any{"Title": st.Spec.Name + " yedeğindeki dosyalar", "S": st.Spec, "D": d, "Dir": dir, "Crumbs": backupCrumbs(dir)}
	status := http.StatusOK
	if folder, err := s.agent.BackupFiles(r.Context(), st.Spec.Name, run, dir); err != nil {
		data["Err"], status = err.Error(), agentStatus(err)
	} else {
		data["L"] = folder
	}
	s.renderStatus(w, r, status, "backup_files", data)
}

func (s *Server) backupFileDownload(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	run, ok := backupRun(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" || len(p) > 4096 {
		http.NotFound(w, r)
		return
	}
	resp, err := s.agent.DownloadBackupFile(r.Context(), st.Spec.Name, run, p, s.actor(r))
	if err != nil {
		var ae *agent.Error
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 600 && ae.Status != http.StatusBadGateway {
			http.Error(w, ae.Message, ae.Status)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	relay(w, resp)
}

// ---- an opened backup's database (the browser's pages, read only) ----

// viewSource is the database of a backup open for a look; a backup that is not open
// sends the browser back to its page.
func (s *Server) viewSource(w http.ResponseWriter, r *http.Request) (agent.SiteStatus, dbSource, bool) {
	st, ok := s.dbSiteOr404(w, r)
	if !ok {
		return st, dbSource{}, false
	}
	run, ok := backupRun(w, r)
	if !ok {
		return st, dbSource{}, false
	}
	d, err := s.agent.SiteBackup(r.Context(), st.Spec.Name, run)
	if err != nil {
		if !s.backupGone(w, r, st.Spec.Name, err) {
			http.Error(w, err.Error(), agentStatus(err))
		}
		return st, dbSource{}, false
	}
	if d.View == nil {
		s.sessions.setFlash(current(r).ID, "Bu yedek incelemeye açık değil (süresi dolmuş ya da kapatılmış olabilir): yeniden aç.")
		http.Redirect(w, r, backupURL(st.Spec.Name, run, ""), http.StatusSeeOther)
		return st, dbSource{}, false
	}
	name, view := st.Spec.Name, d.View.ID
	return st, dbSource{Root: name + "/backups/" + run, View: d.View, Run: run,
		collections: func(ctx context.Context) (agent.DBOverview, error) { return s.agent.ViewCollections(ctx, name, view) },
		find: func(ctx context.Context, q agent.DBQuery) (agent.DBPage, error) {
			return s.agent.ViewFind(ctx, name, view, q)
		},
		doc: func(ctx context.Context, coll, id string) (agent.DBDocument, error) {
			return s.agent.ViewDoc(ctx, name, view, coll, id)
		}}, true
}

func (s *Server) viewDBPage(w http.ResponseWriter, r *http.Request) {
	if st, src, ok := s.viewSource(w, r); ok {
		s.dbPageOf(w, r, st, src)
	}
}

func (s *Server) viewDBColl(w http.ResponseWriter, r *http.Request) {
	if st, src, ok := s.viewSource(w, r); ok {
		s.dbCollOf(w, r, st, src)
	}
}

func (s *Server) viewDBDoc(w http.ResponseWriter, r *http.Request) {
	if st, src, ok := s.viewSource(w, r); ok {
		s.dbDocOf(w, r, st, src)
	}
}

// kindText names a backup by its run's kind.
func kindText(kind string) string {
	switch kind {
	case agent.RunDaily:
		return "gece yedeği"
	case agent.RunHourly:
		return "saatlik yedek"
	case agent.RunManual:
		return "elle alınan yedek"
	}
	return "yedek"
}

// partsText is a backup's parts in words: "veritabanı, dosyalar, site tanımı".
func partsText(parts map[string]agent.BackupSnapshot) string {
	var out []string
	for _, p := range []string{agent.PartDB, agent.PartFiles, agent.PartDef} {
		if _, ok := parts[p]; ok {
			out = append(out, partNames[p])
		}
	}
	return strings.Join(out, ", ")
}

// filesQuery is the address of a folder of a backup's files.
func filesQuery(site, run, dir string) string {
	u := backupURL(site, run, "/files")
	if dir != "" {
		u += "?" + url.Values{"path": {dir}}.Encode()
	}
	return u
}
