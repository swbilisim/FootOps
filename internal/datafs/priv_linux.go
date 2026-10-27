//go:build linux

package datafs

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// DropPrivileges turns a helper started as root into the site's user before it touches
// anything: no supplementary groups (root's would otherwise stay), the site's gid and
// uid, and no way back (no_new_privs). An image that runs as root (uid 0) keeps root,
// but still loses the groups; its paths are confined by os.Root all the same.
func DropPrivileges(uid, gid int) error {
	if uid < 0 || gid < 0 {
		return fmt.Errorf("geçersiz kullanıcı %d:%d", uid, gid)
	}
	unix.Umask(0o022)
	// Go applies these to every thread of the process.
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	groups, err := syscall.Getgroups()
	if err != nil {
		return err
	}
	if syscall.Getuid() != uid || syscall.Geteuid() != uid || syscall.Getgid() != gid || syscall.Getegid() != gid ||
		len(groups) > 1 || (len(groups) == 1 && groups[0] != gid) {
		return fmt.Errorf("kullanıcı değiştirilemedi (uid %d, gid %d, gruplar %v)", syscall.Geteuid(), syscall.Getegid(), groups)
	}
	return nil
}
