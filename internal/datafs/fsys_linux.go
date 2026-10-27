package datafs

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames without ever replacing: an existing target is EEXIST.
func renameNoReplace(olddir int, oldname string, newdir int, newname string) error {
	return unix.Renameat2(olddir, oldname, newdir, newname, unix.RENAME_NOREPLACE)
}

// exchange swaps two names atomically (readers see one or the other), also between two
// folders of one disk.
func exchange(adir int, a string, bdir int, b string) error {
	return unix.Renameat2(adir, a, bdir, b, unix.RENAME_EXCHANGE)
}

// linkOpen gives an open file a new name in dir without copying it. The file is reached
// through its descriptor, so the helper needs no access to the path it came from (the
// agent's staging area); the kernel still checks that this user may link it.
func linkOpen(f *os.File, dir int, name string) error {
	return unix.Linkat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", f.Fd()), dir, name, unix.AT_SYMLINK_FOLLOW)
}

// fdPath names an open descriptor by path (for lookups relative to it).
func fdPath(fd int) string { return fmt.Sprintf("/proc/self/fd/%d", fd) }

// Linux sets close-on-exec atomically for the socket and the received descriptor, so a
// command started at the same moment inherits neither.
const (
	socketCloexec = unix.SOCK_CLOEXEC
	recvCloexec   = unix.MSG_CMSG_CLOEXEC
)

func forkLocked(fn func()) { fn() }
