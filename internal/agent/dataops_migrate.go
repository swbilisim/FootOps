package agent

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
)

// Moving a site from another server (F4). The operator registers the old server as a
// source (a key pair of its own, see package remote), the wizard looks around there
// (read-only), the operator picks the database and the uploads folder, and a copy job
// brings both over without passing through anyone's computer: the database as the
// source's own mongodump archive, checked like an upload (rule K2), the folder as a tar
// piped straight into the unprivileged file helper. Applying them is F2's apply: a
// safety copy, the site stops, the data is swapped and counted, the site must answer,
// any failure puts the previous state back.

const (
	DefaultMigrationsDir = "/var/lib/footops/migrations"
	migrationFile        = "migration.json"
	discoveryFile        = "discovery.json"
	scanFile             = "scan.json"
	migratePrefix        = "mig-" // staging of a copy job (the dump)
	dumpFile             = "db.archive.gz"
	keepCopies           = 10
	// maxCollections is more than any site has; a source reporting more is refused.
	maxCollections = 10_000
	maxErr         = 2000 // bytes of an error kept in a record
)

// Migration is a site's move from another server, kept between the wizard's steps.
type Migration struct {
	Site   string `json:"site"`
	Source string `json:"source"` // remote.Source ID
	// AppDir is the app's folder on the source (looked into, not copied).
	AppDir string `json:"app_dir,omitempty"`
	// FilesDir is copied into the site's data directory; "" copies no files.
	FilesDir string `json:"files_dir,omitempty"`
	// DB is the database on the source, in Container ("" = on the host); "" copies none.
	DB        string       `json:"db,omitempty"`
	Container string       `json:"container,omitempty"`
	Created   time.Time    `json:"created"`
	Updated   time.Time    `json:"updated"`
	Actor     string       `json:"actor,omitempty"`
	Copies    []CopyResult `json:"copies,omitempty"` // oldest first, the last keepCopies
}

// LastCopy is the latest copy, if any.
func (m Migration) LastCopy() *CopyResult {
	if len(m.Copies) == 0 {
		return nil
	}
	return &m.Copies[len(m.Copies)-1]
}

// CopyResult is what one copy did, with the counts on both sides.
type CopyResult struct {
	Job      string    `json:"job"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Err      string    `json:"error,omitempty"`
	DB       string    `json:"db,omitempty"`
	// SourceDocs is what mongodump read on the source, per collection; SiteDocs what
	// the site's database holds afterwards.
	SourceDocs map[string]int64 `json:"source_docs,omitempty"`
	SiteDocs   map[string]int64 `json:"site_docs,omitempty"`
	DumpBytes  int64            `json:"dump_bytes,omitempty"`
	FilesDir   string           `json:"files_dir,omitempty"`
	// SourceFiles was counted on the source just before; SiteFiles is the data
	// directory afterwards.
	SourceFiles *remote.Counts `json:"source_files,omitempty"`
	SiteFiles   *datafs.Stats  `json:"site_files,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
	Undo        string         `json:"undo,omitempty"` // as Applied.result()
	Usage       run.Usage      `json:"usage"`
}

// SourceDocTotal and SiteDocTotal sum the collections.
func (c CopyResult) SourceDocTotal() int64 { return sum(c.SourceDocs) }
func (c CopyResult) SiteDocTotal() int64   { return sum(c.SiteDocs) }

// ErrNoMigration means the site has no move under way.
var ErrNoMigration = errors.New("bu site için taşıma başlatılmamış")

func (d *DataOps) migrationDir(site string) string { return filepath.Join(d.MigrationsDir, site) }

// Migration returns the site's move.
func (d *DataOps) Migration(site string) (Migration, error) {
	var m Migration
	if !docker.ValidName(site) {
		return m, ErrNoMigration
	}
	b, err := os.ReadFile(filepath.Join(d.migrationDir(site), migrationFile))
	if errors.Is(err, fs.ErrNotExist) {
		return m, ErrNoMigration
	}
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Site != site {
		return Migration{}, errors.New("taşıma kaydı bozuk")
	}
	return m, nil
}

func (d *DataOps) saveMigration(m Migration) error {
	if err := os.MkdirAll(d.migrationDir(m.Site), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(d.migrationDir(m.Site), migrationFile), append(b, '\n'))
}

func (d *DataOps) saveJSON(site, name string, v any) error {
	if err := os.MkdirAll(d.migrationDir(site), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(d.migrationDir(site), name), b)
}

