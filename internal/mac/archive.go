package mac

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
)

// Archives on the Mac (panel-tasarim §29.6): a FootOps archive (the panel's Dışa aktar, a
// backup's İndir) or a mongodump archive, checked as the panel's İçe aktar checks an
// upload — by the same code, internal/archive (rule K2) — then opened into a folder or
// loaded into the Mac's MongoDB; and one made from the Mac's MongoDB and a folder, for a
// panel to import (a new site's way: its database from the Mac, panel-tasarim §24.9). An
// archive file is only ever read.

const archiveUsage = `Kullanım: footops-mac arsiv <komut> [--json]

  dogrula <dosya>              arşivi panelin İçe aktar'ının kurallarıyla denetler, hiçbir şey yazmaz:
                               FootOps arşivi (.footops.tar), mongodump arşivi (.archive, .gz) ya da
                               dosya arşivi (zip, tar.gz)
  ac <dosya> [--hedef KLASÖR] [--db] [--dosyalar] [--ayarlar] [--mongo [--kaynak VT] [--ad VT] [--uzerine]]
                               FootOps arşivini denetler ve klasöre açar (varsayılan ~/Downloads/<site>-<zaman>):
                               db.archive, files/, env, manifest.json; yalnız bazıları: --db / --dosyalar / --ayarlar
                               --mongo: veritabanını Mac'teki MongoDB'ye de yükler (varsayılan ad
                               <site>_arsiv_<tarih>_<saat>, başkası --ad ile; o adla veritabanı varsa --uzerine ister)
  mongo <dosya> [--kaynak VT] [--ad VT] [--uzerine]
                               yalnız veritabanını Mac'teki MongoDB'ye yükler (FootOps ya da mongodump arşivi;
                               içinde birden çok veritabanı varsa --kaynak ile biri seçilir); klasör bırakmaz
  olustur [--db VT] [--dosyalar KLASÖR] [--site AD] [--hedef YOL]
                               Mac'teki MongoDB'nin bir veritabanından ve / veya bir klasörden panelin İçe aktar'ına
                               uygun FootOps arşivi üretir (varsayılan ~/Downloads/<site>-<zaman>.footops.tar)
  veritabanlari                Mac'teki MongoDB'nin veritabanları (olustur için)
  acilanlar                    bu Mac'te açılanlar (yedek acilanlar ile aynı liste)
  sil <klasör | veritabanı>    açılan bir klasörü ya da *_yedek_* / *_arsiv_* veritabanını siler

Arşiv önce denetlenir, sonra açılır; arşiv dosyasının kendisi hiç değişmez. MONGO_URI: Mac'teki MongoDB.
`

// The kinds of archive.
const (
	kindFootOps   = "footops"
	kindMongodump = "mongodump"
	kindFiles     = "files"
)

// maxListed and maxDatabases bound what is listed of a dump: a whole server's may hold
// thousands of collections, and a crafted one a hundred thousand databases.
const (
	maxListed    = 1000
	maxDatabases = 100
)

// archiveCmd is footops-mac arsiv …
func (e *env) archiveCmd(ctx context.Context, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return errUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "dogrula":
		if len(rest) != 1 || anyFlag(rest) {
			return errUsage
		}
		return e.checkCmd(ctx, rest[0], jsonOut)
	case "ac", "mongo":
		req, err := archiveOpenArgs(rest, sub == "ac")
		if err != nil {
			return err
		}
		return e.openArchive(ctx, req, jsonOut)
	case "olustur":
		req, err := createArgs(rest)
		if err != nil {
			return err
		}
		return e.createArchive(ctx, req, jsonOut)
	case "veritabanlari":
		if len(rest) > 0 {
			return errUsage
		}
		return e.localDatabases(ctx, jsonOut)
	case "acilanlar":
		if len(rest) > 0 {
			return errUsage
		}
		return e.openedCmd(ctx, jsonOut)
	case "sil":
		if len(rest) != 1 || anyFlag(rest) {
			return errUsage
		}
		return e.deleteOpened(ctx, rest[0], jsonOut)
	case "yardim", "help", "-h", "--help":
		fmt.Fprint(e.out, archiveUsage)
		return nil
	}
	return fmt.Errorf("%w: bilinmeyen komut: arsiv %s", errUsage, clip(sub, 40))
}

