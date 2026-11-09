package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// footopsArchive writes a FootOps archive with a database (the real mongodump fixture),
// files and an environment, as the panel's backup İndir does, and returns its path.
func footopsArchive(t *testing.T) (string, Manifest) {
	t.Helper()
	dump, err := os.ReadFile("testdata/magaza.archive")
	if err != nil {
		t.Fatal(err)
	}
	var files bytes.Buffer
	zw := gzip.NewWriter(&files)
	tw := tar.NewWriter(zw)
	for _, m := range []member{{name: "foto/", typ: tar.TypeDir}, {name: "foto/a.jpg", body: "jpeg"}, {name: "sayfa.html", body: "<p>merhaba</p>"}} {
		h := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		if m.typ != 0 {
			h.Typeflag, h.Mode, h.Size = m.typ, 0o755, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(m.body))
	}
	tw.Close()
	zw.Close()

	p := filepath.Join(t.TempDir(), "magaza-20261110-120000.footops.tar")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	mod := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	w := NewWriter(f)
	var reported int64
	if _, err := w.Add(MemberDB, mod, func(n int64) { reported = n }, func(out io.Writer) error {
		zw := gzip.NewWriter(out)
		zw.Write(dump)
		return zw.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if reported != 0 { // a small member is not reported (every megabyte)
		t.Errorf("reported %d", reported)
	}
	if _, err := w.Add(MemberFiles, mod, nil, func(out io.Writer) error {
		_, err := out.Write(files.Bytes())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(MemberEnv, mod, nil, func(out io.Writer) error {
		_, err := io.WriteString(out, "# magaza ortam değişkenleri\nAPI_KEY=\"gizli\"\nPARA=\"100$\"\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	man := Manifest{FootOps: "test", Site: "magaza", Created: mod, DB: "magaza", Collections: map[string]int64{"urunler": 3, "sayfalar": 2},
		Documents: 5, Files: &ManifestFiles{Files: 2, Dirs: 1, Bytes: 18},
		Backup: &ManifestBackup{Run: "RUN1", Time: mod, Snapshots: map[string]string{"db": "abc"}}}
	if err := w.Finish(man, mod); err != nil {
		t.Fatal(err)
	}
	return p, man
}

func readFootOpsFile(t *testing.T, p string) (Contents, error) {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return ReadFootOps(f)
}

func TestFootOpsRoundTrip(t *testing.T) {
	p, man := footopsArchive(t)
	c, err := readFootOpsFile(t, p)
	if err != nil {
		t.Fatal(err)
	}
	whole, _ := os.ReadFile(p)
	sum := sha256.Sum256(whole)
	if c.Size != int64(len(whole)) || c.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("whole: %d %s", c.Size, c.SHA256)
	}
	if c.Manifest.Format != FootOpsFormat || c.Manifest.Site != "magaza" || c.Manifest.Documents != man.Documents ||
		c.Manifest.Backup == nil || c.Manifest.Backup.Run != "RUN1" || c.Manifest.Files.Files != 2 {
		t.Errorf("manifest %+v", c.Manifest)
	}
	if len(c.Manifest.Members) != 3 {
		t.Errorf("the manifest lists %v", c.Manifest.Members)
	}
	if c.DB == nil || !contains(c.DB.Databases(), "magaza") || c.DBSize <= 0 {
		t.Errorf("db %+v %d", c.DB, c.DBSize)
	}
	if c.Files == nil || c.Files.Files != 2 || c.Files.Dirs != 1 || c.Files.Size != 18 {
		t.Errorf("files %+v", c.Files)
	}
	if !c.Env || c.EnvVars != 2 {
		t.Errorf("env %v %d", c.Env, c.EnvVars)
	}
	// Every member lies where it is said to, its checksum the manifest's: it can be read
	// in place.
	f, _ := os.Open(p)
	defer f.Close()
	for _, name := range []string{MemberDB, MemberFiles, MemberEnv, MemberManifest} {
		at, ok := c.Members[name]
		if !ok {
			t.Fatalf("%s missing", name)
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, at.Offset, at.Size)); err != nil {
			t.Fatal(err)
		}
		if hex.EncodeToString(h.Sum(nil)) != at.SHA256 {
			t.Errorf("%s: not at its offset", name)
		}
		if name != MemberManifest && c.Manifest.Members[name] != at.Member {
			t.Errorf("%s: %+v, manifest %+v", name, at.Member, c.Manifest.Members[name])
		}
	}
	// GNU tar's reader agrees (the headers are filled in afterwards).
	tr := tar.NewReader(bytes.NewReader(whole))
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "db.archive.gz,files.tar.gz,env,manifest.json" {
		t.Errorf("members %v", names)
	}
}

func TestWriterRefuses(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "x.footops.tar"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := NewWriter(f)
	none := func(io.Writer) error { return nil }
	if _, err := w.Add(MemberManifest, time.Now(), nil, none); err == nil {
		t.Error("the manifest is written by Finish alone")
	}
	if _, err := w.Add("../x", time.Now(), nil, none); err == nil {
		t.Error("an unknown member")
	}
	if _, err := w.Add(MemberEnv, time.Now(), nil, none); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add(MemberEnv, time.Now(), nil, none); err == nil {
		t.Error("a member twice")
	}
	if err := w.Finish(Manifest{}, time.Now()); err == nil {
		t.Error("an archive of an environment alone")
	}
	failing := errors.New("araç durdu")
	if _, err := w.Add(MemberDB, time.Now(), nil, func(io.Writer) error { return failing }); !errors.Is(err, failing) {
		t.Errorf("fill's error: %v", err)
	}
}

// rawFootOps writes a tar of the given members, the manifest made from them unless one
// is given, to check what a reader refuses.
func rawFootOps(t *testing.T, members []member, man *Manifest) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	auto := man == nil
	if auto {
		man = &Manifest{Format: FootOpsFormat, Members: map[string]Member{}}
	}
	for _, m := range members {
		if auto && m.name != MemberManifest {
			s := sha256.Sum256([]byte(m.body))
			man.Members[m.name] = Member{Size: int64(len(m.body)), SHA256: hex.EncodeToString(s[:])}
		}
		h := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		if m.typ != 0 {
			h.Typeflag, h.Size = m.typ, 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(m.body))
	}
	data, _ := json.Marshal(man)
	tw.WriteHeader(&tar.Header{Name: MemberManifest, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write(data)
	tw.Close()
	return writeFile(t, "x.footops.tar", buf.Bytes())
}

func TestReadFootOpsRefuses(t *testing.T) {
	db := string(gzipBytes(t, mustRead(t, "testdata/magaza.archive")))
	files := string(gzipBytes(t, tarBytes(t, member{name: "a.txt", body: "a"})))
	cases := map[string]struct {
		path string
		want string
	}{
		"a member changed after": {func() string {
			p := rawFootOps(t, []member{{name: MemberDB, body: db}}, nil)
			b, _ := os.ReadFile(p)
			i := bytes.Index(b, []byte(db))
			b2 := append([]byte(nil), b...)
			b2[i+len(db)-5] ^= 0xff // in the gzip trailer: the CRC no longer holds
			return writeFile(t, "y.footops.tar", b2)
		}(), "db.archive.gz"},
		"wrong format": {rawFootOps(t, []member{{name: MemberDB, body: db}},
			&Manifest{Format: 2, Members: map[string]Member{}}), "biçimi 2"},
		"a member not in the manifest": {rawFootOps(t, []member{{name: MemberDB, body: db}},
			&Manifest{Format: FootOpsFormat, Members: map[string]Member{}}), "manifest'te yok"},
		"a member the manifest names is missing": {rawFootOps(t, []member{{name: MemberDB, body: db}},
			&Manifest{Format: FootOpsFormat, Members: map[string]Member{MemberDB: sumOf(db), MemberFiles: {Size: 1}}}), "arşivde yok"},
		"a checksum that does not hold": {rawFootOps(t, []member{{name: MemberFiles, body: files}},
			&Manifest{Format: FootOpsFormat, Members: map[string]Member{MemberFiles: {Size: int64(len(files)), SHA256: strings.Repeat("0", 64)}}}), "tutmuyor"},
		"an unknown member": {rawFootOps(t, []member{{name: MemberDB, body: db}, {name: "x.sh", body: "rm"}}, nil), "beklenmeyen"},
		"a member twice":    {rawFootOps(t, []member{{name: MemberDB, body: db}, {name: MemberDB, body: db}}, nil), "iki kez"},
		"a link":            {rawFootOps(t, []member{{name: MemberDB, body: db}, {name: MemberFiles, typ: tar.TypeSymlink}}, nil), "yalnız dosya"},
		"no manifest": {writeFile(t, "z.tar", func() []byte {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			tw.WriteHeader(&tar.Header{Name: MemberDB, Mode: 0o644, Size: int64(len(db)), Typeflag: tar.TypeReg})
			tw.Write([]byte(db))
			tw.Close()
			return buf.Bytes()
		}()), "manifest.json yok"},
		"a database alone": {rawFootOps(t, []member{{name: MemberDB, body: db}}, nil), ""},
		"an escaping name": {rawFootOps(t, []member{{name: "../db.archive.gz", body: db}}, nil), "geçersiz"},
		"a broken env":     {rawFootOps(t, []member{{name: MemberDB, body: db}, {name: MemberEnv, body: "A=\"açık"}}, nil), "env okunamadı"},
		"files with an escaping name": {rawFootOps(t, []member{{name: MemberFiles,
			body: string(gzipBytes(t, tarBytes(t, member{name: "../x", body: "a"})))}}, nil), "files.tar.gz"},
	}
	for name, c := range cases {
		_, err := readFootOpsFile(t, c.path)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	// A name of the manifest's own is quoted, never printed raw (a terminal's escapes).
	_, err := readFootOpsFile(t, rawFootOps(t, []member{{name: MemberDB, body: db}},
		&Manifest{Format: FootOpsFormat, Members: map[string]Member{MemberDB: sumOf(db), "\x1b]0;x\a\x1b[31m": {Size: 1}}}))
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\a") {
		t.Errorf("%q", err)
	}
	// The manifest first, then an environment alone: nothing to import.
	env := "A=\"1\"\n"
	sum := sha256.Sum256([]byte(env))
	data, _ := json.Marshal(Manifest{Format: FootOpsFormat, Members: map[string]Member{MemberEnv: {Size: int64(len(env)), SHA256: hex.EncodeToString(sum[:])}}})
	p := writeFile(t, "e.footops.tar", tarBytes(t, member{name: MemberManifest, body: string(data)}, member{name: MemberEnv, body: env}))
	if _, err = readFootOpsFile(t, p); err == nil || !strings.Contains(err.Error(), "ne veritabanı ne dosya") {
		t.Errorf("an environment alone: %v", err)
	}
}

func TestReadFootOpsPlainTar(t *testing.T) {
	for _, first := range []string{"index.html", MemberEnv} {
		p := writeFile(t, "site.tar", tarBytes(t, member{name: first, body: "x"}, member{name: MemberManifest, body: "{}"}))
		if _, err := readFootOpsFile(t, p); !errors.Is(err, ErrNotFootOps) {
			t.Errorf("a tar whose first file is %s: %v", first, err)
		}
	}
}

func TestReadFootOpsSparse(t *testing.T) {
	// Go's writer leaves GNU.sparse records out: the extended header is made by hand.
	record := "GNU.sparse.name=db.archive.gz\n"
	for n := len(record) + 3; ; n++ {
		if r := fmt.Sprintf("%d %s", n, record); len(r) == n {
			record = r
			break
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "PaxHeaders/db", Mode: 0o644, Size: int64(len(record)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
	tw.Write([]byte(record))
	tw.WriteHeader(&tar.Header{Name: MemberDB, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg, Format: tar.FormatUSTAR})
	tw.Write([]byte("x"))
	tw.Close()
	b := buf.Bytes()
	b[156] = tar.TypeXHeader // the first header becomes the next one's extended header
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b[:512] {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	if _, err := readFootOpsFile(t, writeFile(t, "s.tar", b)); err == nil || !strings.Contains(err.Error(), "seyrek") {
		t.Errorf("sparse: %v", err)
	}
}

func sumOf(body string) Member {
	s := sha256.Sum256([]byte(body))
	return Member{Size: int64(len(body)), SHA256: hex.EncodeToString(s[:])}
}

func TestReadMongoPlainAndGzip(t *testing.T) {
	dump := mustRead(t, "testdata/magaza.archive")
	p, size, err := ReadMongo(bytes.NewReader(dump), false, sha256.New())
	if err != nil || size != int64(len(dump)) || !contains(p.Databases(), "magaza") {
		t.Errorf("plain: %v %d %v", err, size, p.Databases())
	}
	h := sha256.New()
	gz := gzipBytes(t, dump)
	p, size, err = ReadMongo(bytes.NewReader(gz), true, h)
	sum := sha256.Sum256(gz)
	if err != nil || size != int64(len(dump)) || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(sum[:]) {
		t.Errorf("gzip: %v %d", err, size)
	}
	if _, _, err := ReadMongo(bytes.NewReader([]byte("not a dump")), false, sha256.New()); err == nil {
		t.Error("not a dump")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tarBytes(t *testing.T, ms ...member) []byte {
	t.Helper()
	b, err := os.ReadFile(writeTar(t, false, ms...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