func (d *DataOps) loadJSON(site, name string, v any) bool {
	b, err := os.ReadFile(filepath.Join(d.migrationDir(site), name))
	return err == nil && json.Unmarshal(b, v) == nil
}

// MigrationChoice is what the operator picked; empty fields are left as they are,
// except that Clear empties the picks of the previous source.
type MigrationChoice struct {
	Source    string `json:"source,omitempty"`
	AppDir    string `json:"app_dir"`
	FilesDir  string `json:"files_dir"`
	DB        string `json:"db"`
	Container string `json:"container"`
	// SourceOnly changes only the source (the other fields are ignored).
	SourceOnly bool `json:"source_only,omitempty"`
}

// SetMigration starts or changes a site's move. Changing the source forgets what was
// found on the old one.
func (d *DataOps) SetMigration(site string, c MigrationChoice, actor string) (Migration, error) {
	spec, err := d.Store.Get(site)
	if err != nil {
		return Migration{}, err
	}
	if !spec.Database && spec.DataMount == "" {
		return Migration{}, errors.New("bu sitenin ne veritabanı ne kalıcı klasörü var; taşınacak veri yok")
	}
	m, err := d.Migration(site)
	switch {
	case errors.Is(err, ErrNoMigration):
		m = Migration{Site: site, Created: d.now().UTC()}
	case err != nil:
		return m, err
	}
	if c.Source != "" && c.Source != m.Source {
		if _, err := d.Sources.Get(c.Source); err != nil {
			return m, err
		}
		m.Source, m.AppDir, m.FilesDir, m.DB, m.Container = c.Source, "", "", "", ""
		_ = os.Remove(filepath.Join(d.migrationDir(site), discoveryFile))
		_ = os.Remove(filepath.Join(d.migrationDir(site), scanFile))
	}
	if m.Source == "" {
		return m, errors.New("önce kaynak sunucuyu seç")
	}
	if !c.SourceOnly {
		var problems []string
		for _, dir := range []string{c.AppDir, c.FilesDir} {
			if dir != "" {
				if err := remote.ValidDir(dir); err != nil {
					problems = append(problems, err.Error())
				}
			}
		}
		if c.DB != "" && !remote.ValidDB(c.DB) {
			problems = append(problems, fmt.Sprintf("veritabanı adı %q kullanılamaz", c.DB))
		}
		if c.Container != "" && !remote.ValidContainer(c.Container) {
			problems = append(problems, fmt.Sprintf("container adı %q geçersiz", c.Container))
		}
		if c.DB != "" && !spec.Database {
			problems = append(problems, "bu sitenin FootOps veritabanı yok; veritabanı taşınamaz (site düzenlenmeli)")
		}
		if c.FilesDir != "" && spec.DataMount == "" {
			problems = append(problems, "bu sitenin kalıcı klasörü yok; dosyalar taşınamaz (site düzenlenmeli)")
		}
		if len(problems) > 0 {
			return m, &remote.ValidationError{Problems: problems}
		}
		if c.AppDir != m.AppDir {
			_ = os.Remove(filepath.Join(d.migrationDir(site), scanFile))
		}
		m.AppDir, m.FilesDir, m.DB, m.Container = c.AppDir, c.FilesDir, c.DB, ""
		if c.DB != "" {
			m.Container = c.Container
		}
	}
	m.Updated, m.Actor = d.now().UTC(), actor
	return m, d.saveMigration(m)
}

// ForgetMigration removes a site's move (its record, discovery and scan). The copies it
// made stay in the site; the source stays registered.
func (d *DataOps) ForgetMigration(site string) error {
	if _, err := d.Migration(site); err != nil {
		return err
	}
	return os.RemoveAll(d.migrationDir(site))
}

// MigrationSaved returns what was found on the source for the site's move.
func (d *DataOps) MigrationSaved(site string) (*remote.Discovery, *remote.Scan) {
	var disc remote.Discovery
	var sc remote.Scan
	dp, sp := &disc, &sc
	if !d.loadJSON(site, discoveryFile, dp) {
		dp = nil
	}
	if !d.loadJSON(site, scanFile, sp) {
		sp = nil
	}
	return dp, sp
}

// migrationSource returns the site's move and its source, which must have been
// connected to (its host key is pinned).
func (d *DataOps) migrationSource(site string) (Migration, remote.Source, error) {
	m, err := d.Migration(site)
	if err != nil {
		return m, remote.Source{}, err
	}
	src, err := d.Sources.Get(m.Source)
	if err != nil {
		return m, src, fmt.Errorf("kaynak sunucu: %w", err)
	}
	if !src.Pinned() {
		return m, src, errors.New("kaynak sunucuya henüz bağlanılmadı: önce “Bağlantıyı dene”")
	}
	return m, src, nil
}

