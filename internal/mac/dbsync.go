package mac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The Mac's own databases beside the sites' (panel-tasarim §33.4; FootOps.app's
// Veritabanları window): a database of the Mac's MongoDB is paired once with a site of a
// server, then compared with the site's latest backup (R2, the Mac's read-only key: the
// server is not touched; the backup is loaded beside it under a temporary name, dropped
// after), sent (an archive the panel's İçe aktar takes: the preview and the go-ahead are
// the panel's), or pulled (the backup in the database's place, a copy of what was there
// kept first). No merging: which side is right is the owner's call.

const dbUsage = `Kullanım: footops-mac veritabani <komut> [--json]

  liste                        Mac'teki MongoDB'nin veritabanları ve eşleştikleri siteler
  eslestir <vt> <sunucu> <site>
                               veritabanını bir sunucunun sitesiyle eşleştirir (bu Mac'te kalır; aşağıdakiler
                               bunu kullanır)
  eslestir <vt> --kaldir       eşleştirmeyi kaldırır
  karsilastir <vt> [zaman | yedek]
                               veritabanını sitenin son yedeğiyle (ya da verilenle) karşılaştırır: koleksiyon başına
                               aynı / farklı / yalnız yerelde / yalnız sunucuda belgeler ve iki tarafın en yeni kaydı.
                               Yedek Mac'teki MongoDB'ye geçici adla yüklenir, iş bitince silinir; sunucuya dokunulmaz
  gonder <vt>                  panelin İçe aktar'ına uygun arşiv üretir (~/Downloads) ve sayfanın adresini yazar;
                               önizleme ve onay panelde
  cek <vt> [zaman | yedek]     sitenin son yedeğini (ya da verileni) veritabanının yerine koyar; önce yereldekinin
                               kopyası alınır: <vt>_yedek_<tarih>_<saat>

  --sunucu S --site X          eşleştirme yerine (karsilastir, gonder, cek)

Birleştirme yok: hangi tarafın doğru olduğuna sen karar verirsin. MONGO_URI: Mac'teki MongoDB.
`

// dbCmd is footops-mac veritabani …
func (e *env) dbCmd(ctx context.Context, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return errUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "liste":
		if len(rest) > 0 {
			return errUsage
		}
		return e.dbList(ctx, jsonOut)
	case "eslestir":
		return e.dbPairCmd(ctx, rest, jsonOut)
	case "karsilastir", "cek", "gonder":
		req, err := dbArgs(rest, sub != "gonder")
		if err != nil {
			return err
		}
		switch sub {
		case "karsilastir":
			return e.dbCompare(ctx, req, jsonOut)
		case "cek":
			return e.dbPull(ctx, req, jsonOut)
		}
		return e.dbSend(ctx, req, jsonOut)
	case "yardim", "help", "-h", "--help":
		fmt.Fprint(e.out, dbUsage)
		return nil
	}
	return fmt.Errorf("%w: bilinmeyen komut: veritabani %s", errUsage, clip(sub, 40))
}

// ---- pairing ----

// The pairings are their own file beside the settings (veritabanlari.json, 0600; no
// secrets): the Settings window writes the settings whole and knows nothing of them.
const pairsFormat = 1

type dbPairsFile struct {
	Format int               `json:"format"`
	Pairs  map[string]dbPair `json:"pairs"`
}

// dbPair is a site of a server a database of the Mac's is paired with.
type dbPair struct {
	Server string `json:"server"`
	Site   string `json:"site"`
}

