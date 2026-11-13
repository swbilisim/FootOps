package mac

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/mongo"
)

// Two databases compared: MongoDB's order of values (against a real MongoDB when there is
// one: FOOTOPS_TEST_MONGO), and the merge, collection by collection.

// pv is a value written as the panel's database pages write one (Extended JSON).
func pv(t *testing.T, text string) mongo.Value {
	t.Helper()
	v, err := mongo.ParseValue(text)
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return v
}

// ascending are values in MongoDB's order (each greater than the one before), as its
// documentation and its _id index order them; none equal to another.
var ascending = []string{
	`{"$minKey": 1}`,
	`null`,
	`{"$numberDouble": "NaN"}`,
	`{"$numberDouble": "-Infinity"}`,
	`{"$numberLong": "-9223372036854775808"}`,
	`-1000000.5`,
	`{"$numberInt": "-3"}`,
	`-0.5`,
	`{"$numberInt": "0"}`,
	`0.25`,
	`{"$numberLong": "1"}`,
	`1.5`,
	`{"$numberInt": "2"}`,
	`{"$numberDouble": "9007199254740992"}`,
	`{"$numberLong": "9007199254740993"}`,
	`{"$numberDouble": "9007199254740994"}`,
	`{"$numberLong": "9223372036854775807"}`,
	`{"$numberDouble": "9223372036854775808"}`,
	`{"$numberDouble": "Infinity"}`,
	`""`,
	`"A"`,
	`"Z"`,
	`"a"`,
	`"ab"`,
	`"b"`,
	`"ç"`,
	`"ğ"`,
	`{}`,
	`{"a": 1}`,
	`{"a": 1, "b": 1}`,
	`{"a": 2}`,
	`{"b": 0}`,   // a field's type before its name: a number's below a string's
	`{"a": "x"}`, // (MongoDB's _id index agrees: TestCompareMatchesMongo)
	`{"b": {"c": 1}}`,
	`[]`,
	`[1]`,
	`[1, 2]`,
	`[2]`,
	`{"$binary": {"base64": "", "subType": "00"}}`,
	`{"$binary": {"base64": "AQ==", "subType": "00"}}`,
	`{"$binary": {"base64": "AQ==", "subType": "80"}}`,
	`{"$binary": {"base64": "AQI=", "subType": "00"}}`,
	`{"$oid": "000000000000000000000000"}`,
	`{"$oid": "5f0000000000000000000001"}`,
	`{"$oid": "6a08d947286b694863255add"}`,
	`{"$oid": "ffffffffffffffffffffffff"}`,
	`false`,
	`true`,
	`{"$date": {"$numberLong": "-1000"}}`,
	`{"$date": "2026-11-13T09:00:00Z"}`,
	`{"$date": "2026-11-13T09:00:01Z"}`,
	`{"$timestamp": {"t": 1, "i": 5}}`,
	`{"$timestamp": {"t": 2, "i": 1}}`,
	`{"$timestamp": {"t": 4294967295, "i": 1}}`,
	`{"$regularExpression": {"pattern": "a", "options": ""}}`,
	`{"$regularExpression": {"pattern": "a", "options": "i"}}`,
	`{"$regularExpression": {"pattern": "b", "options": ""}}`,
	`{"$maxKey": 1}`,
}

