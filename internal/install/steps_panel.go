package install

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/core"
)

const (
	panelUser     = agent.PanelUser
	panelBinPath  = "/usr/local/bin/footops"
	agentUnitPath = "/etc/systemd/system/footops-agent.service"
	webUnitPath   = "/etc/systemd/system/footops-web.service"
	panelStateDir = "/var/lib/footops"
	// dockerConfigDir is the agent's DOCKER_CONFIG.
	dockerConfigDir = panelStateDir + "/docker"
)

// ---- tunnel token ----

type tunnelSecretStep struct {
	h     Host
	token string // empty on a resumed run
}

func (s tunnelSecretStep) Name() string { return "tunnel-secret" }
func (s tunnelSecretStep) Describe() string {
	return "Cloudflare Tunnel token'ı (" + core.TunnelSecretPath + ")"
}

func (s tunnelSecretStep) Check(ctx context.Context) (bool, error) {
	env, err := s.h.ReadFile(core.TunnelSecretPath)
	if err != nil {
		return false, err
	}
	have := core.TunnelTokenFromEnv(env)
	return have != "" && (s.token == "" || s.token == have), nil
}

func (s tunnelSecretStep) Apply(ctx context.Context) error {
	if s.token == "" {
		return errors.New("Cloudflare Tunnel token'ı yok; kurulumu yeniden başlatıp token'ı gir")
	}
	if err := os.MkdirAll(s.h.Path(filepath.Dir(core.TunnelSecretPath)), 0o700); err != nil {
		return err
	}
	return s.h.WriteFile(core.TunnelSecretPath, core.TunnelSecretEnv(s.token), 0o600)
}

// ---- panel system user ----

// The panel user's uid comes from 800-899. A plain system user gets 999, which is the
// uid many images (mongo, mysql, redis) run as: files those containers write to bind
// mounts would then belong to the panel user on the host.
const (
	panelUIDMin = 800
	panelUIDMax = 899
)

type panelUserStep struct{ h Host }

func (s panelUserStep) Name() string { return "panel-user" }
func (s panelUserStep) Describe() string {
	return "Sistem kullanıcısı '" + panelUser + "' (giriş yok, docker grubunda değil, uid 800-899)"
}

func (s panelUserStep) Check(ctx context.Context) (bool, error) {
	uid, ok := s.uid(ctx)
	return ok && uid >= panelUIDMin && uid <= panelUIDMax, nil
}

func (s panelUserStep) uid(ctx context.Context) (int, bool) {
	out, err := s.h.Run.Run(ctx, "id", "-u", panelUser)
	if err != nil {
		return 0, false
	}
	uid, err := strconv.Atoi(strings.TrimSpace(out))
	return uid, err == nil
}

func (s panelUserStep) Apply(ctx context.Context) error {
	if uid, ok := s.uid(ctx); ok {
		// Changing the uid of an existing user would leave files with the old owner;
		// that is an operator decision.
		return fmt.Errorf("'%s' kullanıcısı uid %d ile zaten var (beklenen %d-%d). Container'ların sık kullandığı "+
			"uid'lerle çakışabilir; kullanıcıyı silip (userdel %s) kurulumu tekrar çalıştır",
			panelUser, uid, panelUIDMin, panelUIDMax, panelUser)
	}
	r := fmt.Sprintf
	_, err := s.h.Run.Run(ctx, "useradd", "--system", "--user-group", "--no-create-home",
		"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin",
		"-K", r("SYS_UID_MIN=%d", panelUIDMin), "-K", r("SYS_UID_MAX=%d", panelUIDMax),
		"-K", r("SYS_GID_MIN=%d", panelUIDMin), "-K", r("SYS_GID_MAX=%d", panelUIDMax),
		panelUser)
	return err
}

// ---- panel binary ----

type panelBinaryStep struct {
	h    Host
	self string // path of the running footops binary
}

func (s panelBinaryStep) Name() string { return "panel-binary" }
func (s panelBinaryStep) Describe() string {
	return "footops programını " + panelBinPath + " konumuna kur"
}

