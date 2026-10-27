package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
)

// fakeMongo plays MongoDB for the data tests. A database is collection -> document
// count (plus views); mongosh scripts are recognised by what they do; and the tool
// containers really read and write archives — a real mongodump prelude followed by the
// counts as JSON — so a restore loads exactly what was streamed into it.
type fakeMongo struct {
	mu    sync.Mutex
	dbs   map[string]map[string]int64
	views map[string][]string
	users map[string]bool

	restores    int
	failRestore int  // the n-th restore fails half-way (1-based; 0 never)
	lieNext     bool // the next restore writes one document less than it reports
	scripts     []string
	failDrop    bool        // dropping a temporary database fails
	onDump      func()      // called when a mongodump starts
	onRestore   func(n int) // called when the n-th mongorestore starts
}

func newFakeMongo() *fakeMongo {
	return &fakeMongo{dbs: map[string]map[string]int64{}, views: map[string][]string{}, users: map[string]bool{}}
}

func (m *fakeMongo) set(db string, counts map[string]int64, views ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dbs[db] = map[string]int64{}
	for k, v := range counts {
		m.dbs[db][k] = v
	}
	m.views[db] = append([]string(nil), views...)
}

func (m *fakeMongo) get(db string) map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for k, v := range m.dbs[db] {
		out[k] = v
	}
	return out
}

func bsonDoc(kv ...string) []byte {
	var body bytes.Buffer
	for i := 0; i+1 < len(kv); i += 2 {
		body.WriteByte(0x02)
		body.WriteString(kv[i])
		body.WriteByte(0)
		binary.Write(&body, binary.LittleEndian, int32(len(kv[i+1])+1))
		body.WriteString(kv[i+1])
		body.WriteByte(0)
	}
	out := make([]byte, 4, 5+body.Len())
	out = append(append(out, body.Bytes()...), 0)
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	return out
}

// fakeArchive is a mongodump archive as the fake tools understand it.
func fakeArchive(dbs map[string]map[string]int64, views map[string][]string) []byte {
	b := append([]byte{0x6d, 0xe2, 0x99, 0x81}, bsonDoc("version", "0.1", "server_version", "7.0.45", "tool_version", "100.10.0")...)
	var names []string
	for db := range dbs {
		names = append(names, db)
	}
	sort.Strings(names)
	for _, db := range names {
		for _, c := range sortedKeys(dbs[db]) {
			b = append(b, bsonDoc("db", db, "collection", c, "type", "collection")...)
		}
		for _, v := range views[db] {
			b = append(b, bsonDoc("db", db, "collection", v, "type", "view")...)
		}
	}
	b = append(b, 0xff, 0xff, 0xff, 0xff)
	js, _ := json.Marshal(dbs)
	return append(b, js...)
}

func gzipped(b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

var reGetDB = regexp.MustCompile(`const [dt]=c\.getDB\(("(?:[^"\\]|\\.)*")\)`)

func scriptDB(script string) string {
	m := reGetDB.FindStringSubmatch(script)
	if m == nil {
		return ""
	}
	var s string
	json.Unmarshal([]byte(m[1]), &s)
	return s
}

// Run answers docker commands other than mongosh (docker kill).
func (m *fakeMongo) Run(ctx context.Context, name string, args ...string) (string, error) {
	return "", nil
}

func (m *fakeMongo) RunInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scripts = append(m.scripts, stdin)
	db := scriptDB(stdin)
	switch {
	case strings.Contains(stdin, ".stats()"):
		db = regexp.MustCompile(`getDB\("([^"]+)"\)\.stats`).FindStringSubmatch(stdin)[1]
		var n int64
		for _, v := range m.dbs[db] {
			n += v
		}
		return fmt.Sprintf(`FOOTOPS_JSON {"dataSize":%d,"objects":%d,"collections":%d}`+"\nFOOTOPS_OK\n", n*100, n, len(m.dbs[db])), nil
	case strings.Contains(stdin, "getCollectionInfos"):
		counts := map[string]int64{}
		for k, v := range m.dbs[db] {
			if !strings.HasPrefix(k, "system.") { // like the real script
				counts[k] = v
			}
		}
		o := map[string]any{"counts": counts, "views": m.views[db]}
		js, _ := json.Marshal(o)
		return "FOOTOPS_JSON " + string(js) + "\nFOOTOPS_OK\n", nil
	case strings.Contains(stdin, "system.users"):
		n := 0
		for u := range m.users {
			if strings.HasPrefix(u, tmpDBPrefix) {
				delete(m.users, u)
				n++
			}
		}
		for d := range m.dbs {
			if strings.HasPrefix(d, tmpDBPrefix) {
				delete(m.dbs, d)
				n++
			}
		}
		return fmt.Sprintf("FOOTOPS_DROPPED %d\nFOOTOPS_OK\n", n), nil
	case strings.Contains(stdin, "createUser") && strings.Contains(stdin, tmpDBPrefix):
		m.users[regexp.MustCompile(`getDB\("([^"]+)"\)\.createUser`).FindStringSubmatch(stdin)[1]] = true
	case strings.Contains(stdin, "dropDatabase"):
		if m.failDrop {
			return "FOOTOPS_ERR connection closed\n", errors.New("exit status 3")
		}
		delete(m.users, db)
		delete(m.dbs, db)
		delete(m.views, db)
	case strings.Contains(stdin, "for(const n of"):
		var names []string
		json.Unmarshal([]byte(regexp.MustCompile(`of (\[.*?\])\)`).FindStringSubmatch(stdin)[1]), &names)
		for _, n := range names {
			delete(m.dbs[db], n)
			m.views[db] = remove(m.views[db], n)
		}
	}
	return "FOOTOPS_OK\n", nil
}

