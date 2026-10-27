// Package sites describes a site hosted by FootOps and renders its files: the
// compose file, the Caddy site block and the metadata FootOps keeps next to them.
//
// Everything about a site lives in /srv/<group>/<name>/:
//
//	footops.json   what the operator asked for (no secrets)
//	compose.yaml   generated; holds the environment, including secrets (0600)
//	data/          optional persistent directory mounted into the container
//
// The files on disk are the source of truth; the agent never keeps site state elsewhere.
package sites

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Groups are the fixed top-level folders under /srv.
var Groups = []string{"botaniksan", "posmerkezi", "startup", "musteri", "kuleonu", "kisisel"}

const (
	SrvDir       = "/srv"
	MetaFile     = "footops.json"
	ComposeFile  = "compose.yaml"
	DataDir      = "data"
	maxDomains   = 20
	maxEnv       = 100
	maxValueLen  = 4096
	minMemoryMB  = 64
	maxMemoryMB  = 16384
	defaultDBVar = "MONGODB_URI"
)

// Spec is a site as the operator defines it.
type Spec struct {
	Name     string   `json:"name"`
	Group    string   `json:"group"`
	Domains  []string `json:"domains"`
	Image    string   `json:"image"`
	Port     int      `json:"port"`
	MemoryMB int      `json:"memory_mb"`
	// DataMount is the path inside the container where /srv/<group>/<name>/data is
	// mounted (uploads etc.). Empty means no persistent directory.
	DataMount string `json:"data_mount,omitempty"`
	// DataURL is where the site serves that directory's files ("/uploads"), so the file
	// manager can link a file to its page. Optional; it changes nothing in the container.
	DataURL string `json:"data_url,omitempty"`
	// Database creates a MongoDB database and user named after the site and passes the
	// connection string in DBVar.
	Database bool   `json:"database"`
	DBVar    string `json:"db_var,omitempty"`
	// EnvKeys lists the variable names only; values live in compose.yaml (0600).
	EnvKeys   []string  `json:"env_keys,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Dir is the site's directory on the host.
func (s Spec) Dir() string { return path.Join(SrvDir, s.Group, s.Name) }

// Network is the site's own Docker network. Caddy (and MongoDB, if used) are attached to
// it; sites cannot reach each other.
func (s Spec) Network() string { return "prj_" + s.Group + "_" + s.Name }

// Container is the container name, which is also its host name on the site network.
func (s Spec) Container() string { return s.Name }

// DBName and DBUser are the MongoDB database and user for the site.
func (s Spec) DBName() string { return s.Name }
func (s Spec) DBUser() string { return s.Name }

var (
	reName     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)
	reLabel    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reEnvKey   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	reImage    = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)
	reMount    = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
	reDataURL  = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+){1,20}$`)
	reserved   = map[string]bool{"core": true, "caddy": true, "mongo": true, "cloudflared": true, "docker": true, "admin": true}
	errInvalid = errors.New("geçersiz site")
)

// ValidationError lists every problem in a spec, so a form can show them all at once.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string { return strings.Join(e.Problems, "; ") }
func (e *ValidationError) Unwrap() error { return errInvalid }

// Normalize lower-cases and trims fields and removes duplicate domains.
func (s *Spec) Normalize() {
	s.Name = strings.ToLower(strings.TrimSpace(s.Name))
	s.Group = strings.ToLower(strings.TrimSpace(s.Group))
	s.Image = strings.TrimSpace(s.Image)
	s.DataMount = strings.TrimSpace(s.DataMount)
	s.DataURL = strings.TrimRight(strings.TrimSpace(s.DataURL), "/")
	s.DBVar = strings.TrimSpace(s.DBVar)
	if s.Database && s.DBVar == "" {
		s.DBVar = defaultDBVar
	}
	if !s.Database {
		s.DBVar = ""
	}
	seen := map[string]bool{}
	var ds []string
	for _, d := range s.Domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && !seen[d] {
			seen[d] = true
			ds = append(ds, d)
		}
	}
	s.Domains = ds
}

