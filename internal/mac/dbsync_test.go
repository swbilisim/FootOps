package mac

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// veritabani …: pairing, and comparing with, pulling from and sending to a site, against
// a real restic repository made as the agent makes them and a real MongoDB
// (FOOTOPS_TEST_MONGO). Made-up values only.

func runDB(f *fixture, args ...string) int {
	f.out.Reset()
	f.errb.Reset()
	return f.e.run(context.Background(), append([]string{"veritabani"}, args...))
}

func TestDBArgs(t *testing.T) {
	for _, bad := range [][]string{
		{}, {"karsilastir"}, {"karsilastir", "a", "b", "c"}, {"gonder", "a", "2026-11-07 03:00"}, {"karsilastir", "a", "--sunucu", "t1"},
		{"karsilastir", "a", "--site", "magaza"}, {"karsilastir", "a", "--bilinmeyen"}, {"eslestir", "a"}, {"eslestir", "a", "t1"},
		{"eslestir", "a", "--kaldir", "b"}, {"liste", "x"}, {"yok"},
	} {
		e, _, errb := testEnv(t, &fakeR2{}, testNow)
		if code := e.run(context.Background(), append([]string{"veritabani"}, bad...)); code != 2 || !strings.Contains(errb.String(), "veritabani <komut>") {
			t.Errorf("%v: %d %s", bad, code, errb)
		}
	}
	for _, args := range [][]string{{"karsilastir", "a b"}, {"karsilastir", "admin"}, {"cek", "x", "--sunucu", "t1", "--site", "Kotu_Ad"},
		{"eslestir", "x", "t1", "A"}} {
		e, _, errb := testEnv(t, &fakeR2{}, testNow)
		if code := e.run(context.Background(), append([]string{"veritabani"}, args...)); code != 1 || errb.Len() == 0 {
			t.Errorf("%v: %d %s", args, code, errb)
		}
	}
	req, err := dbArgs([]string{"vt", "2026-11-07 03:00", "--sunucu=H1", "--site", "magaza"}, true)
	if err != nil || req != (dbReq{db: "vt", when: "2026-11-07 03:00", server: "h1", site: "magaza"}) {
		t.Errorf("%+v %v", req, err)
	}
}