func remove(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

func argValue(args []string, prefix string) []string {
	var out []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			out = append(out, v)
		}
	}
	return out
}

func (m *fakeMongo) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	u := run.Usage{PeakMem: 40 << 20, CPU: 100 * time.Millisecond, Wall: time.Second}
	if err := ctx.Err(); err != nil {
		return run.Usage{}, err
	}
	switch {
	case contains(c.Args, "mongodump"):
		if m.onDump != nil {
			m.onDump()
		}
		db := argValue(c.Args, "--db=")[0]
		m.mu.Lock()
		dbs := map[string]map[string]int64{db: m.dbs[db]}
		views := map[string][]string{db: m.views[db]}
		m.mu.Unlock()
		for _, coll := range sortedKeys(dbs[db]) {
			c.Stderr(fmt.Sprintf("2026-10-27T11:00:00.000+0000\tdone dumping `%s.%s` (%d documents)", db, coll, dbs[db][coll]))
		}
		if _, err := c.Stdout.Write(gzipped(fakeArchive(dbs, views))); err != nil {
			return u, err
		}
		return u, nil
	case contains(c.Args, "mongorestore"):
		return u, m.restore(c)
	}
	return u, fmt.Errorf("unexpected tool: %v", c.Args)
}

func (m *fakeMongo) restore(c run.Cmd) error {
	var in io.Reader = c.Stdin
	if contains(c.Args, "--gzip") {
		zr, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		in = zr
	}
	p, err := archive.ReadMongoPrelude(in)
	if err != nil {
		return fmt.Errorf("mongorestore: %w", err)
	}
	var content map[string]map[string]int64
	if err := json.NewDecoder(in).Decode(&content); err != nil {
		return err
	}
	src := strings.TrimSuffix(argValue(c.Args, "--nsInclude=")[0], ".*")
	dst := src
	if to := argValue(c.Args, "--nsTo="); len(to) > 0 {
		dst = strings.TrimSuffix(to[0], ".*")
	}
	exclude := map[string]bool{}
	for _, x := range argValue(c.Args, "--nsExclude=") {
		exclude[strings.TrimPrefix(x, src+".")] = true
	}
	drop := contains(c.Args, "--drop")
	m.mu.Lock()
	m.restores++
	if m.onRestore != nil {
		m.mu.Unlock()
		m.onRestore(m.restores)
		m.mu.Lock()
	}
	defer m.mu.Unlock()
	if m.dbs[dst] == nil {
		m.dbs[dst] = map[string]int64{}
	}
	var total int64
	for i, ns := range p.Collections(src) {
		if exclude[ns.Collection] || (exclude["system.*"] && strings.HasPrefix(ns.Collection, "system.")) {
			continue
		}
		if m.restores == m.failRestore && i > 0 {
			return errors.New("exit status 1: connection lost")
		}
		if ns.Type == "view" {
			if drop {
				m.views[dst] = remove(m.views[dst], ns.Collection)
			}
			m.views[dst] = append(m.views[dst], ns.Collection)
			c.Stderr(fmt.Sprintf("x\tfinished restoring `%s.%s` (0 documents, 0 failures)", dst, ns.Collection))
			continue
		}
		n := content[src][ns.Collection]
		wrote := n
		if m.lieNext {
			wrote--
		}
		if drop {
			m.dbs[dst][ns.Collection] = wrote
		} else {
			m.dbs[dst][ns.Collection] += wrote
		}
		total += n
		c.Stderr(fmt.Sprintf("x\tfinished restoring `%s.%s` (%d documents, 0 failures)", dst, ns.Collection, n))
	}
	c.Stderr(fmt.Sprintf("x\t%d document(s) restored successfully. 0 document(s) failed to restore.", total))
	m.lieNext = false
	return nil
}

// fakeSites plays the site's container.
type fakeSites struct {
	mu       sync.Mutex
	running  bool
	started  int
	calls    []string
	health   func(started int) error
	startErr error
	stopErr  error
}

func (f *fakeSites) Running(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running, nil
}

func (f *fakeSites) Stop(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "stop")
	if f.stopErr != nil {
		return f.stopErr
	}
	f.running = false
	return nil
}

func (f *fakeSites) Start(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "start")
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	f.started++
	return nil
}

func (f *fakeSites) Health(_ context.Context, _ string, tries int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("health%d", tries))
	if f.health != nil {
		return f.health(f.started)
	}
	return nil
}

type f2Env struct {
	*opsEnv
	data  *DataOps
	mongo *fakeMongo
	sites *fakeSites
	clock time.Time
	n     int
}

func newF2(t *testing.T) *f2Env {
	t.Helper()
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("magaza"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	m := newFakeMongo()
	m.set("magaza", map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}, "ucuz")
	f := &f2Env{opsEnv: e, mongo: m, sites: &fakeSites{running: true}, clock: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)}
	var mu sync.Mutex
	f.data = &DataOps{Store: sites.Store{Root: e.root}, Docker: e.net, Run: m, Stream: m, Sites: f.sites,
		Staging: staging.New(filepath.Join(e.root, "staging"), 1<<30), SnapshotDir: filepath.Join(e.root, "snapshots"),
		RecoveryDir: filepath.Join(e.root, "recovery"), chunk: 1000,
		// Every reading moves the clock a second on: copies are named by the second.
		now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			f.clock = f.clock.Add(time.Second)
			return f.clock
		},
		checkFree: func(string, int64) error { return nil }}
	return f
}

