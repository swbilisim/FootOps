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
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
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
	FormatFiles     = "files" // a zip, tar or tar.gz of files
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
	// HasDB: the upload holds a mongodump archive (Section, DataSize describe it).
	HasDB    bool      `json:"has_db"`
	Section  dbSection `json:"section"`
	DataSize int64     `json:"data_size"` // the database archive uncompressed
	// Files describes files to put into the site's data directory, nil if none.
	Files *FilesCheck `json:"files,omitempty"`
	// Notes are things the operator should know (a part that cannot be used here).
	Notes []string `json:"notes,omitempty"`

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

// FilesCheck is the files part of an upload.
type FilesCheck struct {
	Format  string          `json:"format"` // "zip", "tar", "tgz"
	Section dbSection       `json:"section"`
	Archive archive.Summary `json:"archive"`
	Current datafs.Stats    `json:"current"` // the site's data directory at the time of the check
	// StripDefault suggests removing the archive's single top-level folder (a zipped
	// "uploads" folder) because the site has no folder of that name.
	StripDefault bool `json:"strip_default"`
}

// Expect is what extracting the archive produces (hidden entries are left out).
func (f FilesCheck) Expect() datafs.Stats {
	return datafs.Stats{Files: f.Archive.Files - f.Archive.Hidden, Bytes: f.Archive.Size - f.Archive.HiddenBytes}
}

// siteControl stops and starts sites around data work. *SiteOps implements it.
type siteControl interface {
	Running(ctx context.Context, name string) (bool, error)
	Stop(ctx context.Context, name string) error
	Start(ctx context.Context, name string) error
	Health(ctx context.Context, name string, tries int) error
}

// ---- checking ----

// CheckImport checks an upload for a site: its database part, its files part, or both.
// sourceDB picks the database when the archive holds several; empty chooses the only one
// or leaves the choice to the operator. The site is not touched.
func (d *DataOps) CheckImport(ctx context.Context, t *Task, site, uploadID, sourceDB string) (ic ImportCheck, err error) {
	d.init()
	defer d.dropStaging(t)
	spec, err := d.Store.Get(site)
	if err != nil {
		return ic, err
	}
	u, path, err := d.uploadFile(spec.Name, uploadID)
	if err != nil {
		return ic, err
	}
	t.Step(fmt.Sprintf("✓ Dosya: %s (%s)", u.Name, humanBytes(u.Size)))
	ic = ImportCheck{Upload: u.ID, Job: t.ID(), Site: spec.Name, File: u.Name, Size: u.Size}

	t.Step("… Dosya okunuyor: tür, bütünlük, içindekiler")
	p, err := d.inspect(ctx, t, path, &ic)
	if err != nil {
		return ic, err
	}
	what := map[string]string{FormatFootOps: "FootOps arşivi", FormatMongodump: "mongodump arşivi", FormatFiles: "dosya arşivi"}[ic.Format]
	if ic.Manifest != nil {
		what += fmt.Sprintf(" (%s sitesinden, %s)", ic.Manifest.Site, ic.Manifest.Created.Local().Format("02.01.2006 15:04"))
	}
	t.Step("✓ " + what + ", bütünlük tamam")

	if ic.Files != nil {
		if err := d.checkFilesPart(t, spec, &ic); err != nil {
			return ic, err
		}
	}
	if ic.HasDB && !spec.Database {
		ic.HasDB = false
		ic.Notes = append(ic.Notes, "Arşivde veritabanı var ama bu sitenin FootOps veritabanı yok; yalnız dosyalar aktarılabilir.")
	}
	if !ic.HasDB && ic.Files == nil {
		return ic, errors.New("bu siteye aktarılabilecek bir şey yok")
	}
	if ic.HasDB {
		done, err := d.checkDBPart(ctx, t, path, p, sourceDB, &ic)
		if err != nil || !done {
			return ic, err
		}
	}
	ic.Usage, ic.Checked = t.Usage(), d.now().UTC()
	if err := d.saveCheck(ic); err != nil {
		return ic, err
	}
	t.SetResult(ic.Upload)
	t.Step("✓ Önizleme hazır")
	return ic, nil
}

