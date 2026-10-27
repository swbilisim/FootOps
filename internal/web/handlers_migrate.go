package web

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/remote"
)

// Moving a site from another server (F4a): one page per site walks through the source
// server, what was found on it and the copy. The agent does the work; this process only
// shows it and passes the operator's choices on.

var reSourceID = regexp.MustCompile(`^[a-z0-9]{12}$`)

// dbOption is one database the source has, as a choice.
type dbOption struct {
	Value, Label string
	Suggested    bool
}

// dbValue encodes a database choice: "<container>|<name>" ("" container: on the host).
func dbValue(container, name string) string { return container + "|" + name }

func dbOptions(v agent.MigrationView) []dbOption {
	var out []dbOption
	if v.Discovery != nil {
		for _, m := range v.Discovery.Mongo {
			where := "sunucuda"
			if m.Via == "docker" {
				where = "container " + m.Container
			}
			for _, db := range m.DBs {
				if !remote.ValidDB(db.Name) {
					continue
				}
				o := dbOption{Value: dbValue(m.Container, db.Name),
					Label: fmt.Sprintf("%s — %d koleksiyon, %d belge, %s (%s)", db.Name, db.Collections, db.Objects, humanSize(db.DataSize), where)}
				if s := v.Suggest; s != nil && s.DB != nil && s.DB.Name == db.Name && s.DB.Container == m.Container {
					o.Suggested = true
					o.Label += " — önerilen"
				}
				out = append(out, o)
			}
		}
	}
	// The current choice stays selectable even if the last discovery did not list it.
	if m := v.Migration; m != nil && m.DB != "" {
		cur := dbValue(m.Container, m.DB)
		for _, o := range out {
			if o.Value == cur {
				return out
			}
		}
		out = append(out, dbOption{Value: cur, Label: m.DB + " (seçili; son taramada görülmedi)"})
	}
	return out
}

// migrateForm is the "what to move" form: the current choice, else the suggestion.
type migrateForm struct {
	DB, FilesDir, AppDir string
}

func formOf(v agent.MigrationView) migrateForm {
	var f migrateForm
	if m := v.Migration; m != nil && (m.DB != "" || m.FilesDir != "" || m.AppDir != "") {
		f.FilesDir, f.AppDir = m.FilesDir, m.AppDir
		if m.DB != "" {
			f.DB = dbValue(m.Container, m.DB)
		}
		return f
	}
	if s := v.Suggest; s != nil {
		f.FilesDir, f.AppDir = s.FilesDir, s.AppDir
		if s.DB != nil {
			f.DB = dbValue(s.DB.Container, s.DB.Name)
		}
	}
	return f
}

// sourceStats sums a discovery's database for the copy summary.
func chosenDB(v agent.MigrationView) *remote.MongoDB {
	if v.Discovery == nil || v.Migration == nil || v.Migration.DB == "" {
		return nil
	}
	for _, m := range v.Discovery.Mongo {
		for i, db := range m.DBs {
			if db.Name == v.Migration.DB && m.Container == v.Migration.Container {
				return &m.DBs[i]
			}
		}
	}
	return nil
}

func chosenFiles(v agent.MigrationView) *remote.Candidate {
	if v.Discovery == nil || v.Migration == nil || v.Migration.FilesDir == "" {
		return nil
	}
	for i, c := range v.Discovery.Candidates {
		if c.Dir == v.Migration.FilesDir {
			return &v.Discovery.Candidates[i]
		}
	}
	return nil
}

// countRow is one line of the copy result table.
type countRow struct {
	Name         string
	Source, Site int64
}

