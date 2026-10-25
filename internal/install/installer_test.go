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

func TestStepsOrderEndsWithSSHHardening(t *testing.T) {
	steps := Steps(Host{}, validAnswers(), confirmWith(true))
	if steps[len(steps)-1].Name() != "ssh-hardening" {
		t.Errorf("last step = %s", steps[len(steps)-1].Name())
	}
	pos := map[string]int{}
	for i, s := range steps {
		pos[s.Name()] = i
	}
	if pos["packages"] > pos["fail2ban"] || pos["admin-user"] > pos["ssh-hardening"] || pos["firewall"] > pos["ssh-hardening"] {
		t.Errorf("bad order: %v", pos)
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
