package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/install"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/update"
	"github.com/swbilisim/FootOps/internal/version"
)

// `footops self-update` (panel-tasarim §31.3): over SSH, what the panel's Güncelleme
// page does, through the agent — it readies the newest signed release, says what will
// change, and on a yes starts the update as the panel would (the job lock keeps it from
// cutting into a backup or an import); this follows its record until it ends (a dropped
// SSH session does not stop it). --check only looks, with no agent and no root.
//
// --apply is the update service's command, run by the installed binary, the one known
// to work: it keeps itself aside, lets the target binary apply its own standard
// (`install --auto`), waits for the new agent and panel to answer, and if they do not —
// or the target fails, or the service is stopped — puts its own standard back.

const (
	// healthWait is how long a new version has to answer; healthPasses the answers in a
	// row it must give (a panel that dies starting looks alive for a moment).
	healthWait   = 90 * time.Second
	healthPasses = 3
	// applyWait bounds the target's own steps, rollbackWait the going back (within the
	// unit's TimeoutStopSec).
	applyWait    = 20 * time.Minute
	rollbackWait = 12 * time.Minute
)

// releaseSource is GitHub's releases, or (the integration test's) a stand-in; the
// signature is checked either way.
func releaseSource() update.Source {
	base := os.Getenv("FOOTOPS_RELEASE_URL")
	if base == "" {
		base = update.DefaultBase
	}
	return update.Source{Base: base, HTTP: &http.Client{}, Keys: release.Trusted()}
}

func runSelfUpdate(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "only say whether a newer release is out")
	apply := fs.Bool("apply", false, "the update service's own command")
	target := fs.String("target", "", "")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	job := fs.String("job", "", "")
	back := fs.Bool("back", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *apply {
		return superviseUpdate(*target, *from, *to, *job, *back, stderr)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *check {
		lctx, lcancel := context.WithTimeout(ctx, time.Minute)
		defer lcancel()
		r, err := releaseSource().Latest(lctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Bu sunucu: footops %s\nEn yeni:   %s (%s, %s)\n", version.Version, r.Version, r.Commit, r.Date)
		if !release.Newer(r.Version, version.Version) {
			fmt.Fprintln(stdout, "Güncel: kurulacak yeni sürüm yok.")
			return nil
		}
		if r.Notes != "" {
			fmt.Fprintf(stdout, "\nYenilikler:\n  %s\n", strings.ReplaceAll(r.Notes, "\n", "\n  "))
		}
		return nil
	}
	if os.Geteuid() != 0 {
		return errors.New("root olarak çalıştırılmalı: sudo footops self-update (yalnız bakmak için: footops self-update --check)")
	}
	return updateThroughAgent(ctx, stdin, stdout)
}

// updateThroughAgent asks the agent to ready the newest release, shows it, and on a yes
// starts it the panel's way.
func updateThroughAgent(ctx context.Context, stdin io.Reader, out io.Writer) error {
	c := agent.NewClient(agent.DefaultSocket)
	actor := agent.Actor{User: os.Getenv("SUDO_USER"), Via: "ssh"}
	if actor.User == "" {
		actor.User = "root"
	}
	before, err := c.UpdateStatus(ctx, false)
	if err != nil {
		return fmt.Errorf("agent yanıt vermiyor (%v). Panel çalışmıyorsa yeni sürümü elle kur: "+
			"github.com/swbilisim/FootOps/releases → ikiliyi indir, SHA-256'sını footops-release.json'la karşılaştır, sudo ./footops install", err)
	}
	if before.Applying {
		return errors.New("bir güncelleme zaten sürüyor (panel: Sunucu → Güncelleme)")
	}
	if err := c.UpdateCheck(ctx, actor); err != nil {
		var ae *agent.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict {
			return err
		}
	}
	fmt.Fprintln(out, "Yeni sürüm denetleniyor…")
	var st agent.UpdateStatus
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if st, err = c.UpdateStatus(ctx, true); err == nil && !st.Checking && !st.Checked.Equal(before.Checked) {
			break
		}
		if i > 15*60 {
			return errors.New("denetim 15 dakikada bitmedi")
		}
	}
	fmt.Fprintf(out, "Bu sunucu: footops %s\n", st.Running)
	switch {
	case st.Unverified != "":
		return fmt.Errorf("en yeni sürümün imzası tutmuyor, kurulmaz: %s", st.Unverified)
	case st.CheckErr != "":
		return fmt.Errorf("denetlenemedi: %s", st.CheckErr)
	case !st.Newer || st.Latest == nil:
		fmt.Fprintln(out, "Güncel: kurulacak yeni sürüm yok.")
		return nil
	}
	m, r := st.Latest, st.Ready
	fmt.Fprintf(out, "En yeni:   %s (%s, %s)\n", m.Version, m.Commit, m.Date)
	if m.Notes != "" {
		fmt.Fprintf(out, "\nYenilikler:\n  %s\n", strings.ReplaceAll(m.Notes, "\n", "\n  "))
	}
	switch {
	case r == nil || r.Version != m.Version:
		return errors.New("sürüm indirilip denetlenemedi; panelde Sunucu → Güncelleme'ye bak")
	case r.PlanErr != "":
		return fmt.Errorf("güncellemenin planı çıkarılamadı: %s", r.PlanErr)
	case len(r.Plan.Interactive) > 0:
		fmt.Fprintf(out, "\nBu sürümle şunlar değişiyor ve terminalde soru soruyor: %s.\nKurmak için kurulumu bu sürümle çalıştır: sudo %s install\n",
			strings.Join(r.Plan.Interactive, ", "), r.Bin)
		return nil
	}
	if len(r.Plan.Impact) > 0 {
		fmt.Fprintln(out, "\nGüncellemede:")
		for _, l := range r.Plan.Impact {
			fmt.Fprintln(out, "  •", l)
		}
	}
	ok, err := install.NewPrompter(stdin, out, nil).YesNo("Şimdi güncellensin mi?", false)
	if err != nil || !ok {
		return err
	}
	started := time.Now()
	if _, err := c.UpdateApply(ctx, m.Version, actor); err != nil {
		return err
	}
	return followUpdate(ctx, out, m.Version, started)
}

// followUpdate shows the apply's log until its record says it has ended.
func followUpdate(ctx context.Context, out io.Writer, to string, started time.Time) error {
	var shown int
	deadline := time.Now().Add(50 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			fmt.Fprintln(out, "\nİzleme bırakıldı; güncelleme sürüyor. Sonucu: panelde Sunucu → Güncelleme.")
			return nil
		case <-time.After(time.Second):
		}
		r, err := update.ReadRun(update.Dir)
		if err != nil || r == nil || r.To != to || r.Started.Before(started.Add(-time.Second)) {
			continue
		}
		lines := update.Tail(r.Log, 10000)
		for _, l := range lines[min(shown, len(lines)):] {
			fmt.Fprintln(out, l)
		}
		shown = len(lines)
		if !r.Final() {
			continue
		}
		switch r.Phase {
		case update.PhaseDone:
			fmt.Fprintf(out, "\n✓ footops %s → %s\n", r.From, r.To)
			return nil
		case update.PhaseRolledBack:
			return fmt.Errorf("yeni sürüm açılmadı; önceki sürüm (%s) yeniden çalışıyor: %s", r.From, r.Err)
		case update.PhaseRefused:
			return fmt.Errorf("güncelleme yapılmadı (hiçbir şey değişmedi): %s", r.Err)
		}
		return fmt.Errorf("güncelleme yarıda kaldı ve geri dönülemedi: %s (kayıt: %s)", r.Err, r.Log)
	}
	return errors.New("güncelleme 50 dakikada bitmedi: kaydı oku (/var/lib/footops/update/)")
}