// options reads a command's arguments: positional ones, options with a value (--ad AD or
// --ad=AD) and plain ones (--uzerine); any other option is a usage error.
func options(args []string, valued, plain []string) (pos []string, vals map[string]string, set map[string]bool, err error) {
	vals, set = map[string]string{}, map[string]bool{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		name, value, hasValue := strings.Cut(a, "=")
		switch {
		case slices.Contains(valued, name):
			if !hasValue {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return nil, nil, nil, fmt.Errorf("%w: %s bir değer ister", errUsage, name)
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return nil, nil, nil, fmt.Errorf("%w: %s boş olamaz", errUsage, name)
			}
			vals[name] = value
		case !hasValue && slices.Contains(plain, name):
			set[name] = true
		default:
			return nil, nil, nil, fmt.Errorf("%w: bilinmeyen seçenek: %s", errUsage, clip(a, 40))
		}
	}
	return pos, vals, set, nil
}

// ---- checking ----

// ArchiveInfo is what checking an archive found (arsiv dogrula).
type ArchiveInfo struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // footops, mongodump, files
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// A FootOps archive's manifest: the site it was made of, when, by which FootOps, and
	// the backup it was made from (if it was).
	Site     string         `json:"site,omitempty"`
	Created  time.Time      `json:"created,omitzero"`
	FootOps  string         `json:"footops,omitempty"`
	Backup   *ArchiveBackup `json:"backup,omitempty"`
	Database *ArchiveDB     `json:"database,omitempty"`
	Files    *ArchiveFiles  `json:"files,omitempty"`
	// Env: the site's environment is in it (its secrets too), EnvVars variables.
	Env     bool     `json:"env,omitempty"`
	EnvVars int      `json:"env_vars,omitempty"`
	Notes   []string `json:"notes,omitempty"`
	TookMS  int64    `json:"took_ms"`
}

// ArchiveBackup is the backup a FootOps archive was made from (the panel's İndir).
type ArchiveBackup struct {
	Server string    `json:"server,omitempty"`
	Kind   string    `json:"kind,omitempty"`
	Time   time.Time `json:"time"`
}

// ArchiveDB is an archive's database: what the dump says of itself and, in a FootOps
// archive, the manifest's counts — the dump's own word until it is loaded and counted.
type ArchiveDB struct {
	// Name is the database loaded (the manifest's, or the dump's only one); empty: the
	// dump holds several, one is chosen (--kaynak).
	Name string `json:"name,omitempty"`
	// Databases are the dump's (the first maxDatabases; MoreDatabases the rest).
	Databases     []string            `json:"databases"`
	MoreDatabases int                 `json:"more_databases,omitempty"`
	Collections   []ArchiveCollection `json:"collections"`
	// More collections were left off the list.
	More int `json:"more,omitempty"`
	// Documents is the manifest's count of Name (-1: not known — a mongodump archive).
	Documents  int64  `json:"documents"`
	Server     string `json:"server,omitempty"`
	Tool       string `json:"tool,omitempty"`
	Bytes      int64  `json:"bytes"`      // the dump uncompressed
	Compressed int64  `json:"compressed"` // as it lies in the file
}

// ArchiveCollection is one collection (or view) of a dump; Documents -1: not known.
type ArchiveCollection struct {
	DB        string `json:"db"`
	Name      string `json:"name"`
	View      bool   `json:"view,omitempty"`
	Documents int64  `json:"documents"`
}

// ArchiveFiles is an archive's files: all of them (Hidden ones, never imported,
// included), and in an archive of files only, the one folder all are in (Prefix) and
// .bson files (a mongodump folder?).
type ArchiveFiles struct {
	Files       int    `json:"files"`
	Dirs        int    `json:"dirs"`
	Bytes       int64  `json:"bytes"`
	Hidden      int    `json:"hidden,omitempty"`
	HiddenBytes int64  `json:"hidden_bytes,omitempty"`
	Prefix      string `json:"prefix,omitempty"`
	BSON        int    `json:"bson,omitempty"`
	Compressed  int64  `json:"compressed"`
}

// checked is an archive as checking found it.
type checked struct {
	info ArchiveInfo
	c    *archive.Contents // a FootOps archive
	gzip bool              // a mongodump archive, gzip'd
	// dbs are all the dump's databases that can be loaded (info lists only the first).
	dbs []string
	// sha256 is the mongodump archive's (the whole file's), to read it again by.
	sha256 string
}

