package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/run"
)

// Database imports (F2). An upload is first checked (rule K2): its type, its integrity,
// what it holds — by restoring it into a temporary database and counting. Only then can
// it be applied (rule K3): a safety copy of the site's database (K1), the site stops,
// the archive is restored with the site's own user (K4), the counts are compared, the
// site starts and must answer. Any failure restores the safety copy. A restore from one
// of the site's own copies runs the same way.

const (
	DefaultRecoveryDir = "/var/lib/footops/recovery"
	importCheckFile    = "check.json"
	tmpDBPrefix        = "footops_tmp_"

	ModeReplace = "replace" // the database becomes the archive's
	ModeAdd     = "add"     // only collections the site does not have are added

	FormatFootOps   = "footops"
	FormatMongodump = "mongodump"
)

// dbSection is where a mongodump archive lies inside a file: a whole upload or copy, or
// the db.archive.gz member of a FootOps archive (read in place, never extracted).
type dbSection struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Gzip   bool   `json:"gzip"`
	SHA256 string `json:"sha256"`
}

// ImportCheck is what checking an upload found. It is kept next to the upload and is
// the preview the operator confirms.
type ImportCheck struct {
	Upload   string          `json:"upload"`
	Job      string          `json:"job"`
	Site     string          `json:"site"`
	File     string          `json:"file"`
	Size     int64           `json:"size"`
	SHA256   string          `json:"sha256"`
	Format   string          `json:"format"`
	Manifest *ExportManifest `json:"manifest,omitempty"`
	Section  dbSection       `json:"section"`
	DataSize int64           `json:"data_size"` // the archive uncompressed
	HasFiles bool            `json:"has_files"` // files are imported by a later version (F2b)

	ServerVersion string   `json:"server_version"`
	ToolVersion   string   `json:"tool_version"`
	Databases     []string `json:"databases"` // importable databases in the archive
	SourceDB      string   `json:"source_db,omitempty"`
	Target        string   `json:"target"`
	// Skipped lists the source database's system collections: never imported (rule K4).
	Skipped []string `json:"skipped,omitempty"`

	// From the trial restore; empty while a database must still be chosen.
	Archive     map[string]int64 `json:"archive,omitempty"`
	Views       []string         `json:"views,omitempty"`
	ArchiveDocs int64            `json:"archive_docs"`
	// The site's database at the time of the check.
	Current      map[string]int64 `json:"current"`
	CurrentViews []string         `json:"current_views,omitempty"`
	CurrentDocs  int64            `json:"current_docs"`

	Checked time.Time `json:"checked"`
	Usage   run.Usage `json:"usage"`
}

// siteControl stops and starts sites around data work. *SiteOps implements it.
type siteControl interface {
	Running(ctx context.Context, name string) (bool, error)
	Stop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Health(ctx context.Context, name string, tries int) error
}

// ---- checking ----

// CheckImport checks an upload for a site's database. sourceDB picks the database when
// the archive holds several; empty chooses the only one, or leaves the choice to the
// operator. The site is not touched.
func (d *DataOps) CheckImport(ctx context.Context, t *Task, site, uploadID, sourceDB string) (ic ImportCheck, err error) {
	d.init()
	defer d.dropStaging(t)
	spec, creds, err := d.siteDB(site)
	if err != nil {
		return ic, err
	}
	u, path, err := d.uploadFile(spec.Name, uploadID)
	if err != nil {
		return ic, err
	}
	t.Step(fmt.Sprintf("✓ Dosya: %s (%s)", u.Name, humanBytes(u.Size)))
	ic = ImportCheck{Upload: u.ID, Job: t.ID(), Site: spec.Name, File: u.Name, Size: u.Size, Target: creds.DB}

	t.Step("… Dosya okunuyor: tür, bütünlük, içindekiler")
	p, err := d.inspect(ctx, t, path, &ic)
	if err != nil {
		return ic, err
	}
	ic.ServerVersion, ic.ToolVersion = p.ServerVersion, p.ToolVersion
	ic.Databases = importable(p.Databases())
	what := "mongodump arşivi"
	if ic.Format == FormatFootOps {
		what = "FootOps arşivi"
		if ic.Manifest != nil {
			what += fmt.Sprintf(" (%s sitesinden, %s)", ic.Manifest.Site, ic.Manifest.Created.Local().Format("02.01.2006 15:04"))
		}
	}
	t.Step(fmt.Sprintf("✓ %s, bütünlük tamam; MongoDB %s, araç %s; veritabanları: %s", what,
		orDash(p.ServerVersion), orDash(p.ToolVersion), orDash(strings.Join(ic.Databases, ", "))))
	if len(ic.Databases) == 0 {
		return ic, errors.New("arşivde içe aktarılabilecek bir veritabanı yok (admin, config, local alınmaz)")
	}
	switch {
	case sourceDB != "":
		if !contains(ic.Databases, sourceDB) {
			return ic, fmt.Errorf("arşivde %q veritabanı yok", sourceDB)
		}
	case len(ic.Databases) == 1:
		sourceDB = ic.Databases[0]
	}

	ic.Current, ic.CurrentViews, err = d.collCounts(ctx, creds)
	if err != nil {
		return ic, err
	}
	ic.CurrentDocs = sum(ic.Current)
	if sourceDB == "" {
		ic.Checked = d.now().UTC()
		if err := d.saveCheck(ic); err != nil {
			return ic, err
		}
		t.SetResult(ic.Upload)
		t.Step(fmt.Sprintf("ℹ Arşivde %d veritabanı var: önizleme sayfasında hangisinin içe aktarılacağını seç", len(ic.Databases)))
		return ic, nil
	}
	ic.SourceDB = sourceDB
	for _, n := range p.Namespaces {
		if n.DB == sourceDB && strings.HasPrefix(n.Collection, "system.") {
			ic.Skipped = append(ic.Skipped, n.Collection)
		}
	}
	if len(ic.Skipped) > 0 {
		t.Step("ℹ Sistem koleksiyonları alınmaz: " + strings.Join(ic.Skipped, ", "))
	}

	ic.Archive, ic.Views, err = d.trialRestore(ctx, t, path, ic.Section, sourceDB, ic.DataSize)
	if err != nil {
		return ic, err
	}
	ic.ArchiveDocs = sum(ic.Archive)
	ic.Usage, ic.Checked = t.Usage(), d.now().UTC()
	if err := d.saveCheck(ic); err != nil {
		return ic, err
	}
	t.SetResult(ic.Upload)
	t.Step(fmt.Sprintf("✓ Önizleme hazır: arşivde %d koleksiyon, %d belge; sitede şu an %d koleksiyon, %d belge",
		len(ic.Archive), ic.ArchiveDocs, len(ic.Current), ic.CurrentDocs))
	return ic, nil
}