// superviseUpdate is the update service's command, run by the installed binary: it
// keeps itself aside, the target applies itself, and if the target fails, its agent or
// panel does not answer in time, or the service is stopped, the kept binary — this
// one's — puts its own standard back.
func superviseUpdate(target, from, to, job string, back bool, stderr io.Writer) error {
	signal.Ignore(syscall.SIGHUP)
	if target == "" || from == "" || to == "" {
		return errors.New("--target, --from ve --to gerekli")
	}
	// A stop (systemctl stop, the unit's time limit, a shutdown) ends the target's steps
	// and goes back; the going back itself is not stopped.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	fctx, abort := context.WithTimeout(context.Background(), applyWait)
	defer abort()
	go func() {
		select {
		case <-stop:
			abort()
		case <-fctx.Done():
		}
	}()
	dir := update.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("başka bir güncelleme sürüyor")
	}
	logPath := filepath.Join(filepath.Dir(target), "update.log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	// The log first: the journal (stderr) is a copy.
	w := io.MultiWriter(logf, stderr)
	rec := update.Run{Job: job, From: from, To: to, Back: back, Started: time.Now().UTC(), Phase: update.PhaseApplying, Log: logPath}
	if err := update.WriteRun(dir, rec); err != nil {
		return err
	}
	finish := func(phase string, err error) error {
		rec.Phase, rec.Finished = phase, time.Now().UTC()
		if err != nil {
			rec.Err = clip(err.Error(), 600)
		}
		if werr := update.WriteRun(dir, rec); werr != nil {
			fmt.Fprintf(w, "kayıt yazılamadı: %v\n", werr)
		}
		return err
	}
	fmt.Fprintf(w, "== %s · footops %s → %s%s\n", time.Now().Format("2006-01-02 15:04:05"), from, to,
		map[bool]string{true: " (önceki sürüme dönüş)"}[back])
	if err := keepInstalled(dir, from); err != nil {
		return finish(update.PhaseRefused, fmt.Errorf("kurulu ikili saklanamadı: %w", err))
	}
	code, err := runFootops(fctx, w, target, "install", "--auto")
	if code == update.ExitRefused {
		_ = os.RemoveAll(update.PendingDir(dir)) // nothing changed: the way back stays the earlier one
		return finish(update.PhaseRefused, errors.New("bu sürümle terminalde soru soran bir ayar değişiyor: sudo footops self-update"))
	}
	if err == nil {
		err = waitHealthy(fctx, to, w)
	}
	if err == nil && fctx.Err() == nil {
		fmt.Fprintf(w, "✓ footops %s çalışıyor\n", to)
		if back {
			_ = os.RemoveAll(update.PendingDir(dir)) // the version left is still released; the way back stays
		} else if perr := update.Promote(dir); perr != nil {
			fmt.Fprintf(w, "önceki sürüm geri dönüş yolu olarak saklanamadı: %v\n", perr)
		}
		return finish(update.PhaseDone, nil)
	}
	if err == nil {
		err = errors.New("güncelleme durduruldu")
	}
	fmt.Fprintf(w, "✗ %v\n", err)
	// Back to the kept binary: its own steps put its state back.
	fmt.Fprintf(w, "Önceki sürüme dönülüyor (%s)…\n", from)
	bctx, bcancel := context.WithTimeout(context.Background(), rollbackWait)
	defer bcancel()
	if _, rerr := runFootops(bctx, w, filepath.Join(update.PendingDir(dir), "footops"), "install", "--auto"); rerr != nil {
		fmt.Fprintf(w, "✗ önceki sürüme dönülemedi: %v\n", rerr)
		return finish(update.PhaseFailed, fmt.Errorf("%v; önceki sürüme dönülemedi: %v", err, rerr))
	}
	if rerr := waitHealthy(bctx, from, w); rerr != nil {
		fmt.Fprintf(w, "✗ önceki sürüm de açılmadı: %v\n", rerr)
		return finish(update.PhaseFailed, fmt.Errorf("%v; önceki sürüm de açılmadı: %v", err, rerr))
	}
	fmt.Fprintf(w, "✓ önceki sürüm (%s) yeniden çalışıyor\n", from)
	// It runs again from where it is installed: the earlier way back stays.
	_ = os.RemoveAll(update.PendingDir(dir))
	return finish(update.PhaseRolledBack, err)
}

