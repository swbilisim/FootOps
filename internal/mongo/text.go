package mongo

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The panel shows and edits documents as text: MongoDB Extended JSON (relaxed), with
// number rules that keep each value's type, so that a document read and saved
// unchanged is the same document, byte for byte:
//
//	int32         5
//	double        5.0, 1.5, 1e+21 (always a point or an exponent; whole numbers beyond
//	              int32 may be written without, as the Node.js driver stores them)
//	int64         {"$numberLong": "5"}
//	decimal       {"$numberDecimal": "12.50"}
//	date          {"$date": "2026-10-28T09:47:19.647Z"}
//	ObjectId      {"$oid": "…"}
//
// and the canonical Extended JSON wrappers for the other types. A whole number that
// fits no int32 is a double only if the double holds it exactly; otherwise it is
// refused rather than rounded.

// TextInfo says whether a document's text reads back exactly.
type TextInfo struct {
	Lossy  bool // invalid UTF-8, or a NaN or decimal with bits the text cannot carry
	Dollar bool // a field name starts with "$": it would read back as a type or operator
	Dup    bool // a field name appears twice in one document
}

// Exact reports whether the text reads back as the same document.
func (i TextInfo) Exact() bool { return !i.Lossy && !i.Dollar && !i.Dup }

func (i *TextInfo) add(o TextInfo) {
	i.Lossy = i.Lossy || o.Lossy
	i.Dollar = i.Dollar || o.Dollar
	i.Dup = i.Dup || o.Dup
}

type writer struct {
	buf    []byte
	pretty bool
	info   TextInfo
	limit  int // stop writing past this many bytes (0: no limit)
	over   bool
}

// DocText writes a document. Pretty puts one field per line, keeping short containers
// on one line.
func DocText(d Doc, pretty bool) (string, TextInfo) {
	w := &writer{pretty: pretty}
	w.doc(d, false, 0)
	return string(w.buf), w.info
}

// ValueText writes one value on one line.
func ValueText(v Value) (string, TextInfo) {
	w := &writer{}
	w.value(v, 0)
	return string(w.buf), w.info
}

// ValueTextLimit is ValueText cut after about n bytes (the result then ends in "…").
func ValueTextLimit(v Value, n int) string {
	w := &writer{limit: n}
	w.value(v, 0)
	if w.over {
		return Clip(string(w.buf), n)
	}
	return string(w.buf)
}

// Quote writes a string as the text form does (JSON, Turkish letters as they are).
func Quote(s string) string {
	b, _ := appendQuoted(nil, s)
	return string(b)
}

// Clip shortens s to at most n runes, ending in "…" if it was cut.
func Clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:max(n-1, 0)]) + "…"
}

func (w *writer) put(s string) {
	if w.limit > 0 && len(w.buf) > w.limit {
		w.over = true
		return
	}
	w.buf = append(w.buf, s...)
}

func (w *writer) str(s string) {
	if w.limit > 0 && len(w.buf) > w.limit {
		w.over = true
		return
	}
	var lossy bool
	w.buf, lossy = appendQuoted(w.buf, s)
	w.info.Lossy = w.info.Lossy || lossy
}

// appendQuoted writes s as a JSON string. Invalid UTF-8 becomes U+FFFD (reported).
func appendQuoted(buf []byte, s string) ([]byte, bool) {
	lossy := false
	buf = append(buf, '"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			buf = append(buf, `�`...)
			lossy = true
		case r == '"':
			buf = append(buf, `\"`...)
		case r == '\\':
			buf = append(buf, `\\`...)
		case r == '\n':
			buf = append(buf, `\n`...)
		case r == '\r':
			buf = append(buf, `\r`...)
		case r == '\t':
			buf = append(buf, `\t`...)
		case r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029:
			buf = fmt.Appendf(buf, `\u%04x`, r)
		default:
			buf = append(buf, s[i:i+size]...)
		}
		i += size
	}
	return append(buf, '"'), lossy
}

func (w *writer) newline(depth int) {
	w.put("\n")
	w.put(strings.Repeat("  ", depth))
}