// source is where the archive's database lies — a FootOps archive's member, or the file —
// and its checksum as checked: what is loaded is read again by it.
func (k *checked) source() dumpSource {
	if k.c != nil {
		m := k.c.Members[archive.MemberDB]
		return dumpSource{path: k.info.Path, offset: m.Offset, length: m.Size, gzip: true, sha256: m.SHA256}
	}
	return dumpSource{path: k.info.Path, gzip: k.gzip, sha256: k.sha256}
}

// checkArchive reads an archive as the panel's İçe aktar reads an upload: its kind from
// its content, its integrity and what is in it, every byte once (an archive of files,
// twice: archive.Check reads it itself). onBytes is told how far.
func (e *env) checkArchive(ctx context.Context, asked string, onBytes func(done, total int64)) (*checked, error) {
	start := time.Now()
	p, err := filepath.Abs(expandHome(asked))
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, readErr(p, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s bir dosya değil", p)
	}
	kind, err := archive.Detect(p)
	if err != nil {
		return nil, readErr(p, err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, readErr(p, err)
	}
	defer f.Close()
	k := &checked{info: ArchiveInfo{Path: p, Name: clean(filepath.Base(p)), Size: fi.Size()}}
	in := ctxReader{ctx, &countingReader{r: bufio.NewReaderSize(f, 256<<10), total: fi.Size(), on: onBytes}}
	switch kind {
	case archive.Tar:
		c, rerr := archive.ReadFootOps(in)
		switch {
		case errors.Is(rerr, archive.ErrNotFootOps):
			err = k.checkFiles(ctx, f, onBytes)
		case rerr != nil:
			err = rerr
		default:
			err = k.fromFootOps(c)
		}
	case archive.MongoArchive, archive.MongoArchiveGz:
		h := sha256.New()
		pr, size, rerr := archive.ReadMongo(in, kind == archive.MongoArchiveGz, h)
		if err = rerr; err == nil {
			k.info.Kind, k.info.SHA256, k.gzip = kindMongodump, hex.EncodeToString(h.Sum(nil)), kind == archive.MongoArchiveGz
			k.sha256 = k.info.SHA256
			compressed := int64(0)
			if k.gzip {
				compressed = fi.Size()
			}
			k.info.Database, k.dbs, err = dbInfo(pr, size, compressed, nil)
		}
	case archive.TarGz, archive.Zip:
		err = k.checkFiles(ctx, f, onBytes)
	default:
		err = fmt.Errorf("bu dosya tanınmadı (tür: %s): FootOps arşivi (.footops.tar), mongodump arşivi "+
			"(mongodump --archive=dosya.gz --gzip) ya da dosya arşivi (zip, tar.gz) olmalı", orDash(string(kind)))
	}
	if ctx.Err() != nil {
		return nil, errStopped
	}
	if err != nil {
		return nil, err
	}
	k.info.addNotes()
	k.info.TookMS = time.Since(start).Milliseconds()
	return k, nil
}

// fromFootOps is what a FootOps archive's reading found.
func (k *checked) fromFootOps(c archive.Contents) error {
	man := c.Manifest
	i := &k.info
	i.Kind, i.SHA256 = kindFootOps, c.SHA256
	i.Site, i.Created, i.FootOps = clip(clean(man.Site), 64), man.Created, clip(clean(man.FootOps), 64)
	if b := man.Backup; b != nil {
		i.Backup = &ArchiveBackup{Server: clip(clean(b.Server), 32), Kind: clip(clean(b.Kind), 16), Time: b.Time}
	}
	if c.DB != nil {
		d, dbs, err := dbInfo(*c.DB, c.DBSize, c.Members[archive.MemberDB].Size, &man)
		if err != nil {
			return err
		}
		i.Database, k.dbs = d, dbs
	}
	if c.Files != nil {
		i.Files = filesInfo(*c.Files, c.Members[archive.MemberFiles].Size)
	}
	i.Env, i.EnvVars = c.Env, c.EnvVars
	k.c = &c
	return nil
}

// checkFiles checks an archive of files (a zip, a tar, a tar.gz) as the panel does, then
// reads it again for its checksum.
func (k *checked) checkFiles(ctx context.Context, f *os.File, onBytes func(done, total int64)) error {
	s, err := archive.Check(k.info.Path, archive.DefaultLimits)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, &countingReader{r: bufio.NewReaderSize(f, 256<<10), total: k.info.Size, on: onBytes}}); err != nil {
		return err
	}
	k.info.Kind, k.info.SHA256 = kindFiles, hex.EncodeToString(h.Sum(nil))
	k.info.Files = filesInfo(s, k.info.Size)
	return nil
}

