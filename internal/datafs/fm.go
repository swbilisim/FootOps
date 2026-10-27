//go:build linux || darwin

package datafs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/swbilisim/FootOps/internal/archive"
)

// The file manager (the panel's "Dosyalar" page). Like an import it runs in the helper,
// as the site's user. Every folder is opened one name at a time from the data directory
// with O_NOFOLLOW, so no symbolic link is ever followed (not even one that stays
// inside), and every change is a *at system call on a single name in an open folder.
// Nothing is overwritten: renames use RENAME_NOREPLACE, a replaced upload trades places
// with the old file in one step and the old one goes to the trash (trash.go).

// Errors the panel shows as "not found" (404), "taken" (409) and "invalid" (422).
var (
	ErrNotFound = errors.New("bulunamadı")
	ErrExists   = errors.New("aynı adda bir öğe var")
	ErrInvalid  = errors.New("geçersiz istek")
)

// fsErr names the item and maps a system error to one of the above where it fits.
func fsErr(name string, err error) error {
	var errno unix.Errno
	if err == nil || !errors.As(err, &errno) {
		return err
	}
	switch errno {
	case unix.ENOENT:
		return fmt.Errorf("%s: %w", show(name), ErrNotFound)
	case unix.EEXIST, unix.ENOTEMPTY:
		return fmt.Errorf("%s: %w", show(name), ErrExists)
	case unix.ELOOP:
		return fmt.Errorf("%s: bir bağlantı; bağlantılar izlenmez: %w", show(name), ErrInvalid)
	case unix.ENOTDIR:
		return fmt.Errorf("%s: klasör değil: %w", show(name), ErrInvalid)
	case unix.EINVAL:
		return fmt.Errorf("%s: bu işlem yapılamaz (bir klasör kendi içine taşınamaz): %w", show(name), ErrInvalid)
	case unix.EACCES, unix.EPERM:
		return fmt.Errorf("%s: izin yok (sitenin kullanıcısı buna dokunamıyor)", show(name))
	case unix.EXDEV:
		return fmt.Errorf("%s: başka bir diskte, taşınamaz", show(name))
	case unix.ENOSPC:
		return fmt.Errorf("%s: diskte yer kalmadı", show(name))
	}
	return fmt.Errorf("%s: %w", show(name), err)
}

// show is a path as the panel shows it ("" is the data directory itself).
func show(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// CleanPath checks a path inside the data directory: "", "." and "/" are the directory
// itself; anything else must be a clean relative path with no hidden part (names
// starting with a dot are neither shown nor touched, as in imports and exports).
func CleanPath(p string) (string, error) {
	p = strings.TrimPrefix(p, "/")
	c, err := archive.CleanName(p)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	case c != "" && hidden(c):
		return "", fmt.Errorf("%w: %q gizli (nokta ile başlıyor)", ErrInvalid, c)
	}
	return c, nil
}

// CheckName checks one name: a new name, or an existing item the panel acts on.
func CheckName(n string) error {
	c, err := archive.CleanName(n)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	case c == "" || c != n || strings.Contains(n, "/"):
		return fmt.Errorf("%w: %q bir ad değil", ErrInvalid, n)
	case hidden(n):
		return fmt.Errorf("%w: %q nokta ile başlıyor; gizli adlar kullanılmaz", ErrInvalid, n)
	}
	return nil
}

// checkNames checks a list of names (one folder's selection): valid, distinct.
func checkNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("%w: hiçbir öğe seçilmedi", ErrInvalid)
	}
	if len(names) > maxNames {
		return fmt.Errorf("%w: tek seferde en çok %d öğe", ErrInvalid, maxNames)
	}
	seen := map[string]bool{}
	for _, n := range names {
		if err := CheckName(n); err != nil {
			return err
		}
		if seen[n] {
			return fmt.Errorf("%w: %q iki kez seçilmiş", ErrInvalid, n)
		}
		seen[n] = true
	}
	return nil
}