func (f *f2Env) task() *Task {
	f.n++
	return &Task{j: NewJobs(), job: &Job{ID: fmt.Sprintf("Job%d", f.n)}}
}

func steps(t *Task) string { return strings.Join(t.job.Steps, "\n") }

// upload sends data in chunks (the last one first, to show order does not matter).
func (f *f2Env) upload(t *testing.T, name string, data []byte) string {
	t.Helper()
	st, err := f.data.CreateUpload("magaza", name, int64(len(data)), Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	for i := st.Chunks() - 1; i >= 0; i-- {
		part := data[int64(i)*st.Chunk : min(int64(len(data)), int64(i+1)*st.Chunk)]
		sum := sha256.Sum256(part)
		if _, err := f.data.PutChunk(st.ID, i, hex.EncodeToString(sum[:]), bytes.NewReader(part)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.data.FinishUpload(st.ID); err != nil {
		t.Fatal(err)
	}
	return st.ID
}

func TestUploadChunks(t *testing.T) {
	f := newF2(t)
	data := bytes.Repeat([]byte("0123456789"), 250) // 2500 bytes: chunks of 1000, 1000, 500
	st, err := f.data.CreateUpload("magaza", `C:\Users\x\dökümler\yedek.archive.gz`, int64(len(data)), Actor{User: "furkan"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "yedek.archive.gz" || st.Chunks() != 3 || !reflect.DeepEqual(st.Missing, []int{0, 1, 2}) {
		t.Fatalf("created = %+v", st)
	}
	info, _ := os.Stat(filepath.Join(f.data.uploadDir(st.ID), uploadData))
	if info.Size() != 2500 || info.Mode().Perm() != 0o600 {
		t.Errorf("data file %d %v", info.Size(), info.Mode())
	}
	put := func(n int, body []byte, sum []byte) error {
		s := sha256.Sum256(sum)
		_, err := f.data.PutChunk(st.ID, n, hex.EncodeToString(s[:]), bytes.NewReader(body))
		return err
	}
	if err := put(2, data[2000:], data[2000:]); err != nil {
		t.Fatal(err)
	}
	// A corrupted chunk is refused and stays missing.
	bad := append([]byte(nil), data[:1000]...)
	bad[5] ^= 1
	if err := put(0, bad, data[:1000]); !errors.Is(err, errBadChunk) {
		t.Fatalf("bad chunk: %v", err)
	}
	for name, err := range map[string]error{
		"short": put(1, data[1000:1500], data[1000:1500]),
		"long":  put(2, data[1999:], data[1999:]),
		"out of range": func() error {
			_, err := f.data.PutChunk(st.ID, 3, strings.Repeat("a", 64), bytes.NewReader(nil))
			return err
		}(),
		"no checksum": func() error {
			_, err := f.data.PutChunk(st.ID, 1, "", bytes.NewReader(data[1000:2000]))
			return err
		}(),
	} {
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.data.FinishUpload(st.ID); err == nil || !strings.Contains(err.Error(), "3 parça eksik") {
		t.Errorf("finish with missing chunks: %v", err)
	}
	// Resuming: the client asks what is missing. Chunk 2 had arrived, but the overlong
	// copy sent again overwrote it, so it is missing again.
	got, _ := f.data.GetUpload(st.ID)
	if !reflect.DeepEqual(got.Missing, []int{0, 1, 2}) {
		t.Errorf("missing = %v", got.Missing)
	}
	if err := put(2, data[2000:], data[2000:]); err != nil {
		t.Fatal(err)
	}
	if err := put(0, data[:1000], data[:1000]); err != nil {
		t.Fatal(err)
	}
	if err := put(1, data[1000:2000], data[1000:2000]); err != nil {
		t.Fatal(err)
	}
	done, err := f.data.FinishUpload(st.ID)
	if err != nil || !done.Finished || len(done.Missing) != 0 {
		t.Fatalf("finish: %+v %v", done, err)
	}
	if b := readFile(t, filepath.Join(f.data.uploadDir(st.ID), uploadData)); b != string(data) {
		t.Error("data differs")
	}
	if err := put(0, data[:1000], data[:1000]); !errors.Is(err, errUploadState) {
		t.Errorf("chunk after finish: %v", err)
	}
	if _, _, err := f.data.uploadFile("other", st.ID); err == nil {
		t.Error("upload served for another site")
	}

	// Limits and validation.
	for name, size := range map[string]int64{"empty": 0, "too big": MaxUploadSize + 1} {
		if _, err := f.data.CreateUpload("magaza", "x", size, Actor{}); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := f.data.CreateUpload("nope", "x", 10, Actor{}); !errors.Is(err, sites.ErrNotFound) {
		t.Errorf("unknown site: %v", err)
	}
	f.data.Staging = staging.New(f.data.Staging.Root, 100) // a full staging area
	if _, err := f.data.CreateUpload("magaza", "x", 1000, Actor{}); err == nil {
		t.Error("staging quota ignored")
	}
	for _, id := range []string{"../../etc", "short", strings.Repeat("A", 25)} {
		if _, err := f.data.GetUpload(id); !errors.Is(err, ErrNoUpload) {
			t.Errorf("id %q: %v", id, err)
		}
	}
	if err := f.data.RemoveUpload(st.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.data.uploadDir(st.ID)); !os.IsNotExist(err) {
		t.Error("upload not removed")
	}
}

func TestSingleUpload(t *testing.T) {
	f := newF2(t)
	arch := gzipped(fakeArchive(map[string]map[string]int64{"x": {"a": 1}}, nil))
	st, err := f.data.SingleUpload("magaza", "y.gz", bytes.NewReader(arch), Actor{User: "furkan"})
	if err != nil || !st.Finished || st.Kind != archive.MongoArchiveGz || st.Size != int64(len(arch)) {
		t.Fatalf("single = %+v %v", st, err)
	}
	if _, err := f.data.SingleUpload("magaza", "big", io.LimitReader(zeros{}, MaxSingleUpload+1), Actor{}); err == nil {
		t.Error("oversized single upload accepted")
	}
	ents, _ := os.ReadDir(f.data.Staging.Root)
	if len(ents) != 1 {
		t.Errorf("staging holds %d entries, want only the good upload", len(ents))
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestImportReplace(t *testing.T) {
	f := newF2(t)
	arch := gzipped(fakeArchive(map[string]map[string]int64{"eskisite": {"urunler": 10, "sayfalar": 4, "yeni": 1}},
		map[string][]string{"eskisite": {"ucuz"}}))
	id := f.upload(t, "eskisite.archive.gz", arch)

	ct := f.task()
	ic, err := f.data.CheckImport(ctx, ct, "magaza", id, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(ct))
	}
	want := map[string]int64{"urunler": 10, "sayfalar": 4, "yeni": 1}
	if ic.Format != FormatMongodump || ic.SourceDB != "eskisite" || ic.Target != "magaza" || !reflect.DeepEqual(ic.Archive, want) ||
		!reflect.DeepEqual(ic.Views, []string{"ucuz"}) || ic.ArchiveDocs != 15 || ic.CurrentDocs != 12 || ic.Section.Length != int64(len(arch)) {
		t.Fatalf("check = %+v", ic)
	}
	if f.mongo.get("magaza")["urunler"] != 3 {
		t.Error("the check touched the site's database")
	}
	for db := range f.mongo.dbs {
		if strings.HasPrefix(db, tmpDBPrefix) {
			t.Errorf("temporary database left: %s", db)
		}
	}
	if len(f.mongo.users) != 0 || len(f.sites.calls) != 0 {
		t.Errorf("users %v, site calls %v", f.mongo.users, f.sites.calls)
	}
	if loaded, err := f.data.LoadCheck("magaza", id); err != nil || !reflect.DeepEqual(loaded.Archive, want) {
		t.Errorf("stored check: %+v %v", loaded, err)
	}
	for _, s := range f.mongo.scripts {
		if strings.Contains(s, "createUser") && !strings.Contains(s, `role:"readWrite",db:"`+tmpDBPrefix) {
			t.Errorf("temporary user not limited to its database: %s", s)
		}
	}

	at := f.task()
	snap, err := f.data.ApplyImport(ctx, at, "magaza", id, ModeReplace, Actor{User: "furkan"})
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(at))
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, want) {
		t.Errorf("after replace: %v (eski must be gone)", got)
	}
	if !reflect.DeepEqual(f.sites.calls, []string{"health1", "stop", "start", fmt.Sprintf("health%d", healthTries)}) || !f.sites.running {
		t.Errorf("site calls %v", f.sites.calls)
	}
	if snap.Reason != "before-import" || snap.Collections["eski"] != 7 || at.job.Result != snap.ID {
		t.Errorf("safety copy %+v, result %q", snap, at.job.Result)
	}
	if _, err := os.Stat(f.data.uploadDir(id)); !os.IsNotExist(err) {
		t.Error("applied upload kept")
	}
	if ents, _ := os.ReadDir(f.data.RecoveryDir); len(ents) != 0 {
		t.Error("recovery record left")
	}
	if ents, _ := os.ReadDir(f.data.Staging.Root); len(ents) != 0 {
		t.Errorf("staging not cleaned: %v", ents)
	}

	// Undo: the safety copy brings the old database back, views included.
	rt := f.task()
	if _, err := f.data.RestoreSnapshot(ctx, rt, "magaza", snap.ID, Actor{User: "furkan"}); err != nil {
		t.Fatalf("%v\n%s", err, steps(rt))
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}) ||
		!reflect.DeepEqual(f.mongo.views["magaza"], []string{"ucuz"}) {
		t.Errorf("after undo: %v %v", got, f.mongo.views["magaza"])
	}
	if !strings.Contains(steps(rt), "Bu işlemi geri almak için") {
		t.Errorf("undo steps:\n%s", steps(rt))
	}
}

func TestImportAddOnly(t *testing.T) {
	f := newF2(t)
	id := f.upload(t, "a.archive.gz", gzipped(fakeArchive(map[string]map[string]int64{"magaza": {"urunler": 99, "yorumlar": 5}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	at := f.task()
	if _, err := f.data.ApplyImport(ctx, at, "magaza", id, ModeAdd, Actor{}); err != nil {
		t.Fatalf("%v\n%s", err, steps(at))
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7, "yorumlar": 5}) {
		t.Errorf("add mode: %v", got)
	}
	if !strings.Contains(steps(at), "dokunulmuyor: urunler") {
		t.Errorf("steps:\n%s", steps(at))
	}
}

func TestImportRollsBack(t *testing.T) {
	old := map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}
	cases := map[string]func(f *f2Env){
		"restore fails half-way": func(f *f2Env) { f.mongo.failRestore = 2 }, // the trial is restore 1
		"counts differ":          func(f *f2Env) { f.mongo.lieNext = true },
		"site does not start":    func(f *f2Env) { f.sites.startErr = errors.New("container çalışmıyor") },
		"site answers 500": func(f *f2Env) {
			f.sites.health = func(started int) error {
				if started == 1 {
					return errors.New("HTTP 500")
				}
				return nil
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newF2(t)
			id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10, "yeni": 1}}, nil)))
			if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
				t.Fatal(err)
			}
			setup(f)
			at := f.task()
			_, err := f.data.ApplyImport(ctx, at, "magaza", id, ModeReplace, Actor{})
			if err == nil || !strings.Contains(err.Error(), "geri alındı") {
				if name == "site does not start" && err != nil && strings.Contains(err.Error(), "başlatılamadı") {
					// the rollback restored the data but the site still cannot start
				} else {
					t.Fatalf("err = %v\n%s", err, steps(at))
				}
			}
			if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, old) {
				t.Errorf("not rolled back: %v\n%s", got, steps(at))
			}
			if _, err := os.Stat(f.data.uploadDir(id)); err != nil {
				t.Error("upload removed after a failed import (the operator may retry)")
			}
			if ents, _ := os.ReadDir(f.data.RecoveryDir); len(ents) != 0 {
				t.Error("recovery record left")
			}
		})
	}
}

