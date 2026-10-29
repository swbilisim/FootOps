package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/mongo"
)

// ---- database browser (F5) ----
//
// A site's collections, pages of documents, one document, and single-document changes,
// each through the site's own MongoDB user (rule K4) on a short connection the agent
// opens itself (package mongo: no tool process, no generated JavaScript). Reads are
// bounded by maxTimeMS and by reply sizes; a change only happens if the stored document
// is still the one the operator saw, and keeps the previous version for undo
// (panel-tasarim.md §24.8.1).

const (
	DefaultDBEditsDir = "/var/lib/footops/dbedits"
	// DBEditKeep is how long previous versions are kept for undo (rule K9).
	DBEditKeep = 7 * 24 * time.Hour
	// DBPageSize documents are listed per page, at most DBMaxPageSize.
	DBPageSize    = 20
	DBMaxPageSize = 50
	// DBShowMax: larger documents are not shown (nor deleted from the panel); DBEditMax:
	// larger ones are not edited.
	DBShowMax = 1 << 20
	DBEditMax = 256 << 10

	defaultMongoAddr = "127.0.0.1:27017"
	dbReadTime       = 2000 // maxTimeMS of every read
	dbWriteTime      = 5000
	dbOpTimeout      = 15 * time.Second
	dbBrowsers       = 2 // at once: each holds a connection and a few MB
	dbListReply      = 4 << 20
	dbMaxCollections = 500
	dbMaxEdits       = 300 // kept per site, newest first
	dbMaxPage        = 10000
	dbPreviewFields  = 8
	dbPreviewChars   = 80
	dbIDTextMax      = 1024 // a longer _id is shown but not linked
	dbChangeChars    = 200
	dbPreviewChanges = 500 // changes listed before saving
	dbRecordChanges  = 200 // field names a record keeps
	dbFilterMax      = 16 << 10
	dbTextMax        = 3 << 19 // 1.5 MB: a 256 KB document's text, escapes included
	dbEditMeta       = "edit.json"
	dbEditBefore     = "before.bson"
	dbEditAfter      = "after.bson"
)

// DBError is a mistake in what was asked (a filter, an edited document) or a change
// MongoDB refused: the operator can fix it. Its message may quote the operator's text or
// the server's, and those can hold the site's data (a duplicate key's value), so the
// audit log gets Audit() instead.
type DBError struct {
	Msg  string
	Code int    // MongoDB's error code, when MongoDB refused
	Name string // and its name
	Text bool   // a mistake in an edited document's text
}

func (e *DBError) Error() string { return e.Msg }

// Audit describes the error without values (rule K6).
func (e *DBError) Audit() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("MongoDB reddetti: %s (%d)", e.Name, e.Code)
	case e.Text:
		return "düzenlenen metin geçersiz"
	}
	return "istek doğrulamadan geçmedi"
}

func dbErrf(format string, args ...any) error { return &DBError{Msg: fmt.Sprintf(format, args...)} }

// refused is a change MongoDB turned down.
func refused(what string, me *mongo.Error) error {
	return &DBError{Msg: what + ": " + me.Msg, Code: me.Code, Name: me.Name}
}

// auditFailure is what the audit log says about a failed change: never the error's
// text, which may quote values.
func auditFailure(err error) string {
	var de *DBError
	var me *mongo.Error
	switch {
	case errors.Is(err, errDBDuplicate):
		return "yazılmadı: aynı anahtarla bir belge var"
	case errors.Is(err, ErrDBConflict):
		return "yazılmadı: belge arada değişmişti"
	case errors.Is(err, ErrDBNotFound):
		return "belge ya da kayıt bulunamadı"
	case errors.Is(err, ErrDBUnknown):
		return "MongoDB'nin cevabı alınamadı, sonuç belirsiz"
	case errors.Is(err, ErrDBBusy):
		return "tarayıcı meşgul"
	case errors.As(err, &de):
		return de.Audit()
	case errors.As(err, &me):
		return fmt.Sprintf("MongoDB hatası: %s (%d)", me.Name, me.Code)
	}
	return mongo.Clip(err.Error(), 200)
}

var (
	// ErrDBNotFound means the document (or collection) is not there (any more).
	ErrDBNotFound = errors.New("belge bulunamadı (silinmiş olabilir)")
	// ErrDBConflict means the document changed since the operator saw it: nothing was
	// written.
	ErrDBConflict = errors.New("belge sen açtıktan sonra değişmiş; hiçbir şey yazılmadı. Sayfayı yenileyip tekrar dene")
	// ErrDBUnknown means a write was sent but its answer was lost: the change may or may
	// not have happened (its record stays, marked so).
	ErrDBUnknown = errors.New("MongoDB'ye yazma gönderildi ama cevabı alınamadı; değişiklik olmuş da olabilir, olmamış da. Belgeyi yeniden aç")
	errDBTooBig  = errors.New("belge çok büyük")
	// ErrDBBusy: the browser's connections are all in use; try again shortly.
	ErrDBBusy = errors.New("veritabanı tarayıcısı meşgul; birazdan tekrar dene")
	// errDBUndoConflict: the document changed again after the change being undone.
	errDBUndoConflict = fmt.Errorf("belge bu değişiklikten sonra yeniden değişmiş; geri alma yapılmadı: %w", ErrDBConflict)
	// errDBDuplicate: an insert met a unique index (the message says which).
	errDBDuplicate = fmt.Errorf("aynı anahtarla bir belge var: %w", ErrDBConflict)
)

// DBCollection is one collection of a site's database.
type DBCollection struct {
	Name      string `json:"name"`
	Type      string `json:"type"`  // collection, view, timeseries
	Count     int64  `json:"count"` // -1: unknown
	Size      int64  `json:"size"`  // data, uncompressed
	Storage   int64  `json:"storage"`
	Indexes   int    `json:"indexes"`
	IndexSize int64  `json:"index_size"`
	Capped    bool   `json:"capped,omitempty"`
	Err       string `json:"err,omitempty"`
}

// ReadOnly reports whether the panel only shows it: views and time-series collections.
func (c DBCollection) ReadOnly() bool { return c.Type != "collection" }

// DBOverview is a site's database: its collections.
type DBOverview struct {
	DB          string         `json:"db"`
	Collections []DBCollection `json:"collections"`
	Hidden      int            `json:"hidden"` // system.* collections, not shown
	More        bool           `json:"more"`   // more collections than listed
	Took        time.Duration  `json:"took"`
}

// DBIndex is one index of a collection.
type DBIndex struct {
	Name    string `json:"name"`
	Keys    string `json:"keys"`
	Unique  bool   `json:"unique,omitempty"`
	Sparse  bool   `json:"sparse,omitempty"`
	TTL     int64  `json:"ttl"` // seconds; -1 if not a TTL index
	Partial string `json:"partial,omitempty"`
	Hidden  bool   `json:"hidden,omitempty"`
}

// DBField is one field in a document's summary.
type DBField struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