// dbInfo is what a dump says of its databases, the lists cut short (a crafted prelude
// may name a hundred thousand), and all of the databases that can be loaded.
func dbInfo(p archive.MongoPrelude, size, compressed int64, man *archive.Manifest) (*ArchiveDB, []string, error) {
	dbs := loadableDBs(p.Databases())
	if len(dbs) == 0 {
		return nil, nil, errors.New("dökümde yüklenebilecek bir veritabanı yok (admin, config, local alınmaz)")
	}
	d := &ArchiveDB{Databases: dbs[:min(len(dbs), maxDatabases)], MoreDatabases: max(len(dbs)-maxDatabases, 0),
		Collections: []ArchiveCollection{}, Documents: -1, Server: clip(clean(p.ServerVersion), 64),
		Tool: clip(clean(p.ToolVersion), 64), Bytes: size, Compressed: compressed}
	switch {
	case man != nil && slices.Contains(dbs, man.DB):
		d.Name, d.Documents = man.DB, man.Documents
	case len(dbs) == 1:
		d.Name = dbs[0]
	}
	// The namespaces grouped by database in one pass, system collections left out.
	byDB := map[string][]archive.MongoNamespace{}
	for _, db := range dbs {
		byDB[db] = nil
	}
	for _, n := range p.Namespaces {
		if list, ok := byDB[n.DB]; ok && n.Collection != "" && !strings.HasPrefix(n.Collection, "system.") {
			byDB[n.DB] = append(list, n)
		}
	}
	for _, db := range dbs {
		list := byDB[db]
		if len(d.Collections) >= maxListed {
			d.More += len(list)
			continue
		}
		slices.SortFunc(list, func(a, b archive.MongoNamespace) int { return strings.Compare(a.Collection, b.Collection) })
		for _, n := range list {
			if len(d.Collections) >= maxListed {
				d.More++
				continue
			}
			c := ArchiveCollection{DB: db, Name: clip(clean(n.Collection), 255), View: n.Type == "view", Documents: -1}
			if man != nil && db == man.DB {
				if v, ok := man.Collections[n.Collection]; ok {
					c.Documents = v
				}
			}
			d.Collections = append(d.Collections, c)
		}
	}
	return d, dbs, nil
}

// namesTR lists names, the first few: "a, b, c ve 7 daha".
func namesTR(names []string, more int) string {
	shown := names[:min(len(names), 10)]
	more += len(names) - len(shown)
	text := strings.Join(shown, ", ")
	if more > 0 {
		text += fmt.Sprintf(" ve %d daha", more)
	}
	return text
}

func filesInfo(s archive.Summary, compressed int64) *ArchiveFiles {
	return &ArchiveFiles{Files: s.Files, Dirs: s.Dirs, Bytes: s.Size, Hidden: s.Hidden, HiddenBytes: s.HiddenBytes,
		Prefix: clip(clean(s.Prefix), 255), BSON: s.BSON, Compressed: compressed}
}

// loadableDBs are a dump's databases that can be loaded: not MongoDB's own (admin,
// config, local, as the panel leaves them out), named as a database can be — and nothing
// in the name to turn a line around in the window.
func loadableDBs(dbs []string) []string {
	out := []string{}
	for _, db := range dbs {
		switch {
		case db == "", db == "admin", db == "config", db == "local", len(db) > 63,
			strings.ContainsAny(db, "/\\. \"$*<>:|?\x00"), clean(db) != db:
			continue
		}
		out = append(out, db)
	}
	return out
}

// addNotes says what is worth knowing of what was found.
func (i *ArchiveInfo) addNotes() {
	if d := i.Database; d != nil && d.Name == "" {
		i.Notes = append(i.Notes, fmt.Sprintf("Dökümde %d veritabanı var (%s): yüklerken ya da içe aktarırken biri seçilir.",
			len(d.Databases)+d.MoreDatabases, namesTR(d.Databases, d.MoreDatabases)))
	}
	if f := i.Files; f != nil {
		if f.Hidden > 0 {
			i.Notes = append(i.Notes, fmt.Sprintf("%d gizli dosya (%s; .htaccess, .DS_Store gibi) içe aktarılmaz, klasöre de açılmaz.",
				f.Hidden, bytesTR(f.HiddenBytes)))
		}
		if f.BSON > 0 {
			i.Notes = append(i.Notes, fmt.Sprintf("Arşivde %d .bson dosyası var: bu bir mongodump klasörü olabilir. Klasör hâlindeki döküm içe "+
				"aktarılmaz: mongorestore --dir ile Mac'teki MongoDB'ye yükle, sonra footops-mac arsiv olustur ile tek dosya üret.", f.BSON))
		}
		if f.Prefix != "" && i.Kind == kindFiles {
			i.Notes = append(i.Notes, fmt.Sprintf("Bütün dosyalar %s/ klasörünün içinde: panel içe aktarırken \"üst klasörü atla\"yı önerebilir.", f.Prefix))
		}
	}
	if i.Env {
		i.Notes = append(i.Notes, fmt.Sprintf("Arşivde sitenin ortam değişkenleri var (%d değişken; sırlar içerir): içe aktarma onlara "+
			"dokunmaz, klasöre açılınca env dosyasına yazılır.", i.EnvVars))
	}
}