// scalar reports whether a value prints without nesting (wrappers count as scalars).
func scalar(t Type) bool {
	return t != TypeDocument && t != TypeArray && t != TypeCodeScope
}

func (w *writer) doc(d Doc, array bool, depth int) {
	elems, err := d.Elems()
	if err != nil {
		w.put(`"(bozuk BSON)"`)
		w.info.Lossy = true
		return
	}
	open, end := "{", "}"
	if array {
		open, end = "[", "]"
	}
	if len(elems) == 0 {
		w.put(open + end)
		return
	}
	var seen map[string]bool
	if !array && len(elems) > 1 {
		seen = make(map[string]bool, len(elems))
	}
	for i, e := range elems {
		// The text drops an array's keys: they must be the usual 0, 1, … to read back.
		if array && e.Key != strconv.Itoa(i) {
			w.info.Lossy = true
		}
		if !array && strings.HasPrefix(e.Key, "$") {
			w.info.Dollar = true
		}
		if seen != nil {
			if seen[e.Key] {
				w.info.Dup = true
			}
			seen[e.Key] = true
		}
	}
	multi := w.pretty && !w.fitsInline(elems, array)
	w.put(open)
	for i, e := range elems {
		switch {
		case multi && i == 0:
			w.newline(depth + 1)
		case multi:
			w.put(",")
			w.newline(depth + 1)
		case i > 0:
			w.put(", ")
		}
		if !array {
			w.str(e.Key)
			w.put(": ")
		}
		w.value(e.Value, depth+1)
		if w.over {
			return
		}
	}
	if multi {
		w.newline(depth)
	}
	w.put(end)
}

// inlineWidth is how long a container's one-line form may be in pretty text.
const inlineWidth = 72

// fitsInline: a container of a few scalars whose one-line form is short.
func (w *writer) fitsInline(elems []Elem, array bool) bool {
	if len(elems) > 8 {
		return false
	}
	size := 0
	for _, e := range elems {
		if !scalar(e.Type) {
			return false
		}
		size += len(e.Key) + len(e.Data)
	}
	// No scalar's text is shorter than a quarter of its bytes (an int32: 4 bytes, "5").
	if size > 4*inlineWidth {
		return false
	}
	t := &writer{limit: inlineWidth + 8}
	for i, e := range elems {
		if i > 0 {
			t.put(", ")
		}
		if !array {
			t.str(e.Key)
			t.put(": ")
		}
		t.value(e.Value, 0)
	}
	return !t.over && len(t.buf)+2 <= inlineWidth
}

func (w *writer) wrap(key string, val func()) {
	w.put("{")
	w.str(key)
	w.put(": ")
	val()
	w.put("}")
}