// DBDocSummary is a document as listed: its _id, size and first fields, shortened.
type DBDocSummary struct {
	ID      string    `json:"id"`      // the _id as text, for links; "" if not linkable
	IDText  string    `json:"id_text"` // shortened for display
	Size    int64     `json:"size"`
	Fields  int       `json:"fields"`
	Preview []DBField `json:"preview"`
	Rest    int       `json:"rest"` // fields not in the preview (besides _id)
}

// DBQuery selects a page of documents.
type DBQuery struct {
	Collection string
	Filter     string // the text form; "" for all
	Sort       string // "-_id" (default), "_id", or a document such as {"tarih": -1}
	Page       int
	PerPage    int
}

// DBPage is a page of documents.
type DBPage struct {
	Collection DBCollection   `json:"collection"`
	Indexes    []DBIndex      `json:"indexes"`
	Filter     string         `json:"filter"`
	Sort       string         `json:"sort"`
	Page       int            `json:"page"`
	PerPage    int            `json:"per_page"`
	Total      int64          `json:"total"` // -1: not counted in time
	Docs       []DBDocSummary `json:"docs"`
	More       bool           `json:"more"` // a next page exists
	Took       time.Duration  `json:"took"`
}

// DBDocument is one document in full, as text.
type DBDocument struct {
	Collection DBCollection  `json:"collection"`
	ID         string        `json:"id"`
	Text       string        `json:"text,omitempty"`
	Size       int64         `json:"size"`
	Fields     int           `json:"fields"`
	Hash       string        `json:"hash,omitempty"` // of the stored bytes: edits compare it
	Editable   bool          `json:"editable"`
	Deletable  bool          `json:"deletable"`
	Why        string        `json:"why,omitempty"` // why it cannot be edited
	TooBig     bool          `json:"too_big,omitempty"`
	Took       time.Duration `json:"took"`
}

// DBEditRequest changes or deletes one document. Hash is the document as the operator
// saw it: if the stored one differs, nothing is written.
type DBEditRequest struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Hash       string `json:"hash"`
	Text       string `json:"text,omitempty"`
	Actor      Actor  `json:"actor"`
}

// DBChange is one top-level field that differs.
type DBChange struct {
	Field   string `json:"field"`
	Kind    string `json:"kind"` // added, removed, changed
	Old     string `json:"old,omitempty"`
	New     string `json:"new,omitempty"`
	OldType string `json:"old_type,omitempty"`
	NewType string `json:"new_type,omitempty"`
}

// DBPreview is what saving an edited document would change.
type DBPreview struct {
	Changes []DBChange `json:"changes"`
	More    int        `json:"more,omitempty"` // further changes not listed
	Size    int64      `json:"size"`
	Text    string     `json:"text"` // the document as it will be saved
}

// DBEdit is the record of a change: who, what, when, and whether its previous version
// is kept. The versions themselves stay in the agent's directory (they hold the site's
// data); records carry field names, never values (rule K6).
type DBEdit struct {
	ID         string     `json:"id"`
	Site       string     `json:"site"`
	Collection string     `json:"collection"`
	DocID      string     `json:"doc_id"`
	Op         string     `json:"op"` // edit, delete, undo
	Time       time.Time  `json:"time"`
	Actor      string     `json:"actor,omitempty"`
	Changes    []DBChange `json:"changes,omitempty"`
	More       int        `json:"more,omitempty"` // changed fields beyond Changes
	UndoOf     string     `json:"undo_of,omitempty"`
	UndoneBy   string     `json:"undone_by,omitempty"`
	// Pending: the write's answer was lost; it may or may not have happened.
	Pending bool `json:"pending,omitempty"`
	Before  bool `json:"before"` // the document before the change is kept
	After   bool `json:"after"`  // the document after it (nothing after a delete)
}

// DBEditView is a record with its versions as text.
type DBEditView struct {
	Edit       DBEdit `json:"edit"`
	Before     string `json:"before,omitempty"`
	After      string `json:"after,omitempty"`
	BeforeSize int64  `json:"before_size"`
	AfterSize  int64  `json:"after_size"`
	// BeforeExact: the previous version's text reads back as the same document (only
	// then may it go into the editor).
	BeforeExact bool `json:"before_exact"`
}

