package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
)

// fakeDB is the agent's database browser as the panel sees it.
type fakeDB struct {
	overview agent.DBOverview
	page     agent.DBPage
	findErr  error
	doc      agent.DBDocument
	preview  agent.DBPreview
	edits    []agent.DBEdit
	view     agent.DBEditView
	writeErr error
	queries  []agent.DBQuery
	previews []agent.DBEditRequest
	replaced []agent.DBEditRequest
	deleted  []agent.DBEditRequest
	undone   []string
}

func (f *fakeAgent) DBCollections(context.Context, string) (agent.DBOverview, error) {
	return f.db.overview, nil
}

func (f *fakeAgent) DBFind(_ context.Context, site string, q agent.DBQuery) (agent.DBPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.queries = append(f.db.queries, q)
	return f.db.page, f.db.findErr
}

func (f *fakeAgent) DBDoc(_ context.Context, site, coll, id string) (agent.DBDocument, error) {
	if id == `"yok"` {
		return agent.DBDocument{}, &agent.Error{Status: 404, Message: "belge bulunamadı"}
	}
	d := f.db.doc
	d.ID = id
	return d, nil
}

func (f *fakeAgent) DBPreview(_ context.Context, site string, req agent.DBEditRequest) (agent.DBPreview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.previews = append(f.db.previews, req)
	if strings.Contains(req.Text, "bozuk") {
		return agent.DBPreview{}, &agent.Error{Status: 422, Message: "belge: satır 1, sütun 2: JSON hatası"}
	}
	return f.db.preview, nil
}

func (f *fakeAgent) DBReplace(_ context.Context, site string, req agent.DBEditRequest) (agent.DBEdit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.replaced = append(f.db.replaced, req)
	return agent.DBEdit{ID: "20261028-120000-000001-abcd", Changes: []agent.DBChange{{Field: "adet", Kind: "changed"}}}, f.db.writeErr
}

func (f *fakeAgent) DBDelete(_ context.Context, site string, req agent.DBEditRequest) (agent.DBEdit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.deleted = append(f.db.deleted, req)
	return agent.DBEdit{ID: "20261028-120000-000002-abcd"}, f.db.writeErr
}

func (f *fakeAgent) DBEdits(context.Context, string) ([]agent.DBEdit, error) { return f.db.edits, nil }

func (f *fakeAgent) DBEdit(_ context.Context, site, id string) (agent.DBEditView, error) {
	if f.db.view.Edit.ID != id {
		return agent.DBEditView{}, &agent.Error{Status: 404, Message: "kayıt bulunamadı"}
	}
	return f.db.view, nil
}

func (f *fakeAgent) DBUndo(_ context.Context, site, id string, a agent.Actor) (agent.DBEdit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.undone = append(f.db.undone, id)
	return agent.DBEdit{ID: "20261028-120000-000003-abcd", Collection: "urunler", DocID: "7", After: true}, f.db.writeErr
}

