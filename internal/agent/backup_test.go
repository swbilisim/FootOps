package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

// exitErr is a command's exit status, as exec.ExitError reports it.
type exitErr struct{ code int }

func (e exitErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitErr) ExitCode() int { return e.code }

// fakeRestic plays the tool containers: the binary's extraction, and restic itself.
// reply answers a restic command (its arguments after "restic"); the default is a
// successful backup.
type fakeRestic struct {
	mu    sync.Mutex
	calls [][]string // restic arguments, or ["cat-binary"]
	cmds  []run.Cmd
	reply func(ctx context.Context, args []string, c run.Cmd) error
	check func(args []string, c run.Cmd)
	snaps int
}

var fakeBinary = []byte("#!restic fake binary")

func (f *fakeRestic) Stream(ctx context.Context, c run.Cmd) (run.Usage, error) {
	u := run.Usage{PeakMem: 120 << 20, CPU: 300 * time.Millisecond, Wall: time.Second}
	args := c.Args
	if i := indexOf(args, "cat"); i > 0 && args[i-1] == "--entrypoint" {
		f.mu.Lock()
		f.calls = append(f.calls, []string{"cat-binary"})
		f.mu.Unlock()
		_, err := c.Stdout.Write(fakeBinary)
		return u, err
	}
	i := indexOf(args, "restic")
	if i < 0 {
		return u, fmt.Errorf("unexpected command %v", args)
	}
	ra := args[i+1:]
	f.mu.Lock()
	f.calls = append(f.calls, ra)
	f.cmds = append(f.cmds, c)
	f.mu.Unlock()
	if f.check != nil {
		f.check(ra, c)
	}
	if f.reply != nil {
		return u, f.reply(ctx, ra, c)
	}
	return u, f.ok(ra, c)
}

// mountSource is the host side of a container's bind mount at target.
func mountSource(args []string, target string) string {
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			var src, dst string
			for _, kv := range strings.Split(args[i+1], ",") {
				if v, ok := strings.CutPrefix(kv, "source="); ok {
					src = v
				}
				if v, ok := strings.CutPrefix(kv, "target="); ok {
					dst = v
				}
			}
			if dst == target {
				return src
			}
		}
	}
	return ""
}