var (
	// Record ids sort in time order, to the microsecond.
	reDBEditID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9]{6}-[0-9a-f]{4}$`)
	// Values of such fields are hidden in lists (the document page shows them).
	reSecretField = regexp.MustCompile(`(?i)pass|şifre|sifre|parola|secret|token|hash|salt|api_?key`)
)

// ---- session ----

type dbSession struct {
	c  *mongo.Conn
	db string
}

func (d *DataOps) mongoAddr() string {
	if d.MongoAddr != "" {
		return d.MongoAddr
	}
	return defaultMongoAddr
}

// dbOpen logs in as the site's own user. Close releases the connection and the slot.
func (d *DataOps) dbOpen(ctx context.Context, site string) (*dbSession, func(), error) {
	d.init()
	_, creds, err := d.siteDB(site)
	if err != nil {
		return nil, nil, err
	}
	select {
	case d.browse <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ErrDBBusy
	}
	c, err := mongo.Dial(ctx, d.mongoAddr(), mongo.Credential{User: creds.User, Password: creds.Password, Source: creds.DB})
	if err != nil {
		<-d.browse
		return nil, nil, err
	}
	return &dbSession{c: c, db: creds.DB}, func() { c.Close(); <-d.browse }, nil
}

func (s *dbSession) run(ctx context.Context, cmd mongo.Doc, maxReply int) (mongo.Doc, error) {
	return s.c.Run(ctx, s.db, cmd, maxReply)
}

// cursor runs a command answering with a cursor and returns its first batch.
func (s *dbSession) cursor(ctx context.Context, coll string, cmd mongo.Doc, maxReply int) ([]mongo.Doc, error) {
	reply, err := s.run(ctx, cmd, maxReply)
	if err != nil {
		return nil, err
	}
	docs, id, err := mongo.FirstBatch(reply)
	if id != 0 {
		s.c.KillCursor(ctx, s.db, coll, id)
	}
	return docs, err
}

func validCollection(name string) bool {
	return mongo.SafeName(name) && !strings.HasPrefix(name, "system.")
}

// collection describes one collection; an unknown one is ErrDBNotFound.
func (s *dbSession) collection(ctx context.Context, name string) (DBCollection, error) {
	if !validCollection(name) {
		return DBCollection{}, dbErrf("geçersiz koleksiyon adı")
	}
	cmd := mongo.NewBuilder().Int32("listCollections", 1).StartDoc("filter").String("name", name).End().
		Bool("authorizedCollections", true).MustBuild()
	docs, err := s.cursor(ctx, "$cmd.listCollections", cmd, 1<<20)
	if err != nil {
		return DBCollection{}, err
	}
	if len(docs) == 0 {
		return DBCollection{}, fmt.Errorf("%q koleksiyonu yok: %w", name, ErrDBNotFound)
	}
	c := DBCollection{Name: name, Type: str(docs[0], "type"), Count: -1}
	if c.Type == "" {
		c.Type = "collection"
	}
	if o, ok := docs[0].Lookup("options"); ok {
		if od, ok := o.Doc(); ok {
			c.Capped, _ = lookupBool(od, "capped")
		}
	}
	return c, nil
}

func str(d mongo.Doc, k string) string {
	v, _ := d.Lookup(k)
	s, _ := v.Str()
	return s
}

func num(d mongo.Doc, k string) int64 {
	v, _ := d.Lookup(k)
	n, _ := v.Int()
	return n
}

func lookupBool(d mongo.Doc, k string) (bool, bool) {
	v, ok := d.Lookup(k)
	if !ok {
		return false, false
	}
	return v.Bool()
}

// explainRead turns a server error about what the operator asked into a DBError.
func explainRead(err error) error {
	var me *mongo.Error
	switch {
	case mongo.IsCode(err, mongo.CodeMaxTimeExpired):
		return dbErrf("sorgu %d saniyede bitmedi: filtreyi daralt ya da indeksli bir alana göre ara ve sırala", dbReadTime/1000)
	case mongo.IsCode(err, mongo.CodeNoDiskUse):
		return dbErrf("bu sıralama için bellek yetmedi: indeksli bir alana göre sırala ya da filtreyi daralt")
	case mongo.IsCode(err, mongo.CodeUnauthorized):
		return err
	case errors.As(err, &me):
		return dbErrf("MongoDB sorguyu kabul etmedi: %s", me.Msg)
	}
	return err
}

// ---- reading ----

// DBCollections lists a site's collections with their sizes.
func (d *DataOps) DBCollections(ctx context.Context, site string) (DBOverview, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	start := time.Now()
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBOverview{}, err
	}
	defer done()
	out := DBOverview{DB: s.db, Collections: []DBCollection{}}
	cmd := mongo.NewBuilder().Int32("listCollections", 1).Bool("authorizedCollections", true).
		StartDoc("cursor").Int32("batchSize", dbMaxCollections+100).End().MustBuild()
	docs, err := s.cursor(ctx, "$cmd.listCollections", cmd, dbListReply)
	if err != nil {
		return out, err
	}
	for _, doc := range docs {
		name := str(doc, "name")
		if !validCollection(name) {
			out.Hidden++
			continue
		}
		if len(out.Collections) >= dbMaxCollections {
			out.More = true
			continue
		}
		c := DBCollection{Name: name, Type: str(doc, "type"), Count: -1}
		if c.Type == "" {
			c.Type = "collection"
		}
		if c.Type == "collection" {
			if err := s.stats(ctx, &c); err != nil {
				c.Err = err.Error()
			}
		}
		out.Collections = append(out.Collections, c)
	}
	sort.Slice(out.Collections, func(a, b int) bool { return out.Collections[a].Name < out.Collections[b].Name })
	out.Took = time.Since(start)
	return out, nil
}

var statsStage = mustText(`{"$project": {"_id": 0, "size": "$storageStats.size", "count": "$storageStats.count",
	"storage": "$storageStats.storageSize", "indexSize": "$storageStats.totalIndexSize",
	"indexes": "$storageStats.nindexes", "capped": "$storageStats.capped"}}`)

func mustText(s string) mongo.Doc {
	d, err := mongo.ParseDoc(s)
	if err != nil {
		panic(err)
	}
	return d
}

// stats reads a collection's sizes; only the numbers come back, not the storage engine
// details $collStats computes.
func (s *dbSession) stats(ctx context.Context, c *DBCollection) error {
	cmd := mongo.NewBuilder().String("aggregate", c.Name).StartArray("pipeline").
		StartDoc("").StartDoc("$collStats").StartDoc("storageStats").End().End().End().
		Doc("", statsStage).End().StartDoc("cursor").End().Int32("maxTimeMS", dbReadTime).MustBuild()
	docs, err := s.cursor(ctx, c.Name, cmd, 1<<20)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return errors.New("bilgi alınamadı")
	}
	st := docs[0]
	c.Count, c.Size, c.Storage, c.IndexSize = num(st, "count"), num(st, "size"), num(st, "storage"), num(st, "indexSize")
	c.Indexes = int(num(st, "indexes"))
	c.Capped, _ = lookupBool(st, "capped")
	return nil
}

func (s *dbSession) indexes(ctx context.Context, coll string) []DBIndex {
	cmd := mongo.NewBuilder().String("listIndexes", coll).StartDoc("cursor").Int32("batchSize", 200).End().MustBuild()
	docs, err := s.cursor(ctx, coll, cmd, 1<<20)
	if err != nil {
		return nil // views have none
	}
	out := []DBIndex{}
	for _, doc := range docs {
		ix := DBIndex{Name: str(doc, "name"), TTL: -1}
		if v, ok := doc.Lookup("key"); ok {
			ix.Keys = mongo.ValueTextLimit(v, 200)
		}
		ix.Unique, _ = lookupBool(doc, "unique")
		ix.Sparse, _ = lookupBool(doc, "sparse")
		ix.Hidden, _ = lookupBool(doc, "hidden")
		if v, ok := doc.Lookup("expireAfterSeconds"); ok {
			ix.TTL, _ = v.Int()
		}
		if v, ok := doc.Lookup("partialFilterExpression"); ok {
			ix.Partial = mongo.ValueTextLimit(v, 200)
		}
		out = append(out, ix)
	}
	return out
}

// parseFilter reads a filter. Operators that run JavaScript on the server are refused.
func parseFilter(text string) (mongo.Doc, string, error) {
	text = strings.TrimSpace(text)
	if text == "" || text == "{}" {
		return mongo.Empty, "", nil
	}
	if len(text) > dbFilterMax {
		return nil, "", dbErrf("filtre çok uzun (en çok %d KB)", dbFilterMax>>10)
	}
	d, err := mongo.ParseDoc(text)
	if err != nil {
		return nil, "", dbErrf("filtre: %v", err)
	}
	if err := forbidJS(d); err != nil {
		return nil, "", err
	}
	norm, _ := mongo.DocText(d, false)
	return d, norm, nil
}

func forbidJS(d mongo.Doc) error {
	return mongo.Keys(d, func(k string) error {
		switch k {
		case "$where", "$function", "$accumulator":
			return dbErrf("%s kullanılamaz: sunucuda JavaScript çalıştırır", k)
		}
		return nil
	})
}

var (
	sortNewest = mongo.NewBuilder().Int32("_id", -1).MustBuild()
	sortOldest = mongo.NewBuilder().Int32("_id", 1).MustBuild()
)

// parseSort reads a sort: "-_id" (newest first, the default), "_id", or a document of
// up to four fields with 1 or -1.
func parseSort(text string) (mongo.Doc, string, error) {
	switch text = strings.TrimSpace(text); text {
	case "", "-_id":
		return sortNewest, "-_id", nil
	case "_id":
		return sortOldest, "_id", nil
	}
	if len(text) > 1024 {
		return nil, "", dbErrf("sıralama çok uzun")
	}
	d, err := mongo.ParseDoc(text)
	if err != nil {
		return nil, "", dbErrf("sıralama: %v", err)
	}
	elems, _ := d.Elems()
	if len(elems) == 0 || len(elems) > 4 {
		return nil, "", dbErrf(`sıralama 1-4 alan olmalı, ör. {"tarih": -1}`)
	}
	for _, e := range elems {
		n, ok := e.Int()
		if e.Key == "" || strings.HasPrefix(e.Key, "$") || !ok || (n != 1 && n != -1) {
			return nil, "", dbErrf(`sıralamada her alanın değeri 1 (artan) ya da -1 (azalan) olmalı, ör. {"tarih": -1}`)
		}
	}
	norm, _ := mongo.DocText(d, false)
	return d, norm, nil
}

// summaryStage shortens each listed document on the server: its _id, size, field count
// and first fields; strings up to 2 KB come whole (the agent cuts them), longer ones,
// containers and binary data are only measured. A page costs a few hundred KB at most,
// however large the documents. Only byte counts are used: MongoDB keeps invalid UTF-8
// as it was written, and its code-point operators fail on it.
var summaryStage = mustText(`{"$project": {"_id": 0,
	"i": {"$cond": [{"$lte": [{"$bsonSize": {"x": "$_id"}}, 2048]}, "$_id", "$$REMOVE"]},
	"iz": {"$bsonSize": {"x": "$_id"}},
	"z": {"$bsonSize": "$$ROOT"},
	"n": {"$size": {"$objectToArray": "$$ROOT"}},
	"f": {"$map": {"input": {"$slice": [{"$objectToArray": "$$ROOT"}, 9]}, "as": "e", "in": {
		"k": {"$cond": [{"$lte": [{"$binarySize": "$$e.k"}, 256]}, "$$e.k", "$$REMOVE"]},
		"t": {"$type": "$$e.v"},
		"v": {"$switch": {"branches": [
			{"case": {"$eq": [{"$type": "$$e.v"}, "string"]}, "then": {"$cond": [{"$lte": [{"$binarySize": "$$e.v"}, 2048]}, "$$e.v", "$$REMOVE"]}},
			{"case": {"$in": [{"$type": "$$e.v"}, ["object", "array", "binData", "javascript", "javascriptWithScope", "symbol", "regex", "dbPointer"]]}, "then": "$$REMOVE"}],
			"default": "$$e.v"}},
		"l": {"$switch": {"branches": [
			{"case": {"$eq": [{"$type": "$$e.v"}, "string"]}, "then": {"$binarySize": "$$e.v"}},
			{"case": {"$eq": [{"$type": "$$e.v"}, "object"]}, "then": {"$size": {"$objectToArray": "$$e.v"}}},
			{"case": {"$eq": [{"$type": "$$e.v"}, "array"]}, "then": {"$size": "$$e.v"}},
			{"case": {"$eq": [{"$type": "$$e.v"}, "binData"]}, "then": {"$binarySize": "$$e.v"}}],
			"default": "$$REMOVE"}}}}}}}`)

// DBFind returns a page of a collection's documents, summarised.
func (d *DataOps) DBFind(ctx context.Context, site string, q DBQuery) (DBPage, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	start := time.Now()
	if q.PerPage <= 0 {
		q.PerPage = DBPageSize
	}
	q.PerPage = min(q.PerPage, DBMaxPageSize)
	q.Page = min(max(q.Page, 1), dbMaxPage)
	page := DBPage{Page: q.Page, PerPage: q.PerPage, Total: -1, Docs: []DBDocSummary{}, Indexes: []DBIndex{}}
	filter, ftext, err := parseFilter(q.Filter)
	if err != nil {
		return page, err
	}
	sortDoc, stext, err := parseSort(q.Sort)
	if err != nil {
		return page, err
	}
	page.Filter, page.Sort = ftext, stext
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return page, err
	}
	defer done()
	if page.Collection, err = s.collection(ctx, q.Collection); err != nil {
		return page, err
	}
	if ix := s.indexes(ctx, q.Collection); ix != nil {
		page.Indexes = ix
	}
	page.Collection.Indexes = len(page.Indexes)

	count := mongo.NewBuilder().String("count", q.Collection)
	if len(filter) > len(mongo.Empty) {
		count.Doc("query", filter)
	}
	reply, err := s.run(ctx, count.Int32("maxTimeMS", dbReadTime).MustBuild(), 1<<20)
	switch {
	case err == nil:
		page.Total = num(reply, "n")
	case mongo.IsCode(err, mongo.CodeMaxTimeExpired):
		// Too slow to count: the page is still shown, without a total.
	default:
		return page, explainRead(err)
	}

	pipe := mongo.NewBuilder().String("aggregate", q.Collection).StartArray("pipeline").
		StartDoc("").Doc("$match", filter).End().
		StartDoc("").Doc("$sort", sortDoc).End()
	if skip := (q.Page - 1) * q.PerPage; skip > 0 {
		pipe.StartDoc("").Int32("$skip", int32(skip)).End()
	}
	pipe.StartDoc("").Int32("$limit", int32(q.PerPage+1)).End().Doc("", summaryStage).End().
		StartDoc("cursor").Int32("batchSize", int32(q.PerPage+1)).End().
		Bool("allowDiskUse", false).Int32("maxTimeMS", dbReadTime)
	docs, err := s.cursor(ctx, q.Collection, pipe.MustBuild(), dbListReply)
	if err != nil {
		return page, explainRead(err)
	}
	if len(docs) > q.PerPage {
		page.More, docs = q.Page < dbMaxPage, docs[:q.PerPage]
	}
	for _, doc := range docs {
		page.Docs = append(page.Docs, summaryOf(doc))
	}
	page.Took = time.Since(start)
	return page, nil
}

// summaryOf turns one row of summaryStage into what the page shows.
func summaryOf(row mongo.Doc) DBDocSummary {
	var s DBDocSummary
	iv, hasID := row.Lookup("i")
	switch {
	case hasID:
		text, info := mongo.ValueText(iv)
		s.IDText = mongo.Clip(text, 64)
		if len(text) <= dbIDTextMax && info.Exact() && iv.Type != mongo.TypeArray {
			s.ID = text
		}
	case num(row, "iz") > 5: // {x: <_id>} is more than an empty document
		s.IDText = "(_id çok büyük)"
	default:
		s.IDText = "(_id yok)"
	}
	s.Size, s.Fields = num(row, "z"), int(num(row, "n"))
	fv, _ := row.Lookup("f")
	arr, _ := fv.Doc()
	entries, _ := arr.Elems()
	idSeen := false
	for _, e := range entries {
		f, ok := e.Doc()
		if !ok {
			continue
		}
		k, hasKey := f.Lookup("k")
		key, _ := k.Str()
		if hasKey && key == "_id" {
			idSeen = true
			continue
		}
		if len(s.Preview) >= dbPreviewFields {
			break
		}
		if !hasKey {
			key = "(çok uzun alan adı)"
		}
		key = strings.ToValidUTF8(key, "\uFFFD")
		s.Preview = append(s.Preview, DBField{Key: mongo.Clip(key, 64), Text: previewText(key, f)})
	}
	s.Rest = s.Fields - len(s.Preview)
	if idSeen {
		s.Rest--
	}
	s.Rest = max(s.Rest, 0)
	return s
}

func previewText(key string, f mongo.Doc) string {
	if reSecretField.MatchString(key) {
		return "••••"
	}
	t := str(f, "t")
	l := num(f, "l")
	v, hasV := f.Lookup("v")
	switch t {
	case "string":
		if !hasV {
			return "metin, " + humanBytes(l)
		}
		text, _ := v.Str()
		return mongo.Quote(mongo.Clip(text, dbPreviewChars))
	case "object":
		return fmt.Sprintf("{…} %d alan", l)
	case "array":
		return fmt.Sprintf("[…] %d öğe", l)
	case "binData":
		return "ikili veri, " + humanBytes(l)
	}
	if !hasV {
		return "(" + t + ")"
	}
	return mongo.ValueTextLimit(v, 120)
}

// parseID reads a document's _id from its text form.
func parseID(text string) (mongo.Value, error) {
	if text == "" || len(text) > dbIDTextMax {
		return mongo.Value{}, dbErrf("geçersiz belge kimliği")
	}
	v, err := mongo.ParseValue(text)
	if err != nil || v.Type == mongo.TypeArray {
		return mongo.Value{}, dbErrf("geçersiz belge kimliği")
	}
	return v, nil
}

// byID matches exactly one _id: $eq never reads the value as an operator or a pattern.
func byID(id mongo.Value) mongo.Doc {
	return mongo.NewBuilder().StartDoc("_id").Value("$eq", id).End().MustBuild()
}

// fetch reads one document if it is at most limit bytes (errDBTooBig with its size
// otherwise). The size is asked first, so a large document never crosses the wire.
func (s *dbSession) fetch(ctx context.Context, coll string, id mongo.Value, limit int64) (mongo.Doc, int64, error) {
	cmd := mongo.NewBuilder().String("aggregate", coll).StartArray("pipeline").
		StartDoc("").Doc("$match", byID(id)).End().StartDoc("").Int32("$limit", 1).End().
		StartDoc("").StartDoc("$project").Int32("_id", 0).StartDoc("z").String("$bsonSize", "$$ROOT").End().End().End().
		End().StartDoc("cursor").End().Int32("maxTimeMS", dbReadTime).MustBuild()
	docs, err := s.cursor(ctx, coll, cmd, 1<<20)
	if err != nil {
		return nil, 0, err
	}
	if len(docs) == 0 {
		return nil, 0, ErrDBNotFound
	}
	size := num(docs[0], "z")
	if size > limit {
		return nil, size, errDBTooBig
	}
	cmd = mongo.NewBuilder().String("find", coll).Doc("filter", byID(id)).Int32("limit", 1).
		Bool("singleBatch", true).Int32("maxTimeMS", dbReadTime).MustBuild()
	docs, err = s.cursor(ctx, coll, cmd, int(limit)+64<<10)
	switch {
	case errors.Is(err, mongo.ErrTooLarge):
		return nil, size, errDBTooBig // it grew in between
	case err != nil:
		return nil, 0, err
	case len(docs) == 0:
		return nil, 0, ErrDBNotFound
	}
	return docs[0], int64(len(docs[0])), nil
}

func docHash(d mongo.Doc) string {
	sum := sha256.Sum256(d)
	return hex.EncodeToString(sum[:])
}

// editable says whether the panel can edit a document, and why not.
func editable(c DBCollection, d mongo.Doc) (bool, string) {
	_, info := mongo.DocText(d, false)
	elems, _ := d.Elems()
	switch {
	case c.ReadOnly():
		return false, "görünüm ve time-series koleksiyonları yalnız okunur"
	case len(d) > DBEditMax:
		return false, fmt.Sprintf("belge panelde düzenlenemeyecek kadar büyük (en çok %s)", humanBytes(DBEditMax))
	case info.Lossy:
		return false, "belgede metne tam çevrilemeyen bir değer var (geçersiz UTF-8 ya da özel bir NaN); kaydetmek onu değiştirirdi"
	case info.Dollar:
		return false, "belgede $ ile başlayan alan adları var; metin olarak düzenlenemez"
	case info.Dup:
		return false, "belgede aynı alan adı iki kez geçiyor"
	case len(elems) == 0 || elems[0].Key != "_id":
		return false, "belgenin ilk alanı _id değil"
	}
	return true, ""
}

// DBDoc returns one document in full.
func (d *DataOps) DBDoc(ctx context.Context, site, coll, idText string) (DBDocument, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	start := time.Now()
	id, err := parseID(idText)
	if err != nil {
		return DBDocument{}, err
	}
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBDocument{}, err
	}
	defer done()
	c, err := s.collection(ctx, coll)
	if err != nil {
		return DBDocument{}, err
	}
	out := DBDocument{Collection: c, ID: idText}
	doc, size, err := s.fetch(ctx, coll, id, DBShowMax)
	out.Size = size
	switch {
	case errors.Is(err, errDBTooBig):
		out.TooBig, out.Why = true, fmt.Sprintf("belge panelde gösterilemeyecek kadar büyük (%s; en çok %s)", humanBytes(size), humanBytes(DBShowMax))
		out.Took = time.Since(start)
		return out, nil
	case err != nil:
		return out, err
	}
	out.Text, _ = mongo.DocText(doc, true)
	elems, _ := doc.Elems()
	out.Fields = len(elems)
	out.Hash = docHash(doc)
	out.Editable, out.Why = editable(c, doc)
	out.Deletable = !c.ReadOnly()
	out.Took = time.Since(start)
	return out, nil
}

// ---- changing ----

// parseEdited reads an edited document: _id unchanged (the stored one: same type and
// bytes) and first, no top-level operator names (they would turn the replacement into
// an update), within the edit limit.
func parseEdited(text string, cur mongo.Doc) (mongo.Doc, error) {
	if len(text) > dbTextMax {
		return nil, &DBError{Msg: fmt.Sprintf("metin çok uzun (en çok %s)", humanBytes(dbTextMax)), Text: true}
	}
	id, ok := cur.Lookup("_id")
	if !ok {
		return nil, dbErrf("saklı belgede _id yok")
	}
	d, err := mongo.ParseDoc(text)
	if err != nil {
		return nil, &DBError{Msg: "belge: " + err.Error(), Text: true}
	}
	elems, _ := d.Elems()
	var found bool
	b := mongo.NewBuilder()
	for _, e := range elems {
		if e.Key == "_id" {
			if !mongo.Equal(e.Value, id) {
				return nil, dbErrf("_id değiştirilemez (bu belgenin _id'si %s)", mongo.ValueTextLimit(id, 80))
			}
			found = true
			b.Value("_id", e.Value)
		}
	}
	if !found {
		return nil, dbErrf("_id alanı silinemez")
	}
	for _, e := range elems {
		if e.Key == "_id" {
			continue
		}
		if strings.HasPrefix(e.Key, "$") {
			return nil, dbErrf("alan adı $ ile başlayamaz: %q", e.Key)
		}
		b.Value(e.Key, e.Value)
	}
	out, err := b.Build()
	if err != nil {
		return nil, dbErrf("belge: %v", err)
	}
	if _, info := mongo.DocText(out, false); info.Dollar {
		return nil, dbErrf("iç belgelerde de alan adı $ ile başlayamaz")
	}
	if len(out) > DBEditMax {
		return nil, dbErrf("belge çok büyük (%s; panelde en çok %s)", humanBytes(int64(len(out))), humanBytes(DBEditMax))
	}
	return out, nil
}

// diffDocs lists the top-level fields that differ, in the new document's order, then
// the removed ones.
func diffDocs(old, new mongo.Doc) []DBChange {
	oe, _ := old.Elems()
	ne, _ := new.Elems()
	prev := make(map[string]mongo.Value, len(oe))
	for _, e := range oe {
		if _, ok := prev[e.Key]; !ok {
			prev[e.Key] = e.Value
		}
	}
	seen := map[string]bool{}
	var out []DBChange
	for _, e := range ne {
		seen[e.Key] = true
		p, ok := prev[e.Key]
		switch {
		case !ok:
			out = append(out, DBChange{Field: e.Key, Kind: "added", New: mongo.ValueTextLimit(e.Value, dbChangeChars), NewType: e.Type.Name()})
		case !mongo.Equal(p, e.Value):
			c := DBChange{Field: e.Key, Kind: "changed", Old: mongo.ValueTextLimit(p, dbChangeChars), New: mongo.ValueTextLimit(e.Value, dbChangeChars)}
			if p.Type != e.Type {
				c.OldType, c.NewType = p.Type.Name(), e.Type.Name()
			}
			out = append(out, c)
		}
	}
	for _, e := range oe {
		if !seen[e.Key] {
			seen[e.Key] = true
			out = append(out, DBChange{Field: e.Key, Kind: "removed", Old: mongo.ValueTextLimit(e.Value, dbChangeChars), OldType: e.Type.Name()})
		}
	}
	return out
}

// prepareEdit checks an edit against the stored document: the same collection, the
// same version the operator saw, a valid new text.
func (s *dbSession) prepareEdit(ctx context.Context, req DBEditRequest, limit int64) (DBCollection, mongo.Value, mongo.Doc, error) {
	c, err := s.collection(ctx, req.Collection)
	if err != nil {
		return c, mongo.Value{}, nil, err
	}
	if c.ReadOnly() {
		return c, mongo.Value{}, nil, dbErrf("görünüm ve time-series koleksiyonları yalnız okunur")
	}
	id, err := parseID(req.ID)
	if err != nil {
		return c, id, nil, err
	}
	cur, _, err := s.fetch(ctx, req.Collection, id, limit)
	switch {
	case errors.Is(err, errDBTooBig):
		return c, id, nil, dbErrf("belge artık panelde değiştirilemeyecek kadar büyük")
	case err != nil:
		return c, id, nil, err
	}
	if docHash(cur) != req.Hash {
		return c, id, nil, ErrDBConflict
	}
	return c, id, cur, nil
}

// DBPreview checks an edited document and lists what saving it would change; nothing
// is written.
func (d *DataOps) DBPreview(ctx context.Context, site string, req DBEditRequest) (DBPreview, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBPreview{}, err
	}
	defer done()
	c, _, cur, err := s.prepareEdit(ctx, req, DBEditMax)
	if err != nil {
		return DBPreview{}, err
	}
	if ok, why := editable(c, cur); !ok {
		return DBPreview{}, dbErrf("bu belge panelde düzenlenemez: %s", why)
	}
	next, err := parseEdited(req.Text, cur)
	if err != nil {
		return DBPreview{}, err
	}
	text, _ := mongo.DocText(next, true)
	changes := diffDocs(cur, next)
	pv := DBPreview{Size: int64(len(next)), Text: text}
	pv.Changes, pv.More = capChanges(changes, dbPreviewChanges)
	return pv, nil
}

// casQuery matches the document only while it is exactly current (simple collation:
// case and accents count).
func casQuery(id mongo.Value, current mongo.Doc) mongo.Doc {
	return mongo.NewBuilder().StartDoc("_id").Value("$eq", id).End().
		StartDoc("$expr").StartArray("$eq").String("", "$$ROOT").StartDoc("").Doc("$literal", current).End().End().End().
		MustBuild()
}

// replaceIf replaces (or with next nil, removes) the document only while it is current.
// It returns the version it replaced (nil if none matched); a lost answer is
// ErrDBUnknown. The match is MongoDB's equality: a value that only changed type (5 to
// 5.0) still matches, so the caller keeps the returned version, not its own copy.
func (s *dbSession) replaceIf(ctx context.Context, coll string, id mongo.Value, current, next mongo.Doc) (mongo.Doc, error) {
	b := mongo.NewBuilder().String("findAndModify", coll).Doc("query", casQuery(id, current))
	if next == nil {
		b.Bool("remove", true)
	} else {
		b.Doc("update", next).Bool("new", false).Bool("upsert", false)
	}
	cmd := b.StartDoc("collation").String("locale", "simple").End().Int32("maxTimeMS", dbWriteTime).
		StartDoc("writeConcern").Int32("w", 1).Bool("j", true).End().MustBuild()
	reply, err := s.run(ctx, cmd, 2*DBShowMax+64<<10)
	var me *mongo.Error
	switch {
	case errors.As(err, &me):
		return nil, refused("MongoDB değişikliği kabul etmedi", me)
	case err != nil:
		return nil, ErrDBUnknown
	}
	v, _ := reply.Lookup("value")
	if d, ok := v.Doc(); ok && v.Type == mongo.TypeDocument {
		return d, nil
	}
	return nil, nil
}

var reIndexName = regexp.MustCompile(`index: (\S+) `)

// insert adds a document back (undoing a delete).
func (s *dbSession) insert(ctx context.Context, coll string, doc mongo.Doc) error {
	cmd := mongo.NewBuilder().String("insert", coll).StartArray("documents").Doc("", doc).End().
		Int32("maxTimeMS", dbWriteTime).StartDoc("writeConcern").Int32("w", 1).Bool("j", true).End().MustBuild()
	reply, err := s.run(ctx, cmd, 1<<20)
	var me *mongo.Error
	switch {
	case errors.As(err, &me):
		return refused("MongoDB belgeyi kabul etmedi", me)
	case err != nil:
		return ErrDBUnknown
	}
	if err := mongo.WriteErr(reply); err != nil {
		if errors.As(err, &me) && me.Code == mongo.CodeDuplicateKey {
			m := reIndexName.FindStringSubmatch(me.Msg)
			if m == nil || m[1] == "_id_" {
				return fmt.Errorf("aynı _id ile bir belge zaten var: %w", errDBDuplicate)
			}
			return fmt.Errorf("%s benzersiz indeksinde aynı değerle başka bir belge var: %w", m[1], errDBDuplicate)
		}
		if errors.As(err, &me) {
			return refused("MongoDB belgeyi kabul etmedi", me)
		}
		return err
	}
	return nil
}

// DBReplace saves an edited document. The previous version is written to disk first;
// the replacement happens only if the stored document is still the one the operator
// saw.
func (d *DataOps) DBReplace(ctx context.Context, site string, req DBEditRequest) (DBEdit, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBEdit{}, err
	}
	defer done()
	c, id, cur, err := s.prepareEdit(ctx, req, DBEditMax)
	if err != nil {
		return DBEdit{}, err
	}
	if ok, why := editable(c, cur); !ok {
		return DBEdit{}, dbErrf("bu belge panelde düzenlenemez: %s", why)
	}
	next, err := parseEdited(req.Text, cur)
	if err != nil {
		return DBEdit{}, err
	}
	if bytes.Equal(cur, next) {
		return DBEdit{}, dbErrf("değişiklik yok: belge olduğu gibi duruyor")
	}
	rec := d.newEdit(site, c.Name, req.ID, "edit", req.Actor, diffDocs(cur, next))
	return d.applyEdit(rec, cur, next, func() (bool, mongo.Doc, error) { return matched(s.replaceIf(ctx, c.Name, id, cur, next)) })
}

// matched adapts replaceIf to applyEdit: whether it wrote, and what it replaced.
func matched(pre mongo.Doc, err error) (bool, mongo.Doc, error) { return pre != nil, pre, err }

// DBDelete deletes one document, keeping it for undo.
func (d *DataOps) DBDelete(ctx context.Context, site string, req DBEditRequest) (DBEdit, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBEdit{}, err
	}
	defer done()
	c, id, cur, err := s.prepareEdit(ctx, req, DBShowMax)
	if err != nil {
		return DBEdit{}, err
	}
	rec := d.newEdit(site, c.Name, req.ID, "delete", req.Actor, nil)
	return d.applyEdit(rec, cur, nil, func() (bool, mongo.Doc, error) { return matched(s.replaceIf(ctx, c.Name, id, cur, nil)) })
}

// applyEdit writes the record (both versions, synced), makes the change, and settles
// the record: kept if the change happened, removed if it surely did not, kept as
// pending if its answer was lost. If the change replaced a version other than the one
// read (a value that only changed type), the record keeps the version it replaced.
func (d *DataOps) applyEdit(rec DBEdit, before, after mongo.Doc, change func() (bool, mongo.Doc, error)) (DBEdit, error) {
	rec.Before, rec.After, rec.Pending = before != nil, after != nil, true
	if err := d.saveEdit(&rec, before, after); err != nil {
		return rec, fmt.Errorf("önceki hal kaydedilemedi, değişiklik yapılmadı: %w", err)
	}
	ok, pre, err := change()
	switch {
	case errors.Is(err, ErrDBUnknown):
		return rec, err
	case err != nil || !ok:
		d.dropEdit(rec)
		if err == nil {
			err = ErrDBConflict
		}
		return rec, err
	}
	if pre != nil && before != nil && !bytes.Equal(pre, before) {
		_ = writeSynced(filepath.Join(d.editDir(rec.Site, rec.ID), dbEditBefore), pre)
	}
	rec.Pending = false
	// The change is made: a record that cannot be settled stays pending, which undo
	// handles the same way (it only acts on the version the change left).
	_ = d.saveEditMeta(rec)
	return rec, nil
}

// DBUndo reverses a change, if the document is still as the change left it. The undo
// is itself a change with a record (it can be undone too).
func (d *DataOps) DBUndo(ctx context.Context, site, editID string, actor Actor) (DBEdit, DBEdit, error) {
	ctx, cancel := context.WithTimeout(ctx, dbOpTimeout)
	defer cancel()
	orig, before, after, err := d.loadEdit(site, editID)
	if err != nil {
		return DBEdit{}, orig, err
	}
	if orig.UndoneBy != "" {
		return DBEdit{}, orig, dbErrf("bu değişiklik zaten geri alındı (%s)", orig.UndoneBy)
	}
	s, done, err := d.dbOpen(ctx, site)
	if err != nil {
		return DBEdit{}, orig, err
	}
	defer done()
	c, err := s.collection(ctx, orig.Collection)
	if err != nil {
		return DBEdit{}, orig, err
	}
	if c.ReadOnly() {
		return DBEdit{}, orig, dbErrf("koleksiyon artık yalnız okunur (görünüm ya da time-series)")
	}
	id, err := parseID(orig.DocID)
	if err != nil {
		return DBEdit{}, orig, err
	}
	rec := d.newEdit(site, orig.Collection, orig.DocID, "undo", actor, nil)
	rec.UndoOf = orig.ID
	var change func() (bool, mongo.Doc, error)
	switch {
	case before != nil && after != nil: // an edit: put the previous version back
		rec.Changes, rec.More = capChanges(namesOnly(diffDocs(after, before)), dbRecordChanges)
		change = func() (bool, mongo.Doc, error) { return matched(s.replaceIf(ctx, c.Name, id, after, before)) }
	case before != nil: // a delete: insert it again
		change = func() (bool, mongo.Doc, error) {
			if err := s.insert(ctx, c.Name, before); err != nil {
				return false, nil, err
			}
			return true, nil, nil
		}
	case after != nil: // a re-insert: delete it again
		change = func() (bool, mongo.Doc, error) { return matched(s.replaceIf(ctx, c.Name, id, after, nil)) }
	default:
		return DBEdit{}, orig, errors.New("kayıtta belge yok")
	}
	rec, err = d.applyEdit(rec, after, before, change)
	if err != nil {
		if errors.Is(err, ErrDBConflict) && !errors.Is(err, errDBDuplicate) {
			err = errDBUndoConflict
		}
		return rec, orig, err
	}
	// Undone: a second undo of the same change could not act anyway (the document is
	// no longer as it left it), so marking it is only for the list.
	d.editMu.Lock()
	defer d.editMu.Unlock()
	orig.UndoneBy = rec.ID
	_ = d.saveEditMeta(orig)
	return rec, orig, nil
}

// ---- records ----

func (d *DataOps) newEdit(site, coll, docID, op string, actor Actor, changes []DBChange) DBEdit {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	now := d.now().UTC()
	id := now.Format("20060102-150405-") + fmt.Sprintf("%06d-", now.Nanosecond()/1000) + hex.EncodeToString(b)
	rec := DBEdit{ID: id, Site: site, Collection: coll, DocID: docID, Op: op, Time: now, Actor: actor.User}
	rec.Changes, rec.More = capChanges(namesOnly(changes), dbRecordChanges)
	return rec
}

// namesOnly keeps field names (shortened) and kinds: values stay in the version files.
func namesOnly(changes []DBChange) []DBChange {
	var out []DBChange
	for _, c := range changes {
		out = append(out, DBChange{Field: mongo.Clip(c.Field, 128), Kind: c.Kind, OldType: c.OldType, NewType: c.NewType})
	}
	return out
}

// capChanges keeps the first n changes and says how many were left out: a document of
// thousands of fields must not make a page, or a record, unreadable.
func capChanges(cs []DBChange, n int) ([]DBChange, int) {
	if len(cs) <= n {
		return cs, 0
	}
	return cs[:n], len(cs) - n
}

func (d *DataOps) editDir(site, id string) string { return filepath.Join(d.DBEditsDir, site, id) }

// saveEdit writes a record and its versions; the record file comes last, so a
// directory without one is a write cut short.
func (d *DataOps) saveEdit(rec *DBEdit, before, after mongo.Doc) error {
	if d.DBEditsDir == "" {
		return errors.New("kayıt klasörü ayarlı değil")
	}
	if err := os.MkdirAll(filepath.Join(d.DBEditsDir, rec.Site), 0o700); err != nil {
		return err
	}
	dir := d.editDir(rec.Site, rec.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	for name, doc := range map[string]mongo.Doc{dbEditBefore: before, dbEditAfter: after} {
		if doc == nil {
			continue
		}
		if err := writeSynced(filepath.Join(dir, name), doc); err != nil {
			_ = os.RemoveAll(dir)
			return err
		}
	}
	if err := d.saveEditMeta(*rec); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

func (d *DataOps) saveEditMeta(rec DBEdit) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(d.editDir(rec.Site, rec.ID), dbEditMeta), append(b, '\n'))
}

func (d *DataOps) dropEdit(rec DBEdit) { _ = os.RemoveAll(d.editDir(rec.Site, rec.ID)) }

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// siteBorn is when the site was created: records from before belong to an earlier
// site of the same name and are not shown or undone.
func (d *DataOps) siteBorn(site string) time.Time {
	spec, err := d.Store.Get(site)
	if err != nil {
		return time.Time{}
	}
	return spec.CreatedAt
}

func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("dosya beklenenden büyük")
	}
	return b, nil
}

func (d *DataOps) readEditMeta(site, id string) (DBEdit, error) {
	var rec DBEdit
	b, err := readSmall(filepath.Join(d.editDir(site, id), dbEditMeta), 256<<10)
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.ID != id || rec.Site != site {
		return rec, errors.New("değişiklik kaydı bozuk")
	}
	return rec, nil
}

// loadEdit reads a record and its versions.
func (d *DataOps) loadEdit(site, id string) (DBEdit, mongo.Doc, mongo.Doc, error) {
	if !docker.ValidName(site) || !reDBEditID.MatchString(id) {
		return DBEdit{}, nil, nil, fmt.Errorf("geçersiz kayıt: %w", ErrDBNotFound)
	}
	rec, err := d.readEditMeta(site, id)
	if err != nil || rec.Time.Before(d.siteBorn(site)) {
		return rec, nil, nil, fmt.Errorf("kayıt bulunamadı: %w", ErrDBNotFound)
	}
	read := func(name string, want bool) (mongo.Doc, error) {
		if !want {
			return nil, nil
		}
		b, err := readSmall(filepath.Join(d.editDir(site, id), name), mongo.MaxDocument)
		if err != nil {
			return nil, err
		}
		doc := mongo.Doc(b)
		if err := doc.Validate(); err != nil {
			return nil, fmt.Errorf("kayıttaki belge bozuk: %w", err)
		}
		return doc, nil
	}
	before, err := read(dbEditBefore, rec.Before)
	if err != nil {
		return rec, nil, nil, err
	}
	after, err := read(dbEditAfter, rec.After)
	if err != nil {
		return rec, nil, nil, err
	}
	return rec, before, after, nil
}

// DBEdits lists a site's change records, newest first.
func (d *DataOps) DBEdits(site string) ([]DBEdit, error) {
	if !docker.ValidName(site) {
		return nil, errors.New("geçersiz site")
	}
	out := []DBEdit{}
	ents, err := os.ReadDir(filepath.Join(d.DBEditsDir, site))
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	born := d.siteBorn(site)
	for i := len(ents) - 1; i >= 0 && len(out) < dbMaxEdits; i-- {
		if e := ents[i]; e.IsDir() && reDBEditID.MatchString(e.Name()) {
			if rec, err := d.readEditMeta(site, e.Name()); err == nil && !rec.Time.Before(born) {
				out = append(out, rec)
			}
		}
	}
	return out, nil
}

// DBEditView returns a record with its versions as text (each up to DBShowMax).
func (d *DataOps) DBEditView(site, id string) (DBEditView, error) {
	rec, before, after, err := d.loadEdit(site, id)
	if err != nil {
		return DBEditView{}, err
	}
	v := DBEditView{Edit: rec, BeforeSize: int64(len(before)), AfterSize: int64(len(after))}
	if before != nil && len(before) <= DBShowMax {
		var info mongo.TextInfo
		v.Before, info = mongo.DocText(before, true)
		v.BeforeExact = info.Exact()
	}
	if after != nil && len(after) <= DBShowMax {
		v.After, _ = mongo.DocText(after, true)
	}
	return v, nil
}

// SweepDBEdits removes records past DBEditKeep (keeping at most dbMaxEdits per site)
// and directories left by a write cut short. It returns what it removed.
func (d *DataOps) SweepDBEdits() []string {
	d.init()
	if d.DBEditsDir == "" {
		return nil
	}
	siteDirs, err := os.ReadDir(d.DBEditsDir)
	if err != nil {
		return nil
	}
	now := d.now().UTC()
	cutoff := now.Add(-DBEditKeep).Format("20060102-150405")
	stale := now.Add(-time.Hour).Format("20060102-150405")
	var removed []string
	for _, sd := range siteDirs {
		if !sd.IsDir() || !docker.ValidName(sd.Name()) {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(d.DBEditsDir, sd.Name()))
		if err != nil {
			continue
		}
		kept := 0
		for i := len(ents) - 1; i >= 0; i-- {
			name := ents[i].Name()
			if !ents[i].IsDir() || !reDBEditID.MatchString(name) {
				continue
			}
			_, err := os.Stat(filepath.Join(d.DBEditsDir, sd.Name(), name, dbEditMeta))
			incomplete := err != nil && name < stale
			if incomplete || (err == nil && (name < cutoff || kept >= dbMaxEdits)) {
				if os.RemoveAll(filepath.Join(d.DBEditsDir, sd.Name(), name)) == nil {
					removed = append(removed, sd.Name()+"/"+name)
				}
				continue
			}
			if err == nil {
				kept++
			}
		}
	}
	return removed
}