// checkCmd is arsiv dogrula.
func (e *env) checkCmd(ctx context.Context, path string, jsonOut bool) error {
	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	prog.beat()
	k, err := e.checkArchive(ctx, path, func(done, total int64) { prog.report("check", done, total) })
	prog.end()
	if err != nil {
		return err
	}
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			ArchiveInfo
		}{"result", k.info})
		return nil
	}
	printArchiveInfo(e.out, k.info, e.now())
	return nil
}

// ---- opening ----

// archiveOpenReq is what arsiv ac (a folder) and arsiv mongo (the database alone) are
// asked.
type archiveOpenReq struct {
	path   string
	folder bool
	// target is the folder asked for ("": ~/Downloads/<site>-<time>); only the parts
	// asked for (none: every part the archive has).
	target string
	only   map[string]bool
	// mongo loads the database (source: the dump's database, when it holds several) into
	// the Mac's MongoDB as dbName ("": <site>_arsiv_…); overwrite drops one of that name
	// first.
	mongo     bool
	source    string
	dbName    string
	overwrite bool
}

func archiveOpenArgs(args []string, folder bool) (archiveOpenReq, error) {
	req := archiveOpenReq{folder: folder, only: map[string]bool{}, mongo: !folder}
	valued, plain := []string{"--kaynak", "--ad"}, []string{"--uzerine"}
	if folder {
		valued = append(valued, "--hedef")
		plain = append(plain, "--db", "--dosyalar", "--ayarlar", "--mongo")
	}
	pos, vals, set, err := options(args, valued, plain)
	if err != nil {
		return req, err
	}
	if len(pos) != 1 {
		return req, errUsage
	}
	req.path, req.target, req.source, req.dbName, req.overwrite = pos[0], vals["--hedef"], vals["--kaynak"], vals["--ad"], set["--uzerine"]
	req.mongo = req.mongo || set["--mongo"]
	for flag, part := range map[string]string{"--db": partDB, "--dosyalar": partFiles, "--ayarlar": partDef} {
		if set[flag] {
			req.only[part] = true
		}
	}
	switch {
	case (req.source != "" || req.dbName != "" || req.overwrite) && !req.mongo:
		return req, fmt.Errorf("%w: --kaynak, --ad ve --uzerine yalnız --mongo ile", errUsage)
	case req.mongo && len(req.only) > 0 && !req.only[partDB]:
		return req, fmt.Errorf("%w: --mongo veritabanını ister: --db'yi de seç", errUsage)
	}
	return req, nil
}

// archiveDBName is the default name an archive's database is loaded under:
// <site>_arsiv_<date>_<time>, the site's (or the dump's database's) name kept to letters,
// digits and _.
func archiveDBName(base string, at time.Time) string {
	suffix := archiveDBMark + at.Format("20060102_1504")
	b := []byte(base)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			b[i] = '_'
		}
	}
	name := string(b[:min(len(b), 63-len(suffix))])
	if strings.Trim(name, "_") == "" {
		name = "arsiv"
	}
	return name + suffix
}

