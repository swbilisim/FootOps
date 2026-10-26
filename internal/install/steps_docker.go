package install

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
)

// ---- Docker apt repository ----

const (
	dockerKeyURL      = "https://download.docker.com/linux/ubuntu/gpg"
	dockerKeyPath     = "/etc/apt/keyrings/docker.asc"
	dockerKeyDownload = configDir + "/docker.asc.download"
	dockerSourcesPath = "/etc/apt/sources.list.d/docker.sources"
	// Docker's release signing key. The downloaded key is installed only if it is
	// exactly this key, so a compromised download location cannot slip in another.
	dockerKeyFingerprint = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
)

type dockerRepoStep struct{ h Host }

func (s dockerRepoStep) Name() string { return "docker-repo" }
func (s dockerRepoStep) Describe() string {
	return "Docker'ın resmi apt deposu (imza anahtarı parmak iziyle doğrulanır)"
}

func (s dockerRepoStep) sources() (string, error) {
	osr, err := s.h.ReadFile("/etc/os-release")
	if err != nil {
		return "", err
	}
	codename := osReleaseValue(osr, "UBUNTU_CODENAME")
	if codename == "" {
		codename = osReleaseValue(osr, "VERSION_CODENAME")
	}
	if codename == "" {
		return "", errors.New("/etc/os-release içinde Ubuntu kod adı yok")
	}
	return fmt.Sprintf(`# Managed by FootOps
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: %s
Components: stable
Signed-By: %s
`, codename, dockerKeyPath), nil
}

func (s dockerRepoStep) Check(ctx context.Context) (bool, error) {
	want, err := s.sources()
	if err != nil {
		return false, err
	}
	ok, err := s.h.FileEquals(dockerSourcesPath, want)
	if err != nil || !ok {
		return false, err
	}
	return s.keyValid(ctx, dockerKeyPath) == nil, nil
}

func (s dockerRepoStep) Apply(ctx context.Context) error {
	sources, err := s.sources()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.h.Path(configDir), 0o700); err != nil {
		return err
	}
	defer s.h.Remove(dockerKeyDownload)
	if _, err := s.h.Run.Run(ctx, "curl", "-fsSL", "--proto", "=https", "--tlsv1.2",
		"-o", dockerKeyDownload, dockerKeyURL); err != nil {
		return err
	}
	if err := s.keyValid(ctx, dockerKeyDownload); err != nil {
		return err
	}
	key, err := s.h.ReadFile(dockerKeyDownload)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.h.Path("/etc/apt/keyrings"), 0o755); err != nil {
		return err
	}
	if err := s.h.WriteFile(dockerKeyPath, key, 0o644); err != nil {
		return err
	}
	return s.h.WriteFile(dockerSourcesPath, sources, 0o644)
}

// keyValid accepts the key file only if it holds exactly one key: Docker's.
func (s dockerRepoStep) keyValid(ctx context.Context, path string) error {
	out, err := s.h.Run.Run(ctx, "gpg", "--batch", "--show-keys", "--with-colons", "--with-fingerprint", path)
	if err != nil {
		return fmt.Errorf("Docker imza anahtarı okunamadı: %w", err)
	}
	return checkKeyFingerprint(out, dockerKeyFingerprint)
}

// checkKeyFingerprint parses `gpg --with-colons` output: one "pub" record whose first
// fingerprint ("fpr") line must equal want. Subkey fingerprints follow "sub" records.
func checkKeyFingerprint(colons, want string) error {
	pubs := 0
	var primary string
	afterPub := false
	for _, l := range strings.Split(colons, "\n") {
		f := strings.Split(l, ":")
		switch f[0] {
		case "pub":
			pubs++
			afterPub = true
		case "sub":
			afterPub = false
		case "fpr":
			if afterPub && primary == "" && len(f) > 9 {
				primary = f[9]
			}
			afterPub = false
		}
	}
	if pubs != 1 {
		return fmt.Errorf("Docker anahtar dosyasında %d anahtar var, 1 olmalı", pubs)
	}
	if primary != want {
		return fmt.Errorf("Docker imza anahtarı beklenen anahtar değil (parmak izi %s)", primary)
	}
	return nil
}

