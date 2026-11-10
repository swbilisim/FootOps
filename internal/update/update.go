// Package update is FootOps updating itself (panel-tasarim §31.3): fetching the newest
// signed release, downloading its binary beside the running one, and the records an
// update keeps in /var/lib/footops/update. The agent checks for releases and starts an
// update; `footops self-update --apply`, run by the new binary in a transient service,
// applies it (package install) and goes back to the previous binary if the new one does
// not come up. Both read and write these records.
package update

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/release"
)

const (
	// Dir keeps the downloaded releases, the previous binary (PrevDir) and the records.
	Dir = "/var/lib/footops/update"
	// Unit is the transient service an update runs in, outside the agent's sandbox.
	Unit = "footops-update"
	// DefaultBase is where releases are published: GitHub's releases of the repository
	// (latest/download/<file>, download/<version>/<file>).
	DefaultBase = "https://github.com/swbilisim/FootOps/releases"

	prevName    = "prev"
	pendingName = "prev.new"
	runName     = "run.json"
)

// PrevDir keeps the binary the last update replaced, for going back.
func PrevDir(dir string) string { return filepath.Join(dir, prevName) }

// PendingDir keeps the installed binary while an update runs: the way back if the new
// one does not come up. It becomes PrevDir only when the update succeeds — a failed one
// leaves the earlier way back as it was.
func PendingDir(dir string) string { return filepath.Join(dir, pendingName) }

// Promote makes the kept binary of a successful update the way back.
func Promote(dir string) error {
	if err := os.RemoveAll(PrevDir(dir)); err != nil {
		return err
	}
	return os.Rename(PendingDir(dir), PrevDir(dir))
}

// VersionDir keeps a downloaded release.
func VersionDir(dir, version string) string { return filepath.Join(dir, version) }

// Source is where releases come from.
type Source struct {
	Base string
	HTTP *http.Client
	Keys map[string]ed25519.PublicKey
}

// Release is a verified manifest and the bytes it was verified from.
type Release struct {
	release.Manifest
	Raw, Sig []byte
}

// ErrNotVerified wraps a signature that does not hold: a release that is not ours, or
// was changed on the way.
var ErrNotVerified = errors.New("sürüm doğrulanamadı")

// Latest fetches and verifies the newest release's manifest.
func (s Source) Latest(ctx context.Context) (Release, error) {
	return s.fetch(ctx, s.Base+"/latest/download/")
}

func (s Source) fetch(ctx context.Context, prefix string) (Release, error) {
	man, err := s.get(ctx, prefix+release.ManifestName, release.MaxManifest)
	if err != nil {
		return Release{}, err
	}
	sig, err := s.get(ctx, prefix+release.SigName, 4096)
	if err != nil {
		return Release{}, err
	}
	m, err := release.Verify(man, sig, s.Keys)
	if err != nil {
		return Release{}, fmt.Errorf("%w: %v", ErrNotVerified, err)
	}
	return Release{Manifest: m, Raw: man, Sig: sig}, nil
}

func (s Source) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: %d bayttan büyük", url, limit)
	}
	return b, nil
}

// Download puts the release's binary for arch into dir/<version>/footops, checked
// against the manifest, with the manifest and its signature beside it. A binary
// already there and right is kept.
func (s Source) Download(ctx context.Context, r Release, arch, dir string) (string, error) {
	name := release.BinaryName(arch)
	f, ok := r.Files[name]
	if !ok {
		return "", fmt.Errorf("%s için ikili yok (%s)", arch, r.Version)
	}
	vdir := VersionDir(dir, r.Version)
	if err := os.MkdirAll(vdir, 0o700); err != nil {
		return "", err
	}
	bin := filepath.Join(vdir, "footops")
	if f.Check(bin) == nil {
		return bin, writeRecords(vdir, r)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Base+"/download/"+r.Version+"/"+name, nil)
	if err != nil {
		return "", err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s indirilemedi: %s", name, resp.Status)
	}
	part := bin + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, io.LimitReader(resp.Body, f.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = f.Check(part)
	}
	if err != nil {
		os.Remove(part)
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if err := os.Rename(part, bin); err != nil {
		return "", err
	}
	return bin, writeRecords(vdir, r)
}

func writeRecords(vdir string, r Release) error {
	if err := os.WriteFile(filepath.Join(vdir, release.ManifestName), r.Raw, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(vdir, release.SigName), r.Sig, 0o600)
}

// Plan is `footops install --plan`: what applying a binary would change, as the binary
// itself sees it.
type Plan struct {
	Steps []PlanStep `json:"steps"`
	// Interactive names the changing steps that need someone at a terminal (SSH): the
	// panel cannot apply such an update.
	Interactive []string `json:"interactive,omitempty"`
	// Impact says, a line each, what visitors and the operator will notice.
	Impact []string `json:"impact,omitempty"`
}

// PlanStep is one installer step.
type PlanStep struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Change bool   `json:"change"`
	Detail string `json:"detail,omitempty"` // what the step does
	Err    string `json:"error,omitempty"`  // its check failed
}

