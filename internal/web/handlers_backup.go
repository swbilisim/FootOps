package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

// The backup pages (Faz Y2, panel-tasarim §27): connecting the server to its S3
// repository, the repository password shown until it is confirmed as kept elsewhere, the
// schedule and policies, and the runs. The agent does the work; keys and passwords pass
// through here only inside the request that carries them.

// backupRunsShown is how many runs the backup pages list.
const backupRunsShown = 20

// backupSetupForm keeps what was typed when a setup is refused; never a key or password.
type backupSetupForm struct {
	Endpoint, Bucket, KeyID string
	// Mode, replacing a repository: "same" (a new key or address for the same
	// repository) or "new" (a new, empty repository with a new password).
	Mode string
}

// backupGeneralForm is the general settings form as typed.
type backupGeneralForm struct {
	Night, DB, Files, KeepHours, KeepDays, KeepMonths, WarnGB string
	Paused                                                    bool
}

func generalFormOf(s agent.BackupSettings) backupGeneralForm {
	warn := s.WarnGB
	if warn <= 0 {
		warn = agent.DefaultWarnGB
	}
	d := s.Defaults
	return backupGeneralForm{Night: s.Night, DB: d.DB, Files: d.Files, KeepHours: strconv.Itoa(d.KeepHours),
		KeepDays: strconv.Itoa(d.KeepDays), KeepMonths: strconv.Itoa(d.KeepMonths), WarnGB: strconv.Itoa(warn), Paused: s.Paused}
}

// backupSiteForm is a site's settings form as typed; an empty field takes the general
// setting.
type backupSiteForm struct {
	On                                         bool
	DB, Files, KeepHours, KeepDays, KeepMonths string
	Freeze                                     bool
}

func siteFormOf(p agent.BackupPolicy) backupSiteForm {
	n := func(v int) string {
		if v == 0 {
			return ""
		}
		return strconv.Itoa(v)
	}
	return backupSiteForm{On: !p.Off, DB: p.DB, Files: p.Files, KeepHours: n(p.KeepHours), KeepDays: n(p.KeepDays),
		KeepMonths: n(p.KeepMonths), Freeze: p.Freeze}
}

// backupRow is a site on the backup pages: how often each part is backed up and when it
// last was.
type backupRow struct {
	Name  string
	Off   bool
	Own   bool // the site has settings of its own
	Pol   agent.BackupPolicy
	Cells []backupCell // database, files, definition
}

// backupCell is one part of a site: Plan is "" when the site has no such part (no
// database, no data directory).
type backupCell struct {
	Part   string
	Plan   string
	Last   time.Time
	Failed *agent.BackupFailure
}

var (
	dbPlans    = map[string]string{agent.DBDaily: "günlük", agent.DB6h: "6 saatte bir", agent.DBHourly: "saatlik"}
	filesPlans = map[string]string{agent.FilesDaily: "günlük", agent.FilesWeekly: "haftalık", agent.FilesOff: "kapalı"}
	runKinds   = map[string]string{agent.RunDaily: "gece", agent.RunHourly: "saatlik", agent.RunManual: "elle"}
	partNames  = map[string]string{agent.PartDB: "veritabanı", agent.PartFiles: "dosyalar", agent.PartDef: "site tanımı", agent.PartServer: "sunucu ayarları"}
	partTitles = map[string]string{agent.PartDB: "Veritabanı", agent.PartFiles: "Dosyalar", agent.PartDef: "Site tanımı", agent.PartServer: "Sunucu ayarları"}
)

func backupRowOf(st agent.BackupStatus, ss agent.SiteStatus) backupRow {
	sp := ss.Spec
	pol := st.Settings.For(sp.Name)
	_, own := st.Settings.Sites[sp.Name]
	cells := []backupCell{{Part: agent.PartDB}, {Part: agent.PartFiles}, {Part: agent.PartDef, Plan: "günlük"}}
	if sp.Database {
		cells[0].Plan = dbPlans[pol.DB]
	}
	if sp.DataMount != "" {
		cells[1].Plan = filesPlans[pol.Files]
	}
	for i := range cells {
		key := sp.Name + "/" + cells[i].Part
		cells[i].Last = st.Last[key].Time
		if f, ok := st.Failed[key]; ok {
			cells[i].Failed = &f
		}
	}
	return backupRow{Name: sp.Name, Off: pol.Off, Own: own, Pol: pol, Cells: cells}
}

