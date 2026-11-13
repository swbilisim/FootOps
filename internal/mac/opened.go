package mac

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// What was opened on this Mac (panel-tasarim §29.7): a folder holds a site's data and its
// secrets, a database its data — worth deleting once looked at. footops-mac keeps a list
// of what it opened (acilanlar.json beside the settings; no secrets), shows it with its
// age (older than a week: a reminder, in the menu too), and deletes only what is on it —
// what it wrote into a folder still holding its mark — or a database named *_yedek_* or
// *_arsiv_*. Backups (yedek ac) and archives (arsiv ac) share the list.
//
// A folder is on the list from its first moment, "partial" until it is whole, its mark
// held locked meanwhile: a footops-mac killed half-way (SIGKILL, a power cut) leaves a
// folder the list still shows, cut short, to be deleted like any other.

const (
	openedFormat = 1
	// openedMark is the file in an opened folder that ties it to its line on the list:
	// a folder put in its place later is not deleted for it.
	openedMark = ".footops-acilan"
	// openedOld: opened this long ago, it is pointed out.
	openedOld = 7 * 24 * time.Hour
	// openedMax is the most kept on the list (the oldest go).
	openedMax = 200
	// walkMax bounds counting an opened folder's files.
	walkMax = 500_000
)

type openedList struct {
	Format int           `json:"format"`
	Items  []openedEntry `json:"items"`
}

// openedEntry is one thing opened: a folder (Path) or a database (Name), of a server's
// backup or of an archive (Archive: its file's name). Partial: the folder's open has not
// ended (under way, or cut short); Kept: the folder was there before (an empty --hedef)
// and stays when what was written into it goes.
type openedEntry struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // "folder", "database"
	Path    string    `json:"path,omitempty"`
	Name    string    `json:"name,omitempty"`
	Server  string    `json:"server"`
	Site    string    `json:"site"`
	Archive string    `json:"archive,omitempty"`
	Time    time.Time `json:"time"` // the backup's, the archive's
	At      time.Time `json:"at"`   // when it was opened
	Partial bool      `json:"partial,omitempty"`
	Kept    bool      `json:"kept,omitempty"`
	// Copy: the database is a copy of this Mac's database of that name, taken before a
	// pull put a backup in its place (veritabani cek).
	Copy string `json:"copy,omitempty"`
}

