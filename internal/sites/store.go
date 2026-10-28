package sites

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store reads and writes site files below Root (normally "/").
type Store struct{ Root string }

func (st Store) path(p string) string { return filepath.Join(st.Root, p) }

// List returns every site that has a footops.json, sorted by group and name.
func (st Store) List() ([]Spec, error) {
	var out []Spec
	for _, g := range Groups {
		entries, err := os.ReadDir(st.path(filepath.Join(SrvDir, g)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			s, err := st.read(filepath.Join(SrvDir, g, e.Name(), MetaFile))
			if errors.Is(err, fs.ErrNotExist) {
				continue // a directory the operator created by hand
			}
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", g, e.Name(), err)
			}
			// Where the directory is decides the group: a move between groups renames the
			// directory first, so one cut short still finds the site where its files are.
			s.Group = g
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (st Store) read(p string) (Spec, error) {
	b, err := os.ReadFile(st.path(p))
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(b, &s); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// ErrNotFound is returned when no site has the given name.
var ErrNotFound = errors.New("site bulunamadı")

// Get finds a site by name in any group.
func (st Store) Get(name string) (Spec, error) {
	all, err := st.List()
	if err != nil {
		return Spec{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return Spec{}, ErrNotFound
}

// Conflicts reports names and domains already used by other sites, and a panel host
// that must never be served by a site.
func (st Store) Conflicts(s Spec, reservedHosts ...string) ([]string, error) {
	return st.conflicts(s, true, reservedHosts)
}

// UpdateConflicts is Conflicts for a site that already exists: its own name and domains
// are not conflicts.
func (st Store) UpdateConflicts(s Spec, reservedHosts ...string) ([]string, error) {
	return st.conflicts(s, false, reservedHosts)
}

func (st Store) conflicts(s Spec, isNew bool, reservedHosts []string) ([]string, error) {
	all, err := st.List()
	if err != nil {
		return nil, err
	}
	var p []string
	used := map[string]string{}
	for _, o := range all {
		if isNew && o.Name == s.Name {
			p = append(p, fmt.Sprintf("%q adında bir site zaten var (%s grubunda)", s.Name, o.Group))
		}
		for _, d := range o.Domains {
			used[d] = o.Name
		}
	}
	for _, d := range s.Domains {
		if owner, ok := used[d]; ok && owner != s.Name {
			p = append(p, fmt.Sprintf("%s zaten %s sitesinde kullanılıyor", d, owner))
		}
		for _, r := range reservedHosts {
			if strings.EqualFold(d, r) {
				p = append(p, fmt.Sprintf("%s panelin adresi, site için kullanılamaz", d))
			}
		}
	}
	return p, nil
}

// WriteMeta writes footops.json atomically (0600).
func (st Store) WriteMeta(s Spec) error {
	b, err := s.MarshalMeta()
	if err != nil {
		return err
	}
	return writeFileAtomic(st.path(filepath.Join(s.Dir(), MetaFile)), b, 0o600)
}

// Env reads the site's current environment back from its compose file.
func (st Store) Env(s Spec) (map[string]string, error) {
	b, err := os.ReadFile(st.path(filepath.Join(s.Dir(), ComposeFile)))
	if err != nil {
		return nil, err
	}
	return ParseComposeEnv(string(b))
}

// WriteCompose writes compose.yaml atomically (0600: it holds secrets).
func (st Store) WriteCompose(s Spec, env map[string]string) error {
	return writeFileAtomic(st.path(filepath.Join(s.Dir(), ComposeFile)), []byte(Compose(s, env)), 0o600)
}

// Certs lists installed certificates.
func (st Store) Certs() ([]Cert, error) {
	entries, err := os.ReadDir(st.path(CertsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Cert
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".crt") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(st.path(CertsDir), e.Name()))
		if err != nil {
			return nil, err
		}
		c, err := LoadCert(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i].Names, ",") < strings.Join(out[j].Names, ",") })
	return out, nil
}

// InstallCert stores a validated certificate and its key. The key is written first and
// 0600, so the .crt (which makes the pair visible to Certs) never appears without it.
func (st Store) InstallCert(c Cert, keyPEM []byte) error {
	if err := os.MkdirAll(st.path(CertsDir), 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(st.path(c.HostKeyPath()), keyPEM, 0o600); err != nil {
		return err
	}
	return writeFileAtomic(st.path(c.HostCertPath()), c.PEM(), 0o644)
}

// RemoveCert deletes a certificate and its key.
func (st Store) RemoveCert(c Cert) error {
	for _, p := range []string{c.HostCertPath(), c.HostKeyPath()} {
		if err := os.Remove(st.path(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func writeFileAtomic(dst string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
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
	return os.Rename(tmp.Name(), dst)
}