// dumpLog writes what mongodump would into its log file (the command's redirection).
func dumpLog(c run.Cmd, ra []string, lines ...string) {
	i := indexOf(ra, "-c")
	_, name, _ := strings.Cut(ra[i+1], "2>>/tmp/")
	os.WriteFile(filepath.Join(mountSource(c.Args, "/tmp"), name), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// ok answers like a working restic.
func (f *fakeRestic) ok(ra []string, c run.Cmd) error {
	switch ra[0] {
	case "backup":
		if indexOf(ra, "--stdin-from-command") > 0 {
			// restic's own stderr: a line that must not count (it may come or not).
			c.Stderr("subprocess mongodump: 2026-11-06T04:00:00.000+0000\tdone dumping `magaza.yok` (99 documents)")
			dumpLog(c, ra, "2026-11-06T04:00:00.000+0000\twriting `magaza.users` to `archive on stdout`",
				"2026-11-06T04:00:00.000+0000\t[####......]  magaza.users  6/12  (50.0%)",
				"2026-11-06T04:00:00.000+0000\tdone dumping `magaza.users` (12 documents)",
				"2026-11-06T04:00:00.000+0000\tdone dumping `magaza.pages` (1 document)")
		}
		f.mu.Lock()
		f.snaps++
		id := fmt.Sprintf("%064x", f.snaps)
		f.mu.Unlock()
		// Written in pieces, as a pipe delivers it.
		line := fmt.Sprintf(`{"message_type":"status","percent_done":0.5,"bytes_done":1000,"total_bytes":2000}`+"\n"+
			`{"message_type":"summary","files_new":3,"files_changed":1,"files_unmodified":5,"data_added":5000,"data_added_packed":1200,"total_files_processed":9,"total_bytes_processed":20000,"total_duration":0.5,"snapshot_id":"%s"}`+"\n", id)
		for len(line) > 0 {
			n := min(17, len(line))
			if _, err := c.Stdout.Write([]byte(line[:n])); err != nil {
				return err
			}
			line = line[n:]
		}
	case "forget":
		_, err := c.Stdout.Write([]byte(`[{"tags":null,"host":"t1","paths":["/footops/magaza/db.archive"],"keep":[{"id":"a"}],"remove":[{"id":"b"},{"id":"c"}]},{"host":"t1","paths":["/footops/_server"],"keep":[{"id":"d"}],"remove":null}]` + "\n"))
		return err
	case "snapshots":
		_, err := c.Stdout.Write([]byte(`[{"time":"2026-11-05T04:00:00Z","hostname":"T1","paths":["/footops/magaza/db.archive"]}]` + "\n"))
		return err
	case "stats":
		_, err := c.Stdout.Write([]byte(`{"total_size":123456789,"total_uncompressed_size":234567890,"snapshots_count":7}` + "\n"))
		return err
	}
	return nil
}

func (f *fakeRestic) ran(cmd string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

type backupEnv struct {
	*dataEnv
	b      *Backups
	restic *fakeRestic
}

func newBackups(t *testing.T) *backupEnv {
	t.Helper()
	d := newData(t)
	sum := sha256.Sum256(fakeBinary)
	old := resticSHA256[runtime.GOARCH]
	resticSHA256[runtime.GOARCH] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { resticSHA256[runtime.GOARCH] = old })
	fr := &fakeRestic{}
	d.data.Stream = fr
	e := &backupEnv{dataEnv: d, restic: fr}
	e.b = &Backups{Data: d.data, Jobs: NewJobs(), Audit: NewAudit(filepath.Join(d.root, "audit.jsonl")),
		Dir: filepath.Join(d.root, "backup"), SecretPath: filepath.Join(d.root, "etc/footops/secrets/backup.json"),
		ToolsDir: filepath.Join(d.root, "tools"), CacheDir: filepath.Join(d.root, "cache"), Server: "T1",
		now: func() time.Time { return d.now }, loc: time.UTC}
	// A site's directory as the store expects it, with something in its data directory.
	os.WriteFile(filepath.Join(d.root, "srv/musteri/magaza/data/logo.png"), []byte("png"), 0o644)
	return e
}

// configure saves a repository as setup would, confirmed.
func (e *backupEnv) configure(t *testing.T) {
	t.Helper()
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:https://acc123.r2.cloudflarestorage.com/footops-t1",
		KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=", Password: "RepoPassword0123456789abcdefghijklmnop"}); err != nil {
		t.Fatal(err)
	}
	if err := e.b.Confirm(); err != nil {
		t.Fatal(err)
	}
}

func (e *backupEnv) task(id string) *Task { return &Task{j: NewJobs(), job: &Job{ID: id}} }

func TestBackupSettings(t *testing.T) {
	s := DefaultBackupSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.Sites = map[string]BackupPolicy{"pos": {DB: DBHourly}, "kasa": {Freeze: true, KeepDays: 90}, "eski": {Off: true}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if p := s.For("pos"); p.DB != DBHourly || p.Files != FilesDaily || p.KeepHours != 48 || p.Freeze {
		t.Errorf("pos = %+v", p)
	}
	if p := s.For("kasa"); !p.Freeze || p.KeepDays != 90 || p.KeepMonths != 6 || p.DB != DBDaily {
		t.Errorf("kasa = %+v", p)
	}
	if p := s.For("yok"); p != (BackupPolicy{DB: DBDaily, Files: FilesDaily, KeepHours: 48, KeepDays: 30, KeepMonths: 6}) {
		t.Errorf("default = %+v", p)
	}
	bad := DefaultBackupSettings()
	bad.Night = "4:00"
	bad.Defaults.DB = "weekly"
	bad.Defaults.Freeze = true
	bad.Defaults.KeepDays = 0
	bad.Sites = map[string]BackupPolicy{"../x": {}, "a": {Files: "sometimes", KeepHours: 100000}}
	err := bad.Validate()
	var ve *validationError
	if !errors.As(err, &ve) || len(ve.problems) != 7 {
		t.Fatalf("problems = %v", err)
	}
}

func TestCheckRepository(t *testing.T) {
	for repo, ok := range map[string]bool{
		"s3:https://acc.r2.cloudflarestorage.com/footops-h1":          true,
		"s3:https://acc.r2.cloudflarestorage.com/footops-h1/deneme/a": true,
		"s3:http://10.0.0.5:7070/footops-test":                        true,
		"s3:http://s3.example.com/footops-test":                       false, // plain http only on a private address
		"https://acc.r2.cloudflarestorage.com/footops-h1":             false,
		"s3:https://user:pw@acc.r2.cloudflarestorage.com/footops-h1":  false,
		"s3:https://acc.r2.cloudflarestorage.com/":                    false,
		"s3:https://acc.r2.cloudflarestorage.com/Kova":                false,
		"s3:https://acc.r2.cloudflarestorage.com/footops-h1/../x":     false,
		"s3:https://acc.r2.cloudflarestorage.com/footops-h1?x=1":      false,
		"s3:https://acc.r2.cloudflarestorage.com/footops,h1":          false,
	} {
		if err := checkRepository(repo); (err == nil) != ok {
			t.Errorf("%s: %v", repo, err)
		}
	}
	if l := repoLabel("s3:https://acc123.r2.cloudflarestorage.com/footops-h1/x"); l != "footops-h1/x" {
		t.Errorf("label %q", l)
	}
	repo := "s3:https://acc123.r2.cloudflarestorage.com/footops-h1"
	msg := "Is there a repository at the following location? s3:https://acc123.r2.cloudflarestorage.com/footops-h1 · " +
		"Get \"https://acc123.r2.cloudflarestorage.com/footops-h1/config\": dial tcp: lookup acc123.r2.cloudflarestorage.com: no such host"
	if got := maskRepo(repo, msg); strings.Contains(got, "acc123") || !strings.Contains(got, "location? footops-h1 ·") {
		t.Errorf("masked: %s", got)
	}
}

func TestRefusedLine(t *testing.T) {
	for line, want := range map[string]bool{
		"Load(<config/0000000000>, 0, 0) returned error, retrying after 720.5ms: The request signature we calculated does not match the signature you provided.": true,
		"Stat(<config/>) failed: Stat: Access Denied.":                                                                                             true,
		`{"message_type":"exit_error","code":1,"message":"Fatal: unable to open config file: Stat: Access Denied."}`:                               true,
		`{"message_type":"error","error":{"message":"open /footops/x/files/Access Denied.jpg: permission denied"},"during":"archival","item":"x"}`: false,
		"subprocess mongodump: 2026-11-06\tdone dumping `x.access denied` (3 documents)":                                                           false,
		"Load(<data/ab>) returned error, retrying after 1s: The specified bucket does not exist":                                                   true,
		"Fatal: wrong password or no key found":                                                                                                    false,
		"Stat(<config/>) returned error, retrying after 736.88ms: Stat: Unauthorized":                                                              true, // R2, unknown key ID
	} {
		if refusedLine(line) != want {
			t.Errorf("%q: want %v", line, want)
		}
	}
}

func TestJSONStreams(t *testing.T) {
	var got []string
	j := &jsonLines{each: func(b []byte) { got = append(got, string(b)) }}
	for _, p := range []string{`{"a":`, "1}\n{\"b\"", ":2}\n", `{"c":3}`} {
		j.Write([]byte(p))
	}
	j.flush()
	if strings.Join(got, "|") != `{"a":1}|{"b":2}|{"c":3}` {
		t.Errorf("lines = %q", got)
	}
	// A line beyond the limit is dropped, the next one is read.
	got = nil
	j.Write(append([]byte(strings.Repeat("x", maxJSONLine+10)), '\n'))
	j.Write([]byte("ok\n"))
	if len(got) != 1 || got[0] != "ok" {
		t.Errorf("after a long line: %d lines, first %.20q", len(got), got)
	}

	n := 0
	a := newJSONArray(func(raw json.RawMessage) error {
		var g struct{ Remove []json.RawMessage }
		if err := json.Unmarshal(raw, &g); err != nil {
			return err
		}
		n += len(g.Remove)
		return nil
	})
	body := `[{"remove":[{"id":"1"},{"id":"2"}]}, {"remove":null}, {"remove":[{"id":"3"}]}]` + "\n"
	for i := 0; i < len(body); i += 5 {
		a.Write([]byte(body[i:min(i+5, len(body))]))
	}
	if err := a.result(); err != nil || n != 3 {
		t.Errorf("array: n=%d err=%v", n, err)
	}
	// Garbage does not block the writer.
	b := newJSONArray(func(json.RawMessage) error { return nil })
	b.Write([]byte("not json at all " + strings.Repeat("y", 1<<20)))
	if err := b.result(); err == nil {
		t.Error("garbage decoded")
	}
}

func TestResticBinary(t *testing.T) {
	e := newBackups(t)
	tk := e.task("job1")
	p, err := e.b.resticBinary(context.Background(), tk)
	if err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, p)) != string(fakeBinary) || len(e.restic.ran("cat-binary")) != 1 {
		t.Fatalf("binary %s", p)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o700 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	// Checked, not taken out again.
	if _, err := e.b.resticBinary(context.Background(), tk); err != nil || len(e.restic.ran("cat-binary")) != 1 {
		t.Errorf("second call: %v %d", err, len(e.restic.ran("cat-binary")))
	}
	// A file that no longer matches is replaced; a binary that does not match the pin
	// is never used.
	os.WriteFile(p, []byte("tampered"), 0o700)
	if _, err := e.b.resticBinary(context.Background(), tk); err != nil || string(mustRead(t, p)) != string(fakeBinary) {
		t.Errorf("tampered binary not replaced: %v", err)
	}
	resticSHA256[runtime.GOARCH] = strings.Repeat("0", 64)
	os.Remove(p)
	if _, err := e.b.resticBinary(context.Background(), tk); err == nil || !strings.Contains(err.Error(), "özeti beklenenden farklı") {
		t.Errorf("mismatch: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("mismatching binary left in place")
	}
	if _, err := os.Stat(p + ".part"); !os.IsNotExist(err) {
		t.Error("partial binary left")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupSetup(t *testing.T) {
	ctx := context.Background()
	req := BackupSetup{Repository: "s3:https://acc123.r2.cloudflarestorage.com/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123="}
	if err := req.Check(); err != nil {
		t.Fatal(err)
	}

	t.Run("new repository", func(t *testing.T) {
		e := newBackups(t)
		var seen []string
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			seen = append(seen, ra[len(ra)-1])
			args := strings.Join(c.Args, " ")
			for _, secret := range []string{"KEYID12345", "SeCrEt/Key+123=", "acc123"} {
				if strings.Contains(args, secret) {
					t.Errorf("%q on the command line: %s", secret, args)
				}
			}
			cfg := filepath.Join(e.root, "staging/job1/cfg")
			if s3 := string(mustRead(t, filepath.Join(cfg, "s3"))); !strings.Contains(s3, "aws_access_key_id = KEYID12345\naws_secret_access_key = SeCrEt/Key+123=\n") {
				t.Errorf("credentials file: %q", s3)
			}
			if st, _ := os.Stat(filepath.Join(cfg, "password")); st.Mode().Perm() != 0o600 {
				t.Errorf("password file mode %v", st.Mode().Perm())
			}
			if ra[len(ra)-1] == "config" {
				c.Stderr(`{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file"}`)
				return exitErr{10}
			}
			return nil
		}
		tk := e.task("job1")
		outcome, err := e.b.Setup(ctx, tk, req, true)
		if err != nil || outcome != "created" || strings.Join(seen, ",") != "config,init" {
			t.Fatalf("%s %v %v\n%s", outcome, err, seen, strings.Join(tk.job.Steps, "\n"))
		}
		sec, err := e.b.secret()
		if err != nil || len(sec.Password) != 40 || sec.KeyID != "KEYID12345" {
			t.Fatalf("secret %+v %v", sec, err)
		}
		if st, _ := os.Stat(e.b.SecretPath); st.Mode().Perm() != 0o600 {
			t.Errorf("secret mode %v", st.Mode().Perm())
		}
		for _, s := range tk.job.Steps {
			if strings.Contains(s, sec.Password) || strings.Contains(s, "acc123") {
				t.Errorf("step shows a secret: %s", s)
			}
		}
		if s, _ := e.b.Settings(); !s.Confirmed.IsZero() {
			t.Error("a new password is confirmed before anyone saw it")
		}
		if _, err := os.Stat(filepath.Join(e.root, "staging/job1")); !os.IsNotExist(err) {
			t.Error("credentials left in staging")
		}
		if _, err := e.b.Run(ctx, e.task("job2"), runPlan{Kind: RunManual, Server: true}, Actor{}); !errors.Is(err, errNotConfirmed) {
			t.Errorf("run before confirming: %v", err)
		}
	})

	t.Run("existing repository", func(t *testing.T) {
		e := newBackups(t)
		r := req
		r.Password = "TheExistingOne-123"
		outcome, err := e.b.Setup(ctx, e.task("job1"), r, true)
		if err != nil || outcome != "connected" || len(e.restic.ran("init")) != 0 {
			t.Fatalf("%s %v", outcome, err)
		}
		if s, _ := e.b.Settings(); s.Confirmed.IsZero() {
			t.Error("a typed password needs no confirmation")
		}
	})

	t.Run("wrong or other password", func(t *testing.T) {
		e := newBackups(t)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			c.Stderr(`{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}`)
			return exitErr{12}
		}
		if _, err := e.b.Setup(ctx, e.task("job1"), req, true); err == nil || !strings.Contains(err.Error(), "başka bir şifreyle") {
			t.Errorf("fresh: %v", err)
		}
		r := req
		r.Password = "WrongOne-123"
		if _, err := e.b.Setup(ctx, e.task("job2"), r, true); err == nil || !strings.Contains(err.Error(), "şifresi yanlış") {
			t.Errorf("typed: %v", err)
		}
		if _, err := os.Stat(e.b.SecretPath); !os.IsNotExist(err) {
			t.Error("a failed setup saved settings")
		}
	})

	t.Run("bucket refuses the key", func(t *testing.T) {
		e := newBackups(t)
		e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
			c.Stderr("Load(<config/0000000000>, 0, 0) returned error, retrying after 552ms: The request signature we calculated does not match the signature you provided.")
			<-ctx.Done() // restic would keep retrying for about 15 minutes
			return ctx.Err()
		}
		start := time.Now()
		_, err := e.b.Setup(ctx, e.task("job1"), req, true)
		if err == nil || !strings.Contains(err.Error(), "erişimi reddetti") || !isFatal(err) || time.Since(start) > 5*time.Second {
			t.Errorf("%v (%s)", err, time.Since(start))
		}
	})

	t.Run("bucket unreachable", func(t *testing.T) {
		e := newBackups(t)
		old := setupProbe
		setupProbe = 100 * time.Millisecond
		defer func() { setupProbe = old }()
		e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
			c.Stderr("Stat(<config/>) returned error, retrying after 1s: Stat: Get \"https://acc123.r2.cloudflarestorage.com/footops-t1/config\": dial tcp: lookup acc123.r2.cloudflarestorage.com: no such host")
			<-ctx.Done()
			return ctx.Err()
		}
		_, err := e.b.Setup(ctx, e.task("job1"), req, true)
		if err == nil || !strings.Contains(err.Error(), "ulaşılamadı") || !strings.Contains(err.Error(), "no such host") || strings.Contains(err.Error(), "acc123") {
			t.Errorf("%v", err)
		}
	})

	t.Run("invalid requests", func(t *testing.T) {
		bad := BackupSetup{Repository: "s3:https://acc/footops", KeyID: "short", SecretKey: "has space", Password: " pad"}
		var ve *validationError
		if err := bad.Check(); !errors.As(err, &ve) || len(ve.problems) != 3 {
			t.Errorf("%v", err)
		}
	})
}

