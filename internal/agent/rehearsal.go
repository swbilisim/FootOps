package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// A server recovered from its backups (recover.go) starts as a rehearsal (prova kipi,
// panel-tasarim §27.5) of the server it was rebuilt from, which may well still be
// running. While it lasts:
//
//   - nothing reaches the old server's places: no backup repository can be set up (and
//     with none the timetable, "back up now" and the status file stay off), no Telegram
//     message goes out and no bot can be linked; the tunnel's token is not even put in
//     place (the installer keeps it aside with the other two);
//   - the sites cannot open connections to the outside: one firewall rule rejects every
//     new connection from the Docker networks the sites (and Caddy, MongoDB) are on to
//     anywhere but such a network, so an app's mail, payments or marketplace updates do
//     not happen twice. Visitors' requests through Caddy, the sites' database and the
//     agent's own tools (restic and the image pulls run elsewhere) are not affected.
//
// `sudo footops go-live` ends it once the old server is surely off.
const (
	DefaultRehearsalPath = "/var/lib/footops/rehearsal.json"
	// rehearsalTag marks the rule, so it is found again and only it is removed.
	rehearsalTag = "footops-prova"
)

// rehearsalRule is the rule's match: new connections that come in on a user-defined
// bridge (every Docker network the stack makes: br-<id>; the default bridge, docker0,
// where restic's containers run, is not one) and go out anywhere but another bridge.
var rehearsalRule = []string{"DOCKER-USER", "-i", "br-+", "!", "-o", "br-+", "-m", "conntrack", "--ctstate", "NEW",
	"-m", "comment", "--comment", rehearsalTag, "-j", "REJECT"}

// RehearsalInfo is what the marker file says.
type RehearsalInfo struct {
	Since time.Time `json:"since"`
	// Repository is the bucket (and folder) the server was recovered from, Server the
	// server code its backups were taken under.
	Repository string `json:"repository,omitempty"`
	Server     string `json:"server,omitempty"`
}

// Rehearsal keeps the marker and the firewall rule.
type Rehearsal struct {
	Path string
	Run  run.Runner
	Log  *log.Logger

	// work serializes looking at the marker and changing the rule (Begin, End, Ensure):
	// a check that saw the marker cannot put the rule back after End took it away.
	work sync.Mutex
	mu   sync.Mutex
	// ruleErr is why the rule could not be put in place last time (an alert).
	ruleErr   string
	ruleSince time.Time
}

// errRehearsal refuses what a rehearsal must not do.
var errRehearsal = errors.New("bu sunucu yedekten prova kipinde kuruldu: canlı sunucunun yerlerine (tünel, yedek deposu, Telegram) bağlanmaz. Canlıya almak için sunucuda: sudo footops go-live")

// Info reads the marker; ok is false when the server is not in rehearsal.
func (r *Rehearsal) Info() (RehearsalInfo, bool) {
	var in RehearsalInfo
	if r == nil || r.Path == "" {
		return in, false
	}
	data, err := os.ReadFile(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return in, false
	}
	// A marker that cannot be read still marks a rehearsal: refusing is the safe side.
	if err == nil {
		_ = json.Unmarshal(data, &in)
	}
	return in, true
}

// Active reports whether the server is in rehearsal.
func (r *Rehearsal) Active() bool {
	_, ok := r.Info()
	return ok
}

// Begin marks the server as a rehearsal and shuts the sites off from the outside. The
// marker is written first: whatever happens to the rule, the server stays one. An
// error means the rule is not in place — no site may start until it is.
func (r *Rehearsal) Begin(ctx context.Context, in RehearsalInfo) error {
	r.work.Lock()
	defer r.work.Unlock()
	if !r.Active() {
		if in.Since.IsZero() {
			in.Since = time.Now().UTC()
		}
		data, err := json.MarshalIndent(in, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(r.Path), 0o755); err != nil {
			return err
		}
		if err := writeSynced(r.Path, append(data, '\n')); err != nil {
			return err
		}
	}
	return r.ensureRule(ctx)
}

