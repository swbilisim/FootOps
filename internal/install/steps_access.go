package install

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ---- admin user ----

type adminUserStep struct {
	h        Host
	user     string
	keys     []string
	mode     SudoMode
	password string // empty on a resumed run; then an existing password is accepted
}

func (s adminUserStep) Name() string { return "admin-user" }
func (s adminUserStep) Describe() string {
	return fmt.Sprintf("Yönetici kullanıcı %q: sudo grubu, %d SSH anahtarı", s.user, len(s.keys))
}

// The file name has no dot: sudo ignores files in sudoers.d whose name contains one.
func (s adminUserStep) sudoersPath() string    { return "/etc/sudoers.d/90-footops-" + s.user }
func (s adminUserStep) sudoersContent() string { return s.user + " ALL=(ALL) NOPASSWD:ALL\n" }

func (s adminUserStep) Check(ctx context.Context) (bool, error) {
	home, exists := userHome(ctx, s.h, s.user)
	if !exists {
		return false, nil
	}
	groups, err := s.h.Run.Run(ctx, "id", "-nG", s.user)
	if err != nil || !containsField(groups, "sudo") {
		return false, nil
	}
	cur, err := s.h.ReadFile(home + "/.ssh/authorized_keys")
	if err != nil {
		return false, err
	}
	if mergeAuthorizedKeys(cur, s.keys) != cur {
		return false, nil
	}
	switch s.mode {
	case SudoNoPassword:
		return s.h.FileEquals(s.sudoersPath(), s.sudoersContent())
	default:
		if s.h.Exists(s.sudoersPath()) {
			return false, nil
		}
		return s.passwordSet(ctx), nil
	}
}

func (s adminUserStep) passwordSet(ctx context.Context) bool {
	// passwd -S: "<user> P ..." means a usable password is set.
	out, err := s.h.Run.Run(ctx, "passwd", "-S", s.user)
	f := strings.Fields(out)
	return err == nil && len(f) >= 2 && f[1] == "P"
}

