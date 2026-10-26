// Package config is the panel configuration written by the installer and read by the
// web process. It holds no secrets.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
)

const (
	Path            = "/etc/footops/footops.json"
	AuthPath        = "/var/lib/footops/web/auth.json"
	WebStateDir     = "/var/lib/footops/web"
	DefaultTunnel   = "127.0.0.1:7700" // cloudflared connects here; Access JWT required
	DefaultLocal    = "127.0.0.1:7701" // SSH tunnel (break-glass); no Access
	TunnelSecretEnv = "/etc/footops/secrets/cloudflared.env"
)

type Panel struct {
	ServerCode string `json:"server_code"`
	PanelHost  string `json:"panel_host"`
	// AccessTeam and AccessAUD identify the Cloudflare Access application. When empty,
	// the tunnel listener is not started and the panel is reachable over SSH only.
	AccessTeam   string `json:"access_team,omitempty"`
	AccessAUD    string `json:"access_aud,omitempty"`
	ListenTunnel string `json:"listen_tunnel"`
	ListenLocal  string `json:"listen_local"`
}

// TunnelEnabled reports whether the Cloudflare listener should run.
func (p Panel) TunnelEnabled() bool { return p.AccessTeam != "" && p.AccessAUD != "" }

var (
	reTeam = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	reAUD  = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func ValidateTeam(s string) error {
	if !reTeam.MatchString(s) {
		return errors.New("takım adı küçük harf, rakam ve tire (örn. swbilisim)")
	}
	return nil
}

func ValidateAUD(s string) error {
	if !reAUD.MatchString(s) {
		return errors.New("AUD etiketi 64 karakterlik onaltılık bir değer olmalı")
	}
	return nil
}

// Validate refuses any listen address that is not loopback: the panel must never be
// reachable directly from the network.
func (p Panel) Validate() error {
	for _, addr := range []string{p.ListenTunnel, p.ListenLocal} {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("dinleme adresi %q: %w", addr, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("dinleme adresi %q loopback değil; panel ağa açılamaz", addr)
		}
	}
	if p.ListenTunnel == p.ListenLocal {
		return errors.New("tünel ve yerel dinleme adresleri farklı olmalı")
	}
	if (p.AccessTeam == "") != (p.AccessAUD == "") {
		return errors.New("access_team ve access_aud birlikte verilmeli")
	}
	if p.AccessTeam != "" {
		if err := ValidateTeam(p.AccessTeam); err != nil {
			return err
		}
		if err := ValidateAUD(p.AccessAUD); err != nil {
			return err
		}
	}
	return nil
}

func Load(path string) (Panel, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Panel{}, err
	}
	var p Panel
	if err := json.Unmarshal(b, &p); err != nil {
		return Panel{}, fmt.Errorf("%s: %w", path, err)
	}
	if p.ListenTunnel == "" {
		p.ListenTunnel = DefaultTunnel
	}
	if p.ListenLocal == "" {
		p.ListenLocal = DefaultLocal
	}
	return p, p.Validate()
}

func (p Panel) Marshal() (string, error) {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}
