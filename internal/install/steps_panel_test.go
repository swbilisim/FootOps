package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/mongo"
)

func TestTunnelSecret(t *testing.T) {
	h, _ := newTestHost(t, nil)
	if done, _ := (tunnelSecretStep{h, ""}).Check(ctx); done {
		t.Fatal("done without token file")
	}
	if err := (tunnelSecretStep{h, ""}).Apply(ctx); err == nil {
		t.Fatal("applied without a token")
	}
	s := tunnelSecretStep{h, testTunnelToken}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(h.Path(core.TunnelSecretPath))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", st.Mode().Perm())
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
	if done, _ := (tunnelSecretStep{h, ""}).Check(ctx); !done {
		t.Error("resumed run (no token in memory) should accept the stored token")
	}
	if done, _ := (tunnelSecretStep{h, "another-token"}).Check(ctx); done {
		t.Error("changed token not detected")
	}
}

// ownerFake simulates chown/chmod/stat for panelConfigStep.
func ownerFake() func(cmd string) (string, error) {
	owner := map[string]string{}
	mode := map[string]string{}
	return func(cmd string) (string, error) {
		f := strings.Fields(cmd)
		switch f[0] {
		case "chown":
			owner[f[2]] = f[1]
		case "chmod":
			mode[f[2]] = f[1]
		case "stat":
			p := f[len(f)-1]
			if owner[p] == "" {
				return "", errExit
			}
			return owner[p] + " " + mode[p] + "\n", nil
		}
		return "", nil
	}
}

func TestPanelConfig(t *testing.T) {
	h, _ := newTestHost(t, ownerFake())
	cfg := config.Panel{ServerCode: "H1", PanelHost: "panel-h1.example.com", ListenTunnel: config.DefaultTunnel, ListenLocal: config.DefaultLocal}
	s := panelConfigStep{h, cfg}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if done, err := s.Check(ctx); !done || err != nil {
		t.Fatalf("not done after apply: %v", err)
	}
	if !strings.Contains(strings.Join(h.Run.(*fakeRunner).cmds(), "\n"), "systemctl try-restart footops-web") {
		t.Error("running web process not restarted after a config change")
	}
	loaded, err := config.Load(h.Path(config.Path))
	if err != nil || loaded != cfg {
		t.Errorf("written config = %+v, %v", loaded, err)
	}
	bad := cfg
	bad.ListenLocal = "0.0.0.0:7701"
	if err := (panelConfigStep{h, bad}).Apply(ctx); err == nil {
		t.Error("public listen address accepted")
	}
}

