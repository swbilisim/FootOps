package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

// A site's persistent directory (data/) is read and written by a helper process, never
// by the agent itself: `footops data-pack` / `data-extract`, started in a systemd scope
// with its own memory and CPU limits, given the directories as open descriptors, and
// dropping to the site's user before it reads anything. An archive that tries to escape,
// or a link the app planted, meets the user's own permissions and os.Root.
//
// An import never writes into data/ while the site runs: the archive is extracted into
// data.new, the site stops, data becomes data.old-<time> and data.new becomes data (two
// renames), the site starts and must answer. The old directory is kept for FilesKeep, so
// an import can be undone; any failure puts it back at once.
const (
	dataNew       = "data.new"
	dataOldPrefix = "data.old-"
	dataBadPrefix = "data.failed-"
	FilesKeep     = 7 * 24 * time.Hour
	helperPrefix  = "footops-files-"
)

// helperLimits bound the helper: unpacking photos needs little memory.
var helperLimits = run.Limits{MemoryMB: 256, CPUPercent: 50, PIDs: 64}

var reBackupID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

// siteFiles is a site's data directory and the user that owns it (the image's user).
type siteFiles struct {
	Spec     sites.Spec
	Dir      string // .../data
	UID, GID int
}

func (s siteFiles) path(name string) string { return filepath.Join(filepath.Dir(s.Dir), name) }