func TestImportUnhealthyBeforeIsNoReason(t *testing.T) {
	f := newF2(t)
	f.sites.health = func(int) error { return errors.New("HTTP 502") }
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	at := f.task()
	if _, err := f.data.ApplyImport(ctx, at, "magaza", id, ModeReplace, Actor{}); err != nil {
		t.Fatalf("%v\n%s", err, steps(at))
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 10}) {
		t.Errorf("import undone: %v", got)
	}
	if strings.Count(strings.Join(f.sites.calls, " "), "health") != 1 {
		t.Errorf("calls %v: only the baseline check should run", f.sites.calls)
	}
}

func TestImportStoppedSiteStaysStopped(t *testing.T) {
	f := newF2(t)
	f.sites.running = false
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.data.ApplyImport(ctx, f.task(), "magaza", id, ModeReplace, Actor{}); err != nil {
		t.Fatal(err)
	}
	if len(f.sites.calls) != 0 || f.sites.running {
		t.Errorf("calls %v", f.sites.calls)
	}
}

func TestImportChoosesDatabase(t *testing.T) {
	f := newF2(t)
	id := f.upload(t, "all.gz", gzipped(fakeArchive(map[string]map[string]int64{
		"admin": {"system.version": 1}, "blog": {"yazilar": 4}, "magaza": {"urunler": 8}}, nil)))
	ic, err := f.data.CheckImport(ctx, f.task(), "magaza", id, "")
	if err != nil || ic.SourceDB != "" || !reflect.DeepEqual(ic.Databases, []string{"blog", "magaza"}) || ic.Archive != nil {
		t.Fatalf("check = %+v %v", ic, err)
	}
	if _, err := f.data.ApplyImport(ctx, f.task(), "magaza", id, ModeReplace, Actor{}); err == nil {
		t.Error("applied without a chosen database")
	}
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, "admin"); err == nil {
		t.Error("admin accepted")
	}
	ic, err = f.data.CheckImport(ctx, f.task(), "magaza", id, "blog")
	if err != nil || ic.SourceDB != "blog" || !reflect.DeepEqual(ic.Archive, map[string]int64{"yazilar": 4}) {
		t.Fatalf("check blog = %+v %v", ic, err)
	}
}