func sourceText(src remote.Source) string {
	return fmt.Sprintf("%s (%s@%s:%d)", src.Label, src.User, src.Host, src.Port)
}

// Discover looks around the source (read-only) and keeps what it found.
func (d *DataOps) Discover(ctx context.Context, t *Task, site string) (remote.Discovery, error) {
	d.init()
	_, src, err := d.migrationSource(site)
	if err != nil {
		return remote.Discovery{}, err
	}
	t.Step("… Kaynak taranıyor (salt okunur): " + sourceText(src))
	disc, err := d.Sources.Discover(ctx, src)
	if err != nil {
		return disc, err
	}
	sy := disc.System
	t.Step(fmt.Sprintf("✓ Sistem: %s, %d çekirdek, %s bellek (%s boş)", orDash(sy.OS), sy.CPUs, humanBytes(sy.MemTotal), humanBytes(sy.MemAvail)))
	dbs := 0
	for _, m := range disc.Mongo {
		dbs += len(m.DBs)
		where := "sunucuda"
		if m.Via == "docker" {
			where = "container " + m.Container
		}
		if m.Err != "" {
			t.Step(fmt.Sprintf("⚠ MongoDB (%s): %s", where, m.Err))
		} else {
			t.Step(fmt.Sprintf("✓ MongoDB %s (%s): %d veritabanı", orDash(m.Version), where, len(m.DBs)))
		}
	}
	if len(disc.Mongo) == 0 {
		t.Step("ℹ MongoDB bulunamadı (mongo / mongosh yok ya da container'da değil)")
	}
	t.Step(fmt.Sprintf("✓ nginx: %d site · node süreçleri: %d · container: %d · yükleme klasörü adayı: %d",
		len(disc.Web), len(disc.Procs), len(disc.Containers), len(disc.Candidates)))
	if err := d.saveJSON(site, discoveryFile, disc); err != nil {
		return disc, err
	}
	t.Step("✓ Keşif kaydedildi; taşıma sayfasında seçimleri yap")
	return disc, nil
}

// ScanApp looks into the app's folder on the source (read-only) and keeps what it found.
func (d *DataOps) ScanApp(ctx context.Context, t *Task, site string) (remote.Scan, error) {
	d.init()
	m, src, err := d.migrationSource(site)
	if err != nil {
		return remote.Scan{}, err
	}
	if m.AppDir == "" {
		return remote.Scan{}, errors.New("uygulama klasörü seçilmedi")
	}
	t.Step("… Uygulama klasörü inceleniyor (salt okunur): " + m.AppDir)
	sc, err := d.Sources.ScanDir(ctx, src, m.AppDir)
	if err != nil {
		return sc, err
	}
	t.Step(fmt.Sprintf("✓ %s: %d dosya, %s", sc.Dir, sc.Total.Files, humanBytes(sc.Total.Bytes)))
	if sc.Package != nil {
		t.Step(fmt.Sprintf("✓ package.json: %s (%s)", orDash(sc.Package.Name), orDash(strings.Join(sc.Package.Known(), ", "))))
	}
	if len(sc.Secrets) > 0 {
		t.Step(fmt.Sprintf("⚠ Kodda sır olabilecek %d yer var (yalnız dosya:satır alındı, değerler alınmadı)", len(sc.Secrets)))
	}
	if len(sc.URLs) > 0 {
		t.Step(fmt.Sprintf("⚠ Kodda IP ya da localhost içeren %d adres var: taşınınca bozulabilir", len(sc.URLs)))
	}
	if err := d.saveJSON(site, scanFile, sc); err != nil {
		return sc, err
	}
	return sc, nil
}

