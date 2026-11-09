package mac

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/version"
)

// Making a FootOps archive on the Mac (panel-tasarim §29.6): a database of the Mac's
// MongoDB (mongodump --archive --gzip, as the panel's export) and / or a folder that is
// to be the site's persistent one (datafs.Pack: what the panel's export packs and how),
// written by internal/archive and read back as the panel reads an upload before it is
// given its name. The panel's İçe aktar takes it; the manifest's site is only a note —
// the site imported into is the one the panel's page is of.

// "done dumping db.coll (N documents)" (the agent's reDumped).
var reDumped = regexp.MustCompile("done dumping `?([^`\\s]+)`? \\((\\d+) documents?\\)")

// createReq is what arsiv olustur is asked.
type createReq struct {
	db, folder, site, target string
}

func createArgs(args []string) (createReq, error) {
	pos, vals, _, err := options(args, []string{"--db", "--dosyalar", "--site", "--hedef"}, nil)
	if err != nil {
		return createReq{}, err
	}
	if len(pos) > 0 {
		return createReq{}, errUsage
	}
	req := createReq{db: vals["--db"], folder: vals["--dosyalar"], site: vals["--site"], target: vals["--hedef"]}
	switch {
	case req.db == "" && req.folder == "":
		return req, fmt.Errorf("%w: --db ya da --dosyalar (ya da ikisi) gerekli", errUsage)
	case req.site != "" && !reSiteName.MatchString(req.site):
		return req, fmt.Errorf("geçersiz site adı: %q (küçük harf, rakam ve -; 3-32 karakter, harfle başlar)", clip(req.site, 40))
	}
	return req, nil
}

// trPlain is a Turkish letter's plain one, for a site's name.
var trPlain = map[rune]rune{'ç': 'c', 'ğ': 'g', 'ı': 'i', 'ö': 'o', 'ş': 's', 'ü': 'u'}

// siteFrom makes a site's name of a database's or a folder's: lower case, Turkish letters
// plain, other marks as -; "site" when nothing usable is left.
func siteFrom(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case trPlain[r] != 0:
			b.WriteRune(trPlain[r])
		case r >= 0x300 && r <= 0x36f: // a combining mark (İ lowered: i + dot): none
		default:
			if s := b.String(); s != "" && !strings.HasSuffix(s, "-") {
				b.WriteByte('-')
			}
		}
	}
	s := strings.TrimLeft(b.String(), "0123456789-")
	if len(s) > 32 {
		s = s[:32]
	}
	s = strings.TrimRight(s, "-")
	if !reSiteName.MatchString(s) {
		return "site"
	}
	return s
}

