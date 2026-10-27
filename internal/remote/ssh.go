package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// Quote makes s a single word for a POSIX shell.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// command is what the source's login shell runs: sh -c with the script and its
// arguments, each quoted, so no value is ever parsed as shell code. Scripts are POSIX
// sh (Ubuntu's dash, CentOS's bash).
func command(script string, args ...string) string {
	var b strings.Builder
	b.WriteString("sh -c ")
	b.WriteString(Quote(script))
	b.WriteString(" footops")
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(Quote(a))
	}
	return b.String()
}

// sshArgs is the ssh command line for src. No configuration file, agent, forwarding or
// multiplexing; the host key must match the pinned one (accept: pin it now, for the
// first connection only); a dead connection is noticed within a minute.
func (s *Store) sshArgs(src Source, accept bool, remote string) []string {
	dir := s.dir(src.ID)
	check := "yes"
	if accept {
		check = "accept-new"
	}
	return []string{"-F", "/dev/null", "-T", "-i", filepath.Join(dir, keyFile), "-p", strconv.Itoa(src.Port), "-l", src.User,
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "StrictHostKeyChecking=" + check,
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, knownHosts),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HashKnownHosts=no",
		"-o", "UpdateHostKeys=no",
		"-o", "CheckHostIP=no",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "LogLevel=ERROR",
		"--", src.Host, remote}
}

// errTooLong stops a source that sends more than asked for.
var errTooLong = errors.New("kaynak beklenenden uzun çıktı gönderdi")

// capped collects at most max bytes; past that the copy fails, ssh's output pipe
// closes and ssh dies of it.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.max {
		c.over = true
		return 0, errTooLong
	}
	return c.buf.Write(p)
}

// Output runs a script on the source and returns what it printed (at most max bytes).
// For short, read-only work: no systemd scope, the context bounds it.
func (s *Store) Output(ctx context.Context, src Source, max int, script string, args ...string) (string, error) {
	return s.output(ctx, src, false, max, script, args...)
}

func (s *Store) output(ctx context.Context, src Source, accept bool, max int, script string, args ...string) (string, error) {
	out := &capped{max: max}
	var errs lines
	_, err := s.Stream.Stream(ctx, run.Cmd{Name: "ssh", Args: s.sshArgs(src, accept, command(script, args...)),
		Stdout: out, Stderr: errs.add})
	switch {
	case out.over:
		return "", fmt.Errorf("%w (en çok %d KB)", errTooLong, max>>10)
	case err != nil:
		return out.buf.String(), Explain(err, errs.String())
	}
	return out.buf.String(), nil
}

// lines keeps the first stderr lines of a command (ssh's own errors come first).
type lines struct{ l []string }

func (l *lines) add(s string) {
	if len(l.l) < 20 {
		if len(s) > 300 {
			s = s[:300] + "…"
		}
		l.l = append(l.l, s)
	}
}

func (l *lines) String() string { return strings.Join(l.l, "\n") }

// Exit codes of the remote side that callers tell apart.
const (
	// ExitSSH is ssh's own failure (connection, authentication, host key).
	ExitSSH = 255
)

// ExitCode returns the exit status of a failed command, -1 if it did not exit.
func ExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ConnError is ssh failing to reach or log in to the source, explained.
type ConnError struct {
	Msg string
	Err error
}

func (e *ConnError) Error() string { return e.Msg }
func (e *ConnError) Unwrap() error { return e.Err }

// ErrHostKey means the source shows a different host key than the pinned one.
var ErrHostKey = errors.New("host key changed")

