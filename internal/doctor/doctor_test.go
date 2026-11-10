package doctor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
)

// fakeRun answers commands by their full text; anything else fails.
type fakeRun map[string]string

func (f fakeRun) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	out, ok := f[cmd]
	if !ok {
		return "", errors.New("unexpected: " + cmd)
	}
	if strings.HasPrefix(out, "ERR ") {
		return strings.TrimPrefix(out, "ERR "), errors.New("exit status 3")
	}
	return out, nil
}

func (f fakeRun) RunInput(ctx context.Context, _ string, name string, args ...string) (string, error) {
	return f.Run(ctx, name, args...)
}

type fakeDocker struct {
	containers []docker.Container
	usage      docker.ImageUsage
	err        error
}

func (d fakeDocker) Containers(context.Context) ([]docker.Container, error) {
	return d.containers, d.err
}
func (d fakeDocker) Version(context.Context) (docker.Version, error) {
	return docker.Version{Version: "29.9.3"}, d.err
}
func (d fakeDocker) ImageUsage(context.Context) (docker.ImageUsage, error) { return d.usage, d.err }

var now = time.Date(2026, 11, 11, 8, 30, 0, 0, time.UTC)

// touch writes a file under root aged age.
func touch(t *testing.T, root, p, content string, age time.Duration) {
	t.Helper()
	full := filepath.Join(root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(full, now.Add(-age), now.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

const ssClean = `udp UNCONN 0 0 127.0.0.54:53 0.0.0.0:* users:(("systemd-resolve",pid=1,fd=1))
udp UNCONN 0 0 203.0.113.10%eth0:68 0.0.0.0:* users:(("systemd-network",pid=2,fd=2))
udp UNCONN 0 0 *:60336 *:* users:(("cloudflared",pid=3,fd=3))
tcp LISTEN 0 4096 0.0.0.0:443 0.0.0.0:* users:(("docker-proxy",pid=4,fd=4))
tcp LISTEN 0 4096 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=5,fd=5))
tcp LISTEN 0 4096 127.0.0.1:7701 0.0.0.0:* users:(("footops",pid=6,fd=6))
tcp LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=1,fd=7))
tcp LISTEN 0 4096 [::]:443 [::]:* users:(("docker-proxy",pid=4,fd=8))
tcp LISTEN 0 4096 [::1]:6010 [::]:* users:(("sshd",pid=9,fd=9))
`

func cleanRun() fakeRun {
	return fakeRun{
		"apt-get -s -o APT::Get::Never-Include-Phased-Updates=true --with-new-pkgs upgrade": "Reading package lists...\n0 upgraded, 0 newly installed\n",
		"systemctl is-active apt-daily-upgrade.timer":                                       "active\n",
		"timedatectl show -p NTPSynchronized --value":                                       "yes\n",
		"ss -H -tulnp": ssClean,
	}
}

func coreContainers() []docker.Container {
	return []docker.Container{
		{Names: []string{"/footops-caddy"}, Image: "caddy:2.11.7-alpine", State: "running",
			Ports: []docker.Port{{IP: "0.0.0.0", PrivatePort: 443, PublicPort: 443, Type: "tcp"}, {IP: "::", PrivatePort: 443, PublicPort: 443, Type: "tcp"}}},
		{Names: []string{"/footops-mongo"}, Image: "mongo:7.0.45", State: "running",
			Ports: []docker.Port{{IP: "127.0.0.1", PrivatePort: 27017, PublicPort: 27017, Type: "tcp"}}},
		{Names: []string{"/footops-cloudflared"}, Image: "cloudflare/cloudflared:2026.10.3", State: "running"},
		{Names: []string{"/swbilisim"}, Image: "ghcr.io/x/swbilisim:main", State: "running",
			Ports: []docker.Port{{PrivatePort: 3000, Type: "tcp"}}},
	}
}

func baseEnv(t *testing.T) Env {
	root := t.TempDir()
	touch(t, root, uuStamp, "", 3*time.Hour)
	touch(t, root, updateStamp, "", 3*time.Hour)
	return Env{Root: root, Run: cleanRun(), Now: func() time.Time { return now }, Host: "h1", FootOps: "v0.1.0",
		AutoReboot: true, RebootTime: "04:00",
		Docker:     fakeDocker{containers: coreContainers()},
		KeepImages: []string{"caddy:2.11.7-alpine", "restic/restic:0.19.1@sha256:1366"},
		Standard: func(context.Context) []Finding {
			return []Finding{{ID: "step:swap", Section: "standard", Level: OK, Title: "Takas alanı (swap)"},
				{ID: "step:firewall", Section: "standard", Level: OK, Title: "Güvenlik duvarı (UFW)"}}
		},
		Accounts: func() []Finding { return nil },
	}
}

func byID(r Report) map[string]Finding {
	m := map[string]Finding{}
	for _, f := range r.Findings {
		m[f.ID] = f
	}
	return m
}

func TestCleanServer(t *testing.T) {
	r := Run(context.Background(), baseEnv(t))
	if p := r.Problems(); len(p) != 0 {
		t.Fatalf("problems on a clean server: %+v", p)
	}
	m := byID(r)
	for id, want := range map[string]string{
		"reboot":      "Yeniden başlatma gerekmiyor",
		"updates":     "Bekleyen paket güncellemesi yok",
		"unattended":  "Otomatik güvenlik güncellemeleri çalışıyor (son: 3 saat önce)",
		"time":        "Saat senkron (NTP)",
		"ports":       "Dışarıya açık yalnız 22 (SSH) ve 443 (HTTPS)",
		"images":      "Kullanılmayan imaj yok",
		"accounts":    "Yönetici hesabı, bulut hesapları ve sudo kuralları standartta",
		"docker":      "Docker 29.9.3",
		"caddy":       "Caddy 2.11.7-alpine",
		"mongo":       "MongoDB 7.0.45",
		"cloudflared": "cloudflared 2026.10.3 (Cloudflare desteği bitişi: 2027-10)",
		"footops":     "FootOps v0.1.0",
	} {
		if m[id].Title != want || m[id].Level != OK {
			t.Errorf("%s = %q (%s), want %q", id, m[id].Title, m[id].Level, want)
		}
	}
	if r.Format != Format || r.Host != "h1" || !r.Time.Equal(now) {
		t.Errorf("report head = %+v", r)
	}
}

func TestReboot(t *testing.T) {
	env := baseEnv(t)
	touch(t, env.Root, rebootFile, "*** System restart required ***\n", 5*time.Hour)
	touch(t, env.Root, rebootPkgs, "linux-image-6.8.0-150-generic\nlinux-base\nlinux-base\n", 5*time.Hour)
	f := byID(Run(context.Background(), env))["reboot"]
	if f.Level != Info || !strings.Contains(f.Title, "kendiliğinden") || !strings.Contains(f.Detail, "5 saat önce istendi (linux-base, linux-image-6.8.0-150-generic)") {
		t.Errorf("fresh flag with automatic reboot = %+v", f)
	}
	touch(t, env.Root, rebootFile, "", 3*24*time.Hour)
	f = byID(Run(context.Background(), env))["reboot"]
	if f.Level != Warn || !strings.Contains(f.Detail, "3 gün önce") || !strings.Contains(f.Detail, "gerçekleşmemiş") || f.Fix == "" {
		t.Errorf("old flag = %+v", f)
	}
	env.AutoReboot = false
	touch(t, env.Root, rebootFile, "", time.Hour)
	if f := byID(Run(context.Background(), env))["reboot"]; f.Level != Warn || !strings.Contains(f.Detail, "kapalı") {
		t.Errorf("no automatic reboot = %+v", f)
	}
}

const aptPending = `NOTE: This is only a simulation!
Inst docker-ce [5:29.9.3-1~ubuntu.24.04~noble] (5:29.9.5-1~ubuntu.24.04~noble Docker CE:noble [amd64])
Inst containerd.io [2.1.5-1] (2.1.6-1 Docker CE:noble [amd64])
Inst linux-image-6.8.0-150-generic (6.8.0-150.150 Ubuntu:24.04/noble-updates, Ubuntu:24.04/noble-security [amd64])
Conf docker-ce (5:29.9.5-1~ubuntu.24.04~noble Docker CE:noble [amd64])
`

func TestUpdates(t *testing.T) {
	ps := parseInst(aptPending)
	if len(ps) != 3 || ps[0].Name != "docker-ce" || ps[0].To != "5:29.9.5-1~ubuntu.24.04~noble" || ps[0].Security || !ps[2].Security {
		t.Fatalf("parsed = %+v", ps)
	}
	env := baseEnv(t)
	env.Run.(fakeRun)["apt-get -s -o APT::Get::Never-Include-Phased-Updates=true --with-new-pkgs upgrade"] = aptPending
	m := byID(Run(context.Background(), env))
	f := m["updates"]
	if f.Level != Info || f.Title != "3 paket güncellemesi bekliyor (1 güvenlik)" ||
		!strings.Contains(f.Detail, "Diğer: docker-ce, containerd.io") || !strings.Contains(f.Fix, "Docker güncellenirken") {
		t.Errorf("updates = %+v", f)
	}
	if d := m["docker"]; d.Level != Info || d.Title != "Docker 29.9.3 — 29.9.5 çıkmış" {
		t.Errorf("docker = %+v", d)
	}
	// A security update waits and unattended-upgrades has not run for days.
	touch(t, env.Root, uuStamp, "", 4*24*time.Hour)
	m = byID(Run(context.Background(), env))
	if f := m["updates"]; f.Level != Warn || !strings.Contains(f.Detail, "2 günden uzun") {
		t.Errorf("late security update = %+v", f)
	}
	if f := m["unattended"]; f.Level != Warn || f.Title != "Otomatik güvenlik güncellemeleri 4 gün önce çalıştı" {
		t.Errorf("unattended = %+v", f)
	}
	env.Run.(fakeRun)["systemctl is-active apt-daily-upgrade.timer"] = "ERR inactive\n"
	if f := byID(Run(context.Background(), env))["unattended"]; f.Level != Warn || f.Detail != "apt-daily-upgrade.timer etkin değil (inactive)." {
		t.Errorf("timer off = %+v", f)
	}
}

func TestPorts(t *testing.T) {
	ls := parseSS(ssClean)
	if len(ls) != 9 || ls[2].proc != "cloudflared" || ls[2].port != 60336 || ls[1].host != "203.0.113.10%eth0" {
		t.Fatalf("parsed = %+v", ls)
	}
	for host, want := range map[string]bool{"127.0.0.53%lo": true, "[::1]": true, "127.0.0.1": true,
		"0.0.0.0": false, "[::]": false, "*": false, "[fe80::1]%eth0": false, "203.0.113.10%eth0": false} {
		if loopback(host) != want {
			t.Errorf("loopback(%q) = %v", host, !want)
		}
	}
	env := baseEnv(t)
	env.Run.(fakeRun)["ss -H -tulnp"] = ssClean + "tcp LISTEN 0 511 0.0.0.0:8080 0.0.0.0:* users:((\"node\",pid=10,fd=10))\nudp UNCONN 0 0 0.0.0.0:111 0.0.0.0:* users:((\"rpcbind\",pid=11,fd=11))\n"
	f := byID(Run(context.Background(), env))["ports"]
	if f.Level != Warn || f.Title != "Beklenmeyen dinleyen port: tcp 8080 (node), udp 111 (rpcbind)" {
		t.Errorf("host listeners = %+v", f)
	}
	// The same port published by Docker is past UFW: serious, said once.
	cs := append(coreContainers(), docker.Container{Names: []string{"/elle"}, State: "running",
		Ports: []docker.Port{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}, {IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}}})
	env.Docker = fakeDocker{containers: cs}
	f = byID(Run(context.Background(), env))["ports"]
	if f.Level != Bad || f.Title != "Docker dışarıya port açmış: tcp 8080 → elle" || !strings.Contains(f.Detail, "Ayrıca dinleyen: udp 111 (rpcbind)") {
		t.Errorf("published = %+v", f)
	}
}

func TestTimeSync(t *testing.T) {
	env := baseEnv(t)
	env.Run.(fakeRun)["timedatectl show -p NTPSynchronized --value"] = "no\n"
	if f := byID(Run(context.Background(), env))["time"]; f.Level != Warn {
		t.Errorf("unsynchronised = %+v", f)
	}
	delete(env.Run.(fakeRun), "timedatectl show -p NTPSynchronized --value")
	if f := byID(Run(context.Background(), env))["time"]; f.Level != Info {
		t.Errorf("unknown = %+v", f)
	}
}

func TestImages(t *testing.T) {
	env := baseEnv(t)
	env.Docker = fakeDocker{containers: coreContainers(), usage: docker.ImageUsage{Images: []docker.ImageEntry{
		{ID: "sha256:aa", RepoTags: []string{"caddy:2.11.7-alpine"}, Size: 50 << 20, Containers: 1},
		{ID: "sha256:bb", RepoTags: []string{"caddy:2.11.6-alpine"}, Size: 50 << 20, SharedSize: 40 << 20},
		{ID: "sha256:cc", RepoDigests: []string{"restic/restic@sha256:1366"}, Size: 30 << 20},
		{ID: "sha256:dd0123456789abcdef", RepoTags: []string{"<none>:<none>"}, Size: 2 << 30, SharedSize: 1 << 30},
		{ID: "sha256:ee", RepoTags: []string{"x:1"}, Size: 10, Containers: -1},
	}}}
	f := byID(Run(context.Background(), env))["images"]
	if f.Level != Info || f.Title != "2 imajı hiçbir container kullanmıyor (1,0 GB ile 2,0 GB arası)" ||
		f.Detail != "caddy:2.11.6-alpine, etiketsiz dd0123456789." {
		t.Errorf("images = %+v", f)
	}
	env.Docker = fakeDocker{err: errors.New("bağlanılamadı")}
	m := byID(Run(context.Background(), env))
	if m["images"].Level != Info || !strings.Contains(m["ports"].Detail, "Docker'ın yayımladığı portlar denetlenemedi") || m["caddy"].ID != "" {
		t.Errorf("docker down: images %+v, ports %+v, caddy %+v", m["images"], m["ports"], m["caddy"])
	}
}

func TestCloudflaredSupport(t *testing.T) {
	for tag, want := range map[string]string{
		"2026.10.3": OK,   // until 2027-10
		"2025.12.1": Warn, // 2026-12: 20 days left
		"2025.10.0": Bad,  // ended 2026-10
		"latest":    Info,
	} {
		if f := cloudflared(tag, now); f.Level != want {
			t.Errorf("%s = %+v, want %s", tag, f, want)
		}
	}
}

func TestRender(t *testing.T) {
	env := baseEnv(t)
	env.Run.(fakeRun)["apt-get -s -o APT::Get::Never-Include-Phased-Updates=true --with-new-pkgs upgrade"] = aptPending
	env.Standard = func(context.Context) []Finding {
		return []Finding{{ID: "step:swap", Section: "standard", Level: OK, Title: "Takas alanı (swap)"},
			{ID: "step:hostname", Section: "standard", Level: OK, Title: "Sunucu adı"},
			{ID: "step:ssh-hardening", Section: "standard", Level: Bad, Title: "Standarttan farklı: SSH ayarları",
				Detail: "Kurulumun bu adımı: …", Fix: "sudo footops install"}}
	}
	r := Run(context.Background(), env)
	var b bytes.Buffer
	Render(&b, r)
	out := b.String()
	for _, want := range []string{
		"FootOps kontrolü — h1 · footops v0.1.0",
		"Kurulum standardı\n  ✗ Standarttan farklı: SSH ayarları\n      Kurulumun bu adımı: …\n      → sudo footops install\n  ✓ 2 adım standartta\n",
		"  • 3 paket güncellemesi bekliyor (1 güvenlik)",
		"Özet: 1 ciddi, 0 uyarı, 2 bilgi",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("render lacks %q:\n%s", want, out)
		}
	}
	if p := r.Problems(); len(p) != 1 || p[0].ID != "step:ssh-hardening" {
		t.Errorf("problems = %+v", p)
	}
}

func TestBytesAndSpan(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 1536: "1,5 KB", 1 << 30: "1,0 GB", 1900 << 20: "1,9 GB"} {
		if got := bytesTR(n); got != want {
			t.Errorf("bytesTR(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{10 * time.Second: "1 dk", 90 * time.Minute: "1 saat", 47 * time.Hour: "47 saat", 50 * time.Hour: "2 gün"} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}
