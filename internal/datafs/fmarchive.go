//go:build linux || darwin

package datafs

import (
	"archive/zip"
	"bufio"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/swbilisim/FootOps/internal/archive"
)

// Opening an archive that is in the data directory, and downloading folders as a zip.

// Unpacked is an archive opened in the data directory.
type Unpacked struct {
	Stats
	Dir     string `json:"dir"`               // where it was opened (the archive's folder)
	Trashed string `json:"trashed,omitempty"` // the archive's trash entry
}

// Unpack opens the archive (zip, tar or tar.gz) at p in its own folder. It is extracted
// into a hidden folder of the trash first, where every check happens (names, sizes,
// CRCs, the limits), then moved in name by name without replacing anything: folders
// that exist already are merged, a taken name stops it before anything moves, and a
// move that fails half-way puts back what was moved. With trashArchive the archive
// goes to the trash afterwards.
func Unpack(root, trash *os.Root, p string, trashArchive bool, opt Options, st Stamp) (Unpacked, error) {
	p, err := CleanPath(p)
	if err != nil {
		return Unpacked{}, err
	}
	if p == "" {
		return Unpacked{}, fmt.Errorf("%w: arşiv seçilmedi", ErrInvalid)
	}
	dir := parentOf(p)
	out := Unpacked{Dir: dir}
	f, err := OpenFile(root, p)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	kind, err := archive.DetectReader(io.NewSectionReader(f, 0, info.Size()))
	if err != nil {
		return out, fmt.Errorf("%s: %w", p, err)
	}

	work := UnpackPrefix + st.Now.UTC().Format("20060102-150405") + "-" + randomHex(3)
	if err := trash.Mkdir(work, 0o750); err != nil {
		return out, fmt.Errorf("çöp kutusu: %w", err)
	}
	defer trash.RemoveAll(work)
	tmp, err := trash.OpenRoot(work)
	if err != nil {
		return out, err
	}
	defer tmp.Close()
	opt.Compressed = info.Size() // a bomb stops while it expands (rule K2)
	switch kind {
	case archive.Zip:
		out.Stats, err = ExtractZip(f, info.Size(), tmp, opt)
	case archive.TarGz:
		out.Stats, err = ExtractTarGz(bufio.NewReaderSize(f, 256<<10), tmp, opt)
	case archive.Tar:
		out.Stats, err = ExtractTar(bufio.NewReaderSize(f, 256<<10), tmp, opt)
	default:
		return out, fmt.Errorf("%s: zip, tar ya da tar.gz değil: %w", p, ErrInvalid)
	}
	if err != nil {
		return out, fmt.Errorf("%s açılamadı: %w", p, err)
	}

	src, err := openDir(tmp, "")
	if err != nil {
		return out, err
	}
	defer src.close()
	dst, err := openDir(root, dir)
	if err != nil {
		return out, err
	}
	defer dst.close()
	var taken []string
	if err := conflicts(src.fd, dst.fd, dir, &taken); err != nil {
		return out, err
	}
	if len(taken) > 0 {
		return out, fmt.Errorf("şunlar zaten var, hiçbir şey açılmadı (önce taşı ya da yeniden adlandır): %s: %w", list(taken), ErrExists)
	}
	var moved []string
	if err := moveIn(src.fd, dst.fd, "", &moved); err != nil {
		uerr := moveBack(root, tmp, dir, moved)
		// What came back may hold something the app wrote into a moved folder meanwhile:
		// it goes to the trash rather than being deleted with the work folder.
		if id, kerr := keepAsTrash(trash, work, dir, path.Base(p)+" içeriği (açılamadı)", out.Stats, st); kerr == nil {
			err = fmt.Errorf("%w; açılanlar geri alındı, çöp kutusunda (%s)", err, id)
		}
		if uerr != nil {
			return out, fmt.Errorf("%v; geri alınamadı, açılanların bir kısmı yerinde kaldı: %v", err, uerr)
		}
		return out, err
	}
	if trashArchive {
		e, err := trashMove(trash, dst, []move{{path.Base(p), path.Base(p)}}, "unpacked", st)
		if err != nil {
			return out, fmt.Errorf("arşiv açıldı ama çöp kutusuna taşınamadı: %w", err)
		}
		out.Trashed = e.ID
	}
	return out, nil
}