// Pairing: written (0600), listed, replaced, removed; an unknown server or a bad site is
// refused, the file left as it was; a file edited by hand keeps its good lines.
func TestDBPairs(t *testing.T) {
	e, out, errb := testEnv(t, &fakeR2{}, testNow)
	e.mongoURI = "mongodb://127.0.0.1:1"
	run := func(args ...string) int {
		out.Reset()
		errb.Reset()
		return e.run(context.Background(), append([]string{"veritabani"}, args...))
	}
	if code := run("eslestir", "magaza_yerel", "h1", "magaza", "--json"); code != 0 ||
		!strings.Contains(out.String(), `"pair":{"server":"h1","server_name":"H1","site":"magaza"}`) {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	if fi, err := os.Stat(e.pairs); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	if code := run("eslestir", "magaza_yerel", "a", "magaza-2"); code != 0 || !strings.Contains(out.String(), "magaza_yerel → A / magaza-2") {
		t.Fatalf("%d %s %s", code, out, errb)
	}
	before, _ := os.ReadFile(e.pairs)
	if code := run("eslestir", "baska", "yok", "magaza"); code != 1 || !strings.Contains(errb.String(), "bilinmeyen sunucu") {
		t.Errorf("unknown server: %d %s", code, errb)
	}
	if after, _ := os.ReadFile(e.pairs); !bytes.Equal(before, after) {
		t.Error("refused, yet written")
	}
	pairs, err := e.readPairs()
	if err != nil || len(pairs) != 1 || pairs["magaza_yerel"] != (dbPair{Server: "a", Site: "magaza-2"}) {
		t.Fatalf("%v %v", pairs, err)
	}
	// The Mac's MongoDB not there: its error said, the pairings still read.
	if code := run("liste", "--json"); code != 0 || !strings.Contains(out.String(), `"mongo_error"`) {
		t.Errorf("%d %s", code, out)
	}
	if code := run("eslestir", "magaza_yerel", "--kaldir"); code != 0 {
		t.Fatal(errb)
	}
	if pairs, _ := e.readPairs(); len(pairs) != 0 {
		t.Errorf("not removed: %v", pairs)
	}
	// A line not to the rules is left out, the rest read; a file not understood is said.
	os.WriteFile(e.pairs, []byte(`{"format":1,"pairs":{"iyi":{"server":"h1","site":"magaza"},"kotu":{"server":"H 1","site":"x"},"a b":{"server":"h1","site":"magaza"}}}`), 0o600)
	if pairs, err := e.readPairs(); err != nil || len(pairs) != 1 || pairs["iyi"].Site != "magaza" {
		t.Errorf("%v %v", pairs, err)
	}
	os.WriteFile(e.pairs, []byte(`{bozuk`), 0o600)
	if code := run("eslestir", "x", "h1", "magaza"); code != 1 || !strings.Contains(errb.String(), "anlaşılamadı") {
		t.Errorf("%d %s", code, errb)
	}
	if data, _ := os.ReadFile(e.pairs); string(data) != `{bozuk` {
		t.Error("a file not understood was written over")
	}
	// Not paired: what to do is said.
	os.Remove(e.pairs)
	if code := run("karsilastir", "magaza_yerel"); code != 1 || !strings.Contains(errb.String(), "eşleşmedi: footops-mac veritabani eslestir magaza_yerel") {
		t.Errorf("%d %s", code, errb)
	}
}

// dbFixture is a repository holding a site's database as a server backs it up (a real
// dump), a broken one's (random bytes), and a database of the Mac's to compare with it.
type dbFixture struct {
	*fixture
	mt            mongoTarget
	site, broken  string
	local, prefix string
}

func newDBFixture(t *testing.T) *dbFixture {
	t.Helper()
	mt, prefix := testMongo(t)
	restic := findRestic(t)
	dump, err := lookTool("mongodump")
	if err != nil {
		t.Skip("mongodump yok")
	}
	ctx := context.Background()
	f := &dbFixture{mt: mt, prefix: prefix, site: "vtsite-" + prefix[7:], broken: "vtbozuk-" + prefix[7:], local: prefix + "_yerel"}
	t.Cleanup(func() {
		dbs, _ := mt.databases(ctx)
		for name := range dbs {
			if strings.HasPrefix(name, f.site) || strings.HasPrefix(name, strings.ReplaceAll(f.site, "-", "_")) {
				mt.drop(ctx, name)
			}
		}
	})
	c, err := mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The site's database as on its server: dumped as the agent does, then gone.
	insert(t, c, f.site, "urunler", `{"_id": 1, "ad": "çay", "updatedAt": {"$date": "2026-11-07T02:00:00Z"}}`, `{"_id": 2, "ad": "kahve"}`,
		`{"_id": 3, "ad": "su"}`)
	insert(t, c, f.site, "siparisler", `{"_id": "s1", "toplam": 10}`, `{"_id": "s2", "toplam": 20}`)
	archive, err := exec.Command(dump, "--uri="+os.Getenv("FOOTOPS_TEST_MONGO"), "--db="+f.site, "--archive", "--quiet").Output()
	if err != nil {
		t.Fatal(err)
	}
	mt.drop(ctx, f.site)
	// The Mac's own: kahve renamed, su gone, a product of its own, an order not there yet.
	insert(t, c, f.local, "urunler", `{"_id": 1, "ad": "çay", "updatedAt": {"$date": "2026-11-07T02:00:00Z"}}`, `{"_id": 2, "ad": "Kahve"}`,
		`{"_id": 4, "ad": "ayran", "createdAt": {"$date": "2026-11-08T12:00:00Z"}}`)
	insert(t, c, f.local, "siparisler", `{"_id": "s1", "toplam": 10}`, `{"_id": "s2", "toplam": 20}`)
	insert(t, c, f.local, "notlar", `{"_id": 1}`)

	bad := make([]byte, 4096)
	rand.Read(bad)
	f.fixture = &fixture{fixtureRepo: &fixtureRepo{restic: restic}, tmp: t.TempDir()}
	f.repo, f.root = filepath.Join(f.tmp, "repo"), filepath.Join(f.tmp, "footops")
	for _, b := range []struct {
		stdin []byte
		args  []string
	}{
		{nil, []string{"init"}},
		{archive, []string{"backup", "--host", "t1", "--tag", "footops,site=" + f.site + ",part=db,kind=daily,run=" + nightRun,
			"--time", "2026-11-07 03:00:05", "--stdin", "--stdin-filename", f.root + "/" + f.site + "/db.archive"}},
		{bad, []string{"backup", "--host", "t1", "--tag", "footops,site=" + f.broken + ",part=db,kind=daily,run=" + nightRun,
			"--time", "2026-11-07 03:00:06", "--stdin", "--stdin-filename", f.root + "/" + f.broken + "/db.archive"}},
	} {
		cmd := exec.Command(restic, b.args...)
		cmd.Env = append(os.Environ(), "RESTIC_REPOSITORY="+f.repo, "RESTIC_PASSWORD="+fixturePassword, "TZ=UTC",
			"RESTIC_CACHE_DIR="+filepath.Join(f.tmp, "cache-make"))
		if b.stdin != nil {
			cmd.Stdin = bytes.NewReader(b.stdin)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", b.args, err, out)
		}
	}
	f.e, f.out, f.errb = f.fixture.env(t)
	f.e.mongoURI = os.Getenv("FOOTOPS_TEST_MONGO")
	return f
}

// result is a command's last JSON line, read into v.
func result(t *testing.T, out string, v any) map[string]any {
	t.Helper()
	ev := events(t, out)
	last := ev[len(ev)-1]
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), v); err != nil {
		t.Fatal(err)
	}
	return last
}

