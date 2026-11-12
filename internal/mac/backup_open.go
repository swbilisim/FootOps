package mac

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	sitedef "github.com/swbilisim/FootOps/internal/sites"
)

// Opening a site's backup (panel-tasarim §29.5): into a folder of the Mac's — db.archive,
// files/, footops.json and env, as the panel's İndir gives them — and, if asked, its
// database into the Mac's MongoDB (yedek mongo: the database alone, through a temporary
// folder). The folder is the owner's only (0700, files 0600); stopped half-way, what was
// begun is removed.

const (
	// defMax is the most read of a definition's file (footops.json, compose.yaml).
	defMax = 1 << 20
	// spareRoom is left free on the Mac's disk beyond what is opened.
	spareRoom = 256 << 20
	// progressEvery: the app is told how far at most this often.
	progressEvery = 250 * time.Millisecond
	// defaultDBVar is the database connection's variable when the site names none
	// (internal/sites).
	defaultDBVar = "MONGODB_URI"
)

// openReq is what yedek ac (a folder) and yedek mongo (the database alone) are asked.
type openReq struct {
	server, site, when string
	folder             bool
	// target is the folder asked for ("": ~/Downloads/<site>-<time>); only the parts
	// asked for (none: every part the backup has).
	target string
	only   map[string]bool
	// mongo loads the database into the Mac's MongoDB as dbName ("": <site>_yedek_…);
	// overwrite drops one there of that name first.
	mongo     bool
	dbName    string
	overwrite bool
}

// openArgs reads yedek ac's arguments (folder) or yedek mongo's.
func openArgs(args []string, folder bool) (openReq, error) {
	req := openReq{folder: folder, only: map[string]bool{}, mongo: !folder}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := strings.Cut(a, "=")
		takes := func() (string, error) {
			if !hasValue {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
					return "", fmt.Errorf("%w: %s bir değer ister", errUsage, name)
				}
				i++
				value = args[i]
			}
			if strings.TrimSpace(value) == "" {
				return "", fmt.Errorf("%w: %s boş olamaz", errUsage, name)
			}
			return value, nil
		}
		var err error
		switch {
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		case folder && name == "--hedef":
			req.target, err = takes()
		case name == "--ad":
			req.dbName, err = takes()
		case folder && a == "--db":
			req.only[partDB] = true
		case folder && a == "--dosyalar":
			req.only[partFiles] = true
		case folder && a == "--ayarlar":
			req.only[partDef] = true
		case folder && a == "--mongo":
			req.mongo = true
		case a == "--uzerine":
			req.overwrite = true
		default:
			return req, fmt.Errorf("%w: bilinmeyen seçenek: %s", errUsage, clip(a, 40))
		}
		if err != nil {
			return req, err
		}
	}
	switch {
	case len(pos) < 2 || len(pos) > 3:
		return req, errUsage
	case (req.dbName != "" || req.overwrite) && !req.mongo:
		return req, fmt.Errorf("%w: --ad ve --uzerine yalnız --mongo ile", errUsage)
	case req.mongo && len(req.only) > 0 && !req.only[partDB]:
		return req, fmt.Errorf("%w: --mongo veritabanını ister: --db'yi de seç", errUsage)
	}
	req.server, req.site = pos[0], pos[1]
	if len(pos) == 3 {
		req.when = pos[2]
	}
	if !reSiteName.MatchString(req.site) {
		return req, fmt.Errorf("geçersiz site adı: %q", clip(req.site, 40))
	}
	return req, nil
}

// OpenedPart is one part as opened: its snapshot and what was written of it (bytes;
// Count: the files restored, the environment's variables).
type OpenedPart struct {
	BackupPart
	Kind    string `json:"kind,omitempty"`
	Run     string `json:"run,omitempty"`
	Written int64  `json:"written,omitempty"`
	Count   int64  `json:"count,omitempty"`
}

// LoadedDB is a database loaded into the Mac's MongoDB; Replaced: one of that name was
// dropped first. Expected is the documents an archive's manifest counted (0: no count to
// compare with).
type LoadedDB struct {
	Name      string `json:"name"`
	Documents int64  `json:"documents"`
	Failed    int64  `json:"failed,omitempty"`
	Replaced  bool   `json:"replaced,omitempty"`
	Expected  int64  `json:"expected,omitempty"`
}