// siteBackupSummary is the line on a site's page about its backups.
type siteBackupSummary struct {
	Configured, Ready bool // a repository; its password confirmed too
	Row               backupRow
}

// runView is a run as the pages list it; on a site's page only that site's parts.
type runView struct {
	ID, Kind, Actor string
	Started         time.Time
	Took            time.Duration
	Parts           []agent.BackupPart
	Added           int64
	// Status is ok, warn or bad; Note says why when it is not ok.
	Status, Note    string
	Removed         int
	Pruned, Checked bool
	RepoSize        int64
}

func runViews(list []agent.BackupRun, site string) []runView {
	out := make([]runView, 0, len(list))
	for _, r := range list {
		v := runView{ID: r.ID, Kind: r.Kind, Actor: r.Actor, Started: r.Started, Took: r.Finished.Sub(r.Started),
			Removed: r.Removed, Pruned: r.Pruned, Checked: r.Checked, RepoSize: r.RepoSize, Status: "ok"}
		var failed, warned string
		for _, p := range r.Parts {
			if site != "" && p.Site != site {
				continue
			}
			v.Parts = append(v.Parts, p)
			v.Added += p.AddedPacked
			switch {
			case p.Err != "" && failed == "":
				failed = p.Err
			case p.Warning != "" && warned == "":
				warned = p.Warning
			}
		}
		switch {
		case r.Err == context.Canceled.Error():
			v.Status, v.Note = "warn", "iptal edildi"
		case r.Err == context.DeadlineExceeded.Error():
			v.Status, v.Note = "bad", "süre doldu"
		case failed != "":
			v.Status, v.Note = "bad", failed
		case r.Err != "" && (site == "" || r.Site == site):
			v.Status, v.Note = "bad", r.Err
		case warned != "":
			v.Status, v.Note = "warn", warned
		}
		out = append(out, v)
	}
	return out
}

// backupPage is the server's backup page: setting the repository up, the password
// until it is confirmed, then the schedule, the sites and the runs.
func (s *Server) backupPage(w http.ResponseWriter, r *http.Request) {
	s.renderBackup(w, r, http.StatusOK, nil)
}

func (s *Server) renderBackup(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	st, err := s.agent.BackupStatus(r.Context())
	data := map[string]any{"Title": "Yedekleme", "St": st, "Err": errString(err), "BucketHint": "footops-" + strings.ToLower(s.cfg.ServerCode),
		"WarnGB": int(st.Settings.Warn() >> 30)}
	if err == nil {
		list, serr := s.agent.Sites(r.Context())
		var rows []backupRow
		for _, ss := range list {
			rows = append(rows, backupRowOf(st, ss))
		}
		data["Rows"], data["SitesErr"] = rows, errString(serr)
		data["G"] = generalFormOf(st.Settings)
		data["ServerLast"] = st.Last["_server/"+agent.PartServer].Time
		data["UsePct"] = float64(st.RepoSize) / float64(st.Settings.Warn()) * 100
		ready := st.Configured && !st.Settings.Confirmed.IsZero()
		data["Ready"] = ready
		if ready {
			runs, rerr := s.agent.BackupRuns(r.Context(), "", backupRunsShown)
			data["Runs"], data["RunsErr"] = runViews(runs, ""), errString(rerr)
		}
		data["Live"] = backupLive(r, st)
	}
	for k, v := range extra {
		data[k] = v
	}
	s.renderStatus(w, r, status, "backup", data)
}

// backupLive is how often a backup page updates itself: while a backup runs, and only
// when it was asked for with a GET — live.js fetches the page's own address again, and a
// form's answer (a POST, the password among them) must not be fetched in the
// background. A repository being set up changes the whole page once it is done: its
// job's page follows it instead.
func backupLive(r *http.Request, st agent.BackupStatus) any {
	if r.Method != http.MethodGet || st.Running == nil || st.Running.Kind != "backup.run" {
		return nil
	}
	return 5
}

