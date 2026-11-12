package mac

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A restored tree is the server's: what it holds comes out safe to look at. Links and
// special files go, nothing stays executable or set-id, the server's extended attributes
// go, and on the Mac every file is quarantined; hidden files and the contents stay, and
// nothing outside the folder is touched.
func TestDefangRestoredTree(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "dışarıda")
	os.WriteFile(outside, []byte("sahibinin"), 0o600)
	dir := filepath.Join(base, "files")
	os.MkdirAll(filepath.Join(dir, "alt"), 0o755)
	os.WriteFile(filepath.Join(dir, "ac.command"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "suid"), []byte("x"), 0o755)
	os.Chmod(filepath.Join(dir, "suid"), 0o4755|os.ModeSetuid)
	os.WriteFile(filepath.Join(dir, ".htaccess"), []byte("kural"), 0o600)
	os.WriteFile(filepath.Join(dir, "alt", "a.txt"), []byte("içerik"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "bag"))
	os.Symlink(outside, filepath.Join(dir, "alt", "bag"))
	if err := syscall.Mkfifo(filepath.Join(dir, "boru"), 0o644); err != nil {
		t.Fatal(err)
	}
	xattr := "user.footops-sunucu"
	hasXattr := unix.Setxattr(filepath.Join(dir, "ac.command"), xattr, []byte("1"), 0) == nil
	os.Chmod(filepath.Join(dir, "alt"), 0o555) // its link must go all the same

	removed, failed, err := defang(dir)
	if err != nil || removed != 3 || failed != 0 {
		t.Fatalf("removed %d, failed %d, %v", removed, failed, err)
	}
	for name, want := range map[string]os.FileMode{"ac.command": 0o644, "suid": 0o644, ".htaccess": 0o644, "alt/a.txt": 0o644, "alt": 0o755 | os.ModeDir} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v", name, fi.Mode(), err)
		}
	}
	for _, name := range []string{"bag", "alt/bag", "boru"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s kept", name)
		}
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "sahibinin" {
		t.Errorf("outside touched: %q %v", b, err)
	}
	if fi, _ := os.Stat(outside); fi.Mode().Perm() != 0o600 {
		t.Errorf("outside mode %v", fi.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "alt", "a.txt")); string(b) != "içerik" {
		t.Errorf("contents changed: %q", b)
	}
	if hasXattr {
		if _, err := unix.Getxattr(filepath.Join(dir, "ac.command"), xattr, nil); err == nil {
			t.Error("the server's extended attribute kept")
		}
	}
	if runtime.GOOS == "darwin" {
		if _, err := unix.Getxattr(filepath.Join(dir, "ac.command"), "com.apple.quarantine", nil); err != nil {
			t.Errorf("not quarantined: %v", err)
		}
	}
}