func TestImportRefusesOtherFiles(t *testing.T) {
	f := newF2(t)
	var zipped bytes.Buffer
	zipped.WriteString("PK\x03\x04")
	zipped.Write(make([]byte, 100))
	var plainTar bytes.Buffer
	tw := tar.NewWriter(&plainTar)
	tw.WriteHeader(&tar.Header{Name: "dump/magaza/urunler.bson", Size: 3, Mode: 0o644})
	tw.Write([]byte("abc"))
	tw.Close()
	trunc := gzipped(fakeArchive(map[string]map[string]int64{"x": {"a": 1}}, nil))
	for name, data := range map[string][]byte{
		"zip":          zipped.Bytes(),
		"text":         []byte("merhaba"),
		"plain tar":    plainTar.Bytes(),
		"truncated gz": trunc[:len(trunc)-5],
		"only system":  gzipped(fakeArchive(map[string]map[string]int64{"admin": {"x": 1}, "local": {"y": 1}}, nil)),
	} {
		id := f.upload(t, name, data)
		ct := f.task()
		if _, err := f.data.CheckImport(ctx, ct, "magaza", id, ""); err == nil {
			t.Errorf("%s accepted:\n%s", name, steps(ct))
		}
	}
	if f.mongo.restores != 0 {
		t.Errorf("%d restores ran for refused files", f.mongo.restores)
	}
}