// Validate checks the spec and, with env, the environment variables to be set.
func (s Spec) Validate(env map[string]string) error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	if !reName.MatchString(s.Name) {
		add("proje adı 3-32 karakter: küçük harf, rakam ve tire; harfle başlamalı")
	} else if reserved[s.Name] || strings.HasPrefix(s.Name, "footops") {
		add("%q ayrılmış bir ad", s.Name)
	}
	if !validGroup(s.Group) {
		add("grup şunlardan biri olmalı: %s", strings.Join(Groups, ", "))
	}
	if len(s.Domains) == 0 {
		add("en az bir alan adı gerekli")
	}
	if len(s.Domains) > maxDomains {
		add("en fazla %d alan adı", maxDomains)
	}
	for _, d := range s.Domains {
		if err := ValidateDomain(d); err != nil {
			add("%s: %v", d, err)
		}
	}
	if !reImage.MatchString(s.Image) || strings.Contains(s.Image, "..") {
		add("imaj adı geçersiz (örn. ghcr.io/kullanici/site:main)")
	}
	if s.Port < 1 || s.Port > 65535 {
		add("uygulama portu 1-65535 arasında olmalı")
	}
	if s.MemoryMB < minMemoryMB || s.MemoryMB > maxMemoryMB {
		add("bellek sınırı %d-%d MB arasında olmalı", minMemoryMB, maxMemoryMB)
	}
	if s.DataMount != "" && (!reMount.MatchString(s.DataMount) || strings.Contains(s.DataMount, "..") || s.DataMount == "/") {
		add("kalıcı klasör yolu mutlak bir yol olmalı (örn. /app/uploads)")
	}
	if s.DataURL != "" && !ValidDataURL(s.DataURL) {
		add("dosyaların web adresi / ile başlayan bir yol olmalı (örn. /uploads)")
	}
	if s.Database && !reEnvKey.MatchString(s.DBVar) {
		add("veritabanı değişken adı geçersiz")
	}
	if len(env) > maxEnv {
		add("en fazla %d ortam değişkeni", maxEnv)
	}
	for k, v := range env {
		if !reEnvKey.MatchString(k) {
			add("ortam değişkeni adı geçersiz: %q", k)
		}
		if len(v) > maxValueLen || strings.ContainsAny(v, "\n\r\x00") {
			add("%s değeri tek satır ve en fazla %d karakter olmalı", k, maxValueLen)
		}
		if s.Database && k == s.DBVar {
			add("%s veritabanı bağlantısı için ayrıldı", k)
		}
	}
	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// ValidDataURL accepts a path like /uploads or /media/files (no "." or ".." parts).
func ValidDataURL(u string) bool {
	if !reDataURL.MatchString(u) || len(u) > 200 {
		return false
	}
	for _, c := range strings.Split(u[1:], "/") {
		if c == "." || c == ".." {
			return false
		}
	}
	return true
}

func validGroup(g string) bool {
	for _, x := range Groups {
		if x == g {
			return true
		}
	}
	return false
}

// ValidateDomain accepts a lower-case host name with at least two labels.
func ValidateDomain(d string) error {
	labels := strings.Split(d, ".")
	if len(d) > 253 || len(labels) < 2 {
		return errors.New("tam alan adı olmalı (örn. ornek.com)")
	}
	for _, l := range labels {
		if !reLabel.MatchString(l) {
			return fmt.Errorf("geçersiz bölüm %q", l)
		}
	}
	return nil
}

// ParseEnv reads KEY=VALUE lines. Blank lines and lines starting with # are ignored.
// Values are taken literally (no quotes are removed), so what you type is what the app gets.
func ParseEnv(text string) (map[string]string, error) {
	env := map[string]string{}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("satır %d: KEY=DEĞER biçiminde olmalı", i+1)
		}
		if _, dup := env[k]; dup {
			return nil, fmt.Errorf("satır %d: %s iki kez tanımlanmış", i+1, k)
		}
		env[k] = v
	}
	return env, nil
}

// SortedKeys returns the map's keys in order, for deterministic output.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MarshalMeta renders footops.json.
func (s Spec) MarshalMeta() ([]byte, error) {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