func TestBackupRunNight(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	ctx := context.Background()
	// A second site without a database or a data directory: its definition only.
	static := specFor("statik")
	static.Database, static.DataMount = false, ""
	static.Domains = []string{"statik.enisparta.com"}
	if err := e.ops.Create(ctx, static, nil, e.progress); err != nil {
		t.Fatal(err)
	}
	// What the server part reads.
	for _, p := range []string{"etc/footops/footops.json", "var/lib/footops/web/auth.json", "srv/core/caddy/etc/Caddyfile"} {
		os.MkdirAll(filepath.Join(e.root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(e.root, p), []byte("{}"), 0o600)
	}
	e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC) // a Friday
	p, err := e.b.Plan(RunDaily, "", e.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sites) != 2 || !p.Server || !p.Forget || !p.Stats || !p.Maintain /* never pruned yet */ {
		t.Fatalf("plan %+v", p)
	}
	pw := "RepoPassword0123456789abcdefghijklmnop"
	e.restic.check = func(ra []string, c run.Cmd) {
		args := strings.Join(c.Args, " ")
		for _, secret := range []string{pw, "SeCrEt/Key+123=", "KEYID12345", "acc123"} {
			if strings.Contains(args, secret) {
				t.Errorf("secret on the command line: %s", args)
			}
		}
		for _, want := range []string{"--log-driver none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges",
			"--memory 512m", "--cpus 0.50", "--pids-limit 256", "--env RESTIC_PASSWORD_FILE=/run/footops/password",
			"type=bind,source=" + filepath.Join(e.root, "staging/job1/cfg") + ",target=/run/footops,readonly",
			"type=bind,source=" + filepath.Join(e.root, "tools/restic-0.19.1") + ",target=/usr/local/bin/restic,readonly",
			"--entrypoint ionice mongo:7.0.45 -c 3 nice -n 10 restic"} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: lacks %q:\n%s", ra[0], want, args)
			}
		}
		db := indexOf(ra, "--stdin-from-command") > 0
		if db != strings.Contains(args, "--network container:footops-mongo") {
			t.Errorf("only the database part joins MongoDB's network: %s", args)
		}
		if ra[0] != "backup" {
			return
		}
		mongoCfg := filepath.Join(e.root, "staging/job1/cfg/mongo.yaml")
		_, err := os.Stat(mongoCfg)
		if db != (err == nil) {
			t.Errorf("the site's MongoDB credentials are there exactly while its dump runs (db=%v): %v", db, err)
		}
		switch {
		case db:
			if !strings.HasSuffix(strings.Join(ra, " "), "--stdin-filename /footops/magaza/db.archive --stdin-from-command -- sh -c "+
				`exec mongodump "$@" 2>>/tmp/mongodump-magaza.log mongodump --config=/run/footops/mongo.yaml --db=magaza --archive --numParallelCollections=1`) {
				t.Errorf("db: %v", ra)
			}
			if strings.Contains(args, "--gzip") {
				t.Error("gzip defeats deduplication")
			}
		case ra[len(ra)-1] == "/footops/magaza/files":
			if !strings.Contains(args, "--cap-add DAC_READ_SEARCH") ||
				!strings.Contains(args, "type=bind,source="+filepath.Join(e.root, "srv/musteri/magaza/data")+",target=/footops/magaza/files,readonly") {
				t.Errorf("files: %s", args)
			}
		case strings.HasSuffix(ra[len(ra)-1], "/def"):
			site := strings.Split(ra[len(ra)-1], "/")[2]
			for _, f := range []string{"footops.json", "compose.yaml"} {
				if !strings.Contains(args, "source="+filepath.Join(e.root, "srv/musteri", site, f)+",target=/footops/"+site+"/def/"+f+",readonly") {
					t.Errorf("def lacks %s: %s", f, args)
				}
			}
			if strings.Contains(args, "DAC_READ_SEARCH") {
				t.Error("def needs no capability")
			}
		case ra[len(ra)-1] == "/footops/_server":
			for _, want := range []string{"target=/footops/_server/etc,readonly", "target=/footops/_server/web,readonly", "target=/footops/_server/caddy/etc,readonly"} {
				if !strings.Contains(args, want) {
					t.Errorf("server lacks %s: %s", want, args)
				}
			}
			if strings.Contains(args, "target=/footops/_server/sources") {
				t.Error("a missing path is mounted")
			}
		}
		if i := indexOf(ra, "--tag"); i < 0 || !strings.Contains(ra[i+1], "kind=daily,run=job1") || !strings.HasPrefix(ra[i+1], "footops,site=") {
			t.Errorf("tags: %v", ra)
		}
		if i := indexOf(ra, "--host"); i < 0 || ra[i+1] != "T1" {
			t.Errorf("host: %v", ra)
		}
	}
	tk := e.task("job1")
	rec, err := e.b.Run(ctx, tk, p, schedActor)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(tk.job.Steps, "\n"))
	}
	var order []string
	for _, c := range e.restic.calls {
		order = append(order, c[0]+" "+c[len(c)-1])
	}
	want := "cat-binary cat-binary,unlock --remove-all,backup --numParallelCollections=1,backup /footops/magaza/files,backup /footops/magaza/def," +
		"backup /footops/statik/def,backup /footops/_server,snapshots 1,forget 6m,prune 10%,check 5%,stats raw-data"
	if strings.Join(order, ",") != want {
		t.Errorf("order:\n%s\nwant\n%s", strings.Join(order, ","), want)
	}
	if len(rec.Parts) != 5 || rec.Removed != 2 || !rec.Pruned || !rec.Checked || rec.RepoSize != 123456789 {
		t.Errorf("record %+v", rec)
	}
	db := rec.Parts[0]
	if db.Site != "magaza" || db.Part != PartDB || db.Documents != 13 || db.Collections["users"] != 12 || db.Collections["pages"] != 1 ||
		db.AddedPacked != 1200 || db.Bytes != 20000 || db.Snapshot == "" || db.Usage.PeakMem != 120<<20 {
		t.Errorf("db part %+v", db)
	}
	// forget: one policy, over every site and the server (tags OR), counted from each
	// group's newest.
	f := strings.Join(e.restic.ran("forget")[0], " ")
	if !strings.Contains(f, "--tag site=_server --tag site=magaza --tag site=statik --keep-within-hourly 48h --keep-within-daily 30d --keep-within-monthly 6m") {
		t.Errorf("forget: %s", f)
	}
	st := e.b.state()
	if st.NightDone != "2026-11-06" || len(st.Last) != 5 || st.Last["magaza/db"].Snapshot != db.Snapshot || st.RepoSize != 123456789 ||
		st.LastPrune.IsZero() || st.LastCheck.IsZero() || st.Repository == "" {
		t.Errorf("state %+v", st)
	}
	runs, err := e.b.Runs("magaza", 10)
	if err != nil || len(runs) != 1 || runs[0].ID != "job1" || runs[0].Parts[0].Documents != 13 {
		t.Errorf("runs %+v %v", runs, err)
	}
	if runs, _ := e.b.Runs("baska", 10); len(runs) != 0 {
		t.Errorf("filter: %+v", runs)
	}
	if _, err := os.Stat(filepath.Join(e.root, "staging/job1")); !os.IsNotExist(err) {
		t.Error("credentials left in staging")
	}
	if tk.job.Result != "5 parça, depoya 5.9 KB eklendi" {
		t.Errorf("result %q", tk.job.Result)
	}

	// The next night: locks were cleared once; prune waits a week.
	e.restic.calls, e.restic.check = nil, nil
	e.now = e.now.Add(24 * time.Hour)
	p, _ = e.b.Plan(RunDaily, "", e.now)
	if p.Maintain {
		t.Error("prune again after a day")
	}
	if _, err := e.b.Run(ctx, e.task("job2"), p, schedActor); err != nil {
		t.Fatal(err)
	}
	if len(e.restic.ran("unlock")) != 0 || len(e.restic.ran("prune")) != 0 {
		t.Errorf("second night: %v", e.restic.calls)
	}
}

func TestBackupRunFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("mongodump fails", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			if indexOf(ra, "--stdin-from-command") > 0 {
				dumpLog(c, ra, "2026-11-06T04:00:00.000+0000\tFailed: can't create session: auth error: unable to authenticate")
				c.Stderr(`{"message_type":"error","error":{"message":"failed to save /footops/magaza/db.archive: Fatal: command failed: exit status 1"},"during":"archival","item":"/footops/magaza/db.archive"}`)
				c.Stderr(`{"message_type":"exit_error","code":130,"message":"Fatal: unable to save snapshot: context canceled"}`)
				return exitErr{130}
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunManual, "magaza", e.now)
		tk := e.task("job1")
		rec, err := e.b.Run(ctx, tk, p, Actor{User: "furkan"})
		if err == nil || !strings.Contains(err.Error(), "1 işlem başarısız") || !strings.Contains(err.Error(), "mongodump başarısız, yedek alınmadı: Failed: can't create session") {
			t.Fatalf("%v\n%s", err, strings.Join(tk.job.Steps, "\n"))
		}
		if len(rec.Parts) != 3 || rec.Parts[0].Err == "" || rec.Parts[1].Snapshot == "" || rec.Parts[2].Snapshot == "" {
			t.Errorf("the other parts must still run: %+v", rec.Parts)
		}
		st := e.b.state()
		if _, ok := st.Last["magaza/db"]; ok || st.Last["magaza/files"].Snapshot == "" {
			t.Errorf("state %+v", st.Last)
		}
		if runs, _ := e.b.Runs("", 0); len(runs) != 1 || runs[0].Err == "" {
			t.Errorf("record of a failed run: %+v", runs)
		}
	})

	t.Run("repository unusable", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			c.Stderr(`{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file"}`)
			return exitErr{10}
		}
		e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
		p, _ := e.b.Plan(RunDaily, "", e.now)
		_, err := e.b.Run(ctx, e.task("job1"), p, schedActor)
		if err == nil || !isFatal(err) || !strings.Contains(err.Error(), "bulunamadı") {
			t.Fatalf("%v", err)
		}
		if len(e.restic.calls) != 2 { // the binary, then unlock: nothing after
			t.Errorf("went on after a fatal error: %v", e.restic.calls)
		}
		if st := e.b.state(); st.NightDone != "" {
			t.Error("a night that never reached the repository counts as done")
		}
		if !e.b.nightNotBefore.After(e.now.Add(50 * time.Minute)) {
			t.Errorf("retry at %v", e.b.nightNotBefore)
		}
	})

	t.Run("unreadable files", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			if ra[len(ra)-1] == "/footops/magaza/files" {
				c.Stderr(`{"message_type":"error","error":{"message":"open /footops/magaza/files/x: permission denied"},"during":"archival","item":"/footops/magaza/files/x"}`)
				e.restic.ok(ra, c)
				return exitErr{3}
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunManual, "magaza", e.now)
		rec, err := e.b.Run(ctx, e.task("job1"), p, Actor{})
		if err != nil {
			t.Fatal(err)
		}
		if f := rec.Parts[1]; f.Snapshot == "" || !strings.Contains(f.Warning, "permission denied") {
			t.Errorf("files part %+v", f)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		cctx, cancel := context.WithCancel(ctx)
		e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
			if indexOf(ra, "--stdin-from-command") > 0 {
				cancel()
				<-ctx.Done()
				return ctx.Err()
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunManual, "", e.now)
		if _, err := e.b.Run(cctx, e.task("job1"), p, Actor{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
		if n := len(e.restic.ran("backup")); n != 1 {
			t.Errorf("went on after the cancel: %d backups", n)
		}
	})
}

func TestBackupFreeze(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	s, _ := e.b.Settings()
	s.Sites = map[string]BackupPolicy{"magaza": {Freeze: true}}
	if err := e.b.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	var events []string
	copyDst := ""
	e.run.hook = func(cmd string) {
		events = append(events, strings.Fields(cmd)[0]+" "+strings.Fields(cmd)[1])
		if strings.HasPrefix(cmd, "cp -a -T -- ") {
			// While the site is paused: it is written down, and its directory is copied.
			if _, err := os.Stat(e.b.pausedPath()); err != nil {
				t.Errorf("the paused container is not written down while it is paused: %v", err)
			}
			f := strings.Fields(cmd)
			if f[4] != filepath.Join(e.root, "srv/musteri/magaza/data") {
				t.Errorf("copied %s", f[4])
			}
			copyDst = f[5]
		}
	}
	e.restic.check = func(ra []string, c run.Cmd) {
		if ra[0] == "backup" {
			events = append(events, "restic "+ra[len(ra)-1])
			if ra[len(ra)-1] == "/footops/magaza/files" {
				args := strings.Join(c.Args, " ")
				if !strings.Contains(args, "source="+copyDst+",target=/footops/magaza/files,readonly") || indexOf(ra, "--ignore-inode") < 0 {
					t.Errorf("the copy is not what is backed up: %s", args)
				}
			}
		}
	}
	p, _ := e.b.Plan(RunManual, "magaza", e.now)
	tk := e.task("job1")
	if _, err := e.b.Run(context.Background(), tk, p, Actor{}); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(tk.job.Steps, "\n"))
	}
	got := strings.Join(events, " | ")
	// The pause covers the copy only, never restic's network work.
	if !strings.Contains(got, "restic --numParallelCollections=1 | docker pause | cp -a | docker unpause | restic /footops/magaza/files | restic /footops/magaza/def") {
		t.Errorf("order: %s", got)
	}
	if copyDst != filepath.Join(e.root, "staging/job1/frozen-magaza") {
		t.Errorf("copy at %s", copyDst)
	}
	if _, err := os.Stat(e.b.pausedPath()); !os.IsNotExist(err) {
		t.Error("record of paused containers left")
	}

	// A copy that fails: unpaused at once, the part fails, nothing half copied is backed up.
	events = nil
	e.run.fail["cp -a"] = errors.New("cp: disk full")
	_, err := e.b.Run(context.Background(), e.task("job2"), p, Actor{})
	delete(e.run.fail, "cp -a")
	if err == nil || !strings.Contains(err.Error(), "dondurulmuş kopya alınamadı") || !strings.Contains(strings.Join(events, " | "), "cp -a | docker unpause") ||
		strings.Contains(strings.Join(events, " | "), "restic /footops/magaza/files") {
		t.Errorf("failed copy: %v %v", err, events)
	}

	// An unpause that fails stays written down: the next run tries again first.
	events = nil
	e.run.fail["docker unpause"] = errors.New("daemon busy")
	e.b.Run(context.Background(), e.task("job3"), p, Actor{})
	delete(e.run.fail, "docker unpause")
	if _, err := os.Stat(e.b.pausedPath()); err != nil {
		t.Fatal("a container that could not be unpaused was struck off")
	}
	events = nil
	e.b.Run(context.Background(), e.task("job4"), p, Actor{})
	if !strings.HasPrefix(strings.Join(events, " | "), "docker unpause") {
		t.Errorf("not retried first: %v", events)
	}

	// An agent that stopped while a site was paused unpauses it (at start and at stop).
	events = nil
	e.b.markPaused("magaza", true)
	e.b.Recover(context.Background())
	if strings.Join(events, " | ") != "docker unpause" {
		t.Errorf("recover: %v", events)
	}
	if _, err := os.Stat(e.b.pausedPath()); !os.IsNotExist(err) {
		t.Error("record left after recover")
	}

	// Already paused (an earlier run left it): not paused again, unpaused after.
	events = nil
	e.net.paused = true
	e.b.Run(context.Background(), e.task("job5"), p, Actor{})
	e.net.paused = false
	if got := strings.Join(events, " | "); strings.Contains(got, "docker pause") || !strings.Contains(got, "cp -a | docker unpause") {
		t.Errorf("already paused: %v", got)
	}

	// A stopped site is not paused, and its directory is backed up as it is.
	events = nil
	e.restic.reply, e.restic.check = nil, nil
	e.net.running = false
	e.b.Run(context.Background(), e.task("job6"), p, Actor{})
	if strings.Contains(strings.Join(events, " "), "pause") || strings.Contains(strings.Join(events, " "), "cp -a") {
		t.Errorf("paused a stopped site: %v", events)
	}
}

func TestBackupPlan(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	ctx := context.Background()
	for _, n := range []string{"pos", "ofis", "kapali"} {
		if err := e.ops.Create(ctx, func() sites.Spec { s := specFor(n); s.Domains = []string{n + ".enisparta.com"}; return s }(), nil, e.progress); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := e.b.Settings()
	s.Sites = map[string]BackupPolicy{"pos": {DB: DBHourly}, "ofis": {DB: DB6h, Files: FilesWeekly}, "kapali": {Off: true}}
	if err := e.b.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	names := func(p runPlan, part string) string {
		var out []string
		for _, sp := range p.Sites {
			if map[string]bool{PartDB: sp.DB, PartFiles: sp.Files, PartDef: sp.Def}[part] {
				out = append(out, sp.Spec.Name)
			}
		}
		return strings.Join(out, ",")
	}
	at := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 0, 0, 0, time.UTC) }
	// Friday 05:00: hourly only; 10:00 (night + 6 h): the six-hourly too.
	p, _ := e.b.Plan(RunHourly, "", at(6, 5))
	if names(p, PartDB) != "pos" || names(p, PartFiles) != "" || p.Server || p.Forget {
		t.Errorf("05:00 %+v", p)
	}
	if p, _ = e.b.Plan(RunHourly, "", at(6, 10)); names(p, PartDB) != "ofis,pos" || p.Target() != "saatlik yedek: ofis, pos" {
		t.Errorf("10:00 %s / %s", names(p, PartDB), p.Target())
	}
	// The night run: every database, daily files (weekly ones on Sunday), not the off site.
	p, _ = e.b.Plan(RunDaily, "", at(6, 4))
	if names(p, PartDB) != "magaza,ofis,pos" || names(p, PartFiles) != "magaza,pos" || names(p, PartDef) != "magaza,ofis,pos" || p.Target() != "gece yedeği" {
		t.Errorf("Friday night: db %s files %s def %s", names(p, PartDB), names(p, PartFiles), names(p, PartDef))
	}
	if p, _ = e.b.Plan(RunDaily, "", at(8, 4)); names(p, PartFiles) != "magaza,ofis,pos" || !p.Maintain {
		t.Errorf("Sunday night: files %s, maintain %v", names(p, PartFiles), p.Maintain)
	}
	// By hand: one site even if it is off; everything with the server.
	if p, _ = e.b.Plan(RunManual, "kapali", at(6, 12)); names(p, PartDB) != "kapali" || p.Server || p.Target() != "kapali" {
		t.Errorf("manual one: %+v", p)
	}
	if p, _ = e.b.Plan(RunManual, "", at(6, 12)); names(p, PartFiles) != "magaza,ofis,pos" || !p.Server || p.Forget || p.Target() != "tüm siteler" {
		t.Errorf("manual all: %+v", p)
	}
	if _, err := e.b.Plan(RunManual, "yok", at(6, 12)); !errors.Is(err, sites.ErrNotFound) {
		t.Errorf("unknown site: %v", err)
	}
}

