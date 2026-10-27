package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/run"
)

// The file manager (F3): browse a site's data directory, upload into it, download from
// it, make folders, rename, move, delete to the trash and restore. As with imports the
// agent does not touch the files: each operation is one run of the helper (`footops
// data-fs`), which becomes the site's user and gets the data directory and the trash as
// open descriptors. Quick operations run it directly (a listing needs no systemd unit);
// opening an archive and zipping a folder run it in a limited scope like an import.
//
// The trash (/srv/<group>/<site>/trash) sits next to data/, so the app never sees it,
// on the same disk, so deleting is a rename. Entries stay for TrashKeep.
const (
	trashDirName = "trash"
	TrashKeep    = 7 * 24 * time.Hour
	fsTimeout    = time.Minute
	maxFSReply   = 4 << 20
)

// FilesError is a refusal of the helper the panel shows as it is.
type FilesError struct {
	Code string // "notfound", "exists", "invalid", or ""
	Msg  string
}

func (e *FilesError) Error() string { return e.Msg }

// limitedBuffer keeps at most max bytes (a reply is small; a broken helper must not
// grow the agent).
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.max {
		return 0, errors.New("yardımcının cevabı çok büyük")
	}
	return b.Buffer.Write(p)
}

// openDirNoFollow opens a folder the agent made itself (data/, trash/) for the helper.
func openDirNoFollow(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
}

// trashFor returns the site's trash folder, made on first use, owned by the site's user
// (the helper moves files into it) and closed to everyone else (0700, whatever the
// umask). If the image's user changed, the folder and what it holds follow, so the new
// user can still list and restore the old entries.
func (d *DataOps) trashFor(sf siteFiles) (string, error) {
	p := sf.path(trashDirName)
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.Mkdir(p, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(p, 0o700); err != nil {
			return "", err
		}
		return p, os.Lchown(p, sf.UID, sf.GID)
	case err != nil:
		return "", err
	case !st.IsDir():
		return "", errors.New("çöp kutusu bir klasör değil")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && (int(sys.Uid) != sf.UID || int(sys.Gid) != sf.GID) {
		if err := reown(p, sf.UID, sf.GID); err != nil {
			return "", err
		}
	}
	return p, nil
}

// reown gives a folder and everything in it to uid:gid. Root walks it through an
// os.Root and changes links themselves (Lchown), so nothing outside can be reached.
func reown(dir string, uid, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(p, uid, gid)
	})
}

// fsRun says how one file manager operation runs.
type fsRun struct {
	trash bool     // pass the trash (made if needed)
	extra *os.File // the operation's own file (an upload, a socket)
	// task runs the helper in a limited systemd scope as part of a job, which gets its
	// usage and progress; scope does the same outside a job (a copy, a zip).
	task    *Task
	scope   string
	room    bool          // the operation writes: keep the disk's reserve
	timeout time.Duration // quick operations: fsTimeout
}

// readOps only read: the request's end may cancel them. Anything else runs to its end
// even if the browser goes away (a move cut in half is worse than a slow answer).
var readOps = map[string]bool{"list": true, "dirs": true, "open": true, "trash-list": true}

