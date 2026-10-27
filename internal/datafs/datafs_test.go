package datafs

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/swbilisim/FootOps/internal/archive"
)

type entry struct {
	name string
	body string // "" with a trailing / in name: directory
	typ  byte
	link string
}

func tgz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o777, Size: int64(len(e.body)), Typeflag: tar.TypeReg, Linkname: e.link}
		switch {
		case e.typ != 0:
			h.Typeflag, h.Size = e.typ, 0
		case strings.HasSuffix(e.name, "/"):
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func root(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, dir
}

// tree lists a directory as name -> content ("/" for directories), with modes.
func tree(t *testing.T, dir string) (map[string]string, map[string]os.FileMode) {
	t.Helper()
	files, modes := map[string]string{}, map[string]os.FileMode{}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		modes[rel] = info.Mode().Perm()
		if info.IsDir() {
			files[rel] = "/"
		} else {
			b, _ := os.ReadFile(p)
			files[rel] = string(b)
		}
		return nil
	})
	return files, modes
}

func TestExtractTarGz(t *testing.T) {
	dst, dir := root(t)
	data := tgz(t,
		entry{name: "./"},
		entry{name: "ürünler/"},
		entry{name: "ürünler/çay.jpg", body: "jpeg"},
		entry{name: "ürünler/alt/şeker.png", body: "png"}, // parent created on the way
		entry{name: ".htaccess", body: "deny"},
		entry{name: "ürünler/.DS_Store", body: "x"},
		entry{name: ".git/config", body: "x"},
		entry{name: "__MACOSX/._çay.jpg", body: "x"},
		entry{name: "kök.txt", body: "merhaba"},
	)
	var calls int
	st, err := ExtractTarGz(bytes.NewReader(data), dst, Options{Progress: func(Stats) { calls++ }})
	if err != nil {
		t.Fatal(err)
	}
	files, modes := tree(t, dir)
	want := map[string]string{"ürünler": "/", "ürünler/çay.jpg": "jpeg", "ürünler/alt": "/", "ürünler/alt/şeker.png": "png", "kök.txt": "merhaba"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("tree = %v", files)
	}
	if modes["ürünler/çay.jpg"] != 0o644 || modes["ürünler"] != 0o755 || modes["ürünler/alt"] != 0o755 {
		t.Errorf("modes = %v", modes)
	}
	if st.Files != 3 || st.Dirs != 2 || st.Bytes != 14 || st.Skipped != 5 || calls == 0 {
		t.Errorf("stats = %+v, progress %d", st, calls)
	}
}

func TestExtractStrip(t *testing.T) {
	dst, dir := root(t)
	data := tgz(t, entry{name: "uploads/"}, entry{name: "uploads/a.jpg", body: "a"}, entry{name: "uploads/b/c.jpg", body: "c"},
		entry{name: "__MACOSX/uploads/._a.jpg", body: "x"})
	if _, err := ExtractTarGz(bytes.NewReader(data), dst, Options{Strip: "uploads"}); err != nil {
		t.Fatal(err)
	}
	if files, _ := tree(t, dir); !reflect.DeepEqual(files, map[string]string{"a.jpg": "a", "b": "/", "b/c.jpg": "c"}) {
		t.Errorf("tree = %v", files)
	}
	dst2, _ := root(t)
	if _, err := ExtractTarGz(bytes.NewReader(tgz(t, entry{name: "uploads/a", body: "a"}, entry{name: "other/b", body: "b"})), dst2, Options{Strip: "uploads"}); err == nil {
		t.Error("member outside the stripped folder accepted")
	}
}

