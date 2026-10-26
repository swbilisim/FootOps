package install

import (
	"os"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/core"
)

// gpg --show-keys --with-colons --with-fingerprint output for Docker's key.
const dockerKeyColons = `pub:-:4096:1:8D81803C0EBFCD88:1487788586:::-:::scESA::::::23::0:
fpr:::::::::9DC858229FC7DD38854AE2D88D81803C0EBFCD88:
uid:-::::1487792064::B5A08F01796E7F521861B449372D2C79B4BF2CE4::Docker Release (CE deb) <docker@docker.com>::::::::::0:
sub:-:4096:1:7EA0A9C3F273FCD8:1487788586::::::s::::::23:
fpr:::::::::D3306A018370199E527AE7997EA0A9C3F273FCD8:
`

const ubuntuNoble = "PRETTY_NAME=\"Ubuntu 24.04.3 LTS\"\nID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\nUBUNTU_CODENAME=noble\n"

func TestCheckKeyFingerprint(t *testing.T) {
	if err := checkKeyFingerprint(dockerKeyColons, dockerKeyFingerprint); err != nil {
		t.Errorf("Docker's key rejected: %v", err)
	}
	other := strings.Replace(dockerKeyColons, "9DC858229FC7DD38854AE2D88D81803C0EBFCD88", "0000000000000000000000000000000000000000", 1)
	if checkKeyFingerprint(other, dockerKeyFingerprint) == nil {
		t.Error("foreign key accepted")
	}
	// A key whose *subkey* has the wanted fingerprint is still a foreign key.
	sneaky := strings.Replace(other, "D3306A018370199E527AE7997EA0A9C3F273FCD8", dockerKeyFingerprint, 1)
	if checkKeyFingerprint(sneaky, dockerKeyFingerprint) == nil {
		t.Error("subkey fingerprint accepted as primary")
	}
	if checkKeyFingerprint(dockerKeyColons+dockerKeyColons, dockerKeyFingerprint) == nil {
		t.Error("file with two keys accepted")
	}
	if checkKeyFingerprint("", dockerKeyFingerprint) == nil {
		t.Error("empty output accepted")
	}
}

func dockerRepoHost(t *testing.T, colons string) (Host, *fakeRunner) {
	var h Host
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "curl "):
			put(t, h, dockerKeyDownload, "-----BEGIN PGP PUBLIC KEY BLOCK-----\n...\n")
		case strings.HasPrefix(cmd, "gpg "):
			return colons, nil
		}
		return "", nil
	})
	put(t, h, "/etc/os-release", ubuntuNoble)
	return h, f
}

func TestDockerRepoInstallsVerifiedKey(t *testing.T) {
	h, f := dockerRepoHost(t, dockerKeyColons)
	s := dockerRepoStep{h}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	src := read(t, h, dockerSourcesPath)
	if !strings.Contains(src, "Suites: noble") || !strings.Contains(src, "Signed-By: "+dockerKeyPath) {
		t.Errorf("sources = %s", src)
	}
	if !h.Exists(dockerKeyPath) || h.Exists(dockerKeyDownload) {
		t.Error("key not installed or download not cleaned up")
	}
	if !f.ran("curl -fsSL --proto =https --tlsv1.2") {
		t.Errorf("key not fetched over https only: %v", f.cmds())
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestDockerRepoRejectsForeignKey(t *testing.T) {
	h, _ := dockerRepoHost(t, strings.Replace(dockerKeyColons, "9DC8", "0000", 1))
	if err := (dockerRepoStep{h}).Apply(ctx); err == nil {
		t.Fatal("foreign key accepted")
	}
	if h.Exists(dockerKeyPath) || h.Exists(dockerSourcesPath) || h.Exists(dockerKeyDownload) {
		t.Error("files left behind after rejecting the key")
	}
}

func TestDockerRefusesConflictingPackages(t *testing.T) {
	h, f := newTestHost(t, func(cmd string) (string, error) {
		if cmd == "dpkg-query -W -f=${Status} docker.io" {
			return "install ok installed", nil
		}
		return "", errExit
	})
	err := (dockerStep{h}).Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "docker.io") {
		t.Fatalf("err = %v", err)
	}
	if f.ran("apt-get") {
		t.Error("apt ran despite conflicting packages")
	}
}

func TestDockerInstall(t *testing.T) {
	h, f := newTestHost(t, func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "dpkg-query") {
			return "", errExit // nothing installed, no conflicts
		}
		return "", nil
	})
	if err := (dockerStep{h}).Apply(ctx); err != nil {
		t.Fatal(err)
	}
	var install string
	for _, c := range f.cmds() {
		if strings.Contains(c, " install -y") {
			install = c
		}
	}
	for _, p := range dockerPackages {
		if !strings.Contains(install, p) {
			t.Errorf("install misses %s: %q", p, install)
		}
	}
	if !f.ran("systemctl enable --now containerd docker") {
		t.Error("docker not enabled")
	}
}

func TestDockerDaemonRestoresInvalidConfig(t *testing.T) {
	h, f := newTestHost(t, func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "dockerd --validate") {
			return "unable to configure the Docker daemon", errExit
		}
		return "", nil
	})
	put(t, h, dockerDaemonPath, `{"debug": true}`)
	if err := (dockerDaemonStep{h}).Apply(ctx); err == nil {
		t.Fatal("expected error")
	}
	if read(t, h, dockerDaemonPath) != `{"debug": true}` {
		t.Error("previous daemon.json not restored")
	}
	if f.ran("systemctl restart docker") {
		t.Error("docker restarted with an invalid config")
	}
}

