package mac

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/mongo"
)

// Two databases of the Mac's MongoDB compared (panel-tasarim §33.4): the owner's own and
// a site's backup loaded beside it. Each collection is read from both in _id order, side
// by side, as MongoDB's _id index gives it: a document is the same on both (its bytes are
// equal: mongodump and mongorestore keep them as they were), different, or on one side
// only — a batch of each in memory, however big the collection. The merge relies on both
// sides coming in one order: compareValues is MongoDB's order of values, and each side is
// held to rise under it as it is read. A collection where it does not (a collation of its
// own, a number type read otherwise) is said to be so, and only counted.

const (
	// compareBatch is the documents asked of MongoDB at a time (a batch is 16 MB at most).
	compareBatch = 500
	// compareReply is the most read of one reply: a batch of 16 MB and what wraps it.
	compareReply = 48_000_000
	// compareExamples is how many _ids of each kind a collection names.
	compareExamples = 3
	// compareCall bounds one command (a batch, a count, a list).
	compareCall = 2 * time.Minute
	// compareCollections is the most compared of a database's collections; compareSkipped
	// the most of those not compared that are named.
	compareCollections = 1000
	compareSkipped     = 100
)

// errOrder: a side's _ids did not come in the order the merge relies on.
var errOrder = errors.New("_id sırası tutmadı")

// Newest is a side's newest record: the latest of its documents' times — an ObjectId
// _id's (the moment it was made), updatedAt, createdAt — and where it was.
type Newest struct {
	Time       time.Time `json:"time"`
	Collection string    `json:"collection,omitempty"`
	Field      string    `json:"field"`
}

