package install

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunStopsWhenCancelled(t *testing.T) {
	cctx, cancel := context.WithCancel(ctx)
	a := &fakeStep{name: "a"}
	b := &fakeStep{name: "b"}
	cancel()
	if _, err := Run(cctx, []Step{a, b}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if a.applied || b.applied {
		t.Error("steps ran after cancellation")
	}
}

func TestPlanChangesNothing(t *testing.T) {
	a := &fakeStep{name: "done", done: true}
	b := &fakeStep{name: "todo"}
	var out bytes.Buffer
	if err := Plan(ctx, []Step{a, b}, &out); err != nil {
		t.Fatal(err)
	}
	if a.applied || b.applied {
		t.Error("Plan applied a step")
	}
	if !strings.Contains(out.String(), "zaten tamam") || !strings.Contains(out.String(), "yapılacak") {
		t.Errorf("plan output:\n%s", out.String())
	}
}

func TestGatherFactsSkipsKeysWithOptions(t *testing.T) {
	h, _ := newTestHost(t, nil)
	put(t, h, "/etc/hostname", "ubuntu-4gb-nbg1-2\n")
	put(t, h, "/root/.ssh/authorized_keys", keyA+"\n"+`command="echo hi" `+keyB+"\n\n")
	f := gatherFacts(h, 1<<30)
	if f.Hostname != "ubuntu-4gb-nbg1-2" {
		t.Errorf("hostname = %q", f.Hostname)
	}
	if len(f.RootSSHKeys) != 1 || f.RootSSHKeys[0] != keyA {
		t.Errorf("root keys = %v", f.RootSSHKeys)
	}
}

func TestResumeReusesSavedAnswers(t *testing.T) {
	db := &userDB{exists: true, inSudo: true, passwordSet: true}
	h, _ := newTestHost(t, db.handle("furkan"))
	data, _ := validAnswers().Marshal()
	put(t, h, AnswersPath, data)
	p, _ := prompter("e\n")
	a, err := loadOrCollect(ctx, Options{Host: h, Prompt: p}, 4<<30)
	if err != nil {
		t.Fatal(err)
	}
	if a.AdminUser != "furkan" || a.SudoPasswordValue != "" {
		t.Errorf("resumed answers = %+v", a)
	}
}

func TestResumeAsksPasswordOnlyIfNotSet(t *testing.T) {
	db := &userDB{exists: true, inSudo: true}
	h, _ := newTestHost(t, db.handle("furkan"))
	data, _ := validAnswers().Marshal()
	put(t, h, AnswersPath, data)
	p, _ := prompter("e\nnew password 1\nnew password 1\n")
	a, err := loadOrCollect(ctx, Options{Host: h, Prompt: p}, 4<<30)
	if err != nil {
		t.Fatal(err)
	}
	if a.SudoPasswordValue != "new password 1" {
		t.Errorf("password = %q", a.SudoPasswordValue)
	}
}

func TestStepsOrder(t *testing.T) {
	a := validAnswers()
	a.TunnelEnabled, a.AccessTeam, a.AccessAUD = true, "team", strings.Repeat("a", 64)
	steps := Steps(Host{}, a, Deps{Confirm: confirmWith(true), Notes: &Notes{}})
	pos := map[string]int{}
	for i, s := range steps {
		if _, dup := pos[s.Name()]; dup {
			t.Errorf("duplicate step name %s", s.Name())
		}
		pos[s.Name()] = i
	}
	before := [][2]string{
		{"packages", "fail2ban"},        // fail2ban/ufw are installed by packages
		{"admin-user", "ssh-hardening"}, // never lock out before the new user exists
		{"firewall", "ssh-hardening"},
		{"fail2ban", "ssh-hardening"},
		{"ssh-hardening", "docker-repo"}, // root login closed before long downloads
		{"docker-repo", "docker"},
		{"docker", "docker-daemon"},
		{"docker-daemon", "core-files"},
		{"core-files", "core-stack"},
		{"tunnel-secret", "core-files"},  // cloudflared reads the token at start
		{"core-stack", "panel-services"}, // the agent talks to Docker
		{"panel-user", "panel-config"},   // config is chowned to the panel user
		{"panel-config", "panel-account"},
		{"panel-binary", "panel-services"},
		{"panel-account", "panel-services"},
	}
	if len(steps) != 21 {
		t.Errorf("steps = %d, want 21", len(steps))
	}
	for _, b := range before {
		if pos[b[0]] >= pos[b[1]] {
			t.Errorf("%s must run before %s: %v", b[0], b[1], pos)
		}
	}
}

func TestCloudInitRunning(t *testing.T) {
	cases := map[string]bool{
		"status: running\n":                      true,
		"\nstatus: running\n":                    true,
		"status: done\n":                         false,
		"status: not started\n":                  false,
		"status: disabled\n":                     false,
		"status: error\n":                        false,
		"":                                       false,
		"extended_status: running\nstatus: done": false,
	}
	for in, want := range cases {
		if got := cloudInitRunning(in); got != want {
			t.Errorf("cloudInitRunning(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestWaitForCloudInitSkipsWhenNotRunning(t *testing.T) {
	h, f := newTestHost(t, func(cmd string) (string, error) {
		if cmd == "cloud-init status" {
			return "status: not started\n", nil
		}
		return "", nil
	})
	if err := waitForCloudInit(ctx, Options{Host: h, Out: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if f.ran("timeout") {
		t.Error("waited for a cloud-init that is not running")
	}
}

// secretStep reports a secret and then fails, like core-files followed by a failing core-stack.
type secretStep struct {
	fakeStep
	notes *Notes
}

func (s *secretStep) Apply(context.Context) error {
	s.notes.Add("secret-value")
	return errors.New("later failure")
}

func TestSecretsShownEvenWhenRunFails(t *testing.T) {
	notes := &Notes{}
	p, out := prompter("")
	o := Options{Prompt: p, Out: io.Discard}
	func() {
		defer printSecrets(o, notes)
		_, _ = Run(ctx, []Step{&secretStep{fakeStep{name: "s"}, notes}}, io.Discard)
	}()
	if !strings.Contains(out.String(), "secret-value") {
		t.Error("secret generated before a failure was never shown")
	}
}
