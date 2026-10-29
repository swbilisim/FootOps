// Package mongo is a small MongoDB client for the agent: the wire protocol (OP_MSG),
// SCRAM-SHA-256 login and the BSON it needs, using the standard library only. It is not
// a driver: one connection, one command at a time, built by the caller. Documents are
// kept as the bytes the server sent, so one read and written back is the same document.
package mongo

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Type is a BSON element type.
type Type byte

const (
	TypeDouble     Type = 0x01
	TypeString     Type = 0x02
	TypeDocument   Type = 0x03
	TypeArray      Type = 0x04
	TypeBinary     Type = 0x05
	TypeUndefined  Type = 0x06
	TypeObjectID   Type = 0x07
	TypeBool       Type = 0x08
	TypeDate       Type = 0x09
	TypeNull       Type = 0x0a
	TypeRegex      Type = 0x0b
	TypeDBPointer  Type = 0x0c
	TypeJavaScript Type = 0x0d
	TypeSymbol     Type = 0x0e
	TypeCodeScope  Type = 0x0f
	TypeInt32      Type = 0x10
	TypeTimestamp  Type = 0x11
	TypeInt64      Type = 0x12
	TypeDecimal    Type = 0x13
	TypeMinKey     Type = 0xff
	TypeMaxKey     Type = 0x7f
)

// Name is the type's name as MongoDB's $type reports it.
func (t Type) Name() string {
	switch t {
	case TypeDouble:
		return "double"
	case TypeString:
		return "string"
	case TypeDocument:
		return "object"
	case TypeArray:
		return "array"
	case TypeBinary:
		return "binData"
	case TypeUndefined:
		return "undefined"
	case TypeObjectID:
		return "objectId"
	case TypeBool:
		return "bool"
	case TypeDate:
		return "date"
	case TypeNull:
		return "null"
	case TypeRegex:
		return "regex"
	case TypeDBPointer:
		return "dbPointer"
	case TypeJavaScript:
		return "javascript"
	case TypeSymbol:
		return "symbol"
	case TypeCodeScope:
		return "javascriptWithScope"
	case TypeInt32:
		return "int"
	case TypeTimestamp:
		return "timestamp"
	case TypeInt64:
		return "long"
	case TypeDecimal:
		return "decimal"
	case TypeMinKey:
		return "minKey"
	case TypeMaxKey:
		return "maxKey"
	}
	return fmt.Sprintf("0x%02x", byte(t))
}

// MaxDepth is how deeply documents may nest (MongoDB allows 100 levels; the server's own
// replies wrap documents in a few more).
const MaxDepth = 120

// MaxDocument is the largest document MongoDB stores.
const MaxDocument = 16 << 20

var errMalformed = errors.New("bozuk BSON")

// Doc is one BSON document, as bytes.
type Doc []byte

// Value is one element's value: its type and its bytes within the document.
type Value struct {
	Type Type
	Data []byte
}

// Elem is one element of a document.
type Elem struct {
	Key string
	Value
}

// valueLen returns how many bytes a value of type t takes at the start of b, checking
// the lengths it declares.
func valueLen(t Type, b []byte) (int, error) {
	fixed := func(n int) (int, error) {
		if len(b) < n {
			return 0, errMalformed
		}
		return n, nil
	}
	switch t {
	case TypeDouble, TypeDate, TypeTimestamp, TypeInt64:
		return fixed(8)
	case TypeString, TypeJavaScript, TypeSymbol:
		return stringLen(b)
	case TypeDocument, TypeArray:
		if len(b) < 5 {
			return 0, errMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 5 || n > len(b) || b[n-1] != 0 {
			return 0, errMalformed
		}
		return n, nil
	case TypeBinary:
		if len(b) < 5 {
			return 0, errMalformed
		}
		n := binary.LittleEndian.Uint32(b)
		if n > math.MaxInt32 || int(n) > len(b)-5 {
			return 0, errMalformed
		}
		return 5 + int(n), nil
	case TypeUndefined, TypeNull, TypeMinKey, TypeMaxKey:
		return 0, nil
	case TypeObjectID:
		return fixed(12)
	case TypeBool:
		if len(b) < 1 || b[0] > 1 {
			return 0, errMalformed
		}
		return 1, nil
	case TypeRegex:
		p := cstringLen(b)
		if p < 0 {
			return 0, errMalformed
		}
		o := cstringLen(b[p:])
		if o < 0 {
			return 0, errMalformed
		}
		return p + o, nil
	case TypeDBPointer:
		n, err := stringLen(b)
		if err != nil || len(b)-n < 12 {
			return 0, errMalformed
		}
		return n + 12, nil
	case TypeCodeScope:
		if len(b) < 4 {
			return 0, errMalformed
		}
		n := int(binary.LittleEndian.Uint32(b))
		if n < 4+5+5 || n > len(b) {
			return 0, errMalformed
		}
		s, err := stringLen(b[4:n])
		if err != nil {
			return 0, err
		}
		d, err := valueLen(TypeDocument, b[4+s:n])
		if err != nil || 4+s+d != n {
			return 0, errMalformed
		}
		return n, nil
	case TypeInt32:
		return fixed(4)
	case TypeDecimal:
		return fixed(16)
	}
	return 0, fmt.Errorf("%w: bilinmeyen tür 0x%02x", errMalformed, byte(t))
}

// stringLen is the length of a BSON string (int32 length, bytes, NUL).
func stringLen(b []byte) (int, error) {
	if len(b) < 5 {
		return 0, errMalformed
	}
	n := binary.LittleEndian.Uint32(b)
	if n < 1 || n > math.MaxInt32 || int(n) > len(b)-4 || b[4+int(n)-1] != 0 {
		return 0, errMalformed
	}
	return 4 + int(n), nil
}

// cstringLen is the length of a NUL-terminated string including the NUL, -1 if there
// is none.
func cstringLen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i + 1
		}
	}
	return -1
}

