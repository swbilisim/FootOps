package remote

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// sh runs a script the way the source's login shell would: sh -c with the arguments.
func sh(t *testing.T, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-c", script, "footops"}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errb.String())
	}
	return out.String(), nil
}

func gnu(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"find", "tar"} {
		out, _ := exec.Command(tool, "--version").CombinedOutput()
		if !strings.Contains(string(out), "GNU") {
			t.Skipf("%s is not GNU's (the sources run GNU tools; CI checks this)", tool)
		}
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{"", "a b", "it's", `"$HOME" \n`, "'", "''x'", "; rm -rf /", "$(id)", "`id`", "ç ğ ı ö ş ü"} {
		out, err := sh(t, `printf '%s' "$1"`, s)
		if err != nil || out != s {
			t.Errorf("%q came back as %q (%v)", s, out, err)
		}
	}
}

// The remote login shell parses the command line once more: values must arrive intact.
func TestCommandLineArrivesIntact(t *testing.T) {
	args := []string{"/home/site a/www", "it's'", "$(touch /tmp/x)", "a\tb"}
	line := command(`for a in "$@"; do printf '[%s]' "$a"; done`, args...)
	out, err := exec.Command("sh", "-c", line).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "[/home/site a/www][it's'][$(touch /tmp/x)][a\tb]"
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestScriptsParse(t *testing.T) {
	shells := []string{"sh"}
	if _, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, "dash") // Ubuntu's /bin/sh
	}
	rm, _ := RemoveCommand(Source{PublicKey: "ssh-ed25519 AAAA footops-h1-abc123abc123"})
	for name, s := range map[string]string{"discover": DiscoverScript, "scan": ScanScript, "count": CountScript, "dump": DumpScript,
		"tar": TarScript, "probe": probe, "install": InstallCommand("restrict ssh-ed25519 AAAA x"), "remove": rm} {
		for _, shell := range shells {
			if out, err := exec.Command(shell, "-n", "-c", s).CombinedOutput(); err != nil {
				t.Errorf("%s does not parse in %s: %v\n%s", name, shell, err, out)
			}
		}
	}
}

func TestValidate(t *testing.T) {
	if p := Validate("B — eski", "203.0.113.10", 22, "root"); len(p) != 0 {
		t.Errorf("valid source refused: %v", p)
	}
	for _, c := range []struct {
		label, host string
		port        int
		user        string
	}{
		{"", "203.0.113.10", 22, "root"},
		{"x", "-oProxyCommand=id", 22, "root"},
		{"x", "a b", 22, "root"},
		{"x", "0.0.0.0", 22, "root"},
		{"x", "host", 0, "root"},
		{"x", "host", 22, "-l"},
		{"x", "host", 22, "ro ot"},
		{strings.Repeat("x", 41), "host", 22, "root"},
	} {
		if p := Validate(c.label, c.host, c.port, c.user); len(p) == 0 {
			t.Errorf("accepted %+v", c)
		}
	}
	for _, h := range []string{"203.0.113.10", "2001:db8::10", "eski.example.com", "b-server"} {
		if !ValidHost(h) {
			t.Errorf("refused %s", h)
		}
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen yok")
	}
	return &Store{Dir: filepath.Join(t.TempDir(), "sources"), Run: run.Exec{}, Name: "H1 test"}
}

func TestStore(t *testing.T) {
	s := newStore(t)
	src, err := s.Create(context.Background(), " B eski ", "203.0.113.10", 2222, "root", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !reID.MatchString(src.ID) || src.Label != "B eski" || src.Pinned() {
		t.Errorf("src = %+v", src)
	}
	f := strings.Fields(src.PublicKey)
	if len(f) != 3 || f[0] != "ssh-ed25519" || f[2] != "footops-h1-test-"+src.ID {
		t.Errorf("public key = %q", src.PublicKey)
	}
	for name, mode := range map[string]os.FileMode{"": 0o700, keyFile: 0o600, knownHosts: 0o600, sourceFile: 0o600} {
		st, err := os.Stat(filepath.Join(s.dir(src.ID), name))
		if err != nil || st.Mode().Perm() != mode {
			t.Errorf("%s: %v %v", name, st.Mode(), err)
		}
	}
	line := AuthorizedLine(src, []string{"198.51.100.7", "2001:db8::7", "not-an-ip"})
	if line != `restrict,from="198.51.100.7,2001:db8::7" `+src.PublicKey {
		t.Errorf("line = %s", line)
	}
	if AuthorizedLine(src, nil) != "restrict "+src.PublicKey {
		t.Error("no addresses: no from=")
	}
	list, err := s.List()
	if err != nil || len(list) != 1 || list[0].ID != src.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	if _, err := s.Create(context.Background(), "x", "-oProxyCommand=id", 22, "root", ""); err == nil {
		t.Error("bad host accepted")
	}
	if _, err := s.Get("../../etc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get traversal: %v", err)
	}
	if err := s.Remove(src.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.dir(src.ID)); !os.IsNotExist(err) {
		t.Error("key left behind")
	}
}