func TestCompareValuesOrder(t *testing.T) {
	vals := make([]mongo.Value, len(ascending))
	for i, s := range ascending {
		vals[i] = pv(t, s)
	}
	for i := range vals {
		for j := range vals {
			got := compareValues(vals[i], vals[j])
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if sign(got) != want {
				t.Errorf("%s ? %s = %d, want %d", ascending[i], ascending[j], got, want)
			}
		}
	}
	// The same number of every kind is one value; a string and a symbol of the same
	// letters, too.
	for _, eq := range [][2]string{
		{`{"$numberInt": "1"}`, `{"$numberLong": "1"}`}, {`{"$numberInt": "1"}`, `1.0`}, {`-0.0`, `{"$numberInt": "0"}`},
		{`{"$numberDouble": "NaN"}`, `{"$numberDouble": "NaN"}`}, {`"a"`, `{"$symbol": "a"}`}, {`{"a": 1}`, `{"a": 1.0}`},
		{`null`, `null`}, {`{"$maxKey": 1}`, `{"$maxKey": 1}`},
	} {
		if c := compareValues(pv(t, eq[0]), pv(t, eq[1])); c != 0 {
			t.Errorf("%s ? %s = %d, want 0", eq[0], eq[1], c)
		}
	}
	// A long beyond a double's precision, exactly.
	if c := compareValues(pv(t, `{"$numberLong": "9007199254740993"}`), pv(t, `9007199254740992.0`)); c != 1 {
		t.Errorf("2^53+1 ? 2^53 = %d", c)
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// A total order whatever is thrown at it: antisymmetric, transitive (the merge relies on
// it even where it is not MongoDB's).
func TestCompareValuesTotal(t *testing.T) {
	vals := []mongo.Value{pv(t, `{"$numberDecimal": "1.5"}`), pv(t, `{"$numberDecimal": "-2"}`), pv(t, `{"$numberDecimal": "NaN"}`),
		pv(t, `{"$code": "x"}`), pv(t, `{"$code": "x", "$scope": {"a": 1}}`), pv(t, `{"$undefined": true}`),
		pv(t, `{"$dbPointer": {"$ref": "a", "$id": {"$oid": "6a08d947286b694863255add"}}}`)}
	for _, s := range ascending {
		vals = append(vals, pv(t, s))
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		shuffled := slices.Clone(vals)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		slices.SortFunc(shuffled, compareValues)
		for i := range shuffled {
			for j := i + 1; j < len(shuffled); j++ {
				if compareValues(shuffled[i], shuffled[j]) > 0 || sign(compareValues(shuffled[j], shuffled[i])) != -sign(compareValues(shuffled[i], shuffled[j])) {
					t.Fatalf("not a total order at %d, %d", i, j)
				}
			}
		}
	}
}

// testMongo is FOOTOPS_TEST_MONGO's MongoDB (the test is skipped without one) and a
// prefix of databases the test alone uses, dropped at its end.
func testMongo(t *testing.T) (mongoTarget, string) {
	t.Helper()
	uri := os.Getenv("FOOTOPS_TEST_MONGO")
	if uri == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok")
	}
	mt, err := parseMongoURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "vttest_" + newID()[:6]
	t.Cleanup(func() {
		ctx := context.Background()
		dbs, _ := mt.databases(ctx)
		for name := range dbs {
			if strings.HasPrefix(name, prefix) {
				mt.drop(ctx, name)
			}
		}
	})
	return mt, prefix
}

// insert writes documents into a collection; each is a _id and fields (Extended JSON).
func insert(t *testing.T, c *mongo.Conn, db, coll string, docs ...string) {
	t.Helper()
	b := mongo.NewBuilder().String("insert", coll).StartArray("documents")
	for _, d := range docs {
		doc, err := mongo.ParseDoc(d)
		if err != nil {
			t.Fatalf("%s: %v", d, err)
		}
		b.Doc("", doc)
	}
	reply, err := c.Run(context.Background(), db, b.End().MustBuild(), 16<<20)
	if err == nil {
		err = mongo.WriteErr(reply)
	}
	if err != nil {
		t.Fatalf("insert %s.%s: %v", db, coll, err)
	}
}

func command(t *testing.T, c *mongo.Conn, db, cmd string) {
	t.Helper()
	d, err := mongo.ParseDoc(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), db, d, 1<<20); err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
}

// MongoDB's own order of _ids is the comparator's: a collection holding one of every
// kind an _id may be, read as MongoDB's _id index gives it, rises under compareValues
// all the way (the stream would say errOrder otherwise) — over several batches.
func TestCompareMatchesMongo(t *testing.T) {
	mt, prefix := testMongo(t)
	ctx := context.Background()
	c, err := mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := prefix + "_sira"
	var docs []string
	for _, s := range ascending {
		if strings.HasPrefix(s, "[") || strings.Contains(s, "$regularExpression") {
			continue // an _id is never an array or a regular expression
		}
		docs = append(docs, `{"_id": `+s+`}`)
	}
	// And many of the usual ones: ObjectIds, strings, numbers, over a batch's size.
	r := rand.New(rand.NewPCG(3, 4))
	for i := range 1200 {
		switch i % 4 {
		case 0:
			docs = append(docs, fmt.Sprintf(`{"_id": {"$oid": "%024x"}}`, r.Uint64()))
		case 1:
			docs = append(docs, fmt.Sprintf(`{"_id": "k%dç%x"}`, i, r.Uint32()))
		case 2:
			docs = append(docs, fmt.Sprintf(`{"_id": %d.25}`, i-600))
		case 3:
			docs = append(docs, fmt.Sprintf(`{"_id": {"t": %d, "n": "%x"}}`, r.IntN(50), r.Uint32()))
		}
	}
	r.Shuffle(len(docs), func(i, j int) { docs[i], docs[j] = docs[j], docs[i] })
	insert(t, c, db, "ids", docs...)
	s := &collStream{c: c, db: db, coll: "ids"}
	defer s.close()
	for {
		d, _, err := s.next(ctx)
		if err != nil {
			t.Fatalf("after %d: %v (last %s)", s.n, err, mongo.ValueTextLimit(s.last, 80))
		}
		if d == nil {
			break
		}
	}
	if s.n != int64(len(docs)) {
		t.Fatalf("read %d of %d", s.n, len(docs))
	}

	// Decimals among other numbers: MongoDB orders them by value, the comparator does not
	// — the stream says so instead of comparing wrongly.
	insert(t, c, db, "ondalik", `{"_id": 1}`, `{"_id": {"$numberDecimal": "1.5"}}`, `{"_id": 2}`)
	s = &collStream{c: c, db: db, coll: "ondalik"}
	defer s.close()
	var last error
	for range 4 {
		var d mongo.Doc
		if d, _, last = s.next(ctx); last != nil || d == nil {
			break
		}
	}
	if last != errOrder {
		t.Fatalf("decimals: %v", last)
	}
}

