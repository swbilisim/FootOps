package install

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/swbilisim/FootOps/internal/config"
)

// AnswersPath stores the wizard answers so a re-run can resume without asking again.
// It holds no secrets: the sudo password is never persisted.
const AnswersPath = "/etc/footops/install.json"

// SudoMode decides how the admin user gets root.
type SudoMode string

const (
	SudoPassword   SudoMode = "password"   // sudo asks for the user's password (recommended)
	SudoNoPassword SudoMode = "nopassword" // sudo never asks; the SSH key is the only barrier
)

// Answers are the installer's inputs.
type Answers struct {
	ServerCode   string   `json:"server_code"`
	Hostname     string   `json:"hostname"`
	Timezone     string   `json:"timezone"`
	AdminUser    string   `json:"admin_user"`
	SSHKeys      []string `json:"ssh_keys"`
	SudoMode     SudoMode `json:"sudo_mode"`
	SwapBytes    uint64   `json:"swap_bytes"`
	MongoCacheGB float64  `json:"mongo_cache_gb"`
	PanelHost    string   `json:"panel_host"`
	AutoReboot   bool     `json:"auto_reboot"`
	RebootTime   string   `json:"reboot_time"`

	// Cloudflare Tunnel + Access for reaching the panel from outside. Without it the
	// panel is reachable through an SSH tunnel only.
	TunnelEnabled bool   `json:"tunnel_enabled"`
	AccessTeam    string `json:"access_team,omitempty"`
	AccessAUD     string `json:"access_aud,omitempty"`

	// Secrets are only held in memory for the run that sets them.
	SudoPasswordValue string `json:"-"`
	TunnelToken       string `json:"-"`
}

func (a Answers) Marshal() (string, error) {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

func UnmarshalAnswers(s string) (Answers, error) {
	var a Answers
	err := json.Unmarshal([]byte(s), &a)
	return a, err
}

var (
	reServerCode = regexp.MustCompile(`^[A-Za-z0-9-]{1,16}$`)
	reHostname   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reUser       = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reTimezone   = regexp.MustCompile(`^[A-Za-z_]+(/[A-Za-z0-9_+-]+){0,2}$`)
	reDNSLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reClock      = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func ValidateServerCode(s string) error {
	if !reServerCode.MatchString(s) {
		return errors.New("1-16 karakter: harf, rakam, tire")
	}
	return nil
}

func ValidateHostname(s string) error {
	if !reHostname.MatchString(s) {
		return errors.New("küçük harf, rakam ve tire; tire ile başlayamaz/bitemez")
	}
	return nil
}

var reservedUsers = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true, "nobody": true,
	"ubuntu": true, "admin": true, "footops": true, "www-data": true, "syslog": true,
}

func ValidateUser(s string) error {
	if !reUser.MatchString(s) {
		return errors.New("küçük harfle başlamalı; küçük harf, rakam, _ ve - kullanılabilir")
	}
	if reservedUsers[s] {
		return fmt.Errorf("%q sistem tarafından kullanılıyor, başka bir ad seç", s)
	}
	return nil
}

// ValidateTimezone only checks the shape; the installer also checks the zoneinfo file exists.
func ValidateTimezone(s string) error {
	if !reTimezone.MatchString(s) || strings.Contains(s, "..") {
		return errors.New("örnek: Europe/Istanbul")
	}
	return nil
}

// ValidateFQDN accepts a lower-case DNS name with at least two labels.
func ValidateFQDN(s string) error {
	labels := strings.Split(s, ".")
	if len(s) > 253 || len(labels) < 2 {
		return errors.New("tam alan adı gir, örnek: panel-h1.ornek.com")
	}
	for _, l := range labels {
		if !reDNSLabel.MatchString(l) {
			return fmt.Errorf("geçersiz bölüm %q", l)
		}
	}
	return nil
}

func ValidateClock(s string) error {
	if !reClock.MatchString(s) {
		return errors.New("SS:DD biçiminde, örnek: 04:00")
	}
	return nil
}

func ValidateSudoPassword(s string) error {
	if len(s) < 12 {
		return errors.New("en az 12 karakter")
	}
	return nil
}

var allowedKeyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// ParseSSHKey validates an authorized_keys line and returns its "type base64" identity,
// which is used to compare keys regardless of their comment.
func ParseSSHKey(line string) (string, error) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", errors.New("anahtar 'tür base64 [yorum]' biçiminde olmalı")
	}
	if !allowedKeyTypes[f[0]] {
		return "", fmt.Errorf("desteklenmeyen anahtar türü %q (ssh-ed25519 önerilir)", f[0])
	}
	raw, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", errors.New("anahtarın base64 kısmı bozuk")
	}
	// The blob starts with its own type name (uint32 length + string). A mismatch means
	// the key was truncated or mangled while copying.
	if len(raw) < 4 {
		return "", errors.New("anahtar eksik kopyalanmış")
	}
	n := binary.BigEndian.Uint32(raw[:4])
	if uint64(n) > uint64(len(raw)-4) || string(raw[4:4+n]) != f[0] {
		return "", errors.New("anahtar eksik veya bozuk kopyalanmış")
	}
	if f[0] == "ssh-rsa" && len(raw) < 270 { // ~2048-bit modulus and up
		return "", errors.New("RSA anahtarı çok kısa; ssh-ed25519 kullan")
	}
	return f[0] + " " + f[1], nil
}

