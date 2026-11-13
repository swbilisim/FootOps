package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/core"
)

const (
	testRepo     = "s3:https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com/footops-h1"
	testKeyID    = "KEYID12345"
	testSecret   = "SeCrEt/Key+123="
	testPassword = "RepoPassword0123456789abcdefghijklmnop"
)

func testRecovery() RecoverAnswers {
	return RecoverAnswers{Repository: testRepo, KeyID: testKeyID, SecretKey: testSecret, Password: testPassword}
}

func TestRecoveryInfoRoundTrip(t *testing.T) {
	line := FormatRecoveryInfo(testRecovery())
	if !strings.HasPrefix(line, recoveryPrefix) || strings.ContainsAny(line, "\n ") {
		t.Fatalf("line = %q", line)
	}
	got, err := ParseRecoveryInfo("  " + line + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != testRecovery() {
		t.Errorf("got %+v", got)
	}
	for _, bad := range []string{"", "eyJhIjoxfQ==", recoveryPrefix + "%%%", recoveryPrefix + "eyJ9",
		FormatRecoveryInfo(RecoverAnswers{Repository: "s3:https://x/footops-h1", KeyID: "k", SecretKey: testSecret, Password: testPassword}),
		FormatRecoveryInfo(RecoverAnswers{Repository: testRepo, KeyID: testKeyID, SecretKey: testSecret})} {
		if _, err := ParseRecoveryInfo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestR2Endpoint(t *testing.T) {
	for in, want := range map[string]string{
		"0123456789ABCDEF0123456789abcdef":        "https://0123456789abcdef0123456789abcdef.r2.cloudflarestorage.com",
		"https://s3.example.com/":                 "https://s3.example.com",
		" https://acc.r2.cloudflarestorage.com  ": "https://acc.r2.cloudflarestorage.com",
	} {
		if got, err := r2Endpoint(in); err != nil || got != want {
			t.Errorf("r2Endpoint(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", "http://s3.example.com", "https://s3.example.com/bucket"} {
		if _, err := r2Endpoint(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// recoveryInput is the wizard's answers for a recovery up to the repository.
func recoveryInput(repo ...string) string {
	lines := []string{
		"e",      // from a backup
		"H1",     // the old server's code
		"",       // hostname -> h1
		"",       // timezone
		"furkan", // admin user (no default)
		"e",      // reuse root keys
		"",       // no extra key
		"h",      // sudo without password
		"",       // swap
		"",       // mongo cache
		"panel-h1.example.com",
		"", "", // auto reboot, time
	}
	return strings.Join(append(append(lines, repo...), "e"), "\n") + "\n"
}

func TestCollectRecoveryLine(t *testing.T) {
	line := FormatRecoveryInfo(testRecovery())
	p, out := prompter(recoveryInput(line, ""))
	a, err := Collect(p, Facts{RAM: 4 << 30, Hostname: "x", RootSSHKeys: []string{keyA}, ZoneinfoOK: func(string) bool { return true }})
	if err != nil {
		t.Fatalf("Collect: %v\n%s", err, out.String())
	}
	if a.Recover == nil || *a.Recover != testRecovery() {
		t.Fatalf("recover = %+v", a.Recover)
	}
	if a.TunnelEnabled || strings.Contains(out.String(), "Cloudflare Tunnel şimdi kurulsun mu") {
		t.Error("a recovery asked for a tunnel")
	}
	for _, secret := range []string{testSecret, testPassword, line} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("%q echoed", secret)
		}
	}
	saved, _ := a.Marshal()
	if strings.Contains(saved, testSecret) || strings.Contains(saved, testPassword) || !strings.Contains(saved, testRepo) {
		t.Errorf("saved answers:\n%s", saved)
	}
	if !strings.Contains(out.String(), "yedekten kurtarma → prova kipi") || !strings.Contains(out.String(), "footops-h1, anahtar 2345…") {
		t.Errorf("summary:\n%s", out.String())
	}
	back, err := UnmarshalAnswers(saved)
	if err != nil || back.Validate() != nil || back.Recover == nil || back.Recover.Password != "" {
		t.Errorf("reloaded: %+v %v", back.Recover, err)
	}
}

func TestCollectRecoveryFields(t *testing.T) {
	p, out := prompter(recoveryInput("", "bad", "0123456789abcdef0123456789abcdef", "", testKeyID, testSecret, testPassword,
		"07.11.2026", "2026-11-07 04:00"))
	a, err := Collect(p, Facts{RAM: 4 << 30, RootSSHKeys: []string{keyA}, ZoneinfoOK: func(string) bool { return true }})
	if err != nil {
		t.Fatalf("Collect: %v\n%s", err, out.String())
	}
	if a.Recover.Repository != testRepo || a.Recover.KeyID != testKeyID || a.Recover.SecretKey != testSecret || a.Recover.Password != testPassword {
		t.Errorf("recover = %+v", a.Recover)
	}
	want := time.Date(2026, 11, 7, 1, 0, 0, 0, time.UTC) // 04:00 in Istanbul
	if !a.Recover.At.Equal(want) {
		t.Errorf("at = %v, want %v", a.Recover.At, want)
	}
	if strings.Count(out.String(), "✗") != 2 {
		t.Errorf("want two rejections (account, time):\n%s", out.String())
	}
}

func TestStepsOfARecovery(t *testing.T) {
	h, _ := newTestHost(t, nil)
	a := validAnswers()
	a.TunnelEnabled, a.AccessTeam, a.AccessAUD = false, "", ""
	r := testRecovery()
	a.Recover = &r
	var names []string
	for _, s := range Steps(h, a, Deps{}) {
		names = append(names, s.Name())
	}
	got := strings.Join(names, " ")
	if strings.Contains(got, "panel-account") || strings.Contains(got, "tunnel-secret") || !strings.HasSuffix(got, "panel-services recover") {
		t.Errorf("steps: %s", got)
	}
	a.Recover = nil
	names = nil
	for _, s := range Steps(h, a, Deps{}) {
		names = append(names, s.Name())
	}
	if got := strings.Join(names, " "); !strings.Contains(got, "panel-account") || strings.Contains(got, "recover") {
		t.Errorf("steps of a new server: %s", got)
	}
}

func TestRecoverStep(t *testing.T) {
	r := testRecovery()
	var st agent.RecoverStatus
	var serr error
	var ran []agent.RecoverRequest
	step := recoverStep{rec: &r, out: io.Discard,
		status: func(context.Context) (agent.RecoverStatus, error) { return st, serr },
		run: func(_ context.Context, req agent.RecoverRequest, _ io.Writer) error {
			ran = append(ran, req)
			return nil
		}}
	done := agent.RecoverState{Finished: time.Now(), ServerDone: true, Sites: []agent.RecoverSite{{Name: "a", State: "ok"}, {Name: "b", State: "warn"}}}
	for _, c := range []struct {
		st   agent.RecoverStatus
		err  error
		want bool
	}{
		{agent.RecoverStatus{}, nil, false},
		{agent.RecoverStatus{State: &done}, nil, true},
		{agent.RecoverStatus{State: &done, Running: "J1"}, nil, false},
		{agent.RecoverStatus{State: &done}, errors.New("agent'a ulaşılamadı"), false},
	} {
		st, serr = c.st, c.err
		if got, _ := step.Check(ctx); got != c.want {
			t.Errorf("Check(%+v, %v) = %v", c.st, c.err, got)
		}
	}
	failed := done
	failed.Sites = append(failed.Sites, agent.RecoverSite{Name: "c", State: "failed"})
	st, serr = agent.RecoverStatus{State: &failed}, nil
	if got, _ := step.Check(ctx); got {
		t.Error("a failed site counted as done")
	}
	if err := step.Apply(ctx); err != nil || len(ran) != 1 || ran[0].Password != testPassword || ran[0].Repository != testRepo {
		t.Errorf("apply: %v %+v", err, ran)
	}
	r.SecretKey = ""
	st, serr = agent.RecoverStatus{}, nil
	if err := step.Apply(ctx); err == nil {
		t.Error("ran without the secret key")
	}
	// One under way is followed without the secrets.
	st = agent.RecoverStatus{Running: "J1"}
	if err := step.Apply(ctx); err != nil || len(ran) != 2 {
		t.Errorf("following a running recovery: %v (%d runs)", err, len(ran))
	}
}

func TestResumedRecoveryAsksForSecrets(t *testing.T) {
	db := &userDB{exists: true, inSudo: true, passwordSet: true}
	h, _ := newTestHost(t, db.handle("furkan"))
	a := validAnswers()
	a.TunnelEnabled, a.AccessTeam, a.AccessAUD = false, "", ""
	r := testRecovery()
	a.Recover = &r
	data, _ := a.Marshal()
	put(t, h, AnswersPath, data)
	status := func(context.Context) (agent.RecoverStatus, error) { return agent.RecoverStatus{}, nil }
	other := testRecovery()
	other.KeyID = "OTHERKEY99"
	p, out := prompter("e\n" + FormatRecoveryInfo(other) + "\n" + FormatRecoveryInfo(testRecovery()) + "\n")
	got, err := loadOrCollect(ctx, Options{Host: h, Prompt: p, RecoverStatus: status}, 4<<30)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got.Recover == nil || got.Recover.SecretKey != testSecret || got.Recover.Password != testPassword {
		t.Errorf("recover = %+v", got.Recover)
	}
	if !strings.Contains(out.String(), "başka bir depo ya da anahtar için") {
		t.Errorf("a line for another key was not refused:\n%s", out.String())
	}

	// Under way: nothing is asked (the run follows it).
	status = func(context.Context) (agent.RecoverStatus, error) { return agent.RecoverStatus{Running: "J1"}, nil }
	p, out = prompter("e\n")
	if _, err := loadOrCollect(ctx, Options{Host: h, Prompt: p, RecoverStatus: status}, 4<<30); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Kurtarma bilgisi") {
		t.Errorf("asked while a recovery runs:\n%s", out.String())
	}

	// Finished: nothing is asked.
	done := agent.RecoverState{Finished: time.Now(), ServerDone: true}
	status = func(context.Context) (agent.RecoverStatus, error) { return agent.RecoverStatus{State: &done}, nil }
	p, out = prompter("e\n")
	if _, err := loadOrCollect(ctx, Options{Host: h, Prompt: p, RecoverStatus: status}, 4<<30); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Kurtarma bilgisi") {
		t.Errorf("asked again after the recovery finished:\n%s", out.String())
	}
}

// ---- go-live ----

type goLiveEnv struct {
	h      Host
	run    *fakeRunner
	deps   GoLiveDeps
	calls  []string
	setups []agent.BackupSetupRequest
	status agent.RecoverStatus
}

func newGoLive(t *testing.T) *goLiveEnv {
	t.Helper()
	noSleep(t)
	e := &goLiveEnv{}
	e.h, e.run = newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl is-active"):
			return "active\n", nil
		case strings.HasPrefix(cmd, "hostname -I"):
			return "203.0.113.7 10.0.0.2\n", nil
		}
		return "", nil
	})
	a := validAnswers()
	a.TunnelEnabled, a.AccessTeam, a.AccessAUD, a.PanelHost = false, "", "", "panel-typed.example.com"
	r := testRecovery()
	a.Recover = &r
	data, _ := a.Marshal()
	put(t, e.h, AnswersPath, data)
	put(t, e.h, core.MongoSecretPath, core.MongoSecretEnv("NewServersRoot"))
	held := filepath.Join(agent.DefaultRecoverDir, "held")
	old := config.Panel{ServerCode: "H1", PanelHost: "panel-h1.example.com", AccessTeam: "swbilisim", AccessAUD: strings.Repeat("ab", 32),
		ListenTunnel: config.DefaultTunnel, ListenLocal: config.DefaultLocal}
	oldJSON, _ := old.Marshal()
	put(t, e.h, held+"/footops.json", oldJSON)
	put(t, e.h, held+"/cloudflared.env", core.TunnelSecretEnv(testTunnelToken))
	repo, _ := json.Marshal(map[string]string{"repository": testRepo, "key_id": "WRITEKEY01", "secret_key": "WriteSecret/123=", "password": testPassword})
	put(t, e.h, held+"/repository.json", string(repo))
	put(t, e.h, held+"/telegram.json", `{"token":"123:abc","chat_id":42}`)
	e.status = agent.RecoverStatus{Rehearsal: &agent.RehearsalInfo{Server: "H1"}, State: &agent.RecoverState{ServerDone: true,
		Sites: []agent.RecoverSite{{Name: "a", State: "ok", Domains: []string{"a.example.com", "www.a.example.com"}},
			{Name: "b", State: "failed", Domains: []string{"b.example.com"}}}}}
	e.deps = GoLiveDeps{
		Status: func(context.Context) (agent.RecoverStatus, error) { return e.status, nil },
		Setup: func(_ context.Context, req agent.BackupSetupRequest, _ io.Writer) error {
			e.calls = append(e.calls, "setup")
			e.setups = append(e.setups, req)
			return nil
		},
		End:     func(context.Context) error { e.calls = append(e.calls, "end"); return nil },
		Backup:  func(context.Context) (string, error) { e.calls = append(e.calls, "backup"); return "J9", nil },
		HeldDir: held,
	}
	return e
}

func TestGoLive(t *testing.T) {
	e := newGoLive(t)
	p, out := prompter("h1\nH1\n")
	var log bytes.Buffer
	if err := GoLive(ctx, e.h, p, &log, e.deps); err != nil {
		t.Fatalf("go-live: %v\n%s\n%s", err, out.String(), log.String())
	}
	if got := strings.Join(e.calls, " "); got != "setup end backup" {
		t.Errorf("order: %s", got)
	}
	s := e.setups[0]
	if !s.GoLive || s.Repository != testRepo || s.KeyID != "WRITEKEY01" || s.Password != testPassword || s.Keep {
		t.Errorf("setup = %+v", s.BackupSetup)
	}
	if got := core.TunnelTokenFromEnv(read(t, e.h, core.TunnelSecretPath)); got != testTunnelToken {
		t.Errorf("tunnel token = %q", got)
	}
	if !strings.Contains(read(t, e.h, core.ComposePath), "cloudflared") {
		t.Error("compose has no cloudflared")
	}
	if !e.run.ran("systemctl restart footops-agent") {
		t.Error("the agent kept the typed panel address")
	}
	if !e.run.ran("systemctl try-restart footops-web") {
		t.Error("the panel was not restarted: it opens its tunnel listener only when it starts")
	}
	p2, err := config.Load(e.h.Path(config.Path))
	if err != nil || !p2.TunnelEnabled() || p2.PanelHost != "panel-h1.example.com" {
		t.Errorf("panel config = %+v, %v", p2, err)
	}
	a, _ := UnmarshalAnswers(read(t, e.h, AnswersPath))
	if !a.TunnelEnabled || a.AccessTeam != "swbilisim" || a.PanelHost != "panel-h1.example.com" || a.Recover != nil {
		t.Errorf("answers = %+v (recover %+v)", a, a.Recover)
	}
	st, err := os.Stat(e.h.Path(filepath.Join(core.NotifySecretDir, "telegram.json")))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("telegram.json: %v %v", st, err)
	}
	if e.h.Exists(e.deps.HeldDir) {
		t.Error("the held files are still there")
	}
	if !strings.Contains(out.String(), "a.example.com") || strings.Contains(out.String(), "b.example.com") || !strings.Contains(out.String(), "203.0.113.7") {
		t.Errorf("DNS list:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "tutmuyor") {
		t.Errorf("a wrong code was not refused:\n%s", out.String())
	}
	// Done: a second go-live finds nothing to do.
	e.status.Rehearsal = nil
	if err := GoLive(ctx, e.h, mustPrompter("H1\n"), io.Discard, e.deps); err == nil || !strings.Contains(err.Error(), "prova kipinde değil") {
		t.Errorf("second go-live: %v", err)
	}
}

func mustPrompter(in string) *Prompter {
	p, _ := prompter(in)
	return p
}

func TestGoLiveStopsWhenTheRepositoryFails(t *testing.T) {
	e := newGoLive(t)
	e.deps.Setup = func(context.Context, agent.BackupSetupRequest, io.Writer) error {
		return errors.New("depo şifresi yanlış")
	}
	if err := GoLive(ctx, e.h, mustPrompter("H1\n"), io.Discard, e.deps); err == nil || !strings.Contains(err.Error(), "prova kipinde kaldı") {
		t.Fatalf("err = %v", err)
	}
	if len(e.calls) != 0 || !e.h.Exists(e.deps.HeldDir) {
		t.Errorf("went on after the repository failed: %v", e.calls)
	}
	// Running a recovery, or nothing kept: refused before anything.
	e.status.Running = "J1"
	if err := GoLive(ctx, e.h, mustPrompter("H1\n"), io.Discard, e.deps); err == nil || !strings.Contains(err.Error(), "sürüyor") {
		t.Errorf("err = %v", err)
	}
}

func TestGoLiveWithoutTunnel(t *testing.T) {
	e := newGoLive(t)
	if err := os.Remove(e.h.Path(filepath.Join(e.deps.HeldDir, "cloudflared.env"))); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	if err := GoLive(ctx, e.h, mustPrompter("H1\n"), &log, e.deps); err != nil {
		t.Fatal(err)
	}
	if e.h.Exists(core.TunnelSecretPath) || e.h.Exists(core.ComposePath) {
		t.Error("tunnel set up without a token")
	}
	if !strings.Contains(log.String(), "tünel yoktu") {
		t.Errorf("log:\n%s", log.String())
	}
}

func TestGoLiveNewKeyWhenTheHeldOneIsRefused(t *testing.T) {
	e := newGoLive(t)
	setup := e.deps.Setup
	e.deps.Setup = func(ctx context.Context, req agent.BackupSetupRequest, out io.Writer) error {
		if req.KeyID == "WRITEKEY01" {
			e.calls = append(e.calls, "refused")
			return errors.New("Yedek deposu (footops-h1) erişimi reddetti: anahtar yanlış, iptal edilmiş ya da bu kovaya yetkisi yok")
		}
		return setup(ctx, req, out)
	}
	if err := GoLive(ctx, e.h, mustPrompter("H1\nNEWKEY0001\nNew/Secret+Key=\n"), io.Discard, e.deps); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.calls, " "); got != "refused setup end backup" {
		t.Errorf("calls: %s", got)
	}
	if s := e.setups[0]; s.KeyID != "NEWKEY0001" || s.SecretKey != "New/Secret+Key=" || s.Password != testPassword || s.Repository != testRepo {
		t.Errorf("setup with the new key: %+v", s.BackupSetup)
	}
	// No new key: go-live stops, the server stays a rehearsal.
	e = newGoLive(t)
	e.deps.Setup = func(context.Context, agent.BackupSetupRequest, io.Writer) error {
		return errors.New("Yedek deposu (footops-h1) erişimi reddetti: anahtar yanlış")
	}
	if err := GoLive(ctx, e.h, mustPrompter("H1\n\n"), io.Discard, e.deps); err == nil || !strings.Contains(err.Error(), "prova kipinde kaldı") {
		t.Errorf("err = %v", err)
	}
	if len(e.calls) != 0 || !e.h.Exists(e.deps.HeldDir) {
		t.Errorf("went on: %v", e.calls)
	}
}

func TestGoLiveWhileTheOldServerStillBacksUp(t *testing.T) {
	e := newGoLive(t)
	started := time.Now().Add(-time.Hour)
	e.status.State.Started = started
	status := e.status
	e.deps.Status = func(context.Context) (agent.RecoverStatus, error) {
		st := status
		if len(e.setups) > 0 { // the repository's newest snapshot, once connected
			st.Newest = started.Add(30 * time.Minute)
		}
		return st, nil
	}
	p, out := prompter("H1\nh\n")
	if err := GoLive(ctx, e.h, p, io.Discard, e.deps); err == nil || !strings.Contains(err.Error(), "prova kipinde kaldı") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "eski sunucu hâlâ çalışıyor olabilir") || strings.Join(e.calls, " ") != "setup" {
		t.Errorf("calls %v\n%s", e.calls, out.String())
	}
	// Told it is off: it goes on.
	e.calls, e.setups = nil, nil
	if err := GoLive(ctx, e.h, mustPrompter("H1\ne\n"), io.Discard, e.deps); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(e.calls, " "); got != "setup end backup" {
		t.Errorf("calls: %s", got)
	}
}
