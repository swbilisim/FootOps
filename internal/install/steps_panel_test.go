package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/core"
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
	if !f.ran("chown footops:footops " + config.AuthPath) {
		t.Error("account file not handed to the panel user")
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
	active := false
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl restart"):
			active = true
		case strings.HasPrefix(cmd, "systemctl is-active"):
			if active {
				return "active\n", nil
			}
			return "inactive\n", errExit
		}
		return "", nil
	})
	s := panelServicesStep{h}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable footops-agent footops-web", "systemctl restart footops-agent footops-web"} {
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
	for _, hidden := range []string{"-/srv", "-/etc/footops/secrets", "-/var/lib/docker", "-/var/run/docker.sock", "-/var/lib/footops/audit.jsonl"} {
		if !strings.Contains(webUnit, hidden) {
			t.Errorf("web unit does not hide %s", hidden)
		}
	}
	for _, want := range []string{"User=root", "Group=footops", "RuntimeDirectory=footops", "NoNewPrivileges=yes"} {
		if !strings.Contains(agentUnit, want) {
			t.Errorf("agent unit misses %q", want)
		}
	}
}