func orDash(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func sum(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// importable leaves out MongoDB's own databases, our temporary ones and names MongoDB
// would not accept on Linux.
func importable(dbs []string) []string {
	out := []string{}
	for _, db := range dbs {
		switch {
		case db == "admin", db == "config", db == "local", strings.HasPrefix(db, tmpDBPrefix),
			len(db) > 63, strings.ContainsAny(db, "/\\. \"$\x00"):
			continue
		}
		out = append(out, db)
	}
	return out
}

// inspect recognises the upload and reads it once: SHA-256 of the whole file, gzip
// checksum, the mongodump prelude, and for a FootOps archive the manifest and the
// checksum of every member.
func (d *DataOps) inspect(ctx context.Context, t *Task, path string, ic *ImportCheck) (archive.MongoPrelude, error) {
	kind, err := archive.Detect(path)
	if err != nil {
		return archive.MongoPrelude{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return archive.MongoPrelude{}, err
	}
	defer f.Close()
	progress := func(n int64) { t.SetBytes(n, ic.Size) }
	switch kind {
	case archive.MongoArchive, archive.MongoArchiveGz:
		ic.Format = FormatMongodump
		h := sha256.New()
		pr := &progressReader{r: ctxReader{ctx, f}, report: progress}
		p, size, err := readMongoSection(pr, kind == archive.MongoArchiveGz, h)
		if err != nil {
			return p, err
		}
		ic.DataSize = size
		ic.SHA256 = hex.EncodeToString(h.Sum(nil))
		ic.Section = dbSection{Offset: 0, Length: ic.Size, Gzip: kind == archive.MongoArchiveGz, SHA256: ic.SHA256}
		return p, nil
	case archive.Tar:
		ic.Format = FormatFootOps
		return inspectFootOps(ctx, f, ic, progress)
	}
	return archive.MongoPrelude{}, fmt.Errorf("bu dosya veritabanı olarak içe aktarılamaz (tür: %s). Kabul edilenler: "+
		"FootOps arşivi (.footops.tar) ve mongodump arşivi (mongodump --archive=dosya.gz --gzip). "+
		"Klasör halindeki mongodump (zip) sonraki sürümde gelecek", orDash(string(kind)))
}

// readMongoSection reads a mongodump archive (gzip'd or not) to its end through h,
// returning its prelude and its uncompressed size. Reading to the end verifies the gzip
// checksum; a decompression bomb is refused like in archive.Check.
func readMongoSection(r io.Reader, gz bool, h hash.Hash) (archive.MongoPrelude, int64, error) {
	in := &progressReader{r: r}
	br := bufio.NewReaderSize(io.TeeReader(in, h), 256<<10)
	var plain io.Reader = br
	if gz {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return archive.MongoPrelude{}, 0, fmt.Errorf("gzip başlığı bozuk: %w", err)
		}
		plain = zr
	}
	l := archive.DefaultLimits
	lim := &io.LimitedReader{R: plain, N: l.MaxTotal + 1}
	p, err := archive.ReadMongoPrelude(lim)
	if err != nil {
		return p, 0, err
	}
	if _, err := io.Copy(io.Discard, lim); err != nil {
		return p, 0, fmt.Errorf("arşiv bozuk: %w", err)
	}
	size := l.MaxTotal + 1 - lim.N
	if lim.N <= 0 {
		return p, size, fmt.Errorf("arşivin açılmış hali %s sınırını aşıyor", humanBytes(l.MaxTotal))
	}
	if _, err := io.Copy(io.Discard, br); err != nil { // anything after the gzip stream
		return p, size, err
	}
	if gz && size > 64<<20 && size > in.n*l.MaxRatio {
		return p, size, fmt.Errorf("sıkıştırma oranı şüpheli (%d:1, sınır %d:1) — sıkıştırma bombası olabilir", size/max(in.n, 1), l.MaxRatio)
	}
	return p, size, nil
}

// FootOps archive members.
const (
	memberManifest = "manifest.json"
	memberDB       = "db.archive.gz"
	memberFiles    = "files.tar.gz"
	maxManifest    = 1 << 20
)

type tarMember struct {
	offset, size int64
	sha256       string
}

// inspectFootOps walks a FootOps archive (an uncompressed tar). Member offsets are
// counted as the tar reader consumes the file, so the database member can later be
// streamed in place.
func inspectFootOps(ctx context.Context, f *os.File, ic *ImportCheck, progress func(int64)) (archive.MongoPrelude, error) {
	var p archive.MongoPrelude
	total := sha256.New()
	pos := &progressReader{r: ctxReader{ctx, f}, report: progress}
	tr := tar.NewReader(io.TeeReader(pos, total))
	members := map[string]tarMember{}
	var man *ExportManifest
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return p, fmt.Errorf("tar bozuk: %w", err)
		}
		name, err := archive.CleanName(h.Name)
		if err != nil {
			return p, err
		}
		if h.Typeflag != tar.TypeReg {
			return p, fmt.Errorf("%s: FootOps arşivinde yalnız dosya olabilir", name)
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse") {
				return p, fmt.Errorf("%s: seyrek (sparse) dosya kabul edilmez", name)
			}
		}
		if _, dup := members[name]; dup {
			return p, fmt.Errorf("%s: arşivde iki kez var", name)
		}
		m := tarMember{offset: pos.n, size: h.Size}
		mh := sha256.New()
		switch name {
		case memberManifest:
			if h.Size > maxManifest {
				return p, errors.New("manifest.json çok büyük")
			}
			b, err := io.ReadAll(io.TeeReader(tr, mh))
			if err != nil {
				return p, err
			}
			man = &ExportManifest{}
			if err := json.Unmarshal(b, man); err != nil {
				return p, fmt.Errorf("manifest.json okunamadı: %w", err)
			}
		case memberDB:
			if p, ic.DataSize, err = readMongoSection(tr, true, mh); err != nil {
				return p, fmt.Errorf("%s: %w", memberDB, err)
			}
		case memberFiles:
			if _, err := io.Copy(mh, tr); err != nil {
				return p, err
			}
			ic.HasFiles = true
		default:
			return p, fmt.Errorf("FootOps arşivinde beklenmeyen dosya: %s", name)
		}
		// What was checked must be exactly the bytes at the recorded place in the file, as
		// they will be streamed from there later.
		if pos.n-m.offset != h.Size {
			return p, fmt.Errorf("%s: üye boyutu tutmuyor", name)
		}
		m.sha256 = hex.EncodeToString(mh.Sum(nil))
		members[name] = m
	}
	if _, err := io.Copy(total, pos); err != nil { // the tar's own padding after its end marker
		return p, err
	}
	ic.SHA256 = hex.EncodeToString(total.Sum(nil))
	if man == nil {
		return p, errors.New("manifest.json yok: bu bir FootOps arşivi değil. Klasör halindeki mongodump (tar/zip) sonraki sürümde gelecek; " +
			"şimdilik mongodump --archive=dosya.gz --gzip ile tek dosya üretebilirsin")
	}
	if man.Format != exportFormat {
		return p, fmt.Errorf("FootOps arşiv biçimi %d tanınmıyor (bu sürüm %d okur)", man.Format, exportFormat)
	}
	for name, want := range man.Members {
		got, ok := members[name]
		if !ok {
			return p, fmt.Errorf("manifest'te olan %s arşivde yok", name)
		}
		if got.size != want.Size || got.sha256 != want.SHA256 {
			return p, fmt.Errorf("%s manifest'le tutmuyor (boyut ya da sha256): dosya bozulmuş", name)
		}
	}
	for name := range members {
		if _, ok := man.Members[name]; !ok && name != memberManifest {
			return p, fmt.Errorf("%s manifest'te yok", name)
		}
	}
	db, ok := members[memberDB]
	if !ok {
		return p, errors.New("arşivde veritabanı yok (yalnız dosyalar var; dosya içe aktarma sonraki sürümde)")
	}
	ic.Manifest = man
	ic.Section = dbSection{Offset: db.offset, Length: db.size, Gzip: true, SHA256: db.sha256}
	return p, nil
}