// fsCall runs one file manager operation in the helper.
func (d *DataOps) fsCall(ctx context.Context, sf siteFiles, req datafs.Request, o fsRun) (datafs.Reply, error) {
	d.init()
	if req.Stamp.Now.IsZero() {
		req.Stamp.Now = d.now().UTC()
	}
	if !readOps[req.Op] {
		ctx = context.WithoutCancel(ctx)
	}
	if o.task == nil {
		if o.timeout == 0 {
			o.timeout = fsTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return datafs.Reply{}, err
	}
	data, err := openDirNoFollow(sf.Dir)
	if err != nil {
		return datafs.Reply{}, fmt.Errorf("kalıcı klasör açılamadı: %w", err)
	}
	defer data.Close()
	files := []*os.File{data, nil, nil}
	args := []string{"data-fs", "--uid=" + strconv.Itoa(sf.UID), "--gid=" + strconv.Itoa(sf.GID)}
	if o.trash {
		p, err := d.trashFor(sf)
		if err != nil {
			return datafs.Reply{}, fmt.Errorf("çöp kutusu: %w", err)
		}
		tr, err := openDirNoFollow(p)
		if err != nil {
			return datafs.Reply{}, fmt.Errorf("çöp kutusu açılamadı: %w", err)
		}
		defer tr.Close()
		files[1] = tr
		args = append(args, "--trash")
	}
	if o.extra != nil {
		files[2] = o.extra
		args = append(args, "--extra")
	}
	if o.room {
		args = append(args, "--room")
	}
	out := &limitedBuffer{max: maxFSReply}
	var cmd run.Cmd
	switch {
	case o.task != nil:
		cmd = d.helper(ctx, o.task.ID(), files, args...)
		var hs helperStats
		cmd.Stderr = hs.line(o.task, 0)
	case o.scope != "":
		cmd = d.helper(ctx, o.scope, files, args...)
	default:
		cmd = run.Cmd{Name: d.self(), Args: args, ExtraFiles: files}
	}
	cmd.Stdin, cmd.Stdout = bytes.NewReader(body), out
	u, err := d.Stream.Stream(ctx, cmd)
	if o.task != nil {
		o.task.AddUsage(u)
	}
	if err != nil {
		return datafs.Reply{}, fmt.Errorf("dosya işlemi yapılamadı: %w", err)
	}
	var r datafs.Reply
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		return datafs.Reply{}, errors.New("yardımcının cevabı anlaşılamadı")
	}
	if r.Error != "" {
		return r, &FilesError{Code: r.Code, Msg: r.Error}
	}
	return r, nil
}

// stamp is the actor and time recorded in trash entries.
func (d *DataOps) stamp(a Actor) datafs.Stamp {
	d.init()
	return datafs.Stamp{Actor: a.User, Now: d.now().UTC()}
}

// ListFiles returns one page of a folder.
func (d *DataOps) ListFiles(ctx context.Context, site, dir string, opt datafs.ListOptions) (datafs.Listing, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return datafs.Listing{}, err
	}
	r, err := d.fsCall(ctx, sf, datafs.Request{Op: "list", Dir: dir, List: opt}, fsRun{})
	if err != nil || r.Listing == nil {
		return datafs.Listing{}, orErr(err, "liste gelmedi")
	}
	l := *r.Listing
	l.Trash = trashCount(sf.path(trashDirName))
	return l, nil
}

// trashCount counts a site's trash entries by their names (no file is opened).
func trashCount(dir string) int {
	ents, _ := os.ReadDir(dir)
	n := 0
	for _, e := range ents {
		if datafs.TrashIDPattern.MatchString(e.Name()) {
			n++
		}
	}
	return n
}

func orErr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

// FileDirs lists every folder of the data directory (the move dialog).
func (d *DataOps) FileDirs(ctx context.Context, site string) ([]string, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return nil, err
	}
	r, err := d.fsCall(ctx, sf, datafs.Request{Op: "dirs"}, fsRun{})
	return r.Dirs, err
}

// OpenFile opens a file for download. The helper opens it as the site's user and hands
// the open file back over a socket, so the agent serves it (with ranges) without
// opening anything in the data directory itself.
func (d *DataOps) OpenFile(ctx context.Context, site, p string) (*os.File, datafs.FileInfo, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return nil, datafs.FileInfo{}, err
	}
	mine, theirs, err := datafs.SocketPair()
	if err != nil {
		return nil, datafs.FileInfo{}, err
	}
	defer mine.Close()
	r, err := d.fsCall(ctx, sf, datafs.Request{Op: "open", Path: p}, fsRun{extra: theirs})
	theirs.Close() // the helper has exited: nothing more can come but end of file
	if err != nil || r.File == nil {
		return nil, datafs.FileInfo{}, orErr(err, "dosya bilgisi gelmedi")
	}
	f, err := datafs.ReceiveFile(mine, r.File.Name)
	if err != nil {
		return nil, datafs.FileInfo{}, err
	}
	return f, *r.File, nil
}