func TestPanelAccount(t *testing.T) {
	h, f := newTestHost(t, nil)
	os.MkdirAll(h.Path(config.WebStateDir), 0o700) // created by panel-config
	notes := &Notes{}
	var shown string
	setup := func(_ context.Context, secret, uri string) error {
		shown = secret
		if !strings.Contains(uri, secret) {
			t.Error("QR URI does not carry the secret")
		}
		return nil
	}
	s := panelAccountStep{h, "furkan", "FootOps H1", notes, setup}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	acct, err := auth.NewStore(h.Path(config.AuthPath)).Load()
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "furkan" || acct.TOTPSecret != shown || len(acct.RecoveryCodes) != 10 {
		t.Errorf("account = %+v", acct)
	}
	// Handed over through the file's descriptor, not by a chown of its path.
	if f.ran("chown") || owner(t, h.Path(config.AuthPath)) != owner(t, h.Path(config.WebStateDir)) {
		t.Errorf("account file not handed to the directory's owner: %q", f.cmds())
	}
	lines := notes.Lines()
	if len(lines) != 2 {
		t.Fatalf("notes = %v", lines)
	}
	pw := strings.Fields(lines[0])[len(strings.Fields(lines[0]))-1]
	if ok, _ := auth.VerifyPassword(acct.PasswordHash, pw); !ok {
		t.Error("reported password does not match the stored hash")
	}
	if strings.Count(lines[1], "-") < 10 {
		t.Errorf("recovery codes not reported: %s", lines[1])
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func owner(t *testing.T, p string) string {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
}

// resetHost has a running footops-web and an account; it keeps the account as the stop
// and the start of footops-web find it. Before it exits, the web finishes a login it had
// in flight, with a recovery code: it writes back the account as it read it before the
// reset, less that code. stopErr makes the stop fail.
func resetHost(t *testing.T, stopErr error) (Host, *fakeRunner, map[string]auth.Account, auth.Account) {
	t.Helper()
	var root string
	seen := map[string]auth.Account{}
	var old auth.Account
	h, f := newTestHost(t, func(cmd string) (string, error) {
		store := auth.NewStore(filepath.Join(root, config.AuthPath))
		switch cmd {
		case "systemctl is-active footops-web":
			return "active\n", nil
		case "systemctl stop footops-web", "systemctl start footops-web":
			if cmd == "systemctl stop footops-web" && stopErr != nil {
				return "", stopErr
			}
			a, err := store.Load()
			if err != nil {
				return "", err
			}
			seen[cmd] = a
			if cmd == "systemctl stop footops-web" {
				inFlight := old
				inFlight.RecoveryCodes = old.RecoveryCodes[1:]
				return "", store.Save(inFlight)
			}
		}
		return "", nil
	})
	root = h.Root
	os.MkdirAll(h.Path(config.WebStateDir), 0o700)
	secret, _ := auth.NewTOTPSecret()
	hash, _ := auth.HashPassword("old-password-123")
	_, hashes, _ := auth.NewRecoveryCodes(3)
	old = auth.Account{Username: "furkan", PasswordHash: hash, TOTPSecret: secret, RecoveryCodes: hashes}
	if err := auth.NewStore(h.Path(config.AuthPath)).Save(old); err != nil {
		t.Fatal(err)
	}
	return h, f, seen, old
}

// stopThenStart checks that the panel was stopped before the new account was saved and
// started after it (the sessions the old credentials opened are gone), and returns the
// account the panel starts with.
func stopThenStart(t *testing.T, f *fakeRunner, seen map[string]auth.Account, old auth.Account) auth.Account {
	t.Helper()
	cmds := strings.Join(f.cmds(), "\n")
	stop, start := strings.Index(cmds, "systemctl stop footops-web"), strings.Index(cmds, "systemctl start footops-web")
	if stop < 0 || start < stop {
		t.Fatalf("panel not stopped and started again around the save: %q", f.cmds())
	}
	if before := seen["systemctl stop footops-web"]; before.PasswordHash != old.PasswordHash || before.TOTPSecret != old.TOTPSecret {
		t.Error("the account was saved before the panel stopped: a login in flight could write the old one back")
	}
	return seen["systemctl start footops-web"]
}

func TestResetPassword(t *testing.T) {
	h, f, seen, old := resetHost(t, nil)
	p, out := prompter("")
	if err := resetPassword(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	acct := stopThenStart(t, f, seen, old)
	_, pw, _ := strings.Cut(out.String(), "Yeni panel şifresi (furkan): ")
	pw, _, _ = strings.Cut(pw, "\n")
	if ok, _ := auth.VerifyPassword(acct.PasswordHash, pw); !ok {
		t.Error("reported password does not match the stored hash")
	}
	// Read again once the panel stopped: the code the login in flight used stays used.
	if acct.TOTPSecret != old.TOTPSecret || len(acct.RecoveryCodes) != 2 {
		t.Errorf("2FA = %q, %d codes", acct.TOTPSecret, len(acct.RecoveryCodes))
	}
	if !strings.Contains(out.String(), "açık oturumların hepsi kapandı") {
		t.Errorf("output:\n%s", out)
	}
}

// operator answers on the terminal as a person does: first (unless empty) to the first
// question, then, reading the secret off the screen, a current code; enrolled is set just
// before that. The returned func ends the session and gives what was on the screen.
func operator(first string, enrolled *atomic.Bool) (*Prompter, func() string) {
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	if first != "" {
		go fmt.Fprintln(inW, first)
	}
	done := make(chan string)
	go func() {
		var lines []string
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			lines = append(lines, sc.Text())
			if _, s, ok := strings.Cut(sc.Text(), "anahtarı elle gir: "); ok {
				code, _ := auth.TOTPCode(strings.TrimSpace(s), time.Now())
				enrolled.Store(true)
				go fmt.Fprintln(inW, code)
			}
		}
		done <- strings.Join(lines, "\n")
	}()
	return NewPrompter(inR, outW, nil), func() string {
		outW.Close()
		return <-done
	}
}

func TestReset2FA(t *testing.T) {
	h, f, seen, old := resetHost(t, nil)
	var enrolled atomic.Bool
	next := f.handle
	f.handle = func(cmd string) (string, error) {
		if cmd == "systemctl stop footops-web" && !enrolled.Load() {
			t.Error("panel stopped during enrolment: an interrupted command would leave it down")
		}
		return next(cmd)
	}
	p, screen := operator("", &enrolled)
	err := reset2FA(ctx, h, p)
	out := screen()
	if err != nil {
		t.Fatal(err)
	}
	acct := stopThenStart(t, f, seen, old)
	if acct.TOTPSecret == old.TOTPSecret || len(acct.RecoveryCodes) != 10 || acct.PasswordHash != old.PasswordHash {
		t.Errorf("account = %+v", acct)
	}
	if !strings.Contains(out, "açık oturumların hepsi kapandı") {
		t.Errorf("output:\n%s", out)
	}
}

// An enrolment cut short keeps the account, and the panel is never stopped.
func TestReset2FAAborted(t *testing.T) {
	h, f, _, old := resetHost(t, nil)
	p, _ := prompter("")
	if err := reset2FA(ctx, h, p); err == nil {
		t.Fatal("reset without a confirmed authenticator")
	}
	if f.ran("systemctl stop") {
		t.Errorf("panel stopped: %q", f.cmds())
	}
	if acct, _ := auth.NewStore(h.Path(config.AuthPath)).Load(); acct.TOTPSecret != old.TOTPSecret {
		t.Error("account changed")
	}
}

// Without an account file, reset-password makes one, then ends the sessions that outlived
// the old one.
func TestResetPasswordWithoutAccount(t *testing.T) {
	var root string
	made := false
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch cmd {
		case "systemctl is-active footops-web":
			return "active\n", nil
		case "systemctl stop footops-web":
			_, err := auth.NewStore(filepath.Join(root, config.AuthPath)).Load()
			made = err == nil
		}
		return "", nil
	})
	root = h.Root
	os.MkdirAll(h.Path(config.WebStateDir), 0o700)
	var enrolled atomic.Bool
	p, screen := operator("furkan", &enrolled)
	err := resetPassword(ctx, h, p)
	out := screen()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !made || !f.ran("systemctl start footops-web") {
		t.Errorf("panel not restarted once the account was made: %q", f.cmds())
	}
	if acct, err := auth.NewStore(h.Path(config.AuthPath)).Load(); err != nil || acct.Username != "furkan" {
		t.Errorf("account = %+v, %v", acct, err)
	}
}