func TestSSHArgs(t *testing.T) {
	s := &Store{Dir: "/var/lib/footops/sources"}
	src := Source{ID: "abcdefabcdef", Host: "203.0.113.10", Port: 2222, User: "root"}
	args := s.sshArgs(src, false, "true")
	joined := strings.Join(args, " ")
	for _, want := range []string{"-F /dev/null", "-T", "-i /var/lib/footops/sources/abcdefabcdef/id_ed25519", "-p 2222", "-l root",
		"BatchMode=yes", "IdentitiesOnly=yes", "IdentityAgent=none", "StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/var/lib/footops/sources/abcdefabcdef/known_hosts", "GlobalKnownHostsFile=/dev/null",
		"ForwardAgent=no", "ClearAllForwardings=yes", "PermitLocalCommand=no", "ControlPath=none", "UpdateHostKeys=no"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ssh args lack %q: %s", want, joined)
		}
	}
	if args[len(args)-3] != "--" || args[len(args)-2] != src.Host || args[len(args)-1] != "true" {
		t.Errorf("host must follow --: %v", args[len(args)-3:])
	}
	if !strings.Contains(strings.Join(s.sshArgs(src, true, "true"), " "), "StrictHostKeyChecking=accept-new") {
		t.Error("first connection must pin the key")
	}
	c := s.StreamCmd(src, "job1", "true")
	if c.Name != "systemd-run" || !slices.Contains(c.Args, "--scope") || !slices.Contains(c.Args, "MemoryMax=128M") ||
		!slices.Contains(c.Args, "ssh") || c.Cgroup() == "" {
		t.Errorf("stream cmd = %v", c.Args)
	}
}

// fakeSSH plays ssh: it records the command, may pin a host key, and prints out.
type fakeSSH struct {
	dir   string
	out   string
	err   error
	lines []string
	seen  []run.Cmd
	pin   string // known_hosts line written on accept-new
}

func (f *fakeSSH) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	f.seen = append(f.seen, c)
	args := strings.Join(c.Args, " ")
	if strings.Contains(args, "StrictHostKeyChecking=accept-new") && f.pin != "" {
		for i, a := range c.Args {
			if v, ok := strings.CutPrefix(a, "UserKnownHostsFile="); ok && i > 0 {
				_ = os.WriteFile(v, []byte(f.pin+"\n"), 0o600)
			}
		}
	}
	for _, l := range f.lines {
		c.Stderr(l)
	}
	if _, err := io.WriteString(c.Stdout, f.out); err != nil {
		return run.Usage{}, err
	}
	return run.Usage{}, f.err
}

// exitErr makes a real *exec.ExitError with the given code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return fmt.Errorf("ssh: %w: x", err)
}

