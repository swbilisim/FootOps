// Package archive checks archives before anything is taken out of them (rule K2: verify
// first, write later). It recognises the type from the content, not the file name,
// reads every byte once to prove integrity (gzip and zip checksums), and refuses
// members that could escape the target directory or exhaust the disk: absolute paths,
// "..", links, device files, decompression bombs.
//
// Everything streams; memory use does not grow with the archive's size.
package archive

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"
)

// Kind is an archive type recognised from its first bytes.
type Kind string

const (
	Unknown        Kind = ""
	Zip            Kind = "zip"
	Tar            Kind = "tar"
	TarGz          Kind = "tar.gz"
	MongoArchive   Kind = "mongodump"    // mongodump --archive
	MongoArchiveGz Kind = "mongodump.gz" // mongodump --archive --gzip
	Gzip           Kind = "gzip"         // gzip of something else
)

// Limits bound what an archive may expand to.
type Limits struct {
	MaxTotal int64 // all members, uncompressed
	MaxFile  int64 // one member
	MaxFiles int   // members, directories included
	// MaxRatio is the most uncompressed bytes per compressed byte. Photos barely
	// compress and database dumps about 10:1; far beyond that is a bomb. It is only
	// applied above ratioFloor, where it matters.
	MaxRatio int64
}

// DefaultLimits follow the operator's decisions (2026-10-27): 2 GB per file, the staging
// area's 10 GB in total. 50,000 members is far above any site so far (the largest upload
// folder had 2,399 files) and keeps the per-member bookkeeping to a few MB.
var DefaultLimits = Limits{MaxTotal: 10 << 30, MaxFile: 2 << 30, MaxFiles: 50_000, MaxRatio: 100}

const ratioFloor = 64 << 20

var (
	gzipMagic  = []byte{0x1f, 0x8b}
	zipMagic   = []byte("PK\x03\x04")
	zipEmpty   = []byte("PK\x05\x06")
	mongoMagic = []byte{0x6d, 0xe2, 0x99, 0x81} // 0x8199e26d, little-endian
)

// sniff recognises an uncompressed head; tar needs at least 262 bytes.
func sniff(head []byte) Kind {
	switch {
	case bytes.HasPrefix(head, zipMagic), bytes.HasPrefix(head, zipEmpty):
		return Zip
	case bytes.HasPrefix(head, mongoMagic):
		return MongoArchive
	case len(head) >= 262 && string(head[257:262]) == "ustar":
		return Tar
	}
	return Unknown
}

// Detect reads the start of a file, and for gzip the start of what it contains.
func Detect(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return Unknown, err
	}
	defer f.Close()
	return detect(f)
}

func detect(r io.Reader) (Kind, error) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(512)
	if !bytes.HasPrefix(head, gzipMagic) {
		return sniff(head), nil
	}
	zr, err := gzip.NewReader(br)
	if err != nil {
		return Unknown, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	inner := make([]byte, 512)
	n, err := io.ReadFull(zr, inner)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return Unknown, fmt.Errorf("gzip içeriği okunamadı: %w", err)
	}
	switch sniff(inner[:n]) {
	case Tar:
		return TarGz, nil
	case MongoArchive:
		return MongoArchiveGz, nil
	}
	return Gzip, nil
}

// Summary describes a checked archive.
type Summary struct {
	Kind       Kind  `json:"kind"`
	Compressed int64 `json:"compressed"` // file size
	Size       int64 `json:"size"`       // uncompressed: member bytes, or the whole stream
	Files      int   `json:"files"`
	Dirs       int   `json:"dirs"`
	// Hidden counts the files Hidden reports (they are never extracted); HiddenBytes is
	// their size.
	Hidden      int   `json:"hidden"`
	HiddenBytes int64 `json:"hidden_bytes"`
	// Prefix is the one top-level folder every other member is in ("uploads" for a
	// zipped uploads folder), "" when there is none.
	Prefix string `json:"prefix,omitempty"`
	// BSON counts .bson files: an archive of a mongodump folder, not of site files.
	BSON int `json:"bson,omitempty"`

	top   string
	mixed bool
}

