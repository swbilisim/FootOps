package archive

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type member struct {
	name string
	body string
	typ  byte // tar type; 0 = regular file
	link string
}

func writeTar(t *testing.T, gz bool, ms ...member) string {
	t.Helper()
	var buf bytes.Buffer
	var w *tar.Writer
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = tar.NewWriter(zw)
	} else {
		w = tar.NewWriter(&buf)
	}
	for _, m := range ms {
		h := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg, Linkname: m.link, Format: tar.FormatPAX}
		if m.typ != 0 {
			h.Typeflag = m.typ
			if m.typ != tar.TypeReg {
				h.Size = 0
			}
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := w.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	name := "a.tar"
	if gz {
		name = "a.tar.gz"
	}
	return writeFile(t, name, buf.Bytes())
}

func writeFile(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type zmember struct {
	name string
	body string
	mode fs.FileMode
}

func zipBytes(t *testing.T, ms ...zmember) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, m := range ms {
		h := &zip.FileHeader{Name: m.name, Method: zip.Deflate}
		if m.mode != 0 {
			h.SetMode(m.mode)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(m.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var mongoHead = append([]byte{0x6d, 0xe2, 0x99, 0x81}, make([]byte, 100)...)

func TestDetect(t *testing.T) {
	cases := map[string]struct {
		path string
		want Kind
	}{
		"tar":      {writeTar(t, false, member{name: "a.txt", body: "x"}), Tar},
		"tar.gz":   {writeTar(t, true, member{name: "a.txt", body: "x"}), TarGz},
		"zip":      {writeFile(t, "a.zip", zipBytes(t, zmember{name: "a.txt", body: "x"})), Zip},
		"mongo":    {writeFile(t, "db.archive", mongoHead), MongoArchive},
		"mongo.gz": {writeFile(t, "db.archive.gz", gzipBytes(t, mongoHead)), MongoArchiveGz},
		"gzip":     {writeFile(t, "x.gz", gzipBytes(t, []byte("hello"))), Gzip},
		"text":     {writeFile(t, "x.zip", []byte("not an archive at all")), Unknown},
		"empty":    {writeFile(t, "e", nil), Unknown},
		"emptyzip": {writeFile(t, "e.zip", zipBytes(t)), Zip},
		"misnamed": {writeFile(t, "photo.jpg", zipBytes(t, zmember{name: "a", body: "x"})), Zip},
	}
	for name, c := range cases {
		got, err := Detect(c.path)
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", name, got, err, c.want)
		}
	}
}

func TestCheckAcceptsGoodArchives(t *testing.T) {
	tr := writeTar(t, true,
		member{name: "./", typ: tar.TypeDir},
		member{name: "./uploads/", typ: tar.TypeDir},
		member{name: "./uploads/şeker ğüşiöç.jpg", body: "foto"},
		member{name: "uploads/blog/a.png", body: "png!"},
	)
	s, err := Check(tr, DefaultLimits)
	if err != nil || s.Kind != TarGz || s.Files != 2 || s.Dirs != 1 || s.Size != 8 {
		t.Errorf("tar.gz: %+v %v", s, err)
	}
	z := writeFile(t, "a.zip", zipBytes(t, zmember{name: "klasör/"}, zmember{name: "klasör/görsel.webp", body: "abc"}))
	s, err = Check(z, DefaultLimits)
	if err != nil || s.Kind != Zip || s.Files != 1 || s.Dirs != 1 || s.Size != 3 {
		t.Errorf("zip: %+v %v", s, err)
	}
	m := writeFile(t, "db.gz", gzipBytes(t, mongoHead))
	s, err = Check(m, DefaultLimits)
	if err != nil || s.Kind != MongoArchiveGz || s.Size != int64(len(mongoHead)) {
		t.Errorf("mongo.gz: %+v %v", s, err)
	}
}

func TestCheckRejects(t *testing.T) {
	big := strings.Repeat("x", 100)
	cases := map[string]struct {
		path string
		l    Limits
		want string
	}{
		"zip-slip tar":   {writeTar(t, false, member{name: "../../etc/cron.d/x", body: "x"}), DefaultLimits, `".."`},
		"zip-slip inner": {writeTar(t, false, member{name: "a/../../x", body: "x"}), DefaultLimits, `".."`},
		"absolute":       {writeTar(t, true, member{name: "/etc/passwd", body: "x"}), DefaultLimits, "mutlak"},
		"symlink":        {writeTar(t, false, member{name: "l", typ: tar.TypeSymlink, link: "/etc"}), DefaultLimits, "bağlantı"},
		"hardlink":       {writeTar(t, false, member{name: "l", typ: tar.TypeLink, link: "/etc/shadow"}), DefaultLimits, "bağlantı"},
		"device":         {writeTar(t, false, member{name: "d", typ: tar.TypeChar}), DefaultLimits, "desteklenmeyen"},
		"fifo":           {writeTar(t, false, member{name: "p", typ: tar.TypeFifo}), DefaultLimits, "desteklenmeyen"},
		"backslash":      {writeTar(t, false, member{name: `..\..\x`, body: "x"}), DefaultLimits, `"\"`},
		"control":        {writeTar(t, false, member{name: "a\nb", body: "x"}), DefaultLimits, "kontrol"},
		"duplicate":      {writeTar(t, false, member{name: "a", body: "1"}, member{name: "./a", body: "2"}), DefaultLimits, "iki kez"},
		"too many":       {writeTar(t, false, member{name: "a", body: "1"}, member{name: "b", body: "2"}), Limits{MaxTotal: 1 << 20, MaxFile: 1 << 20, MaxFiles: 1, MaxRatio: 100}, "çok fazla"},
		"file too big":   {writeTar(t, false, member{name: "a", body: big}), Limits{MaxTotal: 1 << 20, MaxFile: 10, MaxFiles: 10, MaxRatio: 100}, "çok büyük"},
		"total too big":  {writeTar(t, false, member{name: "a", body: big}, member{name: "b", body: big}), Limits{MaxTotal: 150, MaxFile: 1000, MaxFiles: 10, MaxRatio: 100}, "çok büyük"},
		"zip symlink":    {writeFile(t, "s.zip", zipBytes(t, zmember{name: "l", body: "/etc/passwd", mode: fs.ModeSymlink | 0o777})), DefaultLimits, "bağlantı"},
		"zip slip":       {writeFile(t, "s.zip", zipBytes(t, zmember{name: "../x", body: "x"})), DefaultLimits, `".."`},
		"zip drive":      {writeFile(t, "s.zip", zipBytes(t, zmember{name: "C:/x", body: "x"})), DefaultLimits, "sürücü"},
		"unknown":        {writeFile(t, "x.zip", []byte("hello")), DefaultLimits, "tanınmayan"},
		"truncated gzip": {writeFile(t, "t.gz", gzipBytes(t, mongoHead)[:30]), DefaultLimits, "bozuk"},
	}
	for name, c := range cases {
		_, err := Check(c.path, c.l)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestCheckCorruptZip(t *testing.T) {
	b := zipBytes(t, zmember{name: "a.txt", body: strings.Repeat("hello world ", 200)})
	// Flip a byte inside the compressed data (after the 30-byte local header + name).
	b[40] ^= 0xff
	if _, err := Check(writeFile(t, "c.zip", b), DefaultLimits); err == nil {
		t.Error("corrupt zip accepted")
	}
}

func TestCheckTarGzChecksum(t *testing.T) {
	p := writeTar(t, true, member{name: "a.txt", body: strings.Repeat("hello ", 100)})
	b, _ := os.ReadFile(p)
	b[len(b)-6] ^= 0xff // inside the CRC32 trailer
	if _, err := Check(writeFile(t, "bad.tar.gz", b), DefaultLimits); err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Errorf("corrupt trailer accepted: %v", err)
	}
	// Data hidden after the tar end marker counts towards the size.
	var tail bytes.Buffer
	tw := tar.NewWriter(&tail)
	tw.WriteHeader(&tar.Header{Name: "a", Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	tail.Write(make([]byte, 4096))
	l := Limits{MaxTotal: 2048, MaxFile: 2048, MaxFiles: 10, MaxRatio: 100}
	if _, err := Check(writeFile(t, "pad.tar.gz", gzipBytes(t, tail.Bytes())), l); err == nil {
		t.Error("trailing data beyond the limit accepted")
	}
}

func TestZipEntryCountCheckedFirst(t *testing.T) {
	var ms []zmember
	for i := 0; i < 30; i++ {
		ms = append(ms, zmember{name: strings.Repeat("d", i+1)})
	}
	p := writeFile(t, "many.zip", zipBytes(t, ms...))
	entries, size, err := zipDirectory(p)
	if err != nil || entries != 30 || size == 0 {
		t.Fatalf("directory: %d %d %v", entries, size, err)
	}
	l := DefaultLimits
	l.MaxFiles = 10
	if _, err := Check(p, l); err == nil || !strings.Contains(err.Error(), "(30, sınır 10)") {
		t.Errorf("err = %v", err)
	}
	// A zip with a comment still has its end record found.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	w.SetComment(strings.Repeat("c", 1000))
	f, _ := w.Create("a")
	f.Write([]byte("x"))
	w.Close()
	if n, _, err := zipDirectory(writeFile(t, "c.zip", buf.Bytes())); err != nil || n != 1 {
		t.Errorf("with comment: %d %v", n, err)
	}
	if _, _, err := zipDirectory(writeFile(t, "x.zip", []byte("PK\x03\x04 truncated"))); err == nil {
		t.Error("truncated zip accepted")
	}
}

func TestCheckRejectsBomb(t *testing.T) {
	zeros := make([]byte, 80<<20) // 80 MB of zeros compress ~1000:1
	p := writeFile(t, "bomb.gz", gzipBytes(t, append(append([]byte{}, mongoHead...), zeros...)))
	if _, err := Check(p, DefaultLimits); err == nil || !strings.Contains(err.Error(), "bomba") {
		t.Errorf("bomb: %v", err)
	}
	// The same data under a smaller total limit stops while reading, before the end.
	if _, err := Check(p, Limits{MaxTotal: 1 << 20, MaxFile: 1 << 20, MaxFiles: 10, MaxRatio: 100}); err == nil || !strings.Contains(err.Error(), "aşıyor") {
		t.Errorf("limit: %v", err)
	}
}

func TestCleanName(t *testing.T) {
	ok := map[string]string{"a": "a", "./a/b/": "a/b", "././x": "x", "./": "", "çiçek/ğ.jpg": "çiçek/ğ.jpg", "1:2.txt": "1:2.txt"}
	for in, want := range ok {
		if got, err := CleanName(in); err != nil || got != want {
			t.Errorf("CleanName(%q) = %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/a", "a//b", "a/./b", "..", "a/..", "\x00", "a\x7f", "\xff", strings.Repeat("a", 256), "c:\\x", "D:/x"} {
		if _, err := CleanName(in); err == nil {
			t.Errorf("CleanName(%q) accepted", in)
		}
	}
}

// FuzzCleanName: whatever is accepted stays inside the target directory.
func FuzzCleanName(f *testing.F) {
	for _, s := range []string{"a", "./a/b/", "../x", "a/../../b", "/etc", "çiçek/ğ.jpg", `a\b`, "C:/x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got, err := CleanName(in)
		if err != nil {
			return
		}
		if got != "" && filepath.Clean(got) != got {
			t.Fatalf("%q -> %q is not clean", in, got)
		}
		if filepath.IsAbs(got) || got == ".." || strings.HasPrefix(got, "../") || strings.Contains(got, "\\") {
			t.Fatalf("%q -> %q escapes", in, got)
		}
	})
}
