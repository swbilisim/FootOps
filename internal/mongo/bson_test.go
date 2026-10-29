package mongo

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuilderAndLookup(t *testing.T) {
	d, err := NewBuilder().String("ad", "Ürün").Int32("adet", 5).Int64("büyük", 1<<40).Double("fiyat", 12.5).
		Bool("var", true).Null("yok").StartDoc("ic").String("x", "y").End().
		StartArray("etiket").String("", "a").String("", "b").End().Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	elems, err := d.Elems()
	if err != nil || len(elems) != 8 {
		t.Fatalf("elems = %d, %v", len(elems), err)
	}
	if s, ok := mustLookup(t, d, "ad").Str(); !ok || s != "Ürün" {
		t.Fatalf("ad = %q", s)
	}
	if n, ok := mustLookup(t, d, "adet").Int(); !ok || n != 5 {
		t.Fatalf("adet = %d", n)
	}
	if n, ok := mustLookup(t, d, "büyük").Int(); !ok || n != 1<<40 {
		t.Fatalf("büyük = %d", n)
	}
	arr, _ := mustLookup(t, d, "etiket").Doc()
	ae, _ := arr.Elems()
	if len(ae) != 2 || ae[0].Key != "0" || ae[1].Key != "1" {
		t.Fatalf("array keys: %+v", ae)
	}
	if _, ok := d.Lookup("olmayan"); ok {
		t.Fatal("lookup of a missing field")
	}
}

func mustLookup(t *testing.T, d Doc, k string) Value {
	t.Helper()
	v, ok := d.Lookup(k)
	if !ok {
		t.Fatalf("%s not found", k)
	}
	return v
}

func TestBuilderRefusesNUL(t *testing.T) {
	if _, err := NewBuilder().String("a\x00b", "x").Build(); err == nil {
		t.Fatal("NUL in a key accepted")
	}
	if _, err := NewBuilder().StartDoc("a").Build(); err == nil {
		t.Fatal("unclosed document accepted")
	}
	if _, err := NewBuilder().End().Build(); err == nil {
		t.Fatal("End without StartDoc accepted")
	}
}

func TestValidateRejectsMalformed(t *testing.T) {
	good := NewBuilder().String("a", "b").StartDoc("c").Int32("d", 1).End().MustBuild()
	cases := map[string][]byte{
		"short":         {1, 0, 0},
		"length":        append([]byte{99, 0, 0, 0}, good[4:]...),
		"terminator":    append(append([]byte{}, good[:len(good)-1]...), 1),
		"unknown type":  {8, 0, 0, 0, 0x20, 'a', 0, 0},
		"string length": {14, 0, 0, 0, 2, 'a', 0, 100, 0, 0, 0, 'x', 0, 0},
		"no key end":    {7, 0, 0, 0, 2, 'a', 0},
		"bool value":    {9, 0, 0, 0, 8, 'a', 0, 2, 0},
	}
	for name, b := range cases {
		if err := Doc(b).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A nested document whose own length runs past its parent.
	bad := bytes.Clone(good)
	i := bytes.Index(bad, []byte{3, 'c', 0}) + 3
	binary.LittleEndian.PutUint32(bad[i:], 200)
	if err := Doc(bad).Validate(); err == nil {
		t.Error("nested length accepted")
	}
	// Nesting deeper than MaxDepth.
	b := NewBuilder()
	for range MaxDepth + 2 {
		b.StartDoc("a")
	}
	for range MaxDepth + 2 {
		b.End()
	}
	deep, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := deep.Validate(); err == nil {
		t.Error("too deep accepted")
	}
}

func FuzzValidate(f *testing.F) {
	f.Add([]byte(NewBuilder().String("a", "b").MustBuild()))
	f.Add([]byte(NewBuilder().StartArray("x").Int32("", 1).End().MustBuild()))
	f.Fuzz(func(t *testing.T, b []byte) {
		d := Doc(b)
		if d.Validate() != nil {
			return
		}
		// A valid document can be listed, written and looked up without panicking.
		_, _ = d.Elems()
		_, _ = DocText(d, true)
		_ = Keys(d, func(string) error { return nil })
	})
}