// OpenResult is yedek ac's (and yedek mongo's, arsiv ac's, arsiv mongo's) answer, and its
// start. Archive is the archive's file name (an archive opened: Server is then the server
// of the backup it was made from, if it was).
type OpenResult struct {
	Server  string `json:"server"`
	Bucket  string `json:"bucket"`
	Site    string `json:"site"`
	Archive string `json:"archive,omitempty"`
	// Target is the folder (yedek ac); Time the newest part's.
	Target   string                `json:"target,omitempty"`
	Time     time.Time             `json:"time"`
	Parts    map[string]OpenedPart `json:"parts"`
	Database *LoadedDB             `json:"database,omitempty"`
	Warnings []string              `json:"warnings,omitempty"`
	TookMS   int64                 `json:"took_ms"`
}

// openBackup is yedek ac and yedek mongo.
func (e *env) openBackup(ctx context.Context, req openReq, jsonOut bool) (err error) {
	start := time.Now()
	loc := e.now().Location()
	r, err := e.openRepo(ctx, req.server)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	list, _, err := r.snapshots(lctx, req.site)
	cancel()
	if err != nil {
		return err
	}
	chosen, err := choose(list, req.site, req.when, loc)
	if err != nil {
		return err
	}
	for p := range req.only {
		if _, ok := chosen[p]; !ok {
			return fmt.Errorf("bu yedekte %s yok", partTR[p])
		}
	}
	want := map[string]bool{}
	for _, p := range siteParts {
		if _, ok := chosen[p]; ok && (req.folder && (len(req.only) == 0 || req.only[p]) || !req.folder && p == partDB) {
			want[p] = true
		}
	}
	if req.mongo && !want[partDB] {
		return errors.New("bu yedekte veritabanı yok")
	}

	res := OpenResult{Server: r.code, Bucket: r.bucket, Site: req.site, Parts: map[string]OpenedPart{}}
	var need int64 = spareRoom
	for _, p := range siteParts {
		if !want[p] {
			continue
		}
		s := chosen[p]
		res.Parts[p] = OpenedPart{BackupPart: BackupPart{ID: s.ID, Time: s.Time, Files: s.Files, Bytes: s.Bytes}, Kind: s.Kind, Run: s.Run}
		if s.Time.After(res.Time) {
			res.Time = s.Time
		}
		need += s.Bytes
	}

	// The Mac's MongoDB before anything is fetched: there, and the name free or given up.
	var mt mongoTarget
	exists := false
	if req.mongo {
		if req.dbName == "" {
			req.dbName = defaultDBName(req.site, chosen[partDB].Time.In(loc))
		}
		if err := checkDBName(req.dbName); err != nil {
			return err
		}
		if _, err := e.look("mongorestore"); err != nil {
			return errors.New("mongorestore bulunamadı: brew install mongodb-database-tools")
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
	}

	// Where it goes: a folder of the owner's, or (the database alone) a temporary one. On
	// the list of what was opened from its first moment (partial), its mark held: a
	// footops-mac killed half-way leaves a folder the list still shows, to be deleted.
	var target string
	created := false
	if req.folder {
		if target, created, err = e.openTarget(req.target, req.site, res.Time.In(loc), need); err != nil {
			return err
		}
		res.Target = target
	} else {
		if err := e.roomFor(e.tmpDir(), need); err != nil {
			return err
		}
		if target, err = os.MkdirTemp(e.tmpDir(), "footops-mac-yedek-"); err != nil {
			return err
		}
		created = true
	}
	mark, err := makeMark(target)
	if err != nil {
		if created {
			removeTree(target)
		}
		return err
	}
	defer mark.release()
	entry := e.folderEntry(mark, target, r.code, req.site, res.Time, !created)
	if err := e.beginOpened(entry); err != nil {
		res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
	}
	whole := false
	defer func() {
		// Cut short or failed: what was begun goes (and its line); the database alone:
		// its temporary folder goes in any case. A removal that fails leaves the line,
		// the folder deletable later.
		if whole {
			return
		}
		if _, rerr := removeOpened(target, !created); rerr == nil {
			e.endOpened(entry, false)
		}
	}()
	if req.folder && created && e.exclude != nil {
		// Its secrets kept out of Time Machine: deleted here, deleted for good. (A folder
		// that was there before is the owner's: its exclusion would outlive the backup.)
		e.exclude(target)
	}

	prog := &progressor{e: e, on: jsonOut}
	defer prog.end()
	if jsonOut {
		start := res
		if req.mongo {
			start.Database = &LoadedDB{Name: req.dbName, Replaced: exists}
		}
		e.event(struct {
			Event string `json:"event"`
			OpenResult
		}{"start", start})
	} else {
		printOpenStart(e.out, res, req, loc)
	}
	prog.beat()

	if want[partDB] {
		s := chosen[partDB]
		e.say(jsonOut, "  … veritabanı (%s)\n", bytesTR(s.Bytes))
		n, err := r.dumpTo(ctx, s, filepath.Join(target, "db.archive"), func(done int64) { prog.report(partDB, done, s.Bytes) })
		if err != nil {
			return fmt.Errorf("veritabanı alınamadı: %w", err)
		}
		if s.Bytes > 0 && n != s.Bytes {
			return fmt.Errorf("veritabanı eksik geldi: %d / %d bayt", n, s.Bytes)
		}
		p := res.Parts[partDB]
		p.Written = n
		res.Parts[partDB] = p
		if req.folder {
			e.say(jsonOut, "  ✓ db.archive (%s)\n", bytesTR(n))
		}
	}
	if want[partFiles] {
		s := chosen[partFiles]
		e.say(jsonOut, "  … dosyalar (%d dosya, %s)\n", s.Files, bytesTR(s.Bytes))
		warns, err := r.restore(ctx, s, filepath.Join(target, "files"), func(done, total int64) { prog.report(partFiles, done, total) })
		if err != nil {
			return fmt.Errorf("dosyalar açılamadı: %w", err)
		}
		// The server wrote the tree: its links, special files, execute bits and extended
		// attributes do not come along, and the files are quarantined.
		removed, failed, err := defang(filepath.Join(target, "files"))
		if err != nil {
			return fmt.Errorf("açılan dosyalar güvenli hâle getirilemedi: %w", err)
		}
		if removed > 0 {
			warns = append(warns, fmt.Sprintf("%d bağlantı ya da özel dosya açılmadı (yedekte vardı)", removed))
		}
		if failed > 0 {
			warns = append(warns, fmt.Sprintf("%d dosyanın izni ya da karantinası ayarlanamadı: açmadan önce bak", failed))
		}
		// Counted on the disk: restic's own count takes the folders in.
		files, size := folderSize(filepath.Join(target, "files"))
		p := res.Parts[partFiles]
		p.Written, p.Count = size, files
		res.Parts[partFiles] = p
		res.Warnings = append(res.Warnings, warns...)
		e.say(jsonOut, "  ✓ files/ (%d dosya, %s)\n", files, bytesTR(size))
	}
	if want[partDef] {
		n, err := r.writeDef(ctx, chosen[partDef], target, loc)
		if err != nil {
			return fmt.Errorf("ayarlar açılamadı: %w", err)
		}
		p := res.Parts[partDef]
		p.Count = int64(n)
		res.Parts[partDef] = p
		e.say(jsonOut, "  ✓ footops.json, env (%d değişken; sırlar içerir, klasör yalnız senin)\n", n)
	}
	if req.mongo {
		// Again just before loading: another footops-mac may have taken the name meanwhile
		// (what is loaded is dropped if the loading fails: only what is ours).
		dbs, err := mt.databases(ctx)
		if err != nil {
			return err
		}
		if _, now := dbs[req.dbName]; now && !exists {
			return &codedError{code: "db_exists", fields: map[string]any{"database": req.dbName},
				text: fmt.Sprintf("Mac'teki MongoDB'de %s adlı veritabanı az önce açıldı: üzerine yazmak için --uzerine, başka ad için --ad", req.dbName)}
		}
		e.say(jsonOut, "  … Mac'teki MongoDB: %s → %s (%s)\n", req.site, req.dbName, mt.label())
		db, err := e.loadDB(ctx, mt, dumpSource{path: filepath.Join(target, "db.archive")}, req.site, req.dbName, exists,
			func(done, total int64) { prog.report("mongo", done, total) })
		if err != nil {
			return err
		}
		res.Database = db
		e.say(jsonOut, "  ✓ yüklendi: %s (%d belge%s)\n", db.Name, db.Documents, failedTR(db.Failed))
		if err := e.noteDatabase(res); err != nil {
			res.Warnings = append(res.Warnings, "açılanlar listesine yazılamadı: "+err.Error())
		}
	}

	// The folder whole: its line on the list no longer partial (a list not written
	// leaves what was opened usable). The database alone: its temporary folder goes.
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

func failedTR(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d belge yüklenemedi", n)
}

// say writes a line for the Terminal (with --json, the events say it).
func (e *env) say(jsonOut bool, format string, args ...any) {
	if !jsonOut {
		fmt.Fprintf(e.out, format, args...)
	}
}

// openTarget makes the folder a backup opens into: the one asked for (new, or empty),
// else ~/Downloads/<site>-<time> (-2, -3… when taken). It is the owner's only.
func (e *env) openTarget(asked, site string, at time.Time, need int64) (string, bool, error) {
	if asked != "" {
		p, err := filepath.Abs(expandHome(asked))
		if err != nil {
			return "", false, err
		}
		if err := e.roomFor(filepath.Dir(p), need); err != nil {
			return "", false, err
		}
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(p, 0o700); err != nil {
				return "", false, folderErr(p, err)
			}
			return p, true, nil
		case err != nil:
			return "", false, folderErr(p, err)
		case !fi.IsDir():
			return "", false, fmt.Errorf("%s bir klasör değil", p)
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return "", false, folderErr(p, err)
		}
		for _, en := range entries {
			if en.Name() != ".DS_Store" { // the Finder's own
				return "", false, fmt.Errorf("%s dolu: başka bir --hedef ver ya da klasörü boşalt", p)
			}
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return "", false, folderErr(p, err)
		}
		return p, false, nil
	}
	if err := os.MkdirAll(e.downloads, 0o700); err != nil {
		return "", false, folderErr(e.downloads, err)
	}
	if err := e.roomFor(e.downloads, need); err != nil {
		return "", false, err
	}
	base := filepath.Join(e.downloads, site+"-"+at.Format("20060102-1504"))
	for i := 1; i <= 99; i++ {
		p := base
		if i > 1 {
			p = fmt.Sprintf("%s-%d", base, i)
		}
		err := os.Mkdir(p, 0o700)
		if err == nil {
			return p, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", false, folderErr(p, err)
		}
	}
	return "", false, fmt.Errorf("%s-… adlı 99 klasör var: eskilerini sil", base)
}

// folderErr says what went wrong with a folder; macOS refusing the app its Downloads
// folder (privacy: EPERM, not EACCES) in so many words.
func folderErr(p string, err error) error {
	if errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("macOS %s klasörüne yazma izni vermedi: Sistem Ayarları › Gizlilik ve Güvenlik › Dosyalar ve Klasörler › FootOps (ya da --hedef ile başka bir klasör)", filepath.Dir(p))
	}
	return fmt.Errorf("klasör hazırlanamadı (%s): %v", p, err)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// roomFor refuses to start what would fill the disk.
func (e *env) roomFor(dir string, need int64) error {
	free, err := e.free(dir)
	if err != nil || free >= uint64(need) {
		return nil // unknown: the copy itself says when the disk fills
	}
	return fmt.Errorf("Mac'te yer yetmiyor: %s gerekli (%s pay dahil), %s boş", bytesTR(need), bytesTR(spareRoom), bytesTR(int64(free)))
}

func (e *env) tmpDir() string {
	if e.tmp != "" {
		return e.tmp
	}
	return os.TempDir()
}

// removeTree removes a folder as rm -rf does, read-only folders in it made writable
// first (restic restores the backup's modes: a folder of 0555 holds on to its files).
// Links are removed, never followed.
func removeTree(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// ---- the parts ----

// dumpTo writes one file of a snapshot (a database's dump) to a new file of the owner's.
func (r *repo) dumpTo(ctx context.Context, s snapshot, file string, onBytes func(int64)) (int64, error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, folderErr(file, err)
	}
	cw := &countingWriter{w: f, on: onBytes}
	err = r.run(ctx, cw, "dump", s.ID, partPath(r.root, s.Site, s.Part))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file)
		return 0, err
	}
	return cw.n, nil
}

