package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/cfaccess"
)

// raw sends a request with a body and extra headers (as upload.js does).
func (c *client) raw(method, path string, body io.Reader, hdr http.Header) (*http.Response, string) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, body)
	req.Host = c.host
	req.Header.Set("Origin", c.origin)
	for k, v := range hdr {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set(cfaccess.Header, c.token)
		req.Header.Set("Cf-Connecting-Ip", "203.0.113.7")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestImportPage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	if resp, _ := c.do("GET", "/sites/enisparta/import", nil); resp.StatusCode != 404 {
		t.Errorf("import page for a site without a database: %d", resp.StatusCode)
	}
	e.agent.dbSite = true
	_, body := c.do("GET", "/sites/enisparta/import", nil)
	for _, want := range []string{`<script src="/static/upload.js" defer>`, `data-upload data-site="enisparta"`,
		`enctype="multipart/form-data"`, `action="/sites/enisparta/import/form"`, "90.0 MiB", "2.0 GiB"} {
		if !strings.Contains(body, want) {
			t.Errorf("import page lacks %q", want)
		}
	}
	if _, body := c.do("GET", "/sites/enisparta", nil); strings.Contains(body, "upload.js") ||
		!strings.Contains(body, `href="/sites/enisparta/import"`) || !strings.Contains(body, `action="/sites/enisparta/exports"`) {
		t.Error("site page: upload.js loaded or data buttons missing")
	}
	if resp, body := c.do("GET", "/static/upload.js", nil); resp.StatusCode != 200 || !strings.Contains(body, "X-Chunk-Sha256") {
		t.Errorf("upload.js: %d", resp.StatusCode)
	}
}

