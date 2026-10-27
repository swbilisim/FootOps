package archive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// A mongodump archive (--archive) starts with a prelude: the magic number, a header
// document (format, server and tool versions), one document per collection with its
// database, name and type, and a terminator. Reading it tells which databases and
// collections the archive holds without restoring anything.
//
// Only the few BSON types those documents use are understood; FootOps has no BSON
// library (the binary stays small).

// MongoNamespace is one collection (or view) listed in an archive.
type MongoNamespace struct {
	DB         string `json:"db"`
	Collection string `json:"collection"`
	Type       string `json:"type,omitempty"` // "collection", "view", "timeseries"; empty in old archives
}

// MongoPrelude is what a mongodump archive says about itself.
type MongoPrelude struct {
	FormatVersion string           `json:"format_version"`
	ServerVersion string           `json:"server_version"`
	ToolVersion   string           `json:"tool_version"`
	Namespaces    []MongoNamespace `json:"namespaces"`
}

// The prelude comes from an untrusted upload: it may claim anything. Real ones are a few
// hundred bytes per collection (the collection's options and index definitions).
const (
	maxPreludeDoc        = 4 << 20
	maxPreludeBytes      = 64 << 20
	maxPreludeNamespaces = 100_000
	maxDBName            = 64  // MongoDB's limit
	maxCollName          = 255 // MongoDB's namespace limit
	preludeTerminator    = 0xffffffff
)

// ReadMongoPrelude reads the prelude from the start of an uncompressed mongodump archive.
// It holds at most one prelude document in memory and keeps only names.
func ReadMongoPrelude(r io.Reader) (MongoPrelude, error) {
	var p MongoPrelude
	r = &io.LimitedReader{R: r, N: maxPreludeBytes}
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return p, fmt.Errorf("mongodump arşivi okunamadı: %w", err)
	}
	if string(magic[:]) != string(mongoMagic) {
		return p, errors.New("mongodump arşivi değil")
	}
	header, end, err := readBSONOrEnd(r)
	if err != nil {
		return p, fmt.Errorf("arşiv başlığı: %w", err)
	}
	if end {
		return p, errors.New("arşiv başlığı yok")
	}
	f, err := bsonFields(header, "version", "server_version", "tool_version")
	if err != nil {
		return p, fmt.Errorf("arşiv başlığı: %w", err)
	}
	p.FormatVersion, p.ServerVersion, p.ToolVersion = clip(f["version"]), clip(f["server_version"]), clip(f["tool_version"])
	for {
		doc, end, err := readBSONOrEnd(r)
		if err != nil {
			return p, fmt.Errorf("koleksiyon listesi: %w", err)
		}
		if end {
			return p, nil
		}
		if len(p.Namespaces) >= maxPreludeNamespaces {
			return p, fmt.Errorf("arşivde çok fazla koleksiyon var (sınır %d)", maxPreludeNamespaces)
		}
		f, err := bsonFields(doc, "db", "collection", "type")
		if err != nil {
			return p, fmt.Errorf("koleksiyon listesi: %w", err)
		}
		if len(f["db"]) > maxDBName || len(f["collection"]) > maxCollName || len(f["type"]) > 32 {
			return p, errors.New("koleksiyon listesi: ad çok uzun")
		}
		p.Namespaces = append(p.Namespaces, MongoNamespace{DB: f["db"], Collection: f["collection"], Type: f["type"]})
	}
}

// Databases lists the databases in the archive, sorted.
func (p MongoPrelude) Databases() []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range p.Namespaces {
		if n.DB != "" && !seen[n.DB] {
			seen[n.DB] = true
			out = append(out, n.DB)
		}
	}
	sort.Strings(out)
	return out
}

// Collections lists one database's collections and views, sorted, leaving out
// system collections (they are never imported).
func (p MongoPrelude) Collections(db string) []MongoNamespace {
	var out []MongoNamespace
	for _, n := range p.Namespaces {
		if n.DB == db && n.Collection != "" && !strings.HasPrefix(n.Collection, "system.") {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Collection < out[b].Collection })
	return out
}

// readBSONOrEnd reads one BSON document, or reports the prelude's terminator.
func readBSONOrEnd(r io.Reader) ([]byte, bool, error) {
	var lb [4]byte
	if _, err := io.ReadFull(r, lb[:]); err != nil {
		return nil, false, fmt.Errorf("arşiv yarım: %w", err)
	}
	n := binary.LittleEndian.Uint32(lb[:])
	if n == preludeTerminator {
		return nil, true, nil
	}
	if n < 5 || n > maxPreludeDoc {
		return nil, false, fmt.Errorf("geçersiz belge boyu (%d)", n)
	}
	// Grown as bytes arrive: a forged length costs nothing until the data is really there.
	rest, err := io.ReadAll(io.LimitReader(r, int64(n)-4))
	if err != nil {
		return nil, false, fmt.Errorf("arşiv okunamadı: %w", err)
	}
	if len(rest) != int(n)-4 {
		return nil, false, errors.New("arşiv yarım")
	}
	return append(lb[:], rest...), false, nil
}

// clip shortens a version string for display.
func clip(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// bsonFields walks a document's top-level elements and returns the string values of the
// wanted keys; everything else is skipped.
func bsonFields(doc []byte, wanted ...string) (map[string]string, error) {
	bad := errors.New("bozuk BSON")
	if len(doc) < 5 || int(binary.LittleEndian.Uint32(doc)) != len(doc) || doc[len(doc)-1] != 0 {
		return nil, bad
	}
	out := map[string]string{}
	b := doc[4 : len(doc)-1]
	for len(b) > 0 {
		t := b[0]
		b = b[1:]
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return nil, bad
		}
		key := string(b[:i])
		b = b[i+1:]
		var size int
		switch t {
		case 0x01, 0x09, 0x11, 0x12: // double, datetime, timestamp, int64
			size = 8
		case 0x10: // int32
			size = 4
		case 0x08: // bool
			size = 1
		case 0x0a: // null
			size = 0
		case 0x07: // ObjectId
			size = 12
		case 0x13: // decimal128
			size = 16
		case 0x02, 0x0d, 0x0e: // string, JavaScript code, symbol: int32 length + bytes
			if len(b) < 4 {
				return nil, bad
			}
			n := binary.LittleEndian.Uint32(b)
			if n < 1 || uint64(n) > uint64(len(b)-4) || b[4+n-1] != 0 {
				return nil, bad
			}
			if t == 0x02 && slicesContains(wanted, key) {
				out[key] = string(b[4 : 4+n-1])
			}
			size = 4 + int(n)
		case 0x03, 0x04: // document, array: their own length
			if len(b) < 4 {
				return nil, bad
			}
			size = int(binary.LittleEndian.Uint32(b))
			if size < 5 {
				return nil, bad
			}
		case 0x05: // binary: int32 length + subtype + bytes
			if len(b) < 4 {
				return nil, bad
			}
			n := binary.LittleEndian.Uint32(b)
			if uint64(n) > math.MaxInt32 {
				return nil, bad
			}
			size = 5 + int(n)
		default:
			return nil, fmt.Errorf("desteklenmeyen BSON türü 0x%02x", t)
		}
		if size > len(b) {
			return nil, bad
		}
		b = b[size:]
	}
	return out, nil
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