func (s panelBinaryStep) Check(ctx context.Context) (bool, error) {
	self, err := os.ReadFile(s.self)
	if err != nil {
		return false, err
	}
	installed, err := os.ReadFile(s.h.Path(panelBinPath))
	if err != nil {
		return false, nil
	}
	return sha256.Sum256(self) == sha256.Sum256(installed), nil
}

func (s panelBinaryStep) Apply(ctx context.Context) error {
	self, err := os.ReadFile(s.self)
	if err != nil {
		return err
	}
	if err := s.h.WriteFile(panelBinPath, string(self), 0o755); err != nil {
		return err
	}
	// On an upgrade the services pick up the new binary; on a first install they do not
	// exist yet, which try-restart reports as an error we do not care about.
	_, _ = s.h.Run.Run(ctx, "systemctl", "try-restart", "footops-agent", "footops-web")
	return nil
}

// ---- panel configuration ----

type panelConfigStep struct {
	h   Host
	cfg config.Panel
}

func (s panelConfigStep) Name() string { return "panel-config" }
func (s panelConfigStep) Describe() string {
	return "Panel ayarları (" + config.Path + ") ve veri klasörleri"
}

// Ownership the step maintains: "path" -> "user:group mode" as printed by stat.
func (s panelConfigStep) owners() map[string]string {
	return map[string]string{
		dockerConfigDir:    "root:root 700",
		configDir:          "root:" + panelUser + " 750",
		config.Path:        "root:" + panelUser + " 640",
		panelStateDir:      "root:root 755",
		config.WebStateDir: panelUser + ":" + panelUser + " 700",
	}
}

func (s panelConfigStep) Check(ctx context.Context) (bool, error) {
	want, err := s.cfg.Marshal()
	if err != nil {
		return false, err
	}
	ok, err := s.h.FileEquals(config.Path, want)
	if err != nil || !ok {
		return false, err
	}
	for path, owner := range s.owners() {
		out, err := s.h.Run.Run(ctx, "stat", "-c", "%U:%G %a", path)
		if err != nil || strings.TrimSpace(out) != owner {
			return false, nil
		}
	}
	return true, nil
}

func (s panelConfigStep) Apply(ctx context.Context) error {
	if err := s.cfg.Validate(); err != nil {
		return err
	}
	want, err := s.cfg.Marshal()
	if err != nil {
		return err
	}
	for _, d := range []string{panelStateDir, config.WebStateDir, dockerConfigDir} {
		if err := os.MkdirAll(s.h.Path(d), 0o755); err != nil {
			return err
		}
	}
	if err := s.h.WriteFile(config.Path, want, 0o640); err != nil {
		return err
	}
	for path, owner := range s.owners() {
		f := strings.Fields(owner)
		for _, c := range [][]string{{"chown", f[0], path}, {"chmod", f[1], path}} {
			if _, err := s.h.Run.Run(ctx, c[0], c[1:]...); err != nil {
				return err
			}
		}
	}
	// The web process reads its config at start; a running one must pick up the change.
	// On a first install it does not exist yet and try-restart is a no-op.
	_, _ = s.h.Run.Run(ctx, "systemctl", "try-restart", "footops-web")
	return nil
}

// ---- panel account ----

// TOTPSetupFunc shows the TOTP secret to the operator and returns once they have proven,
// by typing a current code, that their authenticator app has it.
type TOTPSetupFunc func(ctx context.Context, secret, uri string) error

type panelAccountStep struct {
	h      Host
	user   string
	issuer string
	notes  *Notes
	setup  TOTPSetupFunc
}

func (s panelAccountStep) Name() string { return "panel-account" }
func (s panelAccountStep) Describe() string {
	return "Panel hesabı '" + s.user + "': şifre, iki adımlı doğrulama (QR), kurtarma kodları"
}

func (s panelAccountStep) Check(ctx context.Context) (bool, error) {
	_, err := auth.NewStore(s.h.Path(config.AuthPath)).Load()
	if errors.Is(err, auth.ErrNoAccount) {
		return false, nil
	}
	return err == nil, err
}

