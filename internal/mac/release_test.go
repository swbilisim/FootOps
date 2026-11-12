package mac

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/release"
)

func releaseEnv(t *testing.T) (*env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errb bytes.Buffer
	e := &env{out: &out, errw: &errb, signing: fakeKeychain{items: map[string]string{}},
		terminal: func() bool { return true }, stdinTerminal: func() bool { return false },
		trusted: func() map[string]ed25519.PublicKey { return nil }}
	return e, &out, &errb
}

func TestReleaseKey(t *testing.T) {
	ctx := context.Background()
	e, out, _ := releaseEnv(t)
	if code := e.run(ctx, []string{"yayin", "anahtar"}); code != 0 {
		t.Fatalf("anahtar: %d", code)
	}
	seed := e.signing.(fakeKeychain).items[releaseItem]
	raw, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		t.Fatalf("stored seed %q", seed)
	}
	pub := ed25519.NewKeyFromSeed(raw).Public().(ed25519.PublicKey)
	id, b64 := release.KeyID(pub), base64.StdEncoding.EncodeToString(pub)
	if !strings.Contains(out.String(), "Kimlik: "+id) || !strings.Contains(out.String(), "Açık anahtar: "+b64) || strings.Contains(out.String(), seed) {
		t.Fatalf("output:\n%s", out)
	}
	// A second time it says what it has and makes none.
	out.Reset()
	if code := e.run(ctx, []string{"yayin", "anahtar"}); code != 0 || !strings.Contains(out.String(), "zaten var") ||
		e.signing.(fakeKeychain).items[releaseItem] != seed {
		t.Fatalf("again: %d\n%s", code, out)
	}

	// The backup line goes into a pipe only.
	out.Reset()
	if code := e.run(ctx, []string{"yayin", "anahtar", "yedek"}); code != 1 || out.Len() != 0 {
		t.Fatalf("backup to a terminal: %d %q", code, out)
	}
	e.terminal = func() bool { return false }
	if code := e.run(ctx, []string{"yayin", "anahtar", "yedek"}); code != 0 || out.String() != releaseSecret+seed+"\n" {
		t.Fatalf("backup: %d %q", code, out)
	}
	line := out.String()

	// A new Mac takes it back; a Mac with another key keeps its own.
	n, nout, _ := releaseEnv(t)
	n.in = strings.NewReader(line)
	if code := n.run(ctx, []string{"yayin", "anahtar", "yukle"}); code != 0 || n.signing.(fakeKeychain).items[releaseItem] != seed ||
		!strings.Contains(nout.String(), id) {
		t.Fatalf("load: %d %s", code, nout)
	}
	o, _, oerr := releaseEnv(t)
	_, other, _ := ed25519.GenerateKey(nil)
	o.signing.(fakeKeychain).items[releaseItem] = base64.StdEncoding.EncodeToString(other.Seed())
	o.in = strings.NewReader(line)
	if code := o.run(ctx, []string{"yayin", "anahtar", "yukle"}); code != 1 || !strings.Contains(oerr.String(), "başka bir imza anahtarı") {
		t.Fatalf("over another key: %d %s", code, oerr)
	}
	b, _, berr := releaseEnv(t)
	b.in = strings.NewReader("FOOTOPS-IMZA-1:kısa\n")
	if code := b.run(ctx, []string{"yayin", "anahtar", "yukle"}); code != 1 || !strings.Contains(berr.String(), "tanınmıyor") {
		t.Fatalf("a bad line: %d %s", code, berr)
	}
}

// fakeRepo answers yayin yap's commands like a clean, pushed main whose CI passed.
type fakeRepo struct {
	t        *testing.T
	root     string
	worktree string
	answer   map[string]string // command → output; "ERR …" fails
	calls    []string
}

