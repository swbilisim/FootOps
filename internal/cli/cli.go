// Package cli dispatches footops subcommands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/install"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/staging"
	"github.com/swbilisim/FootOps/internal/update"
	"github.com/swbilisim/FootOps/internal/version"
	"github.com/swbilisim/FootOps/internal/web"
)

const usage = `FootOps - lightweight, security-first server panel

Usage:
  footops <command> [flags]

Commands:
  install          Prepare this server and install FootOps (interactive, run as root)
                   --dry-run  only show what would change
  doctor           Check this server against the FootOps standard
  web              Run the web UI (normally started by systemd)
  agent            Run the privileged agent (normally started by systemd)
  reset-password   Reset the panel password (run as root over SSH)
  reset-2fa        Reset two-factor authentication (run as root over SSH)
  rotate-mongo-root  Give the MongoDB root user a new password (run as root)
  go-live          Make a server recovered from backups the live one (run as root)
  self-update      Download, verify and install the latest release (--check: only look)
  version          Print version information
  help             Show this help
`

// Run executes the command named in args[0] and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	var err error
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "footops %s %s/%s\n", version.String(), runtime.GOOS, runtime.GOARCH)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "data-pack", "data-extract", "data-fs", "data-zip", "data-gzip", "data-repack": // hidden: run by the agent only
		err = runData(args[0], args[1:], stdout, stderr)
	case "backup": // hidden until the panel's backup pages (Faz Y2)
		err = runBackup(args[1:], os.Stdin, stdout, stderr)
	case "rehearsal-rule": // hidden: run by footops-prova.service before Docker starts
		err = runRehearsalRule(stdout)
	case "release-verify": // hidden: CI checks the release it rebuilt (panel-tasarim §31.2)
		err = runReleaseVerify(args[1:], stdout)
	case "install":
		err = runInstall(args[1:], stdout, stderr)
		switch {
		case errors.Is(err, flag.ErrHelp):
			return 0
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(stderr, "Kurulum durduruldu. Tekrar çalıştırınca kaldığı yerden devam eder.")
			return 130
		case install.IsAborted(err):
			fmt.Fprintf(stderr, "Kurulum durduruldu: %v\n", err)
			return 1
		case errors.Is(err, install.ErrRefused):
			// An update's --auto that changed nothing: its overseer tells it by the status.
			fmt.Fprintf(stderr, "footops install: %v\n", err)
			return update.ExitRefused
		}
	case "agent":
		err = runService(func(ctx context.Context, l *log.Logger) error {
			cfg := agent.Config{Socket: agent.DefaultSocket, AuditPath: agent.DefaultAuditPath,
				HistoryPath: agent.DefaultHistory, JobsDir: agent.DefaultJobsDir, SnapshotDir: agent.DefaultSnapshotDir,
				StagingDir: staging.DefaultRoot, RecoveryDir: agent.DefaultRecoveryDir, PanelUser: agent.PanelUser,
				SourcesDir: remote.DefaultDir, MigrationsDir: agent.DefaultMigrationsDir, DBEditsDir: agent.DefaultDBEditsDir,
				DockerConfig: os.Getenv("DOCKER_CONFIG"), RegistriesPath: agent.DefaultRegistriesPath,
				BackupDir: agent.DefaultBackupDir, BackupSecret: agent.DefaultBackupSecret, ToolsDir: agent.DefaultToolsDir,
				ResticCache: agent.DefaultResticCache, NotifySecret: agent.DefaultNotifySecret, NotifyState: agent.DefaultNotifyState,
				RehearsalPath: agent.DefaultRehearsalPath, RecoverDir: agent.DefaultRecoverDir, ExpiryState: agent.DefaultExpiryState,
				DoctorPath: agent.DefaultDoctorPath, UpdateDir: update.Dir}
			if cfg.DockerConfig == "" {
				cfg.DockerConfig = agent.DefaultDockerConfig
			}
			if p, err := config.Load(config.Path); err == nil {
				cfg.PanelHost, cfg.ServerCode = p.PanelHost, p.ServerCode
			}
			// Only the integration test sets them (a systemd drop-in: root's to write).
			cfg.NotifyAPI = os.Getenv("FOOTOPS_TELEGRAM_API")
			cfg.ReleaseURL = os.Getenv("FOOTOPS_RELEASE_URL")
			return agent.Main(ctx, cfg, l)
		})
	case "web":
		err = runService(web.Main)
	case "reset-password":
		err = runRecovery(install.ResetPassword, stdout)
	case "reset-2fa":
		err = runRecovery(install.Reset2FA, stdout)
	case "rotate-mongo-root":
		err = runRecovery(install.RotateMongoRoot, stdout)
	case "go-live":
		err = runGoLive(stdout)
	case "self-update":
		err = runSelfUpdate(args[1:], os.Stdin, stdout, stderr)
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "footops %s: %v\n", args[0], err)
		return 1
	}
	return 0
}

