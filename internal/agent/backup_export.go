package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/version"
)

// A backup made into a FootOps archive (panel-tasarim §27.4, "İndir"): the same archive a
// site's export is — db.archive.gz, files.tar.gz, manifest.json — so another FootOps
// imports it, plus, if asked for, the site's environment at the time as a .env (env;
// secrets included, an import leaves it alone). restic dumps each part in its tool
// container; a helper in a limited scope of its own compresses it (data-gzip,
// data-repack) on the way into the archive, which is written once and downloaded from the
// staging area like an export.

// memberEnv is a FootOps archive's .env of the site's environment (backups only).
const (
	memberEnv    = "env"
	maxEnvMember = 256 << 10
)

// nobody owns nothing: the helpers that only turn one stream into another run as it.
const nobody = 65534

// BackupExport says what of a backup goes into the archive.
type BackupExport struct {
	Run   string `json:"run"`
	DB    bool   `json:"db"`
	Files bool   `json:"files"`
	// Env adds the site's environment of the time (its secrets too).
	Env bool `json:"env"`
}

// ExportBackupInfo is the manifest's note of the backup an archive was made from.
type ExportBackupInfo struct {
	Run       string            `json:"run"`
	Kind      string            `json:"kind,omitempty"`
	Time      time.Time         `json:"time"`
	Server    string            `json:"server,omitempty"`
	Snapshots map[string]string `json:"snapshots"` // part → snapshot
}

