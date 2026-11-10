package install

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/sites"
)

func TestDoctorStandard(t *testing.T) {
	h, _ := newTestHost(t, func(cmd string) (string, error) { return "", errExit })
	ctx := context.Background()
	byID := func(r doctor.Report) map[string]doctor.Finding {
		m := map[string]doctor.Finding{}
		for _, f := range r.Findings {
			m[f.ID] = f
		}
		return m
	}

	// No saved answers: the standard cannot be checked, and says so once.
	m := byID(Doctor(ctx, h, nil, "/nonexistent/footops"))
	if f := m["answers"]; f.Level != doctor.Warn || f.Fix != "sudo footops install" {
		t.Fatalf("without answers: %+v", f)
	}
	if _, ok := m["step:swap"]; ok {
		t.Error("steps checked without answers")
	}

	a := validAnswers()
	data, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	put(t, h, AnswersPath, data)
	put(t, h, "/etc/hostname", "h1\n")
	put(t, h, "/etc/hosts", withHostsEntry("127.0.0.1 localhost\n", "h1"))
	put(t, h, "/etc/passwd", "root:x:0:0:root:/root:/bin/bash\nfurkan:x:1000:1000::/home/furkan:/bin/bash\n")
	put(t, h, "/etc/group", "sudo:x:27:furkan\nfurkan:x:1000:\n")
	r := Doctor(ctx, h, nil, "/nonexistent/footops")
	m = byID(r)
	for _, name := range []string{"upgrade", "recover"} {
		if _, ok := m["step:"+name]; ok {
			t.Errorf("step %s checked", name)
		}
	}
	if f := m["step:hostname"]; f.Level != doctor.OK || f.Title != "Sunucu adı" {
		t.Errorf("hostname = %+v", f)
	}
	if f := m["step:ssh-hardening"]; f.Level != doctor.Bad || f.Title != "Standarttan farklı: SSH ayarları" ||
		!strings.HasPrefix(f.Detail, "Kurulumun bu adımı: ") || !strings.HasPrefix(f.Fix, "sudo footops install") {
		t.Errorf("ssh = %+v", f)
	}
	if f := m["step:timezone"]; f.Level != doctor.Warn {
		t.Errorf("timezone = %+v", f)
	}
	// The running binary cannot be read: the check's own error.
	if f := m["step:panel-binary"]; f.Level != doctor.Warn || !strings.HasPrefix(f.Title, "Denetlenemedi: FootOps programı") {
		t.Errorf("panel-binary = %+v", f)
	}
	if f := m["account:uid"]; f.Level != doctor.Warn || !strings.HasPrefix(f.Title, "Yönetici hesabı furkan uid 1000 / gid 1000 ile") ||
		!strings.Contains(f.Detail, "sudo usermod -u 2000 furkan") {
		t.Errorf("account = %+v", f)
	}
	for _, s := range Steps(h, a, Deps{}) {
		if s.Name() != "upgrade" && s.Name() != "recover" && stepTitles[s.Name()] == "" {
			t.Errorf("step %s has no title", s.Name())
		}
	}
	// The answers' reboot setting reaches the reboot check.
	put(t, h, "/var/run/reboot-required", "")
	if f := byID(Doctor(ctx, h, nil, "/nonexistent/footops"))["reboot"]; f.Level != doctor.Info || !strings.Contains(f.Title, "04:00") {
		t.Errorf("reboot = %+v", f)
	}
}

// layerDocker is the Docker the check reads the sites' layers from.
type layerDocker struct{ layers map[string][]docker.Change }

func (layerDocker) Containers(context.Context) ([]docker.Container, error) { return nil, nil }
func (layerDocker) Version(context.Context) (docker.Version, error)        { return docker.Version{}, nil }
func (layerDocker) ImageUsage(context.Context) (docker.ImageUsage, error) {
	return docker.ImageUsage{}, nil
}
func (layerDocker) Inspect(context.Context, string) (docker.Inspect, error) {
	return docker.Inspect{}, nil
}
func (layerDocker) PathStat(context.Context, string, string) (os.FileMode, error) {
	return 0o644, nil
}
func (d layerDocker) Changes(_ context.Context, name string) ([]docker.Change, error) {
	if c, ok := d.layers[name]; ok {
		return c, nil
	}
	return nil, &docker.APIError{Status: 404, Message: "no such container"}
}

// A site whose container wrote outside its persistent folder is a warning of its own;
// none such, one line that says so (Faz L).
func TestDoctorSites(t *testing.T) {
	h, _ := newTestHost(t, func(cmd string) (string, error) { return "", errExit })
	ctx := context.Background()
	st := sites.Store{Root: h.Root}
	for _, s := range []sites.Spec{
		{Name: "kuleonu", Group: "kuleonu", Domains: []string{"kuleonu.bel.tr"}, Image: "ghcr.io/x/kuleonu:main", Port: 3000, MemoryMB: 256},
		{Name: "enisparta", Group: "musteri", Domains: []string{"enisparta.com"}, Image: "nginx:1.27-alpine", Port: 80, MemoryMB: 64, DataMount: "/usr/share/nginx/html/up"},
	} {
		if err := os.MkdirAll(h.Path(s.Dir()), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := st.WriteMeta(s); err != nil {
			t.Fatal(err)
		}
	}
	d := layerDocker{layers: map[string][]docker.Change{
		"kuleonu": {{Path: "/app", Kind: docker.ChangeModified}, {Path: "/app/uploads", Kind: docker.ChangeAdded},
			{Path: "/app/uploads/a.jpg", Kind: docker.ChangeAdded}, {Path: "/app/uploads/b.jpg", Kind: docker.ChangeAdded}},
		"enisparta": {{Path: "/usr/share/nginx/html/up", Kind: docker.ChangeAdded}, {Path: "/run/nginx.pid", Kind: docker.ChangeAdded}},
	}}
	got := siteFindings(ctx, h, d)
	if len(got) != 1 || got[0].ID != "site-layer:kuleonu" || got[0].Level != doctor.Warn ||
		got[0].Title != "kuleonu container'ının içine yazıyor: kalıcı klasör dışında 2 dosya" || got[0].Detail != "/app/uploads — 2 dosya" ||
		!strings.Contains(got[0].Fix, "kalıcı klasör yap") {
		t.Errorf("findings: %+v", got)
	}
	delete(d.layers, "kuleonu")
	if got := siteFindings(ctx, h, d); len(got) != 1 || got[0].ID != "site-layers" || got[0].Level != doctor.OK {
		t.Errorf("none: %+v", got)
	}
	// It runs with the server check when Docker can read layers.
	if m := Doctor(ctx, h, d, "/nonexistent/footops"); !slices.ContainsFunc(m.Findings, func(f doctor.Finding) bool { return f.ID == "site-layers" }) {
		t.Errorf("not in the check: %+v", m.Findings)
	}
}