// End takes the rule away, then the marker.
func (r *Rehearsal) End(ctx context.Context) error {
	r.work.Lock()
	defer r.work.Unlock()
	if err := r.dropRule(ctx); err != nil {
		return err
	}
	if err := os.Remove(r.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	r.mu.Lock()
	r.ruleErr, r.ruleSince = "", time.Time{}
	r.mu.Unlock()
	return nil
}

// Ensure keeps the rule in place while the server is in rehearsal (Docker rebuilds its
// chains when it starts, a reboot loses them) and takes a left-over one away once it is
// not: run when the agent starts and then every few seconds.
func (r *Rehearsal) Ensure(ctx context.Context, startup bool) {
	if r == nil {
		return
	}
	r.work.Lock()
	defer r.work.Unlock()
	if r.Active() {
		_ = r.ensureRule(ctx)
		return
	}
	if startup {
		if err := r.dropRule(ctx); err != nil && r.Log != nil {
			r.Log.Printf("prova kipinin güvenlik duvarı kuralı kaldırılamadı: %v", err)
		}
	}
}

// iptables runs op (-C, -I or -D) with the rule; the error carries iptables' last line.
func (r *Rehearsal) iptables(ctx context.Context, op string) error {
	args := append([]string{"-w", "10", op}, rehearsalRule...)
	if op == "-I" {
		args = append([]string{"-w", "10", "-I", rehearsalRule[0], "1"}, rehearsalRule[1:]...)
	}
	_, err := r.Run.Run(ctx, "iptables", args...)
	return err
}

func (r *Rehearsal) ensureRule(ctx context.Context) error {
	err := r.iptables(ctx, "-C")
	if err != nil {
		err = r.iptables(ctx, "-I")
		if err == nil {
			err = r.iptables(ctx, "-C") // in place now, as the kernel has it
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		msg := "sitelerin dış bağlantısını kapatan güvenlik duvarı kuralı eklenemedi: " + err.Error()
		if msg != r.ruleErr && r.Log != nil {
			r.Log.Printf("%s", msg)
		}
		if r.ruleSince.IsZero() {
			r.ruleSince = time.Now()
		}
		r.ruleErr = msg
		return errors.New(msg)
	}
	if r.ruleErr != "" && r.Log != nil {
		r.Log.Printf("prova kipinin güvenlik duvarı kuralı yerinde")
	}
	r.ruleErr, r.ruleSince = "", time.Time{}
	return nil
}

// dropRule removes every copy of the rule (a second one only if something put it twice).
// Only iptables' "no such rule" (exit status 1, also for a missing chain) means it is
// gone; any other failure is an error, not an absent rule.
func (r *Rehearsal) dropRule(ctx context.Context) error {
	for i := 0; i < 10; i++ {
		err := r.iptables(ctx, "-C")
		if exitCode(err) == 1 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("güvenlik duvarı kuralına bakılamadı: %w", err)
		}
		if err := r.iptables(ctx, "-D"); err != nil {
			return fmt.Errorf("güvenlik duvarı kuralı kaldırılamadı: %w", err)
		}
	}
	return errors.New("güvenlik duvarı kuralı kaldırılamadı: hâlâ yerinde")
}

// RuleArgs are iptables' arguments that insert the rule at the top of DOCKER-USER (the
// boot unit puts it there before Docker starts, rehearsal-rule).
func RuleArgs() []string {
	return append([]string{"-w", "10", "-I", rehearsalRule[0], "1"}, rehearsalRule[1:]...)
}

// RuleCheckArgs are iptables' arguments that check the rule is there.
func RuleCheckArgs() []string { return append([]string{"-w", "10", "-C"}, rehearsalRule...) }

// Alerts tells the panel the server is a rehearsal, and when the sites are not shut
// off from the outside as they should be.
func (r *Rehearsal) Alerts(now time.Time) []Alert {
	in, ok := r.Info()
	if !ok {
		return nil
	}
	from := ""
	if in.Repository != "" {
		from = " (" + in.Repository + ")"
	}
	out := []Alert{{Key: "rehearsal", Level: "warn", Title: "Prova kipi: bu sunucu yedekten kuruldu" + from,
		Detail: "Tünel, yedekleme ve Telegram kapalı; siteler dışarıya bağlantı açamıyor (ziyaretçi istekleri ve veritabanı çalışır). " +
			"Eski sunucu kapalıysa ve bu sunucu onun yerine geçecekse sunucuda: sudo footops go-live",
		Link: "/backup", Since: in.Since}}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ruleErr != "" {
		out = append(out, Alert{Key: "rehearsal:firewall", Level: "bad", Title: "Prova kipinde siteler dışarıya kapatılamadı",
			Detail: r.ruleErr + ". Siteler canlı sunucuyla aynı dış işleri (e-posta, ödeme…) yapabilir: sitelerin container'larını durdur ya da kuralı düzelt.",
			Link:   "/sites", Since: r.ruleSince})
	}
	return out
}
