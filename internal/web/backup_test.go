package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

// fakeBackup plays the agent's backup API.
type fakeBackup struct {
	status    agent.BackupStatus
	key       string
	keyReads  int
	setups    []agent.BackupSetupRequest
	setupErr  error
	confirms  int
	saved     []agent.BackupSettings
	setErr    error
	runs      []agent.BackupRun
	runSites  []string
	runErr    error
	runsAsked []string
}

func (f *fakeAgent) BackupStatus(context.Context) (agent.BackupStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.bk.status
	if st.Settings.Night == "" {
		st.Settings = agent.DefaultBackupSettings()
		f.bk.status.Settings = st.Settings
	}
	return st, nil
}

func (f *fakeAgent) BackupSetup(_ context.Context, req agent.BackupSetupRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bk.setups = append(f.bk.setups, req)
	if f.bk.setupErr != nil {
		return "", f.bk.setupErr
	}
	return "jobsetup", nil
}

func (f *fakeAgent) BackupKey(context.Context, agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bk.keyReads++
	if f.bk.key == "" {
		return "", &agent.Error{Status: 403, Message: "şifre onaylandıktan sonra yalnız sunucuda gösterilir"}
	}
	return f.bk.key, nil
}

func (f *fakeAgent) BackupConfirm(context.Context, agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bk.confirms++
	f.bk.status.Settings.Confirmed = time.Now()
	return nil
}

func (f *fakeAgent) BackupSettings(ctx context.Context) (agent.BackupSettings, error) {
	st, err := f.BackupStatus(ctx)
	return st.Settings, err
}

func (f *fakeAgent) SetBackupSettings(_ context.Context, s agent.BackupSettings, _ agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bk.setErr != nil {
		return f.bk.setErr
	}
	f.bk.saved = append(f.bk.saved, s)
	s.Confirmed = f.bk.status.Settings.Confirmed
	f.bk.status.Settings = s
	return nil
}

func (f *fakeAgent) BackupRun(_ context.Context, site string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bk.runSites = append(f.bk.runSites, site)
	if f.bk.runErr != nil {
		return "", f.bk.runErr
	}
	return "jobbackup", nil
}

func (f *fakeAgent) BackupRuns(_ context.Context, site string, limit int) ([]agent.BackupRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bk.runsAsked = append(f.bk.runsAsked, site)
	return f.bk.runs, nil
}

// confirmedBackups sets the fake up as a server that backs up.
func (e *env) confirmedBackups() {
	s := agent.DefaultBackupSettings()
	s.Confirmed = time.Now().Add(-48 * time.Hour)
	e.agent.bk.status = agent.BackupStatus{Configured: true, Repository: "footops-h1", Settings: s}
}