// Explain turns ssh's failures into what the operator can do about them. Errors of the
// remote command itself are returned as they are.
func Explain(err error, stderr string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &ConnError{Msg: "Kaynak sunucu zamanında cevap vermedi (zaman aşımı).", Err: err}
	}
	if ExitCode(err) != ExitSSH {
		return err
	}
	// ssh's own messages; a remote command's stderr may be mixed in, so the patterns
	// are ssh's exact wording.
	e := stderr
	switch {
	case strings.Contains(e, "REMOTE HOST IDENTIFICATION HAS CHANGED"), strings.Contains(e, "Host key verification failed"),
		strings.Contains(e, "host key for") && strings.Contains(e, "has changed"):
		return &ConnError{Msg: "Kaynak sunucunun anahtarı ilk bağlantıdakinden farklı (parmak izi tutmuyor). Sunucu yeniden kurulmadıysa " +
			"bu bir uyarıdır: bağlanma. Yeniden kurulduysa kaynak sayfasında “Parmak izini sıfırla”.", Err: ErrHostKey}
	case strings.Contains(e, "Permission denied (publickey"), strings.Contains(e, "Permission denied (gssapi"),
		strings.Contains(e, "Too many authentication failures"):
		return &ConnError{Msg: "Kaynak sunucu anahtarı kabul etmedi: satır authorized_keys'e eklendi mi, doğru kullanıcıya mı? " +
			"(from= kısıtı bu sunucunun adresini içermeli.)", Err: err}
	case strings.Contains(e, "port") && strings.Contains(e, "Connection refused"):
		return &ConnError{Msg: "Bağlantı reddedildi: port doğru mu, kaynakta SSH çalışıyor mu?", Err: err}
	case strings.Contains(e, "timed out"), strings.Contains(e, "No route to host"), strings.Contains(e, "Network is unreachable"):
		return &ConnError{Msg: "Kaynak sunucuya ulaşılamadı (zaman aşımı): adres ve port doğru mu, güvenlik duvarı bu sunucuya açık mı?", Err: err}
	case strings.Contains(e, "Could not resolve hostname"), strings.Contains(e, "Name or service not known"):
		return &ConnError{Msg: "Alan adı çözülemedi: adresi kontrol et.", Err: err}
	case strings.Contains(e, "Connection closed"), strings.Contains(e, "closed by remote host"), strings.Contains(e, "Connection reset"),
		strings.Contains(e, "Broken pipe"), strings.Contains(e, "Timeout, server"):
		return &ConnError{Msg: "Bağlantı koptu: " + firstLine(e), Err: err}
	}
	msg := firstLine(e)
	if msg == "" {
		msg = err.Error()
	}
	return &ConnError{Msg: "SSH bağlantısı başarısız: " + msg, Err: err}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// probe is the connection test: who and what the source is.
const probe = `os=$( . /etc/os-release 2>/dev/null && printf '%s' "$PRETTY_NAME" )
[ -n "$os" ] || os=$(cat /etc/redhat-release 2>/dev/null)
printf 'FOOTOPS_PROBE %s|%s|%s\n' "$(id -un)" "$os" "$(uname -r)"`

// Test connects to the source and runs a tiny probe. The first successful connection
// pins the source's host key; later ones must match it.
func (s *Store) Test(ctx context.Context, id string) (Source, error) {
	src, err := s.Get(id)
	if err != nil {
		return src, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.output(ctx, src, !src.Pinned(), 4096, probe)
	if err != nil {
		return src, err
	}
	_, info, ok := strings.Cut(out, "FOOTOPS_PROBE ")
	if !ok {
		return src, errors.New("kaynak beklenen cevabı vermedi (giriş kabuğu sh çalıştırabiliyor mu?)")
	}
	parts := strings.SplitN(firstLine(info), "|", 3)
	system := strings.TrimSpace(strings.Join(parts[1:], " · "))
	fps, err := s.Fingerprints(id)
	if err != nil || len(fps) == 0 {
		return src, errors.New("kaynağın sunucu anahtarı kaydedilemedi")
	}
	return s.update(id, func(src *Source) {
		src.HostKeys, src.Checked, src.System = fps, s.clock().UTC(), clip(system, 120)
	})
}

func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// scopeSeq tells scopes of the same job apart.
var scopeSeq atomic.Int64

// ScopeLimits bound a streaming ssh: the client needs little memory; decryption is
// cheap next to the network.
var ScopeLimits = run.Limits{MemoryMB: 128, CPUPercent: 50, PIDs: 16}

// StreamCmd runs a script on the source whose output is a stream (a database dump, a
// tar), in a systemd scope of its own so it is limited and measured. The caller sets
// Stdout and Stderr. Arguments are quoted; they are never secrets (they show in the
// process list).
func (s *Store) StreamCmd(src Source, job string, script string, args ...string) run.Cmd {
	unit := fmt.Sprintf("footops-ssh-%s-%d", job, scopeSeq.Add(1))
	full := []string{"--scope", "--quiet", "--collect", "--slice=system.slice", "--unit=" + unit,
		"-p", "MemoryMax=" + strconv.Itoa(ScopeLimits.MemoryMB) + "M", "-p", "MemorySwapMax=0",
		"-p", "CPUQuota=" + strconv.Itoa(ScopeLimits.CPUPercent) + "%", "-p", "TasksMax=" + strconv.Itoa(ScopeLimits.PIDs),
		"--", "nice", "-n", "10", "ssh"}
	full = append(full, s.sshArgs(src, false, command(script, args...))...)
	return run.Cmd{
		Name: "systemd-run", Args: full,
		Stop: func() {
			stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = s.Run.Run(stop, "systemctl", "kill", "--signal=SIGKILL", unit+".scope")
		},
		Cgroup: func() string { return run.CgroupRoot + "/system.slice/" + unit + ".scope" },
	}
}