// Phases of an apply.
const (
	PhaseApplying   = "applying"
	PhaseDone       = "done"        // the new version runs
	PhaseRolledBack = "rolled-back" // it did not come up: the previous one runs again
	PhaseFailed     = "failed"      // neither came up: someone has to look (SSH)
	// PhaseRefused: stopped before anything changed (a step would need a terminal, the
	// installed binary could not be kept): the running version stays.
	PhaseRefused = "refused"
)

// Run is run.json: the last apply.
type Run struct {
	Job  string `json:"job,omitempty"` // the agent's job that started it
	From string `json:"from"`
	To   string `json:"to"`
	// Back: a going back to the kept previous binary, which then stays the kept one.
	Back     bool      `json:"back,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	Phase    string    `json:"phase"`
	Err      string    `json:"error,omitempty"`
	Log      string    `json:"log,omitempty"`
}

// Final reports whether the apply has ended.
func (r Run) Final() bool {
	return r.Phase == PhaseDone || r.Phase == PhaseRolledBack || r.Phase == PhaseFailed || r.Phase == PhaseRefused
}

// ReadRun reads dir's run.json; nil when there is none.
func ReadRun(dir string) (*Run, error) {
	b, err := os.ReadFile(filepath.Join(dir, runName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s okunamadı: %w", runName, err)
	}
	return &r, nil
}

// WriteRun replaces dir's run.json (written aside and renamed: never half a file).
func WriteRun(dir string, r Run) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+runName+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, runName))
}

// PrevVersion is the version of the kept previous binary ("" when none).
func PrevVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(PrevDir(dir), "version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Tail returns a log's last n lines (none if it cannot be read).
func Tail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	const max = 256 << 10
	if st, err := f.Stat(); err == nil && st.Size() > max {
		_, _ = f.Seek(st.Size()-max, io.SeekStart)
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// InstalledBin is where the installed footops is: the services run it, and it oversees
// an update to another binary.
const InstalledBin = "/usr/local/bin/footops"

// ExitRefused is `footops install --auto`'s exit status when it changed nothing because
// a step would need a terminal.
const ExitRefused = 3

// UnitArgs are systemd-run's arguments for an apply. The installed binary — the one
// known to work — oversees it in a transient service, outside the agent's sandbox and
// alive while the agent restarts: target (the release's binary, or the kept previous
// one going back) applies its own standard (`install --auto`), and if it does not come
// up the overseer puts its own back. A stop (systemctl stop, the time limit, a
// shutdown) reaches the overseer alone (KillMode=mixed), which stops the update and goes
// back within TimeoutStopSec.
func UnitArgs(target, from, to, job string, back bool) []string {
	args := []string{"--unit=" + Unit, "--description=FootOps güncellemesi " + from + " → " + to, "--quiet", "--collect",
		"--property=RuntimeMaxSec=2700", "--property=KillMode=mixed", "--property=TimeoutStopSec=900",
		"--", InstalledBin, "self-update", "--apply", "--target", target, "--from", from, "--to", to}
	if job != "" {
		args = append(args, "--job", job)
	}
	if back {
		args = append(args, "--back")
	}
	return args
}