// ExportBackup writes a FootOps archive of a backup into the staging area, to download
// for a day like an export.
func (b *Backups) ExportBackup(ctx context.Context, t *Task, site string, req BackupExport) (info ExportInfo, err error) {
	b.init()
	spec, err := b.Data.Store.Get(site)
	if err != nil {
		return info, err
	}
	pt, err := b.SiteBackup(site, req.Run)
	if err != nil {
		return info, err
	}
	dbSnap, hasDB := pt.Parts[PartDB]
	fileSnap, hasFiles := pt.Parts[PartFiles]
	defSnap, hasDef := pt.Parts[PartDef]
	switch {
	case !req.DB && !req.Files:
		return info, errors.New("arşive veritabanı ya da dosyalar seçilmeli")
	case req.DB && !hasDB:
		return info, errors.New("bu yedekte veritabanı yok")
	case req.Files && !hasFiles:
		return info, errors.New("bu yedekte dosyalar yok")
	case req.Env && !hasDef:
		return info, errors.New("bu yedekte sitenin ayarları (ortam değişkenleri) yok")
	}
	need := int64(1 << 20)
	if req.DB {
		need += dbSnap.Bytes // the gzip'd dump is smaller
	}
	if req.Files {
		need += fileSnap.Bytes // photos barely compress
	}
	if err := b.Data.Staging.Allow(need); err != nil {
		return info, err
	}
	sec, err := b.secret()
	if err != nil {
		return info, err
	}
	env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
	if err != nil {
		return info, err
	}
	defer done()
	dir, err := b.Data.Staging.Dir(exportPrefix + t.ID())
	if err != nil {
		return info, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	// The environment first: an archive that cannot have it fails before anything is
	// written.
	var envText string
	var envVars int
	if req.Env {
		if envText, envVars, err = b.backupEnv(ctx, t, env, spec, defSnap, pt.Time); err != nil {
			return info, err
		}
	}
	taken := pt.Time.UTC().Truncate(time.Second)
	now := b.now().UTC().Truncate(time.Second)
	name := fmt.Sprintf("%s-%s.footops.tar", spec.Name, taken.Format("20060102-150405"))
	part := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return info, err
	}
	defer f.Close()
	man := ExportManifest{Format: exportFormat, FootOps: version.Version, Site: spec.Name, Group: spec.Group, Created: taken,
		Collections: map[string]int64{}, Members: map[string]ExportMember{},
		Backup: &ExportBackupInfo{Run: pt.Run, Kind: pt.Kind, Time: taken, Server: b.Server, Snapshots: map[string]string{}}}
	t.Step(fmt.Sprintf("ℹ Yedek: %s (%s)", localTime(pt.Time), runKindText(pt.Kind)))

	if req.DB {
		man.Backup.Snapshots[PartDB] = dbSnap.ID
		t.Step(fmt.Sprintf("… Veritabanı yedekten alınıyor: %s döküm, sıkıştırılarak", humanBytes(dbSnap.Bytes)))
		var hs helperStats
		m, err := writeMember(f, memberDB, taken, func(w io.Writer, _ *countingWriter) error {
			var err error
			hs, err = b.pipeThrough(ctx, t, env, w, dbSnap.Bytes, "db", []string{"data-gzip"},
				"dump", "--no-lock", dbSnap.ID, partPath(site, PartDB))
			return err
		})
		if err != nil {
			return info, fmt.Errorf("veritabanı alınamadı: %w", err)
		}
		if dbSnap.Bytes > 0 && hs.Result.Bytes != dbSnap.Bytes {
			return info, fmt.Errorf("veritabanı dökümü eksik geldi: %s, yedekte %s", humanBytes(hs.Result.Bytes), humanBytes(dbSnap.Bytes))
		}
		// Rule K2 for our own output: the member reads back as an intact mongodump archive.
		p, _, err := readMongoSection(ctxReader{ctx, io.NewSectionReader(f, m.offset, m.Size)}, true, sha256.New())
		if err != nil {
			return info, fmt.Errorf("arşivdeki veritabanı doğrulanamadı: %w", err)
		}
		dbs := importable(p.Databases())
		if len(dbs) != 1 {
			return info, fmt.Errorf("yedekte tek bir veritabanı bekleniyordu, %d var", len(dbs))
		}
		man.DB, man.MongoServer, man.MongoTool = dbs[0], p.ServerVersion, "mongodump "+p.ToolVersion
		if rec, ok := pt.recordPart(PartDB); ok {
			views := map[string]bool{}
			for _, n := range p.Collections(man.DB) {
				views[n.Collection] = n.Type == "view"
			}
			for c, n := range rec.Collections {
				if !views[c] && !strings.HasPrefix(c, "system.") {
					man.Collections[c] = n
					man.Documents += n
				}
			}
		}
		man.Members[memberDB] = m.ExportMember
		t.Step(fmt.Sprintf("✓ Veritabanı: %s → %s sıkıştırılmış", humanBytes(hs.Result.Bytes), humanBytes(m.Size)))
	}

	if req.Files {
		man.Backup.Snapshots[PartFiles] = fileSnap.ID
		t.Step(fmt.Sprintf("… Dosyalar yedekten alınıyor: %d dosya, %s", fileSnap.Files, humanBytes(fileSnap.Bytes)))
		var hs helperStats
		m, err := writeMember(f, memberFiles, taken, func(w io.Writer, _ *countingWriter) error {
			var err error
			hs, err = b.pipeThrough(ctx, t, env, w, fileSnap.Bytes, "files", []string{"data-repack"},
				"dump", "--no-lock", fileSnap.ID+":"+partPath(site, PartFiles), "/")
			return err
		})
		if err != nil {
			return info, fmt.Errorf("dosyalar alınamadı: %w", err)
		}
		// Read back like an upload: names, sizes, gzip checksum.
		s, err := checkFilesMember(ctxReader{ctx, io.NewSectionReader(f, m.offset, m.Size)}, m.Size)
		if err != nil {
			return info, fmt.Errorf("arşivdeki dosyalar doğrulanamadı: %w", err)
		}
		if s.Files != hs.Result.Files || s.Size != hs.Result.Bytes {
			return info, fmt.Errorf("dosya arşivi tutmuyor: %d dosya / %d bayt, paketlenen %d / %d", s.Files, s.Size, hs.Result.Files, hs.Result.Bytes)
		}
		note := ""
		if hs.Result.Skipped > 0 {
			note = fmt.Sprintf(" — %d öğe alınmadı (bağlantı, özel dosya ya da içe aktarılamayacak ad)", hs.Result.Skipped)
		}
		t.Step(fmt.Sprintf("✓ Dosyalar: %d dosya, %d klasör, %s → %s%s", hs.Result.Files, hs.Result.Dirs,
			humanBytes(hs.Result.Bytes), humanBytes(m.Size), note))
		man.Files = &ExportFiles{Files: hs.Result.Files, Dirs: hs.Result.Dirs, Bytes: hs.Result.Bytes}
		man.Members[memberFiles] = m.ExportMember
	}

	if req.Env {
		man.Backup.Snapshots[PartDef] = defSnap.ID
		m, err := writeMember(f, memberEnv, taken, func(w io.Writer, _ *countingWriter) error {
			_, err := io.WriteString(w, envText)
			return err
		})
		if err != nil {
			return info, err
		}
		man.Members[memberEnv] = m.ExportMember
		t.Step(fmt.Sprintf("✓ Ortam değişkenleri: %d değişken (değerleri arşivde, sırlar dahil)", envVars))
	}

	data, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return info, err
	}
	if err := writeTail(f, data, now); err != nil {
		return info, err
	}
	if err := f.Sync(); err != nil {
		return info, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return info, err
	}
	whole := sha256.New()
	total, err := io.Copy(whole, ctxReader{ctx, bufio.NewReaderSize(f, 256<<10)})
	if err != nil {
		return info, err
	}
	if err := f.Close(); err != nil {
		return info, err
	}
	if err := os.Rename(part, filepath.Join(dir, name)); err != nil {
		return info, err
	}
	info = ExportInfo{ID: t.ID(), Site: spec.Name, File: name, Bytes: total, SHA256: hex.EncodeToString(whole.Sum(nil)),
		Created: now, Collections: len(man.Collections), Documents: man.Documents, Backup: taken, Env: req.Env}
	if man.Files != nil {
		info.Files, info.FileBytes = man.Files.Files, man.Files.Bytes
	}
	ib, err := json.Marshal(info)
	if err != nil {
		return info, err
	}
	if err := writeSynced(filepath.Join(dir, exportInfo), ib); err != nil {
		return info, err
	}
	t.SetBytes(total, 0)
	t.SetResult(info.ID)
	t.Step(fmt.Sprintf("✓ Arşiv hazır: %s (%s); sunucuda %d saat indirilebilir", name, humanBytes(total), int(stagingKeep().Hours())))
	return info, nil
}

