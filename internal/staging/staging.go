// Package staging manages the agent's scratch space for data work: uploads, archives
// being checked, temporary files for tool containers. Everything lives under one root
// (root-only), each job in its own directory, and nothing is kept for long: a job
// removes its directory when it ends and a sweep removes anything left behind.
//
// It also guards the disk. A full disk stops every site on the server, so data work
// refuses to start unless it fits with room to spare.
package staging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/swbilisim/FootOps/internal/sysinfo"
)

const (
	DefaultRoot = "/var/lib/footops/staging"
	// DefaultMax caps the whole staging area (operator decision, 2026-10-27).
	DefaultMax = 10 << 30
	// MaxAge: anything older is left over from a crash and is removed by Sweep.
	MaxAge = 24 * time.Hour
)

// Area is the staging root.
type Area struct {
	Root string
	// Max is the most the area may hold; it is also capped at a quarter of the free
	// disk, so a nearly full server cannot be filled by an upload.
	Max int64

	disk func(path string) (free, total uint64, err error)
	now  func() time.Time
}

func New(root string, max int64) *Area { return &Area{Root: root, Max: max} }

func (a *Area) init() {
	if a.disk == nil {
		a.disk = diskSpace
	}
	if a.now == nil {
		a.now = time.Now
	}
}

var reID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Dir creates (or returns) the directory of one job: root-only, 0700.
func (a *Area) Dir(id string) (string, error) {
	if !reID.MatchString(id) {
		return "", errors.New("geçersiz staging adı")
	}
	if err := os.MkdirAll(a.Root, 0o700); err != nil {
		return "", err
	}
	d := filepath.Join(a.Root, id)
	if err := os.Mkdir(d, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return d, nil
}

// Remove deletes a job's directory.
func (a *Area) Remove(id string) error {
	if !reID.MatchString(id) {
		return errors.New("geçersiz staging adı")
	}
	return os.RemoveAll(filepath.Join(a.Root, id))
}

// Sweep removes job directories untouched for MaxAge and returns their names.
func (a *Area) Sweep() []string {
	a.init()
	ents, err := os.ReadDir(a.Root)
	if err != nil {
		return nil
	}
	var removed []string
	for _, e := range ents {
		if !reID.MatchString(e.Name()) {
			continue
		}
		p := filepath.Join(a.Root, e.Name())
		if newest(p).After(a.now().Add(-MaxAge)) {
			continue
		}
		if os.RemoveAll(p) == nil {
			removed = append(removed, e.Name())
		}
	}
	return removed
}

// newest is the latest modification time in a tree (a resumed upload touches only the
// file it appends to, not the directory).
func newest(root string) time.Time {
	var t time.Time
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(t) {
			t = info.ModTime()
		}
		return nil
	})
	return t
}

// Used is the size of everything in the area.
func (a *Area) Used() int64 { return treeSize(a.Root) }

// Allow reports whether need more bytes may be staged now.
func (a *Area) Allow(need int64) error {
	a.init()
	free, total, err := a.disk(a.Root)
	if err != nil {
		return err
	}
	used := a.Used()
	limit := min(a.Max, (int64(free)+used)/4)
	if used+need > limit {
		return fmt.Errorf("geçici alan yetmiyor: %s gerekli, %s kullanılabilir (sınır: en çok %s ve boş diskin dörtte biri)",
			human(need), human(max(limit-used, 0)), human(a.Max))
	}
	return checkFree(free, total, need)
}

// CheckFree reports whether need bytes can be written to the filesystem holding path
// while keeping the reserve free for the sites.
func CheckFree(path string, need int64) error {
	free, total, err := diskSpace(path)
	if err != nil {
		return err
	}
	return checkFree(free, total, need)
}

// Reserve is what data work always leaves free: 2 GB or 10% of the disk, whichever is
// more. Databases and logs keep growing while a job runs.
func Reserve(total uint64) uint64 { return max(2<<30, total/10) }

func checkFree(free, total uint64, need int64) error {
	reserve := Reserve(total)
	if need < 0 || uint64(need)+reserve > free {
		return fmt.Errorf("diskte yer yok: %s gerekli, boş %s ve en az %s boş kalmalı",
			human(need), human(int64(free)), human(int64(reserve)))
	}
	return nil
}

func diskSpace(path string) (free, total uint64, err error) {
	d, err := sysinfo.ReadDisk(path)
	if err != nil {
		return 0, 0, err
	}
	return d.Total - d.Used, d.Total, nil
}

// treeSize sums regular file sizes under root (symbolic links are not followed).
func treeSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func human(n int64) string { return sysinfo.HumanBytes(uint64(max(n, 0))) }
