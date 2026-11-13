package mac

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/mongo"
)

// Archives on the Mac: made-up sites, the archive package's real mongodump fixtures
// (magaza: one database with a view; two-dbs: a whole server's dump).

const (
	dumpFixture   = "../archive/testdata/magaza.archive"
	twoDBsFixture = "../archive/testdata/two-dbs.archive.gz"
)

var archiveTime = time.Date(2026, 11, 8, 13, 45, 0, 0, time.UTC)

// archiveEnv is footops-mac with its own Downloads, list and settings; the Mac's MongoDB
// is FOOTOPS_TEST_MONGO's, or one that is not there.
func archiveEnv(t *testing.T) (*env, *bytes.Buffer, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	var out, errb bytes.Buffer
	uri := os.Getenv("FOOTOPS_TEST_MONGO")
	if uri == "" {
		uri = "mongodb://127.0.0.1:1"
	}
	e := &env{out: &out, errw: &errb, settings: filepath.Join(dir, "ayarlar.json"),
		now:  func() time.Time { return time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC) },
		look: lookTool, downloads: filepath.Join(dir, "Downloads"), opened: filepath.Join(dir, "ayarlar", "acilanlar.json"),
		pairs: filepath.Join(dir, "ayarlar", "veritabanlari.json"),
		tmp:   filepath.Join(dir, "gecici"), mongoURI: uri, terminal: func() bool { return false }, free: freeRoom}
	if err := os.MkdirAll(e.tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeTree(dir) })
	return e, &out, &errb, dir
}

func runArchive(e *env, out, errb *bytes.Buffer, args ...string) int {
	out.Reset()
	errb.Reset()
	return e.run(context.Background(), append([]string{"arsiv"}, args...))
}