func TestExportRoundTrip(t *testing.T) {
	f := newF2(t)
	et := f.task()
	info, err := f.data.Export(ctx, et, "magaza")
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(et))
	}
	list, _ := f.data.Exports("magaza")
	got, path, err := f.data.ExportFile("magaza", info.ID)
	if err != nil || len(list) != 1 || got.SHA256 != info.SHA256 {
		t.Fatalf("export listing: %+v %v", list, err)
	}
	b, _ := os.ReadFile(path)
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != info.SHA256 || int64(len(b)) != info.Bytes {
		t.Error("export checksum")
	}
	// A standard tar reader sees the dump, then the manifest.
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	var man ExportManifest
	var dump []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		body, _ := io.ReadAll(tr)
		if h.Name == memberManifest {
			json.Unmarshal(body, &man)
		} else {
			dump = body
		}
	}
	if sum := sha256.Sum256(dump); man.Members[memberDB].SHA256 != hex.EncodeToString(sum[:]) || man.Members[memberDB].Size != int64(len(dump)) {
		t.Errorf("manifest member %+v", man.Members)
	}
	if !reflect.DeepEqual(names, []string{memberDB, memberManifest}) || man.Format != 1 || man.Site != "magaza" ||
		!reflect.DeepEqual(man.Collections, map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}) || man.Documents != 12 {
		t.Fatalf("archive: %v %+v", names, man)
	}

	// Into another state, back from the export: the database member is read in place.
	f.mongo.set("magaza", map[string]int64{"bambaska": 1})
	id := f.upload(t, info.File, b)
	ct := f.task()
	ic, err := f.data.CheckImport(ctx, ct, "magaza", id, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, steps(ct))
	}
	if ic.Format != FormatFootOps || ic.Manifest == nil || ic.Section.Offset != 512 || ic.SHA256 != info.SHA256 {
		t.Fatalf("check = %+v", ic)
	}
	if _, err := f.data.ApplyImport(ctx, f.task(), "magaza", id, ModeReplace, Actor{}); err != nil {
		t.Fatal(err)
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}) {
		t.Errorf("round trip: %v", got)
	}

	// A flipped byte in the dump breaks the manifest's checksum.
	bad := append([]byte(nil), b...)
	bad[600] ^= 0xff
	id = f.upload(t, "bad.footops.tar", bad)
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err == nil {
		t.Error("corrupted FootOps archive accepted")
	}
}

func TestRecoverAfterAgentStop(t *testing.T) {
	for _, phase := range []string{phaseRestoring, phaseVerified} {
		t.Run(phase, func(t *testing.T) {
			f := newF2(t)
			snap, err := f.data.Snapshot(ctx, f.task(), "magaza", "before-import", Actor{})
			if err != nil {
				t.Fatal(err)
			}
			// The agent stopped mid-restore: half-new data, site stopped.
			f.mongo.set("magaza", map[string]int64{"urunler": 1})
			f.sites.running = false
			r := recovery{Job: "Dead1", Site: "magaza", Snapshot: snap.ID, Running: true, Phase: phase, Started: f.clock}
			if err := f.data.saveRecovery(r); err != nil {
				t.Fatal(err)
			}
			recs := f.data.Recoveries()
			if len(recs) != 1 || recs[0] != r {
				t.Fatalf("recoveries = %+v", recs)
			}
			rt := f.task()
			if err := f.data.Recover(ctx, rt, recs[0]); err != nil {
				t.Fatalf("%v\n%s", err, steps(rt))
			}
			want := map[string]int64{"urunler": 3, "sayfalar": 2, "eski": 7}
			if phase == phaseVerified {
				want = map[string]int64{"urunler": 1} // complete new data is kept
			}
			if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, want) || !f.sites.running {
				t.Errorf("after recovery: %v running=%v\n%s", got, f.sites.running, steps(rt))
			}
			if len(f.data.Recoveries()) != 0 {
				t.Error("record kept")
			}
		})
	}
}

func TestDropTempDatabases(t *testing.T) {
	f := newF2(t)
	f.mongo.set(tmpDBPrefix+"job1", map[string]int64{"a": 1})
	f.mongo.users[tmpDBPrefix+"job1"] = true
	n, err := f.data.DropTempDatabases(ctx)
	if err != nil || n != "2" || len(f.mongo.get(tmpDBPrefix+"job1")) != 0 || f.mongo.get("magaza")["urunler"] != 3 {
		t.Errorf("dropped %q %v; dbs %v", n, err, f.mongo.dbs)
	}
}

func TestNsEscape(t *testing.T) {
	if got := nsEscape(`a*b\c`); got != `a\*b\\c` {
		t.Errorf("nsEscape = %q", got)
	}
	if got := importable([]string{"admin", "config", "local", "footops_tmp_x", "a.b", "ok-db", "magaza"}); !reflect.DeepEqual(got, []string{"ok-db", "magaza"}) {
		t.Errorf("importable = %v", got)
	}
}

