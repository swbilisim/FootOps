package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	configDir = "/etc/footops"
	LogPath   = "/var/log/footops-install.log"
)

// Options configure an installer run.
type Options struct {
	DryRun bool // only report what would change
	Out    io.Writer
	Prompt *Prompter
	Host   Host
	// StepsStarting is called once all questions are answered, right before the host is
	// changed. The CLI uses it to switch Ctrl+C from "quit now" to "stop after the
	// running command".
	StepsStarting func()
}

// Steps returns the ordered host preparation steps for the given answers.
// SSH hardening is last: by then the admin user, firewall and fail2ban are in place,
// and the operator has had time to verify key login from a second terminal.
func Steps(h Host, a Answers, confirm ConfirmFunc) []Step {
	return []Step{
		packagesStep{h},
		upgradeStep{h},
		hostnameStep{h, a.Hostname},
		timezoneStep{h, a.Timezone},
		swapStep{h, a.SwapBytes},
		unattendedStep{h, a.AutoReboot, a.RebootTime},
		adminUserStep{h, a.AdminUser, a.SSHKeys, a.SudoMode, a.SudoPasswordValue},
		firewallStep{h},
		fail2banStep{h},
		sshStep{h, a.AdminUser, confirm},
	}
}

// Main runs preflight, the wizard (or resumes saved answers) and the steps.
func Main(ctx context.Context, o Options) error {
	mem, err := Preflight(o.Host)
	if err != nil {
		return err
	}

	a, err := loadOrCollect(ctx, o, mem.Total)
	if err != nil {
		return err
	}

	steps := Steps(o.Host, a, confirmKeyLogin(o))
	if o.DryRun {
		return Plan(ctx, steps, o.Out)
	}

	if o.StepsStarting != nil {
		o.StepsStarting()
	}
	if err := waitForCloudInit(ctx, o); err != nil {
		return err
	}
	if _, err := Run(ctx, steps, o.Out); err != nil {
		return err
	}
	if o.Host.Exists("/var/run/reboot-required") {
		fmt.Fprintln(o.Out, "\nNot: güncellemeler yeniden başlatma istiyor. Kurulum bitince `sudo reboot` önerilir.")
	}
	fmt.Fprintln(o.Out, "\nSunucu hazırlığı tamamlandı. Sıradaki aşama (Docker, Caddy, MongoDB, panel) henüz yazılmadı.")
	return nil
}

func loadOrCollect(ctx context.Context, o Options, ram uint64) (Answers, error) {
	saved, err := o.Host.ReadFile(AnswersPath)
	if err != nil {
		return Answers{}, err
	}
	if saved != "" {
		a, err := UnmarshalAnswers(saved)
		if err == nil && a.Validate() == nil {
			o.Prompt.Say("Önceki kurulumun cevapları bulundu (%s).", AnswersPath)
			o.Prompt.Say("%s", Summary(a))
			reuse, err := o.Prompt.YesNo("Bu cevaplarla devam edilsin mi?", true)
			if err != nil {
				return Answers{}, err
			}
			if reuse {
				return withResumedSecrets(ctx, o, a)
			}
		}
	}

	a, err := Collect(o.Prompt, gatherFacts(o.Host, ram))
	if err != nil {
		return Answers{}, err
	}
	if o.DryRun {
		return a, nil
	}
	if err := os.MkdirAll(o.Host.Path(configDir), 0o700); err != nil {
		return Answers{}, err
	}
	if err := os.Chmod(o.Host.Path(configDir), 0o700); err != nil {
		return Answers{}, err
	}
	data, err := a.Marshal()
	if err != nil {
		return Answers{}, err
	}
	return a, o.Host.WriteFile(AnswersPath, data, 0o600)
}

// withResumedSecrets asks again for secrets that are never persisted, but only if the
// step that needs them has not completed yet.
func withResumedSecrets(ctx context.Context, o Options, a Answers) (Answers, error) {
	if a.SudoMode != SudoPassword || o.DryRun {
		return a, nil
	}
	step := adminUserStep{h: o.Host, user: a.AdminUser, keys: a.SSHKeys, mode: a.SudoMode}
	if step.passwordSet(ctx) {
		return a, nil
	}
	pw, err := o.Prompt.NewPassword("sudo şifresi ("+a.AdminUser+")", ValidateSudoPassword)
	a.SudoPasswordValue = pw
	return a, err
}