func copyRows(c *agent.CopyResult) []countRow {
	if c == nil {
		return nil
	}
	names := map[string]bool{}
	for k := range c.SourceDocs {
		names[k] = true
	}
	for k := range c.SiteDocs {
		names[k] = true
	}
	var out []countRow
	for k := range names {
		out = append(out, countRow{Name: k, Source: c.SourceDocs[k], Site: c.SiteDocs[k]})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func (s *Server) migratePage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	s.renderMigrate(w, r, st, http.StatusOK, nil, nil)
}

func (s *Server) renderMigrate(w http.ResponseWriter, r *http.Request, st agent.SiteStatus, status int, problems []string, form *migrateForm) {
	v, err := s.agent.Migration(r.Context(), st.Spec.Name)
	data := map[string]any{"Title": st.Spec.Name + " taşıma", "S": st.Spec, "V": v, "Err": errString(err), "Problems": problems,
		"Change": r.URL.Query().Get("kaynak") == "degistir" || v.Source == nil, "DBs": dbOptions(v),
		"ChosenDB": chosenDB(v), "ChosenFiles": chosenFiles(v)}
	if form == nil {
		f := formOf(v)
		form = &f
	}
	data["F"] = form
	if v.Migration != nil {
		data["Rows"] = copyRows(v.Migration.LastCopy())
	}
	s.renderStatus(w, r, status, "migrate", data)
}

func (s *Server) toMigrate(w http.ResponseWriter, r *http.Request, site, anchor, flash string) {
	if flash != "" {
		s.sessions.setFlash(current(r).ID, flash)
	}
	http.Redirect(w, r, "/sites/"+site+"/migrate"+anchor, http.StatusSeeOther)
}

// problemsOf turns an agent error into lines for the form.
func problemsOf(err error) []string {
	var ae *agent.Error
	if errors.As(err, &ae) && len(ae.Problems) > 0 {
		return ae.Problems
	}
	return []string{err.Error()}
}

// migrateSource picks a registered source for the site, or registers a new one.
func (s *Server) migrateSource(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	id := r.PostFormValue("source")
	if r.PostFormValue("new") == "1" {
		port, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("port")))
		if err != nil {
			port = 0
		}
		src, err := s.agent.CreateSource(r.Context(), r.PostFormValue("label"), strings.TrimSpace(r.PostFormValue("host")), port,
			strings.TrimSpace(r.PostFormValue("user")), s.actor(r))
		if err != nil {
			s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, problemsOf(err), nil)
			return
		}
		id = src.ID
	}
	if !reSourceID.MatchString(id) {
		s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, []string{"Bir kaynak sunucu seç ya da yenisini ekle."}, nil)
		return
	}
	if _, err := s.agent.SetMigration(r.Context(), st.Spec.Name, agent.MigrationChoice{Source: id, SourceOnly: true}, s.actor(r)); err != nil {
		s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, problemsOf(err), nil)
		return
	}
	s.toMigrate(w, r, st.Spec.Name, "#kaynak", "")
}

// migrateTest connects to the site's source once (the first time pinning its key).
func (s *Server) migrateTest(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	v, err := s.agent.Migration(r.Context(), st.Spec.Name)
	if err != nil || v.Source == nil {
		s.toMigrate(w, r, st.Spec.Name, "#kaynak", "Önce kaynak sunucuyu seç.")
		return
	}
	src, err := s.agent.TestSource(r.Context(), v.Source.ID, s.actor(r))
	if err != nil {
		s.toMigrate(w, r, st.Spec.Name, "#kaynak", "Bağlanılamadı: "+err.Error())
		return
	}
	s.toMigrate(w, r, st.Spec.Name, "#kaynak", fmt.Sprintf("Bağlantı tamam: %s (%s).", orNone(src.System), strings.Join(src.HostKeys, ", ")))
}

func orNone(s string) string {
	if s == "" {
		return "sistem bilgisi yok"
	}
	return s
}

// migrateChoose saves what to move.
func (s *Server) migrateChoose(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	f := migrateForm{DB: r.PostFormValue("db"), FilesDir: strings.TrimSpace(r.PostFormValue("files_dir")),
		AppDir: strings.TrimSpace(r.PostFormValue("app_dir"))}
	ch := agent.MigrationChoice{FilesDir: f.FilesDir, AppDir: f.AppDir}
	if f.DB != "" {
		c, n, ok := strings.Cut(f.DB, "|")
		if !ok {
			s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, []string{"Geçersiz veritabanı seçimi."}, &f)
			return
		}
		ch.Container, ch.DB = c, n
	}
	if _, err := s.agent.SetMigration(r.Context(), st.Spec.Name, ch, s.actor(r)); err != nil {
		s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, problemsOf(err), &f)
		return
	}
	s.toMigrate(w, r, st.Spec.Name, "#kopya", "Seçimler kaydedildi.")
}

