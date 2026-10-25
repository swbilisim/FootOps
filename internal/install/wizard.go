package install

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/sysinfo"
)

// Facts are host observations that shape the wizard's defaults.
type Facts struct {
	RAM         uint64
	Hostname    string
	RootSSHKeys []string // valid keys already in /root/.ssh/authorized_keys (Hetzner puts the provisioning key there)
	ZoneinfoOK  func(tz string) bool
}

// ErrAborted means the operator declined to continue.
var ErrAborted = errors.New("kullanıcı tarafından iptal edildi")

// Collect runs the interactive wizard and returns confirmed answers.
func Collect(p *Prompter, f Facts) (Answers, error) {
	var a Answers
	var err error

	p.Say("")
	p.Say("FootOps kurulumu — sorular. Köşeli parantezdeki değer varsayılandır, Enter ile kabul edilir.")
	p.Say("")

	if a.ServerCode, err = p.Ask("Sunucu kodu (örn. H1, A)", "", ValidateServerCode); err != nil {
		return a, err
	}
	defHost := strings.ToLower(a.ServerCode)
	if ValidateHostname(defHost) != nil {
		defHost = f.Hostname
	}
	if a.Hostname, err = p.Ask("Hostname", defHost, ValidateHostname); err != nil {
		return a, err
	}
	if a.Timezone, err = p.Ask("Saat dilimi", "Europe/Istanbul", func(s string) error {
		if err := ValidateTimezone(s); err != nil {
			return err
		}
		if f.ZoneinfoOK != nil && !f.ZoneinfoOK(s) {
			return fmt.Errorf("%q bulunamadı", s)
		}
		return nil
	}); err != nil {
		return a, err
	}

	p.Say("")
	p.Say("Yönetici kullanıcı: root ile SSH girişi kapatılacak, sunucuya bu kullanıcıyla gireceksin.")
	if a.AdminUser, err = p.Ask("Kullanıcı adı", "furkan", ValidateUser); err != nil {
		return a, err
	}
	if a.SSHKeys, err = askKeys(p, f.RootSSHKeys); err != nil {
		return a, err
	}
	if a.SudoMode, a.SudoPasswordValue, err = askSudo(p, a.AdminUser); err != nil {
		return a, err
	}

	p.Say("")
	p.Say("Bellek: RAM %s.", sysinfo.HumanBytes(f.RAM))
	if a.SwapBytes, err = askGiB(p, "Swap boyutu (GB)", sysinfo.RecommendSwap(f.RAM), 0.5, 64); err != nil {
		return a, err
	}
	cache, err := askFloat(p, "MongoDB önbelleği (GB)", sysinfo.RecommendMongoCacheGB(f.RAM), 0.25, 64)
	if err != nil {
		return a, err
	}
	a.MongoCacheGB = cache

	p.Say("")
	p.Say("Panel adresi Cloudflare'deki bir alan adının TEK seviye alt alanı olmalı")
	p.Say("(panel-h1.ornek.com olur, h1.panel.ornek.com olmaz: ücretsiz sertifika onu kapsamaz).")
	host, err := p.Ask("Panel adresi", "", func(s string) error { return ValidateFQDN(strings.ToLower(s)) })
	if err != nil {
		return a, err
	}
	a.PanelHost = strings.ToLower(host)

	p.Say("")
	p.Say("Güvenlik güncellemeleri otomatik kurulacak. Bazıları (çekirdek) yeniden başlatma ister;")
	p.Say("siteler restart politikasıyla kendiliğinden geri gelir.")
	if a.AutoReboot, err = p.YesNo("Gerekirse sunucu otomatik yeniden başlatılsın mı?", true); err != nil {
		return a, err
	}
	a.RebootTime = "04:00"
	if a.AutoReboot {
		if a.RebootTime, err = p.Ask("Yeniden başlatma saati", "04:00", ValidateClock); err != nil {
			return a, err
		}
	}

	if err := a.Validate(); err != nil {
		return a, err
	}
	p.Say("")
	p.Say("%s", Summary(a))
	ok, err := p.YesNo("Bu ayarlarla kuruluma başlansın mı?", false)
	if err != nil {
		return a, err
	}
	if !ok {
		return a, ErrAborted
	}
	return a, nil
}

