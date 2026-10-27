// Package cli dispatches footops subcommands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"

	"golang.org/x/term"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/install"
	"github.com/swbilisim/FootOps/internal/staging"
	"github.com/swbilisim/FootOps/internal/sysinfo"
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
  self-update      Download, verify and install the latest release
  version          Print version information
  help             Show this help
`

var errNotImplemented = errors.New("not implemented yet")

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
		err = runDoctor(stdout)
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
		}
	case "agent":
		err = runService(func(ctx context.Context, l *log.Logger) error {
			cfg := agent.Config{Socket: agent.DefaultSocket, AuditPath: agent.DefaultAuditPath,
				HistoryPath: agent.DefaultHistory, JobsDir: agent.DefaultJobsDir, SnapshotDir: agent.DefaultSnapshotDir,
				StagingDir: staging.DefaultRoot, RecoveryDir: agent.DefaultRecoveryDir, PanelUser: agent.PanelUser}
			if p, err := config.Load(config.Path); err == nil {
				cfg.PanelHost = p.PanelHost
			}
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
	case "self-update":
		err = errNotImplemented
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

// runDoctor is a placeholder that reports memory sizing; full checks arrive with the installer.
func runDoctor(out io.Writer) error {
	mem, err := sysinfo.ReadMemory()
	if err != nil {
		return fmt.Errorf("read memory (doctor only runs on Linux): %w", err)
	}
	want := sysinfo.RecommendSwap(mem.Total)
	fmt.Fprintf(out, "RAM:  %s\n", sysinfo.HumanBytes(mem.Total))
	fmt.Fprintf(out, "Swap: %s (recommended %s)\n", sysinfo.HumanBytes(mem.SwapTotal), sysinfo.HumanBytes(want))
	fmt.Fprintf(out, "MongoDB cache: %.1f GB recommended\n", sysinfo.RecommendMongoCacheGB(mem.Total))
	return nil
}

func runInstall(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "only show what would change")
	if err := fs.Parse(args); err != nil {
		return err
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
