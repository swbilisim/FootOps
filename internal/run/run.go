// Package run executes external commands safely: never through a shell, in their own
// process group, and without killing a command that is already running.
package run

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner executes external commands. Tests substitute a fake.
type Runner interface {
	// Run executes name with args and returns combined stdout+stderr.
	Run(ctx context.Context, name string, args ...string) (string, error)
	// RunInput is Run with stdin. stdin is never logged (it may carry a password).
	RunInput(ctx context.Context, stdin string, name string, args ...string) (string, error)
}

// Exec runs real commands and logs each invocation and its output to Log.
//
// Cancellation is deliberately soft: a cancelled ctx prevents new commands from
// starting, but a command that is already running is never killed. Killing apt or
// dpkg half-way leaves the package database broken. Commands also run in their own
// process group, so Ctrl+C or a dropped SSH session (SIGHUP) reaches only the
// installer, which then stops cleanly between commands.
type Exec struct {
	Log io.Writer
	// KillOnCancel kills a running command (and its children) when ctx ends. Only for
	// commands that are safe to interrupt, such as read-only queries; the installer
	// never sets it.
	KillOnCancel bool
}

// Non-interactive apt and needrestart (Ubuntu 24.04's needrestart otherwise stops apt
// with a full-screen prompt); C locale so tool output can be parsed.
var execEnv = []string{"DEBIAN_FRONTEND=noninteractive", "NEEDRESTART_MODE=a", "LC_ALL=C", "LANG=C"}

func (r Exec) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.run(ctx, nil, name, args...)
}

func (r Exec) RunInput(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	return r.run(ctx, strings.NewReader(stdin), name, args...)
}

func (r Exec) run(ctx context.Context, stdin io.Reader, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.Command(name, args...)
	if r.KillOnCancel {
		cmd = exec.CommandContext(ctx, name, args...)
		cmd.Cancel = func() error { killGroup(cmd); return nil }
		cmd.WaitDelay = stopGrace
	}
	cmd.Env = append(os.Environ(), execEnv...)
	cmd.Stdin = stdin
	setOwnProcessGroup(cmd)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if r.Log != nil {
		fmt.Fprintf(r.Log, "$ %s %s\n%s", name, strings.Join(args, " "), buf.String())
		if err != nil {
			fmt.Fprintf(r.Log, "-> %v\n", err)
		}
	}
	if err != nil {
		return buf.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, lastLine(buf.String()))
	}
	return buf.String(), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