// trialRestore restores the archive's sourceDB into a temporary database with a
// temporary user, counts what arrived and removes both again (also on failure or
// cancellation).
func (d *DataOps) trialRestore(ctx context.Context, t *Task, path string, sec dbSection, sourceDB string, size int64) (map[string]int64, []string, error) {
	if err := d.checkFree(d.mongoData(), 2*size); err != nil {
		return nil, nil, fmt.Errorf("deneme yüklemesi için diskte yer yok: %w", err)
	}
	tmp := tmpDBPrefix + strings.ToLower(t.ID())
	pw, err := randomSecret(32)
	if err != nil {
		return nil, nil, err
	}
	creds := dbCreds{User: tmp, Password: pw, DB: tmp}
	if _, err := rootMongosh(ctx, d.Run, d.mongoSecret(), fmt.Sprintf(
		`c.getDB(%s).createUser({user:%s,pwd:%s,roles:[{role:"readWrite",db:%s}]})`, q(tmp), q(tmp), q(pw), q(tmp))); err != nil {
		return nil, nil, fmt.Errorf("geçici veritabanı kullanıcısı oluşturulamadı: %w", err)
	}
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := d.dropTemp(bg, tmp); err != nil {
			t.Step("⚠ Geçici veritabanı silinemedi (agent bir sonraki açılışta siler): " + err.Error())
			_ = os.MkdirAll(d.RecoveryDir, 0o700)
			_ = os.WriteFile(d.tempMarker(), nil, 0o600)
		}
	}()
	t.Step("… Deneme yüklemesi: " + sourceDB + " → geçici veritabanı (site etkilenmez)")
	src := restoreSource{Path: path, Sec: sec, DB: sourceDB}
	res, err := d.mongorestore(ctx, t, creds, src, nil, true)
	if err != nil {
		return nil, nil, err
	}
	counts, views, err := d.collCounts(ctx, creds)
	if err != nil {
		return nil, nil, err
	}
	for coll, n := range res.collections {
		if contains(views, coll) {
			continue
		}
		if counts[coll] != n {
			return nil, nil, fmt.Errorf("%s: mongorestore %d belge dedi, veritabanında %d var", coll, n, counts[coll])
		}
	}
	t.Step(fmt.Sprintf("✓ Deneme yüklemesi tamam: %d koleksiyon, %d belge%s (%s)", len(counts), sum(counts),
		viewNote(views), usageNote(res.usage)))
	return counts, views, nil
}