func TestBackupTimetable(t *testing.T) {
	e := newBackups(t)
	block := make(chan struct{})
	e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
		if ra[0] == "backup" {
			<-block
		}
		return e.restic.ok(ra, c)
	}
	start := time.Date(2026, 11, 6, 1, 30, 0, 0, time.UTC)
	e.now = start
	e.b.init()
	if k, err := e.b.Tick(start.Add(3 * time.Hour)); k != "" || err != nil {
		t.Errorf("not configured: %s %v", k, err)
	}
	e.configure(t)
	s, _ := e.b.Settings()
	s.Sites = map[string]BackupPolicy{"magaza": {DB: DBHourly}}
	e.b.SetSettings(s)
	// 01:30 → the 02:00 slot (no night owed: nothing ran yesterday, but that is over 22 h ago).
	if d := e.b.nextWake(start); d != 30*time.Minute {
		t.Errorf("wake in %s", d)
	}
	waitJob := func() {
		t.Helper()
		for e.b.Jobs.Busy() {
			time.Sleep(time.Millisecond)
		}
	}
	// 02:00: the hourly run.
	e.now = start.Add(30 * time.Minute)
	if k, err := e.b.Tick(e.now); k != RunHourly || err != nil {
		t.Fatalf("02:00: %s %v", k, err)
	}
	// The same slot again (a second wake within the minute): nothing.
	if k, _ := e.b.Tick(e.now.Add(20 * time.Second)); k != "" {
		t.Errorf("slot ran twice")
	}
	close(block)
	waitJob()
	block = make(chan struct{})
	// 04:00: the night run; while it runs, an owed night is retried no sooner than in 5 minutes.
	e.now = start.Add(150 * time.Minute)
	if k, err := e.b.Tick(e.now); k != RunDaily || err != nil {
		t.Fatalf("04:00: %s %v", k, err)
	}
	if d := e.b.nextWake(e.now); d != nightRetry {
		t.Errorf("owed night wakes in %s", d)
	}
	if k, err := e.b.Tick(e.now.Add(nightRetry)); k != RunDaily || err != nil {
		t.Errorf("retry while running: %s %v", k, err) // ErrBusy is swallowed
	}
	close(block)
	waitJob()
	if st := e.b.state(); st.NightDone != "2026-11-06" {
		t.Fatalf("night not done: %+v", st)
	}
	if d := e.b.nextWake(e.now.Add(nightRetry)); d != 55*time.Minute {
		t.Errorf("after the night, the next slot: %s", d)
	}
	// Woken between slots (settings changed): nothing.
	if k, _ := e.b.Tick(e.now.Add(17 * time.Minute)); k != "" {
		t.Errorf("between slots: %s", k)
	}
	// Paused: nothing at all.
	s.Paused = true
	e.b.SetSettings(s)
	if k, _ := e.b.Tick(start.Add(210 * time.Minute)); k != "" {
		t.Errorf("paused: %s", k)
	}
	s.Paused = false
	e.b.SetSettings(s)

	// A night the server missed is caught up after the start grace.
	e2 := newBackups(t)
	boot := time.Date(2026, 11, 7, 9, 12, 0, 0, time.UTC)
	e2.now = boot
	e2.configure(t)
	if d := e2.b.nextWake(boot); d != startGrace {
		t.Errorf("catch-up in %s", d)
	}
	if k, _ := e2.b.Tick(boot.Add(time.Minute)); k != "" {
		t.Error("caught up within the start grace")
	}
	if k, err := e2.b.Tick(boot.Add(startGrace)); k != RunDaily || err != nil {
		t.Errorf("catch-up: %s %v", k, err)
	}
	// 03:30 with yesterday's night missed long ago: wait for tonight.
	e3 := newBackups(t)
	e3.now = time.Date(2026, 11, 7, 3, 30, 0, 0, time.UTC)
	e3.configure(t)
	if d := e3.b.nextWake(e3.now.Add(startGrace)); d != 28*time.Minute {
		t.Errorf("03:30: %s", d)
	}
}

func TestBackupKeyEndpoint(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	s, _ := e.b.Settings()
	s.Confirmed = time.Time{}
	e.b.mu.Lock()
	e.b.saveSettingsLocked(s)
	e.b.mu.Unlock()
	srv := &Server{Backups: e.b, Audit: e.b.Audit, Jobs: e.b.Jobs, Data: e.data,
		AllowedUIDs: map[uint32]bool{0: true, 1000: true}}
	var uid atomic.Uint32 // who connects next
	srv.PeerUID = func(net.Conn) (uint32, error) { return uid.Load(), nil }
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, keyUID, uid.Load())
	}
	ts.Start()
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}} // the peer is per connection
	get := func() (int, string) {
		resp, err := client.Get(ts.URL + "/v1/backup/key")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var k BackupKey
		json.NewDecoder(resp.Body).Decode(&k)
		return resp.StatusCode, k.Password
	}
	uid.Store(1000) // the panel, before the operator confirmed: shown
	if code, pw := get(); code != 200 || pw != "RepoPassword0123456789abcdefghijklmnop" {
		t.Errorf("panel before confirming: %d %q", code, pw)
	}
	e.b.Confirm()
	if code, pw := get(); code != 403 || pw != "" {
		t.Errorf("panel after confirming: %d %q", code, pw)
	}
	uid.Store(0) // root over SSH: always
	if code, pw := get(); code != 200 || pw == "" {
		t.Errorf("root: %d", code)
	}
	entries, _ := e.b.Audit.Tail(10)
	if len(entries) != 3 || entries[0].Action != "backup.key" || !entries[0].OK || entries[1].OK || entries[0].Actor.User != "root" {
		t.Errorf("audit %+v", entries)
	}
	for _, en := range entries {
		if strings.Contains(en.Detail+en.Target, "RepoPassword") {
			t.Error("password in the audit log")
		}
	}
}

func TestBackupRunsSweep(t *testing.T) {
	e := newBackups(t)
	for i, age := range []time.Duration{time.Hour, 30 * 24 * time.Hour, 70 * 24 * time.Hour} {
		e.b.saveRun(BackupRun{ID: fmt.Sprintf("r%d", i), Started: e.now.Add(-age)})
	}
	if got := e.b.SweepRuns(); len(got) != 1 || got[0] != "r2" {
		t.Errorf("swept %v", got)
	}
	if list, _ := e.b.Runs("", 0); len(list) != 2 || list[0].ID != "r0" {
		t.Errorf("left %+v", list)
	}
}

func TestReadDumpLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mongodump.log")
	var b strings.Builder
	b.WriteString("2026-11-06T04:00:00.000+0000\tdone dumping `m.early` (5 documents)\n")
	for b.Len() < 2<<20 {
		b.WriteString("2026-11-06T04:00:01.000+0000\t[########................]  m.big  1000/3000  (33.3%)\n")
	}
	b.WriteString("2026-11-06T04:00:09.000+0000\tdone dumping `m.big` (3000 documents)\n")
	b.WriteString("2026-11-06T04:00:09.000+0000\tdone dumping m.small (1 document)\n")
	os.WriteFile(p, []byte(b.String()), 0o600)
	counts, failure := readDumpLog(p)
	if counts["m.early"] != 5 || counts["m.big"] != 3000 || counts["m.small"] != 1 || failure != "" {
		t.Errorf("counts %v failure %q", counts, failure)
	}
	os.WriteFile(p, []byte("2026-11-06T04:00:00.000+0000\tFailed: error reading collection: connection reset\n"), 0o600)
	if _, failure := readDumpLog(p); failure != "Failed: error reading collection: connection reset" {
		t.Errorf("failure %q", failure)
	}
	if c, f := readDumpLog(p + ".yok"); c != nil || f != "" {
		t.Error("missing log")
	}
}