// Hidden reports whether a cleaned member name is left out when files are extracted:
// a component starting with a dot (.htaccess, .DS_Store, ._x, .git) or macOS's
// __MACOSX folder, wherever it is (a stripped top folder may have held it).
func Hidden(name string) bool {
	for _, c := range strings.Split(name, "/") {
		if strings.HasPrefix(c, ".") || c == "__MACOSX" {
			return true
		}
	}
	return false
}

// note records a member's name for Hidden and Prefix.
func (s *Summary) note(name string, dir bool, size int64) {
	if Hidden(name) {
		if !dir {
			s.Hidden++
			s.HiddenBytes += size
		}
		return
	}
	if !dir && (strings.HasSuffix(name, ".bson") || strings.HasSuffix(name, ".bson.gz")) {
		s.BSON++
	}
	top, _, nested := strings.Cut(name, "/")
	switch {
	case !nested && !dir:
		s.mixed = true // a file at the top level
	case s.top == "":
		s.top = top
	case s.top != top:
		s.mixed = true
	}
	if s.mixed {
		s.Prefix = ""
	} else {
		s.Prefix = s.top
	}
}

// Check verifies a whole archive file against l. Mongo archives are checked for
// integrity only here; their contents are checked by a trial restore.
func Check(path string, l Limits) (Summary, error) {
	kind, err := Detect(path)
	if err != nil {
		return Summary{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Kind: kind, Compressed: st.Size()}
	switch kind {
	case Zip:
		err = checkZip(path, l, &s)
	case Tar, TarGz:
		err = checkTarFile(path, kind == TarGz, l, &s)
	case MongoArchive:
		s.Size = s.Compressed
		if s.Size > l.MaxTotal {
			err = fmt.Errorf("arşiv çok büyük (%d bayt, sınır %d)", s.Size, l.MaxTotal)
		}
	case MongoArchiveGz, Gzip:
		err = checkGzipFile(path, l, &s)
	default:
		err = errors.New("tanınmayan dosya türü: zip, tar, tar.gz veya mongodump arşivi olmalı")
	}
	return s, err
}

// GzipSize reads a gzip stream to the end, which verifies its checksum, and returns the
// uncompressed size. It stops with an error beyond max bytes.
func GzipSize(r io.Reader, max int64) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(zr, max+1))
	if err != nil {
		return n, fmt.Errorf("gzip bozuk (%d bayttan sonra): %w", n, err)
	}
	if n > max {
		return n, fmt.Errorf("açılmış hali %d baytı aşıyor", max)
	}
	return n, nil
}

func checkGzipFile(path string, l Limits, s *Summary) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s.Size, err = GzipSize(bufio.NewReader(f), l.MaxTotal)
	if err != nil {
		return err
	}
	return checkRatio(s.Size, s.Compressed, l)
}

// CheckRatio refuses a decompression bomb: far more output than input, above a floor.
func CheckRatio(size, compressed int64, l Limits) error { return checkRatio(size, compressed, l) }

func checkRatio(size, compressed int64, l Limits) error {
	if size > ratioFloor && l.MaxRatio > 0 && size > compressed*l.MaxRatio {
		return fmt.Errorf("sıkıştırma oranı şüpheli (%d:1, sınır %d:1) — sıkıştırma bombası olabilir", size/max(compressed, 1), l.MaxRatio)
	}
	return nil
}

func checkTarFile(path string, gz bool, l Limits, s *Summary) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = bufio.NewReader(f)
	if gz {
		zr, err := gzip.NewReader(r)
		if err != nil {
			return fmt.Errorf("gzip başlığı bozuk: %w", err)
		}
		r = zr
	}
	if err := CheckTar(r, l, s); err != nil {
		return err
	}
	if gz {
		// The tar reader stops at the end marker; the gzip checksum comes after it and
		// after the padding, which counts towards the size.
		n, err := io.Copy(io.Discard, io.LimitReader(r, l.MaxTotal-s.Size+1))
		if err != nil {
			return fmt.Errorf("gzip bozuk: %w", err)
		}
		if s.Size+n > l.MaxTotal {
			return fmt.Errorf("arşivin açılmış hali çok büyük (sınır %d bayt)", l.MaxTotal)
		}
	}
	return checkRatio(s.Size, s.Compressed, l)
}