func TestExtractRefuses(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]byte{
		"dotdot":    tgz(t, entry{name: "../kaçış", body: "x"}),
		"absolute":  tgz(t, entry{name: "/etc/kaçış", body: "x"}),
		"symlink":   tgz(t, entry{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}),
		"hardlink":  tgz(t, entry{name: "h", typ: tar.TypeLink, link: "x"}),
		"duplicate": tgz(t, entry{name: "a", body: "1"}, entry{name: "a", body: "2"}),
		"truncated": tgz(t, entry{name: "a", body: "1234567890"})[:30],
	}
	for name, data := range cases {
		dst, _ := root(t)
		if _, err := ExtractTarGz(bytes.NewReader(data), dst, Options{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A link the app planted in the directory cannot lead the extraction outside it.
	dst, dir := root(t)
	if err := os.Symlink(outside, filepath.Join(dir, "uploads")); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractTarGz(bytes.NewReader(tgz(t, entry{name: "uploads/x.jpg", body: "x"})), dst, Options{}); err == nil {
		t.Error("extraction followed a link out of the directory")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("written outside: %v", ents)
	}
	// Limits.
	dst, _ = root(t)
	l := archive.Limits{MaxTotal: 5, MaxFile: 5, MaxFiles: 10}
	if _, err := ExtractTarGz(bytes.NewReader(tgz(t, entry{name: "a", body: "123"}, entry{name: "b", body: "456"})), dst, Options{Limits: l}); err == nil {
		t.Error("total size limit ignored")
	}
}

func TestExtractZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{"foto/ç.jpg": "jpeg", "foto/": "", ".hidden": "x", "kök.txt": "k"} {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	zw.Close()
	dst, dir := root(t)
	st, err := ExtractZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), dst, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := tree(t, dir); !reflect.DeepEqual(files, map[string]string{"foto": "/", "foto/ç.jpg": "jpeg", "kök.txt": "k"}) || st.Files != 2 {
		t.Errorf("tree = %v, %+v", files, st)
	}
}

func TestPackRoundTrip(t *testing.T) {
	src, dir := root(t)
	os.MkdirAll(filepath.Join(dir, "ürün/alt"), 0o700)
	os.WriteFile(filepath.Join(dir, "ürün/çay.jpg"), []byte("jpeg"), 0o600)
	os.WriteFile(filepath.Join(dir, "ürün/alt/b.png"), bytes.Repeat([]byte("p"), 100000), 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(dir, "passwd"))
	var out bytes.Buffer
	st, err := Pack(src, &out, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Dirs != 2 || st.Skipped != 2 || st.Bytes != 100004 {
		t.Errorf("pack stats = %+v", st)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Error("hidden file packed")
	}
	if s, err := archive.Check(writeTemp(t, out.Bytes()), archive.DefaultLimits); err != nil || s.Kind != archive.TarGz || s.Files != 2 {
		t.Errorf("packed archive does not pass the check: %+v %v", s, err)
	}
	dst, ddir := root(t)
	if _, err := ExtractTarGz(bytes.NewReader(out.Bytes()), dst, Options{}); err != nil {
		t.Fatal(err)
	}
	files, modes := tree(t, ddir)
	if files["ürün/çay.jpg"] != "jpeg" || len(files["ürün/alt/b.png"]) != 100000 || len(files) != 4 || modes["ürün/alt/b.png"] != 0o644 {
		t.Errorf("round trip: %v %v", len(files), modes)
	}
	c, err := Count(src)
	if err != nil || c.Files != 2 || c.Bytes != 100004 || c.Skipped != 2 {
		t.Errorf("count = %+v %v", c, err)
	}
}

func writeTemp(t *testing.T, b []byte) string {
	p := filepath.Join(t.TempDir(), "a")
	os.WriteFile(p, b, 0o600)
	return p
}

func TestFill(t *testing.T) {
	cur, cdir := root(t)
	os.MkdirAll(filepath.Join(cdir, "eski/alt"), 0o755)
	os.WriteFile(filepath.Join(cdir, "eski/alt/a.jpg"), []byte("eski-a"), 0o644)
	os.WriteFile(filepath.Join(cdir, "ortak.jpg"), []byte("eski-ortak"), 0o644)
	os.WriteFile(filepath.Join(cdir, ".cache"), []byte("app"), 0o644)
	dst, ddir := root(t)
	data := tgz(t, entry{name: "ortak.jpg", body: "yeni-ortak"}, entry{name: "yeni.jpg", body: "yeni"})
	if _, err := ExtractTarGz(bytes.NewReader(data), dst, Options{}); err != nil {
		t.Fatal(err)
	}
	st, err := Fill(cur, dst)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := tree(t, ddir)
	want := map[string]string{"ortak.jpg": "yeni-ortak", "yeni.jpg": "yeni", "eski": "/", "eski/alt": "/", "eski/alt/a.jpg": "eski-a",
		".cache": "app"}
	if !reflect.DeepEqual(files, want) {
		t.Errorf("merged = %v", files)
	}
	// Kept: one regular file (eski/alt/a.jpg); the hidden one counts as skipped, as Count does.
	if st.Files != 1 || st.Skipped != 1 {
		t.Errorf("filled %+v", st)
	}
	if c, _ := Count(dst); c.Files != 3 {
		t.Errorf("result count %+v (archive 2 + kept 1)", c)
	}
	// Hard links: the same file, no copy.
	a1, _ := os.Stat(filepath.Join(cdir, "eski/alt/a.jpg"))
	a2, _ := os.Stat(filepath.Join(ddir, "eski/alt/a.jpg"))
	if a1.Sys().(*syscall.Stat_t).Ino != a2.Sys().(*syscall.Stat_t).Ino {
		t.Error("not linked")
	}
	// The current directory is untouched.
	if cf, _ := tree(t, cdir); cf["ortak.jpg"] != "eski-ortak" {
		t.Errorf("current changed: %v", cf)
	}
}

// A file in one and a folder of the same name in the other cannot be merged without
// losing one of them: refused, both ways.
func TestFillRefusesTypeConflicts(t *testing.T) {
	for name, setup := range map[string]func(cdir string) []byte{
		"site folder, archive file": func(cdir string) []byte {
			os.MkdirAll(filepath.Join(cdir, "foto"), 0o755)
			os.WriteFile(filepath.Join(cdir, "foto/a.jpg"), []byte("a"), 0o644)
			return tgz(t, entry{name: "foto", body: "dosya"})
		},
		"site file, archive folder": func(cdir string) []byte {
			os.WriteFile(filepath.Join(cdir, "foto"), []byte("dosya"), 0o644)
			return tgz(t, entry{name: "foto/a.jpg", body: "a"})
		},
	} {
		cur, cdir := root(t)
		dst, _ := root(t)
		if _, err := ExtractTarGz(bytes.NewReader(setup(cdir)), dst, Options{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Fill(cur, dst); err == nil || !strings.Contains(err.Error(), "birleştiremez") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Names an import would refuse are left out of an export, so the archive reads back.
func TestPackSkipsBadNames(t *testing.T) {
	src, dir := root(t)
	os.WriteFile(filepath.Join(dir, `C:\fakepath\x.jpg`), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "iyi.jpg"), []byte("iyi"), 0o644)
	var out bytes.Buffer
	st, err := Pack(src, &out, Options{})
	if err != nil || st.Files != 1 || st.Skipped != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := archive.Check(writeTemp(t, out.Bytes()), archive.DefaultLimits); err != nil {
		t.Errorf("packed archive refused: %v", err)
	}
}

func TestExtractZipChecksCRC(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "a.txt", Method: zip.Store})
	w.Write([]byte("merhaba dünya"))
	zw.Close()
	b := buf.Bytes()
	i := bytes.Index(b, []byte("merhaba"))
	b[i] ^= 1 // the stored content no longer matches its CRC
	dst, _ := root(t)
	if _, err := ExtractZip(bytes.NewReader(b), int64(len(b)), dst, Options{}); err == nil {
		t.Error("corrupted member extracted")
	}
}

func TestExtractTarGzTrailer(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "a", Size: 1, Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Write([]byte("a"))
	tw.Close()
	zw.Write(make([]byte, maxTrailer+10)) // a bomb would hide here
	zw.Close()
	dst, _ := root(t)
	if _, err := ExtractTarGz(bytes.NewReader(buf.Bytes()), dst, Options{}); err == nil {
		t.Error("data after the tar end accepted")
	}
}