const (
	maxNames     = 1000    // items in one operation
	maxListed    = 100_000 // entries of one folder
	defaultLimit = 200
	maxLimit     = 1000
)

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentOf(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// dirfd is an open folder inside a root.
type dirfd struct {
	fd   int
	path string // relative to the root, "" for the root itself
}

func (d dirfd) close() {
	if d.fd >= 0 {
		unix.Close(d.fd)
	}
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// rootFD opens the root itself as a descriptor of its own.
func rootFD(root *os.Root) (int, error) {
	f, err := root.Open(".")
	if err != nil {
		return -1, err
	}
	defer f.Close()
	return unix.Openat(int(f.Fd()), ".", dirFlags, 0)
}

// openDir opens dir (clean; "" is the root) one name at a time, refusing links on every
// component, not only the last.
func openDir(root *os.Root, dir string) (dirfd, error) {
	fd, err := rootFD(root)
	if err != nil {
		return dirfd{fd: -1}, err
	}
	if dir == "" {
		return dirfd{fd: fd}, nil
	}
	for _, c := range strings.Split(dir, "/") {
		next, err := unix.Openat(fd, c, dirFlags, 0)
		unix.Close(fd)
		if err != nil {
			return dirfd{fd: -1}, fsErr(dir, err)
		}
		fd = next
	}
	return dirfd{fd: fd, path: dir}, nil
}

// mkdirAll is openDir that creates missing folders (a restore into a folder deleted
// since).
func mkdirAll(root *os.Root, dir string) (dirfd, error) {
	fd, err := rootFD(root)
	if err != nil || dir == "" {
		return dirfd{fd: fd}, err
	}
	for _, c := range strings.Split(dir, "/") {
		next, err := unix.Openat(fd, c, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(fd, c, dirMode); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, c, dirFlags, 0)
			}
		}
		unix.Close(fd)
		if err != nil {
			return dirfd{fd: -1}, fsErr(dir, err)
		}
		fd = next
	}
	return dirfd{fd: fd, path: dir}, nil
}

func openSub(dir int, name string) (int, error) { return unix.Openat(dir, name, dirFlags, 0) }

// dirent is a name in a folder and its type as the folder lists it.
type dirent struct {
	name string
	typ  fs.FileMode
}

// entries reads a folder's names, at most max. It opens the folder afresh: a folder
// descriptor's read position is shared by its duplicates.
func entries(dir int, max int) ([]dirent, error) {
	fd, err := unix.Openat(dir, ".", dirFlags, 0)
	if err != nil {
		return nil, err
	}
	// The name matters where a folder does not report entry types: Go then looks the
	// entry up by path (and skips it if that fails).
	f := os.NewFile(uintptr(fd), fdPath(fd))
	defer f.Close()
	var out []dirent
	for {
		list, err := f.ReadDir(256)
		for _, e := range list {
			if len(out) >= max {
				return out, fmt.Errorf("klasörde çok fazla öğe var (sınır %d)", max)
			}
			out = append(out, dirent{e.Name(), e.Type()})
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
	}
}

func lstatAt(dir int, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	return st, err
}

func typeOf(st *unix.Stat_t) string {
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFREG:
		return "file"
	case unix.S_IFLNK:
		return "link"
	}
	return "other"
}

func mtimeOf(st *unix.Stat_t) time.Time { return time.Unix(st.Mtim.Unix()).UTC() }

// Entry is one item of a folder.
type Entry struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // "dir", "file", "link" (never followed), "other"
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
	// Odd marks a name the panel cannot act on (invalid UTF-8, a control character, …):
	// it is shown, with its unusable parts replaced, but offers no actions.
	Odd bool `json:"odd,omitempty"`
}

// Listing is one page of a folder, folders first.
type Listing struct {
	Dir     string  `json:"dir"`
	Entries []Entry `json:"entries"`
	Offset  int     `json:"offset"`
	Total   int     `json:"total"` // entries on all pages
	Dirs    int     `json:"dirs"`
	Files   int     `json:"files"`
	Bytes   int64   `json:"bytes"`  // the files directly in this folder
	Hidden  int     `json:"hidden"` // names starting with a dot, left out
	Trash   int     `json:"trash"`  // entries in the site's trash (filled in by the agent)
}

