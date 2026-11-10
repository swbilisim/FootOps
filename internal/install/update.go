package install

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/swbilisim/FootOps/internal/update"
)

// Updates (panel-tasarim §31.3): a release's binary applies itself with the answers
// saved at installation — no questions, and no `apt-get upgrade`: the system's packages
// stay with unattended-upgrades and `footops doctor`. Steps that change only with
// someone at the terminal stop an update before anything changes.

// BinPath is where the installed footops is: the services run it.
const BinPath = update.InstalledBin

// interactiveSteps ask when they change: SSH hardening's key-login check, a sudo
// password, the tunnel's token, the panel's 2FA, a recovery's secrets.
var interactiveSteps = map[string]bool{
	"ssh-hardening": true, "admin-user": true, "tunnel-secret": true, "panel-account": true, "recover": true,
}

// ErrNeedsTerminal: the update would change what only `sudo footops install` at a
// terminal can. ErrRefused wraps it when the check before the steps found it, so
// nothing has changed; met by a step during the run, it is a failure like any other.
var (
	ErrNeedsTerminal = errors.New("bu değişiklik SSH'den kurulumla yapılmalı")
	ErrRefused       = errors.New("güncelleme yapılmadı, hiçbir şey değişmedi")
)

// stepImpact is what visitors and the operator notice while a step is applied.
var stepImpact = map[string]string{
	"core-stack":     "Caddy ve/veya MongoDB yeniden oluşturulur: siteler birkaç saniye (en çok ~10 sn) erişilemez, veritabanlı siteler veritabanına birkaç saniye ulaşamaz.",
	"docker-daemon":  "Docker yeniden başlar; container'lar çalışmaya devam eder.",
	"core-files":     "Caddy'nin ana ayarı yenilenir.",
	"panel-binary":   "Panel birkaç saniye yeniden başlar; siteler etkilenmez.",
	"panel-config":   "Panel birkaç saniye yeniden başlar; siteler etkilenmez.",
	"panel-services": "Panel birkaç saniye yeniden başlar; siteler etkilenmez.",
	"firewall":       "Güvenlik duvarının kuralları değişir.",
	"fail2ban":       "fail2ban yeniden başlar.",
	"packages":       "Eksik temel paketler kurulur (apt).",
	"docker":         "Docker kurulur ya da başlatılır.",
	"docker-repo":    "Docker deposunun ayarı yenilenir.",
}

// savedAnswers are the answers of the installation, which an update and the doctor use.
func savedAnswers(h Host) (Answers, error) {
	saved, err := h.ReadFile(AnswersPath)
	if err != nil {
		return Answers{}, err
	}
	if saved == "" {
		return Answers{}, fmt.Errorf("kurulum cevapları yok (%s): önce sudo footops install", AnswersPath)
	}
	a, err := UnmarshalAnswers(saved)
	if err == nil {
		err = a.Validate()
	}
	if err != nil {
		return Answers{}, fmt.Errorf("kurulum cevapları okunamadı (%s): %w", AnswersPath, err)
	}
	return a, nil
}

// updateSteps are what an update applies: every step but the system upgrade and a
// recovery's. A step that would ask stops with ErrNeedsTerminal instead.
func updateSteps(h Host, a Answers, self string, out io.Writer) []Step {
	deps := Deps{Self: self, Notes: &Notes{}, Out: out,
		Confirm:   func(context.Context, string) (bool, error) { return false, ErrNeedsTerminal },
		TOTPSetup: func(context.Context, string, string) error { return ErrNeedsTerminal }}
	var steps []Step
	for _, s := range Steps(h, a, deps) {
		if s.Name() != "upgrade" && s.Name() != "recover" {
			steps = append(steps, s)
		}
	}
	return steps
}

func stepTitle(name string) string {
	if t := stepTitles[name]; t != "" {
		return t
	}
	return name
}

// PlanUpdate says what applying self would change. It only reads: the binary of a
// release checks the server against its own standard before anyone agrees to it.
func PlanUpdate(ctx context.Context, h Host, self string) (update.Plan, error) {
	a, err := savedAnswers(h)
	if err != nil {
		return update.Plan{}, err
	}
	p := update.Plan{Steps: []update.PlanStep{}}
	said := map[string]bool{}
	for _, s := range updateSteps(h, a, self, io.Discard) {
		ps := update.PlanStep{Name: s.Name(), Title: stepTitle(s.Name())}
		done, err := s.Check(ctx)
		if err != nil {
			ps.Err = err.Error()
		}
		if ps.Change = err != nil || !done; ps.Change {
			ps.Detail = s.Describe()
			if interactiveSteps[ps.Name] {
				p.Interactive = append(p.Interactive, ps.Title)
			}
			if t := stepImpact[ps.Name]; t != "" && !said[t] {
				said[t] = true
				p.Impact = append(p.Impact, t)
			}
		}
		p.Steps = append(p.Steps, ps)
	}
	return p, nil
}

// ApplyUpdate brings the server to self's standard with the saved answers (`footops
// install --auto`). Nothing changes when a step that asks would have to.
func ApplyUpdate(ctx context.Context, h Host, self string, out io.Writer) error {
	a, err := savedAnswers(h)
	if err != nil {
		return err
	}
	steps := updateSteps(h, a, self, out)
	for _, s := range steps {
		if !interactiveSteps[s.Name()] {
			continue
		}
		if done, err := s.Check(ctx); err != nil || !done {
			return fmt.Errorf("%w: %w: %s", ErrRefused, ErrNeedsTerminal, stepTitle(s.Name()))
		}
	}
	_, err = Run(ctx, steps, out)
	return err
}
