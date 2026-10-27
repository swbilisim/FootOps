// Package datafs reads and writes a site's persistent directory (data/). It runs in a
// short-lived helper process that has already dropped to the site's own user (see
// DropPrivileges), and every path goes through an os.Root, so neither a crafted archive
// nor a symbolic link planted by the app can reach outside the directory. Archives are
// streamed; memory does not grow with their size.
package datafs

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
)

// Stats counts what was written or read.
type Stats struct {
	Files   int   `json:"files"`
	Dirs    int   `json:"dirs"`
	Bytes   int64 `json:"bytes"`
	Skipped int   `json:"skipped"` // hidden files, links and other non-regular entries
}

// Options shape an extraction.
type Options struct {
	Limits archive.Limits
	// Strip is a top-level folder removed from every name ("uploads" turns
	// "uploads/a.jpg" into "a.jpg"); empty keeps names as they are.
	Strip string
	// Progress, if set, is called now and then with the running totals.
	Progress func(Stats)
	// Room, if set, is asked before each file is written whether the disk has room for
	// it (the sites must keep theirs).
	Room func(need int64) error
	// Compressed is the archive's size, for the compression ratio check (a bomb is
	// stopped while it expands); 0 skips the check (the archive was checked before).
	Compressed int64
}

const (
	fileMode = 0o644
	dirMode  = 0o755
)

// hidden reports whether a cleaned name is never extracted nor packed (archive.Hidden).
func hidden(name string) bool { return archive.Hidden(name) }

// target maps an archive member to its name in the directory. skip is true for the
// archive's root, the stripped folder itself and hidden entries.
func target(raw, strip string) (name string, skip bool, err error) {
	name, err = archive.CleanName(raw)
	if err != nil || name == "" {
		return "", true, err
	}
	if strip != "" {
		if name == strip {
			return "", true, nil
		}
		rest, ok := strings.CutPrefix(name, strip+"/")
		if !ok {
			if hidden(name) {
				return "", true, nil
			}
			return "", false, fmt.Errorf("%s: %s/ klasörünün dışında", name, strip)
		}
		name = rest
	}
	return name, hidden(name), nil
}

// writer applies limits and creates entries in dst.
type writer struct {
	dst   *os.Root
	opt   Options
	st    Stats
	last  time.Time
	dirs  map[string]bool
	files int
}

func newWriter(dst *os.Root, opt Options) *writer {
	if opt.Limits == (archive.Limits{}) {
		opt.Limits = archive.DefaultLimits
	}
	return &writer{dst: dst, opt: opt, dirs: map[string]bool{".": true}}
}

func (w *writer) progress(force bool) {
	if w.opt.Progress != nil && (force || time.Since(w.last) > 500*time.Millisecond) {
		w.last = time.Now()
		w.opt.Progress(w.st)
	}
}

func (w *writer) count() error {
	if w.st.Files+w.st.Dirs > w.opt.Limits.MaxFiles {
		return fmt.Errorf("çok fazla dosya (sınır %d)", w.opt.Limits.MaxFiles)
	}
	return nil
}

func (w *writer) mkdir(name string) error {
	if w.dirs[name] {
		return nil
	}
	if err := w.dst.MkdirAll(name, dirMode); err != nil {
		return fmt.Errorf("%s: klasör oluşturulamadı: %w", name, err)
	}
	for d := name; d != "." && !w.dirs[d]; d = path.Dir(d) {
		w.dirs[d] = true
		w.st.Dirs++
	}
	return w.count()
}

// file writes one regular file of size bytes from r. The name must be new: the
// directory is empty (replace) or the archive is extracted before existing files are
// added (merge), and the archive check refuses duplicate names.
func (w *writer) file(name string, size int64, r io.Reader) error {
	if size < 0 || size > w.opt.Limits.MaxFile {
		return fmt.Errorf("%s: dosya çok büyük", name)
	}
	if w.st.Bytes+size > w.opt.Limits.MaxTotal {
		return fmt.Errorf("toplam boyut sınırı aşıldı (%d bayt)", w.opt.Limits.MaxTotal)
	}
	if w.opt.Compressed > 0 {
		if err := archive.CheckRatio(w.st.Bytes+size, w.opt.Compressed, w.opt.Limits); err != nil {
			return err
		}
	}
	if err := w.mkdir(path.Dir(name)); err != nil {
		return err
	}
	if w.opt.Room != nil {
		if err := w.opt.Room(size); err != nil {
			return err
		}
	}
	f, err := w.dst.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return fmt.Errorf("%s: yazılamadı: %w", name, err)
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err == nil && n != size {
		err = fmt.Errorf("%s: arşivde eksik (%d/%d bayt)", name, n, size)
	}
	if err == nil {
		err = f.Chmod(fileMode) // exact, whatever the umask
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	w.st.Files++
	w.st.Bytes += size
	w.progress(false)
	return w.count()
}

// ExtractTar writes the members of a tar stream (already decompressed) into dst.
func ExtractTar(r io.Reader, dst *os.Root, opt Options) (Stats, error) {
	w := newWriter(dst, opt)
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return w.st, fmt.Errorf("tar bozuk: %w", err)
		}
		name, skip, err := target(h.Name, w.opt.Strip)
		if err != nil {
			return w.st, err
		}
		switch {
		case skip:
			w.st.Skipped++
		case h.Typeflag == tar.TypeDir:
			err = w.mkdir(name)
		case h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA:
			err = w.file(name, h.Size, tr)
		default:
			return w.st, fmt.Errorf("%s: yalnız dosya ve klasör kabul edilir", name)
		}
		if err != nil {
			return w.st, err
		}
	}
	w.progress(true)
	return w.st, nil
}