// ListOptions picks the page.
type ListOptions struct {
	Sort     string `json:"sort,omitempty"` // "name" (default), "time" (newest first), "size" (largest first)
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`     // default 200, at most 1000
	DirsOnly bool   `json:"dirs_only,omitempty"` // the folder picker of "move"
}

// List reads one page of dir.
func List(root *os.Root, dir string, opt ListOptions) (Listing, error) {
	dir, err := CleanPath(dir)
	if err != nil {
		return Listing{}, err
	}
	d, err := openDir(root, dir)
	if err != nil {
		return Listing{}, err
	}
	defer d.close()
	ents, err := entries(d.fd, maxListed)
	if err != nil {
		return Listing{}, fsErr(dir, err)
	}
	out := Listing{Dir: dir, Entries: []Entry{}}
	all := make([]Entry, 0, len(ents))
	for _, e := range ents {
		if hidden(e.name) {
			out.Hidden++
			continue
		}
		st, err := lstatAt(d.fd, e.name)
		if errors.Is(err, unix.ENOENT) {
			continue // removed meanwhile
		}
		if err != nil {
			return out, fsErr(join(dir, e.name), err)
		}
		en := Entry{Name: e.name, Type: typeOf(&st), MTime: mtimeOf(&st)}
		if CheckName(e.name) != nil {
			en.Name, en.Odd = strings.ToValidUTF8(strings.Map(printable, e.name), "?"), true
		}
		switch en.Type {
		case "dir":
			out.Dirs++
		case "file":
			en.Size = st.Size
			out.Files++
			out.Bytes += st.Size
		}
		if opt.DirsOnly && (en.Type != "dir" || en.Odd) {
			continue
		}
		all = append(all, en)
	}
	sortEntries(all, opt.Sort)
	out.Total = len(all)
	limit := opt.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	limit = min(limit, maxLimit)
	out.Offset = max(0, min(opt.Offset, len(all)))
	out.Entries = append(out.Entries, all[out.Offset:min(out.Offset+limit, len(all))]...)
	return out, nil
}

func printable(r rune) rune {
	if unicode.IsPrint(r) {
		return r
	}
	return '?'
}

// sortEntries puts folders first, then orders by the chosen key; names break ties in
// Turkish alphabetical order (ç after c, ı before i, …), not by byte.
func sortEntries(list []Entry, by string) {
	keys := make(map[string]string, len(list))
	for _, e := range list {
		keys[e.Name] = SortKey(e.Name)
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		switch by {
		case "time":
			if !a.MTime.Equal(b.MTime) {
				return a.MTime.After(b.MTime)
			}
		case "size":
			if a.Size != b.Size {
				return a.Size > b.Size
			}
		}
		if ka, kb := keys[a.Name], keys[b.Name]; ka != kb {
			return ka < kb
		}
		return a.Name < b.Name
	})
}

// trOrder places the Turkish letters right after their Latin neighbours.
var trOrder = strings.NewReplacer("ç", "c~", "ğ", "g~", "ı", "h~", "ö", "o~", "ş", "s~", "ü", "u~")

// SortKey orders names the way a Turkish reader expects, ignoring case.
func SortKey(name string) string {
	return trOrder.Replace(strings.ToLowerSpecial(unicode.TurkishCase, name))
}

// maxDirs bounds Dirs: the folder list of the move dialog.
const maxDirs = 5000

// Dirs lists every folder of the data directory (paths, Turkish order), for choosing
// where to move things. Hidden folders and links are left out.
func Dirs(root *os.Root) ([]string, error) {
	d, err := openDir(root, "")
	if err != nil {
		return nil, err
	}
	defer d.close()
	out := []string{}
	w := walker{budget: maxCount, visit: func(_ int, rel, name string, st *unix.Stat_t) (bool, error) {
		if typeOf(st) != "dir" || hidden(name) || CheckName(name) != nil {
			return false, nil
		}
		if len(out) >= maxDirs {
			return false, fmt.Errorf("çok fazla klasör (sınır %d)", maxDirs)
		}
		out = append(out, rel)
		return true, nil
	}, unreadable: func(string, error) {}} // a folder it cannot read is still listed itself
	err = w.walk(d.fd, "")
	if errors.Is(err, errTooMany) {
		err = fmt.Errorf("klasörde çok fazla öğe var (sınır %d)", maxCount)
	}
	sort.Slice(out, func(a, b int) bool { return SortKey(out[a]) < SortKey(out[b]) })
	return out, err
}

// OpenFile opens a regular file for reading (a download). Links are not followed.
func OpenFile(root *os.Root, p string) (*os.File, error) {
	p, err := CleanPath(p)
	if err != nil {
		return nil, err
	}
	if p == "" {
		return nil, fmt.Errorf("%w: dosya seçilmedi", ErrInvalid)
	}
	d, err := openDir(root, parentOf(p))
	if err != nil {
		return nil, err
	}
	defer d.close()
	// Non-blocking, so a FIFO put in its place cannot hang the helper.
	fd, err := unix.Openat(d.fd, path.Base(p), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fsErr(p, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || typeOf(&st) != "file" {
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s: yalnız dosyalar indirilebilir: %w", p, ErrInvalid)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), path.Base(p)), nil
}

// Mkdir creates the folder name in dir.
func Mkdir(root *os.Root, dir, name string) error {
	dir, err := CleanPath(dir)
	if err != nil {
		return err
	}
	if err := CheckName(name); err != nil {
		return err
	}
	d, err := openDir(root, dir)
	if err != nil {
		return err
	}
	defer d.close()
	return fsErr(join(dir, name), unix.Mkdirat(d.fd, name, dirMode))
}

// Rename gives the item from in dir the name to; a taken name is refused.
func Rename(root *os.Root, dir, from, to string) error {
	dir, err := CleanPath(dir)
	if err != nil {
		return err
	}
	for _, n := range []string{from, to} {
		if err := CheckName(n); err != nil {
			return err
		}
	}
	if from == to {
		return fmt.Errorf("%w: yeni ad eskisiyle aynı", ErrInvalid)
	}
	d, err := openDir(root, dir)
	if err != nil {
		return err
	}
	defer d.close()
	return renameErr(dir, from, to, renameNoReplace(d.fd, from, d.fd, to))
}

// renameErr names the side a failed rename is about: the source is missing, or the
// target is taken.
func renameErr(dir, from, to string, err error) error {
	if errors.Is(err, unix.ENOENT) {
		return fsErr(join(dir, from), err)
	}
	return fsErr(join(dir, to), err)
}

// Move moves names from dir into to. A taken name in to moves nothing; a move that
// fails half-way puts back the ones already moved.
func Move(root *os.Root, dir string, names []string, to string) error {
	dir, err := CleanPath(dir)
	if err != nil {
		return err
	}
	if to, err = CleanPath(to); err != nil {
		return err
	}
	if err := checkNames(names); err != nil {
		return err
	}
	if dir == to {
		return fmt.Errorf("%w: seçilenler zaten %s klasöründe", ErrInvalid, show(to))
	}
	for _, n := range names {
		if p := join(dir, n); to == p || strings.HasPrefix(to, p+"/") {
			return fmt.Errorf("%w: %s kendi içine taşınamaz", ErrInvalid, p)
		}
	}
	src, err := openDir(root, dir)
	if err != nil {
		return err
	}
	defer src.close()
	dst, err := openDir(root, to)
	if err != nil {
		return err
	}
	defer dst.close()
	var taken []string
	for _, n := range names {
		if _, err := lstatAt(dst.fd, n); err == nil {
			taken = append(taken, n)
		} else if !errors.Is(err, unix.ENOENT) {
			return fsErr(join(to, n), err)
		}
	}
	if len(taken) > 0 {
		return fmt.Errorf("%s klasöründe zaten var: %s: %w", show(to), list(taken), ErrExists)
	}
	var moved []string
	for _, n := range names {
		if err := renameNoReplace(src.fd, n, dst.fd, n); err != nil {
			for i := len(moved) - 1; i >= 0; i-- {
				_ = renameNoReplace(dst.fd, moved[i], src.fd, moved[i])
			}
			return renameErr(dir, n, n, err)
		}
		moved = append(moved, n)
	}
	return nil
}

// list shows up to ten names.
func list(names []string) string {
	if len(names) > 10 {
		return strings.Join(names[:10], ", ") + fmt.Sprintf(" ve %d tane daha", len(names)-10)
	}
	return strings.Join(names, ", ")
}

// Stamp says who acts and when; trash entries record it.
type Stamp struct {
	Actor string    `json:"actor,omitempty"`
	Now   time.Time `json:"now"`
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// tempName is a hidden name for work in progress (never listed, never exported).
func tempName(kind string) string { return ".footops-" + kind + "-" + randomHex(6) }

// numbered is the i-th alternative of a taken name: "foto (1).jpg", "arsiv (2).tar.gz".
func numbered(name string, i int) string {
	if i == 0 {
		return name
	}
	ext := path.Ext(name)
	lower := strings.ToLower(name)
	for _, double := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst"} {
		if strings.HasSuffix(lower, double) && len(name) > len(double) {
			ext = name[len(name)-len(double):]
		}
	}
	if ext == name {
		ext = ""
	}
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext)
}

// renameFree moves from to the first free name of want, want (1), want (2), ….
func renameFree(srcDir int, from string, dstDir int, want string) (string, error) {
	for i := 0; i < 1000; i++ {
		cand := numbered(want, i)
		err := renameNoReplace(srcDir, from, dstDir, cand)
		if err == nil {
			return cand, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return "", err
		}
	}
	return "", unix.EEXIST
}

// Placed is an uploaded file put into the data directory.
type Placed struct {
	Name    string `json:"name"` // the name it got: "foto (1).jpg" when foto.jpg was taken
	Size    int64  `json:"size"`
	Linked  bool   `json:"linked"`            // linked from the upload, no copy made
	Trashed string `json:"trashed,omitempty"` // trash entry holding the file it replaced
}

// PlaceOptions shape how an upload is put in place.
type PlaceOptions struct {
	// Overwrite replaces a file of the same name (it goes to the trash); otherwise the
	// new one gets a numbered name.
	Overwrite bool
	// NoCopy fails with ErrNeedsCopy where a link is not possible, so the agent can run
	// the copy in a limited scope instead.
	NoCopy bool
	// Room is asked before a copy whether the disk has room for it.
	Room func(int64) error
}

// ErrNeedsCopy: the upload cannot be linked into the data directory (another disk) and
// copying was not allowed.
var ErrNeedsCopy = errors.New("yükleme bağlantıyla konamıyor, kopyalanması gerekiyor")

// Place puts an uploaded file (src, already the site user's) into dir as name. The file
// first gets a name in the trash — a link, or a copy where a link is not possible — so a
// half-copied file never shows in data/, and is then moved in: under its own name, or a
// numbered one if that is taken ("foto (1).jpg"). With overwrite, that first name is in
// a new trash entry and the new file trades places with the old one in one step: the old
// one is in the trash from the moment it leaves, and the app never sees the name missing.
func Place(root, trash *os.Root, src *os.File, dir, name string, opt PlaceOptions, st Stamp) (Placed, error) {
	dir, err := CleanPath(dir)
	if err != nil {
		return Placed{}, err
	}
	if err := CheckName(name); err != nil {
		return Placed{}, err
	}
	info, err := src.Stat()
	if err != nil {
		return Placed{}, err
	}
	if !info.Mode().IsRegular() {
		return Placed{}, fmt.Errorf("%w: yüklenen bir dosya değil", ErrInvalid)
	}
	d, err := openDir(root, dir)
	if err != nil {
		return Placed{}, err
	}
	defer d.close()
	tr, err := openDir(trash, "")
	if err != nil {
		return Placed{}, fmt.Errorf("çöp kutusu: %w", err)
	}
	defer tr.close()
	out := Placed{Size: info.Size()}
	// put gives the upload the name n in the folder at, without ever writing into data/.
	put := func(at int, n string) error {
		if err := linkOpen(src, at, n); err == nil {
			out.Linked = true
			return nil
		}
		if opt.NoCopy {
			return ErrNeedsCopy
		}
		if opt.Room != nil {
			if err := opt.Room(info.Size()); err != nil {
				return err
			}
		}
		if err := copyOpen(src, info.Size(), at, n); err != nil {
			_ = unix.Unlinkat(at, n, 0)
			return fsErr(join(dir, name), err)
		}
		return nil
	}

	if opt.Overwrite {
		old, err := lstatAt(d.fd, name)
		switch {
		case err == nil && typeOf(&old) == "dir":
			return out, fmt.Errorf("%s: aynı adda bir klasör var: %w", join(dir, name), ErrExists)
		case err == nil:
			return placeOver(trash, d, name, &old, put, out, st)
		case !errors.Is(err, unix.ENOENT):
			return out, fsErr(join(dir, name), err)
		}
	}
	work := UploadPrefix + st.Now.UTC().Format("20060102-150405") + "-" + randomHex(3)
	if err := put(tr.fd, work); err != nil {
		return out, err
	}
	if opt.Overwrite {
		out.Name, err = name, renameNoReplace(tr.fd, work, d.fd, name)
	} else {
		out.Name, err = renameFree(tr.fd, work, d.fd, name)
	}
	if err != nil {
		_ = unix.Unlinkat(tr.fd, work, 0)
		return out, fsErr(join(dir, name), err)
	}
	return out, nil
}

// placeOver replaces the file name of d with the upload: a trash entry is made for the
// old file, the upload is put into it under the same name, and the two trade places.
func placeOver(trash *os.Root, d dirfd, name string, old *unix.Stat_t, put func(int, string) error, out Placed, st Stamp) (Placed, error) {
	e := TrashEntry{Dir: d.path, Names: []string{name}, Deleted: st.Now.UTC().Truncate(time.Second), Actor: st.Actor, Reason: "overwrite"}
	if typeOf(old) == "file" {
		e.Files, e.Bytes = 1, old.Size
	}
	id, err := newTrashEntry(trash, st.Now)
	if err != nil {
		return out, err
	}
	e.ID = id
	drop := func() { _ = trash.RemoveAll(id) } // the entry holds only the new file (or nothing)
	if err := writeTrashMeta(trash, e); err != nil {
		drop()
		return out, fmt.Errorf("çöp kutusu: %w", err)
	}
	tf, err := openDir(trash, id+"/"+trashFiles)
	if err != nil {
		drop()
		return out, err
	}
	defer tf.close()
	if err := put(tf.fd, name); err != nil {
		drop()
		return out, err
	}
	if err := exchange(tf.fd, name, d.fd, name); err != nil {
		// The old file went away meanwhile: then it is a plain move.
		if errors.Is(err, unix.ENOENT) && renameNoReplace(tf.fd, name, d.fd, name) == nil {
			drop()
			out.Name = name
			return out, nil
		}
		drop()
		return out, fsErr(join(d.path, name), err)
	}
	out.Name, out.Trashed = name, id
	return out, nil
}

// copyOpen copies size bytes of src into a new file name in dir.
func copyOpen(src *os.File, size int64, dir int, name string) error {
	fd, err := unix.Openat(dir, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, fileMode)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err = src.Seek(0, io.SeekStart); err == nil {
		// A LimitedReader over a file lets the kernel copy (copy_file_range).
		var n int64
		n, err = io.Copy(f, io.LimitReader(src, size))
		if err == nil && n != size {
			err = fmt.Errorf("yüklenen dosya eksik (%d/%d bayt)", n, size)
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Chmod(fileMode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