func TestBackupReplaceNeedsPassword(t *testing.T) {
	e := newBackups(t)
	ctx := context.Background()
	req := BackupSetup{Repository: "s3:https://evil.example.com/bucket", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=", Password: "TheirOwnPassword-1"}
	// Nothing set up yet: anyone allowed may set it up; keeping a repository needs one.
	if err := e.b.MayReplace(BackupSetup{}, false); err != nil {
		t.Fatal(err)
	}
	if err := e.b.MayReplace(BackupSetup{Keep: true}, false); err == nil {
		t.Error("kept a repository that is not there")
	}
	e.configure(t)
	// Set up: the panel needs the current password, root does not.
	if err := e.b.MayReplace(BackupSetup{}, false); !errors.Is(err, errReplace) {
		t.Errorf("panel without the password: %v", err)
	}
	if err := e.b.MayReplace(BackupSetup{Current: "wrong"}, false); !errors.Is(err, errReplace) {
		t.Errorf("panel with a wrong password: %v", err)
	}
	if err := e.b.MayReplace(BackupSetup{Current: "RepoPassword0123456789abcdefghijklmnop"}, false); err != nil {
		t.Errorf("panel with the password: %v", err)
	}
	if err := e.b.MayReplace(BackupSetup{}, true); err != nil {
		t.Errorf("root: %v", err)
	}
	// The same repository (its own password, the server's) needs none.
	if err := e.b.MayReplace(BackupSetup{Keep: true}, false); err != nil {
		t.Errorf("keep: %v", err)
	}
	// Not confirmed: the panel may read the password anyway, so it is not asked for.
	s, _ := e.b.Settings()
	s.Confirmed = time.Time{}
	e.b.mu.Lock()
	e.b.saveSettingsLocked(s)
	e.b.mu.Unlock()
	if err := e.b.MayReplace(BackupSetup{}, false); err != nil {
		t.Errorf("unconfirmed: %v", err)
	}
	e.b.Confirm()
	if _, err := e.b.Setup(ctx, e.task("job1"), req, false); !errors.Is(err, errReplace) || len(e.restic.calls) != 0 {
		t.Errorf("setup without the password: %v %v", err, e.restic.calls)
	}
	if sec, _ := e.b.secret(); sec.Repository != "s3:https://acc123.r2.cloudflarestorage.com/footops-t1" {
		t.Errorf("repository replaced: %s", sec.Repository)
	}

	// Through the API: the web process's uid is refused before any job starts.
	srv := &Server{Backups: e.b, Audit: e.b.Audit, Jobs: e.b.Jobs, Data: e.data, AllowedUIDs: map[uint32]bool{0: true, 1000: true}}
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, keyUID, uint32(1000))
	}
	ts.Start()
	defer ts.Close()
	body, _ := json.Marshal(BackupSetupRequest{BackupSetup: req})
	resp, err := http.Post(ts.URL+"/v1/backup/setup", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("panel replacing the repository: %d", resp.StatusCode)
	}
	if _, busy := e.b.Jobs.Running(); busy {
		t.Error("a job started")
	}
}

func TestBackupStaleLock(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	ctx := context.Background()
	locked := true
	e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
		if ra[0] == "forget" && locked {
			c.Stderr(`{"message_type":"exit_error","code":11,"message":"Fatal: unable to create lock in backend: repository is already locked by PID 1 on fo-it"}`)
			return exitErr{11}
		}
		if ra[0] == "unlock" {
			locked = false
		}
		return e.restic.ok(ra, c)
	}
	e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
	e.b.unlocked = true // as after an earlier run of this agent
	p, _ := e.b.Plan(RunDaily, "", e.now)
	tk := e.task("job1")
	rec, err := e.b.Run(ctx, tk, p, schedActor)
	if err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(tk.job.Steps, "\n"))
	}
	if n := len(e.restic.ran("forget")); n != 2 || len(e.restic.ran("unlock")) != 1 || rec.Removed != 2 {
		t.Errorf("forget %d, unlock %d, removed %d", n, len(e.restic.ran("unlock")), rec.Removed)
	}
	if st := e.b.state(); st.NightDone != "2026-11-06" {
		t.Errorf("night %q", st.NightDone)
	}
	// A lock that stays: fatal, not retried forever.
	e.restic.calls = nil
	locked = true
	e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
		if ra[0] == "forget" {
			return exitErr{11}
		}
		return e.restic.ok(ra, c)
	}
	_, err = e.b.Run(ctx, e.task("job2"), p, schedActor)
	if !isFatal(err) || len(e.restic.ran("forget")) != 2 {
		t.Errorf("lock that stays: %v, forget %d", err, len(e.restic.ran("forget")))
	}

	// A stopped restic leaves its lock: the next run removes it first.
	e.restic.calls, e.restic.reply = nil, nil
	e.b.unlocked = true
	cctx, cancel := context.WithCancel(ctx)
	e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
		if ra[0] == "backup" {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}
		return e.restic.ok(ra, c)
	}
	mp, _ := e.b.Plan(RunManual, "magaza", e.now)
	e.b.Run(cctx, e.task("job3"), mp, Actor{})
	if e.b.unlocked {
		t.Error("a stopped command's lock is not cleared before the next run")
	}
	e.restic.calls, e.restic.reply = nil, nil
	e.b.Run(ctx, e.task("job4"), mp, Actor{})
	if c := e.restic.calls; len(c) == 0 || c[0][0] != "unlock" {
		t.Errorf("next run: %v", c)
	}
}

func TestBackupNightBookkeeping(t *testing.T) {
	ctx := context.Background()
	t.Run("late catch-up counts for its night", func(t *testing.T) {
		e := newBackups(t)
		e.now = time.Date(2026, 11, 7, 0, 30, 0, 0, time.UTC) // yesterday's 04:00 run never ran
		e.configure(t)
		e.b.started = e.now.Add(-time.Hour)
		if k, err := e.b.Tick(e.now); k != RunDaily || err != nil {
			t.Fatalf("%s %v", k, err)
		}
		for e.b.Jobs.Busy() {
			time.Sleep(time.Millisecond)
		}
		if st := e.b.state(); st.NightDone != "2026-11-06" {
			t.Fatalf("recorded as %q", st.NightDone)
		}
		// Today's 04:00 run is still owed.
		if _, owed := e.b.nightOwed(DefaultBackupSettings(), time.Date(2026, 11, 7, 4, 0, 0, 0, time.UTC)); !owed {
			t.Error("today's night skipped")
		}
	})
	t.Run("cancelled night is not retried", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		p, _ := e.b.Plan(RunDaily, "", e.now)
		if _, err := e.b.Run(cctx, e.task("job1"), p, schedActor); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if st := e.b.state(); st.NightDone != "2026-11-06" {
			t.Errorf("cancelled night owed again: %q", st.NightDone)
		}
	})
	t.Run("nothing saved: recorded, tried again in an hour", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
		e.data.checkFree = func(string, int64) error { return errors.New("diskte yer yok") }
		p, _ := e.b.Plan(RunDaily, "", e.now)
		if _, err := e.b.Run(ctx, e.task("job1"), p, schedActor); err == nil {
			t.Fatal("ran without room")
		}
		if st := e.b.state(); st.NightDone != "" || !e.b.nightNotBefore.After(e.now.Add(50*time.Minute)) {
			t.Errorf("night %q, retry %v", st.NightDone, e.b.nightNotBefore)
		}
		if runs, _ := e.b.Runs("", 0); len(runs) != 1 || !strings.Contains(runs[0].Err, "yer yok") {
			t.Errorf("no record of the failure: %+v", runs)
		}
	})
	t.Run("every part failed: tried again", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			if ra[0] == "backup" {
				c.Stderr("Cannot connect to the Docker daemon")
				return exitErr{125}
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunDaily, "", e.now)
		e.b.Run(ctx, e.task("job1"), p, schedActor)
		if st := e.b.state(); st.NightDone != "" {
			t.Errorf("a night with nothing saved counted as done")
		}
	})
	t.Run("a snapshot from the future holds retention back", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.now = time.Date(2026, 11, 6, 4, 0, 0, 0, time.UTC)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			if ra[0] == "snapshots" {
				_, err := c.Stdout.Write([]byte(`[{"time":"2027-01-01T00:00:00Z"}]`))
				return err
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunDaily, "", e.now)
		_, err := e.b.Run(ctx, e.task("job1"), p, schedActor)
		if err == nil || !strings.Contains(err.Error(), "ileri tarihli") || len(e.restic.ran("forget")) != 0 {
			t.Errorf("%v, forget %d", err, len(e.restic.ran("forget")))
		}
	})
}