// MigrateCopy brings the chosen database and folder from the source into the site
// (the pre-copy: the site still runs there). See the package comment above.
func (d *DataOps) MigrateCopy(ctx context.Context, t *Task, site string, actor Actor) (res CopyResult, err error) {
	d.init()
	defer d.dropStaging(t)
	defer func() { _ = d.Staging.Remove(migratePrefix + t.ID()) }()
	m, src, err := d.migrationSource(site)
	if err != nil {
		return res, err
	}
	spec, err := d.Store.Get(site)
	if err != nil {
		return res, err
	}
	res = CopyResult{Job: t.ID(), Started: d.now().UTC(), DB: m.DB, FilesDir: m.FilesDir}
	defer func() {
		res.Finished, res.Usage = d.now().UTC(), t.Usage()
		if err != nil {
			res.Err = clipMsg(err.Error(), maxErr)
		}
		// The record is the wizard's memory; a failure to keep it does not undo the copy.
		if cur, lerr := d.Migration(site); lerr == nil {
			cur.Copies = append(cur.Copies, res)
			if len(cur.Copies) > keepCopies {
				cur.Copies = cur.Copies[len(cur.Copies)-keepCopies:]
			}
			if serr := d.saveMigration(cur); serr != nil {
				t.Step("⚠ Taşıma kaydı güncellenemedi: " + serr.Error())
			}
		}
	}()
	switch {
	case m.DB == "" && m.FilesDir == "":
		return res, errors.New("taşınacak bir şey seçilmedi (veritabanı ya da dosya klasörü)")
	case m.DB != "" && !spec.Database:
		return res, errors.New("bu sitenin FootOps veritabanı yok; veritabanı taşınamaz")
	case m.FilesDir != "" && spec.DataMount == "":
		return res, errors.New("bu sitenin kalıcı klasörü yok; dosyalar taşınamaz")
	}
	t.Step("ℹ Kaynak: " + sourceText(src) + " — oradaki site çalışmaya devam eder, yalnız okunur")
	plan := restorePlan{Reason: "before-migrate"}

	var counts remote.Counts
	if m.FilesDir != "" {
		if counts, err = d.Sources.Count(ctx, src, m.FilesDir); err != nil {
			return res, err
		}
		res.SourceFiles = &counts
		note := ""
		if n := counts.Skipped(); n > 0 {
			note = fmt.Sprintf("; alınmayacak: %d gizli, %d bağlantı, %d özel öğe", counts.Hidden, counts.Links, counts.Other)
		}
		t.Step(fmt.Sprintf("✓ Kaynakta %s: %d dosya, %d klasör, %s%s", m.FilesDir, counts.Files, counts.Dirs, humanBytes(counts.Bytes), note))
		l := archive.DefaultLimits
		if counts.Files+counts.Dirs > int64(l.MaxFiles) || counts.Bytes > l.MaxTotal {
			return res, fmt.Errorf("klasör sınırı aşıyor (en çok %d dosya ve %s); önce kaynakta temizlik gerekir", l.MaxFiles, humanBytes(l.MaxTotal))
		}
	}
	if m.DB != "" {
		db, err := d.fetchDump(ctx, t, src, m, &res)
		if err != nil {
			return res, err
		}
		plan.DB, plan.DBMode = &db, ModeReplace
	}
	if m.FilesDir != "" {
		cmd, err := d.Sources.TarCmd(src, t.ID(), m.FilesDir)
		if err != nil {
			return res, err
		}
		plan.Files = &filesSource{Label: "kaynak " + m.FilesDir, Format: "tar", Stream: &cmd, Live: true,
			Expect: datafs.Stats{Files: int(counts.Files), Dirs: int(counts.Dirs), Bytes: counts.Bytes},
			StreamErr: func(err error, log *remote.StreamLog) error {
				serr := remote.StreamError(err, log, true, true)
				if serr == nil && err != nil {
					res.Notes = append(res.Notes, fmt.Sprintf("Kaynakta %d dosya okunurken değişti ya da silindi (site orada çalışıyor); "+
						"son kopyada, eski uygulama durdurulduktan sonra eşitlenir.", log.TarLive))
				}
				return serr
			}}
		plan.FilesMode = ModeReplace
		t.Step("… Dosyalar kaynaktan doğrudan bu sunucuya akıyor (SSH, tar → yardımcı; site burada çalışırken data.new'a)")
	}
	applied, err := d.apply(ctx, t, site, plan, actor)
	if err != nil {
		return res, err
	}
	res.Undo = applied.result()

	// What the site holds now, next to what the source had.
	if m.DB != "" {
		if _, creds, err := d.siteDB(site); err == nil {
			if after, _, err := d.collCounts(ctx, creds); err == nil {
				res.SiteDocs = after
			}
		}
		if res.SiteDocTotal() != res.SourceDocTotal() {
			res.Notes = append(res.Notes, fmt.Sprintf("Belge sayısı: kaynakta %d, sitede %d.", res.SourceDocTotal(), res.SiteDocTotal()))
		}
	}
	if m.FilesDir != "" {
		if sf, err := d.siteFiles(site); err == nil {
			if st, err := countFiles(sf.Dir); err == nil {
				res.SiteFiles = &st
				if int64(st.Files) != counts.Files || st.Bytes != counts.Bytes {
					res.Notes = append(res.Notes, fmt.Sprintf("Dosyalar: kaynakta %d / %s sayılmıştı, sitede %d / %s (kaynak arada değişmiş olabilir).",
						counts.Files, humanBytes(counts.Bytes), st.Files, humanBytes(st.Bytes)))
				}
			}
		}
	}
	t.Step("✓ Ön kopya tamam: site bu sunucuda kaynaktaki verinin kopyasıyla çalışıyor; DNS hâlâ eski sunucuyu gösteriyor")
	if applied.Snapshot != "" {
		t.Step(fmt.Sprintf("ℹ Veritabanının önceki hali: Veritabanı kopyaları → %s", applied.Snapshot))
	}
	if applied.FilesOld != "" {
		t.Step(fmt.Sprintf("ℹ Kalıcı klasörün önceki hali: Dosya kopyaları → %s", applied.FilesOld))
	}
	return res, nil
}