// FilesChange is a file manager change the panel asks for.
type FilesChange struct {
	Op    string   `json:"op"` // mkdir, rename, move, trash
	Dir   string   `json:"dir"`
	Name  string   `json:"name,omitempty"`
	Names []string `json:"names,omitempty"`
	To    string   `json:"to,omitempty"`
	Actor Actor    `json:"actor"`
}

// ChangeFiles makes a folder, renames, moves or deletes to the trash.
func (d *DataOps) ChangeFiles(ctx context.Context, site string, c FilesChange) (*datafs.TrashEntry, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return nil, err
	}
	req := datafs.Request{Op: c.Op, Dir: c.Dir, Name: c.Name, Names: c.Names, To: c.To, Stamp: d.stamp(c.Actor)}
	switch c.Op {
	case "mkdir", "rename", "move":
		_, err := d.fsCall(ctx, sf, req, fsRun{})
		return nil, err
	case "trash":
		r, err := d.fsCall(ctx, sf, req, fsRun{trash: true})
		return r.Entry, err
	}
	return nil, &FilesError{Code: "invalid", Msg: "bilinmeyen işlem " + c.Op}
}

// placeCopyTimeout bounds copying an upload that cannot be linked (2 GB at least
// ~10 MB/s).
const placeCopyTimeout = 30 * time.Minute

// PlaceUpload puts a finished upload into a folder of the data directory and removes
// it from the staging area. The upload is handed to the site's user first, so the
// helper can link it in (no copy) when both are on one disk, which is quick and runs
// like any small change. Where a link is not possible the helper says so, and the copy
// then runs in a limited scope, keeping the disk's reserve.
func (d *DataOps) PlaceUpload(ctx context.Context, site, upload, dir string, overwrite bool, actor Actor) (datafs.Placed, error) {
	if !reUploadID.MatchString(upload) {
		return datafs.Placed{}, ErrNoUpload
	}
	defer d.lockUpload(upload)()
	u, p, err := d.uploadFile(site, upload)
	if err != nil {
		return datafs.Placed{}, err
	}
	sf, err := d.siteFiles(site)
	if err != nil {
		return datafs.Placed{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return datafs.Placed{}, err
	}
	defer f.Close()
	// The staging folder stays root's: the site's user reaches this one file only through
	// the descriptor.
	if err := f.Chown(sf.UID, sf.GID); err != nil {
		return datafs.Placed{}, err
	}
	if err := f.Chmod(0o644); err != nil {
		return datafs.Placed{}, err
	}
	req := datafs.Request{Op: "place", Dir: dir, Name: u.Name, Overwrite: overwrite, NoCopy: true, Stamp: d.stamp(actor)}
	r, err := d.fsCall(ctx, sf, req, fsRun{trash: true, extra: f})
	var fe *FilesError
	if errors.As(err, &fe) && fe.Code == "copy" {
		req.NoCopy = false
		r, err = d.fsCall(ctx, sf, req, fsRun{trash: true, extra: f, scope: "place" + upload[:12], room: true, timeout: placeCopyTimeout})
	}
	if err != nil || r.Placed == nil {
		return datafs.Placed{}, orErr(err, "yerleştirme sonucu gelmedi")
	}
	_ = d.Staging.Remove(uploadPrefix + upload)
	return *r.Placed, nil
}

// TrashList lists a site's trash, newest first. A site whose trash was never used has
// an empty one.
func (d *DataOps) TrashList(ctx context.Context, site string) ([]datafs.TrashEntry, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(sf.path(trashDirName)); errors.Is(err, fs.ErrNotExist) {
		return []datafs.TrashEntry{}, nil
	}
	r, err := d.fsCall(ctx, sf, datafs.Request{Op: "trash-list"}, fsRun{trash: true})
	if r.Trash == nil {
		r.Trash = []datafs.TrashEntry{}
	}
	return r.Trash, err
}

// Untrash puts a trash entry back where it came from.
func (d *DataOps) Untrash(ctx context.Context, site, id string) (datafs.Restored, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return datafs.Restored{}, err
	}
	r, err := d.fsCall(ctx, sf, datafs.Request{Op: "untrash", ID: id}, fsRun{trash: true})
	if err != nil || r.Restored == nil {
		return datafs.Restored{}, orErr(err, "geri alma sonucu gelmedi")
	}
	return *r.Restored, nil
}

