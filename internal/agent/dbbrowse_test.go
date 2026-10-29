package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/swbilisim/FootOps/internal/mongo"
	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- without a server ----

func TestParseFilter(t *testing.T) {
	for _, in := range []string{"", "  ", "{}"} {
		d, norm, err := parseFilter(in)
		if err != nil || !bytes.Equal(d, mongo.Empty) || norm != "" {
			t.Errorf("%q: %v %q", in, err, norm)
		}
	}
	d, norm, err := parseFilter(`{"adet": {"$gt": 5}, "ad": {"$regex": "ürün", "$options": "i"}}`)
	if err != nil || len(d) == 0 || norm != `{"adet": {"$gt": 5}, "ad": {"$regex": "ürün", "$options": "i"}}` {
		t.Fatalf("%v %q", err, norm)
	}
	for _, bad := range []string{
		`{"$where": "sleep(1000)"}`,
		`{"a": {"$elemMatch": {"$where": "1"}}}`,
		`{"$expr": {"$function": {"body": "x", "args": [], "lang": "js"}}}`,
		`{"$or": [{"a": 1}, {"$expr": {"$accumulator": {}}}]}`,
		`[1]`, `{"a": }`, strings.Repeat(" ", dbFilterMax) + "{}x",
	} {
		_, _, err := parseFilter(bad)
		var de *DBError
		if !errors.As(err, &de) {
			t.Errorf("%.60q: %v", bad, err)
		}
	}
}