func TestDataEndpoints(t *testing.T) {
	f := newF2(t)
	f.data.chunk = 2 << 20 // above the 1 MB limit of ordinary requests
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = f.ops, f.data, NewJobs() })
	wait := func(id string) Job {
		var j Job
		waitFor(t, func() bool { j, _ = c.Job(ctx, id); return j.Done })
		return j
	}
	a := Actor{User: "furkan", Via: "tunnel"}

	// Uncompressed archive padded to 5 MB: three chunks.
	data := append(fakeArchive(map[string]map[string]int64{"eski": {"urunler": 10, "yeni": 2}}, nil), bytes.Repeat([]byte(" "), 5<<20)...)
	st, err := c.CreateUpload(ctx, "magaza", "eski.archive", int64(len(data)), a)
	if err != nil || st.Chunks() != 3 {
		t.Fatalf("create: %+v %v", st, err)
	}
	if _, err := c.PutChunk(ctx, st.ID, 0, strings.Repeat("0", 64), bytes.NewReader(make([]byte, MaxUploadChunk+10)), -1); err == nil {
		t.Error("oversized chunk accepted")
	}
	for i := 0; i < st.Chunks(); i++ {
		part := data[int64(i)*st.Chunk : min(int64(len(data)), int64(i+1)*st.Chunk)]
		sum := sha256.Sum256(part)
		if i == 1 { // a wrong checksum first: refused, then sent again
			if _, err := c.PutChunk(ctx, st.ID, i, strings.Repeat("0", 64), bytes.NewReader(part), int64(len(part))); err == nil || err.(*Error).Status != 422 {
				t.Errorf("bad chunk: %v", err)
			}
		}
		if _, err := c.PutChunk(ctx, st.ID, i, hex.EncodeToString(sum[:]), bytes.NewReader(part), int64(len(part))); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	if st, err = c.FinishUpload(ctx, st.ID, a); err != nil || !st.Finished || st.Kind != archive.MongoArchive {
		t.Fatalf("finish: %+v %v", st, err)
	}

	id, err := c.CheckImport(ctx, "magaza", st.ID, "", a)
	if err != nil {
		t.Fatal(err)
	}
	if j := wait(id); j.Err != "" || j.Result != st.ID || !j.Cancellable {
		t.Fatalf("check job: %+v", j)
	}
	ic, err := c.Import(ctx, "magaza", st.ID)
	if err != nil || ic.ArchiveDocs != 12 {
		t.Fatalf("import: %+v %v", ic, err)
	}
	if _, err := c.ApplyImport(ctx, "magaza", st.ID, ModeReplace, "magaz", a); err == nil || err.(*Error).Status != 422 {
		t.Errorf("wrong confirmation: %v", err)
	}
	if _, err := c.ApplyImport(ctx, "magaza", st.ID, "merge", "magaza", a); err == nil {
		t.Error("unknown mode accepted")
	}
	id, err = c.ApplyImport(ctx, "magaza", st.ID, ModeReplace, "magaza", a)
	if err != nil {
		t.Fatal(err)
	}
	j := wait(id)
	if j.Err != "" || j.Result == "" || j.Cancellable {
		t.Fatalf("apply job: %+v", j)
	}
	if got := f.mongo.get("magaza"); !reflect.DeepEqual(got, map[string]int64{"urunler": 10, "yeni": 2}) {
		t.Errorf("after import: %v", got)
	}
	if _, err := c.Import(ctx, "magaza", st.ID); err == nil || err.(*Error).Status != 404 {
		t.Errorf("applied upload still there: %v", err)
	}

	if _, err := c.RestoreSnapshot(ctx, "magaza", j.Result, "yanlış", a); err == nil || err.(*Error).Status != 422 {
		t.Errorf("restore without confirmation: %v", err)
	}
	id, err = c.RestoreSnapshot(ctx, "magaza", j.Result, "magaza", a)
	if err != nil {
		t.Fatal(err)
	}
	if j := wait(id); j.Err != "" {
		t.Fatalf("restore: %+v", j)
	}
	if got := f.mongo.get("magaza"); got["eski"] != 7 {
		t.Errorf("after restore: %v", got)
	}

	id, err = c.Export(ctx, "magaza", a)
	if err != nil {
		t.Fatal(err)
	}
	j = wait(id)
	list, err := c.Exports(ctx, "magaza")
	if j.Err != "" || err != nil || len(list) != 1 || list[0].ID != j.Result {
		t.Fatalf("export: %+v %+v %v", j, list, err)
	}
	resp, err := c.DownloadExport(ctx, "magaza", j.Result, a, http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if sum := sha256.Sum256(body); hex.EncodeToString(sum[:]) != resp.Header.Get("X-Content-Sha256") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), ".footops.tar") {
		t.Errorf("export download: %v", resp.Header)
	}

	single, err := c.SingleUpload(ctx, "magaza", "küçük.archive", bytes.NewReader(data[:4000]), a)
	if err != nil || !single.Finished || single.Size != 4000 || single.Name != "küçük.archive" {
		t.Errorf("single: %+v %v", single, err)
	}
	if err := c.RemoveUpload(ctx, single.ID); err != nil {
		t.Error(err)
	}

	entries, _ := s.Audit.Tail(50)
	seen := map[string]string{}
	for _, e := range entries {
		seen[e.Action] += e.Detail + "|"
		if e.Actor.User != "furkan" {
			t.Errorf("audit actor: %+v", e)
		}
	}
	for action, want := range map[string]string{
		"upload": "eski.archive", "import.check": "kaynak eski", "import.apply": "güvenlik kopyası",
		"db.restore": "geri yüklendi", "site.export": "FootOps arşivi", "site.export.download": "",
	} {
		if d, ok := seen[action]; !ok || !strings.Contains(d, want) {
			t.Errorf("audit %s: %q", action, d)
		}
	}
	if strings.Contains(fmt.Sprint(entries), "RootSecret42") {
		t.Error("secret in the audit log")
	}
}

func (f *f2Env) running() bool {
	f.sites.mu.Lock()
	defer f.sites.mu.Unlock()
	return f.sites.running
}

