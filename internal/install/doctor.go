package install

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/version"
)

// `footops doctor` (panel-tasarim §31.1): the standard is what the steps set up, so a
// step whose Check fails is a deviation from it — the same test the installer makes
// before it changes anything. upgrade is left to doctor's own look at the updates
// waiting, recover belongs to a recovery's first run.

// stepTitles name the steps for the operator.
var stepTitles = map[string]string{
	"packages":            "Temel paketler",
	"hostname":            "Sunucu adı",
	"timezone":            "Saat dilimi",
	"swap":                "Takas alanı (swap)",
	"unattended-upgrades": "Otomatik güvenlik güncellemelerinin ayarı",
	"admin-user":          "Yönetici hesabı ve SSH anahtarları",
	"firewall":            "Güvenlik duvarı (UFW)",
	"fail2ban":            "fail2ban",
	"ssh-hardening":       "SSH ayarları",
	"docker-repo":         "Docker deposu",
	"docker":              "Docker",
	"docker-daemon":       "Docker ayarları",
	"tunnel-secret":       "Cloudflare tüneli",
	"core-files":          "Çekirdek dosyalar (Caddy, MongoDB)",
	"core-stack":          "Çekirdek container'lar",
	"panel-user":          "Panel kullanıcısı",
	"panel-binary":        "FootOps programı",
	"panel-config":        "Panel ayarları",
	"panel-account":       "Panel hesabı",
	"panel-services":      "Panel servisleri",
}

// seriousSteps: a deviation there weakens the server's protection at once.
var seriousSteps = map[string]bool{"ssh-hardening": true, "firewall": true, "fail2ban": true}

// Doctor checks this server. self is the running footops binary (panel-binary compares
// it with the installed one).
func Doctor(ctx context.Context, h Host, d doctor.Docker, self string) doctor.Report {
	host, _ := os.Hostname()
	env := doctor.Env{Root: h.Root, Run: h.Run, Docker: d, Host: host, FootOps: version.Version,
		KeepImages: core.PinnedImages}
	if ld, ok := d.(agent.LayerDocker); ok {
		env.Sites = func(ctx context.Context) []doctor.Finding { return siteFindings(ctx, h, ld) }
	}
	if a, err := savedAnswers(h); err == nil {
		env.AutoReboot, env.RebootTime = a.AutoReboot, a.RebootTime
		env.Standard = func(ctx context.Context) []doctor.Finding { return standardFindings(ctx, h, a, self) }
		env.Accounts = func() []doctor.Finding { return accountFindings(h, a) }
	} else {
		env.Standard = func(context.Context) []doctor.Finding {
			return []doctor.Finding{{ID: "answers", Section: "standard", Level: doctor.Warn,
				Title: "Kurulum cevapları okunamadı: standart denetlenemedi", Detail: err.Error(), Fix: "sudo footops install"}}
		}
	}
	return doctor.Run(ctx, env)
}

// siteFindings are the sites whose containers wrote outside their persistent folders:
// what goes when a container is made anew (Faz L), one finding each so each warns alone.
func siteFindings(ctx context.Context, h Host, d agent.LayerDocker) []doctor.Finding {
	specs, err := sites.Store{Root: h.Root}.List()
	if err != nil {
		return []doctor.Finding{{ID: "site-layers", Section: "disk", Level: doctor.Warn,
			Title: "Siteler okunamadı: container'larının içi denetlenemedi", Detail: err.Error()}}
	}
	var out []doctor.Finding
	for _, s := range specs {
		r := agent.LayerOf(ctx, d, s)
		f := doctor.Finding{ID: "site-layer:" + s.Name, Section: "disk", Level: doctor.Warn}
		switch {
		case r.Err != "":
			f.Title, f.Detail = s.Name+": container'ın içi denetlenemedi", r.Err
		case r.Files > 0:
			f.Title = fmt.Sprintf("%s container'ının içine yazıyor: kalıcı klasör dışında %d dosya", s.Name, r.Files)
			f.Detail = strings.Join(r.Paths, ", ")
			if r.More > 0 {
				f.Detail += fmt.Sprintf(" ve %d yer daha", r.More)
			}
			f.Fix = "Container yeniden oluşturulunca (yeniden dağıtım, imaj değişikliği, grup değiştirme) bunlar kaybolur; panel o işlemlerden önce sorar. "
			if s.DataMount != "" {
				f.Fix += "Site kalıcı klasör olarak " + s.DataMount + " kullanıyor: kalıcı olmaları için uygulama onları oraya yazmalı."
			} else {
				f.Fix += "Kalıcı tutmak için site ayarlarında bu yolu kalıcı klasör yap: içerik oraya taşınır."
			}
		default:
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		out = append(out, doctor.Finding{ID: "site-layers", Section: "disk", Level: doctor.OK,
			Title: "Container'ının içine, kalıcı klasör dışında yazan site yok"})
	}
	return out
}

func standardFindings(ctx context.Context, h Host, a Answers, self string) []doctor.Finding {
	var out []doctor.Finding
	for _, s := range Steps(h, a, Deps{Self: self}) {
		name := s.Name()
		if name == "upgrade" || name == "recover" {
			continue
		}
		title := stepTitle(name)
		f := doctor.Finding{ID: "step:" + name, Section: "standard", Level: doctor.OK, Title: title}
		done, err := s.Check(ctx)
		switch {
		case err != nil:
			f.Level, f.Title, f.Detail = doctor.Warn, "Denetlenemedi: "+title, err.Error()
		case !done:
			f.Level = doctor.Warn
			if seriousSteps[name] {
				f.Level = doctor.Bad
			}
			f.Title = "Standarttan farklı: " + title
			f.Detail = "Kurulumun bu adımı: " + strings.TrimSuffix(s.Describe(), ".") + "."
			f.Fix = "sudo footops install (önceki cevaplarla; yalnız farklı olanı düzeltir)"
		}
		out = append(out, f)
	}
	return out
}

func accountFindings(h Host, a Answers) []doctor.Finding {
	var out []doctor.Finding
	for _, n := range accountProblems(h, a.AdminUser, a.SudoMode) {
		title, rest, _ := strings.Cut(n.text, ". ")
		out = append(out, doctor.Finding{ID: "account:" + n.key, Section: "accounts", Level: doctor.Warn,
			Title: title, Detail: rest})
	}
	return out
}