func gatherFacts(h Host, ram uint64) Facts {
	f := Facts{
		RAM:        ram,
		ZoneinfoOK: func(tz string) bool { return h.Exists("/usr/share/zoneinfo/" + tz) },
	}
	if name, err := h.ReadFile("/etc/hostname"); err == nil {
		f.Hostname = strings.TrimSpace(name)
	}
	if keys, err := h.ReadFile("/root/.ssh/authorized_keys"); err == nil {
		for _, l := range strings.Split(keys, "\n") {
			// Lines with options (from="...", command="...") are skipped: ParseSSHKey
			// expects the key type first, and such keys should not be copied blindly.
			if _, err := ParseSSHKey(l); err == nil {
				f.RootSSHKeys = append(f.RootSSHKeys, strings.TrimSpace(l))
			}
		}
	}
	return f
}

// waitForCloudInit lets a freshly booted cloud server finish its first-boot setup, which
// holds apt locks and may rewrite files the installer is about to change.
//
// It only waits while cloud-init reports "running": a cloud-init that is disabled or
// never started (no systemd, not a cloud machine) would make `status --wait` block
// until the timeout.
func waitForCloudInit(ctx context.Context, o Options) error {
	if _, err := o.Host.Run.Run(ctx, "sh", "-c", "command -v cloud-init"); err != nil {
		return ctx.Err()
	}
	// Exit status is non-zero for error/degraded states; only the text matters here.
	out, _ := o.Host.Run.Run(ctx, "cloud-init", "status")
	if !cloudInitRunning(out) {
		return ctx.Err()
	}
	fmt.Fprintln(o.Out, "cloud-init'in ilk açılış işlerini bitirmesi bekleniyor (en fazla 5 dakika)...")
	_, _ = o.Host.Run.Run(ctx, "timeout", "300", "cloud-init", "status", "--wait")
	return ctx.Err()
}

func cloudInitRunning(status string) bool {
	for _, l := range strings.Split(status, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "status" {
			return strings.TrimSpace(v) == "running"
		}
	}
	return false
}

func confirmKeyLogin(o Options) ConfirmFunc {
	return func(ctx context.Context, user string) (bool, error) {
		ip := "<sunucu-ip>"
		if out, err := o.Host.Run.Run(ctx, "hostname", "-I"); err == nil {
			if f := strings.Fields(out); len(f) > 0 {
				ip = f[0]
			}
		}
		p := o.Prompt
		p.Say("")
		p.Say("⚠  Şimdi root ve şifreyle SSH girişi kapatılacak.")
		p.Say("   Bu terminali KAPATMA. Yeni bir terminal aç ve şunları dene:")
		p.Say("")
		p.Say("     ssh %s@%s", user, ip)
		p.Say("     sudo -v")
		p.Say("")
		p.Say("   Giriş olmazsa üst üste deneme: 5 hatalı denemede fail2ban IP'ni 1 saat engeller.")
		p.Say("   İkisi de çalışıyorsa 'e' yaz. Çalışmıyorsa 'h' yaz; hiçbir şey değişmez.")
		return p.YesNo("Yeni kullanıcıyla giriş ve sudo çalıştı mı?", false)
	}
}

// Plan reports, without changing anything, which steps would run.
func Plan(ctx context.Context, steps []Step, out io.Writer) error {
	fmt.Fprintln(out, "\nDeneme modu (--dry-run): hiçbir şey değiştirilmeyecek.")
	for i, s := range steps {
		done, err := s.Check(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(out, "[%d/%d] %-20s kontrol edilemedi: %v\n", i+1, len(steps), s.Name(), err)
		case done:
			fmt.Fprintf(out, "[%d/%d] %-20s zaten tamam\n", i+1, len(steps), s.Name())
		default:
			fmt.Fprintf(out, "[%d/%d] %-20s yapılacak: %s\n", i+1, len(steps), s.Name(), s.Describe())
		}
	}
	return nil
}

// IsAborted reports whether err means the operator stopped the installer on purpose.
func IsAborted(err error) bool {
	return errors.Is(err, ErrAborted)
}