// Ctrl+C or a dropped SSH session while the panel is stopped is held off: the command
// still saves and starts the panel again.
func TestResetHoldsSignalsWhilePanelStopped(t *testing.T) {
	h, f, _, old := resetHost(t, nil)
	next := f.handle
	f.handle = func(cmd string) (string, error) {
		if cmd == "systemctl stop footops-web" {
			for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT} {
				syscall.Kill(os.Getpid(), sig)
			}
		}
		return next(cmd)
	}
	p, _ := prompter("")
	if err := resetPassword(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	if acct, _ := auth.NewStore(h.Path(config.AuthPath)).Load(); acct.PasswordHash == old.PasswordHash || !f.ran("systemctl start footops-web") {
		t.Errorf("reset not finished: %q", f.cmds())
	}
}

// A panel that does not stop does not block the reset; the owner is told how to end the
// open sessions.
func TestResetPasswordStopFails(t *testing.T) {
	h, f, _, old := resetHost(t, errExit)
	p, out := prompter("")
	if err := resetPassword(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	if acct, _ := auth.NewStore(h.Path(config.AuthPath)).Load(); acct.PasswordHash == old.PasswordHash {
		t.Error("password not reset")
	}
	if f.ran("systemctl start") || !strings.Contains(out.String(), "sudo systemctl restart footops-web") {
		t.Errorf("commands %q, output:\n%s", f.cmds(), out)
	}
}

func TestPanelAccountNotCreatedIfEnrolmentFails(t *testing.T) {
	h, _ := newTestHost(t, nil)
	os.MkdirAll(h.Path(config.WebStateDir), 0o700)
	fail := func(context.Context, string, string) error { return ErrAborted }
	err := (panelAccountStep{h, "furkan", "FootOps", &Notes{}, fail}).Apply(ctx)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v", err)
	}
	if h.Exists(config.AuthPath) {
		t.Error("account saved although 2FA was never confirmed")
	}
}

