//go:build linux || darwin

package datafs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"time"

	"golang.org/x/sys/unix"
)

// The trash is a folder next to data/ (/srv/<group>/<site>/trash) that the app does not
// see. Each deletion is one entry: <id>/meta.json says where the items came from and
// <id>/f/ holds them under their own names. Items are moved, never copied, so deleting
// costs nothing and a restore puts the same files back. The agent removes entries after
// a week (and the folders an unpack left behind, after a day).

// TrashIDPattern is the form of a trash entry's name: the time it was made (UTC) and a
// random part.
var TrashIDPattern = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}-[0-9a-f]{6}$`)

// Work in progress in the trash: an unpack's folder, an upload on its way into data/.
// Hidden names, never listed; the agent removes them after a day.
const (
	UnpackPrefix = ".unpack-"
	UploadPrefix = ".upload-"
)

const (
	trashMeta  = "meta.json"
	trashFiles = "f"
	maxMeta    = 64 << 10
)

// TrashEntry is one deletion in the trash.
type TrashEntry struct {
	ID      string    `json:"id"`
	Dir     string    `json:"dir"`   // the folder the items were in
	Names   []string  `json:"names"` // the items still in the trash (a listing shows the first ones)
	More    int       `json:"more,omitempty"`
	Files   int       `json:"files"`
	Dirs    int       `json:"dirs"`
	Bytes   int64     `json:"bytes"`
	Deleted time.Time `json:"deleted"`
	Actor   string    `json:"actor,omitempty"`
	// Reason is "" for a deletion, "overwrite" for a file an upload replaced and
	// "unpacked" for an archive moved away after it was opened.
	Reason string `json:"reason,omitempty"`
}

// move is one item going to the trash: from in the folder, as in the entry.
type move struct{ from, as string }

// treeStats counts what name holds (itself included), without following links.
func treeStats(dir int, name string) (Stats, error) {
	var s Stats
	st, err := lstatAt(dir, name)
	if err != nil {
		return s, err
	}
	switch typeOf(&st) {
	case "file":
		s.Files, s.Bytes = 1, st.Size
		return s, nil
	case "dir":
	default:
		s.Skipped = 1
		return s, nil
	}
	s.Dirs = 1
	sub, err := openSub(dir, name)
	if err != nil {
		return s, err
	}
	defer unix.Close(sub)
	w := walker{budget: maxCount, visit: func(_ int, _, _ string, st *unix.Stat_t) (bool, error) {
		switch typeOf(st) {
		case "file":
			s.Files++
			s.Bytes += st.Size
		case "dir":
			s.Dirs++
		default:
			s.Skipped++
		}
		return true, nil
	}}
	// A folder this user cannot read still moves (a rename does not look inside); its
	// contents are just not counted.
	w.unreadable = func(string, error) { s.Skipped++ }
	if err := w.walk(sub, name); errors.Is(err, errTooMany) {
		return s, fmt.Errorf("%s: çok fazla öğe (sınır %d)", name, maxCount)
	} else if err != nil {
		return s, err
	}
	return s, nil
}

// errTooMany: a walk went over its budget; callers say which limit it was.
var errTooMany = errors.New("çok fazla öğe")

// walker visits everything below a folder, a folder before its contents, without
// following links. visit gets the entry's folder, its path relative to the start, its
// name and status; returning false skips a folder's contents. Every entry visited costs
// one of budget.
type walker struct {
	budget int
	visit  func(dir int, rel, name string, st *unix.Stat_t) (bool, error)
	// unreadable, if set, is told about a folder this user may not read, and the walk
	// goes on without it; otherwise such a folder stops the walk.
	unreadable func(rel string, err error)
}

func (w *walker) walk(dir int, rel string) error {
	ents, err := entries(dir, maxListed)
	if err != nil {
		return fsErr(rel, err)
	}
	for _, e := range ents {
		if w.budget--; w.budget < 0 {
			return errTooMany
		}
		st, err := lstatAt(dir, e.name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		p := join(rel, e.name)
		if err != nil {
			return fsErr(p, err)
		}
		descend, err := w.visit(dir, p, e.name, &st)
		if err != nil {
			return err
		}
		if !descend || typeOf(&st) != "dir" {
			continue
		}
		sub, err := openSub(dir, e.name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if w.unreadable != nil && (errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)) {
			w.unreadable(p, err)
			continue
		}
		if err != nil {
			return fsErr(p, err)
		}
		err = w.walk(sub, p)
		unix.Close(sub)
		if err != nil {
			return err
		}
	}
	return nil
}

// newTrashEntry creates <id>/f in the trash.
func newTrashEntry(trash *os.Root, now time.Time) (string, error) {
	for i := 0; ; i++ {
		id := now.UTC().Format("20060102-150405") + "-" + randomHex(3)
		err := trash.Mkdir(id, 0o750)
		if errors.Is(err, os.ErrExist) && i < 5 {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("çöp kutusu: %w", err)
		}
		if err := trash.Mkdir(id+"/"+trashFiles, 0o750); err != nil {
			_ = trash.Remove(id)
			return "", fmt.Errorf("çöp kutusu: %w", err)
		}
		return id, nil
	}
}

func writeTrashMeta(trash *os.Root, e TrashEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := trash.OpenFile(e.ID+"/"+trashMeta, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// trashMove moves items out of src into a new trash entry. If one cannot be moved, the
// ones already moved go back; the entry is removed only when all of them did (a file
// is never deleted because it could not be put back).
func trashMove(trash *os.Root, src dirfd, items []move, reason string, st Stamp) (TrashEntry, error) {
	e := TrashEntry{Dir: src.path, Deleted: st.Now.UTC().Truncate(time.Second), Actor: st.Actor, Reason: reason}
	for _, m := range items {
		s, err := treeStats(src.fd, m.from)
		if err != nil {
			return e, fsErr(join(src.path, m.as), err)
		}
		e.Files += s.Files
		e.Dirs += s.Dirs
		e.Bytes += s.Bytes
		e.Names = append(e.Names, m.as)
	}
	id, err := newTrashEntry(trash, st.Now)
	if err != nil {
		return e, err
	}
	e.ID = id
	if err := writeTrashMeta(trash, e); err != nil {
		_ = trash.RemoveAll(id)
		return e, fmt.Errorf("çöp kutusu: %w", err)
	}
	tf, err := openDir(trash, id+"/"+trashFiles)
	if err != nil {
		_ = trash.RemoveAll(id)
		return e, err
	}
	defer tf.close()
	var done []move
	for _, m := range items {
		if err := renameNoReplace(src.fd, m.from, tf.fd, m.as); err != nil {
			back := true
			for i := len(done) - 1; i >= 0; i-- {
				back = renameNoReplace(tf.fd, done[i].as, src.fd, done[i].from) == nil && back
			}
			if back {
				_ = trash.RemoveAll(id)
			}
			return e, fsErr(join(src.path, m.as), err)
		}
		done = append(done, m)
	}
	return e, nil
}

// Trash moves names from dir to the trash as one entry.
func Trash(root, trash *os.Root, dir string, names []string, st Stamp) (TrashEntry, error) {
	dir, err := CleanPath(dir)
	if err != nil {
		return TrashEntry{}, err
	}
	if err := checkNames(names); err != nil {
		return TrashEntry{}, err
	}
	d, err := openDir(root, dir)
	if err != nil {
		return TrashEntry{}, err
	}
	defer d.close()
	items := make([]move, len(names))
	for i, n := range names {
		items[i] = move{n, n}
	}
	return trashMove(trash, d, items, "", st)
}

// readTrashMeta reads an entry's record; the items it holds are read from f/.
func readTrashMeta(trash *os.Root, id string) (TrashEntry, error) {
	f, err := trash.Open(id + "/" + trashMeta)
	if err != nil {
		return TrashEntry{}, err
	}
	defer f.Close()
	var e TrashEntry
	if err := json.NewDecoder(io.LimitReader(f, maxMeta)).Decode(&e); err != nil || e.ID != id {
		return TrashEntry{}, errors.New("çöp kutusu kaydı bozuk")
	}
	return e, nil
}

// The trash page shows at most this many entries (the newest) and names per entry; a
// restore always puts back everything of an entry.
const (
	MaxTrashListed = 500
	maxTrashNames  = 20
)

// TrashList lists the trash, newest first. An entry with a damaged record is still
// listed (it holds files); an empty one is not.
func TrashList(trash *os.Root) ([]TrashEntry, error) {
	d, err := openDir(trash, "")
	if err != nil {
		return nil, err
	}
	defer d.close()
	ents, err := entries(d.fd, maxListed)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, de := range ents {
		if TrashIDPattern.MatchString(de.name) {
			ids = append(ids, de.name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	out := []TrashEntry{}
	for _, id := range ids {
		if len(out) >= MaxTrashListed {
			break
		}
		e, err := readTrashMeta(trash, id)
		if err != nil {
			e = TrashEntry{ID: id}
			e.Deleted, _ = time.Parse("20060102-150405", id[:15])
		}
		e.Names, e.More = nil, 0
		tf, err := openDir(trash, id+"/"+trashFiles)
		if err != nil {
			continue
		}
		items, err := entries(tf.fd, maxListed)
		tf.close()
		if err != nil || len(items) == 0 {
			continue
		}
		for _, it := range items {
			e.Names = append(e.Names, it.name)
		}
		sort.Slice(e.Names, func(a, b int) bool { return SortKey(e.Names[a]) < SortKey(e.Names[b]) })
		if len(e.Names) > maxTrashNames {
			e.More = len(e.Names) - maxTrashNames
			e.Names = e.Names[:maxTrashNames]
		}
		out = append(out, e)
	}
	return out, nil
}

// Restored is what a restore put back.
type Restored struct {
	Dir   string    `json:"dir"`
	Items []Renamed `json:"items"`
}

// Renamed is an item put back, under its own name or, if taken, a numbered one.
type Renamed struct {
	Name string `json:"name"`
	As   string `json:"as"`
}

// Untrash puts an entry's items back where they were (folders deleted since are
// created again). A name taken meanwhile gets a number; nothing is replaced.
func Untrash(root, trash *os.Root, id string) (Restored, error) {
	if !TrashIDPattern.MatchString(id) {
		return Restored{}, fmt.Errorf("çöp kutusu kaydı: %w", ErrNotFound)
	}
	e, err := readTrashMeta(trash, id)
	if errors.Is(err, os.ErrNotExist) {
		return Restored{}, fmt.Errorf("çöp kutusu kaydı: %w", ErrNotFound)
	}
	if err != nil {
		return Restored{}, err
	}
	dir, err := CleanPath(e.Dir)
	if err != nil {
		return Restored{}, err
	}
	tf, err := openDir(trash, id+"/"+trashFiles)
	if err != nil {
		return Restored{}, err
	}
	defer tf.close()
	items, err := entries(tf.fd, maxListed)
	if err != nil {
		return Restored{}, err
	}
	sort.Slice(items, func(a, b int) bool { return SortKey(items[a].name) < SortKey(items[b].name) })
	dst, err := mkdirAll(root, dir)
	if err != nil {
		return Restored{}, err
	}
	defer dst.close()
	out := Restored{Dir: dir, Items: []Renamed{}}
	for _, it := range items {
		if CheckName(it.name) != nil {
			continue // never put there by the panel
		}
		as, err := renameFree(tf.fd, it.name, dst.fd, it.name)
		if err != nil {
			return out, fsErr(join(dir, it.name), err)
		}
		out.Items = append(out.Items, Renamed{Name: it.name, As: as})
	}
	if left, err := entries(tf.fd, 1); err == nil && len(left) == 0 {
		_ = trash.RemoveAll(id)
	}
	return out, nil
}