func dbEnv(t *testing.T) (*env, *client, string) {
	t.Helper()
	e := newEnv(t)
	c := e.client(t, false)
	tok := e.login(t, c)
	e.agent.dbSite = true
	now := time.Now()
	e.agent.db.overview = agent.DBOverview{DB: "enisparta", Collections: []agent.DBCollection{
		{Name: "urunler", Type: "collection", Count: 45, Size: 32000, Indexes: 2, IndexSize: 40960},
		{Name: "aktif ürünler", Type: "view", Count: -1},
	}, Took: 8 * time.Millisecond}
	e.agent.db.page = agent.DBPage{Collection: agent.DBCollection{Name: "urunler", Type: "collection"}, Page: 2, PerPage: 20,
		Total: 45, More: true, Filter: `{"adet": {"$gt": 5}}`, Sort: `{"tarih": -1}`,
		Indexes: []agent.DBIndex{{Name: "_id_", Keys: `{"_id": 1}`, TTL: -1}, {Name: "tarih_1", Keys: `{"tarih": 1}`, TTL: 3600}},
		Docs: []agent.DBDocSummary{{ID: `{"$oid": "6ae1c4a7ec711d712539599d"}`, IDText: `{"$oid": "6ae1c4a7ec711d712539599d"}`,
			Size: 300, Fields: 12, Rest: 9, Preview: []agent.DBField{{Key: "ad", Text: `"<b>Kalem</b>"`}, {Key: "password", Text: "••••"}}}}}
	e.agent.db.doc = agent.DBDocument{Collection: agent.DBCollection{Name: "urunler", Type: "collection"},
		Text: "{\n  \"_id\": 7,\n  \"ad\": \"</pre><script>alert(1)</script>\"\n}", Size: 120, Fields: 2, Hash: "abc123",
		Editable: true, Deletable: true}
	e.agent.db.edits = []agent.DBEdit{{ID: "20261028-110000-000001-abcd", Collection: "urunler", DocID: "7", Op: "edit",
		Time: now.Add(-time.Hour), Actor: "furkan", Changes: []agent.DBChange{{Field: "adet", Kind: "changed"}, {Field: "yeni", Kind: "added"}},
		Before: true, After: true}}
	return e, c, tok
}

func TestDBPages(t *testing.T) {
	e, c, _ := dbEnv(t)
	resp, body := c.do("GET", "/sites/enisparta/db", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("db page: %d", resp.StatusCode)
	}
	for _, want := range []string{`href="/sites/enisparta/db/coll?c=urunler"`, `href="/sites/enisparta/db/coll?c=aktif&#43;%C3%BCr%C3%BCnler"`,
		"görünüm", "45", "Son değişiklikler", `href="/sites/enisparta/db/undo/20261028-110000-000001-abcd"`, "adet, +yeni"} {
		if !strings.Contains(body, want) {
			t.Errorf("db page lacks %s", want)
		}
	}
	_, body = c.do("GET", "/sites/enisparta", nil)
	if !strings.Contains(body, `href="/sites/enisparta/db"`) {
		t.Error("site page has no link to the database")
	}

	resp, body = c.do("GET", "/sites/enisparta/db/coll?c=urunler&filter=%7B%22adet%22%3A1%7D&sort=-_id&osort=%7B%22tarih%22%3A-1%7D&page=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("collection page: %d", resp.StatusCode)
	}
	q := e.agent.db.queries[len(e.agent.db.queries)-1]
	if q.Collection != "urunler" || q.Filter != `{"adet":1}` || q.Sort != `{"tarih":-1}` || q.Page != 2 {
		t.Errorf("query: %+v", q)
	}
	for _, want := range []string{
		`href="/sites/enisparta/db/doc?c=urunler&amp;id=%7B%22%24oid%22%3A&#43;%226ae1c4a7ec711d712539599d%22%7D"`,
		`&#34;&lt;b&gt;Kalem&lt;/b&gt;&#34;`, "••••", "+9 alan", "21–21 / 45", "TTL: 3600 sn",
		// pages keep the filter and the sort
		`page=3`, `filter=%7B%22adet%22%3A&#43;%7B%22%24gt%22%3A&#43;5%7D%7D`, `sort=%7B%22tarih%22%3A&#43;-1%7D`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("collection page lacks %s", want)
		}
	}
	// A mistake in the filter shows the form again with the message.
	e.agent.db.findErr = &agent.Error{Status: 422, Message: "filtre: satır 1, sütun 3: JSON hatası"}
	resp, body = c.do("GET", "/sites/enisparta/db/coll?c=urunler&filter=%7Bx", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "JSON hatası") || !strings.Contains(body, ">{x</textarea>") {
		t.Errorf("bad filter: %d", resp.StatusCode)
	}
	e.agent.db.findErr = &agent.Error{Status: 404, Message: "koleksiyon yok"}
	if resp, _ := c.do("GET", "/sites/enisparta/db/coll?c=yok", nil); resp.StatusCode != 404 {
		t.Errorf("missing collection: %d", resp.StatusCode)
	}

	// The document text is escaped (it is the site's data).
	resp, body = c.do("GET", "/sites/enisparta/db/doc?c=urunler&id=7", nil)
	if resp.StatusCode != 200 || strings.Contains(body, "<script>alert(1)") || !strings.Contains(body, "&lt;/pre&gt;&lt;script&gt;") {
		t.Fatalf("document page: %d", resp.StatusCode)
	}
	if !strings.Contains(body, `href="/sites/enisparta/db/edit?c=urunler&amp;id=7"`) || !strings.Contains(body, `href="/sites/enisparta/db/delete?c=urunler&amp;id=7"`) {
		t.Error("document actions missing")
	}
	// A link to a document that is gone leads back to its collection, with a message.
	if resp, _ := c.do("GET", `/sites/enisparta/db/doc?c=urunler&id="yok"`, nil); resp.StatusCode != 303 ||
		resp.Header.Get("Location") != "/sites/enisparta/db/coll?c=urunler" {
		t.Errorf("missing document: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, path := range []string{"/sites/enisparta/db/doc?c=urunler", "/sites/enisparta/db/coll", "/sites/enisparta/db/doc?id=7"} {
		if resp, _ := c.do("GET", path, nil); resp.StatusCode != 404 {
			t.Errorf("%s: %d", path, resp.StatusCode)
		}
	}
	e.agent.dbSite = false
	if resp, _ := c.do("GET", "/sites/enisparta/db", nil); resp.StatusCode != 404 {
		t.Errorf("site without a database: %d", resp.StatusCode)
	}
}