// restore restores a site's files into dir (a new folder), with restic's own progress.
// Files restic could not write are warnings (the rest is there; restic names them on
// stderr); no summary at all is a failure.
func (r *repo) restore(ctx context.Context, s snapshot, dir string, onProgress func(done, total int64)) ([]string, error) {
	summary := false
	lines := &lineWriter{on: func(line []byte) {
		var m resticMessage
		if json.Unmarshal(line, &m) != nil {
			return
		}
		switch m.Type {
		case "status":
			onProgress(m.BytesRestored, m.TotalBytes)
		case "summary":
			summary = true
			onProgress(m.BytesRestored, m.TotalBytes)
		}
	}}
	err := r.run(ctx, lines, "restore", s.ID+":"+partPath(r.root, s.Site, s.Part), "--target", dir, "--json")
	lines.flush()
	var rf *resticFailure
	if err == nil || !summary || !errors.As(err, &rf) {
		return nil, err
	}
	n, problems := restoreErrors(r.mask(rf.stderr))
	if n == 0 {
		return nil, err
	}
	return []string{fmt.Sprintf("%d dosya açılamadı: %s", n, strings.Join(problems, "; "))}, nil
}

var reErrorCount = regexp.MustCompile(`There were (\d+) errors`)

// restoreErrors reads what restic restore --json says it could not write: how many, and
// the first few.
func restoreErrors(stderr string) (int, []string) {
	var problems []string
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		var m resticMessage
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &m) != nil {
			continue
		}
		switch m.Type {
		case "error":
			n++
			if len(problems) < 5 {
				problems = append(problems, clip(clean(m.Item+": "+m.Error.Message), 200))
			}
		case "exit_error":
			if c := reErrorCount.FindStringSubmatch(m.Message); c != nil {
				if total, err := strconv.Atoi(c[1]); err == nil && total > n {
					n = total // the stderr kept is the end of it
				}
			}
		}
	}
	return n, problems
}