// readPairs reads the pairings; none is none. A pairing not to the rules (the file
// edited by hand) is left out, hiding no other.
func (e *env) readPairs() (map[string]dbPair, error) {
	out := map[string]dbPair{}
	data, err := os.ReadFile(e.pairs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return out, nil
	case err != nil:
		return nil, fmt.Errorf("eşleştirmeler okunamadı: %v", err)
	}
	var f dbPairsFile
	if err := json.Unmarshal(data, &f); err != nil || f.Format != pairsFormat {
		return nil, fmt.Errorf("eşleştirmeler dosyası anlaşılamadı: %s (düzelt ya da sil)", e.pairs)
	}
	for db, p := range f.Pairs {
		if checkDBName(db) == nil && reCode.MatchString(p.Server) && reSiteName.MatchString(p.Site) {
			out[db] = p
		}
	}
	return out, nil
}

// changePairs changes the pairings, one footops-mac at a time, and writes them whole or
// not at all.
func (e *env) changePairs(change func(map[string]dbPair)) error {
	dir := filepath.Dir(e.pairs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(e.pairs+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	pairs, err := e.readPairs()
	if err != nil {
		return err
	}
	change(pairs)
	data, err := json.MarshalIndent(dbPairsFile{Format: pairsFormat, Pairs: pairs}, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".veritabanlari-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, e.pairs)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("eşleştirmeler yazılamadı: %v", err)
	}
	return nil
}

// DBPairView is a pairing as the Mac shows it: the server as it calls it.
type DBPairView struct {
	Server     string `json:"server"`
	ServerName string `json:"server_name"`
	Site       string `json:"site"`
}

func pairView(p dbPair, names map[string]string) *DBPairView {
	return &DBPairView{Server: p.Server, ServerName: displayName(p.Server, names), Site: p.Site}
}

// DBEntry is a database of the Mac's MongoDB (Opened: one footops-mac loaded or copied),
// or a paired one not there (Missing).
type DBEntry struct {
	Name    string      `json:"name"`
	Bytes   int64       `json:"bytes"`
	Opened  bool        `json:"opened,omitempty"`
	Missing bool        `json:"missing,omitempty"`
	Pair    *DBPairView `json:"pair,omitempty"`
}

// DBList is veritabani liste's answer. MongoError: the Mac's MongoDB could not be asked;
// PairsError: the pairings could not be read.
type DBList struct {
	URI        string    `json:"uri,omitempty"`
	Databases  []DBEntry `json:"databases"`
	MongoError string    `json:"mongo_error,omitempty"`
	PairsError string    `json:"pairs_error,omitempty"`
}

func (e *env) dbList(ctx context.Context, jsonOut bool) error {
	v := DBList{Databases: []DBEntry{}}
	pairs, err := e.readPairs()
	if err != nil {
		v.PairsError = err.Error()
	}
	names := e.names()
	var dbs map[string]int64
	if t, err := parseMongoURI(e.mongoURI); err != nil {
		v.MongoError = err.Error()
	} else {
		v.URI = t.label()
		if dbs, err = t.databases(ctx); err != nil {
			if errors.Is(err, errStopped) {
				return err
			}
			v.MongoError = err.Error()
		}
	}
	for name, size := range dbs {
		if checkDBName(name) == nil {
			d := DBEntry{Name: name, Bytes: size, Opened: reDefaultLoad.MatchString(name) || markedDBName(name) && e.registeredDB(name)}
			if p, ok := pairs[name]; ok {
				d.Pair = pairView(p, names)
			}
			v.Databases = append(v.Databases, d)
		}
	}
	if dbs != nil {
		for name, p := range pairs {
			if _, ok := dbs[name]; !ok {
				v.Databases = append(v.Databases, DBEntry{Name: name, Missing: true, Pair: pairView(p, names)})
			}
		}
	}
	sort.Slice(v.Databases, func(i, j int) bool { return v.Databases[i].Name < v.Databases[j].Name })
	if jsonOut {
		e.event(v)
		return nil
	}
	if v.MongoError != "" {
		return errors.New(v.MongoError)
	}
	printDBList(e.out, v)
	return nil
}

// dbPairCmd is veritabani eslestir: <vt> <sunucu> <site>, or <vt> --kaldir.
func (e *env) dbPairCmd(ctx context.Context, args []string, jsonOut bool) error {
	pos, _, set, err := options(args, nil, []string{"--kaldir"})
	if err != nil {
		return err
	}
	if len(pos) == 0 || set["--kaldir"] && len(pos) != 1 || !set["--kaldir"] && len(pos) != 3 {
		return errUsage
	}
	db := pos[0]
	if err := checkDBName(db); err != nil {
		return err
	}
	type answer struct {
		DB   string      `json:"db"`
		Pair *DBPairView `json:"pair"`
	}
	if set["--kaldir"] {
		if err := e.changePairs(func(p map[string]dbPair) { delete(p, db) }); err != nil {
			return err
		}
		if jsonOut {
			e.event(answer{DB: db})
		} else {
			fmt.Fprintf(e.out, "✓ %s artık hiçbir siteyle eşleşmiyor\n", db)
		}
		return nil
	}
	p := dbPair{Server: strings.ToLower(pos[1]), Site: pos[2]}
	if !reSiteName.MatchString(p.Site) {
		return fmt.Errorf("geçersiz site adı: %q (küçük harf, rakam ve -; 3-32 karakter, harfle başlar)", clip(p.Site, 40))
	}
	list, err := e.serverList(ctx)
	if err != nil {
		return err
	}
	known := false
	for _, s := range list {
		known = known || s.Code == p.Server
	}
	if !known {
		return unknownServer(pos[1], list)
	}
	if err := e.changePairs(func(m map[string]dbPair) { m[db] = p }); err != nil {
		return err
	}
	v := pairView(p, e.names())
	if jsonOut {
		e.event(answer{DB: db, Pair: v})
	} else {
		fmt.Fprintf(e.out, "✓ %s → %s / %s\n", db, v.ServerName, v.Site)
	}
	return nil
}

// ---- comparing, pulling, sending ----

// dbReq is what karsilastir, cek and gonder are asked: the database, a backup (none: the
// newest), the site (none: the database's pairing).
type dbReq struct {
	db, when     string
	server, site string
}

func dbArgs(args []string, takesWhen bool) (dbReq, error) {
	pos, vals, _, err := options(args, []string{"--sunucu", "--site"}, nil)
	if err != nil {
		return dbReq{}, err
	}
	most := 1
	if takesWhen {
		most = 2
	}
	if len(pos) == 0 || len(pos) > most {
		return dbReq{}, errUsage
	}
	req := dbReq{db: pos[0], server: strings.ToLower(vals["--sunucu"]), site: vals["--site"]}
	if len(pos) == 2 {
		req.when = pos[1]
	}
	switch {
	case (req.server == "") != (req.site == ""):
		return req, fmt.Errorf("%w: --sunucu ile --site birlikte verilir", errUsage)
	case req.site != "" && !reSiteName.MatchString(req.site):
		return req, fmt.Errorf("geçersiz site adı: %q", clip(req.site, 40))
	case req.server != "" && !reCode.MatchString(req.server):
		return req, fmt.Errorf("geçersiz sunucu kodu: %q", clip(req.server, 40))
	}
	return req, checkDBName(req.db)
}

// pairFor is the site a request is about: the one given, else the database's pairing.
func (e *env) pairFor(req dbReq) (dbPair, error) {
	if req.server != "" {
		return dbPair{Server: req.server, Site: req.site}, nil
	}
	pairs, err := e.readPairs()
	if err != nil {
		return dbPair{}, err
	}
	p, ok := pairs[req.db]
	if !ok {
		return dbPair{}, fmt.Errorf("%s bir siteyle eşleşmedi: footops-mac veritabani eslestir %s <sunucu> <site> (ya da --sunucu ve --site)", req.db, req.db)
	}
	return p, nil
}

// chooseDB opens the site's server's repository and chooses the backup of the site's
// database: the newest, or the one asked for (a backup's id or minute, as yedek ac).
func (e *env) chooseDB(ctx context.Context, p dbPair, when string) (*repo, snapshot, error) {
	r, err := e.openRepo(ctx, p.Server)
	if err != nil {
		return nil, snapshot{}, err
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	list, _, err := r.snapshots(lctx, p.Site)
	cancel()
	if err != nil {
		return nil, snapshot{}, err
	}
	chosen, err := choose(list, p.Site, when, e.now().Location())
	if err != nil {
		return nil, snapshot{}, err
	}
	s, ok := chosen[partDB]
	if !ok {
		return nil, snapshot{}, fmt.Errorf("bu sitenin veritabanı yedeği yok: %s / %s (sitenin veritabanı yok ya da yedeği henüz alınmadı)", r.code, p.Site)
	}
	return r, s, nil
}

// DBBackup is the backup a database was compared with or pulled from.
type DBBackup struct {
	ID    string    `json:"id"`
	Run   string    `json:"run,omitempty"`
	Kind  string    `json:"kind,omitempty"`
	Time  time.Time `json:"time"`
	Bytes int64     `json:"bytes"`
}

func backupOf(s snapshot) DBBackup {
	return DBBackup{ID: s.ID, Run: s.Run, Kind: s.Kind, Time: s.Time, Bytes: s.Bytes}
}

// DBSyncResult is veritabani karsilastir's and cek's answer, and (without Diff and
// Loaded) their start. Of a pull: Copy is the copy of what was there (none: there was no
// such database), Loaded the backup in its place.
type DBSyncResult struct {
	DB         string    `json:"db"`
	Server     string    `json:"server"`
	ServerName string    `json:"server_name"`
	Site       string    `json:"site"`
	Backup     DBBackup  `json:"backup"`
	Temporary  string    `json:"temporary,omitempty"`
	Diff       *DBDiff   `json:"diff,omitempty"`
	Copy       string    `json:"copy,omitempty"`
	Loaded     *LoadedDB `json:"loaded,omitempty"`
	Warnings   []string  `json:"warnings,omitempty"`
	TookMS     int64     `json:"took_ms"`
}

// freeName is base, or base_2 … base_9, whichever is not a database there ("": none).
func freeName(dbs map[string]int64, base string) string {
	for i := 1; i <= 9; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s_%d", base, i)
		}
		if _, taken := dbs[n]; !taken && checkDBName(n) == nil {
			return n
		}
	}
	return ""
}

// copyName is the name a database's copy is taken under before a pull:
// <db>_yedek_<date>_<time> (its own name shortened to fit 63).
func copyName(db string, now time.Time) string {
	stamp := backupDBMark + now.Format("20060102_1504")
	if len(db)+len(stamp) > 61 { // room for _2 … _9
		db = db[:61-len(stamp)]
	}
	return db + stamp
}

// addOpened puts a database on the list of what was opened (one line a name).
func (e *env) addOpened(it openedEntry) error {
	return e.withOpened(func(l *openedList) bool {
		l.Items = dropWhere(l.Items, func(x openedEntry) bool { return x.Kind == "database" && x.Name == it.Name })
		l.Items = append(l.Items, it)
		return true
	})
}

// dropOpened takes a line off the list.
func (e *env) dropOpened(id string) error {
	return e.withOpened(func(l *openedList) bool {
		l.Items = dropWhere(l.Items, func(x openedEntry) bool { return x.ID == id })
		return true
	})
}

// lockDB holds a database of the Mac's for one footops-mac at a time while it is compared,
// pulled, sent — or deleted: two at once would copy a half-put-back database, drop one
// another's copies, delete what is being read. A lock file beside the list of what was
// opened (kept: removing it would race with the next taker), released when the process
// ends however it ends.
func (e *env) lockDB(name string) (release func(), err error) {
	dir := filepath.Join(filepath.Dir(e.opened), "kilit")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &codedError{code: "db_busy", text: fmt.Sprintf("%s ile şu an başka bir iş yapılıyor (karşılaştırma, çekme ya da gönderme; "+
				"uygulama kapandıysa arka planda bitiyor olabilir): bitince yeniden dene", name)}
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

// countsOf is a database's documents, per collection and time series, and in system.js
// (stored functions: a pull's copy keeps them) — what a copy must hold as well.
func countsOf(ctx context.Context, mt mongoTarget, db string) (map[string]int64, error) {
	c, err := mt.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	colls, err := listColls(ctx, c, db)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	colls["system.js"] = "collection"
	for n, kind := range colls {
		if kind == "collection" || kind == "timeseries" {
			if out[n], err = countColl(ctx, c, db, n); err != nil {
				return nil, err
			}
		}
	}
	if out["system.js"] == 0 {
		delete(out, "system.js")
	}
	return out, nil
}

func sameCounts(a, b map[string]int64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, n := range a {
		if m, ok := b[k]; !ok || m != n {
			return false
		}
	}
	return true
}

func sumCounts(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// dbCompare is veritabani karsilastir.
func (e *env) dbCompare(ctx context.Context, req dbReq, jsonOut bool) error {
	start := time.Now()
	loc := e.now().Location()
	p, err := e.pairFor(req)
	if err != nil {
		return err
	}
	if _, err := e.look("mongorestore"); err != nil {
		return errors.New("mongorestore bulunamadı: brew install mongodb-database-tools")
	}
	mt, err := parseMongoURI(e.mongoURI)
	if err != nil {
		return err
	}
	release, err := e.lockDB(req.db)
	if err != nil {
		return err
	}
	defer release()
	dbs, err := mt.databases(ctx)
	if err != nil {
		return err
	}
	if _, ok := dbs[req.db]; !ok {
		return fmt.Errorf("Mac'teki MongoDB'de %s yok (olanlar: footops-mac veritabani liste)", req.db)
	}
	r, s, err := e.chooseDB(ctx, p, req.when)
	if err != nil {
		return err
	}
	res := DBSyncResult{DB: req.db, Server: r.code, ServerName: displayName(r.code, e.names()), Site: p.Site, Backup: backupOf(s)}
	res.Temporary = freeName(dbs, defaultDBName(p.Site, s.Time.In(loc)))
	if res.Temporary == "" {
		return fmt.Errorf("geçici veritabanı için boş ad yok (%s…): Bu Mac'te açılanlar'dan eskileri sil", defaultDBName(p.Site, s.Time.In(loc)))
	}
	// Held as well: it is not deleted from the list of what was opened while it is read.
	releaseTmp, err := e.lockDB(res.Temporary)
	if err != nil {
		return err
	}
	defer releaseTmp()
	if err := e.roomFor(e.tmpDir(), 2*s.Bytes+spareRoom); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(e.tmpDir(), "footops-mac-vt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	// The backup loaded beside the owner's database is on the list of what was opened
	// while it is there: a footops-mac killed half-way leaves it to be deleted there. Only
	// one footops-mac made is dropped.
	now := e.now().UTC().Truncate(time.Second)
	entry := openedEntry{ID: newID(), Kind: "database", Name: res.Temporary, Server: r.code, Site: p.Site, Time: listTime(s.Time, now), At: now}
	ours, listed := false, false
	defer func() {
		if ours {
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			derr := mt.drop(dctx, res.Temporary)
			cancel()
			if derr != nil {
				fmt.Fprintf(e.errw, "geçici veritabanı silinemedi: %s (Bu Mac'te açılanlar'dan sil)\n", res.Temporary)
				return
			}
		}
		if listed {
			_ = e.dropOpened(entry.ID)
		}
	}()

	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			DBSyncResult
		}{"start", res})
	} else {
		fmt.Fprintf(e.out, "Karşılaştırılıyor: %s (bu Mac) ↔ %s / %s, %s yedeği (%s)\n", req.db, res.ServerName, p.Site,
			stamp(s.Time, loc), orDash(kindTR[s.Kind]))
	}
	prog.beat()

	file := filepath.Join(dir, "db.archive")
	e.say(jsonOut, "  … yedek indiriliyor (%s)\n", bytesTR(s.Bytes))
	n, err := r.dumpTo(ctx, s, file, func(done int64) { prog.report("download", done, s.Bytes) })
	if err != nil {
		return fmt.Errorf("yedek alınamadı: %w", err)
	}
	if s.Bytes > 0 && n != s.Bytes {
		return fmt.Errorf("yedek eksik geldi: %d / %d bayt", n, s.Bytes)
	}
	// The name again, just before loading: none is dropped that is not ours.
	if dbs, err = mt.databases(ctx); err != nil {
		return err
	}
	if _, taken := dbs[res.Temporary]; taken {
		return fmt.Errorf("Mac'teki MongoDB'de %s az önce açıldı: yeniden dene", res.Temporary)
	}
	ours = true
	if err := e.addOpened(entry); err != nil {
		res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
	} else {
		listed = true
	}
	e.say(jsonOut, "  … Mac'teki MongoDB'ye geçici olarak yükleniyor: %s\n", res.Temporary)
	loaded, err := e.loadDB(ctx, mt, dumpSource{path: file}, p.Site, res.Temporary, false, func(done, total int64) { prog.report("load", done, total) })
	if err != nil {
		return err
	}
	if loaded.Failed > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("yedeğin %d belgesi yüklenemedi: karşılaştırmada sunucuda yok sayıldı", loaded.Failed))
	}
	e.say(jsonOut, "  … karşılaştırılıyor\n")
	c, err := mt.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	diff, err := compareDBs(ctx, c, req.db, res.Temporary, func(done, total int64) { prog.report("compare", done, total) })
	if err != nil {
		return err
	}
	res.Diff = &diff
	res.TookMS = time.Since(start).Milliseconds()
	prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			DBSyncResult
		}{"result", res})
		return nil
	}
	printDBDiff(e.out, res, e.now())
	return nil
}