func (w *writer) value(v Value, depth int) {
	b := v.Data
	switch v.Type {
	case TypeDouble:
		w.put(doubleText(math.Float64frombits(binary.LittleEndian.Uint64(b)), &w.info))
	case TypeString:
		w.str(string(b[4 : len(b)-1]))
	case TypeDocument:
		w.doc(Doc(b), false, depth)
	case TypeArray:
		w.doc(Doc(b), true, depth)
	case TypeBinary:
		n := binary.LittleEndian.Uint32(b)
		w.wrap("$binary", func() {
			w.put(`{"base64": `)
			w.str(base64.StdEncoding.EncodeToString(b[5 : 5+n]))
			w.put(`, "subType": `)
			w.str(fmt.Sprintf("%02x", b[4]))
			w.put("}")
		})
	case TypeUndefined:
		w.put(`{"$undefined": true}`)
	case TypeObjectID:
		w.wrap("$oid", func() { w.str(hex.EncodeToString(b)) })
	case TypeBool:
		if b[0] == 1 {
			w.put("true")
		} else {
			w.put("false")
		}
	case TypeDate:
		ms := int64(binary.LittleEndian.Uint64(b))
		w.wrap("$date", func() {
			if ms >= 0 && ms <= maxISODate {
				w.str(time.UnixMilli(ms).UTC().Format(isoDate))
			} else {
				w.put(`{"$numberLong": "` + strconv.FormatInt(ms, 10) + `"}`)
			}
		})
	case TypeNull:
		w.put("null")
	case TypeRegex:
		p := cstringLen(b)
		w.wrap("$regularExpression", func() {
			w.put(`{"pattern": `)
			w.str(string(b[:p-1]))
			w.put(`, "options": `)
			w.str(string(b[p : len(b)-1]))
			w.put("}")
		})
	case TypeDBPointer:
		n, _ := stringLen(b)
		w.wrap("$dbPointer", func() {
			w.put(`{"$ref": `)
			w.str(string(b[4 : n-1]))
			w.put(`, "$id": {"$oid": "` + hex.EncodeToString(b[n:n+12]) + `"}}`)
		})
	case TypeJavaScript:
		w.wrap("$code", func() { w.str(string(b[4 : len(b)-1])) })
	case TypeSymbol:
		w.wrap("$symbol", func() { w.str(string(b[4 : len(b)-1])) })
	case TypeCodeScope:
		s, _ := stringLen(b[4:])
		w.put(`{"$code": `)
		w.str(string(b[8 : 4+s-1]))
		w.put(`, "$scope": `)
		w.doc(Doc(b[4+s:]), false, depth)
		w.put("}")
	case TypeInt32:
		w.put(strconv.Itoa(int(int32(binary.LittleEndian.Uint32(b)))))
	case TypeTimestamp:
		i := binary.LittleEndian.Uint32(b)
		t := binary.LittleEndian.Uint32(b[4:])
		w.put(fmt.Sprintf(`{"$timestamp": {"t": %d, "i": %d}}`, t, i))
	case TypeInt64:
		w.put(`{"$numberLong": "` + strconv.FormatInt(int64(binary.LittleEndian.Uint64(b)), 10) + `"}`)
	case TypeDecimal:
		s, ok := decimalText(b)
		if !ok {
			w.info.Lossy = true
		}
		w.wrap("$numberDecimal", func() { w.str(s) })
	case TypeMinKey:
		w.put(`{"$minKey": 1}`)
	case TypeMaxKey:
		w.put(`{"$maxKey": 1}`)
	default:
		w.put(`"(bilinmeyen tür)"`)
		w.info.Lossy = true
	}
}

const (
	isoDate    = "2006-01-02T15:04:05.000Z07:00"
	maxISODate = 253402300799999 // 9999-12-31T23:59:59.999Z
	// canonicalNaN is the NaN JavaScript stores (and the one written back).
	canonicalNaN = 0x7ff8000000000000
)