func TestConnectionTest(t *testing.T) {
	s := newStore(t)
	src, err := s.Create(context.Background(), "B", "203.0.113.10", 22, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := os.ReadFile(filepath.Join(s.dir(src.ID), keyFile+".pub")) // any valid key will do as a host key
	hostKey := strings.Join(strings.Fields(string(pub))[:2], " ")
	fs := &fakeSSH{out: "FOOTOPS_PROBE root|CentOS Linux 7 (Core)|3.10.0\n", pin: "203.0.113.10 " + hostKey}
	s.Stream = fs
	got, err := s.Test(context.Background(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := exec.Command("ssh-keygen", "-l", "-f", filepath.Join(s.dir(src.ID), knownHosts)).Output()
	fp := strings.Fields(string(want))[1]
	if len(got.HostKeys) != 1 || got.HostKeys[0] != "ED25519 "+fp || got.System != "CentOS Linux 7 (Core) · 3.10.0" || got.Checked.IsZero() {
		t.Errorf("after test: %+v (ssh-keygen says %s)", got, fp)
	}
	if !strings.Contains(strings.Join(fs.seen[0].Args, " "), "accept-new") {
		t.Error("first test must pin")
	}
	if _, err := s.Test(context.Background(), src.ID); err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(fs.seen[1].Args, " "); strings.Contains(a, "accept-new") || !strings.Contains(a, "StrictHostKeyChecking=yes") {
		t.Errorf("later tests must check the pinned key: %s", a)
	}

	// A changed key is refused and explained.
	fs.err, fs.lines = exitErr(t, 255), []string{"@@@@@@@@@@@", "@    WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!     @", "Host key verification failed."}
	_, err = s.Test(context.Background(), src.ID)
	var ce *ConnError
	if !errors.As(err, &ce) || !errors.Is(err, ErrHostKey) || !strings.Contains(err.Error(), "Parmak izini sıfırla") {
		t.Errorf("changed key: %v", err)
	}
	reset, err := s.ResetHostKey(src.ID)
	if err != nil || reset.Pinned() {
		t.Errorf("reset: %+v %v", reset, err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dir(src.ID), knownHosts)); len(b) != 0 {
		t.Error("known_hosts not emptied")
	}
}

func TestExplain(t *testing.T) {
	e255 := exitErr(t, 255)
	for stderr, want := range map[string]string{
		"root@203.0.113.10: Permission denied (publickey,password).":   "anahtarı kabul etmedi",
		"ssh: connect to host x port 22: Connection refused":           "reddedildi",
		"ssh: connect to host x port 22: Connection timed out":         "ulaşılamadı",
		"ssh: Could not resolve hostname x: Name or service not known": "çözülemedi",
		"Connection closed by 203.0.113.10 port 22":                    "koptu",
		"something else": "something else",
	} {
		err := Explain(e255, stderr)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q → %v", stderr, err)
		}
	}
	plain := exitErr(t, 1)
	if Explain(plain, "Permission denied") != plain {
		t.Error("a remote command's own failure must pass through")
	}
	if Explain(context.Canceled, "") != context.Canceled {
		t.Error("cancellation must pass through")
	}
}

func TestStreamError(t *testing.T) {
	logOf := func(lines ...string) *StreamLog {
		l := &StreamLog{}
		for _, x := range lines {
			l.Add(x)
		}
		return l
	}
	// While the site runs there, files changing or vanishing are a note, not a failure.
	for _, code := range []int{1, 2} {
		if err := StreamError(exitErr(t, code), logOf("tar: ./a.jpg: file changed as we read it", "tar: ./b: Cannot stat: No such file or directory"), true, true); err != nil {
			t.Errorf("live, exit %d: %v", code, err)
		}
	}
	// Exit 1 without tar saying why is not "a file changed": tar may never have run.
	for _, l := range []*StreamLog{logOf(), logOf("ionice: ioprio_set failed: Operation not permitted"),
		logOf("tar: ./a.jpg: file changed as we read it", "tar: ./c: Cannot open: Permission denied")} {
		if err := StreamError(exitErr(t, 1), l, true, true); err == nil {
			t.Errorf("accepted exit 1 with %q", l.String())
		}
	}
	if err := StreamError(exitErr(t, 1), logOf("tar: ./a.jpg: file changed as we read it"), true, false); err == nil || !strings.Contains(err.Error(), "okunurken değişti") {
		t.Errorf("not live: %v", err)
	}
	// find could not read a folder: an error even though tar ended well.
	if err := StreamError(nil, logOf("find: ‘./gizli’: Permission denied"), true, true); err == nil || !strings.Contains(err.Error(), "okunamadı") {
		t.Errorf("find: %v", err)
	}
	for code, want := range map[int]string{3: "klasör yok", 66: "GNU", 255: "SSH", 2: "exit status 2"} {
		if err := StreamError(exitErr(t, code), logOf("x"), true, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("exit %d → %v", code, err)
		}
	}
	if err := StreamError(exitErr(t, 69), logOf(), false, false); err == nil || !strings.Contains(err.Error(), "mongodump yok") {
		t.Errorf("no mongodump → %v", err)
	}
	// The message names the last line (mongodump's progress lines come first).
	if err := StreamError(exitErr(t, 1), logOf("[####] eski.urunler 10/20", "Failed: error writing data"), false, false); err == nil || !strings.HasSuffix(err.Error(), "Failed: error writing data") {
		t.Errorf("message: %v", err)
	}
	// Whatever the source sends, the log keeps little.
	big := &StreamLog{}
	for i := 0; i < 10000; i++ {
		big.Add(strings.Repeat("x", 64<<10))
	}
	if len(big.String()) > 8*310 {
		t.Errorf("log grew to %d bytes", len(big.String()))
	}
}

func TestOutputCap(t *testing.T) {
	s := &Store{Dir: t.TempDir(), Stream: &fakeSSH{out: strings.Repeat("x", 5000)}}
	if _, err := s.Output(context.Background(), Source{ID: "abcdefabcdef", Host: "h", Port: 22, User: "root"}, 4096, "true"); !errors.Is(err, errTooLong) {
		t.Errorf("err = %v", err)
	}
}

func TestInstallAndRemoveCommands(t *testing.T) {
	home := t.TempDir()
	src := Source{PublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample+/= footops-h1-abcdefabcdef"}
	line := AuthorizedLine(src, []string{"198.51.100.7"})
	run := func(cmd string) {
		t.Helper()
		c := exec.Command("sh", "-c", cmd)
		c.Env = append(os.Environ(), "HOME="+home)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	keys := filepath.Join(home, ".ssh", "authorized_keys")
	run(InstallCommand(line))
	run(InstallCommand(line)) // pasted twice: one line
	b, _ := os.ReadFile(keys)
	if string(b) != line+"\n" {
		t.Errorf("authorized_keys = %q", b)
	}
	if st, _ := os.Stat(keys); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	// The operator's last key has no newline: ours goes on a line of its own.
	mine := "ssh-ed25519 AAAAoperator user@laptop"
	os.WriteFile(keys, []byte(mine), 0o600)
	run(InstallCommand(line))
	if b, _ := os.ReadFile(keys); string(b) != mine+"\n"+line+"\n" {
		t.Errorf("after install on a file without newline: %q", b)
	}
	// Removal takes exactly our lines (options changed, CRLF), never a line that only
	// mentions our key (glued to the operator's by an older install), and leaves the rest.
	glued := mine + "restrict " + src.PublicKey
	other := mine + "\nssh-ed25519 AAAAx footops-h1-abcdefabcdefX\n" + glued + "\n"
	os.WriteFile(keys, []byte(other+line+"\r\n"+`from="203.0.113.9" `+src.PublicKey+"\n"), 0o644)
	rm, err := RemoveCommand(src)
	if err != nil {
		t.Fatal(err)
	}
	run(rm)
	if b, _ := os.ReadFile(keys); string(b) != other {
		t.Errorf("after removal: %q", b)
	}
	if st, _ := os.Stat(keys); st.Mode().Perm() != 0o600 {
		t.Errorf("mode after removal: %v", st.Mode())
	}
	before, _ := os.Stat(keys)
	run(rm) // nothing left to remove: the file is not touched
	if after, _ := os.Stat(keys); !os.SameFile(before, after) {
		t.Error("rewritten although nothing was ours")
	}
	// Only our line, alone in the file: removed too.
	os.WriteFile(keys, []byte(line+"\n"), 0o600)
	run(rm)
	if b, _ := os.ReadFile(keys); len(b) != 0 {
		t.Errorf("left: %q", b)
	}
	if _, err := RemoveCommand(Source{PublicKey: "ssh-ed25519 AAAA someone'else"}); err == nil {
		t.Error("a foreign comment must be refused")
	}
	if _, err := RemoveCommand(Source{PublicKey: "ssh-ed25519 AA'AA footops-h1-x"}); err == nil {
		t.Error("a key that is not base64 must be refused")
	}
}

// mkTree builds a folder like an app's uploads: files, a sub-folder, a hidden file and
// folder, a link and a FIFO (the last three are not copied).
func mkTree(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "up loads")
	for name, body := range map[string]string{"a.jpg": "aaaa", "blog/b.png": "bb", "blog/2026/c ç.pdf": "c", ".env": "SECRET=1",
		".cache/x": "xxxxxxxx", "__MACOSX/y": "y"} {
		p := filepath.Join(d, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(d, "a.jpg"), filepath.Join(d, "blog", "a-again.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(d, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(d, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCountAndTarScripts(t *testing.T) {
	gnu(t)
	d := mkTree(t)
	out, err := sh(t, CountScript, d)
	if err != nil {
		t.Fatal(err)
	}
	_, line, _ := strings.Cut(out, "FOOTOPS_COUNT ")
	c, ok := parseCounts(strings.Fields(line))
	// 4 files (one a hard link), 11 bytes; folders blog, blog/2026, empty; hidden .env .cache
	// __MACOSX; a link; a FIFO; find ended well.
	if want := (Counts{Files: 4, Bytes: 11, Dirs: 3, Hidden: 3, Links: 1, Other: 1}); !ok || c != want {
		t.Errorf("count = %+v, want %+v (%q)", c, want, out)
	}
	if os.Getuid() != 0 { // root reads everything
		locked := filepath.Join(d, "kilitli")
		os.Mkdir(locked, 0o755)
		os.WriteFile(filepath.Join(locked, "x"), []byte("x"), 0o644)
		os.Chmod(locked, 0)
		out, _ := sh(t, CountScript, d)
		_, line, _ := strings.Cut(out, "FOOTOPS_COUNT ")
		if c, ok := parseCounts(strings.Fields(line)); !ok || c.Err == 0 {
			t.Errorf("an unreadable folder is not reported: %q", out)
		}
		os.Chmod(locked, 0o755)
		os.RemoveAll(locked)
	}

	cmd := exec.Command("sh", "-c", TarScript, "footops", d)
	var tarOut bytes.Buffer
	cmd.Stdout = &tarOut
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&tarOut)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			t.Errorf("%s: type %c in the stream", h.Name, h.Typeflag)
		}
		names = append(names, h.Name)
	}
	slices.Sort(names)
	want := []string{"./a.jpg", "./blog/", "./blog/2026/", "./blog/2026/c ç.pdf", "./blog/a-again.jpg", "./blog/b.png", "./empty/"}
	if !slices.Equal(names, want) {
		t.Errorf("tar members = %q, want %q", names, want)
	}
	// The stream ends with the end line after the archive's padding.
	rest, _ := io.ReadAll(&tarOut)
	if got := strings.TrimLeft(string(rest), "\x00"); got != TarEnd+" 0\n" {
		t.Errorf("after the archive: %q", got)
	}
	if err := exec.Command("sh", "-c", TarScript, "footops", d+"-yok").Run(); ExitCode(err) != ExitNoDir {
		t.Errorf("missing folder: %v", err)
	}
}

func TestScanScript(t *testing.T) {
	gnu(t)
	d := t.TempDir()
	files := map[string]string{
		"package.json": "{\n  \"name\": \"tasima\",\n  \"main\": \"app.js\",\n  \"engines\": {\n    \"node\": \">=14\"\n  },\n" +
			"  \"dependencies\": {\n    \"express\": \"^4.17.1\",\n    \"mongoose\": \"^5.9.6\"\n  },\n  \"scripts\": {\n    \"start\": \"PASS=hunter2 node app.js\"\n  }\n}\n",
		".env": "DB_URI=mongodb://localhost:27017/tasimaci\nexport SECRET_KEY = s3cr3tvalue\n# note\n" +
			"PRIVATE_KEY=\"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC=\nAbCdEf123=\n-----END PRIVATE KEY-----\"\nAFTER=1\n",
		"lib/mailConf.js":      "module.exports = { host: 'smtp.example.com', auth: { user: 'x', pass: 'hunter22' } }\n",
		"lib/db.js":            "mongoose.connect('mongodb://admin:Sup3rS3cret@10.0.0.5:27017/magaza?authSource=admin')\n",
		"public/build/a.js":    "fetch(\"http://203.0.113.20:8196/graphql\");fetch(\"http://localhost:4000/x\")\n",
		"public/uploads/1.jpg": "\x00\x01binary",
	}
	for name, body := range files {
		p := filepath.Join(d, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	out, err := sh(t, ScanScript, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "Sup3rS3cret", "s3cr3tvalue", "admin:", "MIIEvQ", "AbCdEf"} {
		if strings.Contains(out, secret) {
			t.Errorf("a secret left the source: %q in\n%s", secret, out)
		}
	}
	sc := ParseScan(out, time.Now())
	if !sc.Complete || sc.Err != "" {
		t.Fatalf("scan = %+v\n%s", sc, out)
	}
	if sc.Package == nil || sc.Package.Name != "tasima" || sc.Package.Main != "app.js" || sc.Package.Node != ">=14" ||
		!slices.Equal(sc.Package.Known(), []string{"express", "mongoose"}) {
		t.Errorf("package = %+v", sc.Package)
	}
	if len(sc.Env) != 1 || !slices.Equal(sc.Env[0].Keys, []string{"DB_URI", "SECRET_KEY", "PRIVATE_KEY", "AFTER"}) {
		t.Errorf("env = %+v (a line of a quoted value must not pass for a name)", sc.Env)
	}
	kinds := map[string]string{}
	for _, f := range sc.Secrets {
		kinds[f.Kind] = f.Where
	}
	if kinds["mongo-uri"] != "lib/db.js:1" || kinds["password"] != "lib/mailConf.js:1" || kinds["smtp"] != "lib/mailConf.js:1" {
		t.Errorf("secrets = %+v", sc.Secrets)
	}
	refs := map[string]bool{}
	for _, r := range sc.DBRefs {
		refs[r.Value] = true
	}
	if !refs["tasimaci"] || !refs["magaza"] {
		t.Errorf("db refs = %+v", sc.DBRefs)
	}
	urls := map[string]bool{}
	for _, u := range sc.URLs {
		urls[u.Value] = true
	}
	if !urls["http://203.0.113.20:8196"] || !urls["http://localhost:4000"] {
		t.Errorf("urls = %+v", sc.URLs)
	}
	if sc.Total.Files != 5 || sc.Total.Hidden != 1 {
		t.Errorf("total = %+v", sc.Total)
	}
}

func TestDiscoverScriptOnThisMachine(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("reads /proc")
	}
	gnu(t)
	// A "node" app run by PM2: a copy of sleep named node, in an app folder with uploads.
	app := t.TempDir()
	os.MkdirAll(filepath.Join(app, "public", "uploads"), 0o755)
	os.WriteFile(filepath.Join(app, "public", "uploads", "a.jpg"), []byte("jpeg"), 0o644)
	bin := filepath.Join(t.TempDir(), "node")
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip(err)
	}
	b, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	os.WriteFile(bin, b, 0o755)
	node := exec.Command(bin, "300")
	node.Dir = app
	node.Env = append(os.Environ(), "name=magaza", "pm_id=3", "PORT=3000", "SECRET=gizli-deger")
	if err := node.Start(); err != nil {
		t.Skip(err)
	}
	defer node.Process.Kill()
	out, err := sh(t, DiscoverScript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "gizli-deger") {
		t.Error("an environment value left the source")
	}
	d := ParseDiscovery(out, time.Now())
	if !d.Complete || d.System.Kernel == "" || d.System.CPUs == 0 || d.System.MemTotal == 0 || len(d.System.Disks) == 0 {
		t.Errorf("discovery = %+v", d.System)
	}
	var p *Proc
	for i := range d.Procs {
		if d.Procs[i].PID == node.Process.Pid {
			p = &d.Procs[i]
		}
	}
	if p == nil || p.Cwd != app || p.PM2Name != "magaza" || p.PM2ID != "3" || p.Port != "3000" {
		t.Fatalf("node process not found: %+v in %+v", p, d.Procs)
	}
	found := false
	for _, c := range d.Candidates {
		found = found || (c.Dir == filepath.Join(app, "public", "uploads") && c.Files == 1)
	}
	if !found {
		t.Errorf("uploads folder not a candidate: %+v", d.Candidates)
	}
}

// discoveryFixture is what the script prints on an old CentOS 7 server with PM2, a
// MongoDB 4.0 without authentication and nginx (values made up).
var discoveryFixture = strings.Join([]string{
	"os\tCentOS Linux 7 (Core)\t3.10.0-1160.el7.x86_64\tx86_64\teski\troot",
	"cpu\t4",
	"mem\t8008596\t2051234",
	"disk\t/\t209612800\t102400000\t107212800",
	"tool\tmongodump\t/usr/bin/mongodump\tmongodump version: r4.0.28",
	"tool\tnginx\t/usr/sbin/nginx\tnginx version: nginx/1.20.1",
	"msrc\thost\t-\tmongo",
	"FOOTOPS_MVER\t4.0.28",
	"FOOTOPS_MDB\tadmin\t1\t2\t300\t40960",
	"FOOTOPS_MDB\ttasimaci\t29\t354\t1200000\t2500000",
	"FOOTOPS_MDB\tmagaza\t12\t800\t1400000\t2600000",
	"proc\t1001\t1\troot\t52000\t/root/.nvm/versions/node/v14.21.3/bin/node\t/root\tPM2 v4.5.6: God Daemon (/root/.pm2)\t",
	"proc\t2001\t1001\troot\t180000\t/root/.nvm/versions/node/v14.21.3/bin/node\t/home/magaza.example/www\tnode /home/magaza.example/www/app.js\tname=magaza|pm_id=3|PM2_HOME=/root/.pm2|PORT=8196|",
	"proc\t2002\t1\tsite\t90000\t/usr/bin/node\t/home/other/www\tnode server.js\tname=foo|",
	"listen\tLISTEN 0 511 *:8196 *:* users:((\"node\",pid=2001,fd=20))",
	"listen\tLISTEN 0 128 127.0.0.1:27017 *:* users:((\"mongod\",1500,11))",
	"listen\tbroken",
	"ngx\t# configuration file /etc/nginx/conf.d/magaza.conf:",
	"ngx\tupstream enis { server 127.0.0.1:8196; }",
	"ngx\tserver {",
	"ngx\t    listen 80;",
	"ngx\t    server_name magaza.example www.magaza.example;",
	"ngx\t    location / { proxy_pass http://enis; }",
	"ngx\t    location /uploads/ { alias /home/magaza.example/www/public/uploads/; }",
	"ngx\t}",
	"cand\t/home/magaza.example/www/public/uploads\t512\t230000000\t14\t1\t0\t0\t0",
	"cand\t/home/magaza.example/www/data\t2\t100\t0\t0\t0\t0\t0",
	"cand\trelative/x\t1\t1\t0\t0\t0\t0\t0",
	"app\t/home/magaza.example/www\t480000",
	"end",
}, "\n")

func TestParseDiscoveryAndSuggest(t *testing.T) {
	d := ParseDiscovery(discoveryFixture, time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC))
	if !d.Complete || d.System.OS != "CentOS Linux 7 (Core)" || d.System.CPUs != 4 || d.System.MemTotal != 8008596<<10 ||
		len(d.System.Disks) != 1 || len(d.Tools) != 2 {
		t.Errorf("system = %+v tools = %+v", d.System, d.Tools)
	}
	if len(d.Mongo) != 1 || d.Mongo[0].Via != "host" || d.Mongo[0].Container != "" || d.Mongo[0].Version != "4.0.28" || len(d.Mongo[0].DBs) != 3 {
		t.Fatalf("mongo = %+v", d.Mongo)
	}
	if len(d.Procs) != 3 || !d.Procs[0].Daemon || d.Procs[0].PM2Home != "/root/.pm2" || d.Procs[1].PM2Name != "magaza" ||
		d.Procs[1].PM2ID != "3" || !slices.Equal(d.Procs[1].Ports, []int{8196}) || d.Procs[2].PM2Name != "" {
		t.Errorf("procs = %+v", d.Procs)
	}
	if len(d.Listen) != 2 || d.Listen[1].PID != 1500 || d.Listen[1].Process != "mongod" {
		t.Errorf("listen = %+v", d.Listen)
	}
	if len(d.Web) != 1 || len(d.Candidates) != 2 || len(d.Apps) != 1 {
		t.Errorf("web = %+v cands = %+v", d.Web, d.Candidates)
	}
	s := Suggest(d, nil, "magaza", []string{"magaza.example"})
	if len(s.Web) != 1 || !slices.Equal(s.Ports, []int{8196}) || len(s.Procs) != 1 || s.AppDir != "/home/magaza.example/www" ||
		s.FilesDir != "/home/magaza.example/www/public/uploads" || s.DB == nil || s.DB.Name != "magaza" {
		t.Errorf("suggestion = %+v db=%+v", s, s.DB)
	}
	// The code names the database: that wins over a similar name.
	sc := &Scan{DBRefs: []Ref{{Value: "tasimaci", Count: 1}}}
	if s := Suggest(d, sc, "tasima", []string{"tasimaci-ornek.example"}); s.DB == nil || s.DB.Name != "tasimaci" ||
		s.DB.Why != "uygulamanın kodunda geçiyor" || len(s.Web) != 0 {
		t.Errorf("by reference: %+v", s.DB)
	}
	// Nothing alike: no guess.
	if s := Suggest(d, nil, "belediye", []string{"belediye.example"}); s.DB != nil {
		t.Errorf("guessed %+v", s.DB)
	}
}

func TestParseNginx(t *testing.T) {
	conf := `# configuration file /etc/nginx/nginx.conf:
events { worker_connections 1024; }
http {
    include /etc/nginx/conf.d/*.conf;
    server { listen 80 default_server; server_name _; root /usr/share/nginx/html; }
}
stream { server { listen 3306; proxy_pass db; } }
# configuration file /etc/nginx/conf.d/a.conf:
upstream app_a { server 127.0.0.1:3000 weight=1; server localhost:3001; server 10.0.0.9:3000; }
server {
    listen 443 ssl http2;
    server_name "a.example.com" www.a.example.com; # comment ; { }
    location / {
        proxy_pass http://app_a;
        location /api/ { proxy_pass http://user:secret@127.0.0.1:4000/api/; }
    }
    location ~* \.(jpg|png)$ { root /var/www/a/public; }
    location /files { alias /data/a/files/; }
    return 301 https://a.example.com/#top;
}
}
server { server_name .b.example.com *.c.example.com; location / { proxy_pass http://[::1]:5000; } }
`
	servers, ups := ParseNginx(conf)
	if len(servers) != 3 {
		t.Fatalf("servers = %+v", servers)
	}
	a := servers[1]
	if a.File != "/etc/nginx/conf.d/a.conf" || !slices.Equal(a.Names, []string{"a.example.com", "www.a.example.com"}) ||
		!slices.Equal(a.Listen, []string{"443 ssl http2"}) || len(a.Locations) != 4 {
		t.Errorf("a = %+v", a)
	}
	if a.Locations[1].Proxy != "http://***@127.0.0.1:4000/api/" {
		t.Errorf("credentials shown: %s", a.Locations[1].Proxy)
	}
	if a.Locations[3].Alias != "/data/a/files/" || a.Locations[2].Root != "/var/www/a/public" {
		t.Errorf("locations = %+v", a.Locations)
	}
	if got := ProxyPorts("http://app_a", ups); !slices.Equal(got, []int{3000, 3001}) {
		t.Errorf("upstream ports = %v (10.0.0.9 is not this host)", got)
	}
	if got := ProxyPorts("http://[::1]:5000", ups); !slices.Equal(got, []int{5000}) {
		t.Errorf("ipv6 port = %v", got)
	}
	b := servers[2]
	for dom, want := range map[string]bool{"b.example.com": true, "x.b.example.com": true, "x.c.example.com": true, "c.example.com": false, "a.example.com": false} {
		if b.Matches(dom) != want {
			t.Errorf("%s matches = %v", dom, !want)
		}
	}
}

func FuzzParseNginx(f *testing.F) {
	f.Add("server { server_name a; location / { proxy_pass http://x; } }")
	f.Add("http { server { listen 80; } } }}} {{{ ; ; 'unterminated")
	f.Add(strings.Repeat("a {", 100))
	f.Fuzz(func(t *testing.T, s string) {
		servers, ups := ParseNginx(s)
		if len(servers) > maxServers || len(ups) > 200 {
			t.Fatal("limits not kept")
		}
		for _, w := range servers {
			if len(w.Locations) > maxLocations {
				t.Fatal("too many locations")
			}
		}
	})
}

func FuzzParseDiscovery(f *testing.F) {
	f.Add(discoveryFixture)
	f.Fuzz(func(t *testing.T, s string) {
		d := ParseDiscovery(s, time.Time{})
		_ = Suggest(d, nil, "x", []string{"x.com"})
	})
}