// The safety copy is taken with the site stopped, so it holds every write.
func TestSafetyCopyWhileStopped(t *testing.T) {
	f := newF2(t)
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	var runningAtDump []bool
	f.mongo.onDump = func() { runningAtDump = append(runningAtDump, f.running()) }
	if _, err := f.data.ApplyImport(ctx, f.task(), "magaza", id, ModeReplace, Actor{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runningAtDump, []bool{false}) {
		t.Errorf("site running during the copy: %v", runningAtDump)
	}
}

// A rollback is never "verified" in the recovery record, even when the new data had
// been counted before the site failed to start.
func TestRollbackPhaseInRecord(t *testing.T) {
	f := newF2(t)
	f.sites.health = func(started int) error {
		if started == 1 {
			return errors.New("HTTP 500")
		}
		return nil
	}
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	phases := map[int]string{}
	f.mongo.onRestore = func(n int) {
		for _, r := range f.data.Recoveries() {
			phases[n] = r.Phase
		}
	}
	at := f.task()
	if _, err := f.data.ApplyImport(ctx, at, "magaza", id, ModeReplace, Actor{}); err == nil || !strings.Contains(err.Error(), "geri alındı") {
		t.Fatalf("%v\n%s", err, steps(at))
	}
	// restore 1 is the trial (no record), 2 the import, 3 the rollback.
	if phases[2] != phaseRestoring || phases[3] != phaseRollback {
		t.Errorf("phases = %v", phases)
	}
}

func TestStopFailureStartsSiteAgain(t *testing.T) {
	f := newF2(t)
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	f.sites.stopErr = errors.New("compose stop: timeout")
	if _, err := f.data.ApplyImport(ctx, f.task(), "magaza", id, ModeReplace, Actor{}); err == nil {
		t.Fatal("apply succeeded although the site could not be stopped")
	}
	if !reflect.DeepEqual(f.sites.calls, []string{"health1", "stop", "start"}) || f.mongo.get("magaza")["eski"] != 7 {
		t.Errorf("calls %v, data %v", f.sites.calls, f.mongo.get("magaza"))
	}
	if len(f.data.Recoveries()) != 0 {
		t.Error("record left")
	}
}

// mongodump writes system.js; restores skip system collections, so counts must too.
func TestRestoreWithSystemCollection(t *testing.T) {
	f := newF2(t)
	f.mongo.set("magaza", map[string]int64{"urunler": 3, "system.js": 2})
	snap, err := f.data.Snapshot(ctx, f.task(), "magaza", "manual", Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Collections["system.js"] != 2 {
		t.Fatalf("copy %+v", snap.Collections)
	}
	f.mongo.set("magaza", map[string]int64{"urunler": 9})
	rt := f.task()
	if _, err := f.data.RestoreSnapshot(ctx, rt, "magaza", snap.ID, Actor{}); err != nil {
		t.Fatalf("%v\n%s", err, steps(rt))
	}
	if got := f.mongo.get("magaza")["urunler"]; got != 3 {
		t.Errorf("urunler = %d", got)
	}
}

func TestRecoverRecordIsTriedOnce(t *testing.T) {
	f := newF2(t)
	r := recovery{Job: "Dead2", Site: "magaza", Snapshot: "20200101-000000", Running: true, Phase: phaseRestoring}
	if err := f.data.saveRecovery(r); err != nil {
		t.Fatal(err)
	}
	if err := f.data.Recover(ctx, f.task(), r); err == nil || !strings.Contains(err.Error(), "bulunamadı") {
		t.Fatalf("err = %v", err)
	}
	if len(f.data.Recoveries()) != 0 {
		t.Error("a failed record is kept and would be replayed later")
	}
	// Nothing had changed yet: only the site comes back.
	r = recovery{Job: "Dead3", Site: "magaza", Running: true, Phase: phasePrepare}
	f.sites.running = false
	if err := f.data.Recover(ctx, f.task(), r); err != nil || !f.sites.running || f.mongo.get("magaza")["eski"] != 7 {
		t.Errorf("prepare phase: %v running=%v", err, f.sites.running)
	}
}

func TestTempDatabaseMarker(t *testing.T) {
	f := newF2(t)
	f.mongo.failDrop = true
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err != nil {
		t.Fatal(err)
	}
	if !f.data.TempLeft() {
		t.Fatal("no marker after a failed drop")
	}
	f.mongo.failDrop = false
	if _, err := f.data.DropTempDatabases(ctx); err != nil || f.data.TempLeft() {
		t.Errorf("marker kept: %v", err)
	}
}

func TestImportNeedsDisk(t *testing.T) {
	f := newF2(t)
	id := f.upload(t, "a.gz", gzipped(fakeArchive(map[string]map[string]int64{"x": {"urunler": 10}}, nil)))
	f.data.checkFree = func(path string, need int64) error {
		if strings.HasSuffix(path, "mongo/data") {
			return errors.New("diskte yer yok")
		}
		return nil
	}
	if _, err := f.data.CheckImport(ctx, f.task(), "magaza", id, ""); err == nil || !strings.Contains(err.Error(), "diskte yer yok") {
		t.Errorf("err = %v", err)
	}
	if len(f.mongo.users) != 0 || f.mongo.restores != 0 {
		t.Error("work started without room")
	}
}

func TestSingleUploadOverChunkSize(t *testing.T) {
	f := newF2(t)
	st, err := f.data.SingleUpload("magaza", "buyuk", io.LimitReader(zeros{}, MaxUploadChunk+5<<20), Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.data.GetUpload(st.ID); err != nil || !got.Finished || len(got.Missing) != 0 || got.Chunks() != 2 {
		t.Errorf("40 MB single upload unusable: %+v %v", got, err)
	}
	if len(f.data.uploadLocks) != 0 {
		t.Errorf("upload locks kept: %d", len(f.data.uploadLocks))
	}
}

func TestRestartRefusedDuringJob(t *testing.T) {
	f := newF2(t)
	block := make(chan struct{})
	c, s := startServer(t, 999, func(s *Server) { s.Sites, s.Data, s.Jobs = f.ops, f.data, NewJobs() })
	if _, err := s.Jobs.Start(JobSpec{Kind: "import.apply", Target: "footops-mongo"}, func(context.Context, *Task) error {
		<-block
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	err := c.Restart(ctx, "footops-mongo", Actor{User: "furkan"})
	close(block)
	if err == nil || err.(*Error).Status != 409 {
		t.Errorf("restart during a job on it: %v", err)
	}
	if d := s.Docker.(*fakeDocker); len(d.restarted) != 0 {
		t.Error("restarted")
	}
}