// counts is a database's collections and their documents.
func (f *dbFixture) counts(t *testing.T, db string) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	c, err := f.mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	colls, err := listColls(ctx, c, db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for n := range colls {
		out[n], _ = countColl(ctx, c, db, n)
	}
	return out
}

func (f *dbFixture) databases(t *testing.T) map[string]int64 {
	t.Helper()
	dbs, err := f.mt.databases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return dbs
}

func (f *dbFixture) opened(t *testing.T) OpenedView {
	t.Helper()
	v, err := f.e.openedView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Compared with the site's backup: the counts, the newest records, the backup's time;
// the backup's temporary database gone after, its line too; through the pairing as well.
func TestDBCompare(t *testing.T) {
	f := newDBFixture(t)
	if code := runDB(f.fixture, "karsilastir", f.local, "--sunucu", "t1", "--site", f.site, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	var res DBSyncResult
	last := result(t, f.out.String(), &res)
	ev := events(t, f.out.String())
	if ev[0]["event"] != "start" || last["event"] != "result" || res.Diff == nil {
		t.Fatalf("%s", f.out)
	}
	parts := map[string]bool{}
	for _, e := range ev {
		if e["event"] == "progress" {
			parts[e["part"].(string)] = true
		}
	}
	if !parts["download"] || !parts["load"] || !parts["compare"] {
		t.Errorf("progress: %v", parts)
	}
	d := res.Diff
	if res.Server != "t1" || res.Site != f.site || res.Backup.Kind != "daily" || res.Backup.Time.Format("2006-01-02 15:04") != "2026-11-07 03:00" ||
		res.Temporary != strings.ReplaceAll(f.site, "-", "_")+"_yedek_20261107_0300" {
		t.Errorf("%+v", res)
	}
	if d.Identical || d.Same != 3 || d.Different != 1 || d.OnlyLocal != 2 || d.OnlyServer != 1 || d.Local.Documents != 6 || d.Server.Documents != 5 {
		t.Errorf("%+v", d)
	}
	if d.Local.Newest == nil || d.Local.Newest.Field != "createdAt" || d.Server.Newest == nil || d.Server.Newest.Field != "updatedAt" {
		t.Errorf("newest: %+v %+v", d.Local.Newest, d.Server.Newest)
	}
	if _, there := f.databases(t)[res.Temporary]; there {
		t.Error("temporary database left")
	}
	if v := f.opened(t); len(v.Databases) != 0 || len(v.Folders) != 0 {
		t.Errorf("left on the list: %+v", v)
	}
	if entries, _ := os.ReadDir(f.e.tmp); len(entries) != 0 {
		t.Errorf("temporary folder left: %v", entries)
	}
	// Paired: no --sunucu / --site; the Terminal's table.
	if code := runDB(f.fixture, "eslestir", f.local, "t1", f.site); code != 0 {
		t.Fatal(f.errb)
	}
	if code := runDB(f.fixture, "karsilastir", f.local); code != 0 || !strings.Contains(f.out.String(), "≠ Aynı değil: 1 farklı, 2 yalnız bu Mac'te, 1 yalnız sunucuda.") ||
		!strings.Contains(f.out.String(), "notlar") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	// The listing shows the pairing.
	if code := runDB(f.fixture, "liste", "--json"); code != 0 {
		t.Fatal(f.errb)
	}
	var l DBList
	json.Unmarshal(f.out.Bytes(), &l)
	found := false
	for _, x := range l.Databases {
		found = found || x.Name == f.local && x.Pair != nil && x.Pair.Site == f.site && x.Pair.ServerName == "T1"
	}
	if !found || l.MongoError != "" {
		t.Errorf("%+v", l)
	}
	// A site without a database backup; a database not on the Mac.
	if code := runDB(f.fixture, "karsilastir", f.local, "--sunucu", "t1", "--site", "yok-site"); code != 1 || !strings.Contains(f.errb.String(), "yedeği yok") {
		t.Errorf("%d %s", code, f.errb)
	}
	if code := runDB(f.fixture, "karsilastir", f.prefix+"_olmayan", "--sunucu", "t1", "--site", f.site); code != 1 || !strings.Contains(f.errb.String(), "yok") {
		t.Errorf("%d %s", code, f.errb)
	}
}

// Pulled: the backup in the database's place, a copy of what was there kept (on the list
// of what was opened, deletable); compared again, the same. Into a database not there:
// made, no copy.
func TestDBPull(t *testing.T) {
	f := newDBFixture(t)
	before := f.counts(t, f.local)
	if code := runDB(f.fixture, "cek", f.local, "--sunucu", "t1", "--site", f.site, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	var res DBSyncResult
	result(t, f.out.String(), &res)
	if res.Copy != f.local+"_yedek_20261109_0900" || res.Loaded == nil || res.Loaded.Documents != 5 || !res.Loaded.Replaced {
		t.Fatalf("%+v %s", res, f.out)
	}
	if got := f.counts(t, f.local); len(got) != 2 || got["urunler"] != 3 || got["siparisler"] != 2 {
		t.Errorf("pulled: %v", got)
	}
	if got := f.counts(t, res.Copy); len(got) != len(before) || got["urunler"] != before["urunler"] || got["notlar"] != 1 {
		t.Errorf("copy: %v, was %v", got, before)
	}
	v := f.opened(t)
	if len(v.Databases) != 1 || v.Databases[0].Name != res.Copy || v.Databases[0].Copy != f.local || !v.Databases[0].Registered {
		t.Errorf("%+v", v.Databases)
	}
	if code := runDB(f.fixture, "karsilastir", f.local, "--sunucu", "t1", "--site", f.site, "--json"); code != 0 {
		t.Fatal(f.errb)
	}
	var again DBSyncResult
	result(t, f.out.String(), &again)
	if !again.Diff.Identical {
		t.Errorf("not the same after the pull: %+v", again.Diff)
	}
	// The copy is footops-mac's to delete.
	if code := f.run(t, "sil", res.Copy); code != 0 {
		t.Errorf("%d %s", code, f.errb)
	}
	// Into a database not there: made, nothing copied.
	fresh := f.prefix + "_yeni"
	if code := runDB(f.fixture, "cek", fresh, "--sunucu", "t1", "--site", f.site); code != 0 || strings.Contains(f.out.String(), "kopya") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if got := f.counts(t, fresh); got["urunler"] != 3 {
		t.Errorf("%v", got)
	}
	for name := range f.databases(t) {
		if strings.Contains(name, fresh+"_yedek") {
			t.Errorf("a copy of nothing: %s", name)
		}
	}
}

// A pull that fails puts what was there back, and drops the copy it needs no more (a
// backup that does not load); stopped before the database is touched, nothing changes;
// stopped while the backup replaces it, it finishes — a half database looks whole.
func TestDBPullPutsBack(t *testing.T) {
	f := newDBFixture(t)
	before := f.counts(t, f.local)
	check := func(what string) {
		t.Helper()
		if got := f.counts(t, f.local); !sameCounts(got, before) {
			t.Errorf("%s: %v, was %v", what, got, before)
		}
		for name := range f.databases(t) {
			if strings.HasPrefix(name, f.local+"_yedek") {
				t.Errorf("%s: copy left: %s", what, name)
			}
		}
		if v := f.opened(t); len(v.Databases) != 0 {
			t.Errorf("%s: list: %+v", what, v.Databases)
		}
	}
	if code := runDB(f.fixture, "cek", f.local, "--sunucu", "t1", "--site", f.broken); code != 1 ||
		!strings.Contains(f.errb.String(), "mongorestore başarısız: stream or file does not appear to be a mongodump archive") ||
		!strings.Contains(f.errb.String(), "olduğu gibi kaldı") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	check("broken backup")

	stopAtWord := func(word string) (int, string) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var out stopAt
		out.word, out.stop = word, cancel
		f.e.out = &out
		defer func() { f.e.out = f.out }()
		code := f.e.run(ctx, []string{"veritabani", "cek", f.local, "--sunucu", "t1", "--site", f.site, "--json"})
		return code, out.String()
	}
	if code, out := stopAtWord(`"part":"copy"`); code != 1 || !strings.Contains(out, `"message":"durduruldu"`) {
		t.Fatalf("stopped while copying: %d %s %s", code, out, f.errb)
	}
	check("stopped while copying")

	code, out := stopAtWord(`"part":"load"`)
	if code != 0 || !strings.Contains(out, `"event":"result"`) {
		t.Fatalf("stopped while replacing: %d %s %s", code, out, f.errb)
	}
	if got := f.counts(t, f.local); got["urunler"] != 3 || got["notlar"] != 0 {
		t.Errorf("not the backup after all: %v", got)
	}
	if v := f.opened(t); len(v.Databases) != 1 || v.Databases[0].Copy != f.local {
		t.Errorf("the copy: %+v", v.Databases)
	}
}

// A database that was not there when the pull began, made meanwhile (an app using it), is
// not replaced: it is nobody's copy.
func TestDBPullAppeared(t *testing.T) {
	f := newDBFixture(t)
	fresh := f.prefix + "_sonradan"
	ctx := context.Background()
	var out stopAt
	out.word, out.stop = "yedek indiriliyor", func() {
		c, err := f.mt.dial(ctx)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		insert(t, c, fresh, "uygulama", `{"_id": 1}`)
	}
	f.e.out = &out
	if code := f.e.run(ctx, []string{"veritabani", "cek", fresh, "--sunucu", "t1", "--site", f.site}); code != 1 || !strings.Contains(f.errb.String(), "az önce oluştu") {
		t.Fatalf("%d %s %s", code, out.String(), f.errb)
	}
	if got := f.counts(t, fresh); len(got) != 1 || got["uygulama"] != 1 {
		t.Errorf("touched: %v", got)
	}
}

// The owner's own database whole in the copy and put back: a time series, stored
// functions (system.js), names with spaces and Turkish letters.
func TestDBPullOwnCollections(t *testing.T) {
	f := newDBFixture(t)
	ctx := context.Background()
	c, err := f.mt.dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command(t, c, f.local, `{"create": "olcum", "timeseries": {"timeField": "t"}}`)
	insert(t, c, f.local, "olcum", `{"t": {"$date": "2026-11-13T10:00:00Z"}, "v": 1}`, `{"t": {"$date": "2026-11-13T11:00:00Z"}, "v": 2}`)
	insert(t, c, f.local, "system.js", `{"_id": "topla", "value": {"$code": "function(a, b) { return a + b }"}}`)
	insert(t, c, f.local, "my coll", `{"_id": 1}`)
	insert(t, c, f.local, "ürünler", `{"_id": 1}`, `{"_id": 2}`)
	c.Close()
	before, err := countsOf(ctx, f.mt, f.local)
	if err != nil || before["system.js"] != 1 || before["olcum"] != 2 || before["my coll"] != 1 {
		t.Fatalf("%v %v", before, err)
	}
	// Put back after a backup that does not load: all of it.
	if code := runDB(f.fixture, "cek", f.local, "--sunucu", "t1", "--site", f.broken); code != 1 || !strings.Contains(f.errb.String(), "olduğu gibi kaldı") {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	if got, _ := countsOf(ctx, f.mt, f.local); !sameCounts(got, before) {
		t.Errorf("put back: %v, was %v", got, before)
	}
	// Pulled: the copy holds all of it.
	if code := runDB(f.fixture, "cek", f.local, "--sunucu", "t1", "--site", f.site, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	var res DBSyncResult
	result(t, f.out.String(), &res)
	if got, _ := countsOf(ctx, f.mt, res.Copy); !sameCounts(got, before) {
		t.Errorf("copy: %v, was %v", got, before)
	}
}

// One database, one footops-mac: a pull, a compare, a send or a delete of one in use is
// refused, saying so.
func TestDBBusy(t *testing.T) {
	f := newDBFixture(t)
	release, err := f.e.lockDB(f.local)
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"cek", "karsilastir", "gonder"} {
		if code := runDB(f.fixture, sub, f.local, "--sunucu", "t1", "--site", f.site, "--json"); code != 1 || !strings.Contains(f.out.String(), `"code":"db_busy"`) {
			t.Errorf("%s: %d %s %s", sub, code, f.out, f.errb)
		}
	}
	release()
	name := f.prefix + "_yedek_20261109_0900"
	held, err := f.e.lockDB(name)
	if err != nil {
		t.Fatal(err)
	}
	if code := f.run(t, "sil", name); code != 1 || !strings.Contains(f.errb.String(), "başka bir iş") {
		t.Errorf("delete in use: %d %s", code, f.errb)
	}
	held()
	if code := runDB(f.fixture, "karsilastir", f.local, "--sunucu", "t1", "--site", f.site); code != 0 {
		t.Errorf("released: %d %s", code, f.errb)
	}
}

// Sent: an archive of the database for the paired site, its panel's import page named.
func TestDBSend(t *testing.T) {
	f := newDBFixture(t)
	if err := os.WriteFile(f.e.settings, []byte(`{"format":1,"servers":{"t1":{"panel":"https://panel.example"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDB(f.fixture, "eslestir", f.local, "t1", f.site); code != 0 {
		t.Fatal(f.errb)
	}
	if code := runDB(f.fixture, "gonder", f.local, "--json"); code != 0 {
		t.Fatalf("%d %s %s", code, f.out, f.errb)
	}
	var res CreateResult
	result(t, f.out.String(), &res)
	if res.Import != "https://panel.example/sites/"+f.site+"/import" || res.Site != f.site || res.DB != f.local || res.Archive == nil ||
		res.Archive.Database == nil || res.Archive.Database.Documents != 6 {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatal(err)
	}
	if code := runDB(f.fixture, "gonder", f.local); code != 0 || !strings.Contains(f.out.String(), "t1: https://panel.example/sites/"+f.site+"/import") {
		t.Errorf("%d %s %s", code, f.out, f.errb)
	}
	// A server with no panel address: no page named.
	if code := runDB(f.fixture, "gonder", f.local, "--sunucu", "t2", "--site", f.site, "--json"); code != 0 {
		t.Fatal(f.errb)
	}
	var none CreateResult
	result(t, f.out.String(), &none)
	if none.Import != "" || none.Path == "" {
		t.Errorf("%+v", none)
	}
}
