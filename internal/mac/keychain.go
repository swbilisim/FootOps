package mac

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Keychain is the Mac's Keychain as FootOps keeps it (panel-tasarim §27.18; the service
// named after footops-yedek, the bash helper footops-mac replaced): one service, an item
// per name — r2-hesap, r2-mac-erisim, r2-mac-gizli, restic-<code>, kova-<code>.
type Keychain interface {
	// Get is an item's secret; errNoItem when there is none.
	Get(ctx context.Context, name string) (string, error)
	// Names are the service's items, without their secrets.
	Names(ctx context.Context) ([]string, error)
	// Command prints an item's secret when run: restic asks it for a repository's
	// password (RESTIC_PASSWORD_COMMAND), so the password sits in no file, no argument.
	Command(name string) []string
}

var errNoItem = errors.New("Anahtar Zinciri'nde yok")

// securityKeychain asks macOS's security(1). The items were made with it, so it reads
// them without a prompt (§29.2); should one come all the same (a locked Keychain), the
// command's end ends security too.
type securityKeychain struct {
	Tool    string // /usr/bin/security: never one found in PATH
	Service string
}

// itemNotFound is security's exit code for errSecItemNotFound.
const itemNotFound = 44

func (k securityKeychain) Get(ctx context.Context, name string) (string, error) {
	cmd := exec.CommandContext(ctx, k.Tool, "find-generic-password", "-s", k.Service, "-a", name, "-w")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == itemNotFound {
			return "", fmt.Errorf("%s / %s: %w", k.Service, name, errNoItem)
		}
		return "", fmt.Errorf("Anahtar Zinciri okunamadı (%s / %s): %s", k.Service, name, clip(firstLine(stderr.String(), err.Error()), 200))
	}
	return strings.TrimSuffix(string(out), "\n"), nil
}

func (k securityKeychain) Command(name string) []string {
	return []string{k.Tool, "find-generic-password", "-s", k.Service, "-a", name, "-w"}
}

func (k securityKeychain) Names(ctx context.Context) ([]string, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, k.Tool, "dump-keychain")
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	names, serr := dumpNames(out, k.Service)
	// A keychain of the search list it could not open fails the command after the
	// others are printed: what was printed still counts.
	if (err != nil && len(names) == 0) || ctx.Err() != nil {
		return nil, fmt.Errorf("Anahtar Zinciri okunamadı: %s", clip(firstLine(stderr.String(), fmt.Sprint(err)), 200))
	}
	if serr != nil {
		return nil, fmt.Errorf("Anahtar Zinciri'nin dökümü okunamadı: %v", serr)
	}
	return names, nil
}

// dumpNames reads the account names of service's generic passwords (what
// find-generic-password reads) from security dump-keychain, which prints attributes
// only (no secret without -d): an item starts at "keychain:", its class is "genp", its
// account "acct"<blob>="…", its service "svce"<blob>="…".
func dumpNames(dump []byte, service string) ([]string, error) {
	seen := map[string]bool{}
	var class, acct, svce string
	flush := func() {
		if class == `"genp"` && svce == service && acct != "" {
			seen[acct] = true
		}
		class, acct, svce = "", "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(dump))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "keychain: "):
			flush()
		case strings.HasPrefix(line, "class: "):
			class = strings.TrimPrefix(line, "class: ")
		case strings.HasPrefix(line, `"acct"<blob>="`):
			acct = quoted(line, `"acct"<blob>=`)
		case strings.HasPrefix(line, `"svce"<blob>="`):
			svce = quoted(line, `"svce"<blob>=`)
		}
	}
	flush()
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, sc.Err()
}

// quoted is the value after prefix when it is a plain quoted string ("" otherwise: a
// value security prints in hex is no name of ours).
func quoted(line, prefix string) string {
	v := strings.TrimPrefix(line, prefix)
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return ""
	}
	return v[1 : len(v)-1]
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return fallback
	}
	return s
}