func (d *DataOps) siteFiles(name string) (siteFiles, error) {
	spec, err := d.Store.Get(name)
	if err != nil {
		return siteFiles{}, err
	}
	if spec.DataMount == "" {
		return siteFiles{}, errors.New("bu sitenin kalıcı klasörü yok")
	}
	dir := filepath.Join(d.Store.Root, spec.Dir(), sites.DataDir)
	st, err := os.Lstat(dir)
	if err != nil {
		return siteFiles{}, fmt.Errorf("kalıcı klasör okunamadı: %w", err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || !ok {
		return siteFiles{}, errors.New("kalıcı klasör bir klasör değil")
	}
	return siteFiles{Spec: spec, Dir: dir, UID: int(sys.Uid), GID: int(sys.Gid)}, nil
}

// countFiles adds up a directory's regular files (metadata only; links not followed).
func countFiles(dir string) (datafs.Stats, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return datafs.Stats{}, err
	}
	defer r.Close()
	return datafs.Count(r)
}

func (d *DataOps) self() string {
	if d.Self != "" {
		return d.Self
	}
	p, err := os.Executable()
	if err != nil {
		return "/usr/local/bin/footops"
	}
	return p
}

// helper runs `footops <args>` in a limited scope; files become descriptors 3, 4, ….
func (d *DataOps) helper(ctx context.Context, jobID string, files []*os.File, args ...string) run.Cmd {
	unit := helperPrefix + jobID
	full := []string{"--scope", "--quiet", "--collect", "--slice=system.slice", "--unit=" + unit,
		"-p", "MemoryMax=" + strconv.Itoa(helperLimits.MemoryMB) + "M", "-p", "MemorySwapMax=0",
		"-p", "CPUQuota=" + strconv.Itoa(helperLimits.CPUPercent) + "%", "-p", "TasksMax=" + strconv.Itoa(helperLimits.PIDs),
		"--", "ionice", "-c", "3", "nice", "-n", "10", d.self()}
	full = append(full, args...)
	return run.Cmd{
		Name: "systemd-run", Args: full, ExtraFiles: files,
		Stop: func() {
			stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = d.Run.Run(stop, "systemctl", "kill", "--signal=SIGKILL", unit+".scope")
		},
		Cgroup: func() string { return run.CgroupRoot + "/system.slice/" + unit + ".scope" },
	}
}

// helperStats reads the helper's progress and result lines.
type helperStats struct {
	Result, Kept datafs.Stats
	done         bool
}

func (h *helperStats) line(t *Task, total int64) func(string) {
	return func(line string) {
		kind, js, ok := strings.Cut(line, " ")
		if !ok {
			return
		}
		var st datafs.Stats
		switch kind {
		case "FOOTOPS_PROGRESS":
			if json.Unmarshal([]byte(js), &st) == nil {
				t.SetBytes(st.Bytes, total)
			}
		case "FOOTOPS_STATS":
			if json.Unmarshal([]byte(js), &h.Result) == nil {
				h.done = true
			}
		case "FOOTOPS_KEPT":
			_ = json.Unmarshal([]byte(js), &h.Kept)
		}
	}
}

// filesSource is an archive of files to put into a site's data directory.
type filesSource struct {
	Label  string
	Path   string
	Sec    dbSection
	Format string // "zip", "tar", "tgz"
	Strip  string
	Expect datafs.Stats // what the extraction must produce (hidden entries left out)
	// Stream, when set, produces the archive instead of Path: a tar written by another
	// server over ssh (remote.TarScript), piped straight into the helper (the agent never
	// touches the bytes). StreamErr judges how it ended from its exit status and stderr
	// (nil: fine). Live: that server's site still runs, so files may change while they
	// are read and Expect (counted just before) is only compared, not required — but a
	// copy that brings far less than counted is refused.
	Stream    *run.Cmd
	StreamErr func(err error, log *remote.StreamLog) error
	Live      bool
}

// extract unpacks src into a fresh data.new owned by the site's user. With merge, the
// current directory's other files are linked in as well (the site must be stopped). It
// returns what data.new must hold: the archive's files plus the kept ones.
func (d *DataOps) extract(ctx context.Context, t *Task, sf siteFiles, src filesSource, merge bool) (datafs.Stats, error) {
	staged := sf.path(dataNew)
	if err := os.RemoveAll(staged); err != nil {
		return datafs.Stats{}, err
	}
	if err := os.Mkdir(staged, 0o750); err != nil {
		return datafs.Stats{}, err
	}
	if err := os.Chown(staged, sf.UID, sf.GID); err != nil {
		return datafs.Stats{}, err
	}
	// A whole upload (zip, tar.gz) is passed as descriptor 3. The files member of a
	// FootOps archive is streamed through stdin instead, so the helper never sees the rest
	// of the archive (the database dump); so is a tar coming from another server.
	var in *os.File
	if src.Stream == nil {
		f, err := os.Open(src.Path)
		if err != nil {
			return datafs.Stats{}, err
		}
		defer f.Close()
		in = f
	}
	dst, err := os.Open(staged)
	if err != nil {
		return datafs.Stats{}, err
	}
	defer dst.Close()
	files := []*os.File{nil, dst}
	args := []string{"data-extract", "--uid=" + strconv.Itoa(sf.UID), "--gid=" + strconv.Itoa(sf.GID), "--format=" + src.Format}
	if src.Strip != "" {
		args = append(args, "--strip="+src.Strip)
	}
	if merge {
		cur, err := os.Open(sf.Dir)
		if err != nil {
			return datafs.Stats{}, err
		}
		defer cur.Close()
		files = append(files, cur)
		args = append(args, "--merge")
	}
	var cmd run.Cmd
	switch {
	case src.Stream != nil:
		entries := src.Expect.Files + src.Expect.Dirs
		// Checked as it arrives (descriptor 3 stays closed): the sender's end line, the
		// disk's reserve, and no more than counted plus a margin for a live site.
		args = append(args, "--stdin", "--end-line", "--room",
			"--max-bytes="+strconv.FormatInt(src.Expect.Bytes+max(src.Expect.Bytes/2, 256<<20), 10),
			"--max-files="+strconv.Itoa(entries+max(entries/2, 1000)))
		cmd = d.helper(ctx, t.ID(), files, args...)
	case src.Sec.Offset == 0:
		files[0] = in
		cmd = d.helper(ctx, t.ID(), files, args...)
	default:
		args = append(args, "--stdin")
		cmd = d.helper(ctx, t.ID(), files, args...)
		cmd.Stdin = bufio.NewReaderSize(io.NewSectionReader(in, src.Sec.Offset, src.Sec.Length), 256<<10)
	}
	var hs helperStats
	cmd.Stderr = hs.line(t, src.Expect.Bytes)
	t.Step("… Dosyalar açılıyor (site kullanıcısı " + strconv.Itoa(sf.UID) + ", sınır: " + limitText(helperLimits) + ")")
	var u run.Usage
	if src.Stream != nil {
		u, err = d.streamInto(ctx, t, src, cmd)
	} else {
		u, err = d.Stream.Stream(ctx, cmd)
		t.AddUsage(u)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && !hs.done {
		err = errors.New("yardımcı sonuç bildirmedi")
	}
	if err != nil {
		_ = os.RemoveAll(staged)
		return hs.Result, fmt.Errorf("dosyalar açılamadı: %w", err)
	}
	st := hs.Result
	if src.Live && src.Expect.Files > 0 && st.Files < src.Expect.Files/2 {
		_ = os.RemoveAll(staged)
		return st, fmt.Errorf("kaynakta %d dosya sayılmıştı, yalnız %d geldi: kopya eksik, uygulanmadı", src.Expect.Files, st.Files)
	}
	if st.Files != src.Expect.Files || st.Bytes != src.Expect.Bytes {
		if !src.Live {
			_ = os.RemoveAll(staged)
			return st, fmt.Errorf("açılan dosyalar beklenenden farklı: %d dosya / %s (beklenen %d / %s)",
				st.Files, humanBytes(st.Bytes), src.Expect.Files, humanBytes(src.Expect.Bytes))
		}
		t.Step(fmt.Sprintf("ℹ Kaynakta az önce %d dosya / %s sayılmıştı, gelen %d / %s: site orada çalıştığı için arada değişmiş olabilir",
			src.Expect.Files, humanBytes(src.Expect.Bytes), st.Files, humanBytes(st.Bytes)))
	}
	msg := fmt.Sprintf("✓ Dosyalar açıldı: %d dosya, %d klasör, %s", st.Files, st.Dirs, humanBytes(st.Bytes))
	if merge {
		msg += fmt.Sprintf("; sitedeki diğer %d dosya korundu (bağlantıyla, kopyalanmadan)", hs.Kept.Files)
	}
	t.Step(msg + " — " + usageNote(u))
	return datafs.Stats{Files: st.Files + hs.Kept.Files, Bytes: st.Bytes + hs.Kept.Bytes}, nil
}

// swap puts data.new in place of data; the old directory becomes old (data.old-<time>).
// The recovery record says so before the first rename, so a crash in between is undone.
func (d *DataOps) swap(sf siteFiles, old string, rec *recovery) error {
	// Never over an existing copy: undoing would then put that one in place.
	if _, err := os.Lstat(sf.path(old)); err == nil {
		return fmt.Errorf("%s zaten var; biraz sonra tekrar dene", old)
	}
	rec.Files.Moved = true
	if err := d.saveRecovery(*rec); err != nil {
		return err
	}
	if err := os.Rename(sf.Dir, sf.path(old)); err != nil {
		return err
	}
	if err := os.Rename(sf.path(dataNew), sf.Dir); err != nil {
		_ = os.Rename(sf.path(old), sf.Dir)
		return err
	}
	return nil
}

// unswap puts the old directory back. What is in data moves to back, or aside to
// data.failed-<time> (removed later by the sweep), rather than being deleted on the spot.
func (d *DataOps) unswap(sf siteFiles, old, back string) error {
	if _, err := os.Lstat(sf.path(old)); err != nil {
		return nil // never moved
	}
	if _, err := os.Lstat(sf.Dir); err == nil {
		if back == "" {
			back = dataBadPrefix + d.now().UTC().Format("20060102-150405")
		}
		if _, err := os.Lstat(sf.path(back)); err == nil {
			back = dataBadPrefix + d.now().UTC().Format("20060102-150405")
		}
		if err := os.Rename(sf.Dir, sf.path(back)); err != nil {
			return err
		}
	}
	return os.Rename(sf.path(old), sf.Dir)
}

// freshOld returns a data.old-<time> name not taken yet: copies are named by the second,
// so a second operation within the same second waits for the next one.
func (d *DataOps) freshOld(sf siteFiles) string {
	for i := 0; ; i++ {
		name := dataOldPrefix + d.now().UTC().Format("20060102-150405")
		if _, err := os.Lstat(sf.path(name)); err != nil || i >= 3 {
			return name // swap refuses a taken name, should the clock stand still
		}
		d.sleep(time.Second)
	}
}

// FilesBackup is a previous data directory kept after an import (data.old-<time>).
type FilesBackup struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

// FilesBackups lists a site's previous data directories, newest first.
func (d *DataOps) FilesBackups(site string) ([]FilesBackup, error) {
	spec, err := d.Store.Get(site)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(filepath.Join(d.Store.Root, spec.Dir()))
	if err != nil {
		return nil, err
	}
	out := []FilesBackup{}
	for _, e := range ents {
		id, ok := strings.CutPrefix(e.Name(), dataOldPrefix)
		if !ok || !e.IsDir() || !reBackupID.MatchString(id) {
			continue
		}
		t, err := time.Parse("20060102-150405", id)
		if err == nil {
			out = append(out, FilesBackup{ID: id, Created: t})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out, nil
}

// SweepFiles removes previous data directories older than FilesKeep and leftovers of
// failed imports older than a day, for every site. Only when no job runs.
func (d *DataOps) SweepFiles() []string {
	d.init()
	list, err := d.Store.List()
	if err != nil {
		return nil
	}
	var removed []string
	now := d.now().UTC()
	for _, spec := range list {
		dir := filepath.Join(d.Store.Root, spec.Dir())
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			var keep time.Duration
			var id string
			switch {
			case strings.HasPrefix(name, dataOldPrefix):
				id, keep = strings.TrimPrefix(name, dataOldPrefix), FilesKeep
			case strings.HasPrefix(name, dataBadPrefix):
				id, keep = strings.TrimPrefix(name, dataBadPrefix), 24*time.Hour
			case name == dataNew:
				info, err := e.Info()
				if err == nil && now.Sub(info.ModTime()) > 24*time.Hour && os.RemoveAll(filepath.Join(dir, name)) == nil {
					removed = append(removed, spec.Name+"/"+name)
				}
				continue
			default:
				continue
			}
			t, err := time.Parse("20060102-150405", id)
			if err != nil || !reBackupID.MatchString(id) || now.Sub(t) < keep {
				continue
			}
			if os.RemoveAll(filepath.Join(dir, name)) == nil {
				removed = append(removed, spec.Name+"/"+name)
			}
		}
	}
	return removed
}