// doubleText writes a double so that it reads back as a double with the same bits.
func doubleText(f float64, info *TextInfo) string {
	switch {
	case math.IsNaN(f):
		if math.Float64bits(f) != canonicalNaN {
			info.Lossy = true
		}
		return `{"$numberDouble": "NaN"}`
	case math.IsInf(f, 1):
		return `{"$numberDouble": "Infinity"}`
	case math.IsInf(f, -1):
		return `{"$numberDouble": "-Infinity"}`
	case f == math.Trunc(f) && math.Abs(f) < 1<<53:
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if f >= math.MinInt32 && f <= math.MaxInt32 {
			s += ".0" // an int32 otherwise
		}
		return s
	case f == math.Trunc(f):
		return strconv.FormatFloat(f, 'e', -1, 64) // beyond 2^53: digits printed may not be exact
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// TextError is a mistake in a document's text, with its position.
type TextError struct {
	Line, Col int
	Msg       string
}

func (e *TextError) Error() string {
	if e.Line == 0 {
		return e.Msg
	}
	return fmt.Sprintf("satır %d, sütun %d: %s", e.Line, e.Col, e.Msg)
}

// jnode is parsed JSON with object keys in order.
type jnode struct {
	kind  byte // '{' '[' 's' 'n' 'b' '0'
	s     string
	b     bool
	keys  []string
	koffs []int64 // where each key starts
	vals  []*jnode
	off   int64
}

// maxTextDepth leaves room under MongoDB's 100 levels for the command around a document.
const maxTextDepth = 90

type parser struct {
	text  string
	dec   *json.Decoder
	nodes int
}

// maxTextNodes bounds the values one text may hold: a parsed value takes a few hundred
// bytes, and the largest document the panel edits (256 KB) holds fewer than 90 000.
const maxTextNodes = 150_000

// errAt reports a mistake at a byte offset. Decoder offsets point just after the
// previous token, so separators and spaces are skipped to the item meant.
func (p *parser) errAt(off int64, format string, args ...any) error {
	for off >= 0 && int(off) < len(p.text) && strings.IndexByte(" \t\r\n,:", p.text[off]) >= 0 {
		off++
	}
	line, col := 1, 1
	for i := 0; i < int(off) && i < len(p.text); {
		r, n := utf8.DecodeRuneInString(p.text[i:])
		if r == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
		i += n
	}
	return &TextError{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) syntax(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return p.errAt(se.Offset, "JSON hatası: %s", turkishJSON(se.Error()))
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return p.errAt(int64(len(p.text)), "metin yarım kalmış (kapanmayan { ya da [)")
	}
	return p.errAt(p.dec.InputOffset(), "JSON hatası: %s", turkishJSON(err.Error()))
}

func turkishJSON(msg string) string {
	msg = strings.TrimPrefix(msg, "json: ")
	r := strings.NewReplacer("invalid character", "beklenmeyen karakter",
		"looking for beginning of value", "bir değer bekleniyordu",
		"after object key:value pair", "alan:değer çiftinden sonra",
		"after object key", "alan adından sonra",
		"after array element", "dizi öğesinden sonra",
		"looking for beginning of object key string", "alan adı (tırnak içinde) bekleniyordu",
		"in string literal", "metnin içinde",
		"in string escape code", "kaçış dizisinde",
		"in numeric literal", "sayının içinde",
		"after top-level value", "metnin sonunda")
	return r.Replace(msg)
}

func parseJSON(text string) (*jnode, *parser, error) {
	p := &parser{text: text}
	if !utf8.ValidString(text) {
		return nil, p, &TextError{Msg: "metin geçerli UTF-8 değil"}
	}
	p.dec = json.NewDecoder(strings.NewReader(text))
	p.dec.UseNumber()
	n, err := p.node(0)
	if err != nil {
		return nil, p, err
	}
	if _, err := p.dec.Token(); err != io.EOF {
		return nil, p, p.errAt(p.dec.InputOffset(), "tek bir değerden sonra fazladan metin var")
	}
	return n, p, nil
}

func (p *parser) node(depth int) (*jnode, error) {
	if depth > maxTextDepth {
		return nil, p.errAt(p.dec.InputOffset(), "çok derin iç içe yapı (en çok %d seviye)", maxTextDepth)
	}
	if p.nodes++; p.nodes > maxTextNodes {
		return nil, p.errAt(p.dec.InputOffset(), "metinde çok fazla değer var (en çok %d)", maxTextNodes)
	}
	off := p.dec.InputOffset()
	tok, err := p.dec.Token()
	if err != nil {
		return nil, p.syntax(err)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{kind: '{', off: off}
			for p.dec.More() {
				koff := p.dec.InputOffset()
				kt, err := p.dec.Token()
				if err != nil {
					return nil, p.syntax(err)
				}
				k, ok := kt.(string)
				if !ok {
					return nil, p.errAt(koff, "alan adı bekleniyordu")
				}
				v, err := p.node(depth + 1)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.koffs = append(n.koffs, koff)
				n.vals = append(n.vals, v)
			}
			if _, err := p.dec.Token(); err != nil {
				return nil, p.syntax(err)
			}
			return n, nil
		case '[':
			n := &jnode{kind: '[', off: off}
			for p.dec.More() {
				v, err := p.node(depth + 1)
				if err != nil {
					return nil, err
				}
				n.vals = append(n.vals, v)
			}
			if _, err := p.dec.Token(); err != nil {
				return nil, p.syntax(err)
			}
			return n, nil
		}
		return nil, p.errAt(off, "beklenmeyen %q", t)
	case string:
		return &jnode{kind: 's', s: t, off: off}, nil
	case json.Number:
		return &jnode{kind: 'n', s: string(t), off: off}, nil
	case bool:
		return &jnode{kind: 'b', b: t, off: off}, nil
	case nil:
		return &jnode{kind: '0', off: off}, nil
	}
	return nil, p.errAt(off, "beklenmeyen değer")
}

// ParseDoc reads a document written in the text form.
func ParseDoc(text string) (Doc, error) {
	n, p, err := parseJSON(text)
	if err != nil {
		return nil, err
	}
	if n.kind != '{' || wrapperKey(n) != "" {
		return nil, p.errAt(n.off, "metin bir belge olmalı: { … }")
	}
	b := NewBuilder()
	if err := p.fields(b, n, 0); err != nil {
		return nil, err
	}
	d, err := b.Build()
	if err != nil {
		return nil, &TextError{Msg: err.Error()}
	}
	return d, nil
}

// ParseValue reads one value (such as an _id) written in the text form.
func ParseValue(text string) (Value, error) {
	n, p, err := parseJSON(text)
	if err != nil {
		return Value{}, err
	}
	b := NewBuilder()
	if err := p.value(b, "v", n, 0); err != nil {
		return Value{}, err
	}
	d, err := b.Build()
	if err != nil {
		return Value{}, &TextError{Msg: err.Error()}
	}
	v, _ := d.Lookup("v")
	return v, nil
}

func (p *parser) fields(b *Builder, n *jnode, depth int) error {
	seen := make(map[string]bool, len(n.keys))
	for i, k := range n.keys {
		if seen[k] {
			return p.errAt(n.koffs[i], "%q alanı aynı belgede iki kez yazılmış", k)
		}
		seen[k] = true
		if strings.IndexByte(k, 0) >= 0 {
			return p.errAt(n.koffs[i], "alan adında NUL karakteri (\\u0000) olamaz")
		}
		if err := p.value(b, k, n.vals[i], depth+1); err != nil {
			return err
		}
	}
	return nil
}

// wrappers are the Extended JSON type objects and the keys each may hold.
var wrappers = map[string][]string{
	"$oid": {"$oid"}, "$date": {"$date"}, "$numberLong": {"$numberLong"}, "$numberInt": {"$numberInt"},
	"$numberDouble": {"$numberDouble"}, "$numberDecimal": {"$numberDecimal"}, "$binary": {"$binary"},
	"$regularExpression": {"$regularExpression"}, "$timestamp": {"$timestamp"}, "$minKey": {"$minKey"},
	"$maxKey": {"$maxKey"}, "$undefined": {"$undefined"}, "$symbol": {"$symbol"},
	"$code": {"$code", "$scope"}, "$dbPointer": {"$dbPointer"},
}

// wrapperKey returns the type wrapper an object is, "" if it is a plain document. Query
// operators ($gt, $in, $regex …) are plain documents.
func wrapperKey(n *jnode) string {
	if n.kind != '{' {
		return ""
	}
	for _, k := range n.keys {
		if _, ok := wrappers[k]; ok {
			return k
		}
	}
	return ""
}

func (n *jnode) get(k string) *jnode {
	for i, key := range n.keys {
		if key == k {
			return n.vals[i]
		}
	}
	return nil
}

func (p *parser) value(b *Builder, key string, n *jnode, depth int) error {
	switch n.kind {
	case 's':
		b.String(key, n.s)
	case 'b':
		b.Bool(key, n.b)
	case '0':
		b.Null(key)
	case 'n':
		return p.number(b, key, n)
	case '[':
		if depth > maxTextDepth {
			return p.errAt(n.off, "çok derin iç içe yapı")
		}
		b.StartArray(key)
		for _, v := range n.vals {
			if err := p.value(b, "", v, depth+1); err != nil {
				return err
			}
		}
		b.End()
	case '{':
		if w := wrapperKey(n); w != "" {
			return p.wrapper(b, key, w, n, depth)
		}
		b.StartDoc(key)
		if err := p.fields(b, n, depth); err != nil {
			return err
		}
		b.End()
	}
	return nil
}

func (p *parser) number(b *Builder, key string, n *jnode) error {
	s := n.s
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsInf(f, 0) {
			return p.errAt(n.off, "%s: sayı double sınırlarının dışında", s)
		}
		b.Double(key, f)
		return nil
	}
	if i, err := strconv.ParseInt(s, 10, 32); err == nil {
		b.Int32(key, int32(i))
		return nil
	}
	// Beyond int32: a double, as the Node.js driver stores it, but only if exact. No
	// double has more than 309 integer digits; longer text is refused before the
	// (quadratic) conversion.
	if len(s) > 330 {
		return p.errAt(n.off, "sayı çok uzun (%d basamak)", len(s))
	}
	bi, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return p.errAt(n.off, "%s: geçersiz sayı", s)
	}
	f, _ := new(big.Float).SetInt(bi).Float64()
	if math.IsInf(f, 0) || new(big.Float).SetFloat64(f).Cmp(new(big.Float).SetInt(bi)) != 0 {
		return p.errAt(n.off, `%s double olarak tam saklanamaz; 64 bitlik tam sayı için {"$numberLong": "%s"} yaz`, Clip(s, 40), Clip(s, 40))
	}
	b.Double(key, f)
	return nil
}

