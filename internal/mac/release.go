package mac

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swbilisim/FootOps/internal/release"
)

// Releases (panel-tasarim §31.2): footops-mac yayin. The release key's private half is
// in this Mac's Keychain (service footops-imza) and, copied by hand, in Vaultwarden —
// nowhere else; servers trust its public half, built into FootOps. A release is built
// here from a clean, pushed commit whose CI passed, with the flags CI rebuilds it with,
// signed, checked, then tagged and published on GitHub.

const releaseUsage = `footops-mac yayin — FootOps sürümleri (yalnız FootOps'un kod klasöründe)

  anahtar                 imza anahtarı yoksa üretir ve Anahtar Zinciri'ne yazar (footops-imza);
                          açık anahtarı ve kimliğini yazar (koda: internal/release/keys.go)
  anahtar yedek           gizli anahtarı Vaultwarden için yazar — yalnız bir boruya:
                          footops-mac yayin anahtar yedek | pbcopy
  anahtar yukle           Vaultwarden'daki satırı stdin'den Anahtar Zinciri'ne yazar (yeni Mac)
  yap vX.Y.Z --notlar <dosya> [--deneme]
                          temiz, itilmiş ve CI'ı geçmiş main'den iki ikili (CI'ınkiyle aynı
                          bayt), manifest, imza, doğrulama; --deneme değilse etiket + GitHub Release
  dogrula <klasör>        bir sürüm klasörünü bu Mac'in güvendiği anahtarlarla doğrular
`

const (
	releaseItem   = "ed25519"
	releaseSecret = "FOOTOPS-IMZA-1:" // the Vaultwarden line: prefix + base64 seed
	releaseModule = "module github.com/swbilisim/FootOps"
)

var releaseArchs = []string{"amd64", "arm64"}

func (e *env) releaseCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errUsage
	}
	switch args[0] {
	case "anahtar":
		if len(args) == 1 {
			return e.releaseKey(ctx)
		}
		switch {
		case len(args) == 2 && args[1] == "yedek":
			return e.releaseKeyBackup(ctx)
		case len(args) == 2 && args[1] == "yukle":
			return e.releaseKeyLoad(ctx)
		}
		return errUsage
	case "yap":
		return e.releaseMake(ctx, args[1:])
	case "dogrula":
		if len(args) != 2 {
			return errUsage
		}
		m, err := verifyReleaseDir(args[1], e.trusted())
		if err != nil {
			return err
		}
		fmt.Fprintf(e.out, "✓ %s (%s): imza ve %d dosya tutuyor\n", m.Version, m.Commit, len(m.Files))
		return nil
	case "yardim", "help":
		fmt.Fprint(e.out, releaseUsage)
		return nil
	}
	return errUsage
}

// signingKey reads the private key from the Keychain.
func (e *env) signingKey(ctx context.Context) (ed25519.PrivateKey, error) {
	seed, err := e.signing.Get(ctx, releaseItem)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(seed)
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("Anahtar Zinciri'ndeki imza anahtarı bozuk (footops-imza / ed25519)")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}

func (e *env) releaseKey(ctx context.Context) error {
	priv, err := e.signingKey(ctx)
	if err == nil {
		pub := priv.Public().(ed25519.PublicKey)
		fmt.Fprintf(e.out, "İmza anahtarı zaten var (Anahtar Zinciri: footops-imza / %s).\nKimlik: %s\nAçık anahtar: %s\n",
			releaseItem, release.KeyID(pub), base64.StdEncoding.EncodeToString(pub))
		return nil
	}
	if !errors.Is(err, errNoItem) {
		return err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := e.signing.Set(ctx, releaseItem, base64.StdEncoding.EncodeToString(priv.Seed())); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "İmza anahtarı üretildi, Anahtar Zinciri'ne yazıldı (footops-imza / %s).\nKimlik: %s\nAçık anahtar: %s\n\n"+
		"Sonra:\n  1) Yedeği Vaultwarden'a: footops-mac yayin anahtar yedek | pbcopy → Vaultwarden'da yeni kayıt\n"+
		"     \"FootOps sürüm imza anahtarı\" → yapıştır → kaydet → pbcopy </dev/null\n"+
		"  2) Açık anahtar koda: internal/release/keys.go (builtinKeys)\n",
		releaseItem, release.KeyID(pub), base64.StdEncoding.EncodeToString(pub))
	return nil
}