func TestParseSort(t *testing.T) {
	for in, want := range map[string]string{"": "-_id", "-_id": "-_id", "_id": "_id", `{"tarih": -1, "ad": 1}`: `{"tarih": -1, "ad": 1}`,
		`{"tarih": -1.0}`: `{"tarih": -1.0}`} {
		_, norm, err := parseSort(in)
		if err != nil || norm != want {
			t.Errorf("%q: %q %v", in, norm, err)
		}
	}
	for _, bad := range []string{`{"a": 2}`, `{"a": "asc"}`, `{}`, `{"$natural": 1}`, `{"a":1,"b":1,"c":1,"d":1,"e":1}`, `[1]`, `{"": 1}`} {
		if _, _, err := parseSort(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func oidValue(t *testing.T, hexID string) mongo.Value {
	t.Helper()
	v, err := mongo.ParseValue(`{"$oid": "` + hexID + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseEdited(t *testing.T) {
	cur := mongo.NewBuilder().Value("_id", oidValue(t, "6ae1c4a7ec711d712539599d")).String("ad", "y").MustBuild()
	d, err := parseEdited(`{"ad": "x", "_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "n": 5}`, cur)
	if err != nil {
		t.Fatal(err)
	}
	elems, _ := d.Elems()
	if elems[0].Key != "_id" || elems[1].Key != "ad" || elems[2].Key != "n" {
		t.Fatalf("_id not moved first: %v", elems)
	}
	for in, want := range map[string]string{
		`{"ad": "x"}`: "_id alanı silinemez",
		`{"_id": {"$oid": "6ae1c4a7ec711d712539599e"}}`:                                                "_id değiştirilemez",
		`{"_id": "6ae1c4a7ec711d712539599d"}`:                                                          "_id değiştirilemez",
		`{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "$set": {"a": 1}}`:                              "$ ile başlayamaz",
		`{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "a": {"$gt": 1}}`:                               "iç belgelerde",
		`{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "a": 1, "a": 2}`:                                "iki kez",
		`{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "s": "` + strings.Repeat("x", DBEditMax) + `"}`: "çok büyük",
	} {
		_, err := parseEdited(in, cur)
		var de *DBError
		if !errors.As(err, &de) || !strings.Contains(err.Error(), want) {
			t.Errorf("%.70s: %v, want %q", in, err, want)
		}
	}
	// The stored _id decides, type included: a double 5.0 is not the int 5.
	five := mongo.NewBuilder().Double("_id", 5).String("a", "b").MustBuild()
	if _, err := parseEdited(`{"_id": 5, "a": "c"}`, five); err == nil || !strings.Contains(err.Error(), "_id değiştirilemez") {
		t.Errorf("_id type change: %v", err)
	}
	if _, err := parseEdited(`{"_id": 5.0, "a": "c"}`, five); err != nil {
		t.Errorf("same _id: %v", err)
	}
	// A mistake in the text is marked so (the audit log names the kind, not the text).
	_, err = parseEdited(`{"_id": 5.0, "n": 98765432109876543210}`, five)
	var de *DBError
	if !errors.As(err, &de) || !de.Text || de.Audit() != "düzenlenen metin geçersiz" || strings.Contains(de.Audit(), "9876") {
		t.Errorf("text error: %#v", err)
	}
}

func TestParseIDRefusesArrays(t *testing.T) {
	for _, bad := range []string{"", "[1]", "{", strings.Repeat("1", dbIDTextMax+1)} {
		if _, err := parseID(bad); err == nil {
			t.Errorf("%.20q accepted", bad)
		}
	}
	for _, good := range []string{`"u1"`, "5", `{"$oid": "6ae1c4a7ec711d712539599d"}`, `{"a": 1}`, `{"$gt": 1}`} {
		if _, err := parseID(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
}

func TestDiffDocs(t *testing.T) {
	old, _ := mongo.ParseDoc(`{"_id": 1, "ad": "x", "n": 5, "eski": true, "ic": {"a": 1}}`)
	new, _ := mongo.ParseDoc(`{"_id": 1, "ad": "y", "n": 5.0, "yeni": null, "ic": {"a": 1}}`)
	got := diffDocs(old, new)
	want := []DBChange{
		{Field: "ad", Kind: "changed", Old: `"x"`, New: `"y"`},
		{Field: "n", Kind: "changed", Old: "5", New: "5.0", OldType: "int", NewType: "double"},
		{Field: "yeni", Kind: "added", New: "null", NewType: "null"},
		{Field: "eski", Kind: "removed", Old: "true", OldType: "bool"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if names := changeNames(got, 0); names != "alanlar: ad, n (int→double), +yeni, −eski" {
		t.Errorf("audit names: %q", names)
	}
	if names := changeNames(got[:1], 41); names != "alanlar: ad, +41 alan daha" {
		t.Errorf("audit names, capped: %q", names)
	}
}

func TestEditable(t *testing.T) {
	coll := DBCollection{Type: "collection"}
	good, _ := mongo.ParseDoc(`{"_id": 1, "a": "b"}`)
	if ok, why := editable(coll, good); !ok {
		t.Fatal(why)
	}
	if ok, _ := editable(DBCollection{Type: "view"}, good); ok {
		t.Error("view editable")
	}
	cases := map[string]mongo.Doc{
		"UTF-8":     mongo.NewBuilder().Int32("_id", 1).String("s", "a\xff").MustBuild(),
		"$ ile":     mongo.NewBuilder().Int32("_id", 1).StartDoc("x").Int32("$y", 1).End().MustBuild(),
		"iki kez":   mongo.NewBuilder().Int32("_id", 1).Int32("a", 1).Int32("a", 2).MustBuild(),
		"ilk alanı": mongo.NewBuilder().Int32("a", 1).Int32("_id", 1).MustBuild(),
		"büyük":     mongo.NewBuilder().Int32("_id", 1).String("s", strings.Repeat("x", DBEditMax)).MustBuild(),
	}
	for want, d := range cases {
		if ok, why := editable(coll, d); ok || !strings.Contains(why, want) {
			t.Errorf("%s: %v %q", want, ok, why)
		}
	}
}

func TestSummaryMasksSecrets(t *testing.T) {
	row, err := mongo.ParseDoc(`{"i": {"$oid": "6ae1c4a7ec711d712539599d"}, "z": 300, "n": 6, "f": [
		{"k": "_id", "t": "objectId", "v": {"$oid": "6ae1c4a7ec711d712539599d"}},
		{"k": "email", "t": "string", "v": "a@b.c", "l": 5},
		{"k": "passwordHash", "t": "string", "v": "$2b$10$abc", "l": 10},
		{"k": "Şifre", "t": "string", "v": "x", "l": 1},
		{"k": "uzun", "t": "string", "v": "` + strings.Repeat("ğ", 300) + `", "l": 600},
		{"k": "çokuzun", "t": "string", "l": 5000},
		{"t": "string", "v": "adı uzun", "l": 8},
		{"k": "ic", "t": "object", "l": 3},
		{"k": "liste", "t": "array", "l": 2},
		{"k": "dosya", "t": "binData", "l": 2048},
		{"k": "tarih", "t": "date", "v": {"$date": "2026-10-28T09:47:19.647Z"}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	s := summaryOf(row)
	if s.ID != `{"$oid": "6ae1c4a7ec711d712539599d"}` || s.Size != 300 || s.Fields != 6 || s.Rest != 0 {
		t.Fatalf("%+v", s)
	}
	want := map[string]string{"email": `"a@b.c"`, "passwordHash": "••••", "Şifre": "••••",
		"uzun": `"` + strings.Repeat("ğ", dbPreviewChars-1) + `…"`, "çokuzun": "metin, 4.9 KB",
		"(çok uzun alan adı)": `"adı uzun"`, "ic": "{…} 3 alan", "liste": "[…] 2 öğe"} // the first 8 fields
	if len(s.Preview) != len(want) {
		t.Fatalf("preview %+v", s.Preview)
	}
	for _, f := range s.Preview {
		if want[f.Key] != f.Text {
			t.Errorf("%s: %q, want %q", f.Key, f.Text, want[f.Key])
		}
	}
}

func TestEditRecords(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	d := &DataOps{DBEditsDir: t.TempDir(), now: func() time.Time { return now }}
	d.init()
	before, _ := mongo.ParseDoc(`{"_id": 1, "a": "eski"}`)
	after, _ := mongo.ParseDoc(`{"_id": 1, "a": "yeni"}`)
	rec := d.newEdit("magaza", "urunler", "1", "edit", Actor{User: "furkan"}, diffDocs(before, after))
	if rec.Changes[0].Old != "" || rec.Changes[0].New != "" {
		t.Fatal("values kept in the record")
	}
	rec.Before, rec.After = true, true
	if err := d.saveEdit(&rec, before, after); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(d.editDir("magaza", rec.ID))
	if st.Mode().Perm() != 0o700 {
		t.Errorf("record dir mode %v", st.Mode().Perm())
	}
	got, b, a, err := d.loadEdit("magaza", rec.ID)
	if err != nil || !bytes.Equal(b, before) || !bytes.Equal(a, after) || got.Actor != "furkan" {
		t.Fatalf("load: %v", err)
	}
	// A document of thousands of changed fields still leaves a readable record.
	var many []DBChange
	for i := range 20000 {
		many = append(many, DBChange{Field: strings.Repeat("alan", 40) + strconv.Itoa(i), Kind: "changed", Old: "x", New: "y"})
	}
	big := d.newEdit("magaza", "urunler", "2", "edit", Actor{}, many)
	big.Before, big.After = true, true
	if err := d.saveEdit(&big, before, after); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := d.loadEdit("magaza", big.ID); err != nil || len(got.Changes) != dbRecordChanges || got.More != 20000-dbRecordChanges ||
		utf8.RuneCountInString(got.Changes[0].Field) > 128 {
		t.Fatalf("large record: %v %d %d", err, len(got.Changes), got.More)
	}
	d.dropEdit(big)
	meta, _ := os.ReadFile(filepath.Join(d.editDir("magaza", rec.ID), dbEditMeta))
	if strings.Contains(string(meta), "eski") || strings.Contains(string(meta), "yeni") {
		t.Errorf("values in edit.json:\n%s", meta)
	}
	list, err := d.DBEdits("magaza")
	if err != nil || len(list) != 1 || list[0].ID != rec.ID {
		t.Fatalf("list: %v %v", list, err)
	}
	v, err := d.DBEditView("magaza", rec.ID)
	if err != nil || !strings.Contains(v.Before, `"eski"`) || !strings.Contains(v.After, `"yeni"`) || !v.BeforeExact {
		t.Fatalf("view: %+v %v", v, err)
	}
	for _, bad := range []string{"../x", "20261028-120000-000000-zzzz", "x"} {
		if _, _, _, err := d.loadEdit("magaza", bad); !errors.Is(err, ErrDBNotFound) {
			t.Errorf("%s: %v", bad, err)
		}
	}

	// A write cut short (no record file) and records past their keep time are swept.
	os.MkdirAll(filepath.Join(d.DBEditsDir, "magaza", "20261028-100000-000000-aaaa"), 0o700)
	now = now.Add(2 * time.Hour)
	if removed := d.SweepDBEdits(); len(removed) != 1 || !strings.HasSuffix(removed[0], "-aaaa") {
		t.Fatalf("sweep: %v", removed)
	}
	now = now.Add(DBEditKeep)
	if removed := d.SweepDBEdits(); len(removed) != 1 {
		t.Fatalf("sweep old: %v", removed)
	}
}

// ---- with a real MongoDB (FOOTOPS_TEST_MONGO) ----

type liveDBEnv struct {
	*dataEnv
	site  string
	admin *mongo.Conn // root, to set data up behind the panel's back
	srv   *Server
}

// newLiveDB creates a site whose database user exists in the test MongoDB.
func newLiveDB(t *testing.T) *liveDBEnv {
	t.Helper()
	raw := os.Getenv("FOOTOPS_TEST_MONGO")
	if raw == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok: gerçek MongoDB testleri atlandı")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatal(err)
	}
	rootPW, _ := u.User.Password()
	ctx := context.Background()
	root, err := mongo.Dial(ctx, u.Host, mongo.Credential{User: u.User.Username(), Password: rootPW, Source: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	site := "f5t-" + hex.EncodeToString(b)

	e := newOps(t)
	if err := e.ops.Create(ctx, specFor(site), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	d := &dataEnv{opsEnv: e, now: time.Now()}
	d.data = &DataOps{Store: sites.Store{Root: e.root}, Docker: e.net, MongoAddr: u.Host,
		DBEditsDir: filepath.Join(e.root, "dbedits"), now: time.Now}
	_, creds, err := d.data.siteDB(site)
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, site, mongo.NewBuilder().String("createUser", creds.User).String("pwd", creds.Password).
		StartArray("roles").StartDoc("").String("role", "readWrite").String("db", site).End().End().MustBuild())
	t.Cleanup(func() {
		_, _ = root.Run(ctx, site, mongo.NewBuilder().String("dropUser", creds.User).MustBuild(), 1<<20)
		_, _ = root.Run(ctx, site, mongo.NewBuilder().Int32("dropDatabase", 1).MustBuild(), 1<<20)
		root.Close()
	})
	srv := &Server{Sites: e.ops, Data: d.data, Jobs: NewJobs(), Audit: NewAudit(filepath.Join(e.root, "audit.jsonl")),
		AllowedUIDs: map[uint32]bool{0: true}, PeerUID: nil}
	return &liveDBEnv{dataEnv: d, site: site, admin: root, srv: srv}
}

func mustRun(t *testing.T, c *mongo.Conn, db string, cmd mongo.Doc) mongo.Doc {
	t.Helper()
	reply, err := c.Run(context.Background(), db, cmd, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := mongo.WriteErr(reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func (e *liveDBEnv) insert(t *testing.T, coll string, docs ...string) {
	t.Helper()
	b := mongo.NewBuilder().String("insert", coll).StartArray("documents")
	for _, text := range docs {
		d, err := mongo.ParseDoc(text)
		if err != nil {
			t.Fatal(err)
		}
		b.Doc("", d)
	}
	mustRun(t, e.admin, e.site, b.End().MustBuild())
}

func (e *liveDBEnv) stored(t *testing.T, coll, id string) mongo.Doc {
	t.Helper()
	v, _ := mongo.ParseValue(id)
	reply := mustRun(t, e.admin, e.site, mongo.NewBuilder().String("find", coll).Doc("filter", byID(v)).MustBuild())
	docs, _, _ := mongo.FirstBatch(reply)
	if len(docs) == 0 {
		return nil
	}
	return docs[0]
}

func TestLiveDBBrowse(t *testing.T) {
	e := newLiveDB(t)
	ctx := context.Background()
	var docs []string
	for i := range 45 {
		docs = append(docs, `{"_id": `+strconv.Itoa(i)+`, "ad": "Ürün `+strconv.Itoa(i)+`", "fiyat": 12.5, "adet": `+strconv.Itoa(i)+
			`, "uzun": "`+strings.Repeat("ç", 300)+`", "password": "gizli", "ic": {"a": 1, "b": 2}}`)
	}
	e.insert(t, "urunler", docs...)
	e.insert(t, "users", `{"_id": "u1", "email": "a@b.c"}`)
	mustRun(t, e.admin, e.site, mongo.NewBuilder().String("create", "v1").String("viewOn", "urunler").
		StartArray("pipeline").StartDoc("").StartDoc("$match").StartDoc("adet").Int32("$lt", 5).End().End().End().End().MustBuild())
	mustRun(t, e.admin, e.site, mongo.NewBuilder().String("createIndexes", "urunler").StartArray("indexes").
		StartDoc("").StartDoc("key").Int32("ad", 1).End().String("name", "ad_1").Bool("unique", true).End().
		StartDoc("").StartDoc("key").Int32("adet", -1).End().String("name", "adet_ttl").Int32("expireAfterSeconds", 3600).End().
		End().MustBuild())

	ov, err := e.data.DBCollections(ctx, e.site)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("koleksiyonlar: %v", ov.Took)
	names := map[string]DBCollection{}
	for _, c := range ov.Collections {
		names[c.Name] = c
	}
	if names["urunler"].Count != 45 || names["urunler"].Indexes != 3 || names["users"].Count != 1 ||
		names["v1"].Type != "view" || !names["v1"].ReadOnly() {
		t.Fatalf("overview: %+v", ov)
	}

	page, err := e.data.DBFind(ctx, e.site, DBQuery{Collection: "urunler"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("sayfa: %v", page.Took)
	if page.Total != 45 || len(page.Docs) != DBPageSize || !page.More || page.Docs[0].ID != "44" || len(page.Indexes) != 3 {
		t.Fatalf("page 1: total %d, %d docs, more %v, first %q", page.Total, len(page.Docs), page.More, page.Docs[0].ID)
	}
	prev := map[string]string{}
	for _, f := range page.Docs[0].Preview {
		prev[f.Key] = f.Text
	}
	if prev["password"] != "••••" || prev["ic"] != "{…} 2 alan" || !strings.HasSuffix(prev["uzun"], `…"`) ||
		len([]rune(prev["uzun"])) != dbPreviewChars+2 || prev["fiyat"] != "12.5" {
		t.Errorf("preview: %v", prev)
	}
	page, err = e.data.DBFind(ctx, e.site, DBQuery{Collection: "urunler", Page: 3})
	if err != nil || len(page.Docs) != 5 || page.More || page.Docs[4].ID != "0" {
		t.Fatalf("page 3: %v %+v", err, page)
	}
	page, err = e.data.DBFind(ctx, e.site, DBQuery{Collection: "urunler", Filter: `{"adet": {"$gte": 40}}`, Sort: `{"adet": 1}`})
	if err != nil || page.Total != 5 || page.Docs[0].ID != "40" || page.Filter != `{"adet": {"$gte": 40}}` {
		t.Fatalf("filter: %v %+v", err, page)
	}
	page, err = e.data.DBFind(ctx, e.site, DBQuery{Collection: "v1"})
	if err != nil || page.Total != 5 || !page.Collection.ReadOnly() {
		t.Fatalf("view: %v %+v", err, page)
	}
	for _, q := range []DBQuery{
		{Collection: "urunler", Filter: `{"$where": "true"}`},
		{Collection: "urunler", Filter: `{"adet": {"$foo": 1}}`},
		{Collection: "system.views"},
	} {
		_, err := e.data.DBFind(ctx, e.site, q)
		var de *DBError
		if !errors.As(err, &de) {
			t.Errorf("%+v: %v", q, err)
		}
	}
	if _, err := e.data.DBFind(ctx, e.site, DBQuery{Collection: "yok"}); !errors.Is(err, ErrDBNotFound) {
		t.Errorf("missing collection: %v", err)
	}

	doc, err := e.data.DBDoc(ctx, e.site, "urunler", "7")
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Editable || !doc.Deletable || !strings.Contains(doc.Text, "\"password\": \"gizli\"") || doc.Hash != docHash(e.stored(t, "urunler", "7")) {
		t.Fatalf("doc: %+v", doc)
	}
	if _, err := e.data.DBDoc(ctx, e.site, "urunler", "999"); !errors.Is(err, ErrDBNotFound) {
		t.Errorf("missing doc: %v", err)
	}
	// $eq: an _id that looks like an operator matches nothing, not a range.
	if _, err := e.data.DBDoc(ctx, e.site, "urunler", `{"$gt": 1}`); !errors.Is(err, ErrDBNotFound) {
		t.Errorf("operator _id: %v", err)
	}
	vdoc, err := e.data.DBDoc(ctx, e.site, "v1", "3")
	if err != nil || vdoc.Editable || vdoc.Deletable {
		t.Fatalf("view doc: %v %+v", err, vdoc)
	}

	// A document over the display limit is not fetched.
	e.insert(t, "buyuk", `{"_id": 1, "s": "`+strings.Repeat("x", DBShowMax+10)+`"}`)
	big, err := e.data.DBDoc(ctx, e.site, "buyuk", "1")
	if err != nil || !big.TooBig || big.Text != "" || big.Editable || big.Deletable {
		t.Fatalf("big: %v %+v", err, big)
	}
}

func TestLiveDBEditUndo(t *testing.T) {
	e := newLiveDB(t)
	ctx := context.Background()
	e.insert(t, "urunler", `{"_id": {"$oid": "6ae1c4a7ec711d712539599d"}, "ad": "Kalem", "fiyat": 12.5, "adet": 3, "tarih": {"$date": "2026-10-28T09:47:19.647Z"}, "ondalik": {"$numberDecimal": "12.50"}, "uzun": {"$numberLong": "5"}}`)
	id := `{"$oid": "6ae1c4a7ec711d712539599d"}`
	orig := e.stored(t, "urunler", id)
	doc, err := e.data.DBDoc(ctx, e.site, "urunler", id)
	if err != nil || !doc.Editable {
		t.Fatalf("%v %+v", err, doc)
	}
	edited := strings.Replace(doc.Text, `"adet": 3`, `"adet": 4`, 1)
	edited = strings.Replace(edited, `"fiyat": 12.5,`, `"fiyat": 13.0, "yeni": true,`, 1)
	req := DBEditRequest{Collection: "urunler", ID: id, Hash: doc.Hash, Text: edited, Actor: Actor{User: "furkan"}}

	pv, err := e.data.DBPreview(ctx, e.site, req)
	if err != nil || len(pv.Changes) != 3 {
		t.Fatalf("preview: %v %+v", err, pv)
	}
	if !bytes.Equal(e.stored(t, "urunler", id), orig) {
		t.Fatal("preview wrote")
	}

	// Through the HTTP API: the audit log names fields, never values.
	rec := e.post(t, "/db/replace", req, http.StatusOK)
	var edit DBEdit
	json.Unmarshal(rec, &edit)
	now := e.stored(t, "urunler", id)
	text, _ := mongo.DocText(now, true)
	if !strings.Contains(text, `"adet": 4`) || !strings.Contains(text, `"fiyat": 13.0`) || !strings.Contains(text, `"ondalik": {"$numberDecimal": "12.50"}`) ||
		!strings.Contains(text, `"uzun": {"$numberLong": "5"}`) {
		t.Fatalf("stored:\n%s", text)
	}
	audit, _ := os.ReadFile(filepath.Join(e.root, "audit.jsonl"))
	if !strings.Contains(string(audit), `"action":"db.edit"`) || !strings.Contains(string(audit), "alanlar: fiyat, +yeni, adet") ||
		strings.Contains(string(audit), "Kalem") || strings.Contains(string(audit), "13.0") {
		t.Errorf("audit:\n%s", audit)
	}

	// The same request again: the document changed since its hash was taken.
	e.post(t, "/db/replace", req, http.StatusConflict)
	// Saving it unchanged is refused.
	doc, _ = e.data.DBDoc(ctx, e.site, "urunler", id)
	_, err = e.data.DBReplace(ctx, e.site, DBEditRequest{Collection: "urunler", ID: id, Hash: doc.Hash, Text: doc.Text})
	if err == nil || !strings.Contains(err.Error(), "değişiklik yok") {
		t.Fatalf("unchanged: %v", err)
	}

	// Undo puts the original bytes back.
	if _, _, err := e.data.DBUndo(ctx, e.site, edit.ID, Actor{User: "furkan"}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.stored(t, "urunler", id), orig) {
		t.Fatal("undo did not restore the original")
	}
	if _, _, err := e.data.DBUndo(ctx, e.site, edit.ID, Actor{}); err == nil || !strings.Contains(err.Error(), "zaten geri alındı") {
		t.Fatalf("undo twice: %v", err)
	}
	list, _ := e.data.DBEdits(e.site)
	if len(list) != 2 || list[0].Op != "undo" || list[0].UndoOf != edit.ID || list[1].UndoneBy != list[0].ID {
		t.Fatalf("records: %+v", list)
	}
	for _, r := range list {
		for _, c := range r.Changes {
			if c.Old != "" || c.New != "" {
				t.Errorf("record %s keeps values: %+v", r.ID, c)
			}
		}
	}

	// Undo the undo, then change the document behind the panel's back: the next undo
	// does not write.
	redo, _, err := e.data.DBUndo(ctx, e.site, list[0].ID, Actor{})
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, e.admin, e.site, mongo.NewBuilder().String("update", "urunler").StartArray("updates").StartDoc("").
		Doc("q", byID(oidValue(t, "6ae1c4a7ec711d712539599d"))).StartDoc("u").StartDoc("$set").Int32("adet", 99).End().End().End().End().MustBuild())
	changed := e.stored(t, "urunler", id)
	if _, _, err := e.data.DBUndo(ctx, e.site, redo.ID, Actor{}); !errors.Is(err, ErrDBConflict) {
		t.Fatalf("undo after a change: %v", err)
	}
	if !bytes.Equal(e.stored(t, "urunler", id), changed) {
		t.Fatal("a conflicting undo wrote")
	}

	// Delete and undo: the document comes back byte for byte.
	doc, _ = e.data.DBDoc(ctx, e.site, "urunler", id)
	del := e.post(t, "/db/delete", DBEditRequest{Collection: "urunler", ID: id, Hash: doc.Hash, Actor: Actor{User: "furkan"}}, http.StatusOK)
	var delRec DBEdit
	json.Unmarshal(del, &delRec)
	if e.stored(t, "urunler", id) != nil {
		t.Fatal("not deleted")
	}
	if _, _, err := e.data.DBUndo(ctx, e.site, delRec.ID, Actor{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.stored(t, "urunler", id), changed) {
		t.Fatal("deleted document not restored exactly")
	}
	// Undoing the delete twice cannot insert it twice.
	list, _ = e.data.DBEdits(e.site)
	if _, _, err := e.data.DBUndo(ctx, e.site, delRec.ID, Actor{}); err == nil {
		t.Fatal("undo of a delete ran twice")
	}

	// Views are read-only; a job on the site holds changes off (423).
	e.post(t, "/db/replace", DBEditRequest{Collection: "yok", ID: id, Hash: doc.Hash, Text: doc.Text}, http.StatusNotFound)
	block := make(chan struct{})
	defer close(block)
	_, err = e.srv.Jobs.Start(JobSpec{Kind: "import.apply", Target: e.site}, func(ctx context.Context, _ *Task) error {
		<-block
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.post(t, "/db/delete", DBEditRequest{Collection: "urunler", ID: id, Hash: doc.Hash}, http.StatusLocked)
}

func (e *liveDBEnv) post(t *testing.T, path string, body any, want int) []byte {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/sites/"+e.site+path, bytes.NewReader(b))
	req = req.WithContext(context.WithValue(req.Context(), keyUID, uint32(0)))
	w := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(w, req)
	if w.Code != want {
		t.Fatalf("%s: %d %s, want %d", path, w.Code, w.Body.String(), want)
	}
	return w.Body.Bytes()
}

func TestSummaryOtherTypes(t *testing.T) {
	row, err := mongo.ParseDoc(`{"iz": 900, "z": 2000, "n": 3, "f": [
		{"k": "dosya", "t": "binData", "l": 2048},
		{"k": "tarih", "t": "date", "v": {"$date": "2026-10-28T09:47:19.647Z"}},
		{"k": "kod", "t": "javascript"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	s := summaryOf(row)
	if s.ID != "" || s.IDText != "(_id çok büyük)" || s.Rest != 0 {
		t.Fatalf("%+v", s)
	}
	want := []string{"ikili veri, 2.0 KB", `{"$date": "2026-10-28T09:47:19.647Z"}`, "(javascript)"}
	for i, f := range s.Preview {
		if f.Text != want[i] {
			t.Errorf("%s: %q, want %q", f.Key, f.Text, want[i])
		}
	}
}

// Found in review: MongoDB's refusal quotes the value (a duplicate key's): it must not
// reach the audit log. An undo meeting another unique index says which.
func TestLiveDBAuditKeepsValuesOut(t *testing.T) {
	e := newLiveDB(t)
	ctx := context.Background()
	e.insert(t, "users", `{"_id": 1, "token": "tok-SECRET-1", "email": "a@x"}`, `{"_id": 2, "token": "tok-2", "email": "b@x"}`)
	mustRun(t, e.admin, e.site, mongo.NewBuilder().String("createIndexes", "users").StartArray("indexes").
		StartDoc("").StartDoc("key").Int32("token", 1).End().String("name", "token_1").Bool("unique", true).End().
		StartDoc("").StartDoc("key").Int32("email", 1).End().String("name", "email_1").Bool("unique", true).End().
		End().MustBuild())
	doc, err := e.data.DBDoc(ctx, e.site, "users", "2")
	if err != nil {
		t.Fatal(err)
	}
	body := e.post(t, "/db/replace", DBEditRequest{Collection: "users", ID: "2", Hash: doc.Hash,
		Text: strings.Replace(doc.Text, "tok-2", "tok-SECRET-1", 1), Actor: Actor{User: "furkan"}}, http.StatusUnprocessableEntity)
	if !strings.Contains(string(body), "E11000") { // the operator sees MongoDB's reason
		t.Errorf("answer: %s", body)
	}
	audit, _ := os.ReadFile(filepath.Join(e.root, "audit.jsonl"))
	if strings.Contains(string(audit), "SECRET") || !strings.Contains(string(audit), "MongoDB reddetti: DuplicateKey (11000)") {
		t.Errorf("audit:\n%s", audit)
	}
	if list, _ := e.data.DBEdits(e.site); len(list) != 0 {
		t.Errorf("a refused change left a record: %+v", list)
	}
	// Delete 1, give its email to another document, undo: the unique index is named.
	doc, _ = e.data.DBDoc(ctx, e.site, "users", "1")
	rec, err := e.data.DBDelete(ctx, e.site, DBEditRequest{Collection: "users", ID: "1", Hash: doc.Hash})
	if err != nil {
		t.Fatal(err)
	}
	e.insert(t, "users", `{"_id": 3, "token": "tok-3", "email": "a@x"}`)
	_, _, err = e.data.DBUndo(ctx, e.site, rec.ID, Actor{})
	if !errors.Is(err, ErrDBConflict) || !strings.Contains(err.Error(), "email_1 benzersiz indeksinde") {
		t.Fatalf("undo against another unique index: %v", err)
	}
}

// Found in review: MongoDB keeps invalid UTF-8 as written; a list holding such a
// document must still open, and the document must show (not editable) and delete.
func TestLiveDBInvalidUTF8(t *testing.T) {
	e := newLiveDB(t)
	ctx := context.Background()
	bad := mongo.NewBuilder().Int32("_id", 1).String("ad", "Ka\xfeyıt").String("k\xfeey", "x").
		String("uzun", strings.Repeat("ş", 3000)).MustBuild()
	big := mongo.NewBuilder().String("_id", strings.Repeat("u", 4000)).Int32("n", 1).MustBuild()
	mustRun(t, e.admin, e.site, mongo.NewBuilder().String("insert", "eski").StartArray("documents").Doc("", bad).Doc("", big).End().MustBuild())
	page, err := e.data.DBFind(ctx, e.site, DBQuery{Collection: "eski", Sort: "_id"})
	if err != nil || len(page.Docs) != 2 {
		t.Fatalf("list: %v %+v", err, page)
	}
	prev := map[string]string{}
	for _, f := range page.Docs[0].Preview {
		prev[f.Key] = f.Text
	}
	if prev["ad"] != `"Ka�yıt"` || prev["uzun"] != "metin, 5.9 KB" || prev["k�ey"] != `"x"` {
		t.Errorf("preview: %q", prev)
	}
	if page.Docs[1].ID != "" || page.Docs[1].IDText != "(_id çok büyük)" {
		t.Errorf("large _id: %+v", page.Docs[1])
	}
	doc, err := e.data.DBDoc(ctx, e.site, "eski", "1")
	if err != nil || doc.Editable || !doc.Deletable || !strings.Contains(doc.Why, "UTF-8") {
		t.Fatalf("document: %v %+v", err, doc)
	}
	rec, err := e.data.DBDelete(ctx, e.site, DBEditRequest{Collection: "eski", ID: "1", Hash: doc.Hash})
	if err != nil {
		t.Fatal(err)
	}
	v, _ := e.data.DBEditView(e.site, rec.ID)
	if v.BeforeExact { // its text is lossy: it must not go into the editor
		t.Error("lossy version reported exact")
	}
	if _, _, err := e.data.DBUndo(ctx, e.site, rec.ID, Actor{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.stored(t, "eski", "1"), bad) {
		t.Fatal("restored document differs")
	}
}

// Found in review: MongoDB's equality lets a value that only changed type (5 → 5.0)
// match; the record keeps the version actually replaced.
func TestApplyEditKeepsReplacedVersion(t *testing.T) {
	d := &DataOps{DBEditsDir: t.TempDir(), now: time.Now}
	d.init()
	read, _ := mongo.ParseDoc(`{"_id": 1, "n": 5}`)
	replaced, _ := mongo.ParseDoc(`{"_id": 1, "n": 5.0}`)
	next, _ := mongo.ParseDoc(`{"_id": 1, "n": 6}`)
	rec := d.newEdit("magaza", "c", "1", "edit", Actor{}, nil)
	rec, err := d.applyEdit(rec, read, next, func() (bool, mongo.Doc, error) { return true, replaced, nil })
	if err != nil || rec.Pending {
		t.Fatal(err, rec.Pending)
	}
	_, before, _, err := d.loadEdit("magaza", rec.ID)
	if err != nil || !bytes.Equal(before, replaced) {
		t.Fatalf("record keeps %v (%v)", before, err)
	}
	// A refused change leaves no record; a lost answer leaves a pending one.
	rec2 := d.newEdit("magaza", "c", "1", "edit", Actor{}, nil)
	if _, err := d.applyEdit(rec2, read, next, func() (bool, mongo.Doc, error) { return false, nil, nil }); !errors.Is(err, ErrDBConflict) {
		t.Fatal(err)
	}
	if _, _, _, err := d.loadEdit("magaza", rec2.ID); err == nil {
		t.Error("conflict left a record")
	}
	rec3 := d.newEdit("magaza", "c", "1", "edit", Actor{}, nil)
	if _, err := d.applyEdit(rec3, read, next, func() (bool, mongo.Doc, error) { return false, nil, ErrDBUnknown }); !errors.Is(err, ErrDBUnknown) {
		t.Fatal(err)
	}
	if got, _, _, err := d.loadEdit("magaza", rec3.ID); err != nil || !got.Pending {
		t.Errorf("lost answer: %+v %v", got, err)
	}
}

// Found in review: records of an earlier site with the same name are not shown or undone.
func TestEditRecordsOfAnEarlierSite(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("magaza"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	spec, _ := e.ops.Store.Get("magaza")
	then := spec.CreatedAt.Add(-time.Hour)
	d := &DataOps{Store: sites.Store{Root: e.root}, DBEditsDir: filepath.Join(e.root, "dbedits"), now: func() time.Time { return then }}
	d.init()
	doc, _ := mongo.ParseDoc(`{"_id": 1}`)
	old := d.newEdit("magaza", "c", "1", "delete", Actor{}, nil)
	old.Before = true
	if err := d.saveEdit(&old, doc, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.DBEdits("magaza"); len(list) != 0 {
		t.Errorf("earlier site's record listed: %+v", list)
	}
	if _, _, _, err := d.loadEdit("magaza", old.ID); !errors.Is(err, ErrDBNotFound) {
		t.Errorf("earlier site's record loaded: %v", err)
	}
}