// writeDef writes a site's definition as the panel's İndir gives its environment:
// footops.json as backed up (variable names only) and env — the variables of
// compose.yaml, sorted, double-quoted, the site's FootOps database connection left out
// (agent backupEnv; it opens nothing anywhere else). compose.yaml itself is not
// written: it holds that connection's password. It returns the variables written.
func (r *repo) writeDef(ctx context.Context, s snapshot, dir string, loc *time.Location) (int, error) {
	meta, compose, err := r.readDef(ctx, s)
	if err != nil {
		return 0, err
	}
	var spec sitedef.Spec
	if err := json.Unmarshal(meta, &spec); err != nil {
		return 0, fmt.Errorf("yedekteki footops.json anlaşılamadı: %v", err)
	}
	vars, err := sitedef.ParseComposeEnv(string(compose))
	if err != nil {
		return 0, fmt.Errorf("yedekteki ortam değişkenleri okunamadı: %v", err)
	}
	name := clip(clean(spec.Name), 64)
	if name == "" {
		name = s.Site
	}
	head := []string{name + " ortam değişkenleri, yedek " + s.Time.In(loc).Format("02.01.2006 15:04"),
		"SIRLAR İÇERİR: güvenli bir yerde sakla. İçe aktarma bunlara dokunmaz; site ayarlarında elle girilir."}
	if spec.Database {
		dbVar := spec.DBVar
		if dbVar == "" {
			dbVar = defaultDBVar
		}
		if _, ok := vars[dbVar]; ok {
			delete(vars, dbVar)
			head = append(head, dbVar+" alınmadı: veritabanı bağlantısını FootOps her sunucuda kendisi kurar.")
		}
	}
	if err := writeNew(filepath.Join(dir, sitedef.MetaFile), meta); err != nil {
		return 0, err
	}
	if err := writeNew(filepath.Join(dir, "env"), []byte(sitedef.FormatDotenv(vars, head...))); err != nil {
		return 0, err
	}
	return len(vars), nil
}