// openArchive is arsiv ac and arsiv mongo: the archive checked whole first (rule K2: an
// archive is opened only once it is known to be intact), then its parts written into a
// folder of the owner's and / or its database loaded into the Mac's MongoDB — straight
// from the archive, nothing else written.
func (e *env) openArchive(ctx context.Context, req archiveOpenReq, jsonOut bool) (err error) {
	start := time.Now()
	loc := e.now().Location()
	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	prog.beat()
	k, err := e.checkArchive(ctx, req.path, func(done, total int64) { prog.report("check", done, total) })
	if err != nil {
		return err
	}
	info := k.info
	switch {
	case info.Kind == kindFiles:
		return errors.New("bu bir dosya arşivi (zip, tar): içinde veritabanı yok; Finder'da açılabilir, panelin İçe aktar'ı da alır")
	case req.folder && k.c == nil:
		return errors.New("klasöre yalnız FootOps arşivi açılır; mongodump arşivini Mac'teki MongoDB'ye yüklemek için: footops-mac arsiv mongo <dosya>")
	}
	have := map[string]bool{partDB: info.Database != nil, partFiles: info.Files != nil, partDef: info.Env}
	for p := range req.only {
		if !have[p] {
			return fmt.Errorf("bu arşivde %s yok", partTR[p])
		}
	}
	want := map[string]bool{}
	if req.folder {
		for _, p := range siteParts {
			if have[p] && (len(req.only) == 0 || req.only[p]) {
				want[p] = true
			}
		}
	}
	if req.mongo && info.Database == nil {
		return errors.New("bu arşivde veritabanı yok")
	}
	site := info.Site
	if !reSiteName.MatchString(site) {
		site = "arsiv"
	}
	at := info.Created
	if at.IsZero() { // a mongodump archive: the file's own time
		if fi, err := os.Stat(info.Path); err == nil {
			at = fi.ModTime().UTC().Truncate(time.Second)
		}
	}
	res := OpenResult{Site: info.Site, Archive: info.Name, Time: at, Parts: map[string]OpenedPart{}}
	if info.Backup != nil {
		res.Server = info.Backup.Server
	}

	// The Mac's MongoDB before anything is written: the database chosen, the name free or
	// given up.
	var mt mongoTarget
	exists := false
	from := ""
	if req.mongo {
		d := info.Database
		from = d.Name
		if req.source != "" {
			if !slices.Contains(k.dbs, req.source) {
				return fmt.Errorf("dökümde %q veritabanı yok (olanlar: %s)", clip(clean(req.source), 64), namesTR(d.Databases, d.MoreDatabases))
			}
			from = req.source
		}
		if from == "" {
			return &codedError{code: "choose_db", fields: map[string]any{"databases": d.Databases},
				text: fmt.Sprintf("dökümde birden çok veritabanı var (%s): --kaynak ile birini seç", namesTR(d.Databases, d.MoreDatabases))}
		}
		if req.dbName == "" {
			base := site
			if k.c == nil || info.Site == "" {
				base = from
			}
			req.dbName = archiveDBName(base, at.In(loc))
		}
		if err := checkDBName(req.dbName); err != nil {
			return err
		}
		if _, err := e.look("mongorestore"); err != nil {
			return errors.New("mongorestore bulunamadı: brew install mongodb-database-tools")
		}
		// The Mac's MongoDB keeps its data on this disk (Homebrew's): room for the dump
		// uncompressed — more than it takes there (a folder's room is asked below).
		if !req.folder {
			if err := e.roomFor(e.tmpDir(), d.Bytes+spareRoom); err != nil {
				return err
			}
		}
		if mt, err = parseMongoURI(e.mongoURI); err != nil {
			return err
		}
		dbs, err := mt.databases(ctx)
		if err != nil {
			return err
		}
		_, exists = dbs[req.dbName]
		if exists && !req.overwrite {
			return &codedError{code: "db_exists", fields: map[string]any{"database": req.dbName},
				text: fmt.Sprintf("Mac'teki MongoDB'de %s adlı veritabanı var: üzerine yazmak için --uzerine, başka ad için --ad", req.dbName)}
		}
		res.Parts[partDB] = OpenedPart{BackupPart: BackupPart{Time: at, Bytes: d.Bytes}}
	}

	// The folder: on the list of what was opened from its first moment (partial), its mark
	// held, as yedek ac's.
	var target string
	var entry openedEntry
	whole := !req.folder
	if req.folder {
		need := int64(spareRoom)
		if want[partDB] {
			need += info.Database.Bytes
		}
		if want[partFiles] {
			need += info.Files.Bytes
		}
		if req.mongo {
			need += info.Database.Bytes // the load as well (see above)
		}
		created := false
		if target, created, err = e.openTarget(req.target, site, at.In(loc), need); err != nil {
			return err
		}
		res.Target = target
		mark, err := makeMark(target)
		if err != nil {
			if created {
				removeTree(target)
			}
			return err
		}
		defer mark.release()
		entry = e.folderEntry(mark, target, res.Server, site, at, !created)
		entry.Archive = info.Name
		if err := e.beginOpened(entry); err != nil {
			res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
		}
		defer func() {
			if whole {
				return
			}
			if _, rerr := removeOpened(target, !created); rerr == nil {
				e.endOpened(entry, false)
			}
		}()
		if created && e.exclude != nil {
			e.exclude(target) // its secrets kept out of Time Machine, as yedek ac's
		}
	}

	if jsonOut {
		head := res
		if req.mongo {
			head.Database = &LoadedDB{Name: req.dbName, Replaced: exists}
		}
		e.event(struct {
			Event string `json:"event"`
			OpenResult
		}{"start", head})
	} else {
		printArchiveOpenStart(e.out, res, req)
	}

	f, err := os.Open(info.Path)
	if err != nil {
		return readErr(info.Path, err)
	}
	defer f.Close()
	// A member is read again by its checksum as checked: the file may have changed since.
	member := func(name string) *reread {
		m := k.c.Members[name]
		return &reread{r: ctxReader{ctx, io.NewSectionReader(f, m.Offset, m.Size)}, h: sha256.New(), want: m.SHA256}
	}
	if want[partDB] {
		m := k.c.Members[archive.MemberDB]
		e.say(jsonOut, "  … veritabanı (%s)\n", bytesTR(info.Database.Bytes))
		r := member(archive.MemberDB)
		n, err := gunzipTo(&countingReader{r: r, total: m.Size,
			on: func(done, total int64) { prog.report(partDB, done, total) }}, filepath.Join(target, "db.archive"))
		if err == nil {
			err = r.same()
		}
		if ctx.Err() != nil {
			return errStopped
		}
		if err != nil {
			return fmt.Errorf("veritabanı açılamadı: %w", err)
		}
		if n != info.Database.Bytes {
			return fmt.Errorf("veritabanı tutmuyor: %d bayt açıldı, denetimde %d", n, info.Database.Bytes)
		}
		res.Parts[partDB] = OpenedPart{BackupPart: BackupPart{Time: at, Bytes: info.Database.Bytes}, Written: n}
		e.say(jsonOut, "  ✓ db.archive (%s)\n", bytesTR(n))
	}
	if want[partFiles] {
		fs := info.Files
		expect := datafs.Stats{Files: fs.Files - fs.Hidden, Bytes: fs.Bytes - fs.HiddenBytes}
		e.say(jsonOut, "  … dosyalar (%d dosya, %s)\n", expect.Files, bytesTR(expect.Bytes))
		dir := filepath.Join(target, "files")
		if err := os.Mkdir(dir, 0o700); err != nil {
			return folderErr(dir, err)
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return folderErr(dir, err)
		}
		r := member(archive.MemberFiles)
		st, err := datafs.ExtractTarGz(r, root, datafs.Options{Compressed: fs.Compressed,
			Progress: func(s datafs.Stats) { prog.report(partFiles, s.Bytes, expect.Bytes) }})
		root.Close()
		if err == nil {
			err = r.same()
		}
		if ctx.Err() != nil {
			return errStopped
		}
		if errors.Is(err, os.ErrExist) {
			// The panel's Linux takes names a case-insensitive Mac disk calls one.
			return fmt.Errorf("dosyalar açılamadı: %w — arşivde yalnız büyük / küçük harfle (ya da Unicode yazılışıyla) ayrılan adlar var; "+
				"Mac'in diski bunları aynı ad sayıyor (panel içe aktarabilir); yalnız --db / --ayarlar açılabilir", err)
		}
		if err != nil {
			return fmt.Errorf("dosyalar açılamadı: %w", err)
		}
		if st.Files != expect.Files || st.Bytes != expect.Bytes {
			return fmt.Errorf("açılan dosyalar arşivle tutmuyor: %d dosya / %d bayt, beklenen %d / %d", st.Files, st.Bytes, expect.Files, expect.Bytes)
		}
		res.Parts[partFiles] = OpenedPart{BackupPart: BackupPart{Time: at, Files: int64(st.Files), Bytes: expect.Bytes}, Written: st.Bytes,
			Count: int64(st.Files)}
		e.say(jsonOut, "  ✓ files/ (%d dosya, %s)\n", st.Files, bytesTR(st.Bytes))
	}
	if want[partDef] {
		r := member(archive.MemberEnv)
		b, err := io.ReadAll(io.LimitReader(r, archive.MaxEnv+1))
		if err == nil {
			err = r.same()
		}
		if err != nil {
			return err
		}
		if err := writeNew(filepath.Join(target, "env"), b); err != nil {
			return err
		}
		res.Parts[partDef] = OpenedPart{BackupPart: BackupPart{Time: at}, Written: int64(len(b)), Count: int64(info.EnvVars)}
		e.say(jsonOut, "  ✓ env (%d değişken; sırlar içerir, klasör yalnız senin)\n", info.EnvVars)
	}
	if req.folder {
		r := member(archive.MemberManifest)
		b, err := io.ReadAll(io.LimitReader(r, archive.MaxManifest+1))
		if err == nil {
			err = r.same()
		}
		if err != nil {
			return err
		}
		if err := writeNew(filepath.Join(target, archive.MemberManifest), b); err != nil {
			return err
		}
	}
	if req.mongo {
		// Again just before loading: another footops-mac may have taken the name meanwhile.
		dbs, err := mt.databases(ctx)
		if err != nil {
			return err
		}
		if _, now := dbs[req.dbName]; now && !exists {
			return &codedError{code: "db_exists", fields: map[string]any{"database": req.dbName},
				text: fmt.Sprintf("Mac'teki MongoDB'de %s adlı veritabanı az önce açıldı: üzerine yazmak için --uzerine, başka ad için --ad", req.dbName)}
		}
		src := k.source()
		if want[partDB] {
			src = dumpSource{path: filepath.Join(target, "db.archive")} // opened already: no gzip to undo
		}
		e.say(jsonOut, "  … Mac'teki MongoDB: %s → %s (%s)\n", from, req.dbName, mt.label())
		db, err := e.loadDB(ctx, mt, src, from, req.dbName, exists, func(done, total int64) { prog.report("mongo", done, total) })
		if err != nil {
			return err
		}
		if k.c != nil && from == info.Database.Name && info.Database.Documents > 0 {
			db.Expected = info.Database.Documents
			if db.Documents+db.Failed != db.Expected {
				res.Warnings = append(res.Warnings, fmt.Sprintf("yüklenen %d belge, arşivin manifestinde %d: döküm manifestle tutmuyor", db.Documents+db.Failed, db.Expected))
			}
		}
		res.Database = db
		e.say(jsonOut, "  ✓ yüklendi: %s (%d belge%s)\n", db.Name, db.Documents, failedTR(db.Failed))
		if err := e.noteDatabase(res); err != nil {
			res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
		}
	}

	// The folder whole: its line on the list no longer partial.
	if req.folder {
		whole = true
		if err := e.endOpened(entry, true); err != nil {
			res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
		}
	}
	res.TookMS = time.Since(start).Milliseconds()
	prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			OpenResult
		}{"result", res})
		return nil
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(e.out, "  ⚠ %s\n", w)
	}
	if req.folder {
		fmt.Fprintf(e.out, "✓ Bitti: %s\n", target)
	} else {
		fmt.Fprintf(e.out, "✓ Bitti: Mac'teki MongoDB'de %s\n", res.Database.Name)
	}
	return nil
}

