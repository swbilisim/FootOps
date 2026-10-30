package install

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// userDB simulates getent/id/passwd for one user.
type userDB struct {
	exists      bool
	inSudo      bool
	passwordSet bool
}

func (u *userDB) handle(user string) func(cmd string) (string, error) {
	return func(cmd string) (string, error) {
		switch {
		case cmd == "getent passwd "+user:
			if !u.exists {
				return "", errExit
			}
			return user + ":x:1000:1000::/home/" + user + ":/bin/bash\n", nil
		case cmd == "id -nG "+user:
			if u.inSudo {
				return user + " sudo\n", nil
			}
			return user + "\n", nil
		case strings.HasPrefix(cmd, "useradd "):
			u.exists, u.inSudo = true, true
		case strings.HasPrefix(cmd, "usermod -aG sudo"):
			u.inSudo = true
		case cmd == "passwd -S "+user:
			if u.passwordSet {
				return user + " P 10/26/2026 0 99999 7 -1\n", nil
			}
			return user + " L 10/26/2026 0 99999 7 -1\n", nil
		case cmd == "chpasswd":
			u.passwordSet = true
		}
		return "", nil
	}
}

func TestUserHome(t *testing.T) {
	h, _ := newTestHost(t, func(cmd string) (string, error) {
		return "deploy:x:1001:1001:Deploy:/srv/deploy:/bin/bash\n", nil
	})
	home, ok := userHome(ctx, h, "deploy")
	if !ok || home != "/srv/deploy" {
		t.Errorf("home = %q, %v", home, ok)
	}
	if _, ok := userHome(ctx, h, "other"); ok {
		t.Error("getent line for a different user accepted")
	}
}

func TestMergeAuthorizedKeys(t *testing.T) {
	cur := "# comment\n" + keyA // no trailing newline
	got := mergeAuthorizedKeys(cur, []string{keyA, keyB, keyB})
	want := "# comment\n" + keyA + "\n" + keyB + "\n"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if mergeAuthorizedKeys(got, []string{keyA, keyB}) != got {
		t.Error("not idempotent")
	}
	// Same key with a different comment is not added twice.
	if mergeAuthorizedKeys(got, []string{strings.Replace(keyA, "a@test", "laptop", 1)}) != got {
		t.Error("duplicate key with different comment added")
	}
}

