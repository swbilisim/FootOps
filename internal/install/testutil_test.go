package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Public test keys (private halves discarded).
const (
	keyA    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBRBprM0gR46s1K3wBImL4/GXs6/gWVFdX6QjwEC/qmZ a@test"
	keyB    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKMvPq5GVDcNTPuzdhKGRl6CCcdNlhafaVh6n8y5WW+M b@test"
	keyRSA  = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCTwjH+DQGD0vnDxYaBAxkBdgN9GYLvrK7QGckgJSzFXvCLETfCQ0VTJ2uuDBYJKfkIJ/poTFbsfWEQWegTiRYp6qpWEdQ6eSxJUC84ajO0GfHf5y+aJ6AyV9V7eaiIp4oQo4/PltT1y++idJu6lhfuyHceMlKfh9zcIgJFoPwzi6Ud6wBfZvrSfSF98SPdSv59VjpgfXd3r11AHCzkeNPXSsh/wrR+oz62/ZxfGc2DCOkjt0VKopDCm4ToQxVT4T+URy068O7EwL9UrQZhCmbqLgpEuI5uo9XKqh6ZbG+hfYA3lHqZOafDY/kQld2toe8CLgAjpeb4Zu9H+4ntOD9sNsUv+bJU5Dt8av9qaMiMrktrYcoX9iIXi80CAVtaFKr1mvgo4rf+ePneqTGsu/FCOzxh3abbNYT3dIAfgyk7jRU4smpWLQxR8w+vX4VgeJdEI0ESU3gBC1xyyERSKdqsVWekbV0eMrmrncikDPDBLbwQ63UUR5nAbQJPAUjlwWc= rsa@test"
	keyWeak = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAAAgQC6D9XyiMDZ8vAtJm7wK5rvuoYTdR1C2WAvRoWGQMJAoARtkKUFU4/jpxUO95qNlZTl7l7xLZCp78ivgAv1U+JMUWcCG7VY998YpsyyUiOZSk4OFhkNb71lObomIGn8y2wsOupJWoGevtmVffleE2lTm78aYGi+fwt153V6Ya8Cyw== weak@test"
)

// call is one recorded command.
type call struct {
	cmd   string // name + args joined by spaces
	stdin string
}

// fakeRunner records commands and answers them through handle.
// handle returns (output, error); a nil handle succeeds with empty output.
type fakeRunner struct {
	calls  []call
	handle func(cmd string) (string, error)
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f.RunInput(ctx, "", name, args...)
}

func (f *fakeRunner) RunInput(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.calls = append(f.calls, call{cmd: cmd, stdin: stdin})
	if f.handle == nil {
		return "", nil
	}
	return f.handle(cmd)
}

func (f *fakeRunner) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c.cmd, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) cmds() []string {
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.cmd
	}
	return out
}

var errExit = errors.New("exit status 1")

// newTestHost returns a Host rooted in a temp dir with a fake runner.
func newTestHost(t *testing.T, handle func(cmd string) (string, error)) (Host, *fakeRunner) {
	t.Helper()
	f := &fakeRunner{handle: handle}
	return Host{Root: t.TempDir(), Run: f}, f
}

// put creates a file under the host root.
func put(t *testing.T, h Host, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(h.Path(p)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.Path(p), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, h Host, p string) string {
	t.Helper()
	s, err := h.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func noSleep(t *testing.T) {
	t.Helper()
	orig := sleep
	sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { sleep = orig })
}
