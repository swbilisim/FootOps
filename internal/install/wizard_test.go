package install

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/auth"

	"github.com/swbilisim/FootOps/internal/sysinfo"
)

func prompter(input string) (*Prompter, *bytes.Buffer) {
	var out bytes.Buffer
	return NewPrompter(strings.NewReader(input), &out, nil), &out
}

func TestAskUsesDefaultAndRetriesInvalid(t *testing.T) {
	p, out := prompter("\n")
	got, err := p.Ask("x", "def", nil)
	if err != nil || got != "def" {
		t.Fatalf("got %q, %v", got, err)
	}

	p, out = prompter("BAD\nh1\n")
	got, err = p.Ask("host", "", ValidateHostname)
	if err != nil || got != "h1" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "✗") {
		t.Error("invalid answer was not reported")
	}
}

func TestAskEOF(t *testing.T) {
	p, _ := prompter("")
	if _, err := p.Ask("x", "", nil); !errors.Is(err, ErrInputClosed) {
		t.Fatalf("err = %v, want ErrInputClosed", err)
	}
	// A last line without newline is still an answer.
	p, _ = prompter("value")
	if got, err := p.Ask("x", "", nil); err != nil || got != "value" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestYesNo(t *testing.T) {
	cases := map[string]bool{"e\n": true, "evet\n": true, "Y\n": true, "h\n": false, "HAYIR\n": false, "hayır\n": false, "no\n": false}
	for in, want := range cases {
		p, _ := prompter(in)
		got, err := p.YesNo("?", !want)
		if err != nil || got != want {
			t.Errorf("YesNo(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	p, _ := prompter("maybe\n\n")
	if got, err := p.YesNo("?", true); err != nil || !got {
		t.Errorf("default after retry = %v, %v", got, err)
	}
}

func TestNewPasswordKeepsSpacesAndRequiresMatch(t *testing.T) {
	p, out := prompter("  pass with spaces  \nmismatch\n  pass with spaces  \n  pass with spaces  \n")
	got, err := p.NewPassword("pw", ValidateSudoPassword)
	if err != nil {
		t.Fatal(err)
	}
	if got != "  pass with spaces  " {
		t.Errorf("password = %q, surrounding spaces must be kept", got)
	}
	if !strings.Contains(out.String(), "aynı değil") {
		t.Error("mismatch was not reported")
	}
}

func TestCollectFullRun(t *testing.T) {
	input := strings.Join([]string{
		"H1",                    // server code
		"",                      // hostname -> h1
		"",                      // timezone default
		"",                      // admin user -> furkan
		"e",                     // reuse root keys
		keyB,                    // extra key
		"",                      // end of extra keys
		"e",                     // sudo asks password
		"correct horse battery", // password
		"correct horse battery", // repeat
		"NaN",                   // swap: rejected
		"",                      // swap default
		"",                      // mongo cache default
		"Panel-H1.Example.com",  // panel host, upper-case accepted and lowered
		"e",                     // set up Cloudflare Tunnel
		"not-a-token",           // rejected
		testTunnelToken,
		"https://SWBilisim.cloudflareaccess.com/", // team, normalised
		strings.Repeat("AB", 32),                  // AUD, lowered
		"",                                        // auto reboot yes
		"",                                        // reboot time default
		"e",                                       // confirm
	}, "\n") + "\n"
	p, out := prompter(input)
	a, err := Collect(p, Facts{
		RAM:         3906588 * 1024,
		Hostname:    "ubuntu-4gb-nbg1-2",
		RootSSHKeys: []string{keyA},
		ZoneinfoOK:  func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("Collect: %v\n%s", err, out.String())
	}
	if a.Hostname != "h1" || a.AdminUser != "furkan" || a.Timezone != "Europe/Istanbul" {
		t.Errorf("unexpected identity answers: %+v", a)
	}
	if len(a.SSHKeys) != 2 {
		t.Errorf("keys = %d, want 2", len(a.SSHKeys))
	}
	if a.SudoMode != SudoPassword || a.SudoPasswordValue != "correct horse battery" {
		t.Errorf("sudo = %v %q", a.SudoMode, a.SudoPasswordValue)
	}
	if a.SwapBytes != 2*sysinfo.GiB || a.MongoCacheGB != 0.5 {
		t.Errorf("sizing = %d / %v", a.SwapBytes, a.MongoCacheGB)
	}
	if a.PanelHost != "panel-h1.example.com" {
		t.Errorf("panel host = %q", a.PanelHost)
	}
	if !a.TunnelEnabled || a.TunnelToken != testTunnelToken || a.AccessTeam != "swbilisim" || a.AccessAUD != strings.Repeat("ab", 32) {
		t.Errorf("tunnel answers = %v %q %q %q", a.TunnelEnabled, a.TunnelToken, a.AccessTeam, a.AccessAUD)
	}
	if strings.Contains(out.String(), testTunnelToken) {
		t.Error("tunnel token echoed")
	}
	if saved, _ := a.Marshal(); strings.Contains(saved, testTunnelToken) {
		t.Error("tunnel token persisted")
	}
	if strings.Contains(out.String(), "correct horse battery") {
		t.Error("password echoed in output")
	}
}

func TestCollectDeclinedAtSummary(t *testing.T) {
	input := strings.Join([]string{"A", "", "", "", keyA, "", "h", "", "", "panel-a.example.com", "h", "h", ""}, "\n") + "\n"
	p, out := prompter(input)
	_, err := Collect(p, Facts{RAM: 8 << 30, ZoneinfoOK: func(string) bool { return true }})
	if !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted\n%s", err, out.String())
	}
}

func TestCollectRejectsUnknownTimezone(t *testing.T) {
	input := strings.Join([]string{"H1", "", "Mars/Olympus", "Europe/Istanbul"}, "\n") + "\n"
	p, out := prompter(input)
	_, _ = Collect(p, Facts{RAM: 4 << 30, ZoneinfoOK: func(tz string) bool { return tz == "Europe/Istanbul" }})
	if !strings.Contains(out.String(), "bulunamadı") {
		t.Errorf("unknown timezone accepted:\n%s", out.String())
	}
}

const testTunnelToken = "eyJhIjogImFjYzEyMyIsICJ0IjogInR1bm5lbC11dWlkIiwgInMiOiAiYzJWamNtVjAifQ=="

func TestValidateTunnelToken(t *testing.T) {
	if err := ValidateTunnelToken(testTunnelToken); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
	for _, bad := range []string{"", "abc", testTunnelToken[:20], "eyJhIjoiIn0="} {
		if ValidateTunnelToken(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestEnrolTOTP(t *testing.T) {
	secret, _ := auth.NewTOTPSecret()
	code, _ := auth.TOTPCode(secret, time.Now())
	p, out := prompter("000000\n" + code + "\n")
	if err := enrolTOTP(p)(ctx, secret, auth.TOTPURI(secret, "FootOps H1", "furkan")); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if !strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "\x1b[30;47m") {
		t.Error("secret or QR not shown")
	}
	p, _ = prompter("1\n2\n3\n")
	if err := enrolTOTP(p)(ctx, secret, "otpauth://x"); !errors.Is(err, ErrAborted) {
		t.Errorf("three wrong codes: %v", err)
	}
}