// CreateResult is arsiv olustur's answer, and (without Archive) its start: the archive
// made and what checking it as an upload found. Skipped: the folder's entries left out —
// hidden files, links, names an import refuses — as the panel's export leaves them out.
type CreateResult struct {
	Path     string       `json:"path"`
	Site     string       `json:"site"`
	DB       string       `json:"db,omitempty"`
	Folder   string       `json:"folder,omitempty"`
	Archive  *ArchiveInfo `json:"archive,omitempty"`
	Skipped  int          `json:"skipped,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
	TookMS   int64        `json:"took_ms"`
}

// createArchive is arsiv olustur.
func (e *env) createArchive(ctx context.Context, req createReq, jsonOut bool) error {
	start := time.Now()
	now := e.now()
	res := CreateResult{DB: req.db, Site: req.site}
	need := int64(spareRoom)

	// The database there, and mongodump to read it.
	var mt mongoTarget
	var tool string
	if req.db != "" {
		var err error
		if tool, err = e.look("mongodump"); err != nil {
			return errors.New("mongodump bulunamadı: brew install mongodb-database-tools")
		}
		if mt, err = parseMongoURI(e.mongoURI); err != nil {
			return err
		}
		dbs, err := mt.databases(ctx)
		if err != nil {
			return err
		}
		size, ok := dbs[req.db]
		if !ok || len(loadableDBs([]string{req.db})) == 0 {
			return fmt.Errorf("Mac'teki MongoDB'de %q adlı veritabanı yok (olanlar: footops-mac arsiv veritabanlari)", clip(clean(req.db), 64))
		}
		need += size
	}
	// The folder, counted first: what the archive is to hold.
	var root *os.Root
	var counted datafs.Stats
	if req.folder != "" {
		p, err := filepath.Abs(expandHome(req.folder))
		if err != nil {
			return err
		}
		fi, err := os.Stat(p)
		switch {
		case err != nil:
			return readErr(p, err)
		case !fi.IsDir():
			return fmt.Errorf("%s bir klasör değil", p)
		}
		if root, err = os.OpenRoot(p); err != nil {
			return readErr(p, err)
		}
		defer root.Close()
		res.Folder = p
		if counted, err = datafs.Count(root); err != nil {
			return fmt.Errorf("%s okunamadı: %v", p, err)
		}
		need += counted.Bytes
		if n := decomposedNames(root); n > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d dosya ya da klasör adında ayrışık harf var (macOS'un \"ü\"yü u + ¨ diye yazdığı biçim): "+
				"Linux'ta bu adlar birleşik yazılmış adla aynı değildir; site o dosyaları birleşik adla arıyorsa bulamaz.", n))
		}
	}
	if res.Site == "" {
		if req.db != "" {
			res.Site = siteFrom(req.db)
		} else {
			res.Site = siteFrom(filepath.Base(res.Folder))
		}
	}
	target, err := e.createTarget(req.target, res.Site, now)
	if err != nil {
		return err
	}
	if res.Folder != "" && inside(filepath.Dir(target), res.Folder) {
		return fmt.Errorf("arşiv, paketlenen klasörün içine yazılamaz (yarısı kendi içine girer): %s — başka bir --hedef ver ya da başka bir klasör seç",
			filepath.Dir(target))
	}
	res.Path = target
	if err := e.roomFor(filepath.Dir(target), need); err != nil {
		return err
	}
	part := target + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return folderErr(part, err)
	}
	named := false
	defer func() {
		f.Close()
		if !named {
			os.Remove(part)
		}
	}()

	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			CreateResult
		}{"start", res})
	} else {
		fmt.Fprintf(e.out, "Arşiv üretiliyor: %s\n", target)
	}
	prog.beat()

	mod := now.UTC().Truncate(time.Second)
	aw := archive.NewWriter(f)
	man := archive.Manifest{FootOps: version.Version, Site: res.Site, Created: mod}
	if req.db != "" {
		e.say(jsonOut, "  … veritabanı: mongodump %s (%s)\n", req.db, mt.label())
		counts, m, err := e.dumpDB(ctx, aw, mt, tool, req.db, mod, prog)
		if err != nil {
			return err
		}
		// Rule K2 for our own output: the member reads back as an intact mongodump archive.
		p, _, err := archive.ReadMongo(ctxReader{ctx, io.NewSectionReader(f, m.Offset, m.Size)}, true, sha256.New())
		if err != nil {
			return fmt.Errorf("döküm doğrulanamadı: %w", err)
		}
		if !slices.Contains(p.Databases(), req.db) {
			return fmt.Errorf("%s veritabanında koleksiyon yok: döküm boş", req.db)
		}
		views := map[string]bool{}
		for _, n := range p.Collections(req.db) {
			views[n.Collection] = n.Type == "view"
		}
		man.DB, man.MongoServer, man.MongoTool, man.Collections = req.db, p.ServerVersion, "mongodump "+p.ToolVersion, map[string]int64{}
		for c, n := range counts {
			if !views[c] && !strings.HasPrefix(c, "system.") {
				man.Collections[c] = n
				man.Documents += n
			}
		}
		e.say(jsonOut, "  ✓ veritabanı: %d koleksiyon, %d belge → %s\n", len(man.Collections), man.Documents, bytesTR(m.Size))
	}
	if root != nil {
		e.say(jsonOut, "  … dosyalar: %d dosya, %s\n", counted.Files, bytesTR(counted.Bytes))
		var st datafs.Stats
		m, err := aw.Add(archive.MemberFiles, mod, nil, func(w io.Writer) error {
			var err error
			st, err = datafs.Pack(root, ctxWriter{ctx, w}, datafs.Options{Progress: func(s datafs.Stats) { prog.report(partFiles, s.Bytes, counted.Bytes) }})
			return err
		})
		if ctx.Err() != nil {
			return errStopped
		}
		if err != nil {
			return fmt.Errorf("dosyalar paketlenemedi: %w", err)
		}
		// Read back like an upload: names, sizes, gzip checksum.
		s, err := archive.CheckFilesMember(ctxReader{ctx, io.NewSectionReader(f, m.Offset, m.Size)}, m.Size)
		if err != nil {
			return fmt.Errorf("dosya arşivi doğrulanamadı: %w", err)
		}
		if s.Files != st.Files || s.Size != st.Bytes {
			return fmt.Errorf("dosya arşivi tutmuyor: %d dosya / %d bayt, paketlenen %d / %d", s.Files, s.Size, st.Files, st.Bytes)
		}
		man.Files = &archive.ManifestFiles{Files: st.Files, Dirs: st.Dirs, Bytes: st.Bytes}
		res.Skipped = st.Skipped
		e.say(jsonOut, "  ✓ dosyalar: %d dosya, %d klasör, %s → %s\n", st.Files, st.Dirs, bytesTR(st.Bytes), bytesTR(m.Size))
	}
	if err := aw.Finish(man, mod); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// The whole read back as the panel reads an upload, then named.
	k, err := e.checkArchive(ctx, part, func(done, total int64) { prog.report("check", done, total) })
	if err != nil {
		return fmt.Errorf("üretilen arşiv denetimden geçmedi: %w", err)
	}
	if err := settle(part, target); err != nil {
		return err
	}
	named = true
	k.info.Path, k.info.Name = target, filepath.Base(target)
	res.Archive = &k.info
	res.TookMS = time.Since(start).Milliseconds()
	prog.end()
	if jsonOut {
		e.event(struct {
			Event string `json:"event"`
			CreateResult
		}{"result", res})
		return nil
	}
	printCreated(e.out, res, e.panels())
	return nil
}

// dumpDB writes the database's dump as the archive's database member: mongodump
// --archive --gzip one collection at a time, as the panel's export runs it; the counts are
// mongodump's own, per collection.
func (e *env) dumpDB(ctx context.Context, aw *archive.Writer, mt mongoTarget, tool, db string, mod time.Time, prog *progressor) (map[string]int64, archive.MemberAt, error) {
	conn, done, err := e.toolArgs(mt)
	if err != nil {
		return nil, archive.MemberAt{}, err
	}
	defer done()
	counts := map[string]int64{}
	said := &tail{max: 16 << 10}
	m, err := aw.Add(archive.MemberDB, mod, func(n int64) { prog.report(partDB, n, 0) }, func(w io.Writer) error {
		cmd := exec.CommandContext(ctx, tool, append(conn, "--db="+db, "--archive", "--gzip", "--numParallelCollections=1")...)
		cmd.Stdout = w
		lines := &lineWriter{on: func(line []byte) {
			if m := reDumped.FindSubmatch(line); m != nil {
				n, _ := strconv.ParseInt(string(m[2]), 10, 64)
				counts[strings.TrimPrefix(string(m[1]), db+".")] = n
			}
			said.Write(append(line, '\n'))
		}}
		cmd.Stderr = lines
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 10 * time.Second
		err := cmd.Run()
		lines.flush()
		switch {
		case ctx.Err() != nil:
			return errStopped
		case err != nil:
			return fmt.Errorf("mongodump başarısız: %s", toolWords(mt.masked(said.String()), err.Error()))
		}
		return nil
	})
	if ctx.Err() != nil {
		return nil, m, errStopped
	}
	return counts, m, err
}

// createTarget is where a made archive goes: the path asked for (a folder: into it), or
// ~/Downloads/<site>-<time>.footops.tar (-2, -3… when taken). Never a file that is there.
func (e *env) createTarget(asked, site string, now time.Time) (string, error) {
	dir := e.downloads
	if asked != "" {
		p, err := filepath.Abs(expandHome(asked))
		if err != nil {
			return "", err
		}
		fi, err := os.Stat(p)
		switch {
		case err == nil && fi.IsDir():
			dir = p
		case err == nil:
			return "", fmt.Errorf("%s var: başka bir --hedef ver", p)
		case !errors.Is(err, fs.ErrNotExist):
			return "", readErr(p, err)
		default:
			if fi, err := os.Stat(filepath.Dir(p)); err != nil || !fi.IsDir() {
				return "", fmt.Errorf("%s klasörü yok", filepath.Dir(p))
			}
			return p, nil
		}
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", folderErr(dir, err)
	}
	base := filepath.Join(dir, site+"-"+now.Format("20060102-150405"))
	for i := 1; i <= 99; i++ {
		p := base + ".footops.tar"
		if i > 1 {
			p = fmt.Sprintf("%s-%d.footops.tar", base, i)
		}
		_, err := os.Lstat(p)
		_, perr := os.Lstat(p + ".part")
		if errors.Is(err, fs.ErrNotExist) && errors.Is(perr, fs.ErrNotExist) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s-… adlı 99 arşiv var", base)
}

// inside tells dir is folder or below it: the folders on the way up from dir compared by
// identity, not by name (a Mac's disk takes "Paket" for "paket", links lead elsewhere).
func inside(dir, folder string) bool {
	want, err := os.Stat(folder)
	if err != nil {
		return false
	}
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(d); err == nil && os.SameFile(fi, want) {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// settle gives the made archive its name, never over a file that appeared meanwhile.
func settle(part, target string) error {
	err := os.Link(part, target)
	switch {
	case err == nil:
		return os.Remove(part)
	case errors.Is(err, fs.ErrExist):
		return fmt.Errorf("%s az önce oluştu: başka bir --hedef ver", target)
	}
	// A disk without hard links (exFAT): renamed, the name still free.
	if _, serr := os.Lstat(target); !errors.Is(serr, fs.ErrNotExist) {
		return fmt.Errorf("%s var: başka bir --hedef ver", target)
	}
	return os.Rename(part, target)
}

// decomposedNames counts the names in a folder written with combining marks (NFD: macOS
// often writes "ü" as u and a combining diaeresis).
func decomposedNames(root *os.Root) int {
	n := 0
	_ = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		for _, r := range d.Name() {
			if r >= 0x300 && r <= 0x36f {
				n++
				break
			}
		}
		return nil
	})
	return n
}

// ctxWriter stops a write once ctx ends.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// LocalDatabase is a database of the Mac's MongoDB; Opened: a backup or an archive was
// loaded into it (*_yedek_*, *_arsiv_*).
type LocalDatabase struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Opened bool   `json:"opened,omitempty"`
}

// LocalDatabases is arsiv veritabanlari's answer; MongoError: the Mac's MongoDB could not
// be asked.
type LocalDatabases struct {
	URI        string          `json:"uri,omitempty"`
	Databases  []LocalDatabase `json:"databases"`
	MongoError string          `json:"mongo_error,omitempty"`
}

// localDatabases is arsiv veritabanlari: the databases an archive can be made of.
func (e *env) localDatabases(ctx context.Context, jsonOut bool) error {
	v := LocalDatabases{Databases: []LocalDatabase{}}
	if t, err := parseMongoURI(e.mongoURI); err != nil {
		v.MongoError = err.Error()
	} else {
		v.URI = t.label()
		dbs, err := t.databases(ctx)
		if errors.Is(err, errStopped) {
			return err
		}
		if err != nil {
			v.MongoError = err.Error()
		}
		for name, size := range dbs {
			if len(loadableDBs([]string{name})) == 1 {
				v.Databases = append(v.Databases, LocalDatabase{Name: name, Bytes: size,
					Opened: reDefaultLoad.MatchString(name) || markedDBName(name) && e.registeredDB(name)})
			}
		}
		sort.Slice(v.Databases, func(i, j int) bool { return v.Databases[i].Name < v.Databases[j].Name })
	}
	if jsonOut {
		e.event(v)
		return nil
	}
	if v.MongoError != "" {
		return errors.New(v.MongoError)
	}
	printLocalDatabases(e.out, v)
	return nil
}

// ServerPanel is a server whose panel address the settings have: where its İçe aktar is.
type ServerPanel struct {
	Code  string
	Panel string
}

// panels are the servers with a panel address in the settings, by code.
func (e *env) panels() []ServerPanel {
	s, err := readSettings(e.settings)
	if err != nil {
		return nil
	}
	var out []ServerPanel
	for code, st := range s.Servers {
		if st.Panel != "" {
			out = append(out, ServerPanel{Code: code, Panel: strings.TrimRight(st.Panel, "/")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}