// releaseKeyBackup writes the private key as one line — only into a pipe or a file.
func (e *env) releaseKeyBackup(ctx context.Context) error {
	if e.terminal() {
		return errors.New("gizli anahtar ekrana yazılmaz. Panoya al: footops-mac yayin anahtar yedek | pbcopy")
	}
	priv, err := e.signingKey(ctx)
	if err != nil {
		return err
	}
	if f, ok := e.out.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			if err := f.Chmod(0o600); err != nil {
				return fmt.Errorf("gizli anahtarın yazılacağı dosya yalnız sana açılamadı: %v", err)
			}
		}
	}
	fmt.Fprintln(e.out, releaseSecret+base64.StdEncoding.EncodeToString(priv.Seed()))
	fmt.Fprintf(e.errw, "Gizli anahtar yazıldı (kimlik %s). Vaultwarden'a yapıştırdıktan sonra panoyu temizle: pbcopy </dev/null\n",
		release.KeyID(priv.Public().(ed25519.PublicKey)))
	return nil
}

// releaseKeyLoad puts a key saved in Vaultwarden into this Mac's Keychain.
func (e *env) releaseKeyLoad(ctx context.Context) error {
	var line string
	var err error
	if e.stdinTerminal() {
		fmt.Fprint(e.errw, "Vaultwarden'daki satır (FOOTOPS-IMZA-1:…; ekranda görünmez): ")
		line, err = e.readHidden()
		fmt.Fprintln(e.errw)
	} else {
		line, err = bufio.NewReader(io.LimitReader(e.in, 4096)).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	seed, ok := strings.CutPrefix(strings.TrimSpace(line), releaseSecret)
	raw, derr := base64.StdEncoding.DecodeString(seed)
	if !ok || derr != nil || len(raw) != ed25519.SeedSize {
		return errors.New("satır tanınmıyor: FOOTOPS-IMZA-1: ile başlayan, footops-mac yayin anahtar yedek'in yazdığı satır olmalı")
	}
	key := ed25519.NewKeyFromSeed(raw)
	if have, err := e.signingKey(ctx); err == nil {
		if have.Equal(key) {
			fmt.Fprintln(e.out, "Bu anahtar zaten Anahtar Zinciri'nde.")
			return nil
		}
		return errors.New("Anahtar Zinciri'nde başka bir imza anahtarı var; üstüne yazılmaz")
	} else if !errors.Is(err, errNoItem) {
		return err
	}
	if err := e.signing.Set(ctx, releaseItem, seed); err != nil {
		return err
	}
	pub := key.Public().(ed25519.PublicKey)
	trusted := ""
	if _, ok := e.trusted()[release.KeyID(pub)]; !ok {
		trusted = " (dikkat: bu footops-mac'in güvendiği anahtarlardan değil)"
	}
	fmt.Fprintf(e.out, "İmza anahtarı Anahtar Zinciri'ne yazıldı; kimlik %s%s.\n", release.KeyID(pub), trusted)
	return nil
}

// releaseMake is yayin yap: everything is checked before anything is published.
func (e *env) releaseMake(ctx context.Context, args []string) error {
	var ver, notesPath string
	trial := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--deneme":
			trial = true
		case a == "--notlar" && i+1 < len(args):
			notesPath = args[i+1]
			i++
		case strings.HasPrefix(a, "-") || ver != "":
			return fmt.Errorf("%w: beklenmeyen: %s", errUsage, clip(a, 40))
		default:
			ver = a
		}
	}
	if v, ok := release.ParseVersion(ver); !ok || v.Ahead > 0 || v.Dirty {
		return fmt.Errorf("%w: sürüm vX.Y.Z olmalı", errUsage)
	}
	if notesPath == "" {
		return fmt.Errorf("%w: --notlar <dosya> gerekli (panelde ve Telegram'da görünen kısa açıklama)", errUsage)
	}
	notesRaw, err := os.ReadFile(notesPath)
	if err != nil {
		return err
	}
	notes := strings.TrimSpace(string(notesRaw))
	if notes == "" || len(notes) > release.MaxNotes || !utf8.ValidString(notes) {
		return fmt.Errorf("notlar boş, %d bayttan uzun ya da UTF-8 değil", release.MaxNotes)
	}

	// The commit: this repository's main, clean, the same as GitHub's, its CI passed.
	root, err := e.command(ctx, "", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("FootOps'un kod klasöründe çalıştır (git deposu bulunamadı)")
	}
	root = strings.TrimSpace(root)
	if mod, err := os.ReadFile(filepath.Join(root, "go.mod")); err != nil || !strings.Contains(string(mod), releaseModule+"\n") {
		return errors.New("bu klasör FootOps'un deposu değil")
	}
	git := func(args ...string) (string, error) {
		out, err := e.command(ctx, root, "git", args...)
		return strings.TrimSpace(out), err
	}
	if out, err := git("status", "--porcelain"); err != nil || out != "" {
		return fmt.Errorf("çalışma ağacı temiz değil (commit edilmemiş değişiklik var):\n%s", clip(out, 400))
	}
	if b, err := git("rev-parse", "--abbrev-ref", "HEAD"); err != nil || b != "main" {
		return fmt.Errorf("sürüm main'den yapılır (şu an: %s)", b)
	}
	if _, err := git("fetch", "--quiet", "--tags", "origin", "main"); err != nil {
		return fmt.Errorf("GitHub'a ulaşılamadı: %v", err)
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if remote, err := git("rev-parse", "origin/main"); err != nil || remote != head {
		return errors.New("main GitHub'dakiyle aynı değil: önce git push (ya da pull)")
	}
	tags, err := git("tag", "--list", "v*")
	if err != nil {
		return err
	}
	if err := newerThanTags(ver, strings.Fields(tags)); err != nil {
		return err
	}
	if err := e.goMatches(ctx, root); err != nil {
		return err
	}
	if err := e.ciPassed(ctx, root, head); err != nil {
		return err
	}

	// The build, the manifest, the signature — from the commit itself, in a worktree of
	// its own: an edit made meanwhile in this folder cannot get in.
	wt, err := os.MkdirTemp("", "footops-yayin-")
	if err != nil {
		return err
	}
	_ = os.Remove(wt) // git makes it
	if _, err := git("worktree", "add", "--detach", wt, head); err != nil {
		return fmt.Errorf("derleme klasörü açılamadı: %v", err)
	}
	defer func() { _, _ = git("worktree", "remove", "--force", wt) }()
	if _, err := e.command(ctx, wt, "make", "release-build", "RELEASE="+ver); err != nil {
		return fmt.Errorf("derleme: %v", err)
	}
	short, err := git("rev-parse", "--short=7", "HEAD")
	if err != nil {
		return err
	}
	ct, err := git("log", "-1", "--format=%ct")
	if err != nil {
		return err
	}
	secs, err := strconv.ParseInt(ct, 10, 64)
	if err != nil {
		return fmt.Errorf("commit zamanı okunamadı: %q", ct)
	}
	dir := filepath.Join(wt, "dist", "release")
	m := release.Manifest{Format: release.Format, Version: ver, Commit: short,
		Date: time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05Z"), Files: map[string]release.File{}, Notes: notes}
	for _, arch := range releaseArchs {
		f, err := release.FileOf(filepath.Join(dir, release.BinaryName(arch)))
		if err != nil {
			return err
		}
		m.Files[release.BinaryName(arch)] = f
	}
	priv, err := e.signingKey(ctx)
	if errors.Is(err, errNoItem) {
		return errors.New("imza anahtarı yok: footops-mac yayin anahtar (ya da Vaultwarden'dan: anahtar yukle)")
	}
	if err != nil {
		return err
	}
	if _, ok := e.trusted()[release.KeyID(priv.Public().(ed25519.PublicKey))]; !ok {
		return errors.New("bu imza anahtarı koda gömülü değil (internal/release/keys.go): sunucular bu imzaya güvenmez")
	}
	if err := writeRelease(dir, m, priv); err != nil {
		return err
	}
	if _, err := verifyReleaseDir(dir, e.trusted()); err != nil {
		return fmt.Errorf("kendi doğrulaması tutmadı: %v", err)
	}
	// Kept in this folder's dist/release (what is published), checked again there.
	out := filepath.Join(root, "dist", "release")
	if err := copyRelease(dir, out); err != nil {
		return err
	}
	if _, err := verifyReleaseDir(out, e.trusted()); err != nil {
		return fmt.Errorf("kopyası doğrulanamadı: %v", err)
	}
	dir = out
	fmt.Fprintf(e.out, "%s (%s, %s) hazır: %s\n", ver, short, m.Date, dir)
	for _, arch := range releaseArchs {
		f := m.Files[release.BinaryName(arch)]
		fmt.Fprintf(e.out, "  %s  %s  %s\n", release.BinaryName(arch), f.SHA256, sizeTR(uint64(f.Size)))
	}
	if trial {
		fmt.Fprintln(e.out, "Deneme: etiketlenmedi, yayınlanmadı.")
		return nil
	}

	// Published: the tag, then the release (marked latest: the servers look there).
	if _, err := git("tag", "-a", ver, "-m", "FootOps "+ver, head); err != nil {
		return err
	}
	if _, err := git("push", "origin", ver); err != nil {
		return fmt.Errorf("etiket GitHub'a gönderilemedi (yerelde var: git push origin %s): %v", ver, err)
	}
	files := []string{}
	for _, arch := range releaseArchs {
		files = append(files, filepath.Join(dir, release.BinaryName(arch)))
	}
	files = append(files, filepath.Join(dir, release.ManifestName), filepath.Join(dir, release.SigName))
	ghArgs := append([]string{"release", "create", ver}, files...)
	ghArgs = append(ghArgs, "--title", "FootOps "+ver, "--notes-file", notesPath, "--verify-tag", "--latest")
	if _, err := e.command(ctx, root, "gh", ghArgs...); err != nil {
		return fmt.Errorf("GitHub Release oluşturulamadı (etiket gönderildi): %v", err)
	}
	fmt.Fprintf(e.out, "Yayınlandı: https://github.com/swbilisim/FootOps/releases/tag/%s\n"+
		"CI aynı koddan yeniden derleyip karşılaştırıyor: gh run list --workflow release.yml\n", ver)
	return nil
}

// newerThanTags: a release comes after every one before it.
func newerThanTags(ver string, tags []string) error {
	v, _ := release.ParseVersion(ver)
	for _, t := range tags {
		if t == ver {
			return fmt.Errorf("%s etiketi zaten var", ver)
		}
		if tv, ok := release.ParseVersion(t); ok && tv.Compare(v) >= 0 {
			return fmt.Errorf("%s, var olan %s sürümünden yeni değil", ver, t)
		}
	}
	return nil
}

var reGoDirective = regexp.MustCompile(`(?m)^go ([0-9.]+)$`)

// goMatches: CI builds with go.mod's Go; another one would make other bytes.
func (e *env) goMatches(ctx context.Context, root string) error {
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	m := reGoDirective.FindSubmatch(mod)
	if m == nil {
		return errors.New("go.mod'da go satırı yok")
	}
	have, err := e.command(ctx, root, "go", "env", "GOVERSION")
	if err != nil {
		return err
	}
	if want := "go" + string(m[1]); strings.TrimSpace(have) != want {
		return fmt.Errorf("bu Mac'te %s var, go.mod %s istiyor: CI başka bayt üretir", strings.TrimSpace(have), want)
	}
	return nil
}

// ciPassed: every workflow run of the commit finished well, CI's among them.
func (e *env) ciPassed(ctx context.Context, root, sha string) error {
	out, err := e.command(ctx, root, "gh", "run", "list", "--commit", sha, "--json", "workflowName,status,conclusion", "--limit", "20")
	if err != nil {
		return fmt.Errorf("CI durumu okunamadı (gh): %v", err)
	}
	var runs []struct {
		WorkflowName string `json:"workflowName"`
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
	}
	if err := json.Unmarshal([]byte(out), &runs); err != nil {
		return fmt.Errorf("CI durumu okunamadı: %v", err)
	}
	ci := false
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			return fmt.Errorf("CI sürüyor (%s): bitince yeniden dene", r.WorkflowName)
		case r.Conclusion != "success":
			return fmt.Errorf("CI geçmedi (%s: %s)", r.WorkflowName, r.Conclusion)
		}
		ci = ci || r.WorkflowName == "CI"
	}
	if !ci {
		return errors.New("bu commit için CI çalışmamış")
	}
	return nil
}