func (p *parser) strField(n *jnode, what string) (string, error) {
	if n == nil || n.kind != 's' {
		return "", p.errAt(nodeOff(n), "%s metin (tırnak içinde) olmalı", what)
	}
	return n.s, nil
}

func nodeOff(n *jnode) int64 {
	if n == nil {
		return 0
	}
	return n.off
}

// only checks that an object holds exactly the given keys.
func (p *parser) only(n *jnode, what string, keys ...string) error {
	if n == nil || n.kind != '{' {
		return p.errAt(nodeOff(n), "%s bir nesne olmalı", what)
	}
	if len(n.keys) != len(keys) {
		return p.errAt(n.off, "%s yalnız şu alanları içermeli: %s", what, strings.Join(keys, ", "))
	}
	for _, k := range keys {
		if n.get(k) == nil {
			return p.errAt(n.off, "%s yalnız şu alanları içermeli: %s", what, strings.Join(keys, ", "))
		}
	}
	return nil
}

func (p *parser) uint32Field(n *jnode, what string) (uint32, error) {
	if n == nil || n.kind != 'n' {
		return 0, p.errAt(nodeOff(n), "%s bir tam sayı olmalı", what)
	}
	v, err := strconv.ParseUint(n.s, 10, 32)
	if err != nil {
		return 0, p.errAt(n.off, "%s 0 ile 4294967295 arasında bir tam sayı olmalı", what)
	}
	return uint32(v), nil
}