// keepAsTrash turns an unpack's work folder into a trash entry named label.
func keepAsTrash(trash *os.Root, work, dir, label string, s Stats, st Stamp) (string, error) {
	id, err := newTrashEntry(trash, st.Now)
	if err != nil {
		return "", err
	}
	e := TrashEntry{ID: id, Dir: dir, Names: []string{label}, Files: s.Files, Dirs: s.Dirs, Bytes: s.Bytes,
		Deleted: st.Now.UTC().Truncate(time.Second), Actor: st.Actor, Reason: "unpack-undone"}
	if err := writeTrashMeta(trash, e); err != nil {
		return "", err
	}
	return id, trash.Rename(work, id+"/"+trashFiles+"/"+label)
}

// conflicts collects (up to 20) names of the extracted tree src that dst has already and
// that cannot be merged: anything over a file, a file over a folder.
func conflicts(src, dst int, rel string, out *[]string) error {
	ents, err := entries(src, maxListed)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if len(*out) >= 20 {
			return nil
		}
		p := join(rel, e.name)
		have, err := lstatAt(dst, e.name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return fsErr(p, err)
		}
		mine, err := lstatAt(src, e.name)
		if err != nil {
			return fsErr(p, err)
		}
		if typeOf(&mine) == "dir" && typeOf(&have) == "dir" {
			if err := inBoth(src, dst, e.name, func(s, d int) error { return conflicts(s, d, p, out) }); err != nil {
				return fsErr(p, err)
			}
			continue
		}
		*out = append(*out, p)
	}
	return nil
}

// inBoth opens name in both folders (no links) and calls fn with them.
func inBoth(a, b int, name string, fn func(a, b int) error) error {
	sa, err := openSub(a, name)
	if err != nil {
		return err
	}
	defer unix.Close(sa)
	sb, err := openSub(b, name)
	if err != nil {
		return err
	}
	defer unix.Close(sb)
	return fn(sa, sb)
}

// moveIn moves the extracted tree into dst. Each move is recorded (path relative to the
// tree) so that it can be undone.
func moveIn(src, dst int, rel string, moved *[]string) error {
	ents, err := entries(src, maxListed)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := join(rel, e.name)
		err := renameNoReplace(src, e.name, dst, e.name)
		if err == nil {
			*moved = append(*moved, p)
			continue
		}
		if mine, serr := lstatAt(src, e.name); !errors.Is(err, unix.EEXIST) || serr != nil || typeOf(&mine) != "dir" {
			return fsErr(p, err)
		}
		// A folder that exists: merge (conflicts made sure it is a folder).
		if err := inBoth(src, dst, e.name, func(s, d int) error { return moveIn(s, d, p, moved) }); err != nil {
			return err
		}
	}
	return nil
}

// moveBack undoes moveIn: everything moved goes back into the work folder.
func moveBack(root, tmp *os.Root, dir string, moved []string) error {
	var errs []error
	for i := len(moved) - 1; i >= 0; i-- {
		p := moved[i]
		err := func() error {
			from, err := openDir(root, parentJoin(dir, parentOf(p)))
			if err != nil {
				return err
			}
			defer from.close()
			to, err := openDir(tmp, parentOf(p))
			if err != nil {
				return err
			}
			defer to.close()
			return renameNoReplace(from.fd, path.Base(p), to.fd, path.Base(p))
		}()
		if err != nil {
			errs = append(errs, fsErr(join(dir, p), err))
		}
	}
	return errors.Join(errs...)
}

func parentJoin(dir, sub string) string {
	if sub == "" {
		return dir
	}
	return join(dir, sub)
}

// stored are the extensions a zip download keeps as they are: already compressed, so
// deflating them would only burn CPU.
var stored = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".avif": true, ".heic": true,
	".mp4": true, ".mov": true, ".webm": true, ".mkv": true, ".m4v": true, ".mp3": true, ".m4a": true,
	".aac": true, ".ogg": true, ".opus": true, ".zip": true, ".gz": true, ".tgz": true, ".bz2": true,
	".xz": true, ".zst": true, ".7z": true, ".rar": true, ".woff": true, ".woff2": true, ".pdf": true,
	".docx": true, ".xlsx": true, ".pptx": true, ".odt": true, ".ods": true,
}