// footopsFixture writes a FootOps archive as the panel's backup İndir makes one: the
// magaza dump, files (a hidden one among them: an import leaves it out), an environment.
func footopsFixture(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	dump, err := os.ReadFile(dumpFixture)
	if err != nil {
		t.Fatal(err)
	}
	var files bytes.Buffer
	zw := gzip.NewWriter(&files)
	tw := tar.NewWriter(zw)
	for _, m := range []struct{ name, body string }{{"foto/", ""}, {"foto/a.jpg", "jpeg"}, {"sayfa.html", "<p>merhaba</p>"}, {".htaccess", "deny"}} {
		h := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(m.name, "/") {
			h.Typeflag, h.Mode = tar.TypeDir, 0o755
		}
		tw.WriteHeader(h)
		tw.Write([]byte(m.body))
	}
	tw.Close()
	zw.Close()
	p := filepath.Join(dir, "magaza-20261108-164500.footops.tar")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := archive.NewWriter(f)
	add := func(name string, body []byte) {
		if _, err := w.Add(name, archiveTime, nil, func(out io.Writer) error {
			_, err := out.Write(body)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var gz bytes.Buffer
	z := gzip.NewWriter(&gz)
	z.Write(dump)
	z.Close()
	add(archive.MemberDB, gz.Bytes())
	add(archive.MemberFiles, files.Bytes())
	add(archive.MemberEnv, []byte("# magaza ortam değişkenleri\nAPI_KEY=\"gizli$1\"\nNOTE=\"iki\\nsatır\"\n"))
	if err := w.Finish(archive.Manifest{FootOps: "test", Site: "magaza", Created: archiveTime, DB: "magaza",
		Collections: map[string]int64{"urunler": 3, "sayfalar": 2}, Documents: 5,
		Files:  &archive.ManifestFiles{Files: 3, Dirs: 1, Bytes: 22},
		Backup: &archive.ManifestBackup{Run: "RUN1", Kind: "daily", Time: archiveTime, Server: "h1", Snapshots: map[string]string{}}}, archiveTime); err != nil {
		t.Fatal(err)
	}
	return p, dump
}

func TestArchiveCheck(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	p, _ := footopsFixture(t, dir)
	if code := runArchive(e, out, errb, "dogrula", p, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	ev := events(t, out.String())
	var info ArchiveInfo
	last, _ := json.Marshal(ev[len(ev)-1])
	json.Unmarshal(last, &info)
	whole, _ := os.ReadFile(p)
	sum := sha256.Sum256(whole)
	if ev[len(ev)-1]["event"] != "result" || info.Kind != kindFootOps || info.Site != "magaza" || !info.Created.Equal(archiveTime) ||
		info.SHA256 != hex.EncodeToString(sum[:]) || info.Backup == nil || info.Backup.Server != "h1" || !info.Env || info.EnvVars != 2 {
		t.Fatalf("%+v", info)
	}
	d := info.Database
	if d == nil || d.Name != "magaza" || d.Documents != 5 || len(d.Collections) != 3 || d.Bytes <= 0 || d.Compressed <= 0 {
		t.Fatalf("database %+v", d)
	}
	for _, c := range d.Collections {
		want := map[string]int64{"sayfalar": 2, "urunler": 3, "ucuz": -1}[c.Name]
		if c.Documents != want || c.View != (c.Name == "ucuz") {
			t.Errorf("collection %+v", c)
		}
	}
	if f := info.Files; f == nil || f.Files != 3 || f.Hidden != 1 || f.Dirs != 1 {
		t.Errorf("files %+v", f)
	}
	if !strings.Contains(strings.Join(info.Notes, "\n"), "1 gizli dosya") || !strings.Contains(strings.Join(info.Notes, "\n"), "2 değişken") {
		t.Errorf("notes %v", info.Notes)
	}
	if ev[0]["event"] != "progress" || ev[0]["part"] != "check" {
		t.Errorf("first %v", ev[0])
	}
	// The Terminal's.
	if code := runArchive(e, out, errb, "dogrula", p); code != 0 || !strings.Contains(out.String(), "Veritabanı: magaza — 2 koleksiyon + 1 görünüm, 5 belge") ||
		!strings.Contains(out.String(), "Yedekten: H1, gece yedeği") || !strings.Contains(out.String(), "Dosyalar: 2 dosya") {
		t.Errorf("%d %s %s", code, out, errb)
	}

	// A plain dump, and a whole server's: no counts, several databases to choose from.
	if code := runArchive(e, out, errb, "dogrula", dumpFixture, "--json"); code != 0 || !strings.Contains(out.String(), `"kind":"mongodump"`) ||
		!strings.Contains(out.String(), `"documents":-1`) || !strings.Contains(out.String(), `"name":"magaza"`) {
		t.Errorf("dump: %d %s %s", code, out, errb)
	}
	if code := runArchive(e, out, errb, "dogrula", twoDBsFixture, "--json"); code != 0 || strings.Contains(out.String(), `"database":{"name"`) ||
		!strings.Contains(out.String(), "veritabanı var (blog, magaza)") || strings.Contains(out.String(), `"admin"`) {
		t.Errorf("two dbs: %d %s %s", code, out, errb)
	}

	// An archive of files: what the panel would take, a mongodump folder pointed out.
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, n := range []string{"uploads/a.jpg", "uploads/dump/magaza/urunler.bson", "uploads/.DS_Store"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	zp := filepath.Join(dir, "yukle.zip")
	os.WriteFile(zp, zb.Bytes(), 0o600)
	if code := runArchive(e, out, errb, "dogrula", zp, "--json"); code != 0 || !strings.Contains(out.String(), `"kind":"files"`) ||
		!strings.Contains(out.String(), `"prefix":"uploads"`) || !strings.Contains(out.String(), "mongodump klasörü olabilir") {
		t.Errorf("zip: %d %s %s", code, out, errb)
	}

	// Refused: a changed byte, a file of no known kind, none, a folder.
	b := append([]byte(nil), whole...)
	b[600] ^= 0xff // inside the database member
	bad := filepath.Join(dir, "bozuk.footops.tar")
	os.WriteFile(bad, b, 0o600)
	other := filepath.Join(dir, "not.json")
	os.WriteFile(other, []byte("{}"), 0o600)
	for _, c := range []struct{ path, want string }{{bad, "db.archive.gz"}, {other, "tanınmadı"},
		{filepath.Join(dir, "yok.tar"), "yok"}, {dir, "bir dosya değil"}} {
		if code := runArchive(e, out, errb, "dogrula", c.path, "--json"); code != 1 || !strings.Contains(out.String(), c.want) {
			t.Errorf("%s: %d %s", c.path, code, out)
		}
	}
}

func TestArchiveOpenFolder(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	p, dump := footopsFixture(t, dir)
	var excluded []string
	e.exclude = func(d string) { excluded = append(excluded, d) }
	if code := runArchive(e, out, errb, "ac", p, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	target := filepath.Join(e.downloads, "magaza-20261108-1345")
	ev := events(t, out.String())
	var starts, results int
	for _, x := range ev {
		switch x["event"] {
		case "start":
			starts++
			if x["target"] != target || x["archive"] != filepath.Base(p) || x["server"] != "h1" {
				t.Errorf("start %v", x)
			}
		case "result":
			results++
		}
	}
	if starts != 1 || results != 1 || ev[len(ev)-1]["event"] != "result" {
		t.Fatalf("%v", ev)
	}
	got, _ := os.ReadFile(filepath.Join(target, "db.archive"))
	envText, _ := os.ReadFile(filepath.Join(target, "env"))
	if !bytes.Equal(got, dump) || !strings.Contains(string(envText), `API_KEY="gizli$1"`) {
		t.Errorf("db %d bytes, env %q", len(got), envText)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "files", "foto", "a.jpg")); string(b) != "jpeg" {
		t.Errorf("files: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(target, "files", ".htaccess")); !os.IsNotExist(err) {
		t.Error("a hidden file was opened: an import never takes it")
	}
	var man archive.Manifest
	mb, _ := os.ReadFile(filepath.Join(target, "manifest.json"))
	if json.Unmarshal(mb, &man) != nil || man.Site != "magaza" || man.Members[archive.MemberDB].Size == 0 {
		t.Errorf("manifest %s", mb)
	}
	for _, c := range []struct {
		path string
		mode os.FileMode
	}{{target, 0o700}, {filepath.Join(target, "db.archive"), 0o600}, {filepath.Join(target, "env"), 0o600}, {filepath.Join(target, "manifest.json"), 0o600}} {
		if fi, err := os.Stat(c.path); err != nil || fi.Mode().Perm() != c.mode {
			t.Errorf("%s: %v %v", c.path, fi.Mode(), err)
		}
	}
	if len(excluded) != 1 || excluded[0] != target {
		t.Errorf("Time Machine: %v", excluded)
	}
	// On the list, from the archive, and deleted like a backup's folder.
	if code := runArchive(e, out, errb, "acilanlar", "--json"); code != 0 {
		t.Fatal(errb)
	}
	var v OpenedView
	json.Unmarshal(out.Bytes(), &v)
	if len(v.Folders) != 1 || v.Folders[0].Archive != filepath.Base(p) || v.Folders[0].Site != "magaza" || v.Folders[0].Partial {
		t.Fatalf("%+v", v)
	}
	if code := runArchive(e, out, errb, "sil", target); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("left: %v", err)
	}

	// Only some parts; one the archive has not; the same again beside it.
	if code := runArchive(e, out, errb, "ac", p, "--db"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	entries, _ := os.ReadDir(target)
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if strings.Join(names, ",") != ".footops-acilan,db.archive,manifest.json" {
		t.Errorf("--db: %v", names)
	}
	if code := runArchive(e, out, errb, "ac", p, "--dosyalar", "--ayarlar"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if _, err := os.Stat(filepath.Join(target+"-2", "env")); err != nil {
		t.Errorf("-2: %v", err)
	}
	dumpOnly := filepath.Join(dir, "yalniz.archive")
	os.WriteFile(dumpOnly, dump, 0o600)
	if code := runArchive(e, out, errb, "ac", dumpOnly); code != 1 || !strings.Contains(errb.String(), "yalnız FootOps arşivi") {
		t.Errorf("a dump into a folder: %d %s", code, errb)
	}
}

// Stopped half-way (the app closed its window), what was begun goes, its line too.
func TestArchiveOpenStopped(t *testing.T) {
	e, _, _, dir := archiveEnv(t)
	p, _ := footopsFixture(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out stopAt
	out.word, out.stop = `"part":"files"`, cancel
	e.out = &out
	if code := e.run(ctx, []string{"arsiv", "ac", p, "--json"}); code != 1 || !strings.Contains(out.String(), `"message":"durduruldu"`) {
		t.Fatalf("%d %s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(e.downloads, "magaza-20261108-1345")); !os.IsNotExist(err) {
		t.Errorf("left: %v", err)
	}
	data, _ := os.ReadFile(e.opened)
	if strings.Contains(string(data), "magaza-20261108-1345") {
		t.Errorf("its line left: %s", data)
	}
}

// stopAt calls stop once word is written.
type stopAt struct {
	mu   sync.Mutex
	b    bytes.Buffer
	word string
	stop func()
}

func (s *stopAt) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Contains(p, []byte(s.word)) {
		s.stop()
	}
	return s.b.Write(p)
}

func (s *stopAt) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestArchiveArgs(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	p, _ := footopsFixture(t, dir)
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"ac"}, 2, "Kullanım: footops-mac arsiv"},
		{[]string{"ac", p, "--ad", "x"}, 2, "yalnız --mongo ile"},
		{[]string{"ac", p, "--mongo", "--ayarlar"}, 2, "--db'yi de seç"},
		{[]string{"mongo", p, "--hedef", "x"}, 2, "bilinmeyen seçenek"},
		{[]string{"mongo", p, "--ad"}, 2, "bir değer ister"},
		{[]string{"olustur"}, 2, "--db ya da --dosyalar"},
		{[]string{"olustur", "--dosyalar", dir, "--site", "Kötü Ad"}, 1, "geçersiz site adı"},
		{[]string{"dogrula", p, "fazla"}, 2, "Kullanım"},
		{[]string{"bilinmeyen"}, 2, "bilinmeyen komut"},
	} {
		if code := runArchive(e, out, errb, c.args...); code != c.code || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: %d %s", c.args, code, errb)
		}
	}
	// A dump of several databases: which is asked, nothing touched.
	if code := runArchive(e, out, errb, "mongo", twoDBsFixture, "--json"); code != 1 || !strings.Contains(out.String(), `"code":"choose_db"`) ||
		!strings.Contains(out.String(), `"databases":["blog","magaza"]`) {
		t.Errorf("choose: %d %s", code, out)
	}
	if code := runArchive(e, out, errb, "mongo", twoDBsFixture, "--kaynak", "yok"); code != 1 || !strings.Contains(errb.String(), `"yok" veritabanı yok`) {
		t.Errorf("no such: %d %s", code, errb)
	}
}

func TestArchiveNames(t *testing.T) {
	at := time.Date(2026, 11, 8, 13, 45, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"magaza":                 "magaza_arsiv_20261108_1345",
		"yeni-site":              "yeni_site_arsiv_20261108_1345",
		"çok.garip ad":           "__ok_garip_ad_arsiv_20261108_1345",
		"":                       "arsiv_arsiv_20261108_1345",
		strings.Repeat("a", 80):  strings.Repeat("a", 63-len("_arsiv_20261108_1345")) + "_arsiv_20261108_1345",
		"___":                    "arsiv_arsiv_20261108_1345",
		"dukkan-baglanti-ikinci": "dukkan_baglanti_ikinci_arsiv_20261108_1345",
	} {
		got := archiveDBName(in, at)
		if got != want || checkDBName(got) != nil {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"dukkan":                 "dukkan",
		"Gül_Bahçesi":            "gul-bahcesi",
		"İstanbul Şube":          "istanbul-sube",
		"2026-yedek":             "yedek",
		"x":                      "site",
		"uploads":                "uploads",
		"ÇOK UZUN BİR AD ":       "cok-uzun-bir-ad",
		strings.Repeat("ab", 30): strings.Repeat("ab", 16),
	} {
		if got := siteFrom(in); got != want {
			t.Errorf("siteFrom(%q) = %q, want %q", in, got, want)
		}
	}
	if got := loadableDBs([]string{"admin", "magaza", "config", "a.b", "loc‮al", "local", "blog"}); strings.Join(got, ",") != "magaza,blog" {
		t.Errorf("%v", got)
	}
}

// Made of a folder alone (no MongoDB needed): what the panel's export packs, read back.
func TestArchiveCreateFiles(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	src := filepath.Join(dir, "Gül Bahçesi")
	os.MkdirAll(filepath.Join(src, "alt"), 0o755)
	os.WriteFile(filepath.Join(src, "a.jpg"), []byte("jpeg"), 0o600)
	os.WriteFile(filepath.Join(src, "alt", "gül.txt"), []byte("ayrışık"), 0o644) // NFD
	os.WriteFile(filepath.Join(src, ".DS_Store"), []byte("finder"), 0o644)
	os.Symlink("a.jpg", filepath.Join(src, "bag"))
	if code := runArchive(e, out, errb, "olustur", "--dosyalar", src, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	ev := events(t, out.String())
	var res CreateResult
	last, _ := json.Marshal(ev[len(ev)-1])
	json.Unmarshal(last, &res)
	want := filepath.Join(e.downloads, "gul-bahcesi-20261110-090000.footops.tar")
	if ev[0]["event"] != "start" || res.Path != want || res.Site != "gul-bahcesi" || res.Skipped != 2 || res.Archive == nil ||
		res.Archive.Files == nil || res.Archive.Files.Files != 2 || res.Archive.Database != nil {
		t.Fatalf("%+v %v", res, ev)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "1 dosya ya da klasör adında ayrışık") {
		t.Errorf("warnings %v", res.Warnings)
	}
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if _, err := os.Stat(want + ".part"); !os.IsNotExist(err) {
		t.Error(".part left")
	}
	f, _ := os.Open(want)
	c, err := archive.ReadFootOps(f)
	f.Close()
	if err != nil || c.Manifest.Site != "gul-bahcesi" || c.Manifest.Files.Files != 2 || c.Files.Hidden != 0 || c.DB != nil {
		t.Fatalf("%+v %v", c, err)
	}
	// What the panel's import puts into the site: these two, exactly.
	dst := filepath.Join(dir, "site")
	os.Mkdir(dst, 0o755)
	root, _ := os.OpenRoot(dst)
	defer root.Close()
	f, _ = os.Open(want)
	m := c.Members[archive.MemberFiles]
	st, err := datafs.ExtractTarGz(io.NewSectionReader(f, m.Offset, m.Size), root, datafs.Options{})
	f.Close()
	if err != nil || st.Files != 2 {
		t.Errorf("%+v %v", st, err)
	}
	// Not over what is there; a target asked for.
	asked := filepath.Join(dir, "benim.footops.tar")
	if code := runArchive(e, out, errb, "olustur", "--dosyalar", src, "--hedef", asked, "--site", "benim"); code != 0 ||
		!strings.Contains(out.String(), "✓ Arşiv hazır: "+asked) {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	if code := runArchive(e, out, errb, "olustur", "--dosyalar", src, "--hedef", asked); code != 1 || !strings.Contains(errb.String(), "var: başka bir --hedef") {
		t.Errorf("over: %d %s", code, errb)
	}
	if code := runArchive(e, out, errb, "olustur", "--dosyalar", filepath.Join(src, "a.jpg")); code != 1 || !strings.Contains(errb.String(), "bir klasör değil") {
		t.Errorf("a file: %d %s", code, errb)
	}
}

// Made of the Mac's MongoDB (FOOTOPS_TEST_MONGO: one for tests, or the Mac's own — the
// names here are the test's alone, dropped at its end), then loaded back: counted as the
// manifest says, listed, deleted.
func TestArchiveCreateMongo(t *testing.T) {
	uri := os.Getenv("FOOTOPS_TEST_MONGO")
	if uri == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok")
	}
	if _, err := lookTool("mongodump"); err != nil {
		t.Skip("mongodump yok")
	}
	if _, err := lookTool("mongorestore"); err != nil {
		t.Skip("mongorestore yok")
	}
	e, out, errb, dir := archiveEnv(t)
	mt, err := parseMongoURI(uri)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := newID()[:6]
	db, site := "arsivtest_"+id, "arsivtest-"+id
	t.Cleanup(func() {
		dbs, _ := mt.databases(ctx)
		for name := range dbs {
			if strings.HasPrefix(name, db) {
				mt.drop(ctx, name)
			}
		}
	})
	c, err := mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for coll, n := range map[string]int{"urunler": 7, "siparisler": 3} {
		b := mongo.NewBuilder().String("insert", coll).StartArray("documents")
		for i := range n {
			b.StartDoc("").Int32("n", int32(i)).String("ad", coll).End()
		}
		if _, err := c.Run(ctx, db, b.End().MustBuild(), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Run(ctx, db, mongo.NewBuilder().String("create", "ucuz").String("viewOn", "urunler").
		StartArray("pipeline").End().MustBuild(), 1<<20); err != nil {
		t.Fatal(err)
	}
	c.Close()
	src := filepath.Join(dir, "uploads")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "a.jpg"), []byte("jpeg"), 0o644)

	if code := runArchive(e, out, errb, "olustur", "--db", db, "--dosyalar", src, "--site", site, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	ev := events(t, out.String())
	var res CreateResult
	last, _ := json.Marshal(ev[len(ev)-1])
	json.Unmarshal(last, &res)
	d := res.Archive.Database
	if res.Site != site || d == nil || d.Name != db || d.Documents != 10 || len(d.Collections) != 3 || res.Archive.Files.Files != 1 {
		t.Fatalf("%+v %+v", res, d)
	}
	f, _ := os.Open(res.Path)
	ac, err := archive.ReadFootOps(f)
	f.Close()
	if err != nil || ac.Manifest.Collections["urunler"] != 7 || ac.Manifest.Collections["ucuz"] != 0 || len(ac.Manifest.Collections) != 2 ||
		!strings.HasPrefix(ac.Manifest.MongoTool, "mongodump ") {
		t.Fatalf("%+v %v", ac.Manifest, err)
	}

	// Loaded back: counted as the manifest says; the name taken, asked.
	name := db + "_arsiv_20261110_0900"
	if code := runArchive(e, out, errb, "mongo", res.Path, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	ev = events(t, out.String())
	loaded, _ := ev[len(ev)-1]["database"].(map[string]any)
	if loaded["name"] != name || loaded["documents"] != 10.0 || loaded["expected"] != 10.0 || ev[len(ev)-1]["warnings"] != nil {
		t.Fatalf("%v", ev[len(ev)-1])
	}
	if code := runArchive(e, out, errb, "mongo", res.Path, "--json"); code != 1 || !strings.Contains(out.String(), `"code":"db_exists"`) {
		t.Errorf("taken: %d %s", code, out)
	}
	other := db + "_arsiv_ikinci"
	if code := runArchive(e, out, errb, "ac", res.Path, "--mongo", "--ad", other); code != 0 || !strings.Contains(out.String(), "yüklendi: "+other+" (10 belge)") {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	if code := runArchive(e, out, errb, "veritabanlari", "--json"); code != 0 || !strings.Contains(out.String(), `{"name":"`+db+`","bytes":`) ||
		!strings.Contains(out.String(), `"name":"`+other+`","bytes":`) {
		t.Errorf("databases: %d %s", code, out)
	}
	if code := runArchive(e, out, errb, "acilanlar", "--json"); code != 0 {
		t.Fatal(errb)
	}
	var v OpenedView
	json.Unmarshal(out.Bytes(), &v)
	listed := map[string]string{}
	for _, x := range v.Databases {
		listed[x.Name] = x.Archive
	}
	if listed[name] != filepath.Base(res.Path) || listed[other] != filepath.Base(res.Path) || len(v.Folders) != 1 {
		t.Errorf("%+v", v)
	}
	for _, x := range []string{name, other} {
		if code := runArchive(e, out, errb, "sil", x); code != 0 || !strings.Contains(out.String(), "Silindi: MongoDB / "+x) {
			t.Errorf("%d %s %s", code, out, errb)
		}
	}
	// A database that is no one's opened one is not deleted — nor one the owner named
	// with "arsiv" in it; one of a default name whose line is gone is.
	if code := runArchive(e, out, errb, "sil", db); code != 1 || !strings.Contains(errb.String(), "yüklediklerinden değil") {
		t.Errorf("%d %s", code, errb)
	}
	own, lost := db+"_arsiv_kendi", db+"_arsiv_20200101_0000"
	c, err = mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{own, lost} {
		if _, err := c.Run(ctx, name, mongo.NewBuilder().String("insert", "x").StartArray("documents").StartDoc("").Int32("n", 1).End().End().MustBuild(), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	c.Close()
	if code := runArchive(e, out, errb, "acilanlar", "--json"); code != 0 {
		t.Fatal(errb)
	}
	var w OpenedView
	json.Unmarshal(out.Bytes(), &w)
	shown := map[string]bool{}
	for _, x := range w.Databases {
		shown[x.Name] = x.Registered
	}
	if _, ok := shown[own]; ok || shown[lost] {
		t.Errorf("listed: %v", shown)
	}
	if _, ok := shown[lost]; !ok {
		t.Errorf("a default name not listed: %v", shown)
	}
	if code := runArchive(e, out, errb, "veritabanlari", "--json"); code != 0 {
		t.Fatalf("databases: %d %s", code, errb)
	}
	var local LocalDatabases
	json.Unmarshal(out.Bytes(), &local)
	for _, x := range local.Databases {
		if x.Name == own && x.Opened || x.Name == lost && !x.Opened {
			t.Errorf("%+v", x)
		}
	}
	if code := runArchive(e, out, errb, "sil", own); code != 1 || !strings.Contains(errb.String(), "yüklediklerinden değil") {
		t.Errorf("own: %d %s", code, errb)
	}
	if code := runArchive(e, out, errb, "sil", lost); code != 0 || !strings.Contains(out.String(), "Silindi: MongoDB / "+lost) {
		t.Errorf("lost: %d %s %s", code, out, errb)
	}
	// One not there.
	if code := runArchive(e, out, errb, "olustur", "--db", db+"_yok"); code != 1 || !strings.Contains(errb.String(), "adlı veritabanı yok") {
		t.Errorf("%d %s", code, errb)
	}
}

// craftedDump is a mongodump archive's prelude naming n databases of one collection
// each — all the reader looks at.
func craftedDump(t *testing.T, dir string, n int) string {
	t.Helper()
	var b bytes.Buffer
	b.Write([]byte{0x6d, 0xe2, 0x99, 0x81})
	b.Write(mongo.NewBuilder().String("version", "0.1").String("server_version", "7.0.45").String("tool_version", "100.12").MustBuild())
	for i := range n {
		b.Write(mongo.NewBuilder().String("db", fmt.Sprintf("db%05d", i)).String("collection", "c").String("type", "collection").MustBuild())
	}
	b.Write([]byte{0xff, 0xff, 0xff, 0xff})
	p := filepath.Join(dir, "cok.archive")
	if err := os.WriteFile(p, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A dump naming thousands of databases (crafted) is read at once and listed in part:
// the window draws a hundred, the Terminal's --kaynak takes any of them.
func TestArchiveManyDatabases(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	p := craftedDump(t, dir, 20_000)
	start := time.Now()
	if code := runArchive(e, out, errb, "dogrula", p, "--json"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
	ev := events(t, out.String())
	var info ArchiveInfo
	b, _ := json.Marshal(ev[len(ev)-1])
	json.Unmarshal(b, &info)
	d := info.Database
	if d == nil || len(d.Databases) != maxDatabases || d.MoreDatabases != 20_000-maxDatabases || len(d.Collections) != maxListed ||
		d.More != 20_000-maxListed || len(out.String()) > 200<<10 {
		t.Fatalf("%d bytes: %+v", out.Len(), d)
	}
	if len(info.Notes) != 1 || !strings.Contains(info.Notes[0], "20000 veritabanı var (db00000, db00001") || !strings.Contains(info.Notes[0], "ve 19990 daha") {
		t.Errorf("%v", info.Notes)
	}
	// One past the hundred listed is still one of the dump's; none chosen: the hundred.
	e.free = func(string) (uint64, error) { return 1 << 40, nil }
	if code := runArchive(e, out, errb, "mongo", p, "--kaynak", "db19999"); code != 1 || strings.Contains(errb.String(), "veritabanı yok") {
		t.Errorf("--kaynak: %d %s", code, errb)
	}
	if code := runArchive(e, out, errb, "mongo", p, "--json"); code != 1 || !strings.Contains(out.String(), `"code":"choose_db"`) || len(out.String()) > 20<<10 {
		t.Errorf("choose: %d %d bytes", code, out.Len())
	}
}

// Loading asks for room first (the Mac's disk is nearly full): before the Mac's MongoDB
// is asked anything.
func TestArchiveMongoRoom(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	p, _ := footopsFixture(t, dir)
	e.free = func(string) (uint64, error) { return 1 << 20, nil }
	e.look = func(string) (string, error) { return "/bin/false", nil } // a mongorestore there (CI has none)
	if code := runArchive(e, out, errb, "mongo", p); code != 1 || !strings.Contains(errb.String(), "Mac'te yer yetmiyor") {
		t.Errorf("%d %s", code, errb)
	}
}

// A member read again is what was checked, or the open stops: a file replaced since the
// check is not loaded.
func TestArchiveRereadChanged(t *testing.T) {
	sum := sha256.Sum256([]byte("denetlenen"))
	r := &reread{r: strings.NewReader("değişmiş"), h: sha256.New(), want: hex.EncodeToString(sum[:])}
	if err := r.same(); err != errArchiveChanged {
		t.Errorf("%v", err)
	}
	r = &reread{r: strings.NewReader("denetlenen"), h: sha256.New(), want: hex.EncodeToString(sum[:])}
	io.CopyN(io.Discard, r, 3) // read in part: the rest is read by same
	if err := r.same(); err != nil {
		t.Errorf("%v", err)
	}
	// loadDB: mongorestore said it loaded, the bytes it was given were not the ones checked.
	dir := t.TempDir()
	tool := filepath.Join(dir, "mongorestore")
	os.WriteFile(tool, []byte("#!/bin/sh\nhead -c 2 > /dev/null\necho '2026-11-10T09:00:00.000+0300	3 document(s) restored successfully. 0 document(s) failed to restore.' >&2\n"), 0o700)
	src := filepath.Join(dir, "db.archive")
	os.WriteFile(src, []byte("değişmiş döküm"), 0o600)
	e := &env{look: func(string) (string, error) { return tool, nil }, tmp: dir}
	mt, _ := parseMongoURI("mongodb://127.0.0.1:1")
	if _, err := e.loadDB(context.Background(), mt, dumpSource{path: src, sha256: hex.EncodeToString(sum[:])}, "a", "a_arsiv_x", false, nil); err != errArchiveChanged {
		t.Errorf("%v", err)
	}
	whole := sha256.Sum256([]byte("değişmiş döküm"))
	if db, err := e.loadDB(context.Background(), mt, dumpSource{path: src, sha256: hex.EncodeToString(whole[:])}, "a", "a_arsiv_x", false, nil); err != nil || db.Documents != 3 {
		t.Errorf("unchanged: %v %+v", err, db)
	}
}

// Names a case-insensitive Mac disk calls one: the archive is fine (the panel takes it),
// opening its files on this Mac says why it cannot.
func TestArchiveCaseNames(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	probe := filepath.Join(dir, "Kucuk")
	os.WriteFile(probe, nil, 0o600)
	if _, err := os.Stat(filepath.Join(dir, "kucuk")); err != nil {
		t.Skip("bu disk büyük / küçük harfi ayırt ediyor")
	}
	files := gzipTar(t, map[string]string{"a.txt": "küçük", "A.txt": "büyük"})
	p := filepath.Join(dir, "harf.footops.tar")
	f, _ := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	w := archive.NewWriter(f)
	w.Add(archive.MemberFiles, archiveTime, nil, func(out io.Writer) error { _, err := out.Write(files); return err })
	w.Finish(archive.Manifest{Site: "harf", Created: archiveTime}, archiveTime)
	f.Close()
	if code := runArchive(e, out, errb, "dogrula", p); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if code := runArchive(e, out, errb, "ac", p); code != 1 || !strings.Contains(errb.String(), "büyük / küçük harfle") {
		t.Errorf("%d %s", code, errb)
	}
	if entries, _ := os.ReadDir(e.downloads); len(entries) != 0 {
		t.Errorf("left: %v", entries)
	}
}

func gzipTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return b.Bytes()
}

// The archive is never written into the folder packed: half of it would go into itself.
func TestArchiveCreateIntoItsFolder(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	src := filepath.Join(dir, "site")
	os.MkdirAll(filepath.Join(src, "alt"), 0o755)
	os.WriteFile(filepath.Join(src, "a.jpg"), []byte("jpeg"), 0o644)
	for _, target := range []string{src, filepath.Join(src, "alt"), filepath.Join(src, "alt", "x.footops.tar")} {
		if code := runArchive(e, out, errb, "olustur", "--dosyalar", src, "--hedef", target); code != 1 || !strings.Contains(errb.String(), "paketlenen klasörün içine") {
			t.Errorf("%s: %d %s", target, code, errb)
		}
	}
	// The same folder by another spelling, on a disk that takes both.
	other := filepath.Join(dir, "SITE")
	if _, err := os.Stat(other); err == nil {
		if code := runArchive(e, out, errb, "olustur", "--dosyalar", src, "--hedef", filepath.Join(other, "alt", "z.footops.tar")); code != 1 ||
			!strings.Contains(errb.String(), "paketlenen klasörün içine") {
			t.Errorf("by another spelling: %d %s", code, errb)
		}
	}
	e.downloads = filepath.Join(src, "Downloads") // the folder chosen is Downloads' parent
	if code := runArchive(e, out, errb, "olustur", "--dosyalar", src); code != 1 || !strings.Contains(errb.String(), "paketlenen klasörün içine") {
		t.Errorf("Downloads below: %d %s", code, errb)
	}
	if entries, _ := os.ReadDir(src); len(entries) != 3 { // a.jpg, alt, Downloads (made, empty)
		t.Errorf("%v", entries)
	}
}

// The names listed and deleted without a line on the list: footops-mac's defaults in full
// (the bash helper's by the day too), no owner's own with an everyday word in it.
func TestDefaultLoadNames(t *testing.T) {
	for name, want := range map[string]bool{
		"magaza_yedek_20261107_0300":   true,
		"magaza_arsiv_20261110_0136":   true,
		"yeni_site_arsiv_20261110_09":  false,
		"magaza_yedek_20261107":        true, // footops-yedek's
		"siparis_arsiv_2024":           false,
		"magaza_yedek_x":               false,
		"_yedek_20261107_0300":         false,
		"magaza_arsiv_20261110_0136_2": false,
	} {
		if got := reDefaultLoad.MatchString(name); got != want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// An archive's words reach the Terminal clean: a member name that turns text around or
// carries a C1 control (CleanName stops C0 only) in the error line, and escape sequences
// in the archive's own file name, are printed as question marks.
func TestArchiveWordsPrintedClean(t *testing.T) {
	e, out, errb, dir := archiveEnv(t)
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "rapor\u202etxt.exe\u009b2J", Typeflag: tar.TypeSymlink, Linkname: "hedef", Mode: 0o777, ModTime: archiveTime})
	tw.Close()
	p := filepath.Join(dir, "uye.tar")
	os.WriteFile(p, b.Bytes(), 0o644)
	if runArchive(e, out, errb, "dogrula", p) == 0 || bytes.Contains(errb.Bytes(), []byte("\u202e")) || bytes.Contains(errb.Bytes(), []byte("\u009b")) {
		t.Errorf("member name raw: %+q", errb.String())
	}
	b.Reset()
	tw = tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "sayfa.html", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3, ModTime: archiveTime})
	tw.Write([]byte("<p>"))
	tw.Close()
	p = filepath.Join(dir, "site\x1b]0;baska\x07.tar")
	os.WriteFile(p, b.Bytes(), 0o644)
	runArchive(e, out, errb, "dogrula", p)
	if bytes.ContainsAny(out.Bytes(), "\x1b\x07") || bytes.ContainsAny(errb.Bytes(), "\x1b\x07") {
		t.Errorf("file name raw: %q %q", out.String(), errb.String())
	}
}