func TestPanelBinary(t *testing.T) {
	h, f := newTestHost(t, nil)
	self := filepath.Join(t.TempDir(), "footops")
	os.WriteFile(self, []byte("binary-v1"), 0o755)
	s := panelBinaryStep{h, self}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before install")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(h.Path(panelBinPath))
	if st.Mode().Perm() != 0o755 || read(t, h, panelBinPath) != "binary-v1" {
		t.Error("binary not installed correctly")
	}
	if !f.ran("systemctl try-restart footops-agent footops-web") {
		t.Error("running services not restarted on upgrade")
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after install")
	}
	os.WriteFile(self, []byte("binary-v2"), 0o755)
	if done, _ := s.Check(ctx); done {
		t.Error("new version not detected")
	}
}

func TestPanelServices(t *testing.T) {
	noSleep(t)
	active, enabled := false, false
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl restart"):
			active = true
		case strings.HasPrefix(cmd, "systemctl enable"):
			enabled = true
		case strings.HasPrefix(cmd, "systemctl is-enabled"):
			if enabled {
				return "enabled\n", nil
			}
			return "disabled\n", errExit
		case strings.HasPrefix(cmd, "systemctl is-active"):
			if active {
				return "active\n", nil
			}
			return "inactive\n", errExit
		}
		return "", nil
	})
	s := panelServicesStep{h}
	if done, _ := s.Check(ctx); done {
		t.Error("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if u := read(t, h, provaUnitPath); !strings.Contains(u, "ConditionPathExists=/var/lib/footops/rehearsal.json") ||
		!strings.Contains(u, "Before=docker.service") || !strings.Contains(u, "footops rehearsal-rule") {
		t.Errorf("rehearsal unit:\n%s", u)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable footops-agent footops-web footops-prova", "systemctl restart footops-agent footops-web"} {
		if !f.ran(want) {
			t.Errorf("missing %q", want)
		}
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestPanelUserAvoidsContainerUIDs(t *testing.T) {
	uid := ""
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case cmd == "id -u footops":
			if uid == "" {
				return "", errExit
			}
			return uid + "\n", nil
		case strings.HasPrefix(cmd, "useradd"):
			uid = "850"
		}
		return "", nil
	})
	s := panelUserStep{h}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("useradd --system --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin -K SYS_UID_MIN=800 -K SYS_UID_MAX=899") {
		t.Errorf("uid range not requested: %v", f.cmds())
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
	uid = "999" // the uid mongo/mysql/redis images write files as
	if done, _ := s.Check(ctx); done {
		t.Error("colliding uid accepted")
	}
	if err := s.Apply(ctx); err == nil || !strings.Contains(err.Error(), "999") {
		t.Errorf("existing colliding user: %v", err)
	}
}

func TestUnitsSeparatePrivileges(t *testing.T) {
	for _, want := range []string{"User=footops", "ProtectSystem=strict", "CapabilityBoundingSet=\n", "NoNewPrivileges=yes", "ReadWritePaths=/var/lib/footops/web"} {
		if !strings.Contains(webUnit, want) {
			t.Errorf("web unit misses %q", want)
		}
	}
	if strings.Contains(webUnit, "SupplementaryGroups") {
		t.Error("web process must not get extra groups")
	}
	for _, hidden := range []string{"-/srv", "-/etc/footops/secrets", "-/var/lib/docker", "-/var/run/docker.sock", "-/var/lib/footops/audit.jsonl",
		"-/var/lib/footops/backup", "-/var/cache/footops"} {
		if !strings.Contains(webUnit, hidden) {
			t.Errorf("web unit does not hide %s", hidden)
		}
	}
	// The agent writes the backup repository's settings into their own directory; the
	// rest of /etc, the other secrets included, stays read-only for it.
	for _, want := range []string{"\nGroup=footops\n", "RuntimeDirectory=footops", "NoNewPrivileges=yes", "ProtectSystem=full\n",
		"\nReadWritePaths=-/etc/footops/secrets/backup -/etc/footops/secrets/notify\n"} {
		if !strings.Contains(agentUnit, want) {
			t.Errorf("agent unit misses %q", want)
		}
	}
	// With User= set (even to root) systemd drops CAP_SETUID under this sandbox, and the
	// file helpers cannot switch to a site's user (found by the integration test).
	if strings.Contains(agentUnit, "\nUser=") {
		t.Error("agent unit sets User=")
	}
}

// fakeRoot plays MongoDB's root user for rotate-mongo-root.
type fakeRoot struct {
	pw        string
	refuse    bool // MongoDB refuses the change
	lose      bool // the change is made, its answer lost
	down      int  // the next logins fail: MongoDB is restarting
	downAfter int  // down, from right after the change
	sets      int
}

func useFakeRoot(t *testing.T, db *fakeRoot) {
	login, set, nap := mongoLogin, mongoSetPassword, sleep
	t.Cleanup(func() { mongoLogin, mongoSetPassword, sleep = login, set, nap })
	mongoLogin = func(_ context.Context, addr string, c mongo.Credential) error {
		switch {
		case addr != core.MongoAddr || c.User != "root" || c.Source != "admin":
			return fmt.Errorf("unexpected %s %+v", addr, c)
		case db.down > 0:
			db.down--
			return errors.New("MongoDB'ye bağlanılamadı: connection refused")
		case c.Password != db.pw:
			return errors.New("MongoDB girişi reddedildi (kullanıcı ya da şifre yanlış)")
		}
		return nil
	}
	mongoSetPassword = func(ctx context.Context, addr string, c mongo.Credential, newPW string) (bool, error) {
		if err := mongoLogin(ctx, addr, c); err != nil {
			return false, err
		}
		db.sets++
		if db.refuse {
			return true, &mongo.Error{Code: 13, Name: "Unauthorized", Msg: "not authorized"}
		}
		db.pw, db.down = newPW, db.downAfter
		if db.lose {
			return true, context.DeadlineExceeded
		}
		return true, nil
	}
	naps := 0
	sleep = func(context.Context, time.Duration) error {
		if naps++; naps > 5 {
			return context.DeadlineExceeded // two minutes passed
		}
		return nil
	}
}

func TestRotateMongoRoot(t *testing.T) {
	h, f := newTestHost(t, nil)
	db := &fakeRoot{pw: "OldPassword123"}
	useFakeRoot(t, db)
	put(t, h, core.MongoSecretPath, core.MongoSecretEnv("OldPassword123"))
	p, out := prompter("e\n")
	if err := rotateMongoRoot(ctx, h, p); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.Contains(c.cmd, "OldPassword123") || strings.Contains(c.stdin, "OldPassword123") {
			t.Fatalf("password given to a command: %s", c.cmd)
		}
	}
	newPW := core.MongoPasswordFromEnv(read(t, h, core.MongoSecretPath))
	if newPW == "OldPassword123" || len(newPW) != 32 || db.pw != newPW {
		t.Fatalf("file %q, MongoDB %q", newPW, db.pw)
	}
	if !strings.Contains(out.String(), newPW) || strings.Contains(out.String(), "⚠") {
		t.Errorf("output:\n%s", out)
	}
	if !f.ran("docker compose --project-name footops-core --file /srv/core/compose.yaml up --detach --wait") {
		t.Error("mongo container not recreated with the new env file")
	}
}