// runDoctor checks this server against the FootOps standard (panel-tasarim §31.1). The
// exit code is 1 when there is something to fix; with --json (the agent's) it is 0
// whenever a report was written.
func runDoctor(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "write the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "footops doctor: root olarak çalıştırılmalı: sudo footops doctor")
		return 1
	}
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fmt.Fprintf(stderr, "footops doctor: %v\n", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()
	// Every command it runs only reads: safe to stop at the time limit.
	r := install.Doctor(ctx, install.Host{Root: "/", Run: run.Exec{KillOnCancel: true}}, docker.New(docker.DefaultSocket), self)
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(r); err != nil {
			fmt.Fprintf(stderr, "footops doctor: %v\n", err)
			return 1
		}
		return 0
	}
	doctor.Render(stdout, r)
	if len(r.Problems()) > 0 {
		return 1
	}
	return 0
}

// doctorTimeout bounds a run; the agent's transient service gets a little more.
const doctorTimeout = 4 * time.Minute

// runReleaseVerify checks a release folder: the manifest's signature with this build's
// keys, then each file the manifest lists, which must all be there.
func runReleaseVerify(args []string, out io.Writer) error {
	if len(args) != 1 {
		return errors.New("kullanım: footops release-verify <klasör>")
	}
	man, err := os.ReadFile(filepath.Join(args[0], release.ManifestName))
	if err != nil {
		return err
	}
	sig, err := os.ReadFile(filepath.Join(args[0], release.SigName))
	if err != nil {
		return err
	}
	m, err := release.Verify(man, sig, release.Trusted())
	if err != nil {
		return err
	}
	names := make([]string, 0, len(m.Files))
	for name := range m.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := m.Files[name].Check(filepath.Join(args[0], name)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(out, "✓ %s %s\n", name, m.Files[name].SHA256)
	}
	fmt.Fprintf(out, "%s (%s): imza ve %d dosya tutuyor\n", m.Version, m.Commit, len(names))
	return nil
}