func TestUploadAPI(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	tok := e.login(t, c)
	e.agent.dbSite = true
	js := http.Header{"Content-Type": {"application/json"}}

	// Without the CSRF header nothing reaches the agent.
	if resp, _ := c.raw("POST", "/sites/enisparta/uploads", strings.NewReader(`{"name":"a.gz","size":10}`), js); resp.StatusCode != 403 {
		t.Errorf("create without token: %d", resp.StatusCode)
	}
	if resp, _ := c.raw("PUT", "/uploads/"+testUpload+"/0", strings.NewReader("x"), nil); resp.StatusCode != 403 || e.agent.chunks != nil {
		t.Errorf("chunk without token: %d", resp.StatusCode)
	}
	bad := http.Header{"X-Csrf-Token": {"nope"}}
	if resp, _ := c.raw("PUT", "/uploads/"+testUpload+"/0", strings.NewReader("x"), bad); resp.StatusCode != 403 {
		t.Errorf("chunk with a wrong token: %d", resp.StatusCode)
	}

	h := http.Header{"X-Csrf-Token": {tok}, "Content-Type": {"application/json"}}
	resp, body := c.raw("POST", "/sites/enisparta/uploads", strings.NewReader(`{"name":"yedek.gz","size":12}`), h)
	var st agent.UploadStatus
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &st) != nil || st.ID != testUpload || st.Site != "enisparta" ||
		st.Actor.User != "furkan" || st.Actor.Via != viaTunnel {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, _ = c.raw("PUT", "/uploads/"+testUpload+"/0", strings.NewReader("hello, world"),
		http.Header{"X-Csrf-Token": {tok}, agent.ChunkSumHeader: {"abc"}})
	if resp.StatusCode != 200 || string(e.agent.chunks[0]) != "hello, world" || e.agent.sums[0] != "abc" {
		t.Errorf("chunk: %d %q", resp.StatusCode, e.agent.chunks[0])
	}
	if resp, body := c.do("GET", "/uploads/"+testUpload, nil); resp.StatusCode != 200 || !strings.Contains(body, `"missing":[0]`) {
		t.Errorf("status: %d %s", resp.StatusCode, body)
	}
	if resp, _ := c.do("GET", "/uploads/..%2fx", nil); resp.StatusCode != 404 {
		t.Errorf("bad id: %d", resp.StatusCode)
	}
	resp, body = c.raw("POST", "/uploads/"+testUpload+"/finish", strings.NewReader("{}"), h)
	if resp.StatusCode != 200 || !strings.Contains(body, `"next":"/jobs/jobcheck"`) || e.agent.checks[0] != testUpload+"/" {
		t.Errorf("finish: %d %s %v", resp.StatusCode, body, e.agent.checks)
	}
	if resp, _ := c.raw("POST", "/uploads/"+testUpload+"/cancel", nil, h); resp.StatusCode != 204 || len(e.agent.removedUp) != 1 {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
	// A cross-site page cannot drive the API even with a stolen token.
	req, _ := http.NewRequest("PUT", c.base+"/uploads/"+testUpload+"/0", strings.NewReader("x"))
	req.Host = c.host
	req.Header.Set("X-Csrf-Token", tok)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set(cfaccess.Header, c.token)
	if r, err := c.hc.Do(req); err != nil || r.StatusCode != 403 {
		t.Errorf("cross-origin chunk: %v %v", r.StatusCode, err)
	}
}

func multipartBody(t *testing.T, fields [][2]string, file []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		mw.WriteField(f[0], f[1])
	}
	if file != nil {
		fw, _ := mw.CreateFormFile("file", "dökümler/yedek.archive.gz")
		fw.Write(file)
	}
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestImportFormWithoutJavaScript(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite = true
	data := bytes.Repeat([]byte("veri"), 1000)

	for name, fields := range map[string][][2]string{"no token": nil, "wrong token": {{"csrf", "x"}}} {
		body, ct := multipartBody(t, fields, data)
		if resp, _ := c.raw("POST", "/sites/enisparta/import/form", body, http.Header{"Content-Type": {ct}}); resp.StatusCode != 403 || e.agent.single != nil {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	body, ct := multipartBody(t, [][2]string{{"csrf", tok}}, data)
	resp, _ := c.raw("POST", "/sites/enisparta/import/form", body, http.Header{"Content-Type": {ct}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/jobs/jobcheck" || !bytes.Equal(e.agent.single, data) ||
		e.agent.upload.Name != "yedek.archive.gz" {
		t.Errorf("form upload: %d %s, %d bytes, name %q", resp.StatusCode, resp.Header.Get("Location"), len(e.agent.single), e.agent.upload.Name)
	}
	body, ct = multipartBody(t, [][2]string{{"csrf", tok}}, nil)
	if resp, _ := c.raw("POST", "/sites/enisparta/import/form", body, http.Header{"Content-Type": {ct}}); resp.StatusCode != 303 ||
		resp.Header.Get("Location") != "/sites/enisparta/import" {
		t.Errorf("no file: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestImportPreviewAndApply(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite = true
	path := "/sites/enisparta/import/" + testUpload

	if _, body := c.do("GET", path, nil); !strings.Contains(body, "bulunamadı") {
		t.Errorf("unknown upload:\n%s", body)
	}
	e.agent.upload = agent.UploadStatus{Upload: agent.Upload{ID: testUpload, Site: "enisparta", Name: "yedek.gz", Size: 9000, Finished: true}}
	if _, body := c.do("GET", path, nil); !strings.Contains(body, "henüz kontrol edilmedi") || !strings.Contains(body, `action="`+path+`/check"`) {
		t.Errorf("unchecked upload:\n%s", body)
	}

	// Several databases: the operator chooses.
	e.agent.check = &agent.ImportCheck{Upload: testUpload, Site: "enisparta", Format: agent.FormatMongodump, SHA256: "ab12",
		Databases: []string{"blog", "enisparta"}, Target: "enisparta", Current: map[string]int64{"users": 3}}
	_, body := c.do("GET", path, nil)
	if !strings.Contains(body, "Hangi veritabanı?") || !strings.Contains(body, `value="enisparta" checked`) || strings.Contains(body, "/apply") {
		t.Errorf("database choice:\n%s", body)
	}
	if resp, _ := c.do("POST", path+"/check", url.Values{"csrf": {tok}, "source_db": {"blog"}}); resp.Header.Get("Location") != "/jobs/jobcheck" ||
		e.agent.checks[0] != testUpload+"/blog" {
		t.Errorf("recheck: %v", e.agent.checks)
	}

	e.agent.check = &agent.ImportCheck{Upload: testUpload, Site: "enisparta", Format: agent.FormatFootOps, SHA256: "ab12",
		Manifest: &agent.ExportManifest{Site: "eski", Created: time.Now()}, Databases: []string{"eski"}, SourceDB: "eski", Target: "enisparta",
		Archive: map[string]int64{"users": 5, "pages": 2}, ArchiveDocs: 7, Views: []string{"ucuz"},
		Current: map[string]int64{"users": 5, "logs": 40}, CurrentDocs: 45, Checked: time.Now(), Skipped: []string{"system.js"}}
	_, body = c.do("GET", path, nil)
	for _, want := range []string{"FootOps arşivi — eski sitesinden", "(ad otomatik değişir)", ">pages<", ">yeni<", ">logs<",
		"“Tamamen değiştir” siler", ">aynı<", "görünüm", "<strong>7</strong>", "<strong>45</strong>", `value="replace" checked`,
		`name="confirm"`, "system.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	resp, body := c.do("POST", path+"/apply", url.Values{"csrf": {tok}, "mode": {"add"}, "confirm": {"enispart"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "site adını aynen yaz") || len(e.agent.applies) != 0 ||
		!strings.Contains(body, `value="add" checked`) {
		t.Errorf("wrong confirmation: %d %v", resp.StatusCode, e.agent.applies)
	}
	resp, _ = c.do("POST", path+"/apply", url.Values{"csrf": {tok}, "mode": {"add"}, "confirm": {"enisparta"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/jobs/jobapply" || e.agent.applies[0] != "add/enisparta" {
		t.Errorf("apply: %d %v", resp.StatusCode, e.agent.applies)
	}
}

func TestRestoreAndExport(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite = true
	e.agent.snapshots = []agent.Snapshot{{ID: "20261027-110000", Site: "enisparta", Created: time.Now(), Reason: "before-import",
		Documents: 89, Collections: map[string]int64{"a": 89}}}
	_, body := c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, "içe aktarmadan önce") || !strings.Contains(body, `href="/sites/enisparta/snapshots/20261027-110000/restore"`) {
		t.Errorf("site page copies:\n%s", body)
	}
	if resp, _ := c.do("GET", "/sites/enisparta/snapshots/20200101-000000/restore", nil); resp.StatusCode != 404 {
		t.Errorf("unknown copy: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/sites/enisparta/snapshots/20261027-110000/restore", nil)
	if !strings.Contains(body, "Kopya geri yüklensin mi?") || !strings.Contains(body, "89 belge") {
		t.Errorf("restore page:\n%s", body)
	}
	if resp, _ := c.do("POST", "/sites/enisparta/snapshots/20261027-110000/restore", url.Values{"csrf": {tok}, "confirm": {"x"}}); resp.StatusCode != 422 || len(e.agent.restores) != 0 {
		t.Errorf("restore without confirmation: %d", resp.StatusCode)
	}
	resp, _ := c.do("POST", "/sites/enisparta/snapshots/20261027-110000/restore", url.Values{"csrf": {tok}, "confirm": {"enisparta"}})
	if resp.Header.Get("Location") != "/jobs/jobrestore" || e.agent.restores[0] != "20261027-110000/enisparta" {
		t.Errorf("restore: %v", e.agent.restores)
	}

	resp, _ = c.do("POST", "/sites/enisparta/exports", url.Values{"csrf": {tok}})
	if resp.Header.Get("Location") != "/jobs/jobexport" || e.agent.exported != 1 {
		t.Errorf("export: %s", resp.Header.Get("Location"))
	}
	e.agent.exports = []agent.ExportInfo{{ID: "jobexport", Site: "enisparta", File: "enisparta-20261027-110000.footops.tar", Bytes: 20, Created: time.Now(), Collections: 1, Documents: 89}}
	if _, body := c.do("GET", "/sites/enisparta", nil); !strings.Contains(body, `href="/sites/enisparta/exports/jobexport/download"`) {
		t.Error("site page lacks the export")
	}
	resp, body = c.do("GET", "/sites/enisparta/exports/jobexport/download", nil)
	if resp.StatusCode != 200 || body != snapshotBody || !strings.Contains(resp.Header.Get("Content-Disposition"), ".footops.tar") {
		t.Errorf("export download: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestJobPageLinks(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, false)
	e.login(t, c)
	for kind, want := range map[string]string{
		"import.check": `href="/sites/enisparta/import/R1"`,
		"import.apply": `href="/sites/enisparta/snapshots/R1/restore"`,
		"db.restore":   `href="/sites/enisparta/snapshots/R1/restore"`,
		"site.export":  `href="/sites/enisparta/exports/R1/download"`,
	} {
		e.agent.job = &agent.Job{ID: "j1", Kind: kind, Target: "enisparta", Done: true, Result: "R1", Started: time.Now(), Finished: time.Now()}
		_, body := c.do("GET", "/jobs/j1", nil)
		if !strings.Contains(body, want) || !strings.Contains(body, jobTitles[kind]) {
			t.Errorf("%s: lacks %s", kind, want)
		}
	}
	e.agent.job = &agent.Job{ID: "j1", Kind: "import.apply", Target: "enisparta", Done: true, Result: "R1", Err: "sayımlar tutmuyor (geri alındı, veri değişmedi)"}
	if _, body := c.do("GET", "/jobs/j1", nil); strings.Contains(body, "/restore") || !strings.Contains(body, "Başka dosya yükle") {
		t.Error("failed import offers an undo")
	}
}

// chunkSink reads chunks like the agent does, without keeping them.
type chunkSink struct{ fakeAgent }

func (c *chunkSink) PutChunk(_ context.Context, id string, n int, sum string, body io.Reader, length int64) (agent.UploadStatus, error) {
	_, err := io.Copy(io.Discard, body)
	return agent.UploadStatus{}, err
}

// TestChunkStreams guards rule K7 for uploads: a 32 MB chunk passes through the panel
// without being buffered.
func TestChunkStreams(t *testing.T) {
	e := newEnv(t)
	e.srv.agent = &chunkSink{}
	c := e.client(t, false)
	tok := e.login(t, c)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, _ := c.raw("PUT", "/uploads/"+testUpload+"/0", io.LimitReader(zeroReader{}, agent.MaxUploadChunk), http.Header{"X-Csrf-Token": {tok}})
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("32 MB chunk: %d bytes allocated in the whole process", alloc)
	if resp.StatusCode != 200 || alloc > 4<<20 {
		t.Errorf("status %d, allocated %d (limit 4 MB)", resp.StatusCode, alloc)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