func TestBackupSetupFlow(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)

	resp, body := c.do("GET", "/backup", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Depoyu bağla") || !strings.Contains(body, `placeholder="footops-h1"`) {
		t.Fatalf("not configured: %d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `href="/backup" aria-current="page"`) {
		t.Error("menu entry not highlighted")
	}

	form := url.Values{"csrf": {tok}, "endpoint": {" https://acc.r2.cloudflarestorage.com/ "}, "bucket": {"footops-h1"},
		"key_id": {"KEYID12345"}, "secret_key": {"S3cr3t/Key+="}}
	resp, _ = c.do("POST", "/backup/setup", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobsetup" {
		t.Fatalf("setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	got := e.agent.bk.setups[0]
	if got.Repository != "s3:https://acc.r2.cloudflarestorage.com/footops-h1" || got.KeyID != "KEYID12345" ||
		got.SecretKey != "S3cr3t/Key+=" || got.Password != "" || got.Current != "" || got.Actor.User != "furkan" {
		t.Errorf("setup request = %+v", got)
	}

	// The bucket in the endpoint would make a folder of the same name: refused here.
	form.Set("endpoint", "https://acc.r2.cloudflarestorage.com/footops-h1")
	resp, body = c.do("POST", "/backup/setup", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "kovayı kendi alanına") || len(e.agent.bk.setups) != 1 {
		t.Errorf("endpoint with a path: %d, %d setups", resp.StatusCode, len(e.agent.bk.setups))
	}
	if strings.Contains(body, "S3cr3t") {
		t.Error("secret key echoed back")
	}
	if !strings.Contains(body, `value="KEYID12345"`) {
		t.Error("typed key ID lost")
	}

	// The agent's problems are shown on the form.
	e.agent.bk.setupErr = &agent.Error{Status: 422, Message: "yedek deposu bilgilerinde sorunlar var", Problems: []string{"kova adı 3-63 karakter"}}
	form.Set("endpoint", "acc.r2.cloudflarestorage.com") // https:// is added
	resp, body = c.do("POST", "/backup/setup", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "kova adı 3-63 karakter") {
		t.Errorf("agent problems: %d", resp.StatusCode)
	}
	if r := e.agent.bk.setups[1].Repository; r != "s3:https://acc.r2.cloudflarestorage.com/footops-h1" {
		t.Errorf("repository = %s", r)
	}
}

func TestBackupKeyShownUntilConfirmed(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.agent.bk.status = agent.BackupStatus{Configured: true, Repository: "footops-h1", Settings: agent.DefaultBackupSettings()}
	e.agent.bk.key = "Abcdefghij0123456789Abcdefghij0123456789"

	_, body := c.do("GET", "/backup", nil)
	if !strings.Contains(body, "Şifreyi göster") || strings.Contains(body, e.agent.bk.key) || e.agent.bk.keyReads != 0 {
		t.Fatalf("the password is shown only when asked (reads %d)", e.agent.bk.keyReads)
	}
	resp, body := c.do("POST", "/backup/key", url.Values{"csrf": {tok}})
	if resp.StatusCode != 200 || !strings.Contains(body, e.agent.bk.key) || e.agent.bk.keyReads != 1 {
		t.Fatalf("key: %d, reads %d", resp.StatusCode, e.agent.bk.keyReads)
	}
	if strings.Contains(body, "data-live=") {
		t.Error("the page with the password refreshes itself")
	}
	resp, _ = c.do("POST", "/backup/confirm", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusUnprocessableEntity || e.agent.bk.confirms != 0 {
		t.Errorf("confirmed without the box: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/backup/confirm", url.Values{"csrf": {tok}, "saved": {"on"}})
	if resp.StatusCode != http.StatusSeeOther || e.agent.bk.confirms != 1 {
		t.Errorf("confirm: %d, %d", resp.StatusCode, e.agent.bk.confirms)
	}
	// Confirmed: the agent refuses the password to the panel from now on.
	e.agent.bk.key = ""
	resp, _ = c.do("POST", "/backup/key", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("key after confirm: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/backup", nil)
	if !strings.Contains(body, "Gece yedeği") || !strings.Contains(body, "Şifre gösterilemedi") || !strings.Contains(body, "Şimdi yedekle") {
		t.Errorf("confirmed page:\n%s", body)
	}
}

func TestBackupReplaceRepository(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.agent.bk.status = agent.BackupStatus{Configured: true, Repository: "footops-h1", Settings: agent.DefaultBackupSettings()}
	e.agent.bk.key = "Abcdefghij0123456789Abcdefghij0123456789"
	form := url.Values{"csrf": {tok}, "endpoint": {"https://acc.r2.cloudflarestorage.com"}, "bucket": {"footops-h1-yeni"},
		"key_id": {"KEYID12345"}, "secret_key": {"S3cr3t"}, "mode": {"new"}}

	// Not confirmed yet: starting over takes no password, and the panel reads none.
	resp, _ := c.do("POST", "/backup/setup", form)
	if got := e.agent.bk.setups[0]; resp.StatusCode != http.StatusSeeOther || got.Current != "" || got.Password != "" || got.Keep {
		t.Fatalf("replace before confirm: %d %+v", resp.StatusCode, got)
	}
	if e.agent.bk.keyReads != 0 {
		t.Error("the panel read the password to replace the repository")
	}

	// Confirmed: the same repository needs no password (the agent uses its own) ...
	e.agent.bk.status.Settings.Confirmed = time.Now()
	e.agent.bk.key = ""
	_, body := c.do("GET", "/backup/repo", nil)
	if !strings.Contains(body, `name="current"`) {
		t.Fatal("no field for the current password")
	}
	form.Set("mode", "same")
	resp, _ = c.do("POST", "/backup/setup", form)
	if got := e.agent.bk.setups[1]; resp.StatusCode != http.StatusSeeOther || !got.Keep || got.Password != "" || got.Current != "" {
		t.Errorf("same repository: %d %+v", resp.StatusCode, got)
	}
	// ... a new one does.
	form.Set("mode", "new")
	resp, body = c.do("POST", "/backup/setup", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "şu anki deponun şifresi gerekli") || len(e.agent.bk.setups) != 2 {
		t.Errorf("new repository without the current password: %d", resp.StatusCode)
	}
	form.Set("current", " eski-sifre-0123456789 ")
	resp, _ = c.do("POST", "/backup/setup", form)
	if got := e.agent.bk.setups[2]; resp.StatusCode != http.StatusSeeOther || got.Current != "eski-sifre-0123456789" || got.Password != "" || got.Keep {
		t.Errorf("new repository: %d %+v", resp.StatusCode, got)
	}
	if strings.Contains(body, "eski-sifre") || strings.Contains(body, "S3cr3t") {
		t.Error("a secret echoed back")
	}
}

// A page answering a POST never updates itself (live.js would GET a POST-only address);
// a broken settings file is said, with the way out.
func TestBackupPagesLiveOnlyOnGet(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.confirmedBackups()
	e.agent.bk.status.Running = &agent.Job{ID: "j9", Kind: "backup.run", Target: "gece yedeği", Started: time.Now()}
	_, body := c.do("GET", "/backup", nil)
	if !strings.Contains(body, `data-live="5"`) || !strings.Contains(body, "Şu an yedek alınıyor") {
		t.Error("a running backup does not update the page")
	}
	resp, body := c.do("POST", "/backup/settings", url.Values{"csrf": {tok}, "night": {"04:00"}, "db": {"daily"}, "files": {"daily"},
		"keep_hours": {"x"}, "keep_days": {"30"}, "keep_months": {"6"}, "warn_gb": {"8"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "data-live=") {
		t.Errorf("POST answer updates itself: %d", resp.StatusCode)
	}
	e.agent.bk.status.Running.Kind = "backup.setup"
	_, body = c.do("GET", "/backup", nil)
	if strings.Contains(body, "data-live=") || !strings.Contains(body, "Yedek deposu bağlanıyor") {
		t.Error("setting a repository up: no live page, its own notice")
	}
	e.agent.bk.status = agent.BackupStatus{Settings: agent.DefaultBackupSettings(), Error: "yedek deposu ayarları bozuk; depoyu yeniden bağla"}
	_, body = c.do("GET", "/backup", nil)
	if !strings.Contains(body, "ayarları bozuk") || !strings.Contains(body, "sudo footops backup setup") {
		t.Error("a broken repository file is not shown")
	}
}

func TestBackupGeneralSettings(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.confirmedBackups()
	e.agent.bk.status.Settings.Sites = map[string]agent.BackupPolicy{"kasa": {Freeze: true}}

	form := url.Values{"csrf": {tok}, "night": {"03:30"}, "db": {"6h"}, "files": {"weekly"}, "keep_hours": {"24"},
		"keep_days": {"14"}, "keep_months": {"3"}, "warn_gb": {"4"}}
	resp, _ := c.do("POST", "/backup/settings", form)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings: %d", resp.StatusCode)
	}
	s := e.agent.bk.saved[0]
	want := agent.BackupPolicy{DB: "6h", Files: "weekly", KeepHours: 24, KeepDays: 14, KeepMonths: 3}
	if s.Night != "03:30" || s.Defaults != want || s.WarnGB != 4 || s.Paused || !s.Sites["kasa"].Freeze {
		t.Errorf("saved %+v", s)
	}
	form.Set("keep_days", "on dört")
	resp, body := c.do("POST", "/backup/settings", form)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Günlük saklama sayı olmalı") || len(e.agent.bk.saved) != 1 {
		t.Errorf("bad number: %d", resp.StatusCode)
	}
	if !strings.Contains(body, `value="on dört"`) {
		t.Error("typed value lost")
	}

	resp, _ = c.do("POST", "/backup/pause", url.Values{"csrf": {tok}, "paused": {"on"}})
	if resp.StatusCode != http.StatusSeeOther || !e.agent.bk.saved[1].Paused {
		t.Errorf("pause: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/backup", nil)
	if !strings.Contains(body, "Zamanlanmış yedekleme duraklatıldı") {
		t.Error("paused notice missing")
	}
	c.do("POST", "/backup/pause", url.Values{"csrf": {tok}, "paused": {"off"}})
	if e.agent.bk.saved[2].Paused {
		t.Error("not resumed")
	}
}

func TestSiteBackupSettings(t *testing.T) {
	e := newEnv(t)
	e.agent.dbSite, e.agent.dataSite = true, true
	c := e.client(t, true)
	tok := e.login(t, c)
	e.confirmedBackups()

	resp, body := c.do("GET", "/sites/enisparta/backup", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="db"`) || !strings.Contains(body, `name="freeze"`) ||
		!strings.Contains(body, "genel ayar (günlük)") {
		t.Fatalf("site backup page: %d\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `href="/sites" aria-current="page"`) {
		t.Error("a site's backups belong to Siteler")
	}
	post := func(v url.Values) *http.Response {
		v.Set("csrf", tok)
		resp, _ := c.do("POST", "/sites/enisparta/backup", v)
		return resp
	}
	if resp := post(url.Values{"on": {"on"}, "db": {"hourly"}, "files": {""}, "keep_days": {"90"}, "freeze": {"on"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	if got := e.agent.bk.status.Settings.Sites["enisparta"]; got != (agent.BackupPolicy{DB: "hourly", KeepDays: 90, Freeze: true}) {
		t.Errorf("policy = %+v", got)
	}
	post(url.Values{"keep_hours": {""}})
	if got := e.agent.bk.status.Settings.Sites["enisparta"]; got != (agent.BackupPolicy{Off: true}) {
		t.Errorf("turned off = %+v", got)
	}
	post(url.Values{"on": {"on"}, "db": {"hourly"}})
	post(url.Values{"reset": {"1"}, "on": {"on"}, "db": {"hourly"}, "keep_days": {"1.5"}}) // what is typed does not matter
	if _, ok := e.agent.bk.status.Settings.Sites["enisparta"]; ok {
		t.Error("reset kept the site's settings")
	}
	if resp := post(url.Values{"on": {"on"}, "keep_months": {"x"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad number: %d", resp.StatusCode)
	}

	// What a site does not have is not set.
	e.agent.dbSite, e.agent.dataSite = false, false
	post(url.Values{"on": {"on"}, "db": {"hourly"}, "files": {"weekly"}, "freeze": {"on"}, "keep_days": {"7"}})
	if got := e.agent.bk.status.Settings.Sites["enisparta"]; got != (agent.BackupPolicy{KeepDays: 7}) {
		t.Errorf("site without database or files = %+v", got)
	}
	_, body = c.do("GET", "/sites/enisparta/backup", nil)
	if strings.Contains(body, `name="db"`) || strings.Contains(body, `name="freeze"`) {
		t.Error("fields for parts the site does not have")
	}
	if resp, _ := c.do("GET", "/sites/yok/backup", nil); resp.StatusCode != 404 {
		t.Errorf("unknown site: %d", resp.StatusCode)
	}
}

func TestBackupRunButtons(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.confirmedBackups()
	resp, _ := c.do("POST", "/backup/run", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobbackup" {
		t.Errorf("run all: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = c.do("POST", "/sites/enisparta/backup/run", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || strings.Join(e.agent.bk.runSites, ",") != ",enisparta" {
		t.Errorf("run site: %d %q", resp.StatusCode, e.agent.bk.runSites)
	}
	if resp, _ := c.do("POST", "/sites/yok/backup/run", url.Values{"csrf": {tok}}); resp.StatusCode != 404 {
		t.Errorf("unknown site: %d", resp.StatusCode)
	}
	e.agent.bk.runErr = &agent.Error{Status: 409, Message: "başka bir işlem sürüyor; bitince tekrar dene"}
	resp, _ = c.do("POST", "/sites/enisparta/backup/run", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/enisparta/backup" {
		t.Errorf("busy: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body := c.do("GET", "/sites/enisparta/backup", nil)
	if !strings.Contains(body, "başka bir işlem sürüyor") {
		t.Error("busy message not shown")
	}
	// Without a token nothing starts.
	n := len(e.agent.bk.runSites)
	c.do("POST", "/backup/run", url.Values{})
	if len(e.agent.bk.runSites) != n {
		t.Error("ran without CSRF token")
	}
}

func TestBackupJobLinks(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	for _, tc := range []struct{ kind, target, back, nav string }{
		{"backup.run", "enisparta", `href="/sites/enisparta/backup"`, `href="/sites" aria-current`},
		{"backup.run", "gece yedeği", `href="/backup"`, `href="/backup" aria-current`},
		{"backup.setup", "footops-h1", `href="/backup"`, `href="/backup" aria-current`},
	} {
		e.agent.job = &agent.Job{ID: "j1", Kind: tc.kind, Target: tc.target, Done: true, Result: "created"}
		_, body := c.do("GET", "/jobs/j1", nil)
		if !strings.Contains(body, `class="crumb" `+tc.back) || !strings.Contains(body, tc.nav) || strings.Contains(body, "Siteye git") {
			t.Errorf("%s %s:\n%s", tc.kind, tc.target, body)
		}
	}
	if _, body := c.do("GET", "/jobs/j1", nil); !strings.Contains(body, "Devam: şifreyi kaydet") {
		t.Error("a new repository's job leads to its password")
	}
}

func TestBackupListsAndSitePage(t *testing.T) {
	e := newEnv(t)
	e.agent.dbSite, e.agent.dataSite = true, true
	c := e.client(t, true)
	e.login(t, c)
	e.confirmedBackups()
	now := time.Now()
	st := &e.agent.bk.status
	st.Failed = map[string]agent.BackupFailure{"enisparta/files": {Time: now, Err: "bazı dosyalar okunamadı", Warning: true}}
	e.agent.bk.runs = []agent.BackupRun{
		{ID: "r2", Kind: agent.RunManual, Actor: "furkan", Started: now.Add(-time.Hour), Finished: now.Add(-time.Hour + 20*time.Second),
			Parts: []agent.BackupPart{{Site: "enisparta", Part: "db", Snapshot: "abc", Collections: map[string]int64{"users": 3}, Documents: 3, AddedPacked: 2048},
				{Site: "blog", Part: "db", Err: "mongodump başarısız"}}},
		{ID: "r1", Kind: agent.RunDaily, Started: now.Add(-20 * time.Hour), Finished: now.Add(-20*time.Hour + time.Minute), Err: "context canceled"},
	}
	_, body := c.do("GET", "/backup", nil)
	for _, want := range []string{"mongodump başarısız", "iptal edildi", "⚠ eksik", "henüz yok", "elle · furkan", "enisparta · veritabanı"} {
		if !strings.Contains(body, want) {
			t.Errorf("backup page lacks %q", want)
		}
	}
	// On the site's page only its own parts count: blog's failure is not its.
	_, body = c.do("GET", "/sites/enisparta/backup", nil)
	if strings.Contains(body, "mongodump başarısız") || !strings.Contains(body, "son yedekte bazı dosyalar okunamadı") {
		t.Errorf("site page:\n%s", body)
	}
	if e.agent.bk.runsAsked[len(e.agent.bk.runsAsked)-1] != "enisparta" {
		t.Error("runs not asked for the site")
	}
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, "Uzak yedek") || !strings.Contains(body, "veritabanı günlük") || !strings.Contains(body, "yedekler ve ayarlar") {
		t.Errorf("site page summary missing")
	}
	st.Settings.Confirmed = time.Time{}
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, "depo şifresi onaylanmadı") {
		t.Error("an unconfirmed repository reads as not set up")
	}
}

func TestRunViews(t *testing.T) {
	now := time.Now()
	runs := []agent.BackupRun{
		{ID: "a", Started: now, Finished: now.Add(time.Minute), Err: "context deadline exceeded"},
		{ID: "b", Parts: []agent.BackupPart{{Site: "x", Part: "files", Warning: "1 dosya okunamadı", AddedPacked: 10}, {Site: "y", Part: "db", Err: "hata", AddedPacked: 5}},
			Err: "1 işlem başarısız: y (veritabanı): hata"},
		{ID: "c", Err: "yedek deposuna ulaşılamadı"},
	}
	all := runViews(runs, "")
	if all[0].Status != "bad" || all[0].Note != "süre doldu" || all[0].Took != time.Minute {
		t.Errorf("deadline: %+v", all[0])
	}
	if all[1].Status != "bad" || all[1].Note != "hata" || all[1].Added != 15 {
		t.Errorf("failed part: %+v", all[1])
	}
	if all[2].Status != "bad" || all[2].Note != "yedek deposuna ulaşılamadı" {
		t.Errorf("run error: %+v", all[2])
	}
	own := runViews([]agent.BackupRun{{ID: "d", Site: "x", Err: "diskte yer yok"}}, "x")
	if own[0].Status != "bad" || own[0].Note != "diskte yer yok" {
		t.Errorf("a site's own run that reached no part: %+v", own[0])
	}
	x := runViews(runs[1:2], "x")
	if x[0].Status != "warn" || x[0].Note != "1 dosya okunamadı" || len(x[0].Parts) != 1 || x[0].Added != 10 {
		t.Errorf("site view: %+v", x[0])
	}
}

func TestRepoAddress(t *testing.T) {
	for _, tc := range []struct {
		endpoint, bucket, want string
		ok                     bool
	}{
		{"https://acc.r2.cloudflarestorage.com", "footops-h1", "s3:https://acc.r2.cloudflarestorage.com/footops-h1", true},
		{"s3:https://acc.r2.cloudflarestorage.com/", "/footops-h1/h1/", "s3:https://acc.r2.cloudflarestorage.com/footops-h1/h1", true},
		{"acc.r2.cloudflarestorage.com", "b", "s3:https://acc.r2.cloudflarestorage.com/b", true},
		{"https://acc.r2.cloudflarestorage.com/footops-h1", "footops-h1", "", false},
		{"https://acc.r2.cloudflarestorage.com?x=1", "b", "", false},
		{"", "b", "", false},
		{"https://acc.r2.cloudflarestorage.com", " ", "", false},
	} {
		got, problems := repoAddress(tc.endpoint, tc.bucket)
		if (len(problems) == 0) != tc.ok || (tc.ok && got != tc.want) {
			t.Errorf("%q %q: %q %v", tc.endpoint, tc.bucket, got, problems)
		}
	}
}

// A job that failed says why on its page, whatever its steps say (a setup refused by the
// bucket writes no step of its own).
func TestJobPageShowsError(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	e.agent.job = &agent.Job{ID: "j1", Kind: "backup.setup", Target: "footops-a", Done: true,
		Steps: []string{"… yedek deposu deneniyor: footops-a"}, Err: "Yedek deposu (footops-a) erişimi reddetti: anahtar yanlış"}
	_, body := c.do("GET", "/jobs/j1", nil)
	if !strings.Contains(body, "Neden:</strong> Yedek deposu (footops-a) erişimi reddetti") {
		t.Errorf("no reason on the page:\n%s", body)
	}
	e.agent.job = &agent.Job{ID: "j1", Kind: "backup.run", Target: "gece yedeği", Done: true, Cancelled: true, Err: "iptal edildi"}
	if _, body := c.do("GET", "/jobs/j1", nil); strings.Contains(body, "Neden:") {
		t.Error("a cancel is shown as a failure")
	}
}