func (p *parser) wrapper(b *Builder, key, w string, n *jnode, depth int) error {
	if w == "$code" {
		for _, k := range n.keys {
			if k != "$code" && k != "$scope" {
				return p.errAt(n.off, "$code nesnesinde yalnız $code ve $scope olabilir")
			}
		}
	} else if len(n.keys) != 1 {
		return p.errAt(n.off, "%s nesnesinde başka alan olamaz", w)
	}
	v := n.get(w)
	switch w {
	case "$oid":
		s, err := p.strField(v, "$oid")
		if err != nil {
			return err
		}
		id, err := hex.DecodeString(s)
		if err != nil || len(id) != 12 {
			return p.errAt(v.off, "$oid 24 onaltılık (0-9, a-f) karakter olmalı")
		}
		b.Value(key, Value{Type: TypeObjectID, Data: id})
	case "$date":
		ms, err := p.date(v)
		if err != nil {
			return err
		}
		b.Value(key, Value{Type: TypeDate, Data: binary.LittleEndian.AppendUint64(nil, uint64(ms))})
	case "$numberLong":
		s, err := p.strField(v, "$numberLong")
		if err != nil {
			return err
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return p.errAt(v.off, "$numberLong 64 bitlik bir tam sayı olmalı")
		}
		b.Int64(key, i)
	case "$numberInt":
		s, err := p.strField(v, "$numberInt")
		if err != nil {
			return err
		}
		i, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return p.errAt(v.off, "$numberInt 32 bitlik bir tam sayı olmalı")
		}
		b.Int32(key, int32(i))
	case "$numberDouble":
		s, err := p.strField(v, "$numberDouble")
		if err != nil {
			return err
		}
		var f float64
		switch s {
		case "NaN":
			f = math.Float64frombits(canonicalNaN)
		case "Infinity":
			f = math.Inf(1)
		case "-Infinity":
			f = math.Inf(-1)
		default:
			f, err = strconv.ParseFloat(s, 64)
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || strings.ContainsAny(s, "xXpP_") {
				return p.errAt(v.off, "$numberDouble bir sayı ya da NaN, Infinity, -Infinity olmalı")
			}
		}
		b.Double(key, f)
	case "$numberDecimal":
		s, err := p.strField(v, "$numberDecimal")
		if err != nil {
			return err
		}
		d, err := parseDecimal(s)
		if err != nil {
			return p.errAt(v.off, "$numberDecimal: %s", err.Error())
		}
		b.Value(key, Value{Type: TypeDecimal, Data: d[:]})
	case "$binary":
		if err := p.only(v, "$binary", "base64", "subType"); err != nil {
			return err
		}
		data, err := p.strField(v.get("base64"), "base64")
		if err != nil {
			return err
		}
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return p.errAt(v.off, "$binary.base64 geçerli base64 değil")
		}
		st, err := p.strField(v.get("subType"), "subType")
		if err != nil {
			return err
		}
		sub, err := strconv.ParseUint(st, 16, 8)
		if err != nil || len(st) > 2 {
			return p.errAt(v.off, "$binary.subType 1-2 onaltılık karakter olmalı (ör. \"00\")")
		}
		b.Binary(key, byte(sub), raw)
	case "$regularExpression":
		if err := p.only(v, "$regularExpression", "pattern", "options"); err != nil {
			return err
		}
		pat, err := p.strField(v.get("pattern"), "pattern")
		if err != nil {
			return err
		}
		opt, err := p.strField(v.get("options"), "options")
		if err != nil {
			return err
		}
		if strings.IndexByte(pat, 0) >= 0 || strings.IndexByte(opt, 0) >= 0 {
			return p.errAt(v.off, "düzenli ifadede NUL karakteri olamaz")
		}
		data := append(append(append([]byte(pat), 0), opt...), 0)
		b.Value(key, Value{Type: TypeRegex, Data: data})
	case "$timestamp":
		if err := p.only(v, "$timestamp", "t", "i"); err != nil {
			return err
		}
		t, err := p.uint32Field(v.get("t"), "$timestamp.t")
		if err != nil {
			return err
		}
		i, err := p.uint32Field(v.get("i"), "$timestamp.i")
		if err != nil {
			return err
		}
		data := binary.LittleEndian.AppendUint32(nil, i)
		b.Value(key, Value{Type: TypeTimestamp, Data: binary.LittleEndian.AppendUint32(data, t)})
	case "$minKey", "$maxKey":
		if v.kind != 'n' || v.s != "1" {
			return p.errAt(v.off, "%s değeri 1 olmalı", w)
		}
		t := TypeMinKey
		if w == "$maxKey" {
			t = TypeMaxKey
		}
		b.Value(key, Value{Type: t})
	case "$undefined":
		if v.kind != 'b' || !v.b {
			return p.errAt(v.off, "$undefined değeri true olmalı")
		}
		b.Value(key, Value{Type: TypeUndefined})
	case "$symbol":
		s, err := p.strField(v, "$symbol")
		if err != nil {
			return err
		}
		b.Value(key, Value{Type: TypeSymbol, Data: appendString(nil, s)})
	case "$code":
		code, err := p.strField(v, "$code")
		if err != nil {
			return err
		}
		scope := n.get("$scope")
		if scope == nil {
			b.Value(key, Value{Type: TypeJavaScript, Data: appendString(nil, code)})
			return nil
		}
		if scope.kind != '{' || wrapperKey(scope) != "" {
			return p.errAt(scope.off, "$scope bir belge olmalı")
		}
		sb := NewBuilder()
		if err := p.fields(sb, scope, depth+1); err != nil {
			return err
		}
		sd, err := sb.Build()
		if err != nil {
			return p.errAt(scope.off, "%s", err.Error())
		}
		s := appendString(nil, code)
		data := binary.LittleEndian.AppendUint32(nil, uint32(4+len(s)+len(sd)))
		data = append(append(data, s...), sd...)
		b.Value(key, Value{Type: TypeCodeScope, Data: data})
	case "$dbPointer":
		if err := p.only(v, "$dbPointer", "$ref", "$id"); err != nil {
			return err
		}
		ref, err := p.strField(v.get("$ref"), "$ref")
		if err != nil {
			return err
		}
		idn := v.get("$id")
		if wrapperKey(idn) != "$oid" {
			return p.errAt(nodeOff(idn), `$dbPointer.$id {"$oid": "…"} olmalı`)
		}
		ib := NewBuilder()
		if err := p.value(ib, "id", idn, depth+1); err != nil {
			return err
		}
		id, _ := ib.MustBuild().Lookup("id")
		b.Value(key, Value{Type: TypeDBPointer, Data: append(appendString(nil, ref), id.Data...)})
	}
	return nil
}

