package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

// fakeRestore plays the agent's restore side.
type fakeRestore struct {
	list       agent.BackupList
	details    map[string]agent.BackupDetail
	current    agent.BackupCurrent
	listed     []string
	opened     []string
	closed     []string
	restores   []agent.BackupRestore
	confirms   []string
	restoreErr error
	exports    []agent.BackupExport
	folder     agent.BackupFolder
	folderAsk  []string
	downloads  []string
	viewCalls  []string
	viewDoc    agent.DBDocument
	compares   int
}

func (f *fakeAgent) ListBackups(_ context.Context, site string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.listed = append(f.rs.listed, site)
	return "joblist", nil
}

func (f *fakeAgent) SiteBackups(context.Context, string) (agent.BackupList, error) {
	return f.rs.list, nil
}

func (f *fakeAgent) SiteBackup(_ context.Context, _, run string) (agent.BackupDetail, error) {
	d, ok := f.rs.details[run]
	if !ok {
		return d, &agent.Error{Status: 404, Message: "bu yedek listede yok: saklama kuralıyla silinmiş olabilir (listeyi yenile)"}
	}
	return d, nil
}

func (f *fakeAgent) BackupCompare(ctx context.Context, site, run string) (agent.BackupCompare, error) {
	f.mu.Lock()
	f.rs.compares++
	f.mu.Unlock()
	d, err := f.SiteBackup(ctx, site, run)
	return agent.BackupCompare{Backup: d, Current: f.rs.current}, err
}

func (f *fakeAgent) OpenBackup(_ context.Context, _, run string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.opened = append(f.rs.opened, run)
	return "jobopen", nil
}

func (f *fakeAgent) CloseView(_ context.Context, _, id string, _ agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.closed = append(f.rs.closed, id)
	return nil
}

func (f *fakeAgent) RestoreBackup(_ context.Context, _ string, req agent.BackupRestore, confirm string, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.restores, f.rs.confirms = append(f.rs.restores, req), append(f.rs.confirms, confirm)
	return "jobrestore", f.rs.restoreErr
}

func (f *fakeAgent) ExportBackup(_ context.Context, _ string, req agent.BackupExport, _ agent.Actor) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.exports = append(f.rs.exports, req)
	return "jobexport", nil
}

func (f *fakeAgent) BackupFiles(_ context.Context, _, run, dir string) (agent.BackupFolder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.folderAsk = append(f.rs.folderAsk, run+":"+dir)
	l := f.rs.folder
	l.Path = dir
	return l, nil
}

func (f *fakeAgent) DownloadBackupFile(_ context.Context, _, run, p string, _ agent.Actor) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.downloads = append(f.rs.downloads, run+":"+p)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Length": {"6"}, "Content-Disposition": {`attachment; filename="b.txt"`}},
		Body: io.NopCloser(strings.NewReader("ikinci"))}, nil
}

func (f *fakeAgent) ViewCollections(_ context.Context, _, view string) (agent.DBOverview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.viewCalls = append(f.rs.viewCalls, "collections "+view)
	return agent.DBOverview{DB: "footops_tmp_x", Collections: []agent.DBCollection{{Name: "urunler", Type: "collection", Count: 5}}}, nil
}

func (f *fakeAgent) ViewFind(_ context.Context, _, view string, q agent.DBQuery) (agent.DBPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.viewCalls = append(f.rs.viewCalls, "find "+view+" "+q.Collection)
	return agent.DBPage{Collection: agent.DBCollection{Name: q.Collection, Type: "collection"}, Total: 1, Page: 1, PerPage: 20,
		Docs: []agent.DBDocSummary{{ID: `{"$oid": "65a1b2c3d4e5f6a7b8c9d0e1"}`, IDText: `{"$oid": "65a1b2c3d4e5f6a7b8c9d0e1"}`}}}, nil
}

func (f *fakeAgent) ViewDoc(_ context.Context, _, view, coll, id string) (agent.DBDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rs.viewCalls = append(f.rs.viewCalls, "doc "+view+" "+coll)
	d := f.rs.viewDoc
	d.ID = id
	return d, nil
}