func (s adminUserStep) Apply(ctx context.Context) error {
	if _, exists := userHome(ctx, s.h, s.user); !exists {
		if _, err := s.h.Run.Run(ctx, "useradd", "--create-home", "--shell", "/bin/bash", "--groups", "sudo", s.user); err != nil {
			return err
		}
	} else if _, err := s.h.Run.Run(ctx, "usermod", "-aG", "sudo", s.user); err != nil {
		return err
	}
	home, exists := userHome(ctx, s.h, s.user)
	if !exists {
		return fmt.Errorf("%s oluşturuldu ama getent ile bulunamadı", s.user)
	}

	keysPath := home + "/.ssh/authorized_keys"
	cur, err := s.h.ReadFile(keysPath)
	if err != nil {
		return err
	}
	if err := s.h.WriteFile(keysPath, mergeAuthorizedKeys(cur, s.keys), 0o600); err != nil {
		return err
	}
	ssh := home + "/.ssh"
	for _, cmd := range [][]string{
		{"chmod", "700", ssh},
		{"chown", "-R", s.user + ":" + s.user, ssh},
	} {
		if _, err := s.h.Run.Run(ctx, cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}

	switch s.mode {
	case SudoNoPassword:
		return s.writeSudoers(ctx)
	default:
		if err := s.h.Remove(s.sudoersPath()); err != nil {
			return err
		}
		if s.password == "" {
			if s.passwordSet(ctx) {
				return nil
			}
			return errors.New("sudo şifresi ayarlı değil; kurulumu yeniden başlatıp şifreyi gir")
		}
		// chpasswd reads "user:password" from stdin, so the password never appears in argv or logs.
		_, err := s.h.Run.RunInput(ctx, s.user+":"+s.password+"\n", "chpasswd")
		return err
	}
}

// writeSudoers validates the drop-in with visudo before moving it into place:
// a broken sudoers file locks everyone out of sudo.
func (s adminUserStep) writeSudoers(ctx context.Context) error {
	tmp := configDir + "/sudoers.check"
	if err := s.h.WriteFile(tmp, s.sudoersContent(), 0o440); err != nil {
		return err
	}
	defer s.h.Remove(tmp)
	if _, err := s.h.Run.Run(ctx, "visudo", "-cf", tmp); err != nil {
		return err
	}
	return s.h.WriteFile(s.sudoersPath(), s.sudoersContent(), 0o440)
}

// userHome looks the user up in the passwd database. The home directory is read rather
// than assumed, in case the account already existed with a non-standard home.
func userHome(ctx context.Context, h Host, user string) (home string, exists bool) {
	out, err := h.Run.Run(ctx, "getent", "passwd", user)
	if err != nil {
		return "", false
	}
	// name:x:uid:gid:gecos:home:shell
	f := strings.Split(strings.TrimSpace(out), ":")
	if len(f) < 7 || f[0] != user || !strings.HasPrefix(f[5], "/") {
		return "", false
	}
	return f[5], true
}

// mergeAuthorizedKeys appends keys that are not yet present, keeping existing lines.
func mergeAuthorizedKeys(current string, keys []string) string {
	have := map[string]bool{}
	for _, l := range strings.Split(current, "\n") {
		if id, err := ParseSSHKey(l); err == nil {
			have[id] = true
		}
	}
	out := current
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	for _, k := range keys {
		id, err := ParseSSHKey(k)
		if err != nil || have[id] {
			continue
		}
		have[id] = true
		out += strings.TrimSpace(k) + "\n"
	}
	return out
}

func containsField(s, want string) bool {
	for _, f := range strings.Fields(s) {
		if f == want {
			return true
		}
	}
	return false
}

// ---- firewall ----

// firewallPorts are open; firewallClosed were opened by earlier versions and are closed
// again (80: sites are https only, nothing listens there).
var (
	firewallPorts  = []string{"22/tcp", "443/tcp"}
	firewallClosed = []string{"80/tcp"}
)

type firewallStep struct{ h Host }

func (s firewallStep) Name() string { return "firewall" }
func (s firewallStep) Describe() string {
	return "UFW: gelen trafik kapalı, sadece " + strings.Join(firewallPorts, ", ") + " açık"
}

func (s firewallStep) Check(ctx context.Context) (bool, error) {
	status, err := s.h.Run.Run(ctx, "ufw", "status", "verbose")
	if err != nil {
		return false, nil
	}
	added, err := s.h.Run.Run(ctx, "ufw", "show", "added")
	if err != nil {
		return false, nil
	}
	return ufwSatisfied(status, firewallPorts) && len(ufwStale(added)) == 0, nil
}

// Apply does only what is missing, so a server already set up (perhaps with a rule
// narrowed by hand) only loses the rules earlier versions added.
func (s firewallStep) Apply(ctx context.Context) error {
	var cmds [][]string
	// `ufw show added` lists the rules even while the firewall is off, so enabling it
	// below never brings a stale one back.
	if added, err := s.h.Run.Run(ctx, "ufw", "show", "added"); err == nil {
		for _, p := range ufwStale(added) {
			cmds = append(cmds, []string{"ufw", "delete", "allow", p})
		}
	}
	if status, err := s.h.Run.Run(ctx, "ufw", "status", "verbose"); err != nil || !ufwSatisfied(status, firewallPorts) {
		cmds = append(cmds, []string{"ufw", "default", "deny", "incoming"}, []string{"ufw", "default", "allow", "outgoing"})
		// SSH is allowed before the firewall is enabled, so the current session survives.
		for _, p := range firewallPorts {
			cmds = append(cmds, []string{"ufw", "allow", p})
		}
		cmds = append(cmds, []string{"ufw", "--force", "enable"})
	}
	for _, c := range cmds {
		if _, err := s.h.Run.Run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// ufwSatisfied checks `ufw status verbose` for an active firewall that denies incoming
// traffic by default and allows each port.
func ufwSatisfied(status string, ports []string) bool {
	if !strings.Contains(status, "Status: active") || !strings.Contains(status, "deny (incoming)") {
		return false
	}
	allowed := map[string]bool{}
	for _, l := range strings.Split(status, "\n") {
		f := strings.Fields(l)
		// "22/tcp                     ALLOW IN    Anywhere"
		if len(f) >= 3 && f[1] == "ALLOW" && f[2] == "IN" {
			allowed[f[0]] = true
		}
	}
	for _, p := range ports {
		if !allowed[p] {
			return false
		}
	}
	return true
}

// ufwStale returns the firewallClosed ports that `ufw show added` still lists exactly as
// an earlier version added them ("ufw allow 80/tcp"); a rule narrowed to a source is
// someone else's and stays.
func ufwStale(added string) []string {
	var stale []string
	for _, l := range strings.Split(added, "\n") {
		for _, p := range firewallClosed {
			if strings.TrimSpace(l) == "ufw allow "+p {
				stale = append(stale, p)
			}
		}
	}
	return stale
}

// ---- fail2ban ----

// mode = aggressive: with password login disabled, attackers' attempts are logged as
// "Connection closed by authenticating user ... [preauth]", which the default "normal"
// mode does not count, so the jail would never ban anyone.
const (
	fail2banPath = "/etc/fail2ban/jail.d/footops.local"
	fail2banConf = `# Managed by FootOps
[sshd]
enabled  = true
backend  = systemd
mode     = aggressive
maxretry = 5
findtime = 10m
bantime  = 1h
`
)

type fail2banStep struct{ h Host }

func (s fail2banStep) Name() string { return "fail2ban" }
func (s fail2banStep) Describe() string {
	return "fail2ban: 10 dakikada 5 hatalı SSH denemesi → 1 saat engel"
}

func (s fail2banStep) Check(ctx context.Context) (bool, error) {
	ok, err := s.h.FileEquals(fail2banPath, fail2banConf)
	if err != nil || !ok {
		return false, err
	}
	out, err := s.h.Run.Run(ctx, "systemctl", "is-active", "fail2ban")
	return err == nil && strings.TrimSpace(out) == "active", nil
}

func (s fail2banStep) Apply(ctx context.Context) error {
	if err := s.h.WriteFile(fail2banPath, fail2banConf, 0o644); err != nil {
		return err
	}
	for _, c := range [][]string{
		{"systemctl", "enable", "fail2ban"},
		{"systemctl", "restart", "fail2ban"},
	} {
		if _, err := s.h.Run.Run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// ---- SSH hardening ----

// The drop-in is named 00- because sshd keeps the first value it reads and Ubuntu's
// cloud-init writes 50-cloud-init.conf, which may enable password login.
const sshdDropIn = "/etc/ssh/sshd_config.d/00-footops.conf"

// ConfirmFunc asks the operator to confirm that key login works before lockout-prone changes.
type ConfirmFunc func(ctx context.Context, user string) (bool, error)

type sshStep struct {
	h       Host
	user    string
	confirm ConfirmFunc
}

func (s sshStep) Name() string { return "ssh-hardening" }
func (s sshStep) Describe() string {
	return "SSH: root ve şifreyle giriş kapalı, sadece " + s.user + " anahtarla"
}

// config is the hardening drop-in. MaxAuthTries stays at OpenSSH's default of 6: each
// key offered by the client's agent counts as a try, so a lower value locks out people
// with several keys loaded. Brute force is fail2ban's job.
func (s sshStep) config() string {
	return fmt.Sprintf(`# Managed by FootOps. Re-running "footops install" overwrites this file.
PermitRootLogin no
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AuthenticationMethods publickey
PermitEmptyPasswords no
MaxAuthTries 6
LoginGraceTime 30
X11Forwarding no
AllowUsers %s
`, s.user)
}

// wantEffective are `sshd -T` values (lower-case keys) that must hold after hardening.
// Checking the effective config catches other files overriding ours.
func (s sshStep) wantEffective() map[string]string {
	return map[string]string{
		"permitrootlogin":              "no",
		"passwordauthentication":       "no",
		"kbdinteractiveauthentication": "no",
		"pubkeyauthentication":         "yes",
		"allowusers":                   s.user,
	}
}

func (s sshStep) Check(ctx context.Context) (bool, error) {
	ok, err := s.h.FileEquals(sshdDropIn, s.config())
	if err != nil || !ok {
		return false, err
	}
	out, err := sshdRun(ctx, s.h, "-T")
	if err != nil {
		return false, nil
	}
	return effectiveMatches(out, s.wantEffective()), nil
}

func (s sshStep) Apply(ctx context.Context) error {
	home, exists := userHome(ctx, s.h, s.user)
	if !exists {
		return fmt.Errorf("%s kullanıcısı yok; root girişini kapatmak sunucuyu kilitler", s.user)
	}
	keys, err := s.h.ReadFile(home + "/.ssh/authorized_keys")
	if err != nil {
		return err
	}
	if strings.TrimSpace(keys) == "" {
		return fmt.Errorf("%s için SSH anahtarı yok; root girişini kapatmak sunucuyu kilitler", s.user)
	}
	ok, err := s.confirm(ctx, s.user)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: SSH sertleştirme onaylanmadı; anahtar girişi çalışınca kurulumu yeniden çalıştır", ErrAborted)
	}
	// The operator may have pressed Ctrl+C while the question was open.
	if err := ctx.Err(); err != nil {
		return err
	}

	prev, err := s.h.ReadFile(sshdDropIn)
	if err != nil {
		return err
	}
	restore := func() {
		if prev == "" {
			_ = s.h.Remove(sshdDropIn)
		} else {
			_ = s.h.WriteFile(sshdDropIn, prev, 0o644)
		}
	}
	if err := s.h.WriteFile(sshdDropIn, s.config(), 0o644); err != nil {
		return err
	}
	// Never leave a config behind that was not validated: an invalid sshd config stops
	// sshd from starting after the next reboot, locking everyone out.
	if _, err := sshdRun(ctx, s.h, "-t"); err != nil {
		restore()
		return err
	}
	out, err := sshdRun(ctx, s.h, "-T")
	if err != nil {
		restore()
		return err
	}
	if !effectiveMatches(out, s.wantEffective()) {
		restore()
		return errors.New("SSH ayarı başka bir dosya tarafından eziliyor; /etc/ssh/sshd_config.d içeriğini ve sshd -T çıktısını kontrol et")
	}
	// Existing sessions stay open; only new connections use the new settings.
	_, err = s.h.Run.Run(ctx, "systemctl", "try-reload-or-restart", "ssh")
	return err
}

// sshdRun runs sshd in test mode. With socket activation (Ubuntu 24.04) the privilege
// separation directory may not exist yet, and sshd -t/-T refuse to run without it.
func sshdRun(ctx context.Context, h Host, flag string) (string, error) {
	if _, err := h.Run.Run(ctx, "mkdir", "-p", "-m", "0755", "/run/sshd"); err != nil {
		return "", err
	}
	return h.Run.Run(ctx, "sshd", flag)
}

func effectiveMatches(sshdT string, want map[string]string) bool {
	got := map[string]string{}
	for _, l := range strings.Split(sshdT, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), " ")
		if ok {
			got[strings.ToLower(k)] = strings.TrimSpace(v)
		}
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
