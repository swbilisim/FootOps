package install

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/swbilisim/FootOps/internal/run"
)

// Runner and ExecRunner live in package run, shared with the agent.
type (
	Runner     = run.Runner
	ExecRunner = run.Exec
)

// Host is the machine being prepared. Root is "/" in production and a temp dir in tests,
// so file-based steps can be tested without touching the real system. Commands always
// act on the real system; tests use a fake Runner.
type Host struct {
	Root string
	Run  Runner
}

// Path maps an absolute host path into Root.
func (h Host) Path(p string) string {
	return filepath.Join(h.Root, p)
}

// ReadFile returns the file content, or "" if it does not exist.
func (h Host) ReadFile(p string) (string, error) {
	b, err := os.ReadFile(h.Path(p))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// FileEquals reports whether p exists with exactly the given content.
func (h Host) FileEquals(p, content string) (bool, error) {
	got, err := h.ReadFile(p)
	if err != nil {
		return false, err
	}
	return got == content, nil
}

// WriteFile writes atomically: a temp file in the same directory is synced and renamed
// into place, so a crash or power loss never leaves a half-written config behind.
// Missing parent directories are created with mode 0755.
func (h Host) WriteFile(p, content string, perm os.FileMode) error {
	dst := h.Path(p)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".footops-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	err = os.Rename(tmp.Name(), dst)
	if errors.Is(err, syscall.EBUSY) {
		// dst is a mount point (containers bind-mount /etc/hosts and /etc/hostname);
		// it cannot be replaced, only rewritten in place.
		return os.WriteFile(dst, []byte(content), perm)
	}
	return err
}

// Symlink atomically points p at target, replacing whatever is at p.
func (h Host) Symlink(target, p string) error {
	dst := h.Path(p)
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".footops-link")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Readlink returns the target of the symlink at p, or "" if p is not a symlink.
func (h Host) Readlink(p string) string {
	t, err := os.Readlink(h.Path(p))
	if err != nil {
		return ""
	}
	return t
}

// Exists reports whether p exists.
func (h Host) Exists(p string) bool {
	_, err := os.Lstat(h.Path(p))
	return err == nil
}

// Remove deletes p if it exists.
func (h Host) Remove(p string) error {
	err := os.Remove(h.Path(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