func TestDBEditFlow(t *testing.T) {
	e, c, tok := dbEnv(t)
	resp, body := c.do("GET", "/sites/enisparta/db/edit?c=urunler&id=7", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="h" value="abc123"`) || !strings.Contains(body, "&lt;/pre&gt;&lt;script&gt;") {
		t.Fatalf("editor: %d", resp.StatusCode)
	}
	text := "{\n  \"_id\": 7,\n  \"adet\": 4\n}"
	form := url.Values{"csrf": {tok}, "c": {"urunler"}, "id": {"7"}, "h": {"abc123"}, "text": {strings.ReplaceAll(text, "\n", "\r\n")}, "do": {"preview"}}

	e.agent.db.preview = agent.DBPreview{Size: 40, Changes: []agent.DBChange{{Field: "adet", Kind: "changed", Old: "3", New: "4.0", OldType: "int", NewType: "double"}}}
	_, body = c.do("POST", "/sites/enisparta/db/edit", form)
	if len(e.agent.db.previews) != 1 || e.agent.db.previews[0].Text != text || e.agent.db.previews[0].Hash != "abc123" {
		t.Fatalf("preview request: %+v", e.agent.db.previews)
	}
	for _, want := range []string{"Değişiklikleri kontrol et", "tür int → double", `name="do" value="save"`, "<textarea name=\"text\" hidden>"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %s", want)
		}
	}
	// The editor's mistakes keep the text.
	bad := url.Values{"csrf": {tok}, "c": {"urunler"}, "id": {"7"}, "h": {"abc123"}, "text": {"bozuk"}, "do": {"preview"}}
	_, body = c.do("POST", "/sites/enisparta/db/edit", bad)
	if !strings.Contains(body, "JSON hatası") || !strings.Contains(body, ">bozuk</textarea>") {
		t.Errorf("mistake: %s", body)
	}

	form.Set("do", "save")
	resp, _ = c.do("POST", "/sites/enisparta/db/edit", form)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/db/doc?c=urunler&id=7" || len(e.agent.db.replaced) != 1 {
		t.Fatalf("save: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if a := e.agent.db.replaced[0].Actor; a.User != "furkan" {
		t.Errorf("actor: %+v", a)
	}
	_, body = c.do("GET", "/sites/enisparta/db/doc?c=urunler&id=7", nil)
	if !strings.Contains(body, "Kaydedildi (1 alan değişti)") {
		t.Error("no flash after saving")
	}
	// A conflict keeps the editor and the text, now against the document as it is: the
	// next preview shows the difference from that version.
	e.agent.db.writeErr = &agent.Error{Status: 409, Message: "belge sen açtıktan sonra değişmiş"}
	e.agent.db.doc.Hash = "yeni456"
	resp, body = c.do("POST", "/sites/enisparta/db/edit", form)
	if resp.StatusCode != 200 || !strings.Contains(body, "Belge sen açtıktan sonra değişti") || !strings.Contains(body, "&#34;adet&#34;: 4") ||
		!strings.Contains(body, `name="h" value="yeni456"`) || !strings.Contains(body, `value="preview"`) {
		t.Errorf("conflict: %d\n%s", resp.StatusCode, body)
	}
	// A write whose answer was lost is not called a failure.
	e.agent.db.writeErr = &agent.Error{Status: 504, Message: "cevabı alınamadı"}
	resp, _ = c.do("POST", "/sites/enisparta/db/edit", form)
	_, body = c.do("GET", resp.Header.Get("Location"), nil)
	if resp.StatusCode != 303 || !strings.Contains(body, "Sonuç belirsiz: cevabı alınamadı") {
		t.Errorf("unknown outcome: %d", resp.StatusCode)
	}
	if resp, _ := c.do("POST", "/sites/enisparta/db/edit", url.Values{"c": {"urunler"}, "id": {"7"}, "text": {"x"}, "do": {"save"}}); resp.StatusCode != 403 {
		t.Errorf("without CSRF: %d", resp.StatusCode)
	}
	// The form is multipart, as the browser sends it: a document's text may be larger
	// than other forms (up to dbEditForm), Turkish letters included.
	e.agent.db.writeErr = nil
	big := url.Values{"csrf": {tok}, "c": {"urunler"}, "id": {"7"}, "h": {"abc123"}, "text": {strings.Repeat("ğ", 1500<<10)}, "do": {"preview"}}
	if resp, body := c.multipart("/sites/enisparta/db/edit", big); resp.StatusCode != 200 || !strings.Contains(body, "Değişiklikleri kontrol et") {
		t.Errorf("large text: %d", resp.StatusCode)
	}
	if got := e.agent.db.previews[len(e.agent.db.previews)-1].Text; len(got) != 3000<<10 {
		t.Errorf("large text arrived as %d bytes", len(got))
	}
	huge := url.Values{"csrf": {tok}, "c": {"urunler"}, "id": {"7"}, "text": {strings.Repeat("x", dbEditForm)}, "do": {"preview"}}
	if resp, _ := c.multipart("/sites/enisparta/db/edit", huge); resp.StatusCode != 403 {
		t.Errorf("over the limit: %d", resp.StatusCode)
	}
	// A document that cannot be edited sends the editor back.
	e.agent.db.doc.Editable, e.agent.db.doc.Why = false, "belgede $ ile başlayan alan adları var"
	resp, _ = c.do("GET", "/sites/enisparta/db/edit?c=urunler&id=7", nil)
	if resp.StatusCode != 303 {
		t.Errorf("non-editable: %d", resp.StatusCode)
	}
}

func TestDBDeleteAndUndo(t *testing.T) {
	e, c, tok := dbEnv(t)
	_, body := c.do("GET", "/sites/enisparta/db/delete?c=urunler&id=7", nil)
	if !strings.Contains(body, "Belge silinsin mi?") || !strings.Contains(body, `name="h" value="abc123"`) {
		t.Fatal("delete confirmation")
	}
	resp, _ := c.do("POST", "/sites/enisparta/db/delete", url.Values{"csrf": {tok}, "c": {"urunler"}, "id": {"7"}, "h": {"abc123"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/db/coll?c=urunler" || len(e.agent.db.deleted) != 1 {
		t.Fatalf("delete: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	e.agent.db.view = agent.DBEditView{Edit: e.agent.db.edits[0], Before: "{\n  \"_id\": 7,\n  \"adet\": 3\n}", BeforeSize: 30, BeforeExact: true}
	_, body = c.do("GET", "/sites/enisparta/db/undo/20261028-110000-000001-abcd", nil)
	if !strings.Contains(body, "Değişiklik geri alınsın mı?") || !strings.Contains(body, "&#34;adet&#34;: 3") {
		t.Fatalf("undo confirmation:\n%s", body)
	}
	resp, _ = c.do("POST", "/sites/enisparta/db/undo/20261028-110000-000001-abcd", url.Values{"csrf": {tok}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/sites/enisparta/db/doc?c=urunler&id=7" {
		t.Fatalf("undo: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A conflicting undo stays on the page and offers the editor with the old version.
	e.agent.db.writeErr = &agent.Error{Status: 409, Message: "belge bu değişiklikten sonra yeniden değişmiş"}
	resp, body = c.do("POST", "/sites/enisparta/db/undo/20261028-110000-000001-abcd", url.Values{"csrf": {tok}})
	if resp.StatusCode != 200 || !strings.Contains(body, "yeniden değişmiş") ||
		!strings.Contains(body, `href="/sites/enisparta/db/edit?c=urunler&amp;from=20261028-110000-000001-abcd&amp;id=7"`) {
		t.Fatalf("undo conflict: %d\n%s", resp.StatusCode, body)
	}
	_, body = c.do("GET", "/sites/enisparta/db/edit?c=urunler&from=20261028-110000-000001-abcd&id=7", nil)
	if !strings.Contains(body, "&#34;adet&#34;: 3") || !strings.Contains(body, `name="h" value="abc123"`) {
		t.Errorf("editor from the old version:\n%s", body)
	}
	if resp, _ := c.do("GET", "/sites/enisparta/db/undo/yok", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown record: %d", resp.StatusCode)
	}
}

// multipart posts a form as multipart/form-data, as a browser sends enctype="multipart/form-data".
func (c *client) multipart(path string, form url.Values) (*http.Response, string) {
	c.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, vs := range form {
		for _, v := range vs {
			mw.WriteField(k, v)
		}
	}
	mw.Close()
	req, _ := http.NewRequest("POST", c.base+path, &buf)
	req.Host = c.host
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", c.origin)
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// A previous version whose text is not exact never goes into the editor.
func TestDBEditorRefusesLossyVersion(t *testing.T) {
	e, c, _ := dbEnv(t)
	e.agent.db.view = agent.DBEditView{Edit: e.agent.db.edits[0], Before: "{\n  \"_id\": 7,\n  \"ad\": \"Ka\uFFFDyıt\"\n}", BeforeSize: 30}
	resp, _ := c.do("GET", "/sites/enisparta/db/edit?c=urunler&from=20261028-110000-000001-abcd&id=7", nil)
	if resp.StatusCode != 303 {
		t.Errorf("lossy version in the editor: %d", resp.StatusCode)
	}
	e.agent.db.writeErr = &agent.Error{Status: 409, Message: "belge bu değişiklikten sonra yeniden değişmiş"}
	_, body := c.do("POST", "/sites/enisparta/db/undo/20261028-110000-000001-abcd", url.Values{"csrf": {csrfFrom(t, body0(c))}})
	if strings.Contains(body, "düzenleme formunda aç") {
		t.Error("the editor is offered for a lossy version")
	}
}

func body0(c *client) string {
	_, b := c.do("GET", "/sites/enisparta/db", nil)
	return b
}