func TestBackupMessages(t *testing.T) {
	repo := "s3:http://10.0.0.5:7070/footops-it"
	if got := maskRepo(repo, "dial tcp: lookup 10.0.0.5: no route · Get http://10.0.0.5:7070/footops-it/config"); strings.Contains(got, "10.0.0.5") {
		t.Errorf("masked: %s", got)
	}
	// Exit 3 is "files unreadable" only for a backup.
	e := newBackups(t)
	e.configure(t)
	e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
		c.Stderr(`{"message_type":"exit_error","code":3,"message":"Fatal: failed to remove 1 snapshots"}`)
		return exitErr{3}
	}
	env, done, err := e.b.prepare(context.Background(), e.task("job1"), backupSecret{Repository: repo, Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	_, err = e.b.restic(context.Background(), e.task("job1"), env, resticOpts{out: io.Discard}, "forget", "--keep-last", "1")
	if err == nil || strings.Contains(err.Error(), "dosyalar") || !strings.Contains(err.Error(), "failed to remove") {
		t.Errorf("forget exit 3: %v", err)
	}
}

func TestBackupRunsNewestFirst(t *testing.T) {
	e := newBackups(t)
	for i := 0; i < 30; i++ {
		site := "a"
		if i%10 == 0 {
			site = "b"
		}
		e.b.saveRun(BackupRun{ID: fmt.Sprintf("r%02d", i), Started: e.now.Add(time.Duration(i) * time.Hour), Parts: []BackupPart{{Site: site}}})
	}
	list, _ := e.b.Runs("", 5)
	if len(list) != 5 || list[0].ID != "r29" || list[4].ID != "r25" {
		t.Errorf("newest: %v", ids(list))
	}
	if list, _ := e.b.Runs("b", 2); len(list) != 2 || list[0].ID != "r20" || list[1].ID != "r10" {
		t.Errorf("site: %v", ids(list))
	}
}

func ids(list []BackupRun) []string {
	var out []string
	for _, r := range list {
		out = append(out, r.ID)
	}
	return out
}

func alertKeys(list []Alert) string {
	var k []string
	for _, a := range list {
		k = append(k, a.Key+"="+a.Level)
	}
	return strings.Join(k, " ")
}

func TestBackupAlerts(t *testing.T) {
	ctx := context.Background()
	e := newBackups(t)
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:setup=warn" || !strings.Contains(got[0].Title, "kurulmadı") {
		t.Fatalf("not set up: %+v", got)
	}
	if err := saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:https://acc123.r2.cloudflarestorage.com/footops-t1", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:setup=warn" || !strings.Contains(got[0].Title, "onaylanmadı") {
		t.Fatalf("not confirmed: %+v", got)
	}
	// Confirmed after the site was made: nothing is owed for a day.
	specs, _ := e.data.Store.List()
	e.now = specs[0].CreatedAt.Add(time.Hour)
	e.configure(t)
	if got := e.b.Alerts(e.now.Add(25 * time.Hour)); len(got) != 0 {
		t.Fatalf("a day after setup: %s", alertKeys(got))
	}
	got := e.b.Alerts(e.now.Add(27 * time.Hour))
	if alertKeys(got) != "backup:stale=bad" || got[0].Title != "Yedek gecikti: magaza (veritabanı), magaza (dosyalar), magaza (site tanımı) ve 1 tane daha" ||
		!got[0].Since.Equal(e.now.Add(26*time.Hour)) {
		t.Fatalf("overdue: %+v", got)
	}

	backup := func(kind string, reply func(context.Context, []string, run.Cmd) error) error {
		t.Helper()
		e.restic.reply = reply
		p, err := e.b.Plan(kind, "", e.now)
		if err != nil {
			t.Fatal(err)
		}
		_, err = e.b.Run(ctx, e.task("job1"), p, Actor{})
		return err
	}
	e.now = e.now.Add(27 * time.Hour)
	if err := backup(RunManual, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.b.Alerts(e.now); len(got) != 0 {
		t.Fatalf("after a backup: %s", alertKeys(got))
	}

	// A failed database part: an alert of its own, not "overdue" too; the next clean
	// backup clears it.
	failDB := func(_ context.Context, ra []string, c run.Cmd) error {
		if indexOf(ra, "--stdin-from-command") > 0 {
			dumpLog(c, ra, "2026-11-06T04:00:00.000+0000\tFailed: connection refused")
			return exitErr{1}
		}
		return e.restic.ok(ra, c)
	}
	backup(RunManual, failDB)
	e.now = e.now.Add(30 * time.Hour)
	got = e.b.Alerts(e.now)
	if alertKeys(got) != "backup:failed=bad backup:stale=bad" || got[0].Title != "Yedek alınamadı: magaza (veritabanı)" ||
		!strings.Contains(got[0].Detail, "connection refused") || strings.Contains(got[1].Title, "veritabanı") {
		t.Fatalf("failed part: %+v", got)
	}
	// A cancelled run is no failure, and leaves what it cut short as it was.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	p, _ := e.b.Plan(RunManual, "", e.now)
	e.b.Run(cctx, e.task("job2"), p, Actor{})
	if st := e.b.state(); st.Failed["magaza/db"].Err == "" || st.RunFailed != nil {
		t.Errorf("cancel changed the failures: %+v %+v", st.Failed, st.RunFailed)
	}
	if err := backup(RunManual, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.b.Alerts(e.now); len(got) != 0 {
		t.Fatalf("after a clean backup: %s", alertKeys(got))
	}

	// Unreadable files: saved with a warning.
	backup(RunManual, func(_ context.Context, ra []string, c run.Cmd) error {
		if ra[len(ra)-1] == "/footops/magaza/files" {
			c.Stderr(`{"message_type":"error","error":{"message":"open /footops/magaza/files/x: permission denied"},"during":"archival","item":"/footops/magaza/files/x"}`)
			e.restic.ok(ra, c)
			return exitErr{3}
		}
		return e.restic.ok(ra, c)
	})
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:partial=warn" || !strings.Contains(got[0].Detail, "permission denied") {
		t.Fatalf("warning: %+v", got)
	}
	backup(RunManual, nil)

	// Nothing saved at all for a reason of the run's own; any saved part clears it.
	e.data.checkFree = func(string, int64) error { return errors.New("diskte yer yok") }
	backup(RunManual, nil)
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:failed=bad" || got[0].Title != "Yedek alınamadı" || got[0].Detail != "diskte yer yok" {
		t.Fatalf("run failure: %+v", got)
	}
	e.data.checkFree = func(string, int64) error { return nil }
	backup(RunManual, nil)
	if got := e.b.Alerts(e.now); len(got) != 0 {
		t.Fatalf("after the disk was freed: %s", alertKeys(got))
	}

	// Retention failed: an alert until a night whose retention runs clean.
	e.now = time.Date(e.now.Year(), e.now.Month(), e.now.Day(), 4, 0, 0, 0, time.UTC).Add(24 * time.Hour)
	backup(RunDaily, func(_ context.Context, ra []string, c run.Cmd) error {
		if ra[0] == "forget" {
			c.Stderr("Fatal: unable to remove snapshot")
			return exitErr{1}
		}
		return e.restic.ok(ra, c)
	})
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:maint=warn" || !strings.Contains(got[0].Detail, "saklama kuralı") {
		t.Fatalf("retention: %+v", got)
	}
	e.now = e.now.Add(24 * time.Hour)
	backup(RunDaily, nil)
	if got := e.b.Alerts(e.now); len(got) != 0 {
		t.Fatalf("after a clean night: %s", alertKeys(got))
	}

	// The repository past its threshold, a failed weekly check.
	e.b.updateState(func(st *backupState) {
		st.RepoSize, st.RepoSized = 9<<30, e.now
		st.CheckFailed, st.LastCheck = "pack abc: data corrupted", e.now
	})
	got = e.b.Alerts(e.now)
	if alertKeys(got) != "backup:check=bad backup:size=warn" || got[1].Title != "Yedek deposu 9.0 GB (uyarı eşiği 8.0 GB)" {
		t.Fatalf("size and check: %+v", got)
	}
	s, _ := e.b.Settings()
	s.WarnGB = 10
	if err := e.b.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	if got := e.b.Alerts(e.now); alertKeys(got) != "backup:check=bad" {
		t.Errorf("threshold raised: %s", alertKeys(got))
	}
	e.b.updateState(func(st *backupState) { st.CheckFailed = "" })

	// Paused: one alert, no "overdue" for the time it is paused.
	s.Paused = true
	e.b.SetSettings(s)
	if got := e.b.Alerts(e.now.Add(72 * time.Hour)); alertKeys(got) != "backup:paused=warn" {
		t.Errorf("paused: %s", alertKeys(got))
	}
	// A site left out owes nothing; failures of a site that is gone do not count.
	s.Paused = false
	s.Sites = map[string]BackupPolicy{"magaza": {Off: true}}
	e.b.SetSettings(s)
	e.b.updateState(func(st *backupState) {
		st.Failed["eski/db"] = BackupFailure{Time: e.now, Err: "x"}
		st.Last["_server/server"] = BackupLast{Time: e.now.Add(72 * time.Hour)}
	})
	if got := e.b.Alerts(e.now.Add(72 * time.Hour)); len(got) != 0 {
		t.Errorf("site left out, site gone: %s", alertKeys(got))
	}
}

func TestBackupAlertsInAlerter(t *testing.T) {
	a := NewAlerter()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	calls := 0
	a.Backups = func(t time.Time) []Alert {
		calls++
		return []Alert{{Key: "backup:setup", Level: "warn", Title: "Uzak yedekleme kurulmadı", Link: "/backup", Since: t}}
	}
	var opened []Alert
	a.OnChange = func(o, _ []Alert) { opened = append(opened, o...) }
	a.Evaluate(Overview{}, now)
	if got := a.Active(); len(got) != 1 || got[0].Key != "backup:setup" || len(opened) != 1 || calls != 1 {
		t.Fatalf("active %+v, opened %+v", got, opened)
	}
	a.Backups = func(time.Time) []Alert { return nil }
	a.Evaluate(Overview{}, now.Add(10*time.Second))
	if got := a.Active(); len(got) != 0 {
		t.Errorf("resolved alert still active: %+v", got)
	}
}

// A new repository's first night run is not held back by the old one's retry delay.
func TestBackupConfirmClearsRetryDelay(t *testing.T) {
	e := newBackups(t)
	e.configure(t)
	e.b.mu.Lock()
	e.b.nightNotBefore = e.now.Add(time.Hour)
	e.b.mu.Unlock()
	if err := e.b.Confirm(); err != nil {
		t.Fatal(err)
	}
	if !e.b.nightNotBefore.IsZero() {
		t.Errorf("retry delay kept: %v", e.b.nightNotBefore)
	}
}