func osReleaseValue(osr, key string) string {
	for _, l := range strings.Split(osr, "\n") {
		if v, ok := strings.CutPrefix(l, key+"="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// ---- Docker engine ----

var (
	dockerPackages = []string{"docker-ce", "docker-ce-cli", "containerd.io", "docker-compose-plugin"}
	// Distribution packages that conflict with Docker's own (from Docker's install guide).
	dockerConflicts = []string{"docker.io", "docker-doc", "docker-compose", "docker-compose-v2", "podman-docker", "containerd", "runc"}
)

type dockerStep struct{ h Host }

func (s dockerStep) Name() string { return "docker" }
func (s dockerStep) Describe() string {
	return "Docker Engine + Compose: " + strings.Join(dockerPackages, ", ")
}

func (s dockerStep) Check(ctx context.Context) (bool, error) {
	if !installed(ctx, s.h, dockerPackages...) {
		return false, nil
	}
	return serviceActive(ctx, s.h, "docker"), nil
}

func (s dockerStep) Apply(ctx context.Context) error {
	var found []string
	for _, p := range dockerConflicts {
		if installed(ctx, s.h, p) {
			found = append(found, p)
		}
	}
	if len(found) > 0 {
		// Removing them would stop whatever already runs on this server; that decision is
		// the operator's, not the installer's.
		return fmt.Errorf("çakışan paketler kurulu: %s. FootOps Docker'ın resmi paketlerini kullanır; "+
			"bu sunucuda çalışan container yoksa kaldırıp tekrar dene: apt-get remove %s",
			strings.Join(found, ", "), strings.Join(found, " "))
	}
	if err := repairDpkg(ctx, s.h); err != nil {
		return err
	}
	if err := apt(ctx, s.h, "update"); err != nil {
		return err
	}
	if err := apt(ctx, s.h, append([]string{"install", "-y", "--no-install-recommends"}, dockerPackages...)...); err != nil {
		return err
	}
	_, err := s.h.Run.Run(ctx, "systemctl", "enable", "--now", "containerd", "docker")
	return err
}

func installed(ctx context.Context, h Host, pkgs ...string) bool {
	for _, p := range pkgs {
		out, err := h.Run.Run(ctx, "dpkg-query", "-W", "-f=${Status}", p)
		if err != nil || !strings.Contains(out, "install ok installed") {
			return false
		}
	}
	return true
}

func serviceActive(ctx context.Context, h Host, unit string) bool {
	out, err := h.Run.Run(ctx, "systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

// ---- Docker daemon configuration ----

const dockerDaemonPath = "/etc/docker/daemon.json"

// dockerDaemonJSON:
//   - log-driver local with rotation: container logs can never fill the disk.
//   - live-restore: containers keep running while dockerd restarts or upgrades.
//   - no-new-privileges: processes in containers cannot gain privileges via setuid binaries.
//   - default-address-pools: room for 256 per-project networks (the default runs out at ~30).
//     10.200.0.0/16 avoids the 10.0.0.0/16 commonly used for provider private networks.
const dockerDaemonJSON = `{
  "log-driver": "local",
  "log-opts": {
    "max-size": "10m",
    "max-file": "3"
  },
  "live-restore": true,
  "no-new-privileges": true,
  "default-address-pools": [
    {"base": "10.200.0.0/16", "size": 24}
  ]
}
`

type dockerDaemonStep struct{ h Host }

func (s dockerDaemonStep) Name() string { return "docker-daemon" }
func (s dockerDaemonStep) Describe() string {
	return "Docker ayarları: log sınırı, live-restore, no-new-privileges, ağ havuzu"
}

func (s dockerDaemonStep) Check(ctx context.Context) (bool, error) {
	ok, err := s.h.FileEquals(dockerDaemonPath, dockerDaemonJSON)
	if err != nil || !ok {
		return false, err
	}
	return serviceActive(ctx, s.h, "docker"), nil
}

func (s dockerDaemonStep) Apply(ctx context.Context) error {
	prev, err := s.h.ReadFile(dockerDaemonPath)
	if err != nil {
		return err
	}
	if err := s.h.WriteFile(dockerDaemonPath, dockerDaemonJSON, 0o644); err != nil {
		return err
	}
	// An invalid daemon.json keeps dockerd from starting; never restart with one.
	if _, err := s.h.Run.Run(ctx, "dockerd", "--validate", "--config-file", dockerDaemonPath); err != nil {
		if prev == "" {
			_ = s.h.Remove(dockerDaemonPath)
		} else {
			_ = s.h.WriteFile(dockerDaemonPath, prev, 0o644)
		}
		return err
	}
	if _, err := s.h.Run.Run(ctx, "systemctl", "restart", "docker"); err != nil {
		return err
	}
	return waitFor(ctx, 30, time.Second, func() bool {
		_, err := s.h.Run.Run(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
		return err == nil
	})
}

// waitFor polls ok up to tries times.
func waitFor(ctx context.Context, tries int, every time.Duration, ok func() bool) error {
	for i := 0; i < tries; i++ {
		if ok() {
			return nil
		}
		if err := sleep(ctx, every); err != nil {
			return err
		}
	}
	return errors.New("zaman aşımı")
}

// ---- core files ----

// coreDirs are created with these modes. Data directories are private: they hold
// databases and TLS keys. The MongoDB entrypoint chowns its directories to its own user.
var coreDirs = []struct {
	path string
	mode os.FileMode
}{
	{"/srv", 0o755},
	{core.Dir, 0o755},
	{core.CaddyDir, 0o755},
	{core.CaddyEtcDir, 0o755},
	{core.CaddySitesDir, 0o755},
	{core.CaddyCertsDir, 0o700},
	{core.CaddyDataDir, 0o700},
	{core.CaddyConfigDir, 0o700},
	{core.MongoDir, 0o755},
	{core.MongoDataDir, 0o700},
	{core.MongoConfigDir, 0o700},
	{configDir, 0o700},
	{filepath.Dir(core.MongoSecretPath), 0o700},
}

// Notes collects messages for the end of the run. They are printed to the terminal only,
// never to the log file, because they may contain secrets.
type Notes struct{ lines []string }

func (n *Notes) Add(format string, args ...any) {
	n.lines = append(n.lines, fmt.Sprintf(format, args...))
}

func (n *Notes) Lines() []string { return n.lines }

type coreFilesStep struct {
	h     Host
	cfg   core.Config
	notes *Notes
}

func (s coreFilesStep) Name() string { return "core-files" }
func (s coreFilesStep) Describe() string {
	return "Caddy ve MongoDB yapılandırması (" + core.Dir + "), MongoDB root şifresi"
}

func (s coreFilesStep) Check(ctx context.Context) (bool, error) {
	for _, d := range coreDirs {
		if !s.h.Exists(d.path) {
			return false, nil
		}
	}
	for path, want := range map[string]string{
		core.CaddyfilePath: core.Caddyfile(),
		core.ComposePath:   core.Compose(s.cfg),
	} {
		ok, err := s.h.FileEquals(path, want)
		if err != nil || !ok {
			return false, err
		}
	}
	env, err := s.h.ReadFile(core.MongoSecretPath)
	return err == nil && core.MongoPasswordFromEnv(env) != "", err
}

func (s coreFilesStep) Apply(ctx context.Context) error {
	for _, d := range coreDirs {
		p := s.h.Path(d.path)
		if err := os.MkdirAll(p, d.mode); err != nil {
			return err
		}
		// MkdirAll does not change existing directories. Data dirs keep the owner the
		// container gave them; only tighten permissions on what we own.
		if err := os.Chmod(p, d.mode); err != nil {
			return err
		}
	}
	if err := s.ensureMongoSecret(); err != nil {
		return err
	}
	if err := s.h.WriteFile(core.CaddyfilePath, core.Caddyfile(), 0o644); err != nil {
		return err
	}
	return s.h.WriteFile(core.ComposePath, core.Compose(s.cfg), 0o644)
}

// ensureMongoSecret creates the root password once. It is never regenerated: MongoDB only
// reads it when initialising an empty data directory, so a new value would not match.
func (s coreFilesStep) ensureMongoSecret() error {
	env, err := s.h.ReadFile(core.MongoSecretPath)
	if err != nil {
		return err
	}
	if core.MongoPasswordFromEnv(env) != "" {
		return nil
	}
	if entries, _ := os.ReadDir(s.h.Path(core.MongoDataDir)); len(entries) > 0 {
		return fmt.Errorf("%s dolu ama root şifre dosyası (%s) yok; yeni şifre mevcut veritabanıyla eşleşmez. "+
			"Dosyayı yedeğinden geri koy", core.MongoDataDir, core.MongoSecretPath)
	}
	pw, err := randomPassword(32)
	if err != nil {
		return err
	}
	if err := s.h.WriteFile(core.MongoSecretPath, core.MongoSecretEnv(pw), 0o600); err != nil {
		return err
	}
	if s.notes != nil {
		s.notes.Add("MongoDB root şifresi (bir kez gösteriliyor, Vaultwarden'a kaydet): %s", pw)
	}
	return nil
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomPassword returns n characters from crypto/rand. Letters and digits only, so the
// password can be pasted into connection strings and env files without escaping.
func randomPassword(n int) (string, error) {
	max := big.NewInt(int64(len(passwordAlphabet)))
	b := make([]byte, n)
	for i := range b {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = passwordAlphabet[v.Int64()]
	}
	return string(b), nil
}

// ---- core stack ----

type coreStackStep struct{ h Host }

func (s coreStackStep) Name() string { return "core-stack" }
func (s coreStackStep) Describe() string {
	return "Caddy ve MongoDB container'larını başlat ve sağlıklı olmalarını bekle"
}

func (s coreStackStep) compose(args ...string) []string {
	return append([]string{"compose", "--project-name", core.ProjectName, "--file", core.ComposePath}, args...)
}

// Check compares the hash Compose computes for each service with the hash label on the
// running container, so any change to compose.yaml or the env file is detected.
func (s coreStackStep) Check(ctx context.Context) (bool, error) {
	out, err := s.h.Run.Run(ctx, "docker", s.compose("config", "--hash", "*")...)
	if err != nil {
		return false, nil
	}
	want := parseConfigHashes(out)
	if len(want) != len(core.Services) {
		return false, nil
	}
	for svc, container := range core.Services {
		got, err := s.h.Run.Run(ctx, "docker", "inspect", "--format",
			`{{index .Config.Labels "com.docker.compose.config-hash"}} {{.State.Running}} {{if .State.Health}}{{.State.Health.Status}}{{end}}`,
			container)
		if err != nil {
			return false, nil
		}
		if !containerUpToDate(got, want[svc]) {
			return false, nil
		}
	}
	return true, nil
}

func (s coreStackStep) Apply(ctx context.Context) error {
	_, err := s.h.Run.Run(ctx, "docker", s.compose("up", "--detach", "--remove-orphans", "--wait", "--wait-timeout", "300")...)
	if err != nil {
		return fmt.Errorf("%w (ayrıntı: docker logs %s / docker logs %s)", err, core.Services["caddy"], core.Services["mongo"])
	}
	return nil
}

// parseConfigHashes parses `docker compose config --hash "*"` output ("service hash" lines).
func parseConfigHashes(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

// containerUpToDate checks `<hash> <running> <health>` from docker inspect.
func containerUpToDate(inspect, wantHash string) bool {
	f := strings.Fields(inspect)
	if len(f) < 2 || f[0] != wantHash || f[1] != "true" {
		return false
	}
	return len(f) == 2 || f[2] == "healthy"
}