// Two databases compared: the same, different and one-sided documents counted and named,
// collections on one side, a view left out, a collection read over several batches, one
// whose _ids are not in the comparator's order said to be so; the newest records; a
// database against itself is identical.
func TestCompareDBs(t *testing.T) {
	mt, prefix := testMongo(t)
	ctx := context.Background()
	c, err := mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	local, server := prefix+"_yerel", prefix+"_sunucu"
	oid := func(n int) string { return fmt.Sprintf(`{"$oid": "6a08d947286b6948632%05x"}`, n) }
	// a: 3 the same, 2 different, 1 only here, 2 only there.
	insert(t, c, local, "a",
		`{"_id": `+oid(1)+`, "ad": "bir", "updatedAt": {"$date": "2026-11-01T10:00:00Z"}}`,
		`{"_id": `+oid(2)+`, "ad": "iki"}`,
		`{"_id": `+oid(3)+`, "ad": "üç"}`,
		`{"_id": `+oid(4)+`, "ad": "dört", "n": 1}`,
		`{"_id": `+oid(5)+`, "ad": "beş", "updatedAt": {"$date": "2026-11-12T18:03:00Z"}}`,
		`{"_id": `+oid(7)+`, "ad": "yedi"}`)
	insert(t, c, server, "a",
		`{"_id": `+oid(1)+`, "ad": "bir", "updatedAt": {"$date": "2026-11-01T10:00:00Z"}}`,
		`{"_id": `+oid(2)+`, "ad": "iki"}`,
		`{"_id": `+oid(3)+`, "ad": "üç"}`,
		`{"_id": `+oid(4)+`, "ad": "dört", "n": 2}`,
		`{"_id": `+oid(5)+`, "ad": "beş", "updatedAt": {"$date": "2026-11-13T08:55:00Z"}}`,
		`{"_id": `+oid(6)+`, "ad": "altı"}`,
		`{"_id": `+oid(8)+`, "ad": "sekiz", "createdAt": {"$date": "2026-11-13T09:30:00Z"}}`)
	// The same number of another type: the same _id, a different document.
	insert(t, c, local, "tur", `{"_id": 1, "x": 1}`)
	insert(t, c, server, "tur", `{"_id": 1.0, "x": 1}`)
	// b only here; c only there, empty.
	insert(t, c, local, "b", `{"_id": "x"}`, `{"_id": "y"}`)
	command(t, c, server, `{"create": "c"}`)
	// A view here: left out.
	command(t, c, local, `{"create": "gorunum", "viewOn": "a", "pipeline": []}`)
	// Over several batches, a few apart.
	var many, manyThere []string
	for i := range 1234 {
		d := fmt.Sprintf(`{"_id": %d, "v": "%d"}`, i, i)
		many = append(many, d)
		switch {
		case i == 10 || i == 700:
			manyThere = append(manyThere, fmt.Sprintf(`{"_id": %d, "v": "değişti"}`, i))
		case i == 1233:
		default:
			manyThere = append(manyThere, d)
		}
	}
	insert(t, c, local, "cok", many...)
	insert(t, c, server, "cok", manyThere...)
	// Decimals among numbers: counted only.
	insert(t, c, local, "ondalik", `{"_id": 1}`, `{"_id": {"$numberDecimal": "1.5"}}`, `{"_id": 2}`)
	insert(t, c, server, "ondalik", `{"_id": 1}`)

	var last [2]int64
	d, err := compareDBs(ctx, c, local, server, func(done, total int64) { last = [2]int64{done, total} })
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]CollectionDiff{}
	for _, cd := range d.Collections {
		by[cd.Name] = cd
	}
	a := by["a"]
	if a.Local != 6 || a.Server != 7 || a.Same != 3 || a.Different != 2 || a.OnlyLocal != 1 || a.OnlyServer != 2 {
		t.Errorf("a: %+v", a)
	}
	if a.Examples == nil || len(a.Examples.Different) != 2 || !strings.Contains(a.Examples.Different[0], "6a08d947286b694863200004") ||
		len(a.Examples.OnlyLocal) != 1 || !strings.Contains(a.Examples.OnlyLocal[0], "00007") || len(a.Examples.OnlyServer) != 2 {
		t.Errorf("a's examples: %+v", a.Examples)
	}
	if n := a.LocalNewest; n == nil || n.Field != "updatedAt" || !n.Time.Equal(time.Date(2026, 11, 12, 18, 3, 0, 0, time.UTC)) {
		t.Errorf("a's newest here: %+v", n)
	}
	if n := a.ServerNewest; n == nil || n.Field != "createdAt" || n.Collection != "a" || !n.Time.Equal(time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)) {
		t.Errorf("a's newest there: %+v", n)
	}
	if x := by["tur"]; x.Different != 1 || x.OnlyLocal+x.OnlyServer != 0 {
		t.Errorf("tur: %+v", x)
	}
	if b := by["b"]; b.Local != 2 || b.Server != -1 || b.OnlyLocal != 2 {
		t.Errorf("b: %+v", b)
	}
	if x := by["c"]; x.Local != -1 || x.Server != 0 || x.OnlyServer != 0 {
		t.Errorf("c: %+v", x)
	}
	if x := by["cok"]; x.Local != 1234 || x.Server != 1233 || x.Same != 1231 || x.Different != 2 || x.OnlyLocal != 1 {
		t.Errorf("cok: %+v", x)
	}
	if x := by["ondalik"]; x.Error == "" || x.Local != 3 || x.Server != 1 || x.Same+x.Different+x.OnlyLocal+x.OnlyServer != 0 {
		t.Errorf("ondalik: %+v", x)
	}
	if _, ok := by["gorunum"]; ok || len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0], "gorunum (görünüm") {
		t.Errorf("view: %v %v", ok, d.Skipped)
	}
	if d.Identical || d.Errors != 1 || d.Local.Collections != 5 || d.Server.Collections != 5 ||
		d.Same != 3+1231 || d.Different != 2+1+2 || d.OnlyLocal != 1+2+1 || d.OnlyServer != 2 {
		t.Errorf("whole: %+v", d)
	}
	if d.Local.Newest == nil || d.Local.Newest.Field != "updatedAt" || d.Server.Newest == nil || d.Server.Newest.Field != "createdAt" {
		t.Errorf("newest: %+v %+v", d.Local.Newest, d.Server.Newest)
	}
	if last[0] != last[1] || last[1] != 6+7+1+1+2+0+1234+1233+3+1 {
		t.Errorf("progress: %v", last)
	}

	// Against itself: identical (the view still left out).
	same, err := compareDBs(ctx, c, server, server, nil)
	if err != nil || !same.Identical || same.Same != same.Server.Documents || same.Server.Documents != 7+1+1233+1 {
		t.Errorf("itself: %+v %v", same, err)
	}
	// A view on one side only is a difference; on both, it is left out alike. A date past
	// 9999 is no newest record (the answer could not carry it).
	insert(t, c, prefix+"_v1", "a", `{"_id": 1, "createdAt": {"$date": {"$numberLong": "253402300800000"}}}`)
	insert(t, c, prefix+"_v2", "a", `{"_id": 1, "createdAt": {"$date": {"$numberLong": "253402300800000"}}}`)
	command(t, c, prefix+"_v1", `{"create": "g", "viewOn": "a", "pipeline": []}`)
	one, err := compareDBs(ctx, c, prefix+"_v1", prefix+"_v2", nil)
	if err != nil || one.Identical || one.Same != 1 || one.Local.Newest != nil {
		t.Errorf("view on one side: %+v %v", one, err)
	}
	command(t, c, prefix+"_v2", `{"create": "g", "viewOn": "a", "pipeline": []}`)
	if both, err := compareDBs(ctx, c, prefix+"_v1", prefix+"_v2", nil); err != nil || !both.Identical {
		t.Errorf("view on both: %+v %v", both, err)
	}
	// Stopped: errStopped, the connection's cursors let go of.
	sctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := compareDBs(sctx, c, local, server, nil); err != errStopped {
		t.Errorf("stopped: %v", err)
	}
}