// backupEnv reads a site's environment as a backup holds it (its compose.yaml) and
// writes it as a .env. The site's database connection FootOps made itself is left out:
// another server makes its own, and the password in it opens nothing elsewhere.
func (b *Backups) backupEnv(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, defSnap BackupSnapshot, at time.Time) (string, int, error) {
	var compose bytes.Buffer
	lim := &limitWriter{w: &compose, left: maxEnvMember * 4}
	if _, err := b.restic(ctx, t, env, resticOpts{out: lim}, "dump", "--no-lock", defSnap.ID,
		partPath(spec.Name, PartDef)+"/"+sites.ComposeFile); err != nil {
		if lim.over {
			return "", 0, errors.New("yedekteki compose.yaml beklenenden büyük")
		}
		return "", 0, fmt.Errorf("sitenin ayarları alınamadı: %w", goneOr(err))
	}
	vars, err := sites.ParseComposeEnv(compose.String())
	if err != nil {
		return "", 0, fmt.Errorf("yedekteki ortam değişkenleri okunamadı: %w", err)
	}
	head := []string{spec.Name + " ortam değişkenleri, yedek " + localTime(at),
		"SIRLAR İÇERİR: güvenli bir yerde sakla. İçe aktarma bunlara dokunmaz; site ayarlarında elle girilir."}
	if dbVar := spec.DBVar; spec.Database && dbVar != "" {
		if _, ok := vars[dbVar]; ok {
			delete(vars, dbVar)
			head = append(head, dbVar+" alınmadı: veritabanı bağlantısını FootOps her sunucuda kendisi kurar.")
		}
	}
	text := sites.FormatDotenv(vars, head...)
	if len(text) > maxEnvMember {
		return "", 0, errors.New("ortam değişkenleri arşive sığmıyor")
	}
	return text, len(vars), nil
}