// next reads the element at off of a document's element list.
func (d Doc) next(off int) (Elem, int, error) {
	if off >= len(d)-1 {
		return Elem{}, 0, errMalformed
	}
	t := Type(d[off])
	k := cstringLen(d[off+1 : len(d)-1])
	if k < 0 {
		return Elem{}, 0, errMalformed
	}
	start := off + 1 + k
	n, err := valueLen(t, d[start:len(d)-1])
	if err != nil {
		return Elem{}, 0, err
	}
	return Elem{Key: string(d[off+1 : off+k]), Value: Value{Type: t, Data: d[start : start+n]}}, start + n, nil
}

// header checks the document's own length and terminator.
func (d Doc) header() error {
	if len(d) < 5 || int(binary.LittleEndian.Uint32(d)) != len(d) || d[len(d)-1] != 0 {
		return errMalformed
	}
	return nil
}

// Validate checks the whole document, nested ones included.
func (d Doc) Validate() error { return d.validate(0) }

func (d Doc) validate(depth int) error {
	if depth > MaxDepth {
		return fmt.Errorf("%w: çok derin iç içe belge", errMalformed)
	}
	if err := d.header(); err != nil {
		return err
	}
	for off := 4; off < len(d)-1; {
		e, n, err := d.next(off)
		if err != nil {
			return err
		}
		switch e.Type {
		case TypeDocument, TypeArray:
			if err := Doc(e.Data).validate(depth + 1); err != nil {
				return err
			}
		case TypeCodeScope:
			s, _ := stringLen(e.Data[4:])
			if err := Doc(e.Data[4+s:]).validate(depth + 1); err != nil {
				return err
			}
		}
		off = n
	}
	return nil
}

// Elems returns the top-level elements.
func (d Doc) Elems() ([]Elem, error) {
	if err := d.header(); err != nil {
		return nil, err
	}
	var out []Elem
	for off := 4; off < len(d)-1; {
		e, n, err := d.next(off)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
		off = n
	}
	return out, nil
}

// Lookup returns a top-level field.
func (d Doc) Lookup(key string) (Value, bool) {
	if d.header() != nil {
		return Value{}, false
	}
	for off := 4; off < len(d)-1; {
		e, n, err := d.next(off)
		if err != nil {
			return Value{}, false
		}
		if e.Key == key {
			return e.Value, true
		}
		off = n
	}
	return Value{}, false
}

// Doc returns an embedded document or array.
func (v Value) Doc() (Doc, bool) {
	if v.Type != TypeDocument && v.Type != TypeArray {
		return nil, false
	}
	return Doc(v.Data), true
}

// Str returns a string value.
func (v Value) Str() (string, bool) {
	if v.Type != TypeString || len(v.Data) < 5 {
		return "", false
	}
	return string(v.Data[4 : len(v.Data)-1]), true
}

// Int returns a numeric value as an integer (int32, int64, or a whole double).
func (v Value) Int() (int64, bool) {
	switch v.Type {
	case TypeInt32:
		return int64(int32(binary.LittleEndian.Uint32(v.Data))), true
	case TypeInt64:
		return int64(binary.LittleEndian.Uint64(v.Data)), true
	case TypeDouble:
		f := math.Float64frombits(binary.LittleEndian.Uint64(v.Data))
		if f == math.Trunc(f) && math.Abs(f) < 1<<62 {
			return int64(f), true
		}
	}
	return 0, false
}

// Bool returns a boolean value.
func (v Value) Bool() (bool, bool) {
	if v.Type != TypeBool || len(v.Data) != 1 {
		return false, false
	}
	return v.Data[0] == 1, true
}