// restoreEnv is a site with a database and files, and two backups: R1 (the night: all
// three parts, its database open for a look) and R2 (an hour later, the database only).
func restoreEnv(t *testing.T) (*env, *client, string) {
	t.Helper()
	e := newEnv(t)
	e.agent.dbSite, e.agent.dataSite = true, true
	c := e.client(t, true)
	tok := e.login(t, c)
	e.confirmedBackups()
	night := time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC)
	parts := map[string]agent.BackupSnapshot{
		agent.PartDB:    {ID: strings.Repeat("a", 64), Time: night, Part: agent.PartDB, Bytes: 4 << 20},
		agent.PartFiles: {ID: strings.Repeat("b", 64), Time: night, Part: agent.PartFiles, Files: 120, Bytes: 30 << 20},
		agent.PartDef:   {ID: strings.Repeat("c", 64), Time: night, Part: agent.PartDef},
	}
	view := &agent.BackupView{ID: "View1", Site: "enisparta", Run: "R1", Taken: night, Ready: true, DB: "enisparta",
		Expires: time.Now().Add(40 * time.Minute), Collections: map[string]int64{"urunler": 5, "yeni": 2, "ayni": 3}, Documents: 10}
	r1 := agent.BackupDetail{BackupPoint: agent.BackupPoint{Run: "R1", Time: night, Kind: agent.RunDaily, Parts: parts}, View: view,
		Record: []agent.BackupPart{{Site: "enisparta", Part: agent.PartDB, Snapshot: parts[agent.PartDB].ID, Collections: map[string]int64{"urunler": 5, "yeni": 2, "ayni": 3, "system.js": 1}}}}
	r2 := agent.BackupDetail{BackupPoint: agent.BackupPoint{Run: "R2", Time: night.Add(time.Hour), Kind: agent.RunHourly,
		Parts: map[string]agent.BackupSnapshot{agent.PartDB: parts[agent.PartDB]}}}
	e.agent.rs.details = map[string]agent.BackupDetail{"R1": r1, "R2": r2}
	e.agent.rs.list = agent.BackupList{Updated: time.Now().Add(-10 * time.Minute), Points: []agent.BackupPoint{r2.BackupPoint, r1.BackupPoint},
		Views: []agent.BackupView{*view}}
	e.agent.rs.current = agent.BackupCurrent{DB: "enisparta", Collections: map[string]int64{"urunler": 7, "eski": 4, "ayni": 3}, Documents: 14, Running: true}
	return e, c, tok
}