// EmptyTrash deletes for good a site's trash entries made before now (root removes them:
// an entry may hold folders the site's user cannot empty). An entry made meanwhile —
// something deleted while the trash was being emptied — stays. It returns how many were
// removed and their size.
func (d *DataOps) EmptyTrash(site string) (int, int64, error) {
	d.init()
	sf, err := d.siteFiles(site)
	if err != nil {
		return 0, 0, err
	}
	cutoff := d.now().UTC().Truncate(time.Second)
	return removeTrash(sf.path(trashDirName), func(name string, made time.Time) bool {
		return datafs.TrashIDPattern.MatchString(name) && made.Before(cutoff)
	})
}

// removeTrash removes the trash entries (and unpack work folders) for which drop says
// so, given the entry's name and the time it was made.
func removeTrash(dir string, drop func(name string, made time.Time) bool) (int, int64, error) {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0, err
	}
	var n int
	var size int64
	var errs []error
	for _, e := range ents {
		name := e.Name()
		entry := datafs.TrashIDPattern.MatchString(name)
		made, ok := workTime(name)
		if entry {
			made, _ = time.Parse("20060102-150405", name[:15])
		} else if !ok {
			continue
		}
		if !drop(name, made) {
			continue
		}
		var meta datafs.TrashEntry
		if entry {
			meta = readMeta(root, name)
		}
		if err := root.RemoveAll(name); err != nil {
			errs = append(errs, err)
			continue
		}
		if entry {
			n++
			size += meta.Bytes
		}
	}
	return n, size, errors.Join(errs...)
}

// workTime is when a work folder or file in the trash (an unpack, an upload on its way)
// was made, read from its name.
func workTime(name string) (time.Time, bool) {
	for _, prefix := range []string{datafs.UnpackPrefix, datafs.UploadPrefix} {
		if rest, ok := strings.CutPrefix(name, prefix); ok && len(rest) >= 15 {
			t, err := time.Parse("20060102-150405", rest[:15])
			return t, err == nil
		}
	}
	return time.Time{}, false
}

// readMeta reads an entry's record for its size. The site's user owns the trash, so the
// file is opened without following a link or blocking on a FIFO, must be a regular
// file, and only its first 64 KB are read.
func readMeta(root *os.Root, name string) datafs.TrashEntry {
	var e datafs.TrashEntry
	f, err := root.OpenFile(name+"/meta.json", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return e
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return e
	}
	_ = json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&e)
	return e
}

// SweepTrash removes trash entries older than TrashKeep and work folders older than a
// day, for every site. Only while nothing runs (housekeeping).
func (d *DataOps) SweepTrash() []string {
	d.init()
	list, err := d.Store.List()
	if err != nil {
		return nil
	}
	now := d.now().UTC()
	var removed []string
	for _, spec := range list {
		dir := filepath.Join(d.Store.Root, spec.Dir(), trashDirName)
		_, _, _ = removeTrash(dir, func(name string, made time.Time) bool {
			keep := TrashKeep
			if !datafs.TrashIDPattern.MatchString(name) {
				keep = 24 * time.Hour
			}
			if now.Sub(made) < keep {
				return false
			}
			removed = append(removed, spec.Name+"/"+trashDirName+"/"+name)
			return true
		})
	}
	return removed
}

// zipBusy lets one zip download run at a time (rule K7: one heavy thing at a time).
var zipBusy atomic.Bool

