//go:build unix

package datafs

import (
	"os"
	"path"

	"golang.org/x/sys/unix"
)

// linker makes hard links from one root into another. The two parent directories are
// opened through their roots (so they are inside them) and only a single name component
// is passed to linkat, which does not follow a link in its final component. The
// directories stay open while consecutive names share them (a walk visits a directory's
// files together).
type linker struct {
	src, dst       *os.Root
	dir            string
	srcDir, dstDir *os.File
	opened         bool
}

func (l *linker) close() {
	if l.srcDir != nil {
		l.srcDir.Close()
	}
	if l.dstDir != nil {
		l.dstDir.Close()
	}
	l.srcDir, l.dstDir, l.opened = nil, nil, false
}

func (l *linker) open(dir string) error {
	if l.opened && l.dir == dir {
		return nil
	}
	l.close()
	s, err := l.src.Open(dir)
	if err != nil {
		return err
	}
	d, err := l.dst.Open(dir)
	if err != nil {
		s.Close()
		return err
	}
	l.srcDir, l.dstDir, l.dir, l.opened = s, d, dir, true
	return nil
}

func (l *linker) link(p string, regular bool) error {
	if err := l.open(path.Dir(p)); err != nil {
		return err
	}
	base := path.Base(p)
	err := unix.Linkat(int(l.srcDir.Fd()), base, int(l.dstDir.Fd()), base, 0)
	if err != nil && regular {
		// Another filesystem, or a file the site's user may not link (protected_hardlinks).
		return copyFile(l.src, l.dst, p)
	}
	return err
}
