// Package cli dispatches footops subcommands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/swbilisim/FootOps/internal/sysinfo"
	"github.com/swbilisim/FootOps/internal/version"
)

const usage = `FootOps - lightweight, security-first server panel

Usage:
  footops <command> [flags]

Commands:
  install          Prepare this server and install FootOps (interactive, run as root)
  doctor           Check this server against the FootOps standard
  web              Run the web UI (normally started by systemd)
  agent            Run the privileged agent (normally started by systemd)
  reset-password   Reset the panel password (run as root over SSH)
  reset-2fa        Reset two-factor authentication (run as root over SSH)
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
	case "install", "web", "agent", "reset-password", "reset-2fa", "self-update":
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
