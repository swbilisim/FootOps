package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/datafs"
)

// fakeFM is the agent's file manager as the panel sees it.
type fakeFM struct {
	listing  datafs.Listing
	dirs     []string
	changes  []agent.FilesChange
	changeEr error
	places   []agent.PlaceRequest
	placeEr  error
	zips     []string
	unpacks  []agent.UnpackRequest
	trash    []datafs.TrashEntry
	restored []string
	emptied  []string
	urls     []string
	dlPaths  []string
}

func (f *fakeAgent) Files(_ context.Context, site, dir string, opt datafs.ListOptions) (datafs.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dir == "yok" {
		return datafs.Listing{}, &agent.Error{Status: 404, Message: "yok: bulunamadı"}
	}
	l := f.fm.listing
	l.Dir = dir
	return l, nil
}

func (f *fakeAgent) FileDirs(context.Context, string) ([]string, error) { return f.fm.dirs, nil }

func (f *fakeAgent) DownloadFile(_ context.Context, site, p string, a agent.Actor, hdr http.Header) (*http.Response, error) {
	f.mu.Lock()
	f.fm.dlPaths = append(f.fm.dlPaths, p+" "+hdr.Get("Range"))
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/octet-stream")
	rec.Header().Set("Content-Disposition", `attachment; filename="a.jpg"`)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header = hdr.Clone()
	http.ServeContent(rec, req, "", time.Time{}, strings.NewReader("jpeg"))
	return rec.Result(), nil
}