// readDef reads a definition's two files in one restic call (each call opens the
// repository's key anew, ~0.5 s): the folder's dump is a tar.
func (r *repo) readDef(ctx context.Context, s snapshot) (meta, compose []byte, err error) {
	dir := partPath(r.root, s.Site, partDef)
	buf := &capped{max: 4 * defMax}
	if err := r.run(ctx, buf, "dump", s.ID, dir); err != nil {
		return nil, nil, err
	}
	if buf.over {
		return nil, nil, errors.New("yedekteki ayarlar beklenenden büyük")
	}
	in := strings.TrimPrefix(dir, "/") + "/"
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("yedekteki ayarlar okunamadı: %v", err)
		}
		if h.Typeflag != tar.TypeReg || h.Size > defMax {
			continue
		}
		var to *[]byte
		switch strings.TrimPrefix(h.Name, "/") {
		case in + sitedef.MetaFile:
			to = &meta
		case in + sitedef.ComposeFile:
			to = &compose
		default:
			continue
		}
		if *to, err = io.ReadAll(io.LimitReader(tr, defMax)); err != nil {
			return nil, nil, fmt.Errorf("yedekteki ayarlar okunamadı: %v", err)
		}
	}
	if meta == nil || compose == nil {
		return nil, nil, errors.New("yedekte footops.json ya da compose.yaml yok")
	}
	return meta, compose, nil
}