// CollectionDiff is one collection compared. Local and Server are its documents on each
// side (-1: not there). Error: it could not be compared document by document, its counts
// alone are given.
type CollectionDiff struct {
	Name         string        `json:"name"`
	Local        int64         `json:"local"`
	Server       int64         `json:"server"`
	Same         int64         `json:"same"`
	Different    int64         `json:"different"`
	OnlyLocal    int64         `json:"only_local"`
	OnlyServer   int64         `json:"only_server"`
	LocalNewest  *Newest       `json:"local_newest,omitempty"`
	ServerNewest *Newest       `json:"server_newest,omitempty"`
	Examples     *DiffExamples `json:"examples,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// DiffExamples are a few _ids of each kind (as the panel's database pages write them),
// to look them up.
type DiffExamples struct {
	Different  []string `json:"different,omitempty"`
	OnlyLocal  []string `json:"only_local,omitempty"`
	OnlyServer []string `json:"only_server,omitempty"`
}

// DiffSide is one side's whole: its documents and collections (the compared ones), its
// newest record.
type DiffSide struct {
	Documents   int64   `json:"documents"`
	Collections int     `json:"collections"`
	Newest      *Newest `json:"newest,omitempty"`
}

// DBDiff is two databases compared. More: collections past compareCollections, not
// compared; Skipped: views and time series, whose documents are not the database's own
// (and collections past those named); Errors: collections not compared document by
// document. Identical: every collection on both sides, every document the same.
type DBDiff struct {
	Collections []CollectionDiff `json:"collections"`
	More        int              `json:"more,omitempty"`
	Skipped     []string         `json:"skipped,omitempty"`
	Local       DiffSide         `json:"local"`
	Server      DiffSide         `json:"server_side"`
	Same        int64            `json:"same"`
	Different   int64            `json:"different"`
	OnlyLocal   int64            `json:"only_local"`
	OnlyServer  int64            `json:"only_server"`
	Errors      int              `json:"errors,omitempty"`
	Identical   bool             `json:"identical"`
}

// compareDBs compares the database local of the Mac's MongoDB with server (a backup
// loaded beside it), on one connection; progress is told the documents read of all.
func compareDBs(ctx context.Context, c *mongo.Conn, local, server string, progress func(done, total int64)) (DBDiff, error) {
	d := DBDiff{Collections: []CollectionDiff{}}
	lc, err := listColls(ctx, c, local)
	if err != nil {
		return d, err
	}
	sc, err := listColls(ctx, c, server)
	if err != nil {
		return d, err
	}
	names := make([]string, 0, len(lc)+len(sc))
	for n := range lc {
		names = append(names, n)
	}
	for n := range sc {
		if _, ok := lc[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	type job struct {
		name       string
		inL, inS   bool
		cntL, cntS int64
	}
	var jobs []job
	skipped := 0
	oneSided := false // a view or a time series on one side only
	var total int64
	for _, n := range names {
		lt, st := lc[n], sc[n]
		if (lt != "" && lt != "collection") || (st != "" && st != "collection") {
			oneSided = oneSided || lt != st
			if skipped++; len(d.Skipped) < compareSkipped {
				d.Skipped = append(d.Skipped, n+" ("+collKindTR(lt, st)+")")
			}
			continue
		}
		if len(jobs) >= compareCollections {
			d.More++
			continue
		}
		j := job{name: n, inL: lt != "", inS: st != ""}
		if j.inL {
			if j.cntL, err = countColl(ctx, c, local, n); err != nil {
				return d, err
			}
		}
		if j.inS {
			if j.cntS, err = countColl(ctx, c, server, n); err != nil {
				return d, err
			}
		}
		total += j.cntL + j.cntS
		jobs = append(jobs, j)
	}
	if over := skipped - len(d.Skipped); over > 0 {
		d.Skipped = append(d.Skipped, fmt.Sprintf("… ve %d tane daha", over))
	}

	var done int64
	for _, j := range jobs {
		cd, err := compareColl(ctx, c, local, server, j.name, j.inL, j.inS, func(n int64) {
			if progress != nil {
				progress(done+n, total)
			}
		})
		if err != nil {
			return d, err
		}
		if cd.Error != "" {
			// Counted instead: what MongoDB keeps of each side.
			if j.inL {
				cd.Local = j.cntL
			}
			if j.inS {
				cd.Server = j.cntS
			}
			d.Errors++
		}
		done += max(cd.Local, 0) + max(cd.Server, 0)
		d.add(cd)
	}
	if progress != nil {
		progress(total, total)
	}
	d.Identical = d.Errors == 0 && d.Different == 0 && d.OnlyLocal == 0 && d.OnlyServer == 0 && d.More == 0 && !oneSided &&
		d.Local.Collections == d.Server.Collections && d.Local.Collections == len(d.Collections)
	return d, nil
}

// add counts a collection into the whole.
func (d *DBDiff) add(cd CollectionDiff) {
	d.Collections = append(d.Collections, cd)
	d.Same += cd.Same
	d.Different += cd.Different
	d.OnlyLocal += cd.OnlyLocal
	d.OnlyServer += cd.OnlyServer
	if cd.Local >= 0 {
		d.Local.Collections++
		d.Local.Documents += cd.Local
		d.Local.Newest = newer(d.Local.Newest, cd.LocalNewest)
	}
	if cd.Server >= 0 {
		d.Server.Collections++
		d.Server.Documents += cd.Server
		d.Server.Newest = newer(d.Server.Newest, cd.ServerNewest)
	}
}

func newer(a, b *Newest) *Newest {
	if b != nil && (a == nil || b.Time.After(a.Time)) {
		return b
	}
	return a
}

func collKindTR(lt, st string) string {
	kind := lt
	if kind == "" || kind == "collection" {
		kind = st
	}
	switch kind {
	case "view":
		return "görünüm, karşılaştırılmaz"
	case "timeseries":
		return "zaman serisi, karşılaştırılmaz"
	}
	return kind + ", karşılaştırılmaz"
}

// compareColl reads a collection of both sides in _id order, side by side (one not
// there is empty); counted tells how many documents were read of the two.
func compareColl(ctx context.Context, c *mongo.Conn, local, server, name string, inL, inS bool, counted func(int64)) (CollectionDiff, error) {
	cd := CollectionDiff{Name: name, Local: -1, Server: -1}
	ls := &collStream{c: c, db: local, coll: name, empty: !inL}
	ss := &collStream{c: c, db: server, coll: name, empty: !inS}
	defer ls.close()
	defer ss.close()
	var ex DiffExamples
	example := func(list *[]string, id mongo.Value) {
		if len(*list) < compareExamples {
			*list = append(*list, mongo.ValueTextLimit(id, 120))
		}
	}
	l, lid, err := ls.next(ctx)
	var r mongo.Doc
	var rid mongo.Value
	if err == nil {
		r, rid, err = ss.next(ctx)
	}
	steps := 0
	for err == nil && (l != nil || r != nil) {
		var o int
		switch {
		case r == nil:
			o = -1
		case l == nil:
			o = 1
		default:
			o = compareValues(lid, rid)
		}
		switch {
		case o < 0:
			cd.OnlyLocal++
			example(&ex.OnlyLocal, lid)
			l, lid, err = ls.next(ctx)
		case o > 0:
			cd.OnlyServer++
			example(&ex.OnlyServer, rid)
			r, rid, err = ss.next(ctx)
		default:
			if bytes.Equal(l, r) {
				cd.Same++
			} else {
				cd.Different++
				example(&ex.Different, lid)
			}
			if l, lid, err = ls.next(ctx); err == nil {
				r, rid, err = ss.next(ctx)
			}
		}
		if steps++; steps%256 == 0 {
			counted(ls.n + ss.n)
		}
	}
	switch {
	case ctx.Err() != nil:
		return cd, errStopped
	case errors.Is(err, errOrder):
		return CollectionDiff{Name: name, Local: -1, Server: -1, Error: "belgeleri tek tek karşılaştırılamadı: _id'leri MongoDB'nin " +
			"sırasıyla gelmedi (koleksiyonun kendi harf sıralaması ya da karışık _id türleri); yalnız belge sayıları"}, nil
	case err != nil:
		return cd, err
	}
	if inL {
		cd.Local = ls.n
		cd.LocalNewest = ls.newest
	}
	if inS {
		cd.Server = ss.n
		cd.ServerNewest = ss.newest
	}
	if len(ex.Different)+len(ex.OnlyLocal)+len(ex.OnlyServer) > 0 {
		cd.Examples = &ex
	}
	counted(ls.n + ss.n)
	return cd, nil
}

// collStream reads a collection in _id order, a batch at a time, holding each _id to be
// greater than the one before it, and keeps its newest record.
type collStream struct {
	c        *mongo.Conn
	db, coll string
	empty    bool
	batch    []mongo.Doc
	i        int
	cursor   int64
	started  bool
	n        int64
	last     mongo.Value
	newest   *Newest
}

// next is the next document and its _id; none (nil) at the end.
func (s *collStream) next(ctx context.Context) (mongo.Doc, mongo.Value, error) {
	for s.i >= len(s.batch) {
		if s.empty || (s.started && s.cursor == 0) {
			return nil, mongo.Value{}, nil
		}
		if err := s.fetch(ctx); err != nil {
			return nil, mongo.Value{}, err
		}
	}
	d := s.batch[s.i]
	s.i++
	id, ok := d.Lookup("_id")
	if !ok {
		return nil, mongo.Value{}, fmt.Errorf("%s / %s: _id'si olmayan belge", s.db, clip(clean(s.coll), 80))
	}
	if s.n > 0 && compareValues(s.last, id) >= 0 {
		return nil, mongo.Value{}, errOrder
	}
	s.last = id
	s.n++
	s.note(d, id)
	return d, id, nil
}

func (s *collStream) fetch(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, compareCall)
	defer cancel()
	cmd := mongo.GetMore(s.cursor, s.coll, compareBatch)
	if !s.started {
		cmd = mongo.NewBuilder().String("find", s.coll).StartDoc("filter").End().StartDoc("sort").Int32("_id", 1).End().
			Int32("batchSize", compareBatch).MustBuild()
	}
	reply, err := s.c.Run(cctx, s.db, cmd, compareReply)
	if err != nil {
		if ctx.Err() != nil {
			return errStopped
		}
		return fmt.Errorf("%s / %s okunamadı: %v", s.db, clip(clean(s.coll), 80), err)
	}
	var docs []mongo.Doc
	if s.started {
		docs, s.cursor, err = mongo.NextBatch(reply)
	} else {
		docs, s.cursor, err = mongo.FirstBatch(reply)
	}
	s.started = true
	if err != nil {
		return err
	}
	s.batch, s.i = docs, 0
	return nil
}

// close lets go of a cursor left open (a collection not read to its end).
func (s *collStream) close() {
	if s.cursor == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.c.KillCursor(ctx, s.db, s.coll, s.cursor)
	s.cursor = 0
}

// note keeps the document's times if newer: its ObjectId _id's, updatedAt, createdAt.
func (s *collStream) note(d mongo.Doc, id mongo.Value) {
	if id.Type == mongo.TypeObjectID && len(id.Data) == 12 {
		s.later(time.Unix(int64(binary.BigEndian.Uint32(id.Data)), 0), "_id")
	}
	for _, f := range []string{"updatedAt", "createdAt"} {
		if v, ok := d.Lookup(f); ok && v.Type == mongo.TypeDate && len(v.Data) == 8 {
			s.later(time.UnixMilli(int64(binary.LittleEndian.Uint64(v.Data))), f)
		}
	}
}

func (s *collStream) later(t time.Time, field string) {
	if _, ok := saneTime(t); !ok {
		return // a year the answer cannot carry (before 1970, after 9999)
	}
	if s.newest == nil || t.After(s.newest.Time) {
		s.newest = &Newest{Time: t.UTC(), Collection: s.coll, Field: field}
	}
}

// listColls is a database's collections and their types (collection, view,
// timeseries); MongoDB's own (system.*) and names the panel does not work with are left
// out.
func listColls(ctx context.Context, c *mongo.Conn, db string) (map[string]string, error) {
	cctx, cancel := context.WithTimeout(ctx, compareCall)
	defer cancel()
	cmd := mongo.NewBuilder().Int32("listCollections", 1).StartDoc("cursor").Int32("batchSize", 1000).End().MustBuild()
	out := map[string]string{}
	const coll = "$cmd.listCollections"
	reply, err := c.Run(cctx, db, cmd, compareReply)
	var docs []mongo.Doc
	var id int64
	if err == nil {
		docs, id, err = mongo.FirstBatch(reply)
	}
	for err == nil {
		for _, d := range docs {
			nv, _ := d.Lookup("name")
			name, ok := nv.Str()
			if !ok || !mongo.SafeName(name) || strings.HasPrefix(name, "system.") {
				continue
			}
			tv, _ := d.Lookup("type")
			kind, _ := tv.Str()
			if kind == "" {
				kind = "collection"
			}
			out[name] = kind
			if len(out) > 100_000 {
				err = fmt.Errorf("%s: çok fazla koleksiyon", db)
				break
			}
		}
		if err != nil || id == 0 {
			break
		}
		if reply, err = c.Run(cctx, db, mongo.GetMore(id, coll, 1000), compareReply); err == nil {
			docs, id, err = mongo.NextBatch(reply)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, errStopped
		}
		return nil, fmt.Errorf("%s'in koleksiyonları okunamadı: %v", db, err)
	}
	return out, nil
}

// countColl is a collection's documents as MongoDB keeps their count (no reading).
func countColl(ctx context.Context, c *mongo.Conn, db, coll string) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, compareCall)
	defer cancel()
	reply, err := c.Run(cctx, db, mongo.NewBuilder().String("count", coll).MustBuild(), 64<<10)
	if err != nil {
		if ctx.Err() != nil {
			return 0, errStopped
		}
		return 0, fmt.Errorf("%s / %s sayılamadı: %v", db, clip(clean(coll), 80), err)
	}
	v, _ := reply.Lookup("n")
	n, _ := v.Int()
	return n, nil
}

// ---- MongoDB's order of values ----

// typeRank is a BSON type's place in MongoDB's order of values (its canonical type: every
// kind of number is one, so are a string and a symbol).
func typeRank(t mongo.Type) int {
	switch t {
	case mongo.TypeMinKey:
		return -1
	case mongo.TypeUndefined:
		return 0
	case mongo.TypeNull:
		return 5
	case mongo.TypeDouble, mongo.TypeInt32, mongo.TypeInt64, mongo.TypeDecimal:
		return 10
	case mongo.TypeString, mongo.TypeSymbol:
		return 15
	case mongo.TypeDocument:
		return 20
	case mongo.TypeArray:
		return 25
	case mongo.TypeBinary:
		return 30
	case mongo.TypeObjectID:
		return 35
	case mongo.TypeBool:
		return 40
	case mongo.TypeDate:
		return 45
	case mongo.TypeTimestamp:
		return 47
	case mongo.TypeRegex:
		return 50
	case mongo.TypeDBPointer:
		return 55
	case mongo.TypeJavaScript:
		return 60
	case mongo.TypeCodeScope:
		return 65
	case mongo.TypeMaxKey:
		return 127
	}
	return 200
}

// compareValues orders two values as MongoDB does (BSON's comparison order: type first,
// then value). Where this is not MongoDB's — a decimal against another number, a code
// with scope — it is still a total order, and a side read out of it is caught (errOrder).
func compareValues(a, b mongo.Value) int {
	if ra, rb := typeRank(a.Type), typeRank(b.Type); ra != rb {
		return cmp.Compare(ra, rb)
	}
	switch a.Type {
	case mongo.TypeMinKey, mongo.TypeMaxKey, mongo.TypeNull, mongo.TypeUndefined:
		return 0
	case mongo.TypeDouble, mongo.TypeInt32, mongo.TypeInt64, mongo.TypeDecimal:
		return compareNumbers(a, b)
	case mongo.TypeString, mongo.TypeSymbol, mongo.TypeJavaScript:
		return bytes.Compare(strData(a), strData(b))
	case mongo.TypeDocument, mongo.TypeArray:
		return compareDocs(mongo.Doc(a.Data), mongo.Doc(b.Data))
	case mongo.TypeBinary:
		// Its length first, then its subtype and bytes.
		if la, lb := len(a.Data), len(b.Data); la != lb {
			return cmp.Compare(la, lb)
		}
		return bytes.Compare(a.Data[4:], b.Data[4:])
	case mongo.TypeObjectID, mongo.TypeBool:
		return bytes.Compare(a.Data, b.Data)
	case mongo.TypeDate:
		return cmp.Compare(int64(binary.LittleEndian.Uint64(a.Data)), int64(binary.LittleEndian.Uint64(b.Data)))
	case mongo.TypeTimestamp:
		return cmp.Compare(binary.LittleEndian.Uint64(a.Data), binary.LittleEndian.Uint64(b.Data))
	case mongo.TypeRegex:
		pa, fa, _ := bytes.Cut(a.Data, []byte{0})
		pb, fb, _ := bytes.Cut(b.Data, []byte{0})
		if c := bytes.Compare(pa, pb); c != 0 {
			return c
		}
		return bytes.Compare(fa, fb)
	case mongo.TypeDBPointer:
		if la, lb := len(a.Data), len(b.Data); la != lb {
			return cmp.Compare(la, lb)
		}
	}
	return bytes.Compare(a.Data, b.Data)
}

// strData is a string's bytes, without its length and its NUL.
func strData(v mongo.Value) []byte {
	if len(v.Data) < 5 {
		return nil
	}
	return v.Data[4 : len(v.Data)-1]
}

// compareDocs orders two documents (or arrays) field by field: each field's type, then
// its name, then its value; the one that ends first is the lesser.
func compareDocs(a, b mongo.Doc) int {
	ea, erra := a.Elems()
	eb, errb := b.Elems()
	if erra != nil || errb != nil {
		return bytes.Compare(a, b)
	}
	for i := 0; ; i++ {
		switch {
		case i == len(ea) && i == len(eb):
			return 0
		case i == len(ea):
			return -1
		case i == len(eb):
			return 1
		}
		if c := cmp.Compare(typeRank(ea[i].Type), typeRank(eb[i].Type)); c != 0 {
			return c
		}
		if c := strings.Compare(ea[i].Key, eb[i].Key); c != 0 {
			return c
		}
		if c := compareValues(ea[i].Value, eb[i].Value); c != 0 {
			return c
		}
	}
}

// compareNumbers orders two numbers by their value, whatever their types: NaN below
// every other, -0 the same as 0, a long against a double exactly. A decimal is ordered
// among decimals by its bytes, after every other number of the same value — not
// MongoDB's order, but a total one (decimal _ids are not met in practice).
func compareNumbers(a, b mongo.Value) int {
	if a.Type == mongo.TypeDecimal || b.Type == mongo.TypeDecimal {
		switch {
		case a.Type != mongo.TypeDecimal:
			return -1
		case b.Type != mongo.TypeDecimal:
			return 1
		}
		return bytes.Compare(a.Data, b.Data)
	}
	ia, aInt := intOf(a)
	ib, bInt := intOf(b)
	switch {
	case aInt && bInt:
		return cmp.Compare(ia, ib)
	case aInt:
		return compareLongDouble(ia, doubleOf(b))
	case bInt:
		return -compareLongDouble(ib, doubleOf(a))
	}
	return cmp.Compare(doubleOf(a), doubleOf(b)) // NaN lowest and equal to itself, -0 = 0
}

func intOf(v mongo.Value) (int64, bool) {
	switch v.Type {
	case mongo.TypeInt32:
		return int64(int32(binary.LittleEndian.Uint32(v.Data))), true
	case mongo.TypeInt64:
		return int64(binary.LittleEndian.Uint64(v.Data)), true
	}
	return 0, false
}

func doubleOf(v mongo.Value) float64 { return math.Float64frombits(binary.LittleEndian.Uint64(v.Data)) }

// compareLongDouble orders a long and a double exactly (a long may not fit a double).
func compareLongDouble(l int64, d float64) int {
	switch {
	case math.IsNaN(d):
		return 1
	case d >= 0x1p63:
		return -1
	case d < -0x1p63:
		return 1
	}
	t := math.Trunc(d)
	if ti := int64(t); l != ti {
		return cmp.Compare(l, ti)
	}
	return cmp.Compare(0.0, d-t) // the double's fraction: above the long, or below it
}