// migrateJob starts the source's discovery, the app folder's scan or the copy.
func (s *Server) migrateJob(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.dataSiteOr404(w, r)
		if !ok {
			return
		}
		confirm := ""
		if verb == "copy" {
			confirm = r.PostFormValue("confirm")
			if confirm != st.Spec.Name {
				s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, []string{"Onay için site adını aynen yaz."}, nil)
				return
			}
		}
		job, err := s.agent.MigrationJob(r.Context(), st.Spec.Name, verb, confirm, s.actor(r))
		if err != nil {
			s.renderMigrate(w, r, st, http.StatusUnprocessableEntity, problemsOf(err), nil)
			return
		}
		http.Redirect(w, r, "/jobs/"+job, http.StatusSeeOther)
	}
}

func (s *Server) migrateForget(w http.ResponseWriter, r *http.Request) {
	st, ok := s.dataSiteOr404(w, r)
	if !ok {
		return
	}
	if err := s.agent.ForgetMigration(r.Context(), st.Spec.Name, s.actor(r)); err != nil {
		s.toMigrate(w, r, st.Spec.Name, "", "Unutulamadı: "+err.Error())
		return
	}
	s.sessions.setFlash(current(r).ID, "Taşıma kaydı silindi; siteye kopyalanan veri yerinde duruyor.")
	http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
}

// ---- source servers ----

func (s *Server) sourcesPage(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.Sources(r.Context())
	s.render(w, r, "sources", map[string]any{"Title": "Kaynak sunucular", "Sources": list, "Err": errString(err)})
}

// sourceOf finds a source by the path's id.
func (s *Server) sourceOf(w http.ResponseWriter, r *http.Request) (agent.SourceView, bool) {
	id := r.PathValue("id")
	if !reSourceID.MatchString(id) {
		http.NotFound(w, r)
		return agent.SourceView{}, false
	}
	list, err := s.agent.Sources(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return agent.SourceView{}, false
	}
	for _, src := range list {
		if src.ID == id {
			return src, true
		}
	}
	http.NotFound(w, r)
	return agent.SourceView{}, false
}

func (s *Server) sourceTest(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceOf(w, r)
	if !ok {
		return
	}
	msg := ""
	if got, err := s.agent.TestSource(r.Context(), src.ID, s.actor(r)); err != nil {
		msg = src.Label + ": bağlanılamadı: " + err.Error()
	} else {
		msg = fmt.Sprintf("%s: bağlantı tamam (%s).", src.Label, orNone(got.System))
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}

// sourceConfirm shows the confirmation for removing a source or forgetting its key.
func (s *Server) sourceConfirm(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		src, ok := s.sourceOf(w, r)
		if !ok {
			return
		}
		s.render(w, r, "source_confirm", map[string]any{"Title": src.Label, "Src": src, "Action": action})
	}
}

func (s *Server) sourceRemove(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceOf(w, r)
	if !ok {
		return
	}
	cleanup := r.PostFormValue("cleanup") == "on"
	if err := s.agent.RemoveSource(r.Context(), src.ID, cleanup, s.actor(r)); err != nil {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "source_confirm",
			map[string]any{"Title": src.Label, "Src": src, "Action": "remove", "Problem": err.Error()})
		return
	}
	msg := src.Label + " kaldırıldı; anahtarı bu sunucudan silindi."
	if cleanup {
		msg += " Kaynaktaki authorized_keys satırı da silindi."
	} else {
		msg += " Kaynaktaki authorized_keys satırını elle sil."
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}

func (s *Server) sourceReset(w http.ResponseWriter, r *http.Request) {
	src, ok := s.sourceOf(w, r)
	if !ok {
		return
	}
	if _, err := s.agent.ResetHostKey(r.Context(), src.ID, s.actor(r)); err != nil {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "source_confirm",
			map[string]any{"Title": src.Label, "Src": src, "Action": "reset", "Problem": err.Error()})
		return
	}
	s.sessions.setFlash(current(r).ID, src.Label+": kayıtlı sunucu anahtarı silindi; bir sonraki bağlantı yeni anahtarı kaydeder.")
	http.Redirect(w, r, "/sources", http.StatusSeeOther)
}