// writeNew writes a new file of the owner's.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return folderErr(path, err)
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ---- progress ----

// heartbeat: while a part says nothing (restic reading the repository's index), its last
// word is said again this often — a line that cannot be written tells the core the app
// is gone (it quit or crashed), and the core cleans up instead of waiting on.
var heartbeat = 2 * time.Second

// progressor tells the app how far a part has come, at most every progressEvery (and
// each part's first word at once), and keeps saying it (heartbeat) until ended.
type progressor struct {
	e           *env
	on          bool
	mu          sync.Mutex
	part        string
	done, total int64
	last        time.Time
	ended       bool
}

func (p *progressor) report(part string, done, total int64) {
	if !p.on {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	final := total > 0 && done >= total
	if part == p.part && now.Sub(p.last) < progressEvery && !final {
		p.done, p.total = done, total
		return
	}
	p.part, p.done, p.total = part, done, total
	p.say(now)
}

func (p *progressor) say(now time.Time) {
	if p.ended {
		return
	}
	p.last = now
	p.e.event(map[string]any{"event": "progress", "part": p.part, "done": p.done, "total": p.total})
}

// beat says the last word again while nothing new comes, until end.
func (p *progressor) beat() {
	if !p.on {
		return
	}
	go func() {
		t := time.NewTicker(heartbeat)
		defer t.Stop()
		for range t.C {
			p.mu.Lock()
			if p.ended {
				p.mu.Unlock()
				return
			}
			if time.Since(p.last) >= heartbeat {
				p.say(time.Now())
			}
			p.mu.Unlock()
		}
	}()
}

// end stops the progress lines: none comes after the result.
func (p *progressor) end() {
	p.mu.Lock()
	p.ended = true
	p.mu.Unlock()
}

type countingWriter struct {
	w  io.Writer
	n  int64
	on func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.on != nil {
		c.on(c.n)
	}
	return n, err
}

type countingReader struct {
	r     io.Reader
	n     int64
	total int64
	on    func(done, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.on != nil && n > 0 {
		c.on(c.n, c.total)
	}
	return n, err
}

// lineWriter hands what is written over a line at a time.
type lineWriter struct {
	on   func([]byte)
	rest []byte
}

func (l *lineWriter) Write(p []byte) (int, error) {
	l.rest = append(l.rest, p...)
	for {
		i := bytes.IndexByte(l.rest, '\n')
		if i < 0 {
			break
		}
		l.on(l.rest[:i])
		l.rest = l.rest[i+1:]
	}
	if len(l.rest) > 1<<20 { // a line that never ends is no line of restic's
		l.rest = nil
	}
	return len(p), nil
}

func (l *lineWriter) flush() {
	if len(l.rest) > 0 {
		l.on(l.rest)
		l.rest = nil
	}
}
