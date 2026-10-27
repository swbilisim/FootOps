package archive

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The fixtures are real mongodump 100.x output (mongo:7.0.45): magaza.archive holds one
// database with a view; two-dbs.archive.gz is a whole-server dump.
func TestReadMongoPreludeReal(t *testing.T) {
	f, err := os.Open("testdata/magaza.archive")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := ReadMongoPrelude(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.ServerVersion, "7.0") || p.ToolVersion == "" || p.FormatVersion == "" {
		t.Errorf("versions = %+v", p)
	}
	if got := p.Databases(); !reflect.DeepEqual(got, []string{"magaza"}) {
		t.Errorf("databases = %v", got)
	}
	var names []string
	types := map[string]string{}
	for _, n := range p.Collections("magaza") {
		names = append(names, n.Collection)
		types[n.Collection] = n.Type
	}
	if !reflect.DeepEqual(names, []string{"sayfalar", "ucuz", "urunler"}) || types["ucuz"] != "view" || types["urunler"] != "collection" {
		t.Errorf("collections = %v %v", names, types)
	}

	g, err := os.Open("testdata/two-dbs.archive.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	zr, err := gzip.NewReader(g)
	if err != nil {
		t.Fatal(err)
	}
	p, err = ReadMongoPrelude(zr)
	if err != nil {
		t.Fatal(err)
	}
	dbs := p.Databases()
	if !contains(dbs, "magaza") || !contains(dbs, "blog") || !contains(dbs, "admin") {
		t.Errorf("databases = %v", dbs)
	}
	for _, n := range p.Collections("admin") {
		if strings.HasPrefix(n.Collection, "system.") {
			t.Errorf("system collection listed: %v", n)
		}
	}
	if k, err := Detect("testdata/two-dbs.archive.gz"); err != nil || k != MongoArchiveGz {
		t.Errorf("detect = %v %v", k, err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// doc builds a BSON document from string fields plus raw extra elements.
func doc(kv ...string) []byte {
	var body bytes.Buffer
	for i := 0; i+1 < len(kv); i += 2 {
		body.WriteByte(0x02)
		body.WriteString(kv[i])
		body.WriteByte(0)
		binary.Write(&body, binary.LittleEndian, int32(len(kv[i+1])+1))
		body.WriteString(kv[i+1])
		body.WriteByte(0)
	}
	// An int32, an int64 and a sub-document, as mongodump writes, must be skipped.
	body.Write([]byte{0x10, 's', 0, 1, 0, 0, 0})
	body.Write([]byte{0x12, 'l', 0, 1, 0, 0, 0, 0, 0, 0, 0})
	body.Write([]byte{0x03, 'd', 0, 5, 0, 0, 0, 0})
	out := make([]byte, 4, 4+body.Len()+1)
	out = append(out, body.Bytes()...)
	out = append(out, 0)
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	return out
}

func prelude(docs ...[]byte) []byte {
	b := append([]byte{}, mongoMagic...)
	for _, d := range docs {
		b = append(b, d...)
	}
	return append(b, 0xff, 0xff, 0xff, 0xff)
}

func TestReadMongoPreludeErrors(t *testing.T) {
	good := prelude(doc("version", "0.1", "server_version", "4.0.28"), doc("db", "a", "collection", "b", "type", "collection"))
	p, err := ReadMongoPrelude(bytes.NewReader(good))
	if err != nil || p.ServerVersion != "4.0.28" || len(p.Namespaces) != 1 || p.Namespaces[0] != (MongoNamespace{"a", "b", "collection"}) {
		t.Fatalf("good: %+v %v", p, err)
	}
	cases := map[string][]byte{
		"not an archive": []byte("PK\x03\x04aaaaaaaa"),
		"truncated":      good[:len(good)-6],
		"no header":      prelude(),
		"huge length":    append(append([]byte{}, mongoMagic...), 0xfe, 0xff, 0xff, 0x7f),
		"bad string":     prelude(append(doc("version", "x")[:5], 0x02, 'k', 0, 0xff, 0xff, 0xff, 0x0f, 0)),
		"unknown type":   prelude([]byte{8, 0, 0, 0, 0x99, 'k', 0, 0}),
		"long db name":   prelude(doc("version", "0.1"), doc("db", strings.Repeat("x", 65), "collection", "b")),
		"huge document":  prelude(doc("version", "0.1"), doc("db", "a", "collection", "b", "metadata", strings.Repeat("x", maxPreludeDoc))),
	}
	for name, b := range cases {
		if _, err := ReadMongoPrelude(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A small gzip'd archive must not be able to make the reader keep much memory: names are
// capped and only one document is held at a time.
func TestReadMongoPreludeMemory(t *testing.T) {
	var docs [][]byte
	docs = append(docs, doc("version", "0.1"))
	for i := 0; i < 20; i++ {
		docs = append(docs, doc("db", "a", "collection", fmt.Sprintf("c%d", i), "metadata", strings.Repeat("x", 3<<20)))
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, err := ReadMongoPrelude(bytes.NewReader(prelude(docs...)))
	runtime.GC()
	runtime.ReadMemStats(&after)
	if err != nil || len(p.Namespaces) != 20 {
		t.Fatalf("%v %d", err, len(p.Namespaces))
	}
	if kept := int64(after.HeapAlloc) - int64(before.HeapAlloc); kept > 1<<20 {
		t.Errorf("prelude of 60 MB metadata keeps %d bytes", kept)
	}
	big := append([][]byte{doc("version", "0.1")}, docs[1:]...)
	for i := 0; i < 5; i++ {
		big = append(big, docs[1:]...)
	}
	if _, err := ReadMongoPrelude(bytes.NewReader(prelude(big...))); err == nil {
		t.Error("prelude over the total limit accepted")
	}
}

func FuzzReadMongoPrelude(f *testing.F) {
	f.Add(prelude(doc("version", "0.1"), doc("db", "a", "collection", "b")))
	if b, err := os.ReadFile("testdata/magaza.archive"); err == nil {
		f.Add(b[:min(len(b), 2048)])
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := ReadMongoPrelude(bytes.NewReader(b))
		if err == nil && len(p.Namespaces) > maxPreludeNamespaces {
			t.Fatal("too many namespaces")
		}
	})
}