// writeRelease writes the manifest and its signature next to the binaries.
func writeRelease(dir string, m release.Manifest, priv ed25519.PrivateKey) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.WriteFile(filepath.Join(dir, release.ManifestName), b, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, release.SigName), release.Sign(priv, b), 0o644)
}

// copyRelease puts a release's files (the binaries, the manifest, its signature) into
// a fresh folder.
func copyRelease(from, to string) error {
	if err := os.RemoveAll(to); err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	names := []string{release.ManifestName, release.SigName}
	for _, arch := range releaseArchs {
		names = append(names, release.BinaryName(arch))
	}
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(from, n))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, n), b, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// verifyReleaseDir checks a release folder with keys.
func verifyReleaseDir(dir string, keys map[string]ed25519.PublicKey) (release.Manifest, error) {
	man, err := os.ReadFile(filepath.Join(dir, release.ManifestName))
	if err != nil {
		return release.Manifest{}, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, release.SigName))
	if err != nil {
		return release.Manifest{}, err
	}
	m, err := release.Verify(man, sig, keys)
	if err != nil {
		return m, err
	}
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := m.Files[n].Check(filepath.Join(dir, n)); err != nil {
			return m, fmt.Errorf("%s: %w", n, err)
		}
	}
	return m, nil
}

// runCommand runs a tool for yayin yap, in dir; its output, or its last words as the error.
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %s", name, strings.Join(args[:min(2, len(args))], " "), clip(firstLine(lastLines(stderr.String(), 3), err.Error()), 300))
	}
	return stdout.String(), nil
}

// lastLines keeps a command's last n lines (where it says why it failed).
func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(l[max(0, len(l)-n):], " · ")
}