// PackZip writes names of dir (everything in dir when names is empty) to w as a zip:
// folders with all they hold, files; links, hidden and special entries are left out.
// What this user may not read is left out too, and listed in a text file at the end of
// the zip (UnreadableNote), so the download is never silently incomplete. It streams:
// nothing is staged, memory does not grow with the files. Photos and other compressed
// files are stored, the rest deflated at the fastest level. An error before anything is
// written means nothing was sent.
func PackZip(root *os.Root, dir string, names []string, w io.Writer, progress func(Stats)) (Stats, error) {
	var st Stats
	dir, err := CleanPath(dir)
	if err != nil {
		return st, err
	}
	if len(names) > 0 {
		if err := checkNames(names); err != nil {
			return st, err
		}
	}
	d, err := openDir(root, dir)
	if err != nil {
		return st, err
	}
	defer d.close()
	if len(names) == 0 {
		ents, err := entries(d.fd, maxListed)
		if err != nil {
			return st, fsErr(dir, err)
		}
		for _, e := range ents {
			if !hidden(e.name) && CheckName(e.name) == nil {
				names = append(names, e.name)
			}
		}
	}
	// Every name must be there before the first byte goes out.
	tops := make([]unix.Stat_t, len(names))
	for i, n := range names {
		if tops[i], err = lstatAt(d.fd, n); err != nil {
			return st, fsErr(join(dir, n), err)
		}
	}
	zw := zip.NewWriter(w)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestSpeed)
	})
	last := time.Now()
	var unreadable []string
	add := func(parent int, rel, name string, s *unix.Stat_t) (bool, error) {
		if hidden(name) || CheckName(name) != nil {
			st.Skipped++
			return false, nil
		}
		switch typeOf(s) {
		case "dir":
			st.Dirs++
			_, err := zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Method: zip.Store, Modified: mtimeOf(s)})
			return true, err
		case "file":
			if err := zipFile(zw, parent, rel, name, &st); errors.Is(err, errUnreadable) {
				unreadable = append(unreadable, rel)
			} else if err != nil {
				return false, err
			}
		default:
			st.Skipped++
		}
		if progress != nil && time.Since(last) > 500*time.Millisecond {
			last = time.Now()
			progress(st)
		}
		return false, nil
	}
	wk := walker{budget: archive.DefaultLimits.MaxFiles - len(names), visit: add,
		unreadable: func(rel string, _ error) { unreadable = append(unreadable, rel+"/") }}
	for i, n := range names {
		descend, err := add(d.fd, n, n, &tops[i])
		if err != nil {
			return st, err
		}
		if !descend {
			continue
		}
		sub, err := openSub(d.fd, n)
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
			unreadable = append(unreadable, n+"/")
			continue
		}
		if err != nil {
			return st, fsErr(join(dir, n), err)
		}
		err = wk.walk(sub, n)
		unix.Close(sub)
		if errors.Is(err, errTooMany) {
			return st, fmt.Errorf("çok fazla dosya (sınır %d)", archive.DefaultLimits.MaxFiles)
		}
		if err != nil {
			return st, err
		}
	}
	if len(unreadable) > 0 {
		st.Skipped += len(unreadable)
		note := "Bu dosyalar ve klasörler sitenin kullanıcısı tarafından okunamadığı için zip'e konmadı:\n\n" +
			strings.Join(unreadable, "\n") + "\n"
		fw, err := zw.Create(UnreadableNote)
		if err == nil {
			_, err = io.WriteString(fw, note)
		}
		if err != nil {
			return st, err
		}
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	if progress != nil {
		progress(st)
	}
	return st, nil
}

// UnreadableNote lists, inside a zip download, what could not be read.
const UnreadableNote = "FOOTOPS-OKUNAMAYANLAR.txt"

// errUnreadable: a file this user may not open.
var errUnreadable = errors.New("okunamadı")

// zipFile adds one file, opened without blocking and without following a link; its size
// is the open file's. A file that grows meanwhile is cut at that size; one that shrinks
// fails the download (it cannot be stored as listed). One this user may not open is
// errUnreadable, before anything of it is written.
func zipFile(zw *zip.Writer, dir int, rel, name string, st *Stats) error {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		st.Skipped++
		return nil
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return errUnreadable
	case err != nil:
		return fsErr(rel, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var s unix.Stat_t
	if err := unix.Fstat(fd, &s); err != nil {
		return err
	}
	if typeOf(&s) != "file" {
		st.Skipped++
		return nil
	}
	method := zip.Deflate
	if stored[strings.ToLower(path.Ext(name))] {
		method = zip.Store
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: method, Modified: mtimeOf(&s)})
	if err != nil {
		return err
	}
	if n, err := io.CopyN(w, f, s.Size); err != nil {
		return fmt.Errorf("%s: okunurken küçüldü (%d/%d bayt): %w", rel, n, s.Size, err)
	}
	st.Files++
	st.Bytes += s.Size
	return nil
}
