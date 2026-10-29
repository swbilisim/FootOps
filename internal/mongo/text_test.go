package mongo

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// allTypes is a document with every BSON type.
func allTypes(t testing.TB) Doc {
	t.Helper()
	oid := []byte{0x6a, 0xe1, 0xc4, 0xa7, 0xec, 0x71, 0x1d, 0x71, 0x25, 0x39, 0x59, 0x9d}
	dec, _ := parseDecimal("12.50")
	ts := binary.LittleEndian.AppendUint32(nil, 7)
	ts = binary.LittleEndian.AppendUint32(ts, 1730000000)
	scope := NewBuilder().Int32("x", 1).MustBuild()
	code := appendString(nil, "function(){return x}")
	cws := binary.LittleEndian.AppendUint32(nil, uint32(4+len(code)+len(scope)))
	cws = append(append(cws, code...), scope...)
	d, err := NewBuilder().
		Value("_id", Value{Type: TypeObjectID, Data: oid}).
		Double("d", 1.5).Double("dTam", 3).Double("dNeg0", math.Copysign(0, -1)).Double("dBüyük", 1730000000000).
		Double("d2_60", 1<<60).Double("dKüçük", 5e-324).Double("dNaN", math.Float64frombits(canonicalNaN)).
		Double("dInf", math.Inf(1)).Double("dEksiInf", math.Inf(-1)).
		String("s", "Ürün \"tırnak\" \\ \n\t\u0001 ğüşıöç ✓").String("boş", "").
		StartDoc("ic").Int32("x", 1).StartDoc("derin").String("y", "z").End().End().
		StartArray("dizi").Int32("", 1).String("", "iki").StartDoc("").Bool("üç", true).End().End().
		Binary("bin", 0, []byte{0, 1, 2}).Binary("uuid", 4, bytes.Repeat([]byte{0xab}, 16)).
		Value("tanımsız", Value{Type: TypeUndefined}).
		Bool("doğru", true).Bool("yanlış", false).
		Value("tarih", Value{Type: TypeDate, Data: binary.LittleEndian.AppendUint64(nil, uint64(1761644839647))}).
		Value("eskiTarih", Value{Type: TypeDate, Data: binary.LittleEndian.AppendUint64(nil, uint64(0xffffffffffff0000))}).
		Null("yok").
		Value("re", Value{Type: TypeRegex, Data: []byte("^ür.n$\x00im\x00")}).
		Value("ptr", Value{Type: TypeDBPointer, Data: append(appendString(nil, "db.c"), oid...)}).
		Value("kod", Value{Type: TypeJavaScript, Data: appendString(nil, "x = 1")}).
		Value("sembol", Value{Type: TypeSymbol, Data: appendString(nil, "sym")}).
		Value("kodKapsam", Value{Type: TypeCodeScope, Data: cws}).
		Int32("i", -2147483648).Int32("i2", 7).
		Value("ts", Value{Type: TypeTimestamp, Data: ts}).
		Int64("uzun", 1<<40).Int64("uzunKüçük", 5).
		Value("ondalık", Value{Type: TypeDecimal, Data: dec[:]}).
		Value("min", Value{Type: TypeMinKey}).Value("max", Value{Type: TypeMaxKey}).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTextRoundTripAllTypes(t *testing.T) {
	d := allTypes(t)
	for _, pretty := range []bool{false, true} {
		text, info := DocText(d, pretty)
		if !info.Exact() {
			t.Fatalf("not exact: %+v", info)
		}
		back, err := ParseDoc(text)
		if err != nil {
			t.Fatalf("pretty=%v: %v\n%s", pretty, err, text)
		}
		if !bytes.Equal(back, d) {
			t.Fatalf("pretty=%v: round trip differs\n%s", pretty, text)
		}
	}
}

func TestTextForms(t *testing.T) {
	d := allTypes(t)
	text, _ := DocText(d, false)
	for _, want := range []string{
		`"_id": {"$oid": "6ae1c4a7ec711d712539599d"}`,
		`"d": 1.5`, `"dTam": 3.0`, `"dNeg0": -0.0`, `"dBüyük": 1730000000000`,
		`"d2_60": 1.152921504606847e+18`, `"dKüçük": 5e-324`,
		`"dNaN": {"$numberDouble": "NaN"}`, `"dInf": {"$numberDouble": "Infinity"}`,
		`"s": "Ürün \"tırnak\" \\ \n\t\u0001 ğüşıöç ✓"`,
		`"bin": {"$binary": {"base64": "AAEC", "subType": "00"}}`,
		`"tarih": {"$date": "2025-10-28T09:47:19.647Z"}`,
		`"eskiTarih": {"$date": {"$numberLong": "-65536"}}`,
		`"re": {"$regularExpression": {"pattern": "^ür.n$", "options": "im"}}`,
		`"i": -2147483648`, `"uzun": {"$numberLong": "1099511627776"}`,
		`"ts": {"$timestamp": {"t": 1730000000, "i": 7}}`,
		`"ondalık": {"$numberDecimal": "12.50"}`,
		`"kodKapsam": {"$code": "function(){return x}", "$scope": {"x": 1}}`,
		`"min": {"$minKey": 1}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestPrettyLayout(t *testing.T) {
	d := NewBuilder().String("ad", "x").StartArray("kısa").Int32("", 1).Int32("", 2).End().
		StartDoc("ic").String("a", strings.Repeat("u", 100)).End().MustBuild()
	text, _ := DocText(d, true)
	want := "{\n  \"ad\": \"x\",\n  \"kısa\": [1, 2],\n  \"ic\": {\n    \"a\": \"" + strings.Repeat("u", 100) + "\"\n  }\n}"
	if text != want {
		t.Fatalf("got\n%s\nwant\n%s", text, want)
	}
	if s, _ := DocText(Empty, true); s != "{}" {
		t.Fatalf("empty: %q", s)
	}
}

func TestNumberRules(t *testing.T) {
	cases := map[string]Type{
		"5": TypeInt32, "-2147483648": TypeInt32, "2147483647": TypeInt32, "-0": TypeInt32,
		"2147483648": TypeDouble, "5.0": TypeDouble, "1e3": TypeDouble, "9007199254740992": TypeDouble,
		"-9007199254740992": TypeDouble, "1E+21": TypeDouble,
	}
	for in, want := range cases {
		v, err := ParseValue(in)
		if err != nil || v.Type != want {
			t.Errorf("%s: %v %v, want %v", in, v.Type.Name(), err, want.Name())
		}
	}
	for _, bad := range []string{"9007199254740993", "123456789012345678901234567890", "1e400", "01", "1.", ".5", "+1", "NaN", "0x10"} {
		if _, err := ParseValue(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// A whole double outside int32 reads back as the same double.
	for _, f := range []float64{2147483648, -2147483649, 1730000000000, 1 << 53, -(1 << 53), 1 << 60, 1e21, 1.7976931348623157e308} {
		d := NewBuilder().Double("f", f).MustBuild()
		text, _ := DocText(d, false)
		back, err := ParseDoc(text)
		if err != nil || !bytes.Equal(back, d) {
			t.Errorf("%v: %s → %v", f, text, err)
		}
	}
}

func TestWrappers(t *testing.T) {
	good := map[string]Type{
		`{"$oid": "6AE1C4A7EC711D712539599D"}`:            TypeObjectID,
		`{"$date": "2026-10-28"}`:                         TypeDate,
		`{"$date": "2026-10-28T12:47:19+03:00"}`:          TypeDate,
		`{"$date": {"$numberLong": "-1"}}`:                TypeDate,
		`{"$date": 1761644839647}`:                        TypeDate,
		`{"$numberInt": "5"}`:                             TypeInt32,
		`{"$numberDouble": "1"}`:                          TypeDouble,
		`{"$binary": {"subType": "4", "base64": "AAEC"}}`: TypeBinary,
		`{"$code": "x"}`:                                  TypeJavaScript,
		`{"$scope": {}, "$code": "x"}`:                    TypeCodeScope,
		`{"$undefined": true}`:                            TypeUndefined,
		`{"$maxKey": 1}`:                                  TypeMaxKey,
		`{"$dbPointer": {"$ref": "c", "$id": {"$oid": "6ae1c4a7ec711d712539599d"}}}`: TypeDBPointer,
		`{"$gt": 5}`:                       TypeDocument, // an operator, not a type
		`{"$regex": "x", "$options": "i"}`: TypeDocument,
	}
	for in, want := range good {
		v, err := ParseValue(in)
		if err != nil || v.Type != want {
			t.Errorf("%s: %s %v, want %s", in, v.Type.Name(), err, want.Name())
		}
	}
	bad := []string{
		`{"$oid": "xyz"}`, `{"$oid": 5}`, `{"$oid": "6ae1c4a7ec711d712539599d", "a": 1}`,
		`{"$date": "dün"}`, `{"$date": "2026-10-28T09:47:19.6471Z"}`, `{"$numberLong": "1.5"}`,
		`{"$numberLong": 5}`, `{"$numberDouble": "nan"}`, `{"$numberDouble": "0x1p-2"}`,
		`{"$numberDecimal": "1.2.3"}`, `{"$binary": {"base64": "!!"}}`, `{"$binary": {"base64": "AA==", "subType": "100"}}`,
		`{"$regularExpression": {"pattern": "a\u0000", "options": ""}}`, `{"$timestamp": {"t": -1, "i": 0}}`,
		`{"$minKey": 2}`, `{"$undefined": false}`, `{"$code": "x", "$foo": 1}`, `{"$symbol": 1}`,
	}
	for _, in := range bad {
		if _, err := ParseValue(in); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
}

func TestParseDocErrors(t *testing.T) {
	cases := map[string]string{
		"[1]":                                  "belge olmalı",
		`{"$oid": "6ae1c4a7ec711d712539599d"}`: "belge olmalı",
		"{\"a\": 1,\n \"a\": 2}":               `satır 2, sütun 2: "a" alanı aynı belgede iki kez`,
		"{\"a\":\n  tru}":                      "satır 2",
		`{"a": 1} x`:                           "fazladan metin",
		`{"a": `:                               "yarım kalmış",
		"{\"a\\u0000b\": 1}":                   "NUL",
		"{\"a\": \xff}":                        "UTF-8",
	}
	for in, want := range cases {
		_, err := ParseDoc(in)
		var te *TextError
		if err == nil || !errors.As(err, &te) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
	deep := strings.Repeat(`{"a":`, maxTextDepth+5) + "1" + strings.Repeat("}", maxTextDepth+5)
	if _, err := ParseDoc(deep); err == nil || !strings.Contains(err.Error(), "derin") {
		t.Errorf("deep: %v", err)
	}
}

func TestTextInfo(t *testing.T) {
	d := NewBuilder().Int32("a", 1).Int32("a", 2).MustBuild()
	if _, info := DocText(d, false); !info.Dup || info.Exact() {
		t.Error("duplicate key not reported")
	}
	d = NewBuilder().StartDoc("x").Int32("$oid", 1).End().MustBuild()
	if _, info := DocText(d, false); !info.Dollar {
		t.Error("$ key not reported")
	}
	d = NewBuilder().String("s", "a\xffb").MustBuild()
	if text, info := DocText(d, false); !info.Lossy || !strings.Contains(text, `�`) {
		t.Errorf("invalid UTF-8: %s %+v", text, info)
	}
	d = NewBuilder().Double("n", math.Float64frombits(0x7ff8000000000001)).MustBuild()
	if _, info := DocText(d, false); !info.Lossy {
		t.Error("NaN payload not reported")
	}
}

func TestValueTextLimit(t *testing.T) {
	v, _ := ParseValue(`{"a": "` + strings.Repeat("ğ", 500) + `"}`)
	s := ValueTextLimit(v, 40)
	if utf8.RuneCountInString(s) > 40 || !strings.HasSuffix(s, "…") || !utf8.ValidString(s) {
		t.Fatalf("%q", s)
	}
}

// randDoc builds a random document of valid values (keys never start with "$" and are
// unique, so its text is exact).
func randDoc(r *rand.Rand, depth int) Doc {
	b := NewBuilder()
	n := r.IntN(6)
	for i := range n {
		k := randKey(r, i)
		randValue(r, b, k, depth)
	}
	return b.MustBuild()
}

func randKey(r *rand.Rand, i int) string {
	keys := []string{"ad", "fiyat", "ürün", "", "a.b", "çok uzun bir alan adı", "x y", "ğ"}
	return keys[r.IntN(len(keys))] + strconv.Itoa(i)
}

func randString(r *rand.Rand) string {
	runes := []rune("aZ09 \"\\\n\t\u0000\u001fğüşıöçİ✓😀 /<>&")
	var sb strings.Builder
	for range r.IntN(12) {
		sb.WriteRune(runes[r.IntN(len(runes))])
	}
	return sb.String()
}

func randValue(r *rand.Rand, b *Builder, k string, depth int) {
	switch r.IntN(16) {
	case 0:
		f := []float64{0, math.Copysign(0, -1), 1, -1.5, 3, 1 << 31, -(1 << 31), 1 << 53, 1<<53 + 2, 1e21, 1e-7, 5e-324,
			math.MaxFloat64, math.Inf(1), math.Float64frombits(canonicalNaN), r.NormFloat64() * 1e6,
			math.Float64frombits(r.Uint64() &^ (0x7ff << 52))}[r.IntN(17)]
		if math.IsNaN(f) && math.Float64bits(f) != canonicalNaN {
			f = 0
		}
		b.Double(k, f)
	case 1:
		b.String(k, randString(r))
	case 2:
		if depth < 4 {
			b.Doc(k, randDoc(r, depth+1))
		} else {
			b.Null(k)
		}
	case 3:
		if depth < 4 {
			b.StartArray(k)
			for range r.IntN(4) {
				randValue(r, b, "", depth+1)
			}
			b.End()
		} else {
			b.Bool(k, true)
		}
	case 4:
		data := make([]byte, r.IntN(20))
		for i := range data {
			data[i] = byte(r.Uint32())
		}
		b.Binary(k, byte(r.IntN(256)), data)
	case 5:
		id := make([]byte, 12)
		for i := range id {
			id[i] = byte(r.Uint32())
		}
		b.Value(k, Value{Type: TypeObjectID, Data: id})
	case 6:
		b.Bool(k, r.IntN(2) == 0)
	case 7:
		ms := []int64{0, -1, maxISODate, maxISODate + 1, r.Int64N(maxISODate), -r.Int64N(1 << 50), math.MinInt64, math.MaxInt64}[r.IntN(8)]
		b.Value(k, Value{Type: TypeDate, Data: binary.LittleEndian.AppendUint64(nil, uint64(ms))})
	case 8:
		b.Null(k)
	case 9:
		b.Int32(k, int32(r.Uint32()))
	case 10:
		b.Int64(k, int64(r.Uint64()))
	case 11:
		coef := r.Uint64() >> r.IntN(64)
		exp := r.IntN(80) - 40
		var dec [16]byte
		binary.LittleEndian.PutUint64(dec[:], coef)
		binary.LittleEndian.PutUint64(dec[8:], uint64(exp+decBias)<<49|uint64(r.IntN(2))<<63)
		b.Value(k, Value{Type: TypeDecimal, Data: dec[:]})
	case 12:
		ts := binary.LittleEndian.AppendUint64(nil, r.Uint64())
		b.Value(k, Value{Type: TypeTimestamp, Data: ts})
	case 13:
		pat := strings.ReplaceAll(randString(r), "\x00", "")
		b.Value(k, Value{Type: TypeRegex, Data: append(append([]byte(pat), 0), "imsx"[:r.IntN(5)]+"\x00"...)})
	case 14:
		types := []Type{TypeMinKey, TypeMaxKey, TypeUndefined}
		b.Value(k, Value{Type: types[r.IntN(3)]})
	case 15:
		b.Value(k, Value{Type: TypeSymbol, Data: appendString(nil, randString(r))})
	}
}

func TestRandomRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for i := range 20000 {
		d := randDoc(r, 0)
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		text, info := DocText(d, i%2 == 0)
		if !info.Exact() {
			t.Fatalf("not exact: %+v\n%s", info, text)
		}
		back, err := ParseDoc(text)
		if err != nil {
			t.Fatalf("%v\n%s", err, text)
		}
		if !bytes.Equal(back, d) {
			again, _ := DocText(back, false)
			t.Fatalf("round trip differs\n%s\n%s", text, again)
		}
	}
}

func FuzzParseDoc(f *testing.F) {
	text, _ := DocText(allTypes(f), true)
	f.Add(text)
	f.Add(`{"a": {"$gt": 5}, "b": [1, 2.0, {"$numberLong": "3"}]}`)
	f.Add(`{"$date": 1}`)
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDoc(s)
		if err != nil {
			return
		}
		if err := d.Validate(); err != nil {
			t.Fatalf("parsed document invalid: %v", err)
		}
		text, info := DocText(d, false)
		if !info.Exact() {
			return
		}
		d2, err := ParseDoc(text)
		if err != nil || !bytes.Equal(d, d2) {
			t.Fatalf("not stable: %v\n%s", err, text)
		}
	})
}

func TestParseLimits(t *testing.T) {
	// Many values: refused before the tree grows (each parsed value costs memory).
	many := "{\"a\": [" + strings.Repeat("1,", maxTextNodes) + "1]}"
	if _, err := ParseDoc(many); err == nil || !strings.Contains(err.Error(), "çok fazla değer") {
		t.Errorf("many values: %v", err)
	}
	// A huge integer is refused before the quadratic conversion.
	start := time.Now()
	if _, err := ParseValue(strings.Repeat("9", 1<<20)); err == nil || !strings.Contains(err.Error(), "çok uzun") {
		t.Errorf("long integer: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("long integer took %v", d)
	}
	// A whole number as large as doubles go (2^1023, 308 digits) still reads back.
	if v, err := ParseValue(new(big.Int).Lsh(big.NewInt(1), 1023).String()); err != nil || v.Type != TypeDouble {
		t.Errorf("2^1023: %v", err)
	}
}

func TestArrayKeys(t *testing.T) {
	// MongoDB keeps an array's keys as stored; the text cannot carry other keys.
	d := NewBuilder().StartDoc("x").End().MustBuild()
	odd := NewBuilder().Value("a", Value{Type: TypeArray, Data: NewBuilder().Int32("5", 1).Int32("9", 2).MustBuild()}).MustBuild()
	if _, info := DocText(odd, false); !info.Lossy {
		t.Error("array with keys 5, 9 reported exact")
	}
	if _, info := DocText(d, false); !info.Exact() {
		t.Error("plain document not exact")
	}
	if Quote("ğ\"\n") != `"ğ\"\n"` {
		t.Errorf("Quote: %s", Quote("ğ\"\n"))
	}
}