// repoAddress is restic's address of a repository: the endpoint (https://… without a
// path) and the bucket, with any folder below it.
func repoAddress(endpoint, bucket string) (string, []string) {
	var p []string
	e := strings.TrimRight(strings.TrimPrefix(strings.TrimSpace(endpoint), "s3:"), "/")
	if e != "" && !strings.Contains(e, "://") {
		e = "https://" + e
	}
	u, err := url.Parse(e)
	switch {
	case e == "":
		p = append(p, "Uç nokta (S3 adresi) gerekli")
	case err != nil || u.Host == "":
		p = append(p, "Uç nokta https://<sunucu> biçiminde olmalı")
	case u.Path != "" || u.RawQuery != "":
		p = append(p, "Uç noktaya yalnız sunucu adresini yaz (https://….r2.cloudflarestorage.com); kovayı kendi alanına")
	}
	b := strings.Trim(strings.TrimSpace(bucket), "/")
	if b == "" {
		p = append(p, "Kova adı gerekli")
	}
	return "s3:" + e + "/" + b, p
}

// backupSetup connects the server to a repository (or replaces the one it has): the
// agent tries it as a job, makes it if there is none, and keeps the keys.
func (s *Server) backupSetup(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.BackupStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	f := backupSetupForm{Endpoint: strings.TrimSpace(r.PostFormValue("endpoint")), Bucket: strings.TrimSpace(r.PostFormValue("bucket")),
		KeyID: strings.TrimSpace(r.PostFormValue("key_id")), Mode: r.PostFormValue("mode")}
	replacing := st.Configured
	again := func(problems ...string) {
		if replacing {
			s.renderStatus(w, r, http.StatusUnprocessableEntity, "backup_repo", map[string]any{"Title": "Yedek deposunu değiştir",
				"St": st, "F": f, "Problems": problems, "BucketHint": "footops-" + strings.ToLower(s.cfg.ServerCode)})
			return
		}
		s.renderBackup(w, r, http.StatusUnprocessableEntity, map[string]any{"F": f, "Problems": problems})
	}
	repo, problems := repoAddress(f.Endpoint, f.Bucket)
	req := agent.BackupSetupRequest{BackupSetup: agent.BackupSetup{Repository: repo, KeyID: f.KeyID,
		SecretKey: r.PostFormValue("secret_key"), Password: r.PostFormValue("password")}, Actor: s.actor(r)}
	if replacing {
		// The same repository is opened with the password the agent has (no password
		// passes through here); a new one needs the current password once it was confirmed
		// (before, the agent does not ask: the panel could read it anyway).
		switch f.Mode {
		case "same":
			req.Keep, req.Password = true, ""
		case "new":
			req.Password, req.Current = "", strings.TrimSpace(r.PostFormValue("current"))
			if req.Current == "" && !st.Settings.Confirmed.IsZero() {
				problems = append(problems, "Yeni depoya geçmek için şu anki deponun şifresi gerekli")
			}
		default:
			problems = append(problems, "Aynı depo mu yeni depo mu, seç")
		}
	}
	if len(problems) > 0 {
		again(problems...)
		return
	}
	id, err := s.agent.BackupSetup(r.Context(), req)
	if err != nil {
		again(problemsOf(err)...)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// backupRepoPage is the form that replaces the repository: a new key for the same one,
// or a new, empty repository.
func (s *Server) backupRepoPage(w http.ResponseWriter, r *http.Request) {
	st, err := s.agent.BackupStatus(r.Context())
	if err == nil && !st.Configured {
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	s.render(w, r, "backup_repo", map[string]any{"Title": "Yedek deposunu değiştir", "St": st, "Err": errString(err),
		"F": backupSetupForm{Mode: "same"}, "BucketHint": "footops-" + strings.ToLower(s.cfg.ServerCode)})
}

// backupKey shows the repository password: a POST, so it never sits in a link, a
// history entry or a live refresh, and every look at it is in the audit log. The agent
// gives it to the panel only until it is confirmed.
func (s *Server) backupKey(w http.ResponseWriter, r *http.Request) {
	pw, err := s.agent.BackupKey(r.Context(), s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Şifre gösterilemedi: "+err.Error())
		http.Redirect(w, r, "/backup", http.StatusSeeOther)
		return
	}
	s.renderBackup(w, r, http.StatusOK, map[string]any{"Key": pw})
}

func (s *Server) backupConfirm(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("saved") != "on" {
		s.renderBackup(w, r, http.StatusUnprocessableEntity, map[string]any{"Problems": []string{"Önce şifreyi kaydettiğini işaretle."}})
		return
	}
	msg := "Onaylandı: zamanlanmış yedekler başladı. Gece işinin zamanı geçtiyse ilk yedek birkaç dakika içinde kendiliğinden başlar."
	if err := s.agent.BackupConfirm(r.Context(), s.actor(r)); err != nil {
		msg = "Onaylanamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// atoiField reads a whole number from a form field; empty is 0 when allowed.
func atoiField(v, what string, empty bool, problems *[]string) int {
	v = strings.TrimSpace(v)
	if v == "" && empty {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*problems = append(*problems, what+" sayı olmalı")
	}
	return n
}

// backupSettings saves the general settings; each site's own settings stay as they
// are.
func (s *Server) backupSettings(w http.ResponseWriter, r *http.Request) {
	g := backupGeneralForm{Night: strings.TrimSpace(r.PostFormValue("night")), DB: r.PostFormValue("db"), Files: r.PostFormValue("files"),
		KeepHours: r.PostFormValue("keep_hours"), KeepDays: r.PostFormValue("keep_days"), KeepMonths: r.PostFormValue("keep_months"),
		WarnGB: r.PostFormValue("warn_gb"), Paused: r.PostFormValue("paused") == "on"}
	cur, err := s.agent.BackupSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var problems []string
	cur.Night, cur.Paused = g.Night, g.Paused
	cur.Defaults.DB, cur.Defaults.Files = g.DB, g.Files
	cur.Defaults.KeepHours = atoiField(g.KeepHours, "Saatlik saklama", false, &problems)
	cur.Defaults.KeepDays = atoiField(g.KeepDays, "Günlük saklama", false, &problems)
	cur.Defaults.KeepMonths = atoiField(g.KeepMonths, "Aylık saklama", false, &problems)
	cur.WarnGB = atoiField(g.WarnGB, "Depo boyutu uyarısı", false, &problems)
	if cur.WarnGB == 0 && len(problems) == 0 {
		problems = append(problems, "Depo boyutu uyarısı 1-10000 GB arasında olmalı")
	}
	if len(problems) == 0 {
		if err := s.agent.SetBackupSettings(r.Context(), cur, s.actor(r)); err != nil {
			problems = problemsOf(err)
		}
	}
	if len(problems) > 0 {
		s.renderBackup(w, r, http.StatusUnprocessableEntity, map[string]any{"G": g, "SettingsProblems": problems})
		return
	}
	s.sessions.setFlash(current(r).ID, "Yedek ayarları kaydedildi.")
	http.Redirect(w, r, "/backup#ayarlar", http.StatusSeeOther)
}

// backupPause stops or resumes the timetable in one click.
func (s *Server) backupPause(w http.ResponseWriter, r *http.Request) {
	cur, err := s.agent.BackupSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	cur.Paused = r.PostFormValue("paused") == "on"
	msg := "Zamanlanmış yedekleme sürüyor."
	if cur.Paused {
		msg = "Zamanlanmış yedekleme duraklatıldı. “Şimdi yedekle” çalışmaya devam eder."
	}
	if err := s.agent.SetBackupSettings(r.Context(), cur, s.actor(r)); err != nil {
		msg = "Kaydedilemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/backup", http.StatusSeeOther)
}

// backupRun backs up now: every site and the server's settings, or one site.
func (s *Server) backupRun(w http.ResponseWriter, r *http.Request) {
	site, back := "", "/backup"
	if r.PathValue("name") != "" {
		st, ok := s.findSite(w, r)
		if !ok {
			return
		}
		site, back = st.Spec.Name, "/sites/"+st.Spec.Name+"/backup"
	}
	id, err := s.agent.BackupRun(r.Context(), site, s.actor(r))
	if err != nil {
		s.sessions.setFlash(current(r).ID, "Yedekleme başlatılamadı: "+err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// siteBackupPage is one site's backups: its settings over the general ones, its parts'
// last backups and its runs.
func (s *Server) siteBackupPage(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.findSite(w, r)
	if !ok {
		return
	}
	st, err := s.agent.BackupStatus(r.Context())
	s.renderSiteBackup(w, r, http.StatusOK, ss, st, err, siteFormOf(st.Settings.Sites[ss.Spec.Name]), nil)
}

func (s *Server) renderSiteBackup(w http.ResponseWriter, r *http.Request, status int, ss agent.SiteStatus, st agent.BackupStatus,
	err error, f backupSiteForm, problems []string) {
	name := ss.Spec.Name
	data := map[string]any{"Title": name + " yedekleri", "S": ss, "St": st, "Err": errString(err), "F": f, "Problems": problems}
	if err == nil {
		data["Row"] = backupRowOf(st, ss)
		data["Ready"] = st.Configured && !st.Settings.Confirmed.IsZero()
		runs, rerr := s.agent.BackupRuns(r.Context(), name, backupRunsShown)
		data["Runs"], data["RunsErr"] = runViews(runs, name), errString(rerr)
		data["Live"] = backupLive(r, st)
		if st.Configured {
			list, lerr := s.agent.SiteBackups(r.Context(), name)
			points := list.Points
			if r.URL.Query().Get("all") != "1" && len(points) > backupPointsShown {
				data["More"] = len(points) - backupPointsShown
				points = points[:backupPointsShown]
			}
			data["List"], data["Points"], data["ListErr"] = list, points, errString(lerr)
		}
	}
	s.renderStatus(w, r, status, "site_backup", data)
}

// siteBackupSave saves a site's own settings; "reset" goes back to the general ones.
func (s *Server) siteBackupSave(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.findSite(w, r)
	if !ok {
		return
	}
	name := ss.Spec.Name
	f := backupSiteForm{On: r.PostFormValue("on") == "on", DB: r.PostFormValue("db"), Files: r.PostFormValue("files"),
		KeepHours: r.PostFormValue("keep_hours"), KeepDays: r.PostFormValue("keep_days"), KeepMonths: r.PostFormValue("keep_months"),
		Freeze: r.PostFormValue("freeze") == "on"}
	cur, err := s.agent.BackupSettings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var problems []string
	var pol agent.BackupPolicy // "reset": back to the general settings, whatever was typed
	if r.PostFormValue("reset") != "1" {
		pol = agent.BackupPolicy{Off: !f.On, DB: f.DB, Files: f.Files, Freeze: f.Freeze,
			KeepHours:  atoiField(f.KeepHours, "Saatlik saklama", true, &problems),
			KeepDays:   atoiField(f.KeepDays, "Günlük saklama", true, &problems),
			KeepMonths: atoiField(f.KeepMonths, "Aylık saklama", true, &problems)}
	}
	// What the site does not have is not set: a database plan for a site without one
	// would only confuse the next reader.
	if !ss.Spec.Database {
		pol.DB = ""
	}
	if ss.Spec.DataMount == "" {
		pol.Files, pol.Freeze = "", false
	}
	if cur.Sites == nil {
		cur.Sites = map[string]agent.BackupPolicy{}
	}
	if pol == (agent.BackupPolicy{}) {
		delete(cur.Sites, name)
	} else {
		cur.Sites[name] = pol
	}
	if len(problems) == 0 {
		if err := s.agent.SetBackupSettings(r.Context(), cur, s.actor(r)); err != nil {
			problems = problemsOf(err)
		}
	}
	if len(problems) > 0 {
		st, serr := s.agent.BackupStatus(r.Context())
		s.renderSiteBackup(w, r, http.StatusUnprocessableEntity, ss, st, serr, f, problems)
		return
	}
	msg := name + " yedek ayarları kaydedildi."
	if pol == (agent.BackupPolicy{}) {
		msg = name + " genel yedek ayarlarına döndü."
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/sites/"+name+"/backup", http.StatusSeeOther)
}