// Validate checks every field; used before running steps with resumed answers.
func (a Answers) Validate() error {
	checks := []struct {
		field string
		err   error
	}{
		{"server_code", ValidateServerCode(a.ServerCode)},
		{"hostname", ValidateHostname(a.Hostname)},
		{"timezone", ValidateTimezone(a.Timezone)},
		{"admin_user", ValidateUser(a.AdminUser)},
		{"panel_host", ValidateFQDN(a.PanelHost)},
		{"reboot_time", ValidateClock(a.RebootTime)},
	}
	for _, c := range checks {
		if c.err != nil {
			return fmt.Errorf("%s: %w", c.field, c.err)
		}
	}
	if len(a.SSHKeys) == 0 {
		return errors.New("ssh_keys: en az bir anahtar gerekli")
	}
	for _, k := range a.SSHKeys {
		if _, err := ParseSSHKey(k); err != nil {
			return fmt.Errorf("ssh_keys: %w", err)
		}
	}
	if a.SudoMode != SudoPassword && a.SudoMode != SudoNoPassword {
		return fmt.Errorf("sudo_mode: %q geçersiz", a.SudoMode)
	}
	if a.SwapBytes < 512<<20 || a.SwapBytes > 64<<30 {
		return errors.New("swap_bytes: 512 MiB ile 64 GiB arasında olmalı")
	}
	if !(a.MongoCacheGB >= 0.25 && a.MongoCacheGB <= 64) {
		return errors.New("mongo_cache_gb: 0.25 ile 64 arasında olmalı")
	}
	if a.TunnelEnabled {
		if err := config.ValidateTeam(a.AccessTeam); err != nil {
			return fmt.Errorf("access_team: %w", err)
		}
		if err := config.ValidateAUD(a.AccessAUD); err != nil {
			return fmt.Errorf("access_aud: %w", err)
		}
	}
	return nil
}

// ValidateTunnelToken checks the shape of a Cloudflare Tunnel token: base64-encoded JSON
// with the account tag ("a"), tunnel ID ("t") and secret ("s"). It catches truncated or
// wrong pastes before cloudflared fails at runtime.
func ValidateTunnelToken(s string) error {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return errors.New("token base64 değil; Cloudflare'deki token'ın tamamını kopyala")
	}
	var t struct {
		A string `json:"a"`
		T string `json:"t"`
		S string `json:"s"`
	}
	if err := json.Unmarshal(raw, &t); err != nil || t.A == "" || t.T == "" || t.S == "" {
		return errors.New("bu bir Cloudflare Tunnel token'ı gibi görünmüyor")
	}
	return nil
}