// withOpened reads the list, lets change it (true: write it back) — one footops-mac at a
// time (a lock file beside it) — and writes it whole or not at all.
func (e *env) withOpened(change func(*openedList) bool) error {
	if err := os.MkdirAll(filepath.Dir(e.opened), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(e.opened+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	list := openedList{Format: openedFormat}
	data, err := os.ReadFile(e.opened)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(data, &list); err != nil || list.Format != openedFormat {
			// Not understood (edited by hand, a newer footops-mac's): begun anew, the
			// folders still there on disk.
			list = openedList{Format: openedFormat}
		}
	}
	if !change(&list) {
		return nil
	}
	if len(list.Items) > openedMax {
		list.Items = list.Items[len(list.Items)-openedMax:]
	}
	out, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.opened), ".acilanlar-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(out, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, e.opened)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func newID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// openMark is the mark of a folder being opened: written first, its line on the list
// partial, held locked (flock) until the open ends — the lock goes with the process
// however it dies, so the list tells an open under way from one cut short.
type openMark struct {
	f  *os.File
	id string
}

func makeMark(dir string) (*openMark, error) {
	f, err := os.OpenFile(filepath.Join(dir, openedMark), os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, folderErr(filepath.Join(dir, openedMark), err)
	}
	m := &openMark{f: f, id: newID()}
	// A volume without flock (some network disks): no lock, the open goes on — the list
	// then shows it cut short while it is under way, nothing worse.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if _, err := f.WriteString(m.id + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return m, nil
}

// release lets the mark go (its lock with it).
func (m *openMark) release() { m.f.Close() }

// opening reports whether a folder's open is under way: its mark held by a footops-mac.
func opening(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, openedMark), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// folderEntry is the line of a folder being opened, partial.
func (e *env) folderEntry(m *openMark, dir, server, site string, at time.Time, kept bool) openedEntry {
	now := e.now().UTC().Truncate(time.Second)
	return openedEntry{ID: m.id, Kind: "folder", Path: dir, Server: server, Site: site, Time: listTime(at, now),
		At: now, Partial: true, Kept: kept}
}

// listTime is a backup's or an archive's own time as the list keeps it: in UTC, or the
// opening time when it lies beyond the years the app reads (the server or the archive
// chose it, and the app reads the list whole).
func listTime(t, opened time.Time) time.Time {
	if u, ok := saneTime(t); ok {
		return u
	}
	return opened
}

// beginOpened puts a folder about to be filled on the list, partial.
func (e *env) beginOpened(it openedEntry) error {
	return e.withOpened(func(l *openedList) bool {
		l.Items = append(l.Items, it)
		return true
	})
}

// endOpened settles a folder's line once its open ends: whole (kept — put there now if
// it could not be at the start), or gone (dropped).
func (e *env) endOpened(it openedEntry, whole bool) error {
	now := e.now().UTC().Truncate(time.Second)
	return e.withOpened(func(l *openedList) bool {
		if !whole {
			l.Items = dropWhere(l.Items, func(x openedEntry) bool { return x.ID == it.ID })
			return true
		}
		for i := range l.Items {
			if l.Items[i].ID == it.ID {
				l.Items[i].Partial, l.Items[i].At = false, now
				return true
			}
		}
		it.Partial, it.At = false, now
		l.Items = append(l.Items, it)
		return true
	})
}

// noteDatabase puts a database a backup or an archive was loaded into on the list when it
// is named *_yedek_* or *_arsiv_* (one of another name is the owner's own to keep).
func (e *env) noteDatabase(res OpenResult) error {
	db := res.Database
	if db == nil || !markedDBName(db.Name) {
		return nil
	}
	now := e.now().UTC().Truncate(time.Second)
	return e.withOpened(func(l *openedList) bool {
		// Loaded anew: one line.
		l.Items = dropWhere(l.Items, func(it openedEntry) bool { return it.Kind == "database" && it.Name == db.Name })
		l.Items = append(l.Items, openedEntry{ID: newID(), Kind: "database", Name: db.Name, Server: res.Server, Site: res.Site,
			Archive: res.Archive, Time: listTime(res.Parts[partDB].Time, now), At: now})
		return true
	})
}

// ourNames are what footops-mac writes into a folder it opens a backup or an archive into
// (and the Finder's own file, which keeps an emptied folder from being removed).
var ourNames = []string{"db.archive", "files", "env", "footops.json", "manifest.json", ".DS_Store"}

// removeOpened removes what footops-mac wrote into an opened folder — its parts, then
// its mark: last, so a removal cut short leaves a folder still known as one opened —
// and the folder once nothing else is in it (unless keep: it was there before). Files
// the owner put there are left; then remains reports so.
func removeOpened(dir string, keep bool) (remains bool, err error) {
	for _, n := range ourNames {
		if err := removeTree(filepath.Join(dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	if err := os.Remove(filepath.Join(dir, openedMark)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if keep {
		return false, nil
	}
	if err := os.Remove(dir); err != nil {
		if entries, rerr := os.ReadDir(dir); rerr == nil && len(entries) > 0 {
			return true, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func dropWhere(items []openedEntry, gone func(openedEntry) bool) []openedEntry {
	out := items[:0]
	for _, it := range items {
		if !gone(it) {
			out = append(out, it)
		}
	}
	return out
}

// OpenedFolder is a folder a backup was opened into, still there. Partial: its open did
// not end — Busy: it is under way (else it was cut short). Error: it could not be looked
// at (macOS privacy, a disk not mounted): its line is kept.
type OpenedFolder struct {
	Path   string `json:"path"`
	Server string `json:"server"`
	// ServerName is the server as the Mac calls it now (a backup's; none for an archive).
	ServerName string    `json:"server_name,omitempty"`
	Site       string    `json:"site"`
	Archive    string    `json:"archive,omitempty"`
	Time       time.Time `json:"time"`
	At         time.Time `json:"at"`
	Files      int64     `json:"files"`
	Bytes      int64     `json:"bytes"`
	Old        bool      `json:"old,omitempty"`
	Partial    bool      `json:"partial,omitempty"`
	Busy       bool      `json:"busy,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// OpenedDatabase is a database of the Mac's MongoDB a backup or an archive was loaded
// into: one on the list, or one of a default name (<site>_yedek_<date>_<time>, …_arsiv_…)
// whose line is gone (Registered: on the list).
type OpenedDatabase struct {
	Name       string    `json:"name"`
	Server     string    `json:"server,omitempty"`
	ServerName string    `json:"server_name,omitempty"`
	Site       string    `json:"site,omitempty"`
	Archive    string    `json:"archive,omitempty"`
	Time       time.Time `json:"time,omitzero"`
	At         time.Time `json:"at,omitzero"`
	Bytes      int64     `json:"bytes"`
	Registered bool      `json:"registered"`
	Old        bool      `json:"old,omitempty"`
	// Copy: a copy of this Mac's database of that name, taken before a pull.
	Copy string `json:"copy,omitempty"`
}

// OpenedView is yedek acilanlar's answer. MongoError: the Mac's MongoDB could not be
// asked (its databases are not listed).
type OpenedView struct {
	Time       time.Time        `json:"time"`
	Folders    []OpenedFolder   `json:"folders"`
	Databases  []OpenedDatabase `json:"databases"`
	MongoError string           `json:"mongo_error,omitempty"`
}

// folderState is what became of an opened folder: still the one opened (a folder, no
// link, its mark in it), gone (none there, or one without its mark), or not known —
// err: it cannot be looked at now, and its line is kept.
func folderState(it openedEntry) (there bool, err error) {
	fi, err := os.Lstat(it.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !fi.IsDir():
		return false, nil
	}
	mark := filepath.Join(it.Path, openedMark)
	mi, err := os.Lstat(mark)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case !mi.Mode().IsRegular() || mi.Size() > 128:
		return false, nil
	}
	f, err := os.OpenFile(mark, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	data := make([]byte, 129)
	n, _ := f.Read(data)
	return strings.TrimSpace(string(data[:n])) == it.ID, nil
}

// openedView lists what was opened; lines of what is gone are dropped from the list
// (a database only when the Mac's MongoDB could be asked).
func (e *env) openedView(ctx context.Context) (OpenedView, error) {
	now := e.now()
	v := OpenedView{Time: now.Truncate(time.Second), Folders: []OpenedFolder{}, Databases: []OpenedDatabase{}}
	var dbs map[string]int64
	if t, err := parseMongoURI(e.mongoURI); err != nil {
		v.MongoError = err.Error()
	} else if dbs, err = t.databases(ctx); err != nil {
		if errors.Is(err, errStopped) {
			return v, err
		}
		v.MongoError = err.Error()
	}
	listed := map[string]bool{}
	looked := map[string]error{}
	names := e.names()
	serverName := func(code string) string {
		if code == "" {
			return ""
		}
		return displayName(code, names)
	}
	err := e.withOpened(func(l *openedList) bool {
		before := len(l.Items)
		l.Items = dropWhere(l.Items, func(it openedEntry) bool {
			switch it.Kind {
			case "folder":
				there, err := folderState(it)
				looked[it.ID] = err
				return !there && err == nil
			case "database":
				_, there := dbs[it.Name]
				return dbs != nil && !there
			}
			return true
		})
		for _, it := range l.Items {
			old := now.Sub(it.At) > openedOld
			switch it.Kind {
			case "folder":
				// Lines an older footops-mac wrote may hold an archive's name or time as it came.
				f := OpenedFolder{Path: it.Path, Server: it.Server, ServerName: serverName(it.Server), Site: it.Site, Archive: clean(it.Archive),
					Time: listTime(it.Time, it.At), At: it.At, Old: old, Partial: it.Partial}
				if err := looked[it.ID]; err != nil {
					f.Error = "bakılamadı: " + clip(err.Error(), 200)
				} else {
					f.Files, f.Bytes = folderSize(it.Path)
					f.Busy = it.Partial && opening(it.Path)
				}
				v.Folders = append(v.Folders, f)
			case "database":
				listed[it.Name] = true
				v.Databases = append(v.Databases, OpenedDatabase{Name: it.Name, Server: it.Server, ServerName: serverName(it.Server),
					Site: it.Site, Archive: clean(it.Archive), Time: listTime(it.Time, it.At), At: it.At, Bytes: dbs[it.Name], Registered: true, Old: old,
					Copy: clean(it.Copy)})
			}
		}
		return len(l.Items) != before
	})
	if err != nil {
		return v, fmt.Errorf("açılanlar listesi okunamadı: %v", err)
	}
	for name, size := range dbs {
		if reDefaultLoad.MatchString(name) && !listed[name] {
			v.Databases = append(v.Databases, OpenedDatabase{Name: name, Bytes: size})
		}
	}
	sort.SliceStable(v.Folders, func(i, j int) bool { return v.Folders[i].At.After(v.Folders[j].At) })
	sort.SliceStable(v.Databases, func(i, j int) bool {
		a, b := v.Databases[i], v.Databases[j]
		if a.At.Equal(b.At) {
			return a.Name < b.Name
		}
		return a.At.After(b.At)
	})
	return v, nil
}

// registeredDB tells a database is on the list of what was opened.
func (e *env) registeredDB(name string) bool {
	on := false
	_ = e.withOpened(func(l *openedList) bool {
		for _, it := range l.Items {
			on = on || it.Kind == "database" && it.Name == name
		}
		return false
	})
	return on
}

// folderSize counts a folder's files and their size (links not followed).
func folderSize(dir string) (int64, int64) {
	var files, size, seen int64
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if seen++; seen > walkMax {
			return filepath.SkipAll
		}
		if d.Type().IsRegular() && d.Name() != openedMark {
			if fi, err := d.Info(); err == nil {
				files++
				size += fi.Size()
			}
		}
		return nil
	})
	return files, size
}

// openedOldCount is how many things on the list were opened over a week ago (for the
// menu's reminder, every few minutes): the list alone is read — looking into the folders
// (in Downloads) could make macOS ask the app for that folder out of the blue. What was
// deleted otherwise leaves the list when the Backups window lists it.
func (e *env) openedOldCount() int {
	data, err := os.ReadFile(e.opened)
	if err != nil {
		return 0
	}
	var l openedList
	if json.Unmarshal(data, &l) != nil || l.Format != openedFormat {
		return 0
	}
	n := 0
	for _, it := range l.Items {
		if e.now().Sub(it.At) > openedOld {
			n++
		}
	}
	return n
}

// openedCmd is yedek acilanlar.
func (e *env) openedCmd(ctx context.Context, jsonOut bool) error {
	v, err := e.openedView(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		e.event(v)
		return nil
	}
	printOpened(e.out, v)
	return nil
}

// deleteOpened is yedek sil (and arsiv sil): a folder on the list (still holding its
// mark), or a database on it or of a default name (reDefaultLoad) — no other.
func (e *env) deleteOpened(ctx context.Context, what string, jsonOut bool) error {
	var done string
	if strings.Contains(what, "/") || strings.HasPrefix(what, "~") {
		p, err := filepath.Abs(expandHome(what))
		if err != nil {
			return err
		}
		var entry *openedEntry
		if err := e.withOpened(func(l *openedList) bool {
			for _, it := range l.Items {
				if it.Kind == "folder" && it.Path == p {
					entry = &it
				}
			}
			return false
		}); err != nil {
			return fmt.Errorf("açılanlar listesi okunamadı: %v", err)
		}
		if entry == nil {
			return fmt.Errorf("%s footops-mac'in açtıklarından değil: yalnız yedek ac'ın ve arsiv ac'ın açtığı klasörler silinir (gerekirse Finder'dan sil)", p)
		}
		there, err := folderState(*entry)
		switch {
		case err != nil:
			return fmt.Errorf("%s bakılamadı: %v", p, err)
		case !there:
			if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%s artık açılan klasör değil (işareti yok ya da değişmiş): elle sil", p)
			}
			done = p + " zaten yok"
		case opening(p):
			return fmt.Errorf("%s şu an açılıyor: önce durdur", p)
		default:
			remains, err := removeOpened(p, entry.Kept)
			if err != nil {
				return fmt.Errorf("%s silinemedi: %v", p, err)
			}
			done = "Silindi: " + p
			if remains {
				done = "footops-mac'in yazdıkları silindi; klasörde başka dosyalar olduğu için klasör bırakıldı: " + p
			}
		}
		id := entry.ID
		if err := e.withOpened(func(l *openedList) bool {
			l.Items = dropWhere(l.Items, func(it openedEntry) bool { return it.ID == id })
			return true
		}); err != nil {
			return fmt.Errorf("klasör silindi ama listeden çıkarılamadı: %v", err)
		}
	} else {
		if err := checkDBName(what); err != nil {
			return err
		}
		if !reDefaultLoad.MatchString(what) && !e.registeredDB(what) {
			return fmt.Errorf("%s footops-mac'in yüklediklerinden değil: yalnız açılanlar listesindeki ya da <site>_yedek_<tarih>_<saat> / "+
				"<site>_arsiv_<tarih>_<saat> adlı veritabanları silinir", what)
		}
		t, err := parseMongoURI(e.mongoURI)
		if err != nil {
			return err
		}
		release, err := e.lockDB(what)
		if err != nil {
			return err
		}
		defer release()
		dbs, err := t.databases(ctx)
		if err != nil {
			return err
		}
		if _, ok := dbs[what]; ok {
			if err := t.drop(ctx, what); err != nil {
				return err
			}
			done = "Silindi: MongoDB / " + what
		} else {
			done = "MongoDB'de " + what + " zaten yok"
		}
		if err := e.withOpened(func(l *openedList) bool {
			l.Items = dropWhere(l.Items, func(it openedEntry) bool { return it.Kind == "database" && it.Name == what })
			return true
		}); err != nil {
			return fmt.Errorf("veritabanı silindi ama listeden çıkarılamadı: %v", err)
		}
	}
	if jsonOut {
		e.event(map[string]any{"done": done})
		return nil
	}
	fmt.Fprintln(e.out, "✓ "+done)
	return nil
}