func TestRotateMongoRootKeepsOldPassword(t *testing.T) {
	for name, db := range map[string]*fakeRoot{
		"refused":      {pw: "OldPassword123", refuse: true},
		"wrong file":   {pw: "SomethingElse1"},
		"mongodb down": {pw: "OldPassword123", down: 1},
	} {
		t.Run(name, func(t *testing.T) {
			h, f := newTestHost(t, nil)
			useFakeRoot(t, db)
			before := db.pw
			put(t, h, core.MongoSecretPath, core.MongoSecretEnv("OldPassword123"))
			p, _ := prompter("e\n")
			err := rotateMongoRoot(ctx, h, p)
			if err == nil || !strings.Contains(err.Error(), "eski şifre geçerli") {
				t.Fatalf("err = %v", err)
			}
			if core.MongoPasswordFromEnv(read(t, h, core.MongoSecretPath)) != "OldPassword123" || db.pw != before {
				t.Error("a password changed")
			}
			if len(f.calls) != 0 {
				t.Errorf("container recreated: %v", f.cmds())
			}
		})
	}
}

// The change was made but its answer lost; MongoDB may restart right after it. The
// file follows MongoDB: the new password is found by logging in.
func TestRotateMongoRootLostAnswer(t *testing.T) {
	for name, db := range map[string]*fakeRoot{
		"answer lost":          {pw: "OldPassword123", lose: true},
		"restarting meanwhile": {pw: "OldPassword123", lose: true, downAfter: 5},
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newTestHost(t, nil)
			useFakeRoot(t, db)
			put(t, h, core.MongoSecretPath, core.MongoSecretEnv("OldPassword123"))
			p, out := prompter("e\n")
			if err := rotateMongoRoot(ctx, h, p); err != nil {
				t.Fatal(err)
			}
			if got := core.MongoPasswordFromEnv(read(t, h, core.MongoSecretPath)); got != db.pw || got == "OldPassword123" {
				t.Errorf("file %q, MongoDB %q", got, db.pw)
			}
			if !strings.Contains(out.String(), "cevabı alınamadı ama yeni şifreyle giriş yapılabiliyor") {
				t.Errorf("output:\n%s", out)
			}
		})
	}
}

