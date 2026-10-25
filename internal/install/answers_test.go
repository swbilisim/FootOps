package install

import (
	"strings"
	"testing"
)

func validAnswers() Answers {
	return Answers{
		ServerCode:   "H1",
		Hostname:     "h1",
		Timezone:     "Europe/Istanbul",
		AdminUser:    "furkan",
		SSHKeys:      []string{keyA},
		SudoMode:     SudoPassword,
		SwapBytes:    2 << 30,
		MongoCacheGB: 0.5,
		PanelHost:    "panel-h1.example.com",
		AutoReboot:   true,
		RebootTime:   "04:00",
	}
}

func TestParseSSHKey(t *testing.T) {
	cases := []struct {
		name string
		line string
		ok   bool
	}{
		{"ed25519", keyA, true},
		{"ed25519 without comment", strings.Join(strings.Fields(keyA)[:2], " "), true},
		{"rsa 3072", keyRSA, true},
		{"rsa 1024 too weak", keyWeak, false},
		{"empty", "", false},
		{"only type", "ssh-ed25519", false},
		{"unknown type", "ssh-dss AAAAB3NzaC1kc3M=", false},
		{"bad base64", "ssh-ed25519 !!!notbase64", false},
		{"truncated blob", "ssh-ed25519 " + strings.Fields(keyA)[1][:12], false},
		{"type mismatch", "ssh-rsa " + strings.Fields(keyA)[1], false},
		{"with options prefix", `from="1.2.3.4" ` + keyA, false},
	}
	for _, c := range cases {
		_, err := ParseSSHKey(c.line)
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestParseSSHKeyIdentityIgnoresComment(t *testing.T) {
	a, _ := ParseSSHKey(keyA)
	b, _ := ParseSSHKey(strings.Replace(keyA, "a@test", "other comment", 1))
	if a != b {
		t.Errorf("identity depends on comment: %q vs %q", a, b)
	}
}

func TestValidators(t *testing.T) {
	type tc struct {
		in string
		ok bool
	}
	run := func(name string, fn func(string) error, cases []tc) {
		for _, c := range cases {
			if err := fn(c.in); (err == nil) != c.ok {
				t.Errorf("%s(%q) err = %v, want ok=%v", name, c.in, err, c.ok)
			}
		}
	}
	run("ValidateHostname", ValidateHostname, []tc{
		{"h1", true}, {"web-01", true}, {"H1", false}, {"-h1", false}, {"h1-", false}, {"h_1", false}, {"", false},
	})
	run("ValidateUser", ValidateUser, []tc{
		{"furkan", true}, {"deploy_1", true}, {"root", false}, {"ubuntu", false}, {"Furkan", false}, {"1abc", false}, {"a.b", false},
	})
	run("ValidateTimezone", ValidateTimezone, []tc{
		{"Europe/Istanbul", true}, {"UTC", true}, {"Etc/GMT+3", true}, {"America/Argentina/Buenos_Aires", true},
		{"../etc/passwd", false}, {"Europe/../../x", false}, {"", false},
	})
	run("ValidateFQDN", ValidateFQDN, []tc{
		{"panel-h1.swbilisim.com", true}, {"a.b", true}, {"localhost", false}, {"https://x.com", false},
		{"Panel.example.com", false}, {"a..b", false}, {"-a.com", false},
	})
	run("ValidateClock", ValidateClock, []tc{
		{"04:00", true}, {"23:59", true}, {"24:00", false}, {"4:00", false}, {"04:60", false},
	})
	run("ValidateSudoPassword", ValidateSudoPassword, []tc{
		{"short", false}, {"twelve-chars", true},
	})
}

func TestAnswersValidate(t *testing.T) {
	if err := validAnswers().Validate(); err != nil {
		t.Fatalf("valid answers rejected: %v", err)
	}
	mutations := map[string]func(*Answers){
		"no keys":        func(a *Answers) { a.SSHKeys = nil },
		"bad key":        func(a *Answers) { a.SSHKeys = []string{keyWeak} },
		"bad sudo mode":  func(a *Answers) { a.SudoMode = "maybe" },
		"tiny swap":      func(a *Answers) { a.SwapBytes = 1 << 20 },
		"zero cache":     func(a *Answers) { a.MongoCacheGB = 0 },
		"bad user":       func(a *Answers) { a.AdminUser = "root" },
		"bad panel host": func(a *Answers) { a.PanelHost = "panel" },
	}
	for name, mutate := range mutations {
		a := validAnswers()
		mutate(&a)
		if a.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAnswersMarshalNeverIncludesPassword(t *testing.T) {
	a := validAnswers()
	a.SudoPasswordValue = "super-secret-password"
	s, err := a.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "super-secret-password") {
		t.Fatal("sudo password was persisted")
	}
	back, err := UnmarshalAnswers(s)
	if err != nil {
		t.Fatal(err)
	}
	if back.SudoPasswordValue != "" || back.AdminUser != a.AdminUser || back.SwapBytes != a.SwapBytes {
		t.Errorf("round trip mismatch: %+v", back)
	}
}