// pipeThrough runs a restic command in its tool container and a helper (helper's
// arguments) in a limited scope of its own, restic's output going into the helper by a
// pipe and the helper's into w: neither the agent nor the disk holds the stream between.
// The helper runs as a user that owns nothing, in a scope named after the job and what
// (one job runs several); total is what restic should send.
func (b *Backups) pipeThrough(ctx context.Context, t *Task, env *resticEnv, w io.Writer, total int64, what string, helper []string, args ...string) (helperStats, error) {
	var hs helperStats
	pr, pw, err := os.Pipe()
	if err != nil {
		return hs, err
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	sent := make(chan error, 1)
	go func() {
		_, err := b.restic(rctx, t, env, resticOpts{out: pw}, args...)
		pw.Close() // the helper sees the end once restic's own copy is closed as well
		sent <- err
	}()
	full := append(append([]string{}, helper...), "--uid="+strconv.Itoa(nobody), "--gid="+strconv.Itoa(nobody))
	cmd := b.Data.helper(ctx, t.ID()+"-"+what, nil, full...)
	cmd.Stdin, cmd.Stdout = pr, w
	cmd.Stderr = hs.line(t, total)
	u, herr := b.Data.Stream.Stream(ctx, cmd)
	t.AddUsage(u)
	pr.Close() // a restic still writing now fails instead of waiting forever
	if herr != nil {
		stop()
	}
	rerr := <-sent
	switch {
	case herr != nil && (rerr == nil || errors.Is(rerr, context.Canceled)) && ctx.Err() == nil:
		return hs, fmt.Errorf("sıkıştırılamadı: %w", herr)
	case rerr != nil:
		return hs, goneOr(rerr)
	case herr != nil:
		return hs, herr
	case !hs.done:
		return hs, errors.New("yardımcı sonuç bildirmedi")
	}
	return hs, nil
}

// ---- a backup's files, one at a time ----

// BackupEntry is one item of a folder in a backup's files.
type BackupEntry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // "file", "dir", "symlink", …
	Size  int64     `json:"size,omitempty"`
	Mode  string    `json:"mode,omitempty"` // as ls shows it
	MTime time.Time `json:"mtime"`
	UID   int       `json:"uid"`
}

// BackupFolder is a folder of a backup's files.
type BackupFolder struct {
	Path    string        `json:"path"` // "" at the top, "a/b" below
	Entries []BackupEntry `json:"entries"`
	More    bool          `json:"more,omitempty"` // cut at backupListMax
	Took    time.Duration `json:"took"`
}

const (
	backupListMax = 2000
	// A read outside a job is bounded: a listing by listTime, a single file's download
	// by fileTime (a large file over a slow link).
	listTime = 90 * time.Second
	fileTime = 2 * time.Hour
)

// readTask is a task for a short read outside the job system: it names the tool
// container and the staging directory of the credentials, and keeps no record.
func readTask() *Task { return &Task{j: NewJobs(), job: &Job{ID: "rd" + randomToken()}} }

// read runs fn with the repository at hand outside the job system, one at a time in its
// slot (listings in one, single-file downloads in another, so a slow download holds no
// listing up): they only read (--no-lock) and so may run beside a job.
func (b *Backups) read(ctx context.Context, limit time.Duration, slot chan struct{}, fn func(ctx context.Context, t *Task, env *resticEnv) error) error {
	b.init()
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	default:
		return ErrReadBusy
	}
	sec, err := b.secret()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	t := readTask()
	env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
	if err != nil {
		return err
	}
	defer done()
	return fn(ctx, t, env)
}

// ErrReadBusy: another read of the repository is under way.
var ErrReadBusy = errors.New("yedek deposundan başka bir okuma sürüyor; birazdan tekrar dene")

var (
	errBadPath     = errors.New("geçersiz yol")
	errNoFilesPart = errors.New("bu yedekte dosyalar yok")
)

// readLimits bound a read outside the jobs: a listing or one file needs little, and it
// may run beside a job's own tool container.
var readLimits = run.Limits{MemoryMB: 256, CPUPercent: 50, PIDs: 128}

// cleanFilesPath checks a path below a backup's files: "" for the top.
func cleanFilesPath(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	name, err := archive.CleanName(p)
	if err != nil || name == "" {
		return "", errBadPath
	}
	return name, nil
}