func (s panelAccountStep) Apply(ctx context.Context) error {
	pw, err := auth.NewPassword()
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return err
	}
	if err := s.setup(ctx, secret, auth.TOTPURI(secret, s.issuer, s.user)); err != nil {
		return err
	}
	codes, hashes, err := auth.NewRecoveryCodes(10)
	if err != nil {
		return err
	}
	acct := auth.Account{Username: s.user, PasswordHash: hash, TOTPSecret: secret, RecoveryCodes: hashes}
	if err := saveAccount(ctx, s.h, acct); err != nil {
		return err
	}
	s.notes.Add("Panel girişi — kullanıcı: %s  şifre: %s", s.user, pw)
	s.notes.Add("Kurtarma kodları (her biri bir kez kullanılır, 2FA telefonun kaybolursa):\n   %s", strings.Join(codes, "  "))
	return nil
}

// saveAccount writes the account file and hands it to the panel user, which needs to
// update it (failed-login counter, used codes).
func saveAccount(ctx context.Context, h Host, a auth.Account) error {
	// The directory belongs to the panel user (panel-config creates it); creating it
	// here as root would give it the wrong owner.
	if !h.Exists(config.WebStateDir) {
		return fmt.Errorf("%s yok; önce kurulumu tamamla (sudo footops install)", config.WebStateDir)
	}
	if err := auth.NewStore(h.Path(config.AuthPath)).Save(a); err != nil {
		return err
	}
	_, err := h.Run.Run(ctx, "chown", panelUser+":"+panelUser, config.AuthPath)
	return err
}

// ---- systemd services ----

// The agent runs as root (it drives Docker) but only exposes the fixed API in package
// agent; the web process runs as an unprivileged user under a tight sandbox.
const agentUnit = `# Managed by FootOps
[Unit]
Description=FootOps agent (privileged actions for the panel)
After=docker.service
Wants=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/footops agent
User=root
Group=footops
# docker CLI keeps its config (and later registry logins) here; /root is hidden by ProtectHome.
Environment=DOCKER_CONFIG=/var/lib/footops/docker
RuntimeDirectory=footops
RuntimeDirectoryMode=0750
UMask=0077
Restart=on-failure
RestartSec=3
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native

[Install]
WantedBy=multi-user.target
`

const webUnit = `# Managed by FootOps
[Unit]
Description=FootOps web panel
After=network-online.target footops-agent.service
Wants=network-online.target footops-agent.service

[Service]
Type=simple
ExecStart=/usr/local/bin/footops web
User=footops
Group=footops
UMask=0077
Restart=on-failure
RestartSec=3
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/footops/web
# Defence in depth: even if the web process were compromised it cannot read site and
# database files, secrets, Docker's data or the audit log (it asks the agent instead).
InaccessiblePaths=-/srv -/etc/footops/secrets -/var/lib/docker -/var/lib/containerd -/var/lib/footops/audit.jsonl -/var/lib/footops/audit.jsonl.1 -/var/run/docker.sock
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
RestrictSUIDSGID=yes
RestrictNamespaces=yes
RestrictRealtime=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM
MemoryMax=256M

[Install]
WantedBy=multi-user.target
`

type panelServicesStep struct{ h Host }

func (s panelServicesStep) Name() string { return "panel-services" }
func (s panelServicesStep) Describe() string {
	return "systemd servisleri: footops-agent (root, sabit işler), footops-web (yetkisiz, sandbox)"
}

func (s panelServicesStep) Check(ctx context.Context) (bool, error) {
	for path, want := range map[string]string{agentUnitPath: agentUnit, webUnitPath: webUnit} {
		ok, err := s.h.FileEquals(path, want)
		if err != nil || !ok {
			return false, err
		}
	}
	return serviceActive(ctx, s.h, "footops-agent") && serviceActive(ctx, s.h, "footops-web"), nil
}