// Builder writes a document element by element. Containers are opened and closed in
// order; inside an array the keys are written for the caller ("0", "1", …).
type Builder struct {
	buf    []byte
	levels []level
	err    error
}

type level struct {
	start int
	array bool
	n     int
}

// NewBuilder starts a document.
func NewBuilder() *Builder {
	b := &Builder{}
	b.open(false)
	return b
}

func (b *Builder) open(array bool) {
	b.levels = append(b.levels, level{start: len(b.buf), array: array})
	b.buf = append(b.buf, 0, 0, 0, 0)
}

func (b *Builder) key(t Type, k string) {
	if len(b.levels) == 0 {
		b.err = errors.New("BSON: kapanmış belgeye yazma")
		return
	}
	top := &b.levels[len(b.levels)-1]
	if top.array {
		k = strconv.Itoa(top.n)
		top.n++
	}
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			b.err = errors.New("alan adında NUL karakteri olamaz")
			return
		}
	}
	b.buf = append(b.buf, byte(t))
	b.buf = append(b.buf, k...)
	b.buf = append(b.buf, 0)
}

// String appends a string.
func (b *Builder) String(k, v string) *Builder {
	b.key(TypeString, k)
	b.buf = appendString(b.buf, v)
	return b
}

func appendString(buf []byte, v string) []byte {
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(v)+1))
	buf = append(buf, v...)
	return append(buf, 0)
}

// Int32 appends a 32-bit integer.
func (b *Builder) Int32(k string, v int32) *Builder {
	b.key(TypeInt32, k)
	b.buf = binary.LittleEndian.AppendUint32(b.buf, uint32(v))
	return b
}

// Int64 appends a 64-bit integer.
func (b *Builder) Int64(k string, v int64) *Builder {
	b.key(TypeInt64, k)
	b.buf = binary.LittleEndian.AppendUint64(b.buf, uint64(v))
	return b
}

// Double appends a double.
func (b *Builder) Double(k string, v float64) *Builder {
	b.key(TypeDouble, k)
	b.buf = binary.LittleEndian.AppendUint64(b.buf, math.Float64bits(v))
	return b
}

// Bool appends a boolean.
func (b *Builder) Bool(k string, v bool) *Builder {
	b.key(TypeBool, k)
	if v {
		b.buf = append(b.buf, 1)
	} else {
		b.buf = append(b.buf, 0)
	}
	return b
}

// Null appends null.
func (b *Builder) Null(k string) *Builder {
	b.key(TypeNull, k)
	return b
}

// Binary appends binary data.
func (b *Builder) Binary(k string, subtype byte, data []byte) *Builder {
	b.key(TypeBinary, k)
	b.buf = binary.LittleEndian.AppendUint32(b.buf, uint32(len(data)))
	b.buf = append(b.buf, subtype)
	b.buf = append(b.buf, data...)
	return b
}

// Value appends a value as it is (a field copied from another document).
func (b *Builder) Value(k string, v Value) *Builder {
	b.key(v.Type, k)
	b.buf = append(b.buf, v.Data...)
	return b
}

// Doc appends a whole document.
func (b *Builder) Doc(k string, d Doc) *Builder {
	return b.Value(k, Value{Type: TypeDocument, Data: d})
}

// StartDoc opens an embedded document; End closes it.
func (b *Builder) StartDoc(k string) *Builder {
	b.key(TypeDocument, k)
	b.open(false)
	return b
}

// StartArray opens an array; its elements are appended with any key.
func (b *Builder) StartArray(k string) *Builder {
	b.key(TypeArray, k)
	b.open(true)
	return b
}

// End closes the innermost open document or array.
func (b *Builder) End() *Builder {
	if len(b.levels) < 2 {
		b.err = errors.New("BSON: kapatılacak iç belge yok")
		return b
	}
	b.close()
	return b
}

func (b *Builder) close() {
	top := b.levels[len(b.levels)-1]
	b.levels = b.levels[:len(b.levels)-1]
	b.buf = append(b.buf, 0)
	binary.LittleEndian.PutUint32(b.buf[top.start:], uint32(len(b.buf)-top.start))
}

// Build finishes the document.
func (b *Builder) Build() (Doc, error) {
	if b.err != nil {
		return nil, b.err
	}
	if len(b.levels) != 1 {
		return nil, errors.New("BSON: kapanmamış iç belge")
	}
	b.close()
	if len(b.buf) > MaxDocument {
		return nil, fmt.Errorf("belge çok büyük (%d bayt; en çok 16 MB)", len(b.buf))
	}
	return Doc(b.buf), nil
}

// MustBuild is Build for commands the program itself writes.
func (b *Builder) MustBuild() Doc {
	d, err := b.Build()
	if err != nil {
		panic(err)
	}
	return d
}

// Empty is the empty document.
var Empty = Doc{5, 0, 0, 0, 0}