// BackupFiles lists one folder of a backup's files (restic ls, the folder only).
func (b *Backups) BackupFiles(ctx context.Context, site, run, dir string) (BackupFolder, error) {
	dir, err := cleanFilesPath(dir)
	if err != nil {
		return BackupFolder{}, err
	}
	pt, err := b.SiteBackup(site, run)
	if err != nil {
		return BackupFolder{}, err
	}
	snap, ok := pt.Parts[PartFiles]
	if !ok {
		return BackupFolder{}, errNoFilesPart
	}
	base := partPath(site, PartFiles)
	want := base
	if dir != "" {
		want += "/" + dir
	}
	out := BackupFolder{Path: dir, Entries: []BackupEntry{}}
	start := time.Now()
	found := false
	err = b.read(ctx, listTime, b.reads, func(ctx context.Context, t *Task, env *resticEnv) error {
		lines := &jsonLines{each: func(line []byte) {
			var n struct {
				Type        string    `json:"struct_type"`
				Name        string    `json:"name"`
				Kind        string    `json:"type"`
				Path        string    `json:"path"`
				Size        int64     `json:"size"`
				Permissions string    `json:"permissions"`
				MTime       time.Time `json:"mtime"`
				UID         int       `json:"uid"`
			}
			if json.Unmarshal(line, &n) != nil || n.Type != "node" {
				return
			}
			switch {
			case n.Path == want:
				found = n.Kind == "dir"
			case path.Dir(n.Path) == want:
				if len(out.Entries) >= backupListMax {
					out.More = true
					return
				}
				out.Entries = append(out.Entries, BackupEntry{Name: n.Name, Type: n.Kind, Size: n.Size, Mode: n.Permissions,
					MTime: n.MTime, UID: n.UID})
			}
		}}
		_, err := b.restic(ctx, t, env, resticOpts{out: lines, limits: &readLimits}, "ls", "--json", "--no-lock", snap.ID, want)
		lines.flush()
		return goneOr(err)
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, errNoBackupPath
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		di, dj := out.Entries[i].Type == "dir", out.Entries[j].Type == "dir"
		if di != dj {
			return di
		}
		return out.Entries[i].Name < out.Entries[j].Name
	})
	out.Took = time.Since(start)
	return out, nil
}

var errNoBackupPath = errors.New("bu yol yedekte yok")

// BackupFile is one file of a backup, about to be streamed.
type BackupFile struct {
	Name  string
	Size  int64
	MTime time.Time
}

// StreamBackupFile streams one regular file of a backup's files into w (restic dump),
// after head is told its size: once w has the first byte, a failure can only cut the
// stream (its size tells the browser it is short).
func (b *Backups) StreamBackupFile(ctx context.Context, site, run, p string, head func(BackupFile) io.Writer) error {
	p, err := cleanFilesPath(p)
	if err != nil || p == "" {
		return errBadPath
	}
	pt, err := b.SiteBackup(site, run)
	if err != nil {
		return err
	}
	snap, ok := pt.Parts[PartFiles]
	if !ok {
		return errNoFilesPart
	}
	full := partPath(site, PartFiles) + "/" + p
	return b.read(ctx, fileTime, b.fetches, func(ctx context.Context, t *Task, env *resticEnv) error {
		var file *BackupFile
		lines := &jsonLines{each: func(line []byte) {
			var n struct {
				Type  string    `json:"struct_type"`
				Kind  string    `json:"type"`
				Path  string    `json:"path"`
				Size  int64     `json:"size"`
				MTime time.Time `json:"mtime"`
			}
			if json.Unmarshal(line, &n) == nil && n.Type == "node" && n.Path == full && n.Kind == "file" {
				file = &BackupFile{Name: path.Base(p), Size: n.Size, MTime: n.MTime}
			}
		}}
		if _, err := b.restic(ctx, t, env, resticOpts{out: lines, limits: &readLimits}, "ls", "--json", "--no-lock", snap.ID, full); err != nil {
			return goneOr(err)
		}
		lines.flush()
		if file == nil {
			return errNoBackupPath
		}
		w := head(*file)
		cw := &countingWriter{w: w}
		if _, err := b.restic(ctx, t, env, resticOpts{out: cw, limits: &readLimits}, "dump", "--no-lock", snap.ID, full); err != nil {
			return &streamCut{err}
		}
		if cw.n != file.Size {
			return &streamCut{fmt.Errorf("dosya eksik geldi (%d/%d bayt)", cw.n, file.Size)}
		}
		return nil
	})
}

// streamCut is a failure after the response began: it can only be cut short.
type streamCut struct{ err error }

func (e *streamCut) Error() string { return e.err.Error() }
func (e *streamCut) Unwrap() error { return e.err }
