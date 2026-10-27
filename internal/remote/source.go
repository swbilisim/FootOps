// Package remote reaches the servers sites are moved from (F4): a key pair per source
// server, its pinned host key, and the scripts the agent runs there over ssh. The ssh
// client only carries bytes. What comes back is parsed here with limits, or streamed
// to the unprivileged file helper and the database tools, never trusted as it is.
package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/swbilisim/FootOps/internal/run"
)

const (
	DefaultDir = "/var/lib/footops/sources"
	sourceFile = "source.json"
	keyFile    = "id_ed25519"
	knownHosts = "known_hosts"
)

// Source is a server data is copied from.
type Source struct {
	ID      string    `json:"id"`
	Label   string    `json:"label"`
	Host    string    `json:"host"`
	Port    int       `json:"port"`
	User    string    `json:"user"`
	Created time.Time `json:"created"`
	Actor   string    `json:"actor,omitempty"`
	// PublicKey is this server's key for the source: "ssh-ed25519 AAAA… comment".
	PublicKey string `json:"public_key"`
	// HostKeys are the fingerprints of the source's key, pinned at the first connection
	// (trust on first use); empty until then.
	HostKeys []string  `json:"host_keys,omitempty"`
	Checked  time.Time `json:"checked,omitempty"` // last successful connection
	System   string    `json:"system,omitempty"`  // what the source said about itself
}

// Pinned reports whether the source's host key is known.
func (s Source) Pinned() bool { return len(s.HostKeys) > 0 }

// Store keeps the sources, each in its own root-only directory with its key pair.
type Store struct {
	Dir string
	// Run runs ssh-keygen.
	Run run.Runner
	// Stream runs ssh.
	Stream run.Streamer
	// Name is this server's name, put into the key's comment so the line can be found
	// on the source later.
	Name string

	mu  sync.Mutex
	now func() time.Time
}

func (s *Store) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