// checkFilesPart compares the files part with the site's data directory.
func (d *DataOps) checkFilesPart(t *Task, spec sites.Spec, ic *ImportCheck) error {
	a := ic.Files.Archive
	t.Step(fmt.Sprintf("✓ Dosyalar: %d dosya, %d klasör, %s%s", a.Files-a.Hidden, a.Dirs, humanBytes(a.Size-a.HiddenBytes),
		map[bool]string{true: fmt.Sprintf(" (%d gizli dosya alınmaz)", a.Hidden), false: ""}[a.Hidden > 0]))
	sf, err := d.siteFiles(spec.Name)
	if err != nil {
		if ic.HasDB && spec.Database {
			ic.Files = nil
			ic.Notes = append(ic.Notes, "Arşivde dosyalar var ama bu sitenin kalıcı klasörü yok; yalnız veritabanı aktarılabilir.")
			return nil
		}
		return err
	}
	if a.BSON > 0 {
		ic.Notes = append(ic.Notes, fmt.Sprintf("Arşivde %d .bson dosyası var: bu bir mongodump klasörü olabilir. Klasör halindeki "+
			"veritabanı yedeği henüz içe aktarılamıyor; mongodump --archive=dosya.gz --gzip ile tek dosya üret.", a.BSON))
	}
	if ic.Files.Current, err = countFiles(sf.Dir); err != nil {
		return fmt.Errorf("kalıcı klasör sayılamadı: %w", err)
	}
	if a.Prefix != "" && ic.Format != FormatFootOps {
		_, serr := os.Lstat(filepath.Join(sf.Dir, a.Prefix))
		ic.Files.StripDefault = serr != nil
	}
	return nil
}