// ErrZipBusy refuses a second zip download while one is streaming.
var ErrZipBusy = errors.New("başka bir zip indirmesi sürüyor; bitince tekrar dene")

// Zip streams names of dir as a zip to w (everything in dir when names is empty), made
// by the helper in a limited scope. Nothing is staged.
func (d *DataOps) Zip(ctx context.Context, site, dir string, names []string, w io.Writer) (datafs.Stats, run.Usage, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return datafs.Stats{}, run.Usage{}, err
	}
	if !zipBusy.CompareAndSwap(false, true) {
		return datafs.Stats{}, run.Usage{}, ErrZipBusy
	}
	defer zipBusy.Store(false)
	body, err := json.Marshal(datafs.Request{Op: "zip", Dir: dir, Names: names})
	if err != nil {
		return datafs.Stats{}, run.Usage{}, err
	}
	data, err := openDirNoFollow(sf.Dir)
	if err != nil {
		return datafs.Stats{}, run.Usage{}, err
	}
	defer data.Close()
	id, err := randomSecret(12)
	if err != nil {
		return datafs.Stats{}, run.Usage{}, err
	}
	cmd := d.helper(ctx, "zip"+id, []*os.File{data}, "data-zip", "--uid="+strconv.Itoa(sf.UID), "--gid="+strconv.Itoa(sf.GID))
	var st datafs.Stats
	var done bool
	var failed *FilesError
	cmd.Stdin, cmd.Stdout = bytes.NewReader(body), w
	cmd.Stderr = func(line string) {
		if js, ok := strings.CutPrefix(line, "FOOTOPS_STATS "); ok && json.Unmarshal([]byte(js), &st) == nil {
			done = true
		}
		var r datafs.Reply
		if js, ok := strings.CutPrefix(line, "FOOTOPS_ERROR "); ok && json.Unmarshal([]byte(js), &r) == nil {
			failed = &FilesError{Code: r.Code, Msg: r.Error}
		}
	}
	u, err := d.Stream.Stream(ctx, cmd)
	switch {
	case err != nil && failed != nil:
		err = failed // the helper's own words, with a status the panel can use
	case err == nil && !done:
		err = errors.New("yardımcı sonuç bildirmedi")
	}
	return st, u, err
}

// Unpack opens an archive of the data directory in its folder (a job: it may take a
// while and runs in a limited scope).
func (d *DataOps) Unpack(ctx context.Context, t *Task, site, p string, trashArchive bool, actor Actor) (datafs.Unpacked, error) {
	sf, err := d.siteFiles(site)
	if err != nil {
		return datafs.Unpacked{}, err
	}
	t.Step("… Arşiv açılıyor: " + p + " (site kullanıcısı " + strconv.Itoa(sf.UID) + ", sınır: " + limitText(helperLimits) + ")")
	req := datafs.Request{Op: "unpack", Path: p, TrashArchive: trashArchive, Stamp: d.stamp(actor)}
	r, err := d.fsCall(ctx, sf, req, fsRun{trash: true, task: t, room: true})
	if err != nil || r.Unpacked == nil {
		return datafs.Unpacked{}, orErr(err, "açma sonucu gelmedi")
	}
	u := *r.Unpacked
	t.SetBytes(u.Bytes, u.Bytes)
	t.Step(fmt.Sprintf("✓ Açıldı: %d dosya, %d klasör, %s → %s — %s", u.Files, u.Dirs, humanBytes(u.Bytes), "/"+u.Dir, usageNote(t.Usage())))
	if u.Skipped > 0 {
		t.Step(fmt.Sprintf("ℹ %d gizli dosya (nokta ile başlayan, __MACOSX) alınmadı", u.Skipped))
	}
	if u.Trashed != "" {
		t.Step("✓ Arşiv çöp kutusuna taşındı (" + strconv.Itoa(int(TrashKeep.Hours()/24)) + " gün geri alınabilir)")
	}
	t.SetResult(u.Dir)
	return u, nil
}