func TestAdminUserCreatesUserWithPassword(t *testing.T) {
	db := &userDB{}
	h, f := newTestHost(t, db.handle("furkan"))
	s := adminUserStep{h: h, user: "furkan", keys: []string{keyA}, mode: SudoPassword, password: "  secret password  "}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("useradd --create-home --shell /bin/bash --groups sudo furkan") {
		t.Errorf("useradd missing: %v", f.cmds())
	}
	if got := read(t, h, "/home/furkan/.ssh/authorized_keys"); got != keyA+"\n" {
		t.Errorf("authorized_keys = %q", got)
	}
	var chpasswd *call
	for i := range f.calls {
		if f.calls[i].cmd == "chpasswd" {
			chpasswd = &f.calls[i]
		}
		if strings.Contains(f.calls[i].cmd, "secret password") {
			t.Fatalf("password leaked into argv: %q", f.calls[i].cmd)
		}
	}
	if chpasswd == nil || chpasswd.stdin != "furkan:  secret password  \n" {
		t.Errorf("chpasswd stdin = %+v", chpasswd)
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestAdminUserResumeWithoutPassword(t *testing.T) {
	db := &userDB{exists: true, inSudo: true}
	h, _ := newTestHost(t, db.handle("furkan"))
	s := adminUserStep{h: h, user: "furkan", keys: []string{keyA}, mode: SudoPassword}
	if err := s.Apply(ctx); err == nil {
		t.Fatal("password mode without any password accepted")
	}
	db.passwordSet = true
	if err := s.Apply(ctx); err != nil {
		t.Fatalf("existing password not accepted on resume: %v", err)
	}
}

func TestAdminUserNoPasswordSudoers(t *testing.T) {
	db := &userDB{}
	h, f := newTestHost(t, db.handle("furkan"))
	s := adminUserStep{h: h, user: "furkan", keys: []string{keyA}, mode: SudoNoPassword}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("visudo -cf /etc/footops/sudoers.check") {
		t.Error("sudoers not validated with visudo")
	}
	if h.Exists("/etc/footops/sudoers.check") {
		t.Error("temporary sudoers file left behind")
	}
	if read(t, h, "/etc/sudoers.d/90-footops-furkan") != "furkan ALL=(ALL) NOPASSWD:ALL\n" {
		t.Error("sudoers drop-in missing")
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestAdminUserInvalidSudoersNotInstalled(t *testing.T) {
	db := &userDB{}
	inner := db.handle("furkan")
	h, _ := newTestHost(t, func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "visudo") {
			return "parse error", errExit
		}
		return inner(cmd)
	})
	s := adminUserStep{h: h, user: "furkan", keys: []string{keyA}, mode: SudoNoPassword}
	if err := s.Apply(ctx); err == nil {
		t.Fatal("expected error")
	}
	if h.Exists("/etc/sudoers.d/90-footops-furkan") {
		t.Fatal("unvalidated sudoers file installed")
	}
}

func TestUfwSatisfied(t *testing.T) {
	status := `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), deny (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
443/tcp                    ALLOW IN    Anywhere
22/tcp (v6)                ALLOW IN    Anywhere (v6)
`
	if !ufwSatisfied(status, firewallPorts) {
		t.Error("complete config rejected")
	}
	if ufwSatisfied(strings.Replace(status, "443/tcp                    ALLOW IN", "443/tcp                    DENY IN ", 1), firewallPorts) {
		t.Error("missing 443 accepted")
	}
	if ufwSatisfied(strings.Replace(status, "Status: active", "Status: inactive", 1), firewallPorts) {
		t.Error("inactive firewall accepted")
	}
	if ufwSatisfied(strings.Replace(status, "deny (incoming)", "allow (incoming)", 1), firewallPorts) {
		t.Error("allow-by-default accepted")
	}
}

// `ufw show added` lists rules as they were added, also while the firewall is off.
const ufwAddedOld = `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp
ufw allow 80/tcp
ufw allow 443/tcp
`

func TestUfwStale(t *testing.T) {
	if got := ufwStale(ufwAddedOld); len(got) != 1 || got[0] != "80/tcp" {
		t.Errorf("stale = %v", got)
	}
	// Only the rule as FootOps added it: a rule narrowed to a source is someone else's.
	narrowed := strings.Replace(ufwAddedOld, "ufw allow 80/tcp", "ufw allow from 10.0.0.0/8 to any port 80 proto tcp", 1)
	if got := ufwStale(narrowed); len(got) != 0 {
		t.Errorf("narrowed rule taken as stale: %v", got)
	}
}

func TestFirewallClosesWhatEarlierVersionsOpened(t *testing.T) {
	active := "Status: active\nDefault: deny (incoming), allow (outgoing)\n\n" +
		"22/tcp                     ALLOW IN    Anywhere\n80/tcp                     ALLOW IN    Anywhere\n" +
		"443/tcp                    ALLOW IN    Anywhere\n80/tcp (v6)                ALLOW IN    Anywhere (v6)\n"
	host := func(status string) (Host, *fakeRunner) {
		return newTestHost(t, func(cmd string) (string, error) {
			switch cmd {
			case "ufw status verbose":
				return status, nil
			case "ufw show added":
				return ufwAddedOld, nil
			}
			return "", nil
		})
	}
	// A server set up by an earlier version: Check fails, Apply deletes that one rule and
	// touches nothing else.
	h, f := host(active)
	if ok, _ := (firewallStep{h}).Check(ctx); ok {
		t.Error("80 left open accepted")
	}
	if err := (firewallStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, c := range f.cmds() {
		if !strings.HasPrefix(c, "ufw status") && !strings.HasPrefix(c, "ufw show") {
			changes = append(changes, c)
		}
	}
	if strings.Join(changes, "; ") != "ufw delete allow 80/tcp" {
		t.Errorf("changes: %v", changes)
	}
	// Firewall off: the stale rule goes before enabling, so it never comes back on.
	h, f = host("Status: inactive\n")
	if err := (firewallStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(f.cmds(), "\n")
	del, enable := strings.Index(cmds, "ufw delete allow 80/tcp"), strings.Index(cmds, "ufw --force enable")
	if del < 0 || enable < 0 || del > enable || strings.Contains(cmds, "ufw allow 80/tcp") {
		t.Errorf("commands:\n%s", cmds)
	}
	// A fresh server has no such rule: nothing to delete.
	h, f = newTestHost(t, nil)
	if err := (firewallStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if cmds := strings.Join(f.cmds(), "\n"); strings.Contains(cmds, "delete") {
		t.Errorf("deleted on a fresh server:\n%s", cmds)
	}
}

func TestFirewallAllowsSSHBeforeEnabling(t *testing.T) {
	h, f := newTestHost(t, nil)
	if err := (firewallStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	cmds := f.cmds()
	ssh, enable := -1, -1
	for i, c := range cmds {
		switch c {
		case "ufw allow 22/tcp":
			ssh = i
		case "ufw --force enable":
			enable = i
		}
	}
	if ssh < 0 || enable < 0 || ssh > enable {
		t.Errorf("SSH must be allowed before enabling: %v", cmds)
	}
}

// sshHost simulates sshd: -T reports the effective config of our drop-in.
func sshHost(t *testing.T, sshdTestErr error, effective string) (Host, *fakeRunner) {
	db := &userDB{exists: true, inSudo: true}
	users := db.handle("furkan")
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch cmd {
		case "sshd -t":
			return "", sshdTestErr
		case "sshd -T":
			return effective, nil
		}
		return users(cmd)
	})
	put(t, h, "/home/furkan/.ssh/authorized_keys", keyA+"\n")
	return h, f
}

const goodEffective = "port 22\npermitrootlogin no\npasswordauthentication no\nkbdinteractiveauthentication no\npubkeyauthentication yes\nallowusers furkan\n"

func confirmWith(answer bool) ConfirmFunc {
	return func(context.Context, string) (bool, error) { return answer, nil }
}

func TestSSHHardeningNeedsConfirmation(t *testing.T) {
	h, f := sshHost(t, nil, goodEffective)
	err := (sshStep{h, "furkan", confirmWith(false)}).Apply(ctx)
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	if h.Exists(sshdDropIn) || f.ran("systemctl") {
		t.Fatal("SSH changed without confirmation")
	}
}

func TestSSHHardeningRefusesWithoutKeys(t *testing.T) {
	h, _ := sshHost(t, nil, goodEffective)
	put(t, h, "/home/furkan/.ssh/authorized_keys", "")
	asked := false
	confirm := func(context.Context, string) (bool, error) { asked = true; return true, nil }
	if err := (sshStep{h, "furkan", confirm}).Apply(ctx); err == nil {
		t.Fatal("hardened SSH for a user without keys")
	}
	if asked || h.Exists(sshdDropIn) {
		t.Fatal("proceeded without keys")
	}
}

func TestSSHHardeningAppliesAndReloads(t *testing.T) {
	h, f := sshHost(t, nil, goodEffective)
	s := sshStep{h, "furkan", confirmWith(true)}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	conf := read(t, h, sshdDropIn)
	for _, want := range []string{"PermitRootLogin no", "PasswordAuthentication no", "AllowUsers furkan", "MaxAuthTries 6"} {
		if !strings.Contains(conf, want) {
			t.Errorf("config misses %q", want)
		}
	}
	if !f.ran("systemctl try-reload-or-restart ssh") {
		t.Error("sshd not reloaded")
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestSSHHardeningRestoresOnInvalidConfig(t *testing.T) {
	h, f := sshHost(t, errExit, goodEffective)
	put(t, h, sshdDropIn, "# previous\n")
	if err := (sshStep{h, "furkan", confirmWith(true)}).Apply(ctx); err == nil {
		t.Fatal("expected error")
	}
	if read(t, h, sshdDropIn) != "# previous\n" {
		t.Error("previous config not restored")
	}
	if f.ran("systemctl") {
		t.Error("reloaded an invalid config")
	}
}

func TestSSHHardeningRestoresWhenOverridden(t *testing.T) {
	h, f := sshHost(t, nil, strings.Replace(goodEffective, "passwordauthentication no", "passwordauthentication yes", 1))
	if err := (sshStep{h, "furkan", confirmWith(true)}).Apply(ctx); err == nil {
		t.Fatal("expected error when another file re-enables passwords")
	}
	if h.Exists(sshdDropIn) {
		t.Error("drop-in left in place after failed verification")
	}
	if f.ran("systemctl") {
		t.Error("reloaded unverified config")
	}
}

func TestSSHHardeningStopsIfCancelledDuringQuestion(t *testing.T) {
	h, _ := sshHost(t, nil, goodEffective)
	cctx, cancel := context.WithCancel(ctx)
	confirm := func(context.Context, string) (bool, error) { cancel(); return true, nil }
	if err := (sshStep{h, "furkan", confirm}).Apply(cctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if h.Exists(sshdDropIn) {
		t.Fatal("config written after cancellation")
	}
}

func TestEffectiveMatches(t *testing.T) {
	want := sshStep{user: "furkan"}.wantEffective()
	if !effectiveMatches(goodEffective, want) {
		t.Error("good config rejected")
	}
	if effectiveMatches(strings.Replace(goodEffective, "allowusers furkan", "allowusers root", 1), want) {
		t.Error("wrong AllowUsers accepted")
	}
}

func TestFail2banCountsKeyOnlyFailures(t *testing.T) {
	// Without aggressive mode, failed public-key logins are never counted.
	if !strings.Contains(fail2banConf, "mode     = aggressive") {
		t.Error("sshd jail must use aggressive mode")
	}
}