// dbPull is veritabani cek: the site's backup in the database's place, a copy of what
// was there taken first. Once the database is dropped for the backup, nothing stops it
// half-way (a stop, the app quitting, ^C): it becomes the backup or — the loading failed
// — what it was, put back from its own dump.
func (e *env) dbPull(ctx context.Context, req dbReq, jsonOut bool) (err error) {
	start := time.Now()
	loc := e.now().Location()
	p, err := e.pairFor(req)
	if err != nil {
		return err
	}
	for _, tool := range []string{"mongodump", "mongorestore"} {
		if _, err := e.look(tool); err != nil {
			return fmt.Errorf("%s bulunamadı: brew install mongodb-database-tools", tool)
		}
	}
	mt, err := parseMongoURI(e.mongoURI)
	if err != nil {
		return err
	}
	release, err := e.lockDB(req.db)
	if err != nil {
		return err
	}
	defer release()
	dbs, err := mt.databases(ctx)
	if err != nil {
		return err
	}
	size, exists := dbs[req.db]
	r, s, err := e.chooseDB(ctx, p, req.when)
	if err != nil {
		return err
	}
	res := DBSyncResult{DB: req.db, Server: r.code, ServerName: displayName(r.code, e.names()), Site: p.Site, Backup: backupOf(s)}
	if exists {
		if res.Copy = freeName(dbs, copyName(req.db, e.now().In(loc))); res.Copy == "" {
			return fmt.Errorf("kopya için boş ad yok (%s…): Bu Mac'te açılanlar'dan eskileri sil", copyName(req.db, e.now().In(loc)))
		}
		releaseCopy, err := e.lockDB(res.Copy)
		if err != nil {
			return err
		}
		defer releaseCopy()
	}
	// The backup's dump and the database's (uncompressed: two or three times what the
	// disk keeps of it), the copy and the backup loaded.
	if err := e.roomFor(e.tmpDir(), 2*s.Bytes+4*size+spareRoom); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(e.tmpDir(), "footops-mac-vt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			DBSyncResult
		}{"start", res})
	} else {
		fmt.Fprintf(e.out, "Çekiliyor: %s / %s, %s yedeği (%s) → bu Mac'te %s\n", res.ServerName, p.Site, stamp(s.Time, loc),
			orDash(kindTR[s.Kind]), req.db)
	}
	prog.beat()

	server := filepath.Join(dir, "sunucu.archive")
	e.say(jsonOut, "  … yedek indiriliyor (%s)\n", bytesTR(s.Bytes))
	n, err := r.dumpTo(ctx, s, server, func(done int64) { prog.report("download", done, s.Bytes) })
	if err != nil {
		return fmt.Errorf("yedek alınamadı: %w", err)
	}
	if s.Bytes > 0 && n != s.Bytes {
		return fmt.Errorf("yedek eksik geldi: %d / %d bayt", n, s.Bytes)
	}

	// The copy of what is there, whole before anything changes (its system.js too); it
	// must hold what the database holds now, else the database is in use and nothing
	// changes. Dropped again when the pull ends with the database as it was.
	var mine string
	keepCopy := false
	if exists {
		mine = filepath.Join(dir, "yerel.archive")
		e.say(jsonOut, "  … yereldekinin kopyası: %s\n", res.Copy)
		prog.report("copy", 0, 0)
		if err := e.dumpLocal(ctx, mt, req.db, mine); err != nil {
			return fmt.Errorf("yereldekinin kopyası alınamadı: %w", err)
		}
		if dbs, err = mt.databases(ctx); err != nil {
			return err
		}
		if _, taken := dbs[res.Copy]; taken {
			return fmt.Errorf("Mac'teki MongoDB'de %s az önce açıldı: yeniden dene", res.Copy)
		}
		cp, err := e.loadDB(ctx, mt, dumpSource{path: mine, own: true}, req.db, res.Copy, false, func(done, total int64) { prog.report("copy", done, total) })
		if err != nil {
			return fmt.Errorf("yereldekinin kopyası alınamadı: %w", err)
		}
		entry := openedEntry{ID: newID(), Kind: "database", Name: res.Copy, Copy: req.db, Time: e.now().UTC().Truncate(time.Second)}
		entry.At = entry.Time
		defer func() {
			if keepCopy {
				return
			}
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if mt.drop(dctx, res.Copy) == nil {
				_ = e.dropOpened(entry.ID)
			}
			cancel()
		}()
		want, err := countsOf(ctx, mt, req.db)
		if err != nil {
			return err
		}
		got, err := countsOf(ctx, mt, res.Copy)
		if err != nil {
			return err
		}
		if !sameCounts(want, got) || cp.Failed > 0 {
			return fmt.Errorf("yereldekinin kopyası tutmadı: %s'te %d belge, kopyada %d%s — veritabanı kopyalanırken değişmiş olabilir: "+
				"onu kullanan uygulamayı (geliştirme sunucusu) durdurup yeniden dene; hiçbir şey değişmedi", req.db, sumCounts(want), sumCounts(got), failedTR(cp.Failed))
		}
		if err := e.addOpened(entry); err != nil {
			res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
		}
		e.say(jsonOut, "  ✓ kopya: %s (%d belge)\n", res.Copy, sumCounts(got))
	}

	// From here the database is replaced: not stopped half-way, whatever comes.
	if ctx.Err() != nil {
		return errStopped
	}
	if e.hold != nil {
		e.hold()
	}
	crit := context.WithoutCancel(ctx)
	prog.report("load", 0, 0)
	// One that appeared meanwhile is not ours to replace (it is not copied).
	if dbs, err = mt.databases(crit); err != nil {
		return err
	}
	if _, now := dbs[req.db]; now && !exists {
		return fmt.Errorf("Mac'teki MongoDB'de %s az önce oluştu (onu kullanan bir uygulama açık olabilir): durdurup yeniden dene; hiçbir şey değişmedi", req.db)
	}
	if exists {
		if err := mt.drop(crit, req.db); err != nil {
			keepCopy = true
			return fmt.Errorf("yereldeki %s bırakılamadı (%v): önceki hâlinin kopyası %s'te", req.db, err, res.Copy)
		}
	}
	e.say(jsonOut, "  … yedek %s'in yerine yükleniyor (bu adım durdurulamaz)\n", req.db)
	loaded, lerr := e.loadDB(crit, mt, dumpSource{path: server}, p.Site, req.db, false, func(done, total int64) { prog.report("load", done, total) })
	if lerr != nil {
		if !exists {
			return lerr // there was none: nothing to put back (what was loaded is dropped)
		}
		e.say(jsonOut, "  … yüklenemedi; yereldeki eski hâline getiriliyor\n")
		prog.report("restore", 0, 0)
		back, berr := e.loadDB(crit, mt, dumpSource{path: mine, own: true}, req.db, req.db, false, func(done, total int64) { prog.report("restore", done, total) })
		if berr == nil && back.Failed > 0 {
			berr = fmt.Errorf("%d belge yüklenemedi", back.Failed)
		}
		if berr != nil {
			keepCopy = true
			return fmt.Errorf("%v — yereldeki %s eski hâline de getirilemedi (%v): önceki hâli %s'te duruyor", lerr, req.db, berr, res.Copy)
		}
		return fmt.Errorf("%v — yereldeki %s olduğu gibi kaldı", lerr, req.db)
	}
	keepCopy = true
	loaded.Replaced = exists
	res.Loaded = loaded
	if loaded.Failed > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("yedeğin %d belgesi yüklenemedi: yereldeki yedekle birebir değil", loaded.Failed))
	}
	res.TookMS = time.Since(start).Milliseconds()
	prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			DBSyncResult
		}{"result", res})
		return nil
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(e.out, "  ⚠ %s\n", w)
	}
	fmt.Fprintf(e.out, "✓ Bitti: %s artık %s / %s'in %s yedeği (%d belge)", req.db, res.ServerName, p.Site, stamp(s.Time, loc), loaded.Documents)
	if res.Copy != "" {
		fmt.Fprintf(e.out, "; önceki hâli %s'te (gerekmeyince sil: footops-mac yedek sil %s)", res.Copy, res.Copy)
	}
	fmt.Fprintln(e.out)
	return nil
}