// CheckTar reads a tar stream to its end, checking every member.
func CheckTar(r io.Reader, l Limits, s *Summary) error {
	tr := tar.NewReader(r)
	m := newMembers(l, s)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar bozuk: %w", err)
		}
		name, skip, err := m.name(h.Name)
		if err != nil {
			return err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if !skip {
				s.Dirs++
				s.note(name, true, 0)
			}
			if err := m.count(); err != nil {
				return err
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%s: bağlantı (link) kabul edilmez", name)
		default:
			return fmt.Errorf("%s: desteklenmeyen üye türü (%q); yalnız dosya ve klasör", name, h.Typeflag)
		}
		if skip {
			return fmt.Errorf("%q: dosya adı geçersiz", h.Name)
		}
		if err := m.file(name, h.Size); err != nil {
			return err
		}
		s.note(name, false, h.Size)
		n, err := io.Copy(io.Discard, tr)
		if err != nil {
			return fmt.Errorf("%s: okunamadı: %w", name, err)
		}
		if n != h.Size {
			return fmt.Errorf("%s: boyut tutmuyor", name)
		}
	}
}

func checkZip(path string, l Limits, s *Summary) error {
	// archive/zip loads the whole central directory into memory; refuse a huge one first.
	entries, dirSize, err := zipDirectory(path)
	if err != nil {
		return err
	}
	if entries > uint64(l.MaxFiles) {
		return fmt.Errorf("arşivde çok fazla üye var (%d, sınır %d)", entries, l.MaxFiles)
	}
	if dirSize > uint64(l.MaxFiles)*(46+maxPath+64) {
		return errors.New("zip dizini beklenenden büyük")
	}
	zr, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("zip bozuk: %w", err)
	}
	defer zr.Close()
	m := newMembers(l, s)
	for _, f := range zr.File {
		name, skip, err := m.name(f.Name)
		if err != nil {
			return err
		}
		if f.Flags&0x1 != 0 {
			return fmt.Errorf("%s: şifreli zip kabul edilmez", name)
		}
		mode := f.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: bağlantı (link) kabul edilmez", name)
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			if !skip {
				s.Dirs++
				s.note(name, true, 0)
			}
			if err := m.count(); err != nil {
				return err
			}
			continue
		case !mode.IsRegular():
			return fmt.Errorf("%s: desteklenmeyen üye türü; yalnız dosya ve klasör", name)
		}
		if skip {
			return fmt.Errorf("%q: dosya adı geçersiz", f.Name)
		}
		if f.UncompressedSize64 > uint64(l.MaxFile) {
			return fmt.Errorf("%s: dosya çok büyük (sınır %d bayt)", name, l.MaxFile)
		}
		size := int64(f.UncompressedSize64)
		if err := m.file(name, size); err != nil {
			return err
		}
		s.note(name, false, size)
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("%s: açılamadı: %w", name, err)
		}
		// archive/zip checks the CRC and the declared size at the end of the member.
		n, err := io.Copy(io.Discard, io.LimitReader(rc, size+1))
		rc.Close()
		if err != nil {
			return fmt.Errorf("%s: bozuk: %w", name, err)
		}
		if n != size {
			return fmt.Errorf("%s: boyut tutmuyor", name)
		}
	}
	return checkRatio(s.Size, s.Compressed, l)
}

