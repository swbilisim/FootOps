package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func valid() Panel {
	return Panel{ServerCode: "H1", PanelHost: "panel-h1.example.com", ListenTunnel: DefaultTunnel, ListenLocal: DefaultLocal}
}

func TestValidateRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:7700", ":7700", "10.0.0.5:7700", "[::]:7700", "example.com:7700"} {
		p := valid()
		p.ListenTunnel = addr
		if p.Validate() == nil {
			t.Errorf("%s accepted", addr)
		}
	}
	p := valid()
	p.ListenLocal = "[::1]:7701"
	if err := p.Validate(); err != nil {
		t.Errorf("IPv6 loopback rejected: %v", err)
	}
}

func TestValidateAccess(t *testing.T) {
	p := valid()
	p.AccessTeam = "swbilisim"
	if p.Validate() == nil {
		t.Error("team without AUD accepted")
	}
	p.AccessAUD = strings.Repeat("ab", 32)
	if err := p.Validate(); err != nil || !p.TunnelEnabled() {
		t.Errorf("valid access config: %v", err)
	}
	p.AccessAUD = "short"
	if p.Validate() == nil {
		t.Error("bad AUD accepted")
	}
}

func TestLoadDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "footops.json")
	os.WriteFile(path, []byte(`{"server_code":"H1","panel_host":"panel-h1.example.com"}`), 0o640)
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p.ListenTunnel != DefaultTunnel || p.ListenLocal != DefaultLocal || p.TunnelEnabled() {
		t.Errorf("p = %+v", p)
	}
}