var (
	reID   = regexp.MustCompile(`^[a-z0-9]{12}$`)
	reUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	// A host name: letters, digits, hyphens and dots, not starting with a hyphen.
	reHostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*\.?$`)
	reKeyName  = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// ErrNotFound means the source is unknown.
var ErrNotFound = errors.New("kaynak sunucu bulunamadı")

// Validate checks what the operator typed. Every value later ends up in an ssh command
// line, after "--" for the host, so it must be what it claims to be.
func Validate(label, host string, port int, user string) []string {
	var problems []string
	if l := strings.TrimSpace(label); l == "" || utf8.RuneCountInString(l) > 40 || strings.IndexFunc(l, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0 {
		problems = append(problems, "Ad 1-40 yazdırılabilir karakter olmalı (ör. “eski sunucu”).")
	}
	if !ValidHost(host) {
		problems = append(problems, "Adres bir IP ya da alan adı olmalı (ör. 203.0.113.10).")
	}
	if port < 1 || port > 65535 {
		problems = append(problems, "Port 1-65535 arasında olmalı (SSH için genelde 22).")
	}
	if !reUser.MatchString(user) {
		problems = append(problems, "Kullanıcı adı geçersiz (eski sunucularda genelde root).")
	}
	return problems
}

// ValidHost accepts an IP address or a host name.
func ValidHost(h string) bool {
	if h == "" || len(h) > 253 || strings.HasPrefix(h, "-") {
		return false
	}
	if ip := net.ParseIP(h); ip != nil {
		return !ip.IsUnspecified()
	}
	return reHostname.MatchString(h)
}

func (s *Store) dir(id string) string { return filepath.Join(s.Dir, id) }

func newID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b), nil
}

// Create adds a source and makes its key pair (ed25519, no passphrase: the agent uses
// it unattended; the private key never leaves this server's root-only directory).
func (s *Store) Create(ctx context.Context, label, host string, port int, user, actor string) (Source, error) {
	if p := Validate(label, host, port, user); len(p) > 0 {
		return Source{}, &ValidationError{Problems: p}
	}
	id, err := newID()
	if err != nil {
		return Source{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return Source{}, err
	}
	dir := s.dir(id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Source{}, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	comment := "footops-" + keyName(s.Name) + "-" + id
	if _, err := s.Run.Run(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", filepath.Join(dir, keyFile)); err != nil {
		return Source{}, fmt.Errorf("anahtar üretilemedi: %w", err)
	}
	pub, err := os.ReadFile(filepath.Join(dir, keyFile+".pub"))
	if err != nil {
		return Source{}, err
	}
	line := strings.TrimSpace(string(pub))
	if f := strings.Fields(line); len(f) != 3 || f[0] != "ssh-ed25519" || f[2] != comment {
		return Source{}, errors.New("üretilen anahtar beklenen biçimde değil")
	}
	if err := os.WriteFile(filepath.Join(dir, knownHosts), nil, 0o600); err != nil {
		return Source{}, err
	}
	src := Source{ID: id, Label: strings.TrimSpace(label), Host: host, Port: port, User: user, Created: s.clock().UTC(),
		Actor: actor, PublicKey: line}
	if err := s.save(src); err != nil {
		return Source{}, err
	}
	ok = true
	return src, nil
}

// keyName makes this server's name safe for a key comment.
func keyName(n string) string {
	n = strings.Trim(reKeyName.ReplaceAllString(strings.ToLower(n), "-"), "-.")
	if n == "" {
		return "sunucu"
	}
	return n
}

func (s *Store) save(src Source) error {
	b, err := json.MarshalIndent(src, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir(src.ID), sourceFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir(src.ID), sourceFile))
}

// Get returns a source.
func (s *Store) Get(id string) (Source, error) {
	var src Source
	if !reID.MatchString(id) {
		return src, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(s.dir(id), sourceFile))
	if errors.Is(err, fs.ErrNotExist) {
		return src, ErrNotFound
	}
	if err != nil {
		return src, err
	}
	if err := json.Unmarshal(b, &src); err != nil || src.ID != id || !ValidHost(src.Host) || !reUser.MatchString(src.User) ||
		src.Port < 1 || src.Port > 65535 {
		return Source{}, errors.New("kaynak kaydı bozuk")
	}
	return src, nil
}

// List returns the sources, oldest first.
func (s *Store) List() ([]Source, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Source{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Source{}
	for _, e := range ents {
		if !e.IsDir() || !reID.MatchString(e.Name()) {
			continue
		}
		if src, err := s.Get(e.Name()); err == nil {
			out = append(out, src)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out, nil
}

// Remove deletes a source and its keys.
func (s *Store) Remove(id string) error {
	if _, err := s.Get(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.dir(id))
}

// update changes a stored source under the lock.
func (s *Store) update(id string, fn func(*Source)) (Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.Get(id)
	if err != nil {
		return src, err
	}
	fn(&src)
	return src, s.save(src)
}

// ResetHostKey forgets the pinned host key: the next connection pins the key the
// source shows then (for a source that was reinstalled).
func (s *Store) ResetHostKey(id string) (Source, error) {
	src, err := s.update(id, func(src *Source) { src.HostKeys, src.Checked = nil, time.Time{} })
	if err != nil {
		return src, err
	}
	return src, os.WriteFile(filepath.Join(s.dir(id), knownHosts), nil, 0o600)
}

// AuthorizedLine is the line to add to ~/.ssh/authorized_keys on the source. restrict
// turns off forwarding, PTYs and ~/.ssh/rc; from= limits the key to this server's
// addresses (none known: no limit).
func AuthorizedLine(src Source, from []string) string {
	opts := "restrict"
	var ok []string
	for _, a := range from {
		if ip := net.ParseIP(a); ip != nil {
			ok = append(ok, ip.String())
		}
	}
	if len(ok) > 0 {
		opts += `,from="` + strings.Join(ok, ",") + `"`
	}
	return opts + " " + src.PublicKey
}

// InstallCommand adds line to the authorized keys of the user who runs it (on the
// source, as the user the panel connects as). A file whose last line lacks its newline
// gets one first, or the line would be glued to someone else's key; the same line twice
// is added once. SELinux (CentOS) needs the right label on a new ~/.ssh.
func InstallCommand(line string) string {
	return "umask 077; mkdir -p ~/.ssh && f=~/.ssh/authorized_keys && " +
		"{ [ ! -s \"$f\" ] || [ -z \"$(tail -c 1 \"$f\")\" ] || echo >> \"$f\"; } && " +
		"{ grep -qxF -- " + Quote(line) + " \"$f\" 2>/dev/null || printf '%s\\n' " + Quote(line) + " >> \"$f\"; } && " +
		"{ command -v restorecon >/dev/null 2>&1 && restorecon -R ~/.ssh || true; }"
}

// RemoveCommand removes this source's line from the authorized keys: exactly the lines
// whose key is ours (after an options field, if any), never a line that only mentions it.
// The new file replaces the old one in one rename; nothing is written when no line is
// ours, and a failure leaves the file as it was.
func RemoveCommand(src Source) (string, error) {
	f := strings.Fields(src.PublicKey)
	if len(f) != 3 || f[0] != "ssh-ed25519" || !reKeyBlob.MatchString(f[1]) || !strings.HasPrefix(f[2], "footops-") {
		return "", errors.New("anahtar beklenen biçimde değil")
	}
	match := `{i = ($1 == "ssh-ed25519") ? 1 : 2; m = ($i == "ssh-ed25519" && $(i+1) == k)}`
	return "f=~/.ssh/authorized_keys; [ -f \"$f\" ] || exit 0; k=" + Quote(f[1]) + "; " +
		"n=$(awk -v k=\"$k\" " + Quote(match+` m {c++} END {print c+0}`) + " \"$f\") || exit 1; " +
		"[ \"$n\" -gt 0 ] || exit 0; umask 077; " +
		"awk -v k=\"$k\" " + Quote(match+` !m {print}`) + " \"$f\" > \"$f.footops-tmp\" || { rm -f \"$f.footops-tmp\"; exit 1; }; " +
		"mv -f \"$f.footops-tmp\" \"$f\"", nil
}

var reKeyBlob = regexp.MustCompile(`^[A-Za-z0-9+/]+={0,2}$`)

// Fingerprints reads the pinned host keys as OpenSSH shows them ("ED25519 SHA256:…").
func (s *Store) Fingerprints(id string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(s.dir(id), knownHosts))
	if err != nil {
		return nil, err
	}
	return fingerprints(b), nil
}

func fingerprints(knownHostsData []byte) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range bytes.Split(knownHostsData, []byte("\n")) {
		f := strings.Fields(string(line))
		if len(f) < 3 || strings.HasPrefix(f[0], "#") || strings.HasPrefix(f[0], "@") {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(f[2])
		if err != nil {
			continue
		}
		sum := sha256.Sum256(blob)
		fp := keyType(f[1]) + " SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
		if !seen[fp] {
			seen[fp] = true
			out = append(out, fp)
		}
	}
	return out
}

func keyType(t string) string {
	switch {
	case t == "ssh-ed25519":
		return "ED25519"
	case strings.HasPrefix(t, "ecdsa-"):
		return "ECDSA"
	case t == "ssh-rsa":
		return "RSA"
	}
	return strings.ToUpper(t)
}

// ValidationError lists what is wrong with a new source.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, " ") }
