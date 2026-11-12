package mac

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// defang makes a restored files tree safe to look at. The server wrote it, and restic
// restores what it holds as written: links, special files, execute and set-id bits and
// extended attributes (a quarantine one marked approved among them). Working through the
// folder's own root, so no step leads out of it, links and special files go, folders
// become 0755 and files 0644 with no extended attribute of the server's, and on the Mac
// each file is quarantined: opening one asks first, as for a download. Hidden files stay:
// they are the site's. It reports how many entries went and how many files could not be
// set right.
func defang(dir string) (removed, failed int, err error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil // restic wrote nothing
	}
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	quarantine := fmt.Appendf(nil, "0081;%08x;FootOps;", time.Now().Unix())
	var walk func(rel string) error
	walk = func(rel string) error {
		// First: a folder restored without write or search permission keeps its entries.
		if err := root.Chmod(rel, 0o755); err != nil {
			return err
		}
		d, err := root.Open(rel)
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, ent := range ents {
			p := filepath.Join(rel, ent.Name())
			switch t := ent.Type(); {
			case t.IsDir():
				if err := walk(p); err != nil {
					return err
				}
			case t.IsRegular():
				if !defangFile(root, p, quarantine) {
					failed++
				}
			default:
				if root.Remove(p) != nil {
					failed++
				} else {
					removed++
				}
			}
		}
		return nil
	}
	err = walk(".")
	return removed, failed, err
}

// defangFile sets one restored file right through its own descriptor.
func defangFile(root *os.Root, name string, quarantine []byte) bool {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	ok := f.Chmod(0o644) == nil
	fd := int(f.Fd())
	for _, n := range xattrNames(fd) {
		_ = unix.Fremovexattr(fd, n) // some are the system's own, kept by macOS
	}
	if runtime.GOOS == "darwin" {
		ok = unix.Fsetxattr(fd, "com.apple.quarantine", quarantine, 0) == nil && ok
	}
	return ok
}

// xattrNames lists a file's extended attributes; none when they cannot be read.
func xattrNames(fd int) []string {
	n, err := unix.Flistxattr(fd, nil)
	if err != nil || n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	if n, err = unix.Flistxattr(fd, buf); err != nil {
		return nil
	}
	var names []string
	for _, b := range bytes.Split(buf[:n], []byte{0}) {
		if len(b) > 0 {
			names = append(names, string(b))
		}
	}
	return names
}