// The same repository with a new key: no password typed, the confirmation (or its
// absence) and the state kept; another repository starts with a fresh state.
func TestBackupKeepAndNewRepository(t *testing.T) {
	ctx := context.Background()
	e := newBackups(t)
	e.configure(t)
	confirmed := e.b.mustSettings(t).Confirmed
	e.b.updateState(func(st *backupState) {
		st.Last["magaza/db"] = BackupLast{Time: e.now, Snapshot: "abc"}
		st.Failed["magaza/files"] = BackupFailure{Time: e.now, Err: "x"}
		st.NightDone, st.CheckFailed, st.RepoSize = "2026-10-27", "corrupt", 9<<30
	})
	e.now = e.now.Add(time.Hour)
	keep := BackupSetup{Repository: "s3:https://acc123.r2.cloudflarestorage.com/footops-t1", KeyID: "NEWKEY12345", SecretKey: "NewSecret/123=", Keep: true}
	if err := keep.Check(); err != nil {
		t.Fatal(err)
	}
	if out, err := e.b.Setup(ctx, e.task("job1"), keep, false); err != nil || out != "connected" {
		t.Fatalf("%s %v", out, err)
	}
	sec, _ := e.b.secret()
	if sec.KeyID != "NEWKEY12345" || sec.Password != "RepoPassword0123456789abcdefghijklmnop" {
		t.Errorf("secret %+v", sec)
	}
	if got := e.b.mustSettings(t).Confirmed; !got.Equal(confirmed) {
		t.Errorf("confirmation changed: %v → %v", confirmed, got)
	}
	if st := e.b.state(); st.Last["magaza/db"].Snapshot != "abc" || st.NightDone == "" {
		t.Errorf("state of the same repository lost: %+v", st)
	}

	// Unconfirmed, the same password passed back (by anyone) confirms nothing.
	s := e.b.mustSettings(t)
	s.Confirmed = time.Time{}
	e.b.mu.Lock()
	e.b.saveSettingsLocked(s)
	e.b.mu.Unlock()
	if _, err := e.b.Setup(ctx, e.task("job2"), keep, false); err != nil {
		t.Fatal(err)
	}
	typed := keep
	typed.Keep, typed.Password = false, sec.Password
	if _, err := e.b.Setup(ctx, e.task("job3"), typed, false); err != nil {
		t.Fatal(err)
	}
	if !e.b.mustSettings(t).Confirmed.IsZero() {
		t.Error("a password nobody saw got confirmed")
	}

	// Keep where this server's repository is not: refused, nothing saved.
	e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
		c.Stderr(`{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file"}`)
		return exitErr{10}
	}
	other := keep
	other.Repository = "s3:https://acc123.r2.cloudflarestorage.com/footops-yeni"
	if _, err := e.b.Setup(ctx, e.task("job4"), other, false); err == nil || !strings.Contains(err.Error(), "bu sunucunun deposu yok") {
		t.Errorf("keep at another address: %v", err)
	}
	if sec, _ := e.b.secret(); sec.Repository != keep.Repository {
		t.Errorf("repository replaced: %s", sec.Repository)
	}

	// A new repository: the old one's state is not this one's.
	e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
		if ra[len(ra)-1] == "config" {
			c.Stderr(`{"message_type":"exit_error","code":10,"message":"Fatal: repository does not exist: unable to open config file"}`)
			return exitErr{10}
		}
		return nil
	}
	fresh := other
	fresh.Keep = false
	if out, err := e.b.Setup(ctx, e.task("job5"), fresh, false); err != nil || out != "created" {
		t.Fatalf("new repository: %s %v", out, err)
	}
	st := e.b.state()
	if len(st.Last) != 0 || len(st.Failed) != 0 || st.NightDone != "" || st.CheckFailed != "" || st.RepoSize != 0 || st.Repository != fresh.Repository {
		t.Errorf("old state kept for a new repository: %+v", st)
	}
	bad := BackupSetup{Repository: keep.Repository, KeyID: "NEWKEY12345", SecretKey: "NewSecret/123=", Keep: true, Password: "x-password"}
	if err := bad.Check(); err == nil {
		t.Error("keep with a password")
	}
}

func (b *Backups) mustSettings(t *testing.T) BackupSettings {
	t.Helper()
	s, err := b.Settings()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// What the review of Y2 found in the bookkeeping: a check cut short, a cancel after a
// real failure, a failed prune, a site's own run that never reached a part.
func TestBackupBookkeepingEdges(t *testing.T) {
	ctx := context.Background()
	t.Run("a check cut short keeps the earlier finding", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.b.updateState(func(st *backupState) {
			st.Repository = "s3:https://acc123.r2.cloudflarestorage.com/footops-t1"
			st.CheckFailed, st.LastCheck = "pack x: data corrupted", e.now.Add(-7*24*time.Hour)
		})
		e.now = time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC) // a Sunday: prune and check
		cctx, cancel := context.WithCancel(ctx)
		e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
			if ra[0] == "check" {
				cancel()
				<-ctx.Done()
				return ctx.Err()
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunDaily, "", e.now)
		if _, err := e.b.Run(cctx, e.task("job1"), p, schedActor); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if st := e.b.state(); st.CheckFailed == "" || st.LastCheck.Equal(e.now) {
			t.Errorf("finding cleared by a check that never finished: %+v", st)
		}
		// The next night checks again (not a week later).
		if p, _ := e.b.Plan(RunDaily, "", e.now.Add(24*time.Hour)); !p.Maintain {
			t.Error("no check the next night")
		}
	})
	t.Run("a cancel keeps an earlier real failure", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		cctx, cancel := context.WithCancel(ctx)
		e.restic.reply = func(ctx context.Context, ra []string, c run.Cmd) error {
			if indexOf(ra, "--stdin-from-command") > 0 {
				dumpLog(c, ra, "2026-11-06T04:00:00.000+0000\tFailed: connection refused")
				return exitErr{1}
			}
			if ra[len(ra)-1] == "/footops/magaza/files" {
				cancel()
				<-ctx.Done()
				return ctx.Err()
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunManual, "magaza", e.now)
		e.b.Run(cctx, e.task("job1"), p, Actor{})
		st := e.b.state()
		if !strings.Contains(st.Failed["magaza/db"].Err, "connection refused") {
			t.Errorf("real failure dropped: %+v", st.Failed)
		}
		if _, ok := st.Failed["magaza/files"]; ok {
			t.Error("the part the cancel cut counts as failed")
		}
	})
	t.Run("a failed prune stays until a prune succeeds", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.now = time.Date(2026, 11, 8, 4, 0, 0, 0, time.UTC)
		e.restic.reply = func(_ context.Context, ra []string, c run.Cmd) error {
			if ra[0] == "prune" {
				c.Stderr("Fatal: pack not found")
				return exitErr{1}
			}
			return e.restic.ok(ra, c)
		}
		p, _ := e.b.Plan(RunDaily, "", e.now)
		e.b.Run(ctx, e.task("job1"), p, schedActor)
		if st := e.b.state(); st.PruneFailed == nil || st.ForgetFailed != nil {
			t.Fatalf("prune failure: %+v %+v", st.PruneFailed, st.ForgetFailed)
		}
		e.now = e.now.Add(24 * time.Hour) // Monday: not the weekly night
		e.restic.reply = nil
		p, _ = e.b.Plan(RunDaily, "", e.now)
		if !p.Maintain {
			t.Fatal("failed prune not tried again")
		}
		e.b.Run(ctx, e.task("job2"), p, schedActor)
		if st := e.b.state(); st.PruneFailed != nil {
			t.Errorf("still failed after a good prune: %+v", st.PruneFailed)
		}
	})
	t.Run("a site's run that reached no part is in its history", func(t *testing.T) {
		e := newBackups(t)
		e.configure(t)
		e.data.checkFree = func(string, int64) error { return errors.New("diskte yer yok") }
		p, _ := e.b.Plan(RunManual, "magaza", e.now)
		e.b.Run(ctx, e.task("job1"), p, Actor{})
		if runs, _ := e.b.Runs("magaza", 10); len(runs) != 1 || runs[0].Err == "" {
			t.Errorf("site history: %+v", runs)
		}
	})
	t.Run("a deadline is said in words", func(t *testing.T) {
		e := newBackups(t)
		bp := BackupPart{}
		e.b.failPart(e.task("job1"), &bp, context.DeadlineExceeded)
		if !strings.HasPrefix(bp.Err, "süre doldu") {
			t.Errorf("%q", bp.Err)
		}
	})
}

// A read error keeps the last alerts; a site made again does not inherit a failure;
// the Alerter keeps a condition's own, earlier Since.
func TestBackupAlertsSteady(t *testing.T) {
	e := newBackups(t)
	specs, _ := e.data.Store.List()
	e.now = specs[0].CreatedAt.Add(time.Hour)
	e.configure(t)
	e.b.updateState(func(st *backupState) {
		st.Failed["magaza/db"] = BackupFailure{Time: e.now, Err: "x"}
	})
	if got := alertKeys(e.b.Alerts(e.now)); got != "backup:failed=bad" {
		t.Fatalf("%s", got)
	}
	os.WriteFile(e.b.settingsPath(), []byte("{bozuk"), 0o600)
	got := e.b.Alerts(e.now)
	if alertKeys(got) != "backup:failed=bad backup:settings=warn" || !got[1].Since.IsZero() {
		t.Errorf("read error: %+v", got)
	}
	os.Remove(e.b.settingsPath())
	e.b.Confirm()
	e.b.updateState(func(st *backupState) {
		st.Failed["magaza/db"] = BackupFailure{Time: specs[0].CreatedAt.Add(-time.Hour), Err: "the old site's"}
	})
	if got := alertKeys(e.b.Alerts(e.now)); strings.Contains(got, "failed") {
		t.Errorf("a new site inherited a failure: %s", got)
	}

	a := NewAlerter()
	past := e.now.Add(-48 * time.Hour)
	a.Backups = func(time.Time) []Alert {
		return []Alert{{Key: "backup:stale", Level: "bad", Title: "Yedek gecikti", Link: "/backup", Since: past}}
	}
	a.Evaluate(Overview{}, e.now)
	if got := a.Active(); len(got) != 1 || !got[0].Since.Equal(past) {
		t.Errorf("since: %+v", got)
	}
}