// ExtractTarGz is ExtractTar for a gzip'd stream; the gzip checksum is verified.
func ExtractTarGz(r io.Reader, dst *os.Root, opt Options) (Stats, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return Stats{}, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	st, err := ExtractTar(zr, dst, opt)
	if err != nil {
		return st, err
	}
	// The checksum follows the tar end marker and its padding; nothing much else may.
	n, err := io.Copy(io.Discard, io.LimitReader(zr, maxTrailer+1))
	if err != nil {
		return st, fmt.Errorf("gzip bozuk: %w", err)
	}
	if n > maxTrailer {
		return st, errors.New("tar'ın sonundan sonra beklenmeyen veri var")
	}
	return st, nil
}

// maxTrailer is what may follow a tar's end marker (tar pads to 10 KB records).
const maxTrailer = 1 << 20

// ExtractZip writes the members of a zip file into dst; each member's CRC is checked. A
// central directory too big to load is refused from the end record first.
func ExtractZip(ra io.ReaderAt, size int64, dst *os.Root, opt Options) (Stats, error) {
	w := newWriter(dst, opt)
	if err := archive.CheckZipDirectory(ra, size, w.opt.Limits); err != nil {
		return Stats{}, err
	}
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		return Stats{}, fmt.Errorf("zip bozuk: %w", err)
	}
	if len(zr.File) > w.opt.Limits.MaxFiles {
		return w.st, fmt.Errorf("çok fazla dosya (sınır %d)", w.opt.Limits.MaxFiles)
	}
	for _, f := range zr.File {
		name, skip, err := target(f.Name, w.opt.Strip)
		if err != nil {
			return w.st, err
		}
		mode := f.Mode()
		switch {
		case skip:
			w.st.Skipped++
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			err = w.mkdir(name)
		case mode.IsRegular():
			if f.Flags&0x1 != 0 {
				return w.st, fmt.Errorf("%s: şifreli zip kabul edilmez", name)
			}
			if f.UncompressedSize64 > uint64(w.opt.Limits.MaxFile) {
				return w.st, fmt.Errorf("%s: dosya çok büyük", name)
			}
			var rc io.ReadCloser
			if rc, err = f.Open(); err != nil {
				return w.st, fmt.Errorf("%s: açılamadı: %w", name, err)
			}
			err = w.file(name, int64(f.UncompressedSize64), rc)
			if err == nil {
				// archive/zip checks the CRC when the member is read to its end.
				if _, cerr := io.Copy(io.Discard, io.LimitReader(rc, 1)); cerr != nil {
					err = fmt.Errorf("%s: bozuk: %w", name, cerr)
				}
			}
			rc.Close()
		default:
			return w.st, fmt.Errorf("%s: yalnız dosya ve klasör kabul edilir", name)
		}
		if err != nil {
			return w.st, err
		}
	}
	w.progress(true)
	return w.st, nil
}

// maxCount bounds Count: a data directory with more entries is refused rather than
// walked (the agent counts as root, in its own memory).
const maxCount = 1_000_000

// Count adds up a directory's regular files without following links, reading each
// directory in small batches. Hidden entries are counted as skipped (they are neither
// exported nor imported).
func Count(root *os.Root) (Stats, error) {
	var st Stats
	dirs := []string{"."}
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		f, err := root.Open(dir)
		if err != nil {
			return st, err
		}
		for {
			ents, err := f.ReadDir(256)
			for _, d := range ents {
				p := path.Join(dir, d.Name())
				if st.Files+st.Dirs+st.Skipped >= maxCount {
					f.Close()
					return st, fmt.Errorf("klasörde çok fazla dosya var (sınır %d)", maxCount)
				}
				switch {
				case hidden(p):
					st.Skipped++
				case d.IsDir():
					st.Dirs++
					dirs = append(dirs, p)
				case d.Type().IsRegular():
					info, ierr := d.Info()
					if ierr != nil {
						f.Close()
						return st, ierr
					}
					st.Files++
					st.Bytes += info.Size()
				default:
					st.Skipped++
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				f.Close()
				return st, err
			}
		}
		f.Close()
	}
	return st, nil
}