func TestDockerDaemonApplies(t *testing.T) {
	noSleep(t)
	infoCalls := 0
	h, f := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "docker info"):
			infoCalls++
			if infoCalls < 3 {
				return "Cannot connect to the Docker daemon", errExit
			}
		case cmd == "systemctl is-active docker":
			return "active\n", nil
		}
		return "", nil
	})
	s := dockerDaemonStep{h}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if !f.ran("systemctl restart docker") || infoCalls != 3 {
		t.Errorf("restart/wait wrong: info calls %d, cmds %v", infoCalls, f.cmds())
	}
	for _, want := range []string{`"log-driver": "local"`, `"live-restore": true`, `"no-new-privileges": true`, `"10.200.0.0/16"`} {
		if !strings.Contains(read(t, h, dockerDaemonPath), want) {
			t.Errorf("daemon.json misses %s", want)
		}
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}
}

func TestCoreFilesGeneratesSecretOnce(t *testing.T) {
	h, _ := newTestHost(t, nil)
	notes := &Notes{}
	s := coreFilesStep{h, core.Config{MongoCacheGB: 0.5}, notes}
	if done, _ := s.Check(ctx); done {
		t.Fatal("done before apply")
	}
	if err := s.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	env := read(t, h, core.MongoSecretPath)
	pw := core.MongoPasswordFromEnv(env)
	if len(pw) != 32 {
		t.Fatalf("password length = %d", len(pw))
	}
	if len(notes.Lines()) != 1 || !strings.Contains(notes.Lines()[0], pw) {
		t.Errorf("password not reported once: %v", notes.Lines())
	}
	st, err := os.Stat(h.Path(core.MongoSecretPath))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("secret mode = %v, %v", st.Mode().Perm(), err)
	}
	for _, d := range []string{core.MongoDataDir, core.CaddyCertsDir, configDir, "/etc/footops/secrets"} {
		st, err := os.Stat(h.Path(d))
		if err != nil || st.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v, %v", d, st.Mode().Perm(), err)
		}
	}
	if done, _ := s.Check(ctx); !done {
		t.Error("not done after apply")
	}

	// A second run, e.g. after changing the cache size, keeps the password.
	notes2 := &Notes{}
	s2 := coreFilesStep{h, core.Config{MongoCacheGB: 1}, notes2}
	if done, _ := s2.Check(ctx); done {
		t.Fatal("changed cache size not detected")
	}
	if err := s2.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if core.MongoPasswordFromEnv(read(t, h, core.MongoSecretPath)) != pw {
		t.Error("password regenerated")
	}
	if len(notes2.Lines()) != 0 {
		t.Error("existing password reported again")
	}
}

func TestCoreFilesRefusesNewSecretForExistingData(t *testing.T) {
	h, _ := newTestHost(t, nil)
	put(t, h, core.MongoDataDir+"/WiredTiger", "data")
	err := (coreFilesStep{h, core.Config{MongoCacheGB: 0.5}, &Notes{}}).Apply(ctx)
	if err == nil {
		t.Fatal("generated a password that cannot match the existing database")
	}
	if h.Exists(core.MongoSecretPath) {
		t.Error("secret written anyway")
	}
}

func TestRandomPassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := randomPassword(32)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 32 || strings.Trim(pw, passwordAlphabet) != "" {
			t.Fatalf("bad password %q", pw)
		}
		if seen[pw] {
			t.Fatal("repeated password")
		}
		seen[pw] = true
	}
}

func TestContainerUpToDate(t *testing.T) {
	cases := []struct {
		inspect string
		want    bool
	}{
		{"abc true healthy", true},
		{"abc true", true}, // no healthcheck defined
		{"abc true starting", false},
		{"abc true unhealthy", false},
		{"abc false", false},
		{"old true healthy", false},
		{"", false},
	}
	for _, c := range cases {
		if got := containerUpToDate(c.inspect, "abc"); got != c.want {
			t.Errorf("containerUpToDate(%q) = %v", c.inspect, got)
		}
	}
}

func TestCoreStackCheck(t *testing.T) {
	inspect := map[string]string{
		"footops-caddy": "h1 true healthy",
		"footops-mongo": "h2 true healthy",
	}
	h, _ := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "config --hash *"):
			return "caddy h1\nmongo h2\n", nil
		case strings.HasPrefix(cmd, "docker inspect"):
			name := cmd[strings.LastIndex(cmd, " ")+1:]
			if v, ok := inspect[name]; ok {
				return v, nil
			}
			return "", errExit
		}
		return "", nil
	})
	s := coreStackStep{h}
	if done, _ := s.Check(ctx); !done {
		t.Error("healthy, up-to-date stack reported as pending")
	}
	inspect["footops-mongo"] = "stale true healthy"
	if done, _ := s.Check(ctx); done {
		t.Error("changed compose config not detected")
	}
	delete(inspect, "footops-mongo")
	if done, _ := s.Check(ctx); done {
		t.Error("missing container not detected")
	}
}