// runFootops runs a footops binary with its output in the log; a cancelled ctx asks it
// to stop (SIGTERM: the installer stops between steps, never in one). It returns the
// exit status.
func runFootops(ctx context.Context, w io.Writer, bin string, args ...string) (int, error) {
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return cmd.ProcessState.ExitCode(), err
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-done:
		if err == nil {
			err = ctx.Err()
		}
		return cmd.ProcessState.ExitCode(), err
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		<-done
		return -1, fmt.Errorf("%s durmadı: öldürüldü", filepath.Base(bin))
	}
}

// keepInstalled copies the installed binary aside (prev.new/footops + version): the way
// back while the update runs.
func keepInstalled(dir, ver string) error {
	pdir := update.PendingDir(dir)
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		return err
	}
	src, err := os.Open(install.BinPath)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := filepath.Join(pdir, ".footops.tmp")
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	if _, err = io.Copy(dst, src); err == nil {
		err = dst.Sync()
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(pdir, "footops")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pdir, "version"), []byte(ver+"\n"), 0o600)
}

// waitHealthy waits for the agent to answer with ver and the panel to serve its login
// page, healthPasses times in a row a second apart.
func waitHealthy(ctx context.Context, ver string, w io.Writer) error {
	c := agent.NewClient(agent.DefaultSocket)
	web := &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.Now().Add(healthWait)
	last, passes := "yanıt yok", 0
	for time.Now().Before(deadline) {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := c.Server(qctx)
		cancel()
		switch {
		case err != nil:
			last = err.Error()
		case info.FootOps != ver:
			last = "agent " + info.FootOps + " diyor"
		default:
			last = panelAnswers(ctx, web)
		}
		if last == "" {
			if passes++; passes >= healthPasses {
				fmt.Fprintf(w, "agent %s yanıt veriyor, panel giriş sayfasını veriyor\n", ver)
				return nil
			}
		} else {
			passes = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s %d sn içinde açılmadı (%s)", ver, int(healthWait.Seconds()), last)
}

// panelAnswers asks the panel's local address for its login page: "" when it serves it.
func panelAnswers(ctx context.Context, c *http.Client) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+config.DefaultLocal+"/login", nil)
	if err != nil {
		return err.Error()
	}
	resp, err := c.Do(req)
	if err != nil {
		return "panel yanıt vermiyor: " + err.Error()
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "panel " + resp.Status + " veriyor"
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
