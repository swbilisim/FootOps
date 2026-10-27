//go:build !linux

package datafs

import "errors"

// DropPrivileges is only implemented on Linux, where the helper runs.
func DropPrivileges(uid, gid int) error {
	return errors.New("yalnız Linux'ta çalışır")
}
