package install

import (
	"context"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/doctor"
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