// dumpLocal writes a database of the Mac's MongoDB to a file (mongodump --archive, a
// collection at a time).
func (e *env) dumpLocal(ctx context.Context, mt mongoTarget, db, file string) error {
	tool, err := e.look("mongodump")
	if err != nil {
		return errors.New("mongodump bulunamadı: brew install mongodb-database-tools")
	}
	conn, done, err := e.toolArgs(mt)
	if err != nil {
		return err
	}
	defer done()
	said := &tail{max: 16 << 10}
	cmd := exec.CommandContext(ctx, tool, append(conn, "--db="+db, "--archive="+file, "--numParallelCollections=1")...)
	cmd.Stdout, cmd.Stderr = said, said
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	switch {
	case ctx.Err() != nil:
		return errStopped
	case err != nil:
		return fmt.Errorf("mongodump başarısız: %s", toolWords(mt.masked(said.String()), err.Error()))
	}
	return nil
}

// dbSend is veritabani gonder: arsiv olustur of the database, for the paired site's
// panel, whose import page it names.
func (e *env) dbSend(ctx context.Context, req dbReq, jsonOut bool) error {
	p, err := e.pairFor(req)
	if err != nil {
		return err
	}
	release, err := e.lockDB(req.db)
	if err != nil {
		return err
	}
	defer release()
	return e.createArchive(ctx, createReq{db: req.db, site: p.Site, importTo: p.Server}, jsonOut)
}