// MongoDB does not come back: neither password can be checked. The file stays, and the
// new password is shown so it is not lost.
func TestRotateMongoRootUnknown(t *testing.T) {
	h, f := newTestHost(t, nil)
	db := &fakeRoot{pw: "OldPassword123", lose: true, downAfter: 1000}
	useFakeRoot(t, db)
	put(t, h, core.MongoSecretPath, core.MongoSecretEnv("OldPassword123"))
	p, out := prompter("e\n")
	err := rotateMongoRoot(ctx, h, p)
	if err == nil || !strings.Contains(err.Error(), "anlaşılamadı") {
		t.Fatalf("err = %v", err)
	}
	if core.MongoPasswordFromEnv(read(t, h, core.MongoSecretPath)) != "OldPassword123" || !strings.Contains(out.String(), db.pw) {
		t.Errorf("file changed or new password not shown:\n%s", out)
	}
	if len(f.calls) != 0 {
		t.Errorf("container recreated: %v", f.cmds())
	}
}

func TestRotateMongoRootDeclined(t *testing.T) {
	h, f := newTestHost(t, nil)
	put(t, h, core.MongoSecretPath, core.MongoSecretEnv("OldPassword123"))
	db := &fakeRoot{pw: "OldPassword123"}
	useFakeRoot(t, db)
	p, _ := prompter("\n") // default: no
	if err := rotateMongoRoot(ctx, h, p); !errors.Is(err, ErrAborted) || db.sets != 0 {
		t.Fatalf("err = %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("ran commands after declining: %v", f.cmds())
	}
}

// With a real MongoDB (FOOTOPS_TEST_MONGO): a throwaway administrator's password is
// changed; only the new one logs in afterwards.
func TestLiveChangeMongoPassword(t *testing.T) {
	raw := os.Getenv("FOOTOPS_TEST_MONGO")
	if raw == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok: gerçek MongoDB testleri atlandı")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatal(err)
	}
	rootPW, _ := u.User.Password()
	root, err := mongo.Dial(ctx, u.Host, mongo.Credential{User: u.User.Username(), Password: rootPW, Source: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	user, _ := randomPassword(8)
	user = "rot" + user
	cmd := mongo.NewBuilder().String("createUser", user).String("pwd", "Eski12345").
		StartArray("roles").StartDoc("").String("role", "root").String("db", "admin").End().End().MustBuild()
	if _, err := root.Run(ctx, "admin", cmd, 1<<20); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = root.Run(ctx, "admin", mongo.NewBuilder().String("dropUser", user).MustBuild(), 1<<20) })
	login := func(pw string) error {
		c, err := mongo.Dial(ctx, u.Host, mongo.Credential{User: user, Password: pw, Source: "admin"})
		if err == nil {
			c.Close()
		}
		return err
	}

	if state, err := changeMongoPassword(ctx, u.Host, mongo.Credential{User: user, Password: "Yanlis123", Source: "admin"}, "Yeni12345"); state != pwOld || err == nil {
		t.Errorf("wrong old password: %v, %v", state, err)
	}
	state, err := changeMongoPassword(ctx, u.Host, mongo.Credential{User: user, Password: "Eski12345", Source: "admin"}, "Yeni12345")
	if state != pwNew || err != nil {
		t.Fatalf("%v, %v", state, err)
	}
	if login("Eski12345") == nil || login("Yeni12345") != nil {
		t.Error("the new password is not the only one that logs in")
	}
}