// gunzipTo writes a gzip'd stream, uncompressed, to a new file of the owner's: its gzip
// checksum read to the end.
func gunzipTo(r io.Reader, file string) (int64, error) {
	zr, err := gzip.NewReader(bufio.NewReaderSize(r, 256<<10))
	if err != nil {
		return 0, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	out, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, folderErr(file, err)
	}
	n, err := io.Copy(out, zr)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file)
		return n, err
	}
	return n, nil
}

// errArchiveChanged: a member read again is not what was checked.
var errArchiveChanged = errors.New("arşiv dosyası denetimden sonra değişmiş: yeniden denetle")

// reread is a member of a checked archive read again, through its checksum.
type reread struct {
	r    io.Reader
	h    hash.Hash
	want string
}

func (m *reread) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	m.h.Write(p[:n])
	return n, err
}

// same reads what is left of the member and tells whether it was all as checked.
func (m *reread) same() error {
	if _, err := io.Copy(io.Discard, m); err != nil {
		return err
	}
	if hex.EncodeToString(m.h.Sum(nil)) != m.want {
		return errArchiveChanged
	}
	return nil
}

// ctxReader stops a read once ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// readErr says why a file could not be read; macOS refusing the app a folder of the
// owner's (privacy: EPERM) in so many words.
func readErr(p string, err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("macOS %s dosyasını okumaya izin vermedi: Sistem Ayarları › Gizlilik ve Güvenlik › Dosyalar ve Klasörler › FootOps", p)
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s yok", p)
	}
	return fmt.Errorf("%s okunamadı: %v", p, err)
}