// checkDBPart chooses the database and restores it into a temporary one to count it.
// done is false when the operator must choose the database first (the check is saved).
func (d *DataOps) checkDBPart(ctx context.Context, t *Task, path string, p archive.MongoPrelude, sourceDB string, ic *ImportCheck) (bool, error) {
	_, creds, err := d.siteDB(ic.Site)
	if err != nil {
		return false, err
	}
	ic.Target = creds.DB
	ic.ServerVersion, ic.ToolVersion = p.ServerVersion, p.ToolVersion
	ic.Databases = importable(p.Databases())
	t.Step(fmt.Sprintf("✓ Veritabanı arşivi: MongoDB %s, araç %s; veritabanları: %s",
		orDash(p.ServerVersion), orDash(p.ToolVersion), orDash(strings.Join(ic.Databases, ", "))))
	if len(ic.Databases) == 0 {
		return false, errors.New("arşivde içe aktarılabilecek bir veritabanı yok (admin, config, local alınmaz)")
	}
	switch {
	case sourceDB != "":
		if !contains(ic.Databases, sourceDB) {
			return false, fmt.Errorf("arşivde %q veritabanı yok", sourceDB)
		}
	case len(ic.Databases) == 1:
		sourceDB = ic.Databases[0]
	}
	if ic.Current, ic.CurrentViews, err = d.collCounts(ctx, creds); err != nil {
		return false, err
	}
	ic.CurrentDocs = sum(ic.Current)
	if sourceDB == "" {
		ic.Checked = d.now().UTC()
		if err := d.saveCheck(*ic); err != nil {
			return false, err
		}
		t.SetResult(ic.Upload)
		t.Step(fmt.Sprintf("ℹ Arşivde %d veritabanı var: önizleme sayfasında hangisinin içe aktarılacağını seç", len(ic.Databases)))
		return false, nil
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
	if ic.Archive, ic.Views, err = d.trialRestore(ctx, t, path, ic.Section, sourceDB, ic.DataSize); err != nil {
		return false, err
	}
	ic.ArchiveDocs = sum(ic.Archive)
	t.Step(fmt.Sprintf("✓ Veritabanı: arşivde %d koleksiyon, %d belge; sitede şu an %d koleksiyon, %d belge",
		len(ic.Archive), ic.ArchiveDocs, len(ic.Current), ic.CurrentDocs))
	return true, nil
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
		ic.DataSize, ic.HasDB = size, true
		ic.SHA256 = hex.EncodeToString(h.Sum(nil))
		ic.Section = dbSection{Offset: 0, Length: ic.Size, Gzip: kind == archive.MongoArchiveGz, SHA256: ic.SHA256}
		return p, nil
	case archive.Tar:
		ic.Format = FormatFootOps
		p, err := inspectFootOps(ctx, f, ic, progress)
		if errors.Is(err, errPlainTar) {
			*ic = ImportCheck{Upload: ic.Upload, Job: ic.Job, Site: ic.Site, File: ic.File, Size: ic.Size}
			return p, d.inspectFiles(ctx, path, "tar", ic)
		}
		return p, err
	case archive.TarGz:
		return archive.MongoPrelude{}, d.inspectFiles(ctx, path, "tgz", ic)
	case archive.Zip:
		return archive.MongoPrelude{}, d.inspectFiles(ctx, path, "zip", ic)
	}
	return archive.MongoPrelude{}, fmt.Errorf("bu dosya tanınmadı (tür: %s). Kabul edilenler: FootOps arşivi (.footops.tar), "+
		"mongodump arşivi (mongodump --archive=dosya.gz --gzip) ve dosya arşivi (zip, tar.gz)", orDash(string(kind)))
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
		if len(members) == 0 && name != memberManifest && name != memberDB && name != memberFiles {
			return p, errPlainTar
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
			s, err := checkFilesMember(io.TeeReader(tr, mh), h.Size)
			if err != nil {
				return p, fmt.Errorf("%s: %w", memberFiles, err)
			}
			ic.Files = &FilesCheck{Format: "tgz", Archive: s}
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
	ic.Manifest = man
	if db, ok := members[memberDB]; ok {
		ic.HasDB = true
		ic.Section = dbSection{Offset: db.offset, Length: db.size, Gzip: true, SHA256: db.sha256}
	}
	if fm, ok := members[memberFiles]; ok {
		ic.Files.Section = dbSection{Offset: fm.offset, Length: fm.size, Gzip: true, SHA256: fm.sha256}
	}
	if !ic.HasDB && ic.Files == nil {
		return p, errors.New("FootOps arşivinde ne veritabanı ne dosya var")
	}
	return p, nil
}

// errPlainTar: a tar that is not a FootOps archive (taken as an archive of files).
var errPlainTar = errors.New("FootOps arşivi değil")

// checkFilesMember checks a FootOps archive's files.tar.gz member like archive.Check
// checks a whole file: integrity, names, limits, compression ratio.
func checkFilesMember(r io.Reader, size int64) (archive.Summary, error) {
	s := archive.Summary{Kind: archive.TarGz, Compressed: size}
	zr, err := gzip.NewReader(bufio.NewReaderSize(r, 256<<10))
	if err != nil {
		return s, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	l := archive.DefaultLimits
	if err := archive.CheckTar(zr, l, &s); err != nil {
		return s, err
	}
	// The tar's padding and the gzip checksum follow; nothing much else may.
	n, err := io.Copy(io.Discard, io.LimitReader(zr, 1<<20+1))
	if err != nil {
		return s, fmt.Errorf("gzip bozuk: %w", err)
	}
	if n > 1<<20 {
		return s, errors.New("tar'ın sonundan sonra beklenmeyen veri var")
	}
	return s, archive.CheckRatio(s.Size, size, l)
}

// inspectFiles checks a whole upload that is an archive of files (zip, tar, tar.gz).
func (d *DataOps) inspectFiles(ctx context.Context, path, format string, ic *ImportCheck) error {
	ic.Format = FormatFiles
	s, err := archive.Check(path, archive.DefaultLimits)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(ctxReader{ctx, f}, 256<<10)); err != nil {
		return err
	}
	ic.SHA256 = hex.EncodeToString(h.Sum(nil))
	ic.Files = &FilesCheck{Format: format, Archive: s,
		Section: dbSection{Offset: 0, Length: ic.Size, Gzip: format == "tgz", SHA256: ic.SHA256}}
	return nil
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
	// Registered first: a user whose creation answer was lost is removed too.
	defer func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := d.mongoOps().DropTemp(bg, tmp); err != nil {
			t.Step("⚠ Geçici veritabanı silinemedi (agent bir sonraki açılışta siler): " + err.Error())
			_ = os.MkdirAll(d.RecoveryDir, 0o700)
			_ = os.WriteFile(d.tempMarker(), nil, 0o600)
		}
	}()
	if err := d.mongoOps().SetUser(ctx, tmp, tmp, pw); err != nil {
		return nil, nil, fmt.Errorf("geçici veritabanı kullanıcısı oluşturulamadı: %w", err)
	}
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
	n, err := d.mongoOps().DropTemps(ctx)
	if err != nil {
		return "", err
	}
	_ = os.Remove(d.tempMarker())
	return strconv.Itoa(n), nil
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
	counts, views, err := d.mongoOps().Counts(ctx, c)
	if err != nil {
		return nil, nil, fmt.Errorf("koleksiyonlar sayılamadı: %w", err)
	}
	return counts, views, nil
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
			if err := d.mongoOps().DropCollections(ctx, creds, extra); err != nil {
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

// WaitMongo waits (up to five minutes) until MongoDB answers: after a reboot the agent
// may start before the database container.
func (d *DataOps) WaitMongo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		// One try may not take the whole wait: a MongoDB that hangs is asked again.
		try, stop := context.WithTimeout(ctx, 15*time.Second)
		err := d.mongoOps().Ping(try)
		stop()
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
