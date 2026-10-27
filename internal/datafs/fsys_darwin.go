package datafs

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Darwin equivalents, so the file manager's tests run on a Mac too.

func renameNoReplace(olddir int, oldname string, newdir int, newname string) error {
	return unix.RenameatxNp(olddir, oldname, newdir, newname, unix.RENAME_EXCL)
}

func exchange(adir int, a string, bdir int, b string) error {
	return unix.RenameatxNp(adir, a, bdir, b, unix.RENAME_SWAP)
}

// linkOpen is Linux-only (/proc/self/fd); the caller copies instead.
func linkOpen(*os.File, int, string) error { return unix.ENOTSUP }

func fdPath(int) string { return "." }

// Darwin has no atomic close-on-exec for these: the fork lock is held until it is set.
const (
	socketCloexec = 0
	recvCloexec   = 0
)

func forkLocked(fn func()) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fn()
}