// Pack writes a directory as a gzip'd tar to w: directories and regular files only,
// hidden entries and links left out, modes normalised, no owners. Level 1 compression:
// a site's files are mostly photos, which do not compress anyway.
func Pack(src *os.Root, w io.Writer, opt Options) (Stats, error) {
	if opt.Limits == (archive.Limits{}) {
		opt.Limits = archive.DefaultLimits
	}
	var st Stats
	last := time.Time{}
	zw, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return st, err
	}
	tw := tar.NewWriter(zw)
	err = fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		// The site keeps running: what it removes meanwhile is simply not exported.
		if errors.Is(err, fs.ErrNotExist) {
			st.Skipped++
			return nil
		}
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		// Names an import would refuse (a backslash, control characters, …) are left out,
		// like hidden ones: the archive must read back.
		if _, nerr := archive.CleanName(p); nerr != nil || hidden(p) {
			st.Skipped++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if st.Files+st.Dirs >= opt.Limits.MaxFiles {
			return fmt.Errorf("çok fazla dosya (sınır %d)", opt.Limits.MaxFiles)
		}
		switch {
		case d.IsDir():
			info, err := d.Info()
			if errors.Is(err, fs.ErrNotExist) {
				st.Skipped++
				return fs.SkipDir
			}
			if err != nil {
				return err
			}
			st.Dirs++
			return tw.WriteHeader(&tar.Header{Name: p + "/", Typeflag: tar.TypeDir, Mode: dirMode, ModTime: info.ModTime()})
		case d.Type().IsRegular():
			return packFile(src, tw, p, &st, opt)
		default:
			st.Skipped++
		}
		if opt.Progress != nil && time.Since(last) > 500*time.Millisecond {
			last = time.Now()
			opt.Progress(st)
		}
		return nil
	})
	if err != nil {
		return st, err
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	if err := zw.Close(); err != nil {
		return st, err
	}
	if opt.Progress != nil {
		opt.Progress(st)
	}
	return st, nil
}

// packFile adds one regular file. It is opened without blocking (a file swapped for a
// FIFO meanwhile must not hang the export) and its size is taken from the open file;
// one that vanished is skipped.
func packFile(src *os.Root, tw *tar.Writer, p string, st *Stats, opt Options) error {
	f, err := src.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		st.Skipped++
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		st.Skipped++
		return nil
	}
	size := info.Size()
	if size > opt.Limits.MaxFile || st.Bytes+size > opt.Limits.MaxTotal {
		return fmt.Errorf("%s: boyut sınırı aşıldı", p)
	}
	if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Size: size, Mode: fileMode, ModTime: info.ModTime()}); err != nil {
		return err
	}
	// A file that grows meanwhile is cut at its size; one cut short by the site cannot be
	// archived as it was listed.
	if n, err := io.CopyN(tw, f, size); err != nil {
		return fmt.Errorf("%s: okunurken küçüldü (%d/%d bayt): %w", p, n, size, err)
	}
	st.Files++
	st.Bytes += size
	return nil
}

// Fill adds to dst everything of src that dst does not have, as hard links (no copy;
// it falls back to copying when a link is not possible). It is the "add and overwrite"
// import: the archive is extracted into dst first, so its files win; src is the
// current directory and is left untouched. Hidden files of src are kept too: they are
// the site's own.
func Fill(src, dst *os.Root) (Stats, error) {
	var st Stats
	l := &linker{src: src, dst: dst}
	defer l.close()
	err := fs.WalkDir(src.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		have, lerr := dst.Lstat(p)
		switch {
		case d.IsDir():
			if lerr == nil && !have.IsDir() {
				return fmt.Errorf("%s: sitede bir klasör, arşivde bir dosya; \"ekle\" bunu birleştiremez (\"tamamen değiştir\" kullan)", p)
			}
			if lerr != nil {
				if err := dst.Mkdir(p, dirMode); err != nil {
					return err
				}
			}
			return nil
		case lerr == nil && have.IsDir():
			return fmt.Errorf("%s: sitede bir dosya, arşivde bir klasör; \"ekle\" bunu birleştiremez (\"tamamen değiştir\" kullan)", p)
		case lerr == nil:
			return nil // the archive has this file: it wins
		case d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0:
			info, err := d.Info()
			if err != nil {
				return err
			}
			if err := l.link(p, d.Type().IsRegular()); err != nil {
				return fmt.Errorf("%s: aktarılamadı: %w", p, err)
			}
			// Counted like Count counts the result: hidden files and links as skipped.
			if hidden(p) || !d.Type().IsRegular() {
				st.Skipped++
			} else {
				st.Files++
				st.Bytes += info.Size()
			}
		default:
			st.Skipped++
		}
		return nil
	})
	return st, err
}

// copyFile copies a regular file between roots (when a hard link is not possible).
func copyFile(src, dst *os.Root, p string) error {
	in, err := src.Open(p)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := dst.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