func (f *fakeAgent) ZipFiles(_ context.Context, site, dir string, names []string, a agent.Actor) (*http.Response, error) {
	f.mu.Lock()
	f.fm.zips = append(f.fm.zips, dir+":"+strings.Join(names, ","))
	f.mu.Unlock()
	switch dir {
	case "yok":
		return nil, &agent.Error{Status: 404, Message: "yok: bulunamadı"}
	case "kirik": // the agent's stream breaks half-way
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/zip"}},
			Body: io.NopCloser(io.MultiReader(strings.NewReader("PK yarım"), errReader{}))}, nil
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/zip")
	rec.WriteString("PK zip")
	return rec.Result(), nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (f *fakeAgent) ChangeFiles(_ context.Context, site string, ch agent.FilesChange) (*datafs.TrashEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.changes = append(f.fm.changes, ch)
	if f.fm.changeEr != nil {
		return nil, f.fm.changeEr
	}
	if ch.Op == "trash" {
		return &datafs.TrashEntry{ID: "20261028-093000-abcdef", Files: 3, Bytes: 2048}, nil
	}
	return nil, nil
}

func (f *fakeAgent) PlaceUpload(_ context.Context, site string, req agent.PlaceRequest) (datafs.Placed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.places = append(f.fm.places, req)
	if f.fm.placeEr != nil {
		return datafs.Placed{}, f.fm.placeEr
	}
	return datafs.Placed{Name: f.upload.Name, Size: f.upload.Size, Linked: true}, nil
}

func (f *fakeAgent) Unpack(_ context.Context, site string, req agent.UnpackRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.unpacks = append(f.fm.unpacks, req)
	return "jobunpack", nil
}

func (f *fakeAgent) Trash(context.Context, string) ([]datafs.TrashEntry, error) {
	return f.fm.trash, nil
}

func (f *fakeAgent) RestoreTrash(_ context.Context, site, id string, a agent.Actor) (datafs.Restored, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.restored = append(f.fm.restored, id)
	return datafs.Restored{Dir: "fotolar", Items: []datafs.Renamed{{Name: "a.jpg", As: "a (1).jpg"}, {Name: "b.jpg", As: "b.jpg"}}}, nil
}

func (f *fakeAgent) EmptyTrash(_ context.Context, site, confirm string, a agent.Actor) (agent.EmptyTrashResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.emptied = append(f.fm.emptied, confirm)
	return agent.EmptyTrashResult{Entries: 2, Bytes: 4096}, nil
}

func (f *fakeAgent) SetDataURL(_ context.Context, site, u string, a agent.Actor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fm.urls = append(f.fm.urls, u)
	return nil
}

func filesEnv(t *testing.T) (*env, *client, string) {
	t.Helper()
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dataSite = true
	now := time.Now()
	e.agent.fm.listing = datafs.Listing{Total: 4, Dirs: 1, Files: 2, Bytes: 3000, Hidden: 1, Entries: []datafs.Entry{
		{Name: "alt klasör", Type: "dir", MTime: now},
		{Name: "çiçek #1.jpg", Type: "file", Size: 2000, MTime: now},
		{Name: "arşiv.zip", Type: "file", Size: 1000, MTime: now},
		{Name: "tab?bozuk", Type: "file", Odd: true, MTime: now},
	}}
	return e, c, tok
}

func TestFilesPage(t *testing.T) {
	e, c, _ := filesEnv(t)
	e.agent.dataURL = "/uploads"
	resp, body := c.do("GET", "/sites/enisparta/files?dir=fotolar/2024", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{
		`href="/sites/enisparta/files?dir=fotolar"`,                            // breadcrumb
		`href="/sites/enisparta/files?dir=fotolar%2f2024%2falt%20klas%c3%b6r"`, // folder link
		`href="/sites/enisparta/files/download?path=fotolar%2f2024%2f%c3%a7i%c3%a7ek%20%231.jpg"`,
		`href="https://enisparta.com/uploads/fotolar/2024/%C3%A7i%C3%A7ek%20%231.jpg"`, // site link, escaped
		`href="/sites/enisparta/files/unpack?path=fotolar%2f2024%2far%c5%9fiv.zip"`,
		`data-mode="files"`, `name="dir" value="fotolar/2024"`, `value="çiçek #1.jpg"`,
		`1 gizli öğe`, `/static/upload.js`, `action="/sites/enisparta/files/act"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %s", want)
		}
	}
	if strings.Contains(body, `value="tab?bozuk"`) {
		t.Error("an odd name can be selected")
	}
	for path, code := range map[string]int{"/sites/enisparta/files?dir=../x": 404, "/sites/enisparta/files?dir=.git": 404,
		"/sites/enisparta/files?dir=yok": 404} {
		if resp, _ := c.do("GET", path, nil); resp.StatusCode != code {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	e.agent.dataSite = false
	if resp, _ := c.do("GET", "/sites/enisparta/files", nil); resp.StatusCode != 404 {
		t.Errorf("site without a data directory: %d", resp.StatusCode)
	}
}

func TestFilesChanges(t *testing.T) {
	e, c, tok := filesEnv(t)
	resp, _ := c.do("POST", "/sites/enisparta/files/mkdir", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "name": {" yeni "}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/files?dir=fotolar" {
		t.Errorf("mkdir: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := c.do("POST", "/sites/enisparta/files/mkdir", url.Values{"dir": {"fotolar"}, "name": {"x"}}); resp.StatusCode != 403 {
		t.Errorf("mkdir without CSRF: %d", resp.StatusCode)
	}
	// The selection form opens the trash confirmation and the move dialog.
	_, body := c.do("POST", "/sites/enisparta/files/act", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "n": {"a.jpg", "b"}, "do": {"trash"}})
	if !strings.Contains(body, "2 öğe çöp kutusuna taşınsın mı?") || !strings.Contains(body, `name="n" value="b"`) {
		t.Errorf("trash confirmation: %s", body)
	}
	e.agent.fm.dirs = []string{"b", "b/iç", "başka", "fotolar"}
	_, body = c.do("POST", "/sites/enisparta/files/act", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "n": {"a.jpg", "b"}, "do": {"move"}})
	if !strings.Contains(body, `<option value="">/</option>`) || !strings.Contains(body, `<option value="başka">`) {
		t.Errorf("move targets: %s", body)
	}
	// fotolar/b and below are inside the selection; fotolar is where they are.
	if strings.Contains(body, `<option value="fotolar">`) {
		t.Errorf("move offers the current folder")
	}
	resp, _ = c.do("POST", "/sites/enisparta/files/move", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "n": {"a.jpg"}, "to": {"başka"}})
	if resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "dir=ba%C5%9Fka") {
		t.Errorf("move: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = c.do("POST", "/sites/enisparta/files/trash", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "n": {"a.jpg", "b"}})
	if resp.StatusCode != 303 {
		t.Errorf("trash: %d", resp.StatusCode)
	}
	_, body = c.do("GET", "/sites/enisparta/files?dir=fotolar", nil)
	if !strings.Contains(body, "2 öğe çöp kutusuna taşındı (3 dosya, 2.0 KiB)") {
		t.Errorf("flash after trash missing")
	}
	resp, _ = c.do("POST", "/sites/enisparta/files/rename", url.Values{"csrf": {tok}, "dir": {""}, "name": {"a.jpg"}, "to": {"b.jpg"}})
	if resp.StatusCode != 303 {
		t.Errorf("rename: %d", resp.StatusCode)
	}
	e.agent.fm.changeEr = &agent.Error{Status: 409, Message: "b.jpg: aynı adda bir öğe var"}
	resp, body = c.do("POST", "/sites/enisparta/files/rename", url.Values{"csrf": {tok}, "dir": {""}, "name": {"a.jpg"}, "to": {"b.jpg"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "aynı adda bir öğe var") || !strings.Contains(body, `value="b.jpg"`) {
		t.Errorf("rename refused: %d", resp.StatusCode)
	}
	// Names the panel would never send are refused before the agent sees them.
	for _, n := range []string{"../x", ".env", "a/b"} {
		if resp, _ := c.do("POST", "/sites/enisparta/files/trash", url.Values{"csrf": {tok}, "dir": {""}, "n": {n}}); resp.StatusCode != 404 {
			t.Errorf("trash %q: %d", n, resp.StatusCode)
		}
	}
	got := []string{}
	for _, ch := range e.agent.fm.changes {
		got = append(got, ch.Op+":"+ch.Dir+":"+ch.Name+strings.Join(ch.Names, ",")+">"+ch.To)
	}
	want := "mkdir:fotolar:yeni> move:fotolar:a.jpg>başka trash:fotolar:a.jpg,b> rename::a.jpg>b.jpg rename::a.jpg>b.jpg"
	if strings.Join(got, " ") != want {
		t.Errorf("changes = %q", got)
	}
}

func TestFilesDownloads(t *testing.T) {
	e, c, tok := filesEnv(t)
	c.header = http.Header{"Range": {"bytes=2-"}}
	resp, body := c.do("GET", "/sites/enisparta/files/download?path=fotolar/a.jpg", nil)
	c.header = nil
	if resp.StatusCode != 206 || body != "eg" || e.agent.fm.dlPaths[0] != "fotolar/a.jpg bytes=2-" {
		t.Errorf("download: %d %q %v", resp.StatusCode, body, e.agent.fm.dlPaths)
	}
	if resp, _ := c.do("HEAD", "/sites/enisparta/files/download?path=fotolar/a.jpg", nil); resp.StatusCode != 405 {
		t.Errorf("HEAD: %d", resp.StatusCode)
	}
	if resp, _ := c.do("GET", "/sites/enisparta/files/download?path=../etc/passwd", nil); resp.StatusCode != 404 {
		t.Errorf("escape: %d", resp.StatusCode)
	}
	resp, body = c.do("GET", "/sites/enisparta/files/zip?dir=fotolar", nil)
	if resp.StatusCode != 200 || body != "PK zip" || resp.Header.Get("Content-Type") != "application/zip" {
		t.Errorf("folder zip: %d %q", resp.StatusCode, body)
	}
	resp, body = c.do("POST", "/sites/enisparta/files/act", url.Values{"csrf": {tok}, "dir": {"fotolar"}, "n": {"a.jpg", "b"}, "do": {"zip"}})
	if resp.StatusCode != 200 || body != "PK zip" || strings.Join(e.agent.fm.zips, " ") != "fotolar: fotolar:a.jpg,b" {
		t.Errorf("selection zip: %d %q %v", resp.StatusCode, body, e.agent.fm.zips)
	}
}

// A refusal goes back to the folder with a message; a stream that breaks reaches the
// browser as a failed download, not as a whole (truncated) file.
func TestFilesDownloadFailures(t *testing.T) {
	e, c, _ := filesEnv(t)
	resp, _ := c.do("GET", "/sites/enisparta/files/zip?dir=yok", nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/files?dir=yok" {
		t.Errorf("refused zip: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	req, _ := http.NewRequest("GET", c.base+"/sites/enisparta/files/zip?dir=kirik", nil)
	req.Host = c.host
	// Before or after the headers, the client must see an error.
	if resp, err := c.hc.Do(req); err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Error("a broken stream looked complete")
		}
	}
	_ = e
}

func TestFilesPlace(t *testing.T) {
	e, c, tok := filesEnv(t)
	e.agent.upload = agent.UploadStatus{Upload: agent.Upload{ID: testUpload, Site: "enisparta", Name: "kapak.jpg", Size: 4}}
	place := func(body string) *http.Response {
		resp, _ := c.raw("POST", "/sites/enisparta/files/place", strings.NewReader(body), http.Header{csrfHeader: {tok}, "Content-Type": {"application/json"}})
		return resp
	}
	if resp := place(`{"upload":"` + testUpload + `","dir":"fotolar","overwrite":true}`); resp.StatusCode != 200 {
		t.Fatalf("place: %d", resp.StatusCode)
	}
	if !e.agent.upload.Finished {
		t.Error("the upload was not finished first")
	}
	e.agent.upload.Name = "arka.jpg"
	place(`{"upload":"` + testUpload + `","dir":"fotolar"}`)
	if len(e.agent.fm.places) != 2 || e.agent.fm.places[0] != (agent.PlaceRequest{Upload: testUpload, Dir: "fotolar", Overwrite: true,
		Actor: e.agent.fm.places[0].Actor}) || e.agent.fm.places[0].Actor.User != "furkan" {
		t.Errorf("places = %+v", e.agent.fm.places)
	}
	// Both uploads show up on the next page.
	_, body := c.do("GET", "/sites/enisparta/files?dir=fotolar", nil)
	if !strings.Contains(body, "“kapak.jpg” yüklendi. “arka.jpg” yüklendi.") {
		t.Errorf("flash missing")
	}
	e.agent.fm.placeErr(&agent.Error{Status: 423, Message: "bu sitede bir işlem sürüyor"})
	if resp := place(`{"upload":"` + testUpload + `","dir":""}`); resp.StatusCode != 423 {
		t.Errorf("busy: %d", resp.StatusCode)
	}
	for _, bad := range []string{`{"upload":"../x","dir":""}`, `{"upload":"` + testUpload + `","dir":"../x"}`} {
		if resp := place(bad); resp.StatusCode/100 != 4 {
			t.Errorf("%s: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := c.raw("POST", "/sites/enisparta/files/place", strings.NewReader(`{}`), http.Header{"Content-Type": {"application/json"}}); resp.StatusCode != 403 {
		t.Errorf("without CSRF: %d", resp.StatusCode)
	}
}

func (f *fakeFM) placeErr(err error) { f.placeEr = err }

func TestFilesUploadWithoutJavaScript(t *testing.T) {
	e, c, tok := filesEnv(t)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf", tok)
	mw.WriteField("dir", "fotolar")
	mw.WriteField("overwrite", "on")
	for _, name := range []string{"a.jpg", "b.jpg"} {
		fw, _ := mw.CreateFormFile("file", name)
		fw.Write([]byte("veri " + name))
	}
	mw.Close()
	resp, _ := c.raw("POST", "/sites/enisparta/files/upload", &buf, http.Header{"Content-Type": {mw.FormDataContentType()}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/files?dir=fotolar" {
		t.Fatalf("form: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(e.agent.fm.places) != 2 || !e.agent.fm.places[1].Overwrite || e.agent.fm.places[1].Dir != "fotolar" || string(e.agent.single) != "veri b.jpg" {
		t.Errorf("places = %+v, last body %q", e.agent.fm.places, e.agent.single)
	}
	var bad bytes.Buffer
	mw = multipart.NewWriter(&bad)
	mw.WriteField("csrf", "yanlış")
	mw.Close()
	if resp, _ := c.raw("POST", "/sites/enisparta/files/upload", &bad, http.Header{"Content-Type": {mw.FormDataContentType()}}); resp.StatusCode != 403 {
		t.Errorf("wrong token: %d", resp.StatusCode)
	}
}

func TestTrashPages(t *testing.T) {
	e, c, tok := filesEnv(t)
	e.agent.fm.listing.Trash = 2
	e.agent.fm.trash = []datafs.TrashEntry{{ID: "20261028-093000-abcdef", Dir: "fotolar", Names: []string{"a.jpg", "b.jpg"}, Files: 2, Bytes: 3000,
		Deleted: time.Now(), Actor: "furkan"}, {ID: "20261027-093000-123456", Names: []string{"kapak.jpg"}, Files: 1, Reason: "overwrite", Deleted: time.Now()}}
	_, body := c.do("GET", "/sites/enisparta/trash", nil)
	for _, want := range []string{"a.jpg, b.jpg", "/fotolar", "yüklenen dosya yerine geçti", `action="/sites/enisparta/trash/20261028-093000-abcdef/restore"`} {
		if !strings.Contains(body, want) {
			t.Errorf("trash page lacks %s", want)
		}
	}
	_, body = c.do("GET", "/sites/enisparta/files", nil)
	if !strings.Contains(body, "Çöp kutusu (2)") {
		t.Error("files page lacks the trash count")
	}
	resp, _ := c.do("POST", "/sites/enisparta/trash/20261028-093000-abcdef/restore", url.Values{"csrf": {tok}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/files?dir=fotolar" {
		t.Errorf("restore: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body = c.do("GET", "/sites/enisparta/files?dir=fotolar", nil)
	if !strings.Contains(body, "“a.jpg” → “a (1).jpg”") {
		t.Error("restore flash lacks the new name")
	}
	if resp, _ := c.do("POST", "/sites/enisparta/trash/../restore", url.Values{"csrf": {tok}}); resp.StatusCode == 303 {
		t.Error("bad id accepted")
	}
	resp, body = c.do("POST", "/sites/enisparta/trash/empty", url.Values{"csrf": {tok}, "confirm": {"yanlış"}})
	if resp.StatusCode != 422 || len(e.agent.fm.emptied) != 0 {
		t.Errorf("empty without the name: %d", resp.StatusCode)
	}
	resp, _ = c.do("POST", "/sites/enisparta/trash/empty", url.Values{"csrf": {tok}, "confirm": {"enisparta"}})
	if resp.StatusCode != 303 || len(e.agent.fm.emptied) != 1 {
		t.Errorf("empty: %d %v", resp.StatusCode, e.agent.fm.emptied)
	}
}

func TestFilesUnpackAndURL(t *testing.T) {
	e, c, tok := filesEnv(t)
	_, body := c.do("GET", "/sites/enisparta/files/unpack?path=gelen/arşiv.zip", nil)
	if !strings.Contains(body, `name="path" value="gelen/arşiv.zip"`) || !strings.Contains(body, "Arşiv açılsın mı?") {
		t.Errorf("unpack confirmation: %s", body)
	}
	resp, _ := c.do("POST", "/sites/enisparta/files/unpack", url.Values{"csrf": {tok}, "path": {"gelen/arşiv.zip"}, "trash_archive": {"on"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/jobs/jobunpack" || !e.agent.fm.unpacks[0].TrashArchive {
		t.Errorf("unpack: %d %s %+v", resp.StatusCode, resp.Header.Get("Location"), e.agent.fm.unpacks)
	}
	resp, _ = c.do("POST", "/sites/enisparta/files/url", url.Values{"csrf": {tok}, "url": {" /uploads "}, "dir": {"fotolar"}})
	if resp.StatusCode != 303 || e.agent.fm.urls[0] != "/uploads" {
		t.Errorf("url: %d %v", resp.StatusCode, e.agent.fm.urls)
	}
}

// The upload script's file mode is there and stays small.
func TestUploadScript(t *testing.T) {
	b, err := assets.ReadFile("static/upload.js")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`data-mode`)) || len(b) > 12<<10 {
		t.Errorf("upload.js: %d bytes", len(b))
	}
	_ = io.Discard
}