// fetchDump streams the source's mongodump archive into the staging area and checks it
// like an upload (rule K2): its header and gzip checksum, then a trial restore into a
// temporary database whose counts must equal what mongodump reported.
func (d *DataOps) fetchDump(ctx context.Context, t *Task, src remote.Source, m Migration, res *CopyResult) (restoreSource, error) {
	var est int64 = 64 << 20 // unknown size: a modest reservation, the real size is checked below
	if disc, _ := d.MigrationSaved(m.Site); disc != nil {
		for _, ms := range disc.Mongo {
			for _, db := range ms.DBs {
				if db.Name == m.DB && ms.Container == m.Container {
					est = max(db.DataSize, 1<<20)
				}
			}
		}
	}
	if err := d.Staging.Allow(est); err != nil {
		return restoreSource{}, err
	}
	dir, err := d.Staging.Dir(migratePrefix + t.ID())
	if err != nil {
		return restoreSource{}, err
	}
	path := filepath.Join(dir, dumpFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return restoreSource{}, err
	}
	defer f.Close()
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h), report: func(n int64) { t.SetBytes(n, 0) }}
	// A dump is smaller than its data; past twice the estimate (and the staging rules)
	// the source is sending something else.
	maxDump := max(2*est, 256<<20)
	limit := &limitWriter{w: cw, left: maxDump}
	bw := bufio.NewWriterSize(limit, 256<<10)
	cmd, err := d.Sources.DumpCmd(src, t.ID(), m.Container, m.DB)
	if err != nil {
		return restoreSource{}, err
	}
	// The documents per collection, as mongodump reports them. The source is not
	// trusted: names longer than MongoDB allows or more collections than any site has
	// fail the copy instead of filling the agent's memory.
	counts := map[string]int64{}
	var log remote.StreamLog
	odd := ""
	cmd.Stdout = bw
	cmd.Stderr = func(line string) {
		if mm := reDumped.FindStringSubmatch(line); mm != nil {
			coll := strings.TrimPrefix(mm[1], m.DB+".")
			if _, seen := counts[coll]; !seen && (len(coll) > 255 || len(counts) >= maxCollections) {
				if odd == "" {
					odd = fmt.Sprintf("kaynak beklenmeyen bir koleksiyon listesi bildirdi (ad %d bayt, %d koleksiyon)", len(coll), len(counts))
				}
				return
			}
			n, _ := strconv.ParseInt(mm[2], 10, 64)
			counts[strings.Clone(coll)] = n
			return
		}
		log.Add(line)
	}
	where := "sunucuda"
	if m.Container != "" {
		where = "container " + m.Container
	}
	t.Step(fmt.Sprintf("… Kaynakta mongodump çalışıyor (%s, veritabanı %s) → SSH → bu sunucu", where, m.DB))
	u, err := d.Stream.Stream(ctx, cmd)
	t.AddUsage(u)
	if limit.over {
		return restoreSource{}, fmt.Errorf("kaynağın gönderdiği döküm beklenenden çok büyük (%s üstü); veritabanı büyüdüyse kaynağı yeniden tara",
			humanBytes(maxDump))
	}
	if odd != "" {
		return restoreSource{}, errors.New(odd)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err = remote.StreamError(err, &log, false, false); err != nil {
		return restoreSource{}, fmt.Errorf("kaynakta mongodump başarısız: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return restoreSource{}, err
	}
	if err := f.Sync(); err != nil {
		return restoreSource{}, err
	}
	size, digest := cw.n, hex.EncodeToString(h.Sum(nil))
	res.DumpBytes = size
	t.Step(fmt.Sprintf("✓ Döküm geldi: %d koleksiyon, %d belge → %s (%s)", len(counts), sum(counts), humanBytes(size), usageNote(u)))

	// Rule K2: what came is read back like an upload.
	p, dataSize, err := readMongoSection(ctxReader{ctx, io.NewSectionReader(f, 0, size)}, true, sha256.New())
	if err != nil {
		return restoreSource{}, fmt.Errorf("gelen döküm bozuk: %w", err)
	}
	if !contains(p.Databases(), m.DB) {
		return restoreSource{}, fmt.Errorf("gelen dökümde %s veritabanı yok", m.DB)
	}
	sec := dbSection{Offset: 0, Length: size, Gzip: true, SHA256: digest}
	archived, views, err := d.trialRestore(ctx, t, path, sec, m.DB, dataSize)
	if err != nil {
		return restoreSource{}, err
	}
	res.SourceDocs = map[string]int64{}
	var wrong []string
	for coll, n := range counts {
		if strings.HasPrefix(coll, "system.") || contains(views, coll) {
			continue
		}
		res.SourceDocs[coll] = n
		if archived[coll] != n {
			wrong = append(wrong, fmt.Sprintf("%s: kaynakta %d, denemede %d", coll, n, archived[coll]))
		}
	}
	for coll := range archived {
		if _, ok := res.SourceDocs[coll]; !ok {
			wrong = append(wrong, coll+": kaynak bildirmedi")
		}
	}
	if len(wrong) > 0 {
		sort.Strings(wrong)
		more := ""
		if len(wrong) > 10 {
			wrong, more = wrong[:10], fmt.Sprintf(" … ve %d koleksiyon daha", len(wrong)-10)
		}
		return restoreSource{}, fmt.Errorf("deneme yüklemesi kaynağın sayımlarıyla tutmuyor: %s%s", strings.Join(wrong, "; "), more)
	}
	return restoreSource{Label: "kaynaktan alınan döküm (" + m.DB + ")", Path: path, Sec: sec, DB: m.DB,
		Keep: append(sortedKeys(archived), views...), Expect: archived, Size: dataSize}, nil
}

// limitWriter fails once more than left bytes were written.
type limitWriter struct {
	w    io.Writer
	left int64
	over bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		l.over = true
		return 0, errors.New("döküm sınırı aşıldı")
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// streamInto runs the helper with another server's tar piped into its stdin: the two
// processes are joined by a pipe and the bytes never pass through the agent. The helper
// reads to the end of the stream, so the sender finishes and its exit status says
// whether everything came; a helper that gives up stops the sender.
func (d *DataOps) streamInto(ctx context.Context, t *Task, src filesSource, helper run.Cmd) (run.Usage, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return run.Usage{}, err
	}
	sender := *src.Stream
	sender.Stdout = pw
	var log remote.StreamLog
	sender.Stderr = log.Add
	helper.Stdin = pr
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	type result struct {
		u   run.Usage
		err error
	}
	done := make(chan result, 1)
	go func() {
		u, err := d.Stream.Stream(sctx, sender)
		pw.Close() // the helper sees the end once the sender's own copy is closed as well
		done <- result{u, err}
	}()
	hu, herr := d.Stream.Stream(ctx, helper)
	pr.Close() // a sender still writing now fails instead of waiting forever
	var s result
	select {
	case s = <-done:
	default:
		if herr != nil {
			stop()
			s = <-done
			s.err = nil // stopped because the helper gave up: the helper's error says why
		} else {
			s = <-done
		}
	}
	// The two ran side by side: their memory adds up.
	u := run.Usage{PeakMem: s.u.PeakMem + hu.PeakMem, CPU: s.u.CPU + hu.CPU, Wall: max(s.u.Wall, hu.Wall)}
	t.AddUsage(u)
	judge := src.StreamErr
	if judge == nil {
		judge = func(err error, log *remote.StreamLog) error { return remote.StreamError(err, log, true, src.Live) }
	}
	serr := judge(s.err, &log)
	switch {
	case serr != nil && herr != nil:
		return u, fmt.Errorf("kaynaktan okuma: %v (açarken: %v)", serr, herr)
	case serr != nil:
		return u, fmt.Errorf("kaynaktan okuma: %w", serr)
	}
	return u, herr
}