// zipDirectory reads the entry count and central directory size from the end record
// (zip64 aware) without loading the directory.
func zipDirectory(path string) (entries, size uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	// The end record is 22 bytes plus a comment of up to 64 KB.
	n := min(st.Size(), 22+65535)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, st.Size()-n); err != nil {
		return 0, 0, fmt.Errorf("zip okunamadı: %w", err)
	}
	i := bytes.LastIndex(buf, []byte("PK\x05\x06"))
	if i < 0 || len(buf)-i < 22 {
		return 0, 0, errors.New("zip bozuk: dizin sonu bulunamadı")
	}
	eocd := buf[i:]
	entries = uint64(le16(eocd[10:]))
	size = uint64(le32(eocd[12:]))
	if entries != 0xffff && size != 0xffffffff {
		return entries, size, nil
	}
	// zip64: a locator right before the end record points at the zip64 end record.
	if i < 20 || string(buf[i-20:i-16]) != "PK\x06\x07" {
		return 0, 0, errors.New("zip bozuk: zip64 bilgisi yok")
	}
	off := int64(le64(buf[i-20+8:]))
	rec := make([]byte, 56)
	if off < 0 || off > st.Size()-56 {
		return 0, 0, errors.New("zip bozuk: zip64 kaydı dışarıda")
	}
	if _, err := f.ReadAt(rec, off); err != nil || string(rec[:4]) != "PK\x06\x06" {
		return 0, 0, errors.New("zip bozuk: zip64 kaydı okunamadı")
	}
	return le64(rec[32:]), le64(rec[40:]), nil
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 { return uint32(le16(b)) | uint32(le16(b[2:]))<<16 }
func le64(b []byte) uint64 { return uint64(le32(b)) | uint64(le32(b[4:]))<<32 }

// members tracks totals and names across an archive.
type members struct {
	l    Limits
	s    *Summary
	seen map[string]bool
}

func newMembers(l Limits, s *Summary) *members {
	return &members{l: l, s: s, seen: map[string]bool{}}
}

// name validates a member name; skip is true for the archive's root ("./").
func (m *members) name(raw string) (string, bool, error) {
	name, err := CleanName(raw)
	if err != nil {
		return "", false, err
	}
	return name, name == "", nil
}

func (m *members) count() error {
	if m.s.Files+m.s.Dirs > m.l.MaxFiles {
		return fmt.Errorf("arşivde çok fazla üye var (sınır %d)", m.l.MaxFiles)
	}
	return nil
}

func (m *members) file(name string, size int64) error {
	if m.seen[name] {
		return fmt.Errorf("%s: arşivde iki kez var", name)
	}
	m.seen[name] = true
	m.s.Files++
	if err := m.count(); err != nil {
		return err
	}
	if size < 0 || size > m.l.MaxFile {
		return fmt.Errorf("%s: dosya çok büyük (sınır %d bayt)", name, m.l.MaxFile)
	}
	m.s.Size += size
	if m.s.Size > m.l.MaxTotal {
		return fmt.Errorf("arşivin açılmış hali çok büyük (sınır %d bayt)", m.l.MaxTotal)
	}
	return nil
}

const (
	maxPath      = 1024
	maxComponent = 255
)

// CleanName validates an archive member name and returns it without a leading "./" or
// trailing "/". The result is always a relative path that stays inside the target
// directory. Turkish and other UTF-8 letters are fine; separators other than "/",
// control characters and "." / ".." components are not.
func CleanName(raw string) (string, error) {
	bad := func(why string) (string, error) {
		return "", fmt.Errorf("%q: geçersiz dosya adı (%s)", trunc(raw), why)
	}
	if !utf8.ValidString(raw) {
		return bad("UTF-8 değil")
	}
	if len(raw) > maxPath {
		return bad("çok uzun")
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return bad("kontrol karakteri")
		}
		if r == '\\' {
			return bad(`"\" kullanılamaz`)
		}
	}
	if strings.HasPrefix(raw, "/") {
		return bad("mutlak yol")
	}
	name := raw
	for strings.HasPrefix(name, "./") {
		name = name[2:]
	}
	name = strings.TrimSuffix(name, "/")
	if name == "" || name == "." {
		return "", nil
	}
	for _, c := range strings.Split(name, "/") {
		switch {
		case c == "":
			return bad("boş klasör adı")
		case c == "." || c == "..":
			return bad(`"." veya ".."`)
		case len(c) > maxComponent:
			return bad("bir parçası çok uzun")
		}
	}
	if len(name) >= 2 && name[1] == ':' && (name[0]|0x20 >= 'a' && name[0]|0x20 <= 'z') {
		return bad("sürücü harfi")
	}
	return name, nil
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