func TestSiteBackupsList(t *testing.T) {
	e, c, tok := restoreEnv(t)
	_, body := c.do("GET", "/sites/enisparta/backup", nil)
	for _, want := range []string{`id="yedekler"`, `href="/sites/enisparta/backups/R1"`, `href="/sites/enisparta/backups/R2"`,
		"veritabanı, dosyalar, site tanımı · 120 dosya", "incelemeye açık", `href="/sites/enisparta/backups/R1/db"`, "Listeyi yenile", "2 yedek"} {
		if !strings.Contains(body, want) {
			t.Errorf("site backup page lacks %q", want)
		}
	}
	if strings.Contains(body, "· 0 dosya") {
		t.Error("a backup without files shows a file count")
	}
	resp, _ := c.do("POST", "/sites/enisparta/backups/list", url.Values{"csrf": {tok}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/joblist" || len(e.agent.rs.listed) != 1 || e.agent.rs.listed[0] != "enisparta" {
		t.Errorf("refresh: %d %s %v", resp.StatusCode, resp.Header.Get("Location"), e.agent.rs.listed)
	}
	// Many backups: the newest are shown, the rest one click away.
	var many []agent.BackupPoint
	for i := 0; i < backupPointsShown+5; i++ {
		many = append(many, agent.BackupPoint{Run: "R" + strings.Repeat("x", i+1), Time: time.Now(), Kind: agent.RunHourly})
	}
	e.agent.rs.list.Points = many
	_, body = c.do("GET", "/sites/enisparta/backup", nil)
	if !strings.Contains(body, "Daha eski 5 yedeği de göster") {
		t.Error("no link to the older backups")
	}
	if _, body = c.do("GET", "/sites/enisparta/backup?all=1", nil); strings.Contains(body, "Daha eski") ||
		strings.Count(body, "/backups/Rx") < 2*(backupPointsShown+5) {
		t.Error("all backups not shown")
	}
}

func TestBackupPointPage(t *testing.T) {
	e, c, tok := restoreEnv(t)
	_, body := c.do("GET", "/sites/enisparta/backups/R1", nil)
	for _, want := range []string{"Yedek: ", "10 belge · döküm", "120 dosya", "Koleksiyonlara bak", `action="/sites/enisparta/backups/R1/close"`,
		`action="/sites/enisparta/backups/R1/export"`, `name="env"`, "Dosyalara göz at", `href="/sites/enisparta/backups/R1/restore"`} {
		if !strings.Contains(body, want) {
			t.Errorf("backup page lacks %q", want)
		}
	}
	_, body = c.do("GET", "/sites/enisparta/backups/R2", nil)
	if !strings.Contains(body, "Aç ve incele") || strings.Contains(body, `name="files"`) || strings.Contains(body, `name="env"`) ||
		strings.Contains(body, "Dosyalara göz at") || !strings.Contains(body, "yedek açılınca görünür") {
		t.Errorf("a database-only backup:\n%s", body)
	}
	resp, _ := c.do("POST", "/sites/enisparta/backups/R2/open", url.Values{"csrf": {tok}})
	if resp.Header.Get("Location") != "/jobs/jobopen" || len(e.agent.rs.opened) != 1 || e.agent.rs.opened[0] != "R2" {
		t.Errorf("open: %s %v", resp.Header.Get("Location"), e.agent.rs.opened)
	}
	resp, _ = c.do("POST", "/sites/enisparta/backups/R1/close", url.Values{"csrf": {tok}})
	if resp.Header.Get("Location") != "/sites/enisparta/backups/R1" || len(e.agent.rs.closed) != 1 || e.agent.rs.closed[0] != "View1" {
		t.Errorf("close: %s %v", resp.Header.Get("Location"), e.agent.rs.closed)
	}
	// A backup retention removed: back to the list, with why.
	resp, _ = c.do("GET", "/sites/enisparta/backups/R9", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/enisparta/backup#yedekler" {
		t.Errorf("gone: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if _, body := c.do("GET", "/sites/enisparta/backup", nil); !strings.Contains(body, "saklama kuralıyla silinmiş") {
		t.Error("why it is gone not shown")
	}
	if resp, _ := c.do("GET", "/sites/enisparta/backups/..%2Fx", nil); resp.StatusCode != 404 {
		t.Errorf("bad run: %d", resp.StatusCode)
	}
	// Without a token nothing opens.
	c.do("POST", "/sites/enisparta/backups/R2/open", url.Values{})
	if len(e.agent.rs.opened) != 1 {
		t.Error("opened without CSRF token")
	}
}

func TestBackupRestorePage(t *testing.T) {
	e, c, tok := restoreEnv(t)
	_, body := c.do("GET", "/sites/enisparta/backups/R1/restore", nil)
	for _, want := range []string{`name="db" checked`, "yeni: yedekten gelir", "yalnız sitede: silinir", "farklı: yedekteki hali gelir",
		"aynı sayıda", "120 dosya", `name="confirm"`, "<strong>10</strong>", "<strong>14</strong>"} {
		if !strings.Contains(body, want) {
			t.Errorf("restore page lacks %q", want)
		}
	}
	if strings.Contains(body, `name="files" checked`) {
		t.Error("files chosen without asking")
	}
	if _, body = c.do("GET", "/sites/enisparta/backups/R1/restore?part=files", nil); !strings.Contains(body, `name="files" checked`) {
		t.Error("?part=files not chosen")
	}
	// Without the database open, its preview is a step away.
	_, body = c.do("GET", "/sites/enisparta/backups/R2/restore", nil)
	if !strings.Contains(body, `formaction="/sites/enisparta/backups/R2/open"`) || strings.Contains(body, `name="db"`) {
		t.Errorf("unopened database:\n%s", body)
	}
	for _, tc := range []struct {
		form url.Values
		run  string
		want string
	}{
		{url.Values{"confirm": {"enisparta"}}, "R1", "Geri yüklenecek bir şey seç"},
		{url.Values{"db": {"on"}, "confirm": {"yanlis"}}, "R1", "site adını aynen yaz"},
		{url.Values{"db": {"on"}, "confirm": {"enisparta"}}, "R2", "önce yedeği aç"},
	} {
		tc.form.Set("csrf", tok)
		resp, body := c.do("POST", "/sites/enisparta/backups/"+tc.run+"/restore", tc.form)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tc.want) {
			t.Errorf("%v: %d, want %q", tc.form, resp.StatusCode, tc.want)
		}
	}
	if len(e.agent.rs.restores) != 0 {
		t.Fatal("restored despite the problems")
	}
	resp, _ := c.do("POST", "/sites/enisparta/backups/R1/restore", url.Values{"csrf": {tok}, "db": {"on"}, "files": {"on"}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/jobs/jobrestore" {
		t.Fatalf("restore: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	got := e.agent.rs.restores[0]
	if got.Run != "R1" || !got.DB || !got.Files || got.View != "View1" || e.agent.rs.confirms[0] != "enisparta" {
		t.Errorf("request %+v %v", got, e.agent.rs.confirms)
	}
	// The agent refusing it keeps the form.
	e.agent.rs.restoreErr = &agent.Error{Status: 409, Message: "veritabanının önizlemesi hazır değil"}
	resp, body = c.do("POST", "/sites/enisparta/backups/R1/restore", url.Values{"csrf": {tok}, "files": {"on"}, "confirm": {"enisparta"}})
	if resp.StatusCode != 409 || !strings.Contains(body, "önizlemesi hazır değil") || !strings.Contains(body, `name="files" checked`) {
		t.Errorf("refused: %d", resp.StatusCode)
	}
}

func TestBackupExportForm(t *testing.T) {
	e, c, tok := restoreEnv(t)
	resp, _ := c.do("POST", "/sites/enisparta/backups/R1/export", url.Values{"csrf": {tok}, "db": {"on"}, "env": {"on"}})
	if resp.Header.Get("Location") != "/jobs/jobexport" || len(e.agent.rs.exports) != 1 {
		t.Fatalf("export: %s %v", resp.Header.Get("Location"), e.agent.rs.exports)
	}
	if x := e.agent.rs.exports[0]; x.Run != "R1" || !x.DB || x.Files || !x.Env {
		t.Errorf("export %+v", x)
	}
	resp, _ = c.do("POST", "/sites/enisparta/backups/R1/export", url.Values{"csrf": {tok}, "env": {"on"}})
	if resp.Header.Get("Location") != "/sites/enisparta/backups/R1#indir" || len(e.agent.rs.exports) != 1 {
		t.Errorf("env alone: %s", resp.Header.Get("Location"))
	}
}

func TestBackupFilesPages(t *testing.T) {
	e, c, _ := restoreEnv(t)
	e.agent.rs.folder = agent.BackupFolder{Entries: []agent.BackupEntry{{Name: "alt klasör", Type: "dir"}, {Name: "b ğ.txt", Type: "file", Size: 6},
		{Name: "bag", Type: "symlink"}}}
	_, body := c.do("GET", "/sites/enisparta/backups/R1/files?path=foto", nil)
	for _, want := range []string{`href="/sites/enisparta/backups/R1/files?path=foto%2Falt&#43;klas%C3%B6r"`,
		`href="/sites/enisparta/backups/R1/files/download?path=foto%2fb%20%c4%9f.txt"`, "bağlantı", `aria-current="page">foto`} {
		if !strings.Contains(body, want) {
			t.Errorf("files page lacks %q\n%s", want, body)
		}
	}
	if e.agent.rs.folderAsk[0] != "R1:foto" {
		t.Errorf("asked %v", e.agent.rs.folderAsk)
	}
	resp, body := c.do("GET", "/sites/enisparta/backups/R1/files/download?path=foto/b.txt", nil)
	if resp.StatusCode != 200 || body != "ikinci" || resp.Header.Get("Content-Disposition") == "" || e.agent.rs.downloads[0] != "R1:foto/b.txt" {
		t.Errorf("download: %d %q %v", resp.StatusCode, body, e.agent.rs.downloads)
	}
}

func TestViewDatabasePages(t *testing.T) {
	e, c, _ := restoreEnv(t)
	e.agent.rs.viewDoc = agent.DBDocument{Collection: agent.DBCollection{Name: "urunler", Type: "collection"}, Text: "{}", Editable: true, Deletable: true}
	_, body := c.do("GET", "/sites/enisparta/backups/R1/db", nil)
	if !strings.Contains(body, "Yedekteki veritabanı") || !strings.Contains(body, `href="/sites/enisparta/backups/R1/db/coll?c=urunler"`) ||
		strings.Contains(body, "Son değişiklikler") || !strings.Contains(body, "salt okunur") {
		t.Errorf("view collections:\n%s", body)
	}
	_, body = c.do("GET", "/sites/enisparta/backups/R1/db/coll?c=urunler", nil)
	if !strings.Contains(body, `action="/sites/enisparta/backups/R1/db/coll"`) || !strings.Contains(body, `href="/sites/enisparta/backups/R1/db/doc?c=urunler&amp;id=`) {
		t.Errorf("view collection:\n%s", body)
	}
	_, body = c.do("GET", "/sites/enisparta/backups/R1/db/doc?c=urunler&id=%7B%7D", nil)
	if strings.Contains(body, "Düzenle") || strings.Contains(body, "Sil…") || !strings.Contains(body, "salt okunur") {
		t.Errorf("a backup's document can be changed:\n%s", body)
	}
	if got := strings.Join(e.agent.rs.viewCalls, ", "); got != "collections View1, find View1 urunler, doc View1 urunler" {
		t.Errorf("calls %s", got)
	}
	// Not open: back to the backup's page.
	resp, _ := c.do("GET", "/sites/enisparta/backups/R2/db", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/sites/enisparta/backups/R2" {
		t.Errorf("unopened: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRestoreJobLinks(t *testing.T) {
	e, c, _ := restoreEnv(t)
	for _, tc := range []struct {
		kind, result string
		want         []string
	}{
		{"backup.open", "R1", []string{`href="/sites/enisparta/backups/R1/db"`, `href="/sites/enisparta/backups/R1/restore"`, `class="crumb" href="/sites/enisparta/backup"`}},
		{"backup.export", "X1", []string{`href="/sites/enisparta/exports/X1/download"`}},
		{"backup.restore", "20261106-090000 files-20261106-090001", []string{`href="/sites/enisparta/snapshots/20261106-090000/restore"`, `href="/sites/enisparta/files/20261106-090001/restore"`, "Siteye git"}},
		{"backup.list", "120 anlık görüntü", []string{`href="/sites/enisparta/backup#yedekler"`}},
	} {
		e.agent.job = &agent.Job{ID: "j1", Kind: tc.kind, Target: "enisparta", Done: true, Result: tc.result}
		_, body := c.do("GET", "/jobs/j1", nil)
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", tc.kind, want)
			}
		}
	}
}

func TestRestoreRecordDiffAndCurrentErrors(t *testing.T) {
	e, c, tok := restoreEnv(t)
	r1 := e.agent.rs.details["R1"]
	view := *r1.View
	view.RecordChecked, view.RecordDiff = true, "urunler: kayıtta 6, yüklenen 5"
	r1.View = &view
	e.agent.rs.details["R1"] = r1
	e.agent.rs.current.DBErr = "MongoDB cevap vermedi"
	e.agent.rs.current.Collections["eski"] = -1
	_, body := c.do("GET", "/sites/enisparta/backups/R1/restore", nil)
	for _, want := range []string{"yedek alınırken sayılanla tutmuyor", `name="accept_diff"`, "urunler: kayıtta 6, yüklenen 5",
		"Sitenin şu anki veritabanı okunamadı: MongoDB cevap vermedi", "şu anki sayı okunamadı", "<td data-label=\"Sitede şu an\">?</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("restore page lacks %q", want)
		}
	}
	if _, body := c.do("GET", "/sites/enisparta/backups/R1", nil); !strings.Contains(body, "onaylanmadan yapılmaz") {
		t.Error("the backup's page does not warn")
	}
	resp, body := c.do("POST", "/sites/enisparta/backups/R1/restore", url.Values{"csrf": {tok}, "db": {"on"}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "farkı onayla") || len(e.agent.rs.restores) != 0 {
		t.Errorf("a difference not accepted: %d", resp.StatusCode)
	}
	n := e.agent.rs.compares
	resp, _ = c.do("POST", "/sites/enisparta/backups/R1/restore", url.Values{"csrf": {tok}, "db": {"on"}, "accept_diff": {"on"}, "confirm": {"enisparta"}})
	if resp.StatusCode != http.StatusSeeOther || len(e.agent.rs.restores) != 1 || !e.agent.rs.restores[0].AcceptDiff {
		t.Fatalf("accepted: %d %+v", resp.StatusCode, e.agent.rs.restores)
	}
	if e.agent.rs.compares != n {
		t.Error("a restore that goes ahead read the site's counts again")
	}
}

func TestExportListMarksBackupsAndSecrets(t *testing.T) {
	e, c, _ := restoreEnv(t)
	e.agent.exports = []agent.ExportInfo{{ID: "X1", Site: "enisparta", File: "enisparta-20261106-010000.footops.tar", Bytes: 1 << 20,
		Created: time.Now(), Backup: time.Date(2026, 11, 6, 1, 0, 0, 0, time.UTC), Env: true, Files: 3, FileBytes: 2 << 10}}
	_, body := c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, "yedekten (") || !strings.Contains(body, "sırlar içerir") || strings.Contains(body, "0 koleksiyon, 0 belge") {
		t.Errorf("export list:\n%s", body[strings.Index(body, "enisparta-2026"):][:600])
	}
}
