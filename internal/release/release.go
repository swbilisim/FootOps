// Package release is FootOps' signed releases (panel-tasarim §31.2): the manifest that
// lists a release's files with their SHA-256, its ed25519 signature — made on the Mac,
// whose Keychain alone holds the private key —, the public keys a server trusts, and the
// order of versions. A server installs a release only if the manifest's signature is
// one of its own keys' and the version is newer than its own: GitHub carries the files,
// it cannot make one.
package release

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	ManifestName = "footops-release.json"
	SigName      = ManifestName + ".sig"
	// Format is the manifest's: a server refuses one newer than it knows.
	Format = 1
	// sigPrefix starts the signature file: "footops-imza-1 <key id> <base64 signature>".
	sigPrefix = "footops-imza-1"
	// Size bounds for what a server downloads.
	MaxManifest = 64 << 10
	MaxBinary   = 100 << 20
	MaxNotes    = 4000
)

// Manifest is footops-release.json.
type Manifest struct {
	Format  int             `json:"format"`
	Version string          `json:"version"`
	Commit  string          `json:"commit"`
	Date    string          `json:"date"` // the commit's time, UTC (the binaries carry it)
	Files   map[string]File `json:"files"`
	// Notes say what changed, in a few lines of Turkish: the panel and Telegram show them.
	Notes string `json:"notes,omitempty"`
}

// File is a released file.
type File struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// BinaryName is the server binary of an architecture (runtime.GOARCH).
func BinaryName(arch string) string { return "footops_linux_" + arch }

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// Sign signs the manifest's exact bytes: the signature file's content.
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	sig := ed25519.Sign(priv, manifest)
	return []byte(fmt.Sprintf("%s %s %s\n", sigPrefix, KeyID(priv.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(sig)))
}

var (
	ErrSignature = errors.New("imza tutmuyor")
	ErrUntrusted = errors.New("imza tanınmayan bir anahtarla")
)

// Verify checks the signature with keys (by key id) and returns the manifest.
func Verify(manifest, sig []byte, keys map[string]ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	f := strings.Fields(string(sig))
	if len(f) != 3 || f[0] != sigPrefix {
		return m, errors.New("imza dosyası tanınmıyor")
	}
	pub, ok := keys[f[1]]
	if !ok {
		return m, fmt.Errorf("%w (%s)", ErrUntrusted, clip(f[1], 32))
	}
	raw, err := base64.StdEncoding.DecodeString(f[2])
	if err != nil || len(raw) != ed25519.SignatureSize {
		return m, errors.New("imza dosyası bozuk")
	}
	if !ed25519.Verify(pub, manifest, raw) {
		return m, ErrSignature
	}
	if err := json.Unmarshal(manifest, &m); err != nil {
		return m, fmt.Errorf("manifest okunamadı: %w", err)
	}
	return m, m.Validate()
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks what a signed manifest says before anything is downloaded on its word.
func (m Manifest) Validate() error {
	switch v, ok := ParseVersion(m.Version); {
	case m.Format < 1 || m.Format > Format:
		return fmt.Errorf("manifest biçimi %d tanınmıyor (bu sürüm %d'e kadar okur)", m.Format, Format)
	case !ok || v.Ahead > 0 || v.Dirty:
		return fmt.Errorf("manifestteki sürüm geçersiz: %q", clip(m.Version, 40))
	case len(m.Files) == 0:
		return errors.New("manifestte dosya yok")
	case len(m.Notes) > MaxNotes:
		return errors.New("manifestin notları çok uzun")
	}
	for name, f := range m.Files {
		if name == "" || strings.ContainsAny(name, "/\\") || !reSHA.MatchString(f.SHA256) || f.Size <= 0 || f.Size > MaxBinary {
			return fmt.Errorf("manifestteki dosya geçersiz: %q", clip(name, 60))
		}
	}
	return nil
}

// Check compares a file on disk with its entry.
func (f File) Check(path string) error {
	r, err := os.Open(path)
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(r, MaxBinary+1))
	if err != nil {
		return err
	}
	if n != f.Size {
		return fmt.Errorf("boyut %d bayt, manifestte %d", n, f.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("SHA-256 tutmuyor: %s…, manifestte %s…", got[:12], f.SHA256[:min(12, len(f.SHA256))])
	}
	return nil
}

// FileOf hashes a file into its manifest entry.
func FileOf(path string) (File, error) {
	r, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return File{}, err
	}
	return File{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}, nil
}

// Version is vX.Y.Z; a build after a release says how many commits after it (git
// describe: v0.1.0-3-gabc1234, maybe -dirty).
type Version struct {
	Major, Minor, Patch int
	Ahead               int
	Dirty               bool
}

var reVersion = regexp.MustCompile(`^v(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})\.(0|[1-9][0-9]{0,5})(?:-([1-9][0-9]{0,6})-g[0-9a-f]{4,40})?(-dirty)?$`)

// ParseVersion reads a version; false for anything else (a commit hash, "dev", an
// integration build's "it-…").
func ParseVersion(s string) (Version, bool) {
	m := reVersion.FindStringSubmatch(s)
	if m == nil {
		return Version{}, false
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	if m[4] != "" {
		v.Ahead, _ = strconv.Atoi(m[4])
	}
	v.Dirty = m[5] != ""
	return v, true
}

// Compare orders versions: -1, 0 or 1. A build after a release comes after it.
func (v Version) Compare(o Version) int {
	for _, d := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}, {v.Ahead, o.Ahead}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

// Newer reports whether release is newer than running. A running build that is no
// release (no version: a commit's build from before the first one) is older than every
// release.
func Newer(release, running string) bool {
	r, ok := ParseVersion(release)
	if !ok || r.Ahead > 0 || r.Dirty {
		return false
	}
	cur, ok := ParseVersion(running)
	if !ok {
		return true
	}
	return r.Compare(cur) > 0
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