func (f *fakeRepo) run(_ context.Context, dir, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	if name == "git" && len(args) > 2 && args[0] == "worktree" && args[1] == "add" {
		if err := os.MkdirAll(args[3], 0o755); err != nil {
			f.t.Fatal(err)
		}
		f.worktree = args[3]
		return "", nil
	}
	if name == "make" {
		if dir != f.worktree {
			f.t.Errorf("built in %s, not the worktree %s", dir, f.worktree)
		}
		d := filepath.Join(dir, "dist", "release")
		if err := os.MkdirAll(d, 0o755); err != nil {
			f.t.Fatal(err)
		}
		for _, a := range releaseArchs {
			if err := os.WriteFile(filepath.Join(d, release.BinaryName(a)), []byte("ikili "+a), 0o755); err != nil {
				f.t.Fatal(err)
			}
		}
		return "", nil
	}
	for prefix, out := range f.answer {
		if strings.HasPrefix(cmd, prefix) {
			if msg, ok := strings.CutPrefix(out, "ERR "); ok {
				return "", &codedError{text: msg}
			}
			return out, nil
		}
	}
	return "", nil
}

func newFakeRepo(t *testing.T) *fakeRepo {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(releaseModule+"\n\ngo 1.27.3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("ab", 20)
	return &fakeRepo{t: t, root: root, answer: map[string]string{
		"git rev-parse --show-toplevel":   root + "\n",
		"git status --porcelain":          "",
		"git rev-parse --abbrev-ref HEAD": "main\n",
		"git rev-parse HEAD":              sha + "\n",
		"git rev-parse origin/main":       sha + "\n",
		"git rev-parse --short=7 HEAD":    "ababab0\n",
		"git tag --list":                  "v0.1.0\nv0.0.9\n",
		"git log -1 --format=%ct":         "1762941600\n",
		"go env GOVERSION":                "go1.27.3\n",
		"gh run list":                     `[{"workflowName":"CI","status":"completed","conclusion":"success"}]`,
	}}
}

func TestReleaseMake(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(nil)
	notes := filepath.Join(t.TempDir(), "notlar.txt")
	if err := os.WriteFile(notes, []byte("Eski imajlar silinir.\nSağlık kontrolleri seyreldi.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup := func() (*env, *fakeRepo, *bytes.Buffer, *bytes.Buffer) {
		e, out, errb := releaseEnv(t)
		e.signing.(fakeKeychain).items[releaseItem] = base64.StdEncoding.EncodeToString(priv.Seed())
		e.trusted = func() map[string]ed25519.PublicKey { return map[string]ed25519.PublicKey{release.KeyID(pub): pub} }
		repo := newFakeRepo(t)
		e.command = repo.run
		return e, repo, out, errb
	}

	// A trial: built, signed and checked, nothing published.
	e, repo, out, errb := setup()
	if code := e.run(ctx, []string{"yayin", "yap", "v0.2.0", "--notlar", notes, "--deneme"}); code != 0 {
		t.Fatalf("trial: %d\n%s%s", code, out, errb)
	}
	dir := filepath.Join(repo.root, "dist", "release")
	m, err := verifyReleaseDir(dir, e.trusted())
	if err != nil || m.Version != "v0.2.0" || m.Commit != "ababab0" || m.Date != "2025-11-12T10:00:00Z" ||
		m.Notes != "Eski imajlar silinir.\nSağlık kontrolleri seyreldi." || len(m.Files) != 2 {
		t.Fatalf("manifest = %+v, %v", m, err)
	}
	for _, c := range repo.calls {
		if strings.HasPrefix(c, "git tag -a") || strings.HasPrefix(c, "git push") || strings.HasPrefix(c, "gh release") {
			t.Errorf("a trial ran %q", c)
		}
	}
	// Built from the commit in a worktree of its own, which goes afterwards.
	if all := strings.Join(repo.calls, "\n"); !strings.Contains(all, "git worktree add --detach "+repo.worktree+" "+strings.Repeat("ab", 20)) ||
		!strings.Contains(all, "git worktree remove --force "+repo.worktree) {
		t.Errorf("worktree calls:\n%s", all)
	}
	if !strings.Contains(out.String(), "Deneme: etiketlenmedi") {
		t.Errorf("output:\n%s", out)
	}

	// For real: the tag, its push, the release with the four files.
	e, repo, out, errb = setup()
	if code := e.run(ctx, []string{"yayin", "yap", "v0.2.0", "--notlar", notes}); code != 0 {
		t.Fatalf("release: %d\n%s%s", code, out, errb)
	}
	all := strings.Join(repo.calls, "\n")
	for _, want := range []string{"make release-build RELEASE=v0.2.0", "git tag -a v0.2.0 -m FootOps v0.2.0 " + strings.Repeat("ab", 20),
		"git push origin v0.2.0", "gh release create v0.2.0 " + filepath.Join(repo.root, "dist", "release", "footops_linux_amd64"),
		"footops-release.json.sig --title FootOps v0.2.0 --notes-file " + notes + " --verify-tag --latest"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in:\n%s", want, all)
		}
	}

	// Refused before anything is built.
	for name, c := range map[string]struct {
		args   []string
		answer map[string]string
		key    bool // the key is not a trusted one
		want   string
	}{
		"dirty tree":    {answer: map[string]string{"git status --porcelain": " M x.go\n"}, want: "temiz değil"},
		"other branch":  {answer: map[string]string{"git rev-parse --abbrev-ref HEAD": "u3\n"}, want: "main'den"},
		"not pushed":    {answer: map[string]string{"git rev-parse origin/main": "cd\n"}, want: "git push"},
		"tag exists":    {args: []string{"v0.1.0"}, want: "etiketi zaten var"},
		"older":         {args: []string{"v0.0.10"}, want: "yeni değil"},
		"other go":      {answer: map[string]string{"go env GOVERSION": "go1.27.4\n"}, want: "go1.27.3 istiyor"},
		"ci running":    {answer: map[string]string{"gh run list": `[{"workflowName":"CI","status":"in_progress","conclusion":""}]`}, want: "CI sürüyor"},
		"ci failed":     {answer: map[string]string{"gh run list": `[{"workflowName":"CI","status":"completed","conclusion":"failure"}]`}, want: "CI geçmedi"},
		"no ci":         {answer: map[string]string{"gh run list": `[]`}, want: "CI çalışmamış"},
		"untrusted key": {key: true, want: "koda gömülü değil"},
	} {
		e, repo, _, errb := setup()
		for k, v := range c.answer {
			repo.answer[k] = v
		}
		if c.key {
			e.trusted = func() map[string]ed25519.PublicKey { return nil }
		}
		ver := "v0.2.0"
		if len(c.args) > 0 {
			ver = c.args[0]
		}
		if code := e.run(ctx, []string{"yayin", "yap", ver, "--notlar", notes}); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%s: %d %s", name, code, errb)
		}
		for _, call := range repo.calls {
			if strings.HasPrefix(call, "git tag -a") || strings.HasPrefix(call, "gh release") {
				t.Errorf("%s: published", name)
			}
		}
	}
	// Usage: a version and notes.
	for _, args := range [][]string{{"yap", "0.2.0", "--notlar", notes}, {"yap", "v0.2.0"}, {"yap", "v0.2.0-1-gabc1234", "--notlar", notes}} {
		e, _, _, _ := setup()
		if code := e.run(ctx, append([]string{"yayin"}, args...)); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
}

// A release folder is not the Mac's: an unknown key ID in its signature line is quoted
// in the error, so its escape sequences cannot rewrite the Terminal's "✗" line.
func TestReleaseCheckQuotesUnknownKey(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, release.ManifestName), []byte("{\"format\":1}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, release.SigName), []byte("footops-imza-1 \x1b[2K\x1b[1G✓ AAAA\n"), 0o644)
	e, _, errb := releaseEnv(t)
	if code := e.run(context.Background(), []string{"yayin", "dogrula", dir}); code == 0 || bytes.Contains(errb.Bytes(), []byte{0x1b}) {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
}