func (s panelServicesStep) Apply(ctx context.Context) error {
	for path, content := range map[string]string{agentUnitPath: agentUnit, webUnitPath: webUnit} {
		if err := s.h.WriteFile(path, content, 0o644); err != nil {
			return err
		}
	}
	for _, c := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "footops-agent", "footops-web"},
		// restart, not start: picks up changed unit files and binaries.
		{"systemctl", "restart", "footops-agent", "footops-web"},
	} {
		if _, err := s.h.Run.Run(ctx, c[0], c[1:]...); err != nil {
			return err
		}
	}
	return waitFor(ctx, 20, 500*time.Millisecond, func() bool {
		return serviceActive(ctx, s.h, "footops-agent") && serviceActive(ctx, s.h, "footops-web")
	})
}

// ---- recovery commands (run as root over SSH) ----

// ResetPassword gives the panel account a new password and lifts any lockout. If there is
// no account yet, it creates one, including 2FA enrolment.
func ResetPassword(ctx context.Context, h Host, p *Prompter) error {
	if os.Geteuid() != 0 {
		return errors.New("root olarak çalıştırılmalı: sudo footops reset-password")
	}
	store := auth.NewStore(h.Path(config.AuthPath))
	acct, err := store.Load()
	if errors.Is(err, auth.ErrNoAccount) {
		return createAccount(ctx, h, p)
	}
	if err != nil {
		return err
	}
	pw, err := auth.NewPassword()
	if err != nil {
		return err
	}
	if acct.PasswordHash, err = auth.HashPassword(pw); err != nil {
		return err
	}
	acct.FailedLogins, acct.LockedUntil = 0, time.Time{}
	if err := saveAccount(ctx, h, acct); err != nil {
		return err
	}
	recordReset(h, "auth.reset-password")
	p.Say("Yeni panel şifresi (%s): %s", acct.Username, pw)
	p.Say("Vaultwarden'a kaydet; bir daha gösterilmeyecek. Açık oturumlar etkilenmez.")
	return nil
}

// Reset2FA enrols a new authenticator and issues new recovery codes; the old secret and
// codes stop working.
func Reset2FA(ctx context.Context, h Host, p *Prompter) error {
	if os.Geteuid() != 0 {
		return errors.New("root olarak çalıştırılmalı: sudo footops reset-2fa")
	}
	store := auth.NewStore(h.Path(config.AuthPath))
	acct, err := store.Load()
	if err != nil {
		return err
	}
	secret, err := auth.NewTOTPSecret()
	if err != nil {
		return err
	}
	if err := enrolTOTP(p)(ctx, secret, auth.TOTPURI(secret, issuer(h), acct.Username)); err != nil {
		return err
	}
	codes, hashes, err := auth.NewRecoveryCodes(10)
	if err != nil {
		return err
	}
	acct.TOTPSecret, acct.LastTOTPStep, acct.RecoveryCodes = secret, 0, hashes
	acct.FailedLogins, acct.LockedUntil = 0, time.Time{}
	if err := saveAccount(ctx, h, acct); err != nil {
		return err
	}
	recordReset(h, "auth.reset-2fa")
	p.Say("Yeni kurtarma kodları (her biri bir kez kullanılır):")
	p.Say("   %s", strings.Join(codes, "  "))
	return nil
}

func createAccount(ctx context.Context, h Host, p *Prompter) error {
	user, err := p.Ask("Panel kullanıcı adı", "", ValidateUser)
	if err != nil {
		return err
	}
	notes := &Notes{}
	step := panelAccountStep{h, user, issuer(h), notes, enrolTOTP(p)}
	if err := step.Apply(ctx); err != nil {
		return err
	}
	recordReset(h, "auth.create-account")
	for _, l := range notes.Lines() {
		p.Say("%s", l)
	}
	return nil
}

func issuer(h Host) string {
	if cfg, err := config.Load(h.Path(config.Path)); err == nil {
		return "FootOps " + cfg.ServerCode
	}
	return "FootOps"
}

// recordReset writes to the agent's audit log directly: the command runs as root on the
// server, so it does not need the agent to be running.
func recordReset(h Host, action string) {
	_ = agent.NewAudit(h.Path(agent.DefaultAuditPath)).Record(agent.AuditEntry{
		Actor: agent.Actor{User: "root", IP: "local", Via: "cli"}, Action: action, OK: true,
	})
}