// date reads the value of $date: ISO 8601 text (a date alone means midnight UTC),
// {"$numberLong": "<ms>"} or a whole number of milliseconds.
func (p *parser) date(v *jnode) (int64, error) {
	switch {
	case v != nil && v.kind == 's':
		var t time.Time
		var err error
		if len(v.s) == len("2006-01-02") {
			t, err = time.Parse("2006-01-02", v.s)
		} else {
			t, err = time.Parse(time.RFC3339Nano, v.s)
		}
		if err != nil {
			return 0, p.errAt(v.off, "$date: tarih anlaşılamadı (ör. \"2026-10-28T09:47:19.647Z\" ya da \"2026-10-28\")")
		}
		if t.Nanosecond()%int(time.Millisecond) != 0 {
			return 0, p.errAt(v.off, "$date: en çok milisaniye hassasiyeti (3 basamak) olabilir")
		}
		return t.UnixMilli(), nil
	case v != nil && v.kind == '{' && wrapperKey(v) == "$numberLong" && len(v.keys) == 1:
		s, err := p.strField(v.get("$numberLong"), "$numberLong")
		if err != nil {
			return 0, err
		}
		ms, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, p.errAt(v.off, "$date.$numberLong 64 bitlik bir tam sayı olmalı")
		}
		return ms, nil
	case v != nil && v.kind == 'n':
		ms, err := strconv.ParseInt(v.s, 10, 64)
		if err != nil {
			return 0, p.errAt(v.off, "$date milisaniye olarak bir tam sayı olmalı")
		}
		return ms, nil
	}
	return 0, p.errAt(nodeOff(v), `$date: "2026-10-28T09:47:19.647Z" gibi bir tarih olmalı`)
}

// Keys returns the field names used anywhere in a document (filters are checked for
// forbidden operators this way).
func Keys(d Doc, fn func(key string) error) error {
	elems, err := d.Elems()
	if err != nil {
		return err
	}
	for _, e := range elems {
		if err := fn(e.Key); err != nil {
			return err
		}
		switch e.Type {
		case TypeDocument, TypeArray:
			if err := Keys(Doc(e.Data), fn); err != nil {
				return err
			}
		case TypeCodeScope:
			s, _ := stringLen(e.Data[4:])
			if err := Keys(Doc(e.Data[4+s:]), fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Equal reports whether two values are the same bytes (and type).
func Equal(a, b Value) bool { return a.Type == b.Type && bytes.Equal(a.Data, b.Data) }