func viewNote(views []string) string {
	if len(views) == 0 {
		return ""
	}
	return fmt.Sprintf(", %d görünüm (view)", len(views))
}

func usageNote(u run.Usage) string {
	if u.PeakMem == 0 {
		return took(u.Wall)
	}
	return fmt.Sprintf("%s, en çok %s bellek, %s CPU", took(u.Wall), humanBytes(int64(u.PeakMem)), took(u.CPU))
}

// dropStaging removes a job's own staging directory (tool connection files) when it ends.
func (d *DataOps) dropStaging(t *Task) { _ = d.Staging.Remove(t.ID()) }

func (d *DataOps) mongoSecret() string { return filepath.Join(d.Store.Root, core.MongoSecretPath) }

// mongoData is where MongoDB keeps its files: restores need room there.
func (d *DataOps) mongoData() string { return filepath.Join(d.Store.Root, core.MongoDataDir) }

// dropTemp removes a temporary database and its user.
func (d *DataOps) dropTemp(ctx context.Context, db string) error {
	_, err := rootMongosh(ctx, d.Run, d.mongoSecret(), fmt.Sprintf(
		`const t=c.getDB(%s);if(t.getUser(%s)){t.dropUser(%s)};t.dropDatabase()`, q(db), q(db), q(db)))
	return err
}

// tempMarker exists while a temporary database may be left over (its removal failed).
func (d *DataOps) tempMarker() string { return filepath.Join(d.RecoveryDir, "temp-databases") }

// TempLeft reports whether a temporary database may have been left behind.
func (d *DataOps) TempLeft() bool {
	_, err := os.Stat(d.tempMarker())
	return err == nil
}

// DropTempDatabases removes temporary databases and users left by checks cut short by
// an agent restart. It returns how many databases and users it removed.
func (d *DataOps) DropTempDatabases(ctx context.Context) (string, error) {
	out, err := rootMongosh(ctx, d.Run, d.mongoSecret(), fmt.Sprintf(`const p=%s;let n=0;`+
		`c.getDB("admin").getCollection("system.users").find({db:{$regex:"^"+p}}).toArray().forEach(u=>{c.getDB(u.db).dropUser(u.user);n++});`+
		`c.getDBNames().filter(x=>x.startsWith(p)).forEach(x=>{c.getDB(x).dropDatabase();n++});print("FOOTOPS_DROPPED "+n)`, q(tmpDBPrefix)))
	if err != nil {
		return "", err
	}
	_ = os.Remove(d.tempMarker())
	_, n, _ := strings.Cut(out, "FOOTOPS_DROPPED ")
	return firstLine(n), nil
}

func (d *DataOps) checkPath(uploadID string) string {
	return filepath.Join(d.uploadDir(uploadID), importCheckFile)
}

func (d *DataOps) saveCheck(ic ImportCheck) error {
	b, err := json.Marshal(ic)
	if err != nil {
		return err
	}
	return writeSynced(d.checkPath(ic.Upload), b)
}

// ErrNoCheck means an upload has not been checked (or the check was lost).
var ErrNoCheck = errors.New("bu yükleme için kontrol sonucu yok")

// LoadCheck returns the latest check of an upload of site.
func (d *DataOps) LoadCheck(site, uploadID string) (ImportCheck, error) {
	var ic ImportCheck
	if _, err := d.loadUpload(uploadID); err != nil {
		return ic, err
	}
	b, err := os.ReadFile(d.checkPath(uploadID))
	if errors.Is(err, fs.ErrNotExist) {
		return ic, ErrNoCheck
	}
	if err != nil {
		return ic, err
	}
	if err := json.Unmarshal(b, &ic); err != nil || ic.Upload != uploadID {
		return ImportCheck{}, errors.New("kontrol sonucu bozuk")
	}
	if ic.Site != site {
		return ImportCheck{}, ErrNoUpload
	}
	return ic, nil
}

// ---- restoring ----

// restoreSource is a mongodump archive to load into a site's database.
type restoreSource struct {
	Label string
	Path  string
	Sec   dbSection
	DB    string // database inside the archive
	// Keep lists the collections and views the archive brings: in replace mode
	// everything else is dropped first, so the database ends up equal to the archive.
	Keep []string
	// Expect is the documents per collection once restored (views excluded).
	Expect map[string]int64
	// Size estimates what the restore writes (uncompressed), for the disk check.
	Size int64
}

type restoreResult struct {
	collections map[string]int64 // as mongorestore reported them, views included
	restored    int64
	failed      int64
	usage       run.Usage
}