func runInstall(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "only show what would change")
	// An update's (panel-tasarim §31.3): what this binary would change, as JSON (--plan,
	// only reads), and applying it with the saved answers, without a question (--auto).
	plan := fs.Bool("plan", false, "")
	auto := fs.Bool("auto", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *plan || *auto {
		self, err := os.Executable()
		if err == nil {
			self, err = filepath.EvalSymlinks(self)
		}
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer cancel()
		if *plan {
			p, err := install.PlanUpdate(ctx, install.Host{Root: "/", Run: run.Exec{KillOnCancel: true}}, self)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(p)
		}
		return install.ApplyUpdate(ctx, install.Host{Root: "/", Run: run.Exec{Log: stdout}}, self, stdout)
	}

	// Everything the installer prints and every command it runs goes to the log file;
	// prompts and typed answers do not.
	var logw io.Writer = io.Discard
	if !*dryRun {
		f, err := os.OpenFile(install.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("log dosyası açılamadı (root olarak çalıştır): %w", err)
		}
		defer f.Close()
		logw = f
		fmt.Fprintf(stdout, "Ayrıntılı kayıt: %s\n", install.LogPath)
	}

	fd := int(os.Stdin.Fd())
	var readSecret func() (string, error)
	restoreTerminal := func() {}
	if term.IsTerminal(fd) {
		readSecret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
		// Ctrl+C during a password prompt would otherwise leave echo switched off.
		if st, err := term.GetState(fd); err == nil {
			restoreTerminal = func() { _ = term.Restore(fd, st) }
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var changing atomic.Bool
	handleSignals(cancel, &changing, restoreTerminal, stderr)

	return install.Main(ctx, install.Options{
		DryRun: *dryRun,
		// Log first: MultiWriter stops at the first failing writer, and stdout fails once
		// the SSH session is gone.
		Out:           io.MultiWriter(logw, stdout),
		Prompt:        install.NewPrompter(os.Stdin, stdout, readSecret),
		Host:          install.Host{Root: "/", Run: install.ExecRunner{Log: logw}},
		StepsStarting: func() { changing.Store(true) },
	})
}

// handleSignals implements the installer's interrupt behaviour:
//   - While questions are asked nothing has changed yet, so Ctrl+C quits at once.
//   - Once the host is being changed, the first Ctrl+C lets the running command finish
//     and stops before the next one (killing apt/dpkg mid-way breaks the package
//     database); a second Ctrl+C quits immediately.
//   - SIGHUP (the SSH session dropped) is ignored: the installer keeps going, writes to
//     the log file, and stops safely at the next question it cannot ask.
func handleSignals(cancel context.CancelFunc, changing *atomic.Bool, restoreTerminal func(), stderr io.Writer) {
	signal.Ignore(syscall.SIGHUP)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		stopping := false
		for range sigs {
			if !changing.Load() || stopping {
				restoreTerminal()
				fmt.Fprintln(stderr, "\nKurulum iptal edildi.")
				os.Exit(130)
			}
			stopping = true
			cancel()
			fmt.Fprintln(stderr, "\nDurduruluyor: çalışan komut bitince kurulum duracak. Hemen çıkmak için tekrar Ctrl+C.")
		}
	}()
}

// runService runs a long-lived systemd service until SIGTERM/SIGINT. Logs go to stderr,
// which journald collects (and timestamps).
func runService(run func(context.Context, *log.Logger) error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, log.New(os.Stderr, "", 0))
}

// runRehearsalRule shuts a recovered server's sites off from the outside before Docker
// starts them at boot (the agent, which keeps the rule otherwise, starts after Docker):
// DOCKER-USER and FORWARD's jump to it are made if Docker has not made them yet — it
// keeps both when it starts.
func runRehearsalRule(stdout io.Writer) error {
	if _, err := os.Stat(agent.DefaultRehearsalPath); err != nil {
		return nil // not a rehearsal
	}
	r := run.Exec{}
	ctx := context.Background()
	ipt := func(args ...string) error {
		_, err := r.Run(ctx, "iptables", append([]string{"-w", "10"}, args...)...)
		return err
	}
	if ipt("-n", "-L", "DOCKER-USER") != nil {
		if err := ipt("-N", "DOCKER-USER"); err != nil {
			return err
		}
	}
	if ipt("-C", "FORWARD", "-j", "DOCKER-USER") != nil {
		if err := ipt("-I", "FORWARD", "1", "-j", "DOCKER-USER"); err != nil {
			return err
		}
	}
	if _, err := r.Run(ctx, "iptables", agent.RuleCheckArgs()...); err != nil {
		if _, err := r.Run(ctx, "iptables", agent.RuleArgs()...); err != nil {
			return err
		}
	}
	fmt.Fprintln(stdout, "prova kipi: sitelerin dışarıya bağlantısı kapalı")
	return nil
}

// runGoLive makes a recovered server the live one; its commands go to the install log.
func runGoLive(stdout io.Writer) error {
	f, err := os.OpenFile(install.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("log dosyası açılamadı (root olarak çalıştır): %w", err)
	}
	defer f.Close()
	var readSecret func() (string, error)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		readSecret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	h := install.Host{Root: "/", Run: install.ExecRunner{Log: f}}
	return install.GoLive(ctx, h, install.NewPrompter(os.Stdin, stdout, readSecret), io.MultiWriter(f, stdout), install.AgentGoLive())
}

func runRecovery(run func(context.Context, install.Host, *install.Prompter) error, stdout io.Writer) error {
	var readSecret func() (string, error)
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		readSecret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
	}
	h := install.Host{Root: "/", Run: install.ExecRunner{}}
	return run(context.Background(), h, install.NewPrompter(os.Stdin, stdout, readSecret))
}