func askKeys(p *Prompter, rootKeys []string) ([]string, error) {
	if len(rootKeys) > 0 {
		p.Say("root kullanıcısında %d SSH anahtarı bulundu:", len(rootKeys))
		for _, k := range rootKeys {
			p.Say("  %s", keyLabel(k))
		}
		use, err := p.YesNo("Bu anahtarlar yönetici kullanıcıya da eklensin mi?", true)
		if err != nil {
			return nil, err
		}
		if use {
			extra, err := readKeys(p, "Eklemek istediğin başka public key varsa yapıştır", true)
			if err != nil {
				return nil, err
			}
			return dedupeKeys(append(append([]string{}, rootKeys...), extra...)), nil
		}
	}
	return readKeys(p, "SSH public key(ler)ini yapıştır (Mac'te: cat ~/.ssh/id_ed25519.pub)", false)
}

func readKeys(p *Prompter, label string, optional bool) ([]string, error) {
	for {
		lines, err := p.Lines(label)
		if err != nil {
			return nil, err
		}
		var keys []string
		bad := false
		for _, l := range lines {
			if _, err := ParseSSHKey(l); err != nil {
				p.Say("  ✗ %s: %v", keyLabel(l), err)
				bad = true
				continue
			}
			keys = append(keys, l)
		}
		switch {
		case bad:
			p.Say("  Hatalı anahtarlar var, tümünü yeniden yapıştır.")
		case len(keys) == 0 && !optional:
			p.Say("  ✗ En az bir anahtar gerekli.")
		default:
			return dedupeKeys(keys), nil
		}
	}
}

func askSudo(p *Prompter, user string) (SudoMode, string, error) {
	p.Say("")
	p.Say("sudo: %s root yetkisi alırken şifre sorulsun mu?", user)
	p.Say("  Önerilen: evet. SSH anahtarın çalınsa bile root için ayrıca bu şifre gerekir.")
	ask, err := p.YesNo("sudo şifre istesin mi?", true)
	if err != nil {
		return "", "", err
	}
	if !ask {
		return SudoNoPassword, "", nil
	}
	pw, err := p.NewPassword("sudo şifresi (en az 12 karakter, Vaultwarden'a kaydet)", ValidateSudoPassword)
	if err != nil {
		return "", "", err
	}
	return SudoPassword, pw, nil
}

func askGiB(p *Prompter, label string, def uint64, min, max float64) (uint64, error) {
	v, err := askFloat(p, label, float64(def)/sysinfo.GiB, min, max)
	if err != nil {
		return 0, err
	}
	return uint64(v * sysinfo.GiB), nil
}

func askFloat(p *Prompter, label string, def, min, max float64) (float64, error) {
	s, err := p.Ask(label+" (önerilen)", strconv.FormatFloat(def, 'f', -1, 64), func(s string) error {
		v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
		// Written as !(in range) so NaN, which ParseFloat accepts, is rejected too.
		if err != nil || !(v >= min && v <= max) {
			return fmt.Errorf("%g ile %g arasında bir sayı gir", min, max)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
}

// Summary renders answers for confirmation. Secrets are never printed.
func Summary(a Answers) string {
	var b strings.Builder
	w := func(k, v string) { fmt.Fprintf(&b, "  %-22s %s\n", k, v) }
	b.WriteString("Özet:\n")
	w("Sunucu kodu", a.ServerCode)
	w("Hostname", a.Hostname)
	w("Saat dilimi", a.Timezone)
	w("Yönetici kullanıcı", a.AdminUser)
	for i, k := range a.SSHKeys {
		label := ""
		if i == 0 {
			label = "SSH anahtarları"
		}
		w(label, keyLabel(k))
	}
	if a.SudoMode == SudoPassword {
		w("sudo", "şifre ister")
	} else {
		w("sudo", "şifresiz")
	}
	w("Swap", sysinfo.HumanBytes(a.SwapBytes))
	w("MongoDB önbelleği", strconv.FormatFloat(a.MongoCacheGB, 'f', -1, 64)+" GB")
	w("Panel adresi", a.PanelHost)
	if a.AutoReboot {
		w("Otomatik reboot", "evet, "+a.RebootTime)
	} else {
		w("Otomatik reboot", "hayır")
	}
	b.WriteString("\nYapılacaklar: paket güncellemeleri, swap, saat dilimi, otomatik güvenlik güncellemeleri,\n")
	b.WriteString("yönetici kullanıcı, güvenlik duvarı (22/80/443), fail2ban, SSH sertleştirme (en sonda, onayınla).\n")
	return b.String()
}

// keyLabel shortens a key for display: type, fingerprint-ish tail and comment.
func keyLabel(line string) string {
	f := strings.Fields(line)
	switch {
	case len(f) >= 3:
		return f[0] + " …" + tail(f[1], 8) + " " + strings.Join(f[2:], " ")
	case len(f) == 2:
		return f[0] + " …" + tail(f[1], 8)
	default:
		return line
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func dedupeKeys(keys []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		id, err := ParseSSHKey(k)
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, k)
	}
	return out
}