var (
	// "finished restoring `db.coll` (3 documents, 0 failures)"; older tools omit the backticks.
	reRestored = regexp.MustCompile("finished restoring `?([^`\\s]+)`? \\((\\d+) documents?, (\\d+) failures?\\)")
	// "4 document(s) restored successfully. 0 document(s) failed to restore."
	reRestoreTotal = regexp.MustCompile(`(\d+) document\(s\) restored successfully\. (\d+) document\(s\) failed to restore`)
)

// nsEscape escapes a name for mongorestore's namespace patterns.
func nsEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`).Replace(s)
}

// mongorestore streams src into creds' database. With drop, each collection in the
// archive is dropped before it is restored; exclude names collections to leave out.
// The bytes read are hashed and must match the section's checksum.
func (d *DataOps) mongorestore(ctx context.Context, t *Task, creds dbCreds, src restoreSource, exclude []string, drop bool) (res restoreResult, err error) {
	cfgDir, err := d.toolConfig(t.ID(), creds)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(filepath.Join(cfgDir, "mongo.yaml")) }()
	f, err := os.Open(src.Path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	h := sha256.New()
	in := &progressReader{r: io.TeeReader(io.NewSectionReader(f, src.Sec.Offset, src.Sec.Length), h),
		report: func(n int64) { t.SetBytes(n, src.Sec.Length) }}

	from := nsEscape(src.DB)
	args := []string{"mongorestore", "--config=/run/footops/mongo.yaml", "--archive"}
	if src.Sec.Gzip {
		args = append(args, "--gzip")
	}
	args = append(args, "--nsInclude="+from+".*", "--nsExclude="+from+".system.*")
	for _, c := range exclude {
		args = append(args, "--nsExclude="+from+"."+nsEscape(c))
	}
	if src.DB != creds.DB {
		args = append(args, "--nsFrom="+from+".*", "--nsTo="+nsEscape(creds.DB)+".*")
	}
	args = append(args, "--numParallelCollections=1", "--stopOnError")
	if drop {
		args = append(args, "--drop")
	}
	res.collections = map[string]int64{}
	cmd := d.toolCmdIn(ctx, t.ID(), cfgDir, in, args...)
	cmd.Stderr = func(line string) {
		if m := reRestored.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			fl, _ := strconv.ParseInt(m[3], 10, 64)
			res.collections[strings.TrimPrefix(m[1], creds.DB+".")] = n
			res.failed += fl
		}
		if m := reRestoreTotal.FindStringSubmatch(line); m != nil {
			res.restored, _ = strconv.ParseInt(m[1], 10, 64)
			fl, _ := strconv.ParseInt(m[2], 10, 64)
			res.failed = max(res.failed, fl)
		}
	}
	t.Step("… mongorestore çalışıyor (sınır: " + limitText(d.Limits) + ")")
	res.usage, err = d.Stream.Stream(ctx, cmd)
	t.AddUsage(res.usage)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return res, fmt.Errorf("mongorestore başarısız: %w", err)
	}
	if res.failed > 0 {
		return res, fmt.Errorf("mongorestore %d belgeyi yazamadı", res.failed)
	}
	// The tool may stop at the archive's end marker; the rest still counts for the checksum.
	if _, err := io.Copy(io.Discard, in); err != nil {
		return res, err
	}
	if src.Sec.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != src.Sec.SHA256 {
		return res, errors.New("dosya kontrolden sonra değişmiş (sha256 tutmuyor)")
	}
	return res, nil
}

// collCounts lists a database's collections with their document counts, and its views.
// System collections are left out (a site's user may not even read them).
func (d *DataOps) collCounts(ctx context.Context, c dbCreds) (map[string]int64, []string, error) {
	out, err := mongosh(ctx, d.Run, c.User, c.Password, c.DB, fmt.Sprintf(`const d=c.getDB(%s);const o={counts:{},views:[]};`+
		`d.getCollectionInfos().forEach(i=>{if(i.name.startsWith("system."))return;`+
		`if(i.type==="view"){o.views.push(i.name)}else{o.counts[i.name]=d.getCollection(i.name).countDocuments({})}});`+
		`print("FOOTOPS_JSON "+JSON.stringify(o))`, q(c.DB)))
	if err != nil {
		return nil, nil, fmt.Errorf("koleksiyonlar sayılamadı: %w", err)
	}
	var o struct {
		Counts map[string]int64 `json:"counts"`
		Views  []string         `json:"views"`
	}
	_, js, ok := strings.Cut(out, "FOOTOPS_JSON ")
	if !ok || json.Unmarshal([]byte(strings.TrimSpace(firstLine(js))), &o) != nil {
		return nil, nil, errors.New("koleksiyon sayıları anlaşılamadı")
	}
	if o.Counts == nil {
		o.Counts = map[string]int64{}
	}
	sort.Strings(o.Views)
	return o.Counts, o.Views, nil
}

func (d *DataOps) dropCollections(ctx context.Context, c dbCreds, names []string) error {
	list, _ := json.Marshal(names)
	_, err := mongosh(ctx, d.Run, c.User, c.Password, c.DB,
		fmt.Sprintf(`const d=c.getDB(%s);for(const n of %s){d.getCollection(n).drop()}`, q(c.DB), list))
	return err
}

// load restores src into the site's database and checks the result. It returns the
// counts before and after.
func (d *DataOps) load(ctx context.Context, t *Task, creds dbCreds, src restoreSource, mode string) (before, after map[string]int64, err error) {
	before, views, err := d.collCounts(ctx, creds)
	if err != nil {
		return nil, nil, err
	}
	existing := append(sortedKeys(before), views...)
	var exclude []string
	switch mode {
	case ModeReplace:
		var extra []string
		for _, n := range existing {
			if !contains(src.Keep, n) {
				extra = append(extra, n)
			}
		}
		if len(extra) > 0 {
			if err := d.dropCollections(ctx, creds, extra); err != nil {
				return before, nil, fmt.Errorf("arşivde olmayan koleksiyonlar silinemedi: %w", err)
			}
			t.Step(fmt.Sprintf("✓ Arşivde olmayan %d koleksiyon silindi: %s", len(extra), strings.Join(extra, ", ")))
		}
	case ModeAdd:
		for _, n := range existing {
			if contains(src.Keep, n) {
				exclude = append(exclude, n)
			}
		}
		if len(exclude) > 0 {
			t.Step(fmt.Sprintf("ℹ Sitede zaten olan %d koleksiyona dokunulmuyor: %s", len(exclude), strings.Join(exclude, ", ")))
		}
	default:
		return before, nil, fmt.Errorf("bilinmeyen mod %q", mode)
	}
	res, err := d.mongorestore(ctx, t, creds, src, exclude, mode == ModeReplace)
	if err != nil {
		return before, nil, err
	}
	after, _, err = d.collCounts(ctx, creds)
	if err != nil {
		return before, after, err
	}
	var wrong []string
	for coll, n := range src.Expect {
		want := n
		if contains(exclude, coll) {
			want = before[coll]
		}
		if after[coll] != want {
			wrong = append(wrong, fmt.Sprintf("%s: %d beklenirken %d", coll, want, after[coll]))
		}
	}
	if mode == ModeReplace {
		for coll := range after {
			if _, ok := src.Expect[coll]; !ok {
				wrong = append(wrong, coll+": arşivde yok ama veritabanında var")
			}
		}
	}
	sort.Strings(wrong)
	if len(wrong) > 0 {
		return before, after, fmt.Errorf("sayımlar tutmuyor: %s", strings.Join(wrong, "; "))
	}
	t.Step(fmt.Sprintf("✓ Yüklendi ve sayıldı: %d koleksiyon, %d belge (önce %d belge) — %s",
		len(after), sum(after), sum(before), usageNote(res.usage)))
	return before, after, nil
}

func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// recovery records a restore in progress, so an agent stopped half-way (restart,
// crash, reboot) can put the site back when it starts again.
type recovery struct {
	Job      string    `json:"job"`
	Kind     string    `json:"kind"`
	Site     string    `json:"site"`
	Snapshot string    `json:"snapshot"` // the safety copy taken before
	Running  bool      `json:"running"`  // the site ran before
	Phase    string    `json:"phase"`    // "restoring", then "verified" once the new data is complete
	Actor    Actor     `json:"actor"`
	Started  time.Time `json:"started"`
}

const (
	phasePrepare   = "prepare"      // stopping the site, taking the copy: no data changed yet
	phaseRestoring = "restoring"    // the copy is complete; the database is being replaced
	phaseVerified  = "verified"     // the new data is complete and counted
	phaseRollback  = "rolling-back" // the copy is being put back
)

func (d *DataOps) recoveryPath(job string) string { return filepath.Join(d.RecoveryDir, job+".json") }

func (d *DataOps) saveRecovery(r recovery) error {
	if err := os.MkdirAll(d.RecoveryDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeSynced(d.recoveryPath(r.Job), b)
}

func (d *DataOps) removeRecovery(job string) { _ = os.Remove(d.recoveryPath(job)) }

// Recoveries lists restores an agent stop cut short.
func (d *DataOps) Recoveries() []recovery {
	ents, err := os.ReadDir(d.RecoveryDir)
	if err != nil {
		return nil
	}
	var out []recovery
	for _, e := range ents {
		if !reJobFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.RecoveryDir, e.Name()))
		var r recovery
		if err != nil || json.Unmarshal(b, &r) != nil || r.Job+".json" != e.Name() {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

// snapshotSource describes one of the site's copies as a restore source.
func (d *DataOps) snapshotSource(snap Snapshot) (restoreSource, error) {
	_, path, err := d.SnapshotFile(snap.Site, snap.ID)
	if err != nil {
		return restoreSource{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return restoreSource{}, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return restoreSource{}, fmt.Errorf("kopya okunamadı: %w", err)
	}
	p, err := archive.ReadMongoPrelude(zr)
	if err != nil {
		return restoreSource{}, fmt.Errorf("kopya okunamadı: %w", err)
	}
	src := restoreSource{Label: "kopya " + snap.ID, Path: path, DB: snap.DB, Expect: map[string]int64{}, Size: snap.DataSize,
		Sec: dbSection{Offset: 0, Length: snap.Bytes, Gzip: true, SHA256: snap.SHA256}}
	views := map[string]bool{}
	for _, n := range p.Collections(snap.DB) {
		src.Keep = append(src.Keep, n.Collection)
		views[n.Collection] = n.Type == "view"
	}
	for coll, n := range snap.Collections {
		// System collections (system.js, …) are never restored (rule K4), so not counted.
		if !views[coll] && !strings.HasPrefix(coll, "system.") {
			src.Expect[coll] = n
		}
	}
	return src, nil
}

// apply replaces (or adds to) a site's database from src, inside the safety net: the
// site stops first, so the copy taken next (K1) holds every write; counts are checked;
// the site starts and must answer; any failure restores the copy. A recovery record on
// disk lets the next agent start finish the job if this process dies half-way. It
// returns the copy, which also undoes a successful run.
func (d *DataOps) apply(ctx context.Context, t *Task, site string, src restoreSource, mode, reason string, actor Actor) (Snapshot, error) {
	d.init()
	_, creds, err := d.siteDB(site)
	if err != nil {
		return Snapshot{}, err
	}
	if mode != ModeReplace && mode != ModeAdd {
		return Snapshot{}, fmt.Errorf("bilinmeyen mod %q", mode)
	}
	if err := d.checkFree(d.mongoData(), 2*src.Size); err != nil {
		return Snapshot{}, fmt.Errorf("veritabanı için diskte yer yok, hiçbir şey değiştirilmedi: %w", err)
	}
	running, err := d.Sites.Running(ctx, site)
	if err != nil {
		return Snapshot{}, err
	}
	healthy := false
	if running {
		healthy = d.Sites.Health(ctx, site, 1) == nil
		if !healthy {
			t.Step("ℹ Site şu an da sağlıklı cevap vermiyor; sonradan sağlık kontrolü geri alma sebebi sayılmayacak")
		}
	}
	rec := recovery{Job: t.ID(), Kind: "restore", Site: site, Running: running, Phase: phasePrepare,
		Actor: actor, Started: d.now().UTC()}
	if err := d.saveRecovery(rec); err != nil {
		return Snapshot{}, err
	}
	defer d.removeRecovery(t.ID())
	// Before anything is changed, a failure only has to bring the site back.
	untouched := func(err error) (Snapshot, error) {
		if running {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if serr := d.Sites.Start(bg, site); serr != nil {
				return Snapshot{}, fmt.Errorf("%v; site yeniden başlatılamadı: %v", err, serr)
			}
		}
		return Snapshot{}, err
	}
	if running {
		if err := d.Sites.Stop(ctx, site); err != nil {
			return untouched(err)
		}
		t.Step("✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)")
	} else {
		t.Step("ℹ Site zaten çalışmıyordu; işlemden sonra da başlatılmayacak")
	}
	snap, err := d.Snapshot(ctx, t, site, reason, actor)
	if err != nil {
		return untouched(fmt.Errorf("güvenlik kopyası alınamadı, hiçbir şey değiştirilmedi: %w", err))
	}
	back, err := d.snapshotSource(snap)
	if err != nil {
		return untouched(fmt.Errorf("güvenlik kopyası okunamadı, hiçbir şey değiştirilmedi: %w", err))
	}
	rec.Snapshot, rec.Phase = snap.ID, phaseRestoring
	if err := d.saveRecovery(rec); err != nil {
		return untouched(err)
	}

	t.Step(fmt.Sprintf("… %s yükleniyor (%s)", src.Label, map[string]string{ModeReplace: "tamamen değiştir", ModeAdd: "yalnız olmayan koleksiyonları ekle"}[mode]))
	_, _, err = d.load(ctx, t, creds, src, mode)
	if err == nil {
		rec.Phase = phaseVerified
		err = d.saveRecovery(rec)
	}
	if err == nil && running {
		if err = d.Sites.Start(ctx, site); err == nil {
			t.Step("✓ Site başlatıldı")
			if healthy {
				if err = d.Sites.Health(ctx, site, healthTries); err == nil {
					t.Step("✓ Site sağlıklı cevap veriyor")
				}
			}
		}
	}
	if err == nil {
		t.SetResult(snap.ID)
		return snap, nil
	}

	// Roll back with a fresh context: the job's own may be what ran out. The record says
	// so first: data being rolled back is never "verified" for a recovery.
	t.Step("✗ " + err.Error())
	t.Step("… Geri alınıyor: veritabanı " + snap.ID + " kopyasından eski haline getiriliyor")
	rec.Phase = phaseRollback
	if serr := d.saveRecovery(rec); serr != nil {
		t.Step("⚠ Kurtarma kaydı güncellenemedi: " + serr.Error())
	}
	bg, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if running {
		_ = d.Sites.Stop(bg, site)
	}
	if rerr := d.rollback(bg, t, creds, back); rerr != nil {
		return snap, fmt.Errorf("%v — GERİ ALMA DA BAŞARISIZ: %v. Site durduruldu; panelde Veritabanı kopyaları → %s → Geri yükle ile tekrar dene", err, rerr, snap.ID)
	}
	if running {
		if serr := d.Sites.Start(bg, site); serr != nil {
			return snap, fmt.Errorf("%v — veritabanı eski haline döndü ama site başlatılamadı: %v", err, serr)
		}
	}
	t.Step("✓ Geri alındı: veritabanı işlemden önceki halinde" + map[bool]string{true: ", site çalışıyor", false: ""}[running])
	t.SetResult(snap.ID)
	return snap, fmt.Errorf("%v (geri alındı, veri değişmedi)", err)
}

// rollback restores the safety copy. A tool container left by the failed attempt is
// removed first (it may still be writing, and it holds the job's container name); a
// brief MongoDB outage gets one more try.
func (d *DataOps) rollback(ctx context.Context, t *Task, creds dbCreds, back restoreSource) error {
	var err error
	for try := 0; try < 2; try++ {
		if try > 0 {
			t.Step("… Geri alma tekrar deneniyor: " + err.Error())
		}
		_, _ = d.Run.Run(ctx, "docker", "rm", "--force", toolPrefix+t.ID())
		if werr := d.WaitMongo(ctx); werr != nil {
			err = werr
			continue
		}
		if _, _, err = d.load(ctx, t, creds, back, ModeReplace); err == nil {
			return nil
		}
	}
	return err
}

// ApplyImport applies a checked upload to the site's database.
func (d *DataOps) ApplyImport(ctx context.Context, t *Task, site, uploadID, mode string, actor Actor) (Snapshot, error) {
	d.init()
	defer d.dropStaging(t)
	ic, err := d.LoadCheck(site, uploadID)
	if err != nil {
		return Snapshot{}, err
	}
	if ic.SourceDB == "" || ic.Archive == nil {
		return Snapshot{}, errors.New("önce kontrol tamamlanmalı (veritabanı seçilmedi)")
	}
	u, path, err := d.uploadFile(site, uploadID)
	if err != nil {
		return Snapshot{}, err
	}
	if u.Size != ic.Size {
		return Snapshot{}, errors.New("yüklenen dosya kontrolden sonra değişmiş")
	}
	src := restoreSource{Label: "yüklenen dosya " + ic.File, Path: path, Sec: ic.Section, DB: ic.SourceDB,
		Keep: append(sortedKeys(ic.Archive), ic.Views...), Expect: ic.Archive, Size: ic.DataSize}
	snap, err := d.apply(ctx, t, site, src, mode, "before-import", actor)
	if err == nil {
		if rerr := d.Staging.Remove(uploadPrefix + uploadID); rerr == nil {
			t.Step("✓ Yüklenen dosya sunucudan silindi")
		}
		t.Step(fmt.Sprintf("ℹ Geri almak için: site sayfası → Veritabanı kopyaları → %s → Geri yükle", snap.ID))
	}
	return snap, err
}

// RestoreSnapshot puts one of the site's copies back.
func (d *DataOps) RestoreSnapshot(ctx context.Context, t *Task, site, id string, actor Actor) (Snapshot, error) {
	d.init()
	defer d.dropStaging(t)
	snap, _, err := d.SnapshotFile(site, id)
	if err != nil {
		return Snapshot{}, err
	}
	src, err := d.snapshotSource(snap)
	if err != nil {
		return Snapshot{}, err
	}
	t.Step(fmt.Sprintf("ℹ Geri yüklenecek kopya: %s (%d koleksiyon, %d belge)", snap.ID, len(snap.Collections), snap.Documents))
	before, err := d.apply(ctx, t, site, src, ModeReplace, "before-restore", actor)
	if err == nil {
		t.Step(fmt.Sprintf("ℹ Bu işlemi geri almak için: Veritabanı kopyaları → %s → Geri yükle", before.ID))
	}
	return before, err
}

// Recover finishes what an agent stop interrupted: unless no data had changed yet or
// the new data was already complete, the safety copy is restored; the site is started if
// it ran before. A record is tried once: if this fails, it is removed and the error says
// how to restore by hand, so an old record can never be replayed over newer data later.
func (d *DataOps) Recover(ctx context.Context, t *Task, r recovery) error {
	d.init()
	defer d.dropStaging(t)
	defer d.removeRecovery(r.Job)
	t.Step(fmt.Sprintf("ℹ Agent, %s veritabanı işlemi sürerken durdu (iş %s, %s)", r.Site, r.Job, r.Started.Local().Format("02.01.2006 15:04:05")))
	_, creds, err := d.siteDB(r.Site)
	if err != nil {
		return err
	}
	switch r.Phase {
	case phaseVerified:
		t.Step("ℹ Yeni veri tamamen yüklenmiş ve sayılmıştı; korunuyor")
	case phasePrepare:
		t.Step("ℹ Veritabanına henüz dokunulmamıştı")
	default:
		snap, _, err := d.SnapshotFile(r.Site, r.Snapshot)
		if err != nil {
			return fmt.Errorf("güvenlik kopyası %s bulunamadı: %w — site durduruldu, veritabanı yarım olabilir", r.Snapshot, err)
		}
		back, err := d.snapshotSource(snap)
		if err != nil {
			return err
		}
		if running, _ := d.Sites.Running(ctx, r.Site); running {
			if err := d.Sites.Stop(ctx, r.Site); err != nil {
				return err
			}
		}
		t.Step("… Veritabanı " + snap.ID + " kopyasından eski haline getiriliyor")
		if err := d.rollback(ctx, t, creds, back); err != nil {
			return fmt.Errorf("geri alma başarısız: %w — site durduruldu; panelde Veritabanı kopyaları → %s → Geri yükle", err, snap.ID)
		}
	}
	if r.Running {
		if err := d.Sites.Start(ctx, r.Site); err != nil {
			return err
		}
		t.Step("✓ Site başlatıldı")
	}
	return nil
}

// WaitMongo waits (up to five minutes) until MongoDB answers: after a reboot the agent
// may start before the database container.
func (d *DataOps) WaitMongo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		_, err := rootMongosh(ctx, d.Run, d.mongoSecret(), `c.getDB("admin").runCommand({ping:1})`)
		if err == nil {
			return nil
		}
		if serr := sleepCtx(ctx, 3*time.Second); serr != nil {
			return fmt.Errorf("MongoDB cevap vermiyor: %w", err)
		}
	}
}

// progressReader counts what passes through it and reports every megabyte.
type progressReader struct {
	r      io.Reader
	n      int64
	last   int64
	report func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.report != nil && (p.n-p.last >= 1<<20 || (err == io.EOF && p.n != p.last)) {
		p.last = p.n
		p.report(p.n)
	}
	return n, err
}
