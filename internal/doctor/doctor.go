// Package doctor checks a server against the FootOps standard (panel-tasarim §31.1):
// what the installer set up (its steps' checks, which package install supplies) and what
// drifts by itself — a reboot owed, updates waiting, ports nobody opened on purpose, the
// clock, images no container uses, versions near the end of their support. It only
// reads. `footops doctor` prints it; the agent runs `footops doctor --json` once a day in
// a transient service and turns its problems into alerts.
package doctor

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
)

// Format is the report's JSON format: fields are only ever added.
const Format = 1

// Levels, mildest first. Warn and Bad are problems: the agent makes alerts of them.
const (
	OK   = "ok"
	Info = "info"
	Warn = "warn"
	Bad  = "bad"
)

// Section groups findings on the page and in the terminal.
type Section struct{ ID, Title string }

// Sections in the order they are shown.
var Sections = []Section{
	{"standard", "Kurulum standardı"},
	{"system", "Sistem"},
	{"network", "Ağ"},
	{"disk", "Disk"},
	{"accounts", "Hesaplar"},
	{"versions", "Sürümler"},
}

// Finding is one checked item. ID is stable across runs (an alert's identity).
type Finding struct {
	ID      string `json:"id"`
	Section string `json:"section"`
	Level   string `json:"level"`
	Title   string `json:"title"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
}

// Problem reports whether the finding needs the operator.
func (f Finding) Problem() bool { return f.Level == Warn || f.Level == Bad }

// Report is one run.
type Report struct {
	Format   int       `json:"format"`
	Host     string    `json:"host"`
	FootOps  string    `json:"footops"`
	Time     time.Time `json:"time"`
	TookMS   int64     `json:"took_ms"`
	Findings []Finding `json:"findings"`
}

// Problems returns the warnings and the serious ones, serious first.
func (r Report) Problems() []Finding {
	var out []Finding
	for _, lvl := range []string{Bad, Warn} {
		for _, f := range r.Findings {
			if f.Level == lvl {
				out = append(out, f)
			}
		}
	}
	return out
}

// Count returns how many findings have the level.
func (r Report) Count(level string) int {
	n := 0
	for _, f := range r.Findings {
		if f.Level == level {
			n++
		}
	}
	return n
}

// Docker is the part of the engine's API the checks read.
type Docker interface {
	Containers(ctx context.Context) ([]docker.Container, error)
	Version(ctx context.Context) (docker.Version, error)
	ImageUsage(ctx context.Context) (docker.ImageUsage, error)
}

// Env is what a run reads. Root maps absolute paths (a temp dir in tests); commands
// always run on the real system through Run.
type Env struct {
	Root   string
	Run    run.Runner
	Docker Docker // nil: Docker's checks say they could not run
	Now    func() time.Time
	Host   string
	// FootOps is the running version.
	FootOps string
	// AutoReboot and RebootTime are the installer's answers: unattended-upgrades
	// reboots by itself at that time when an update asks for it.
	AutoReboot bool
	RebootTime string
	// Standard runs the installer's checks, Accounts its account notes (package
	// install supplies both); nil skips them.
	Standard func(ctx context.Context) []Finding
	Accounts func() []Finding
	// KeepImages are image references FootOps itself pulls (its core and tools): never
	// counted as unused, even while no container runs them.
	KeepImages []string
}

func (e Env) path(p string) string { return filepath.Join(e.Root, p) }

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) stat(p string) (os.FileInfo, error) { return os.Stat(e.path(p)) }

func (e Env) read(p string) string {
	b, err := os.ReadFile(e.path(p))
	if err != nil {
		return ""
	}
	return string(b)
}

// Run checks everything once.
func Run(ctx context.Context, env Env) Report {
	start := env.now()
	r := Report{Format: Format, Host: env.Host, FootOps: env.FootOps, Time: start}
	add := func(fs ...Finding) { r.Findings = append(r.Findings, fs...) }
	if env.Standard != nil {
		add(env.Standard(ctx)...)
	}
	add(reboot(env, start))
	pkgs, f := updates(ctx, env, start)
	add(f, unattended(ctx, env, start), timeSync(ctx, env))
	var containers []docker.Container
	var dockerErr error
	if env.Docker == nil {
		dockerErr = fmt.Errorf("Docker'a bağlanılmadı")
	} else {
		containers, dockerErr = env.Docker.Containers(ctx)
	}
	add(ports(ctx, env, containers, dockerErr))
	add(images(ctx, env))
	if env.Accounts != nil {
		acc := env.Accounts()
		if len(acc) == 0 {
			acc = []Finding{{ID: "accounts", Section: "accounts", Level: OK, Title: "Yönetici hesabı, bulut hesapları ve sudo kuralları standartta"}}
		}
		add(acc...)
	}
	add(versions(ctx, env, containers, dockerErr, pkgs, start)...)
	r.TookMS = env.now().Sub(start).Milliseconds()
	return r
}

// Render writes the report for a terminal: problems and notes in full, what is fine
// in one line per section.
func Render(w io.Writer, r Report) {
	fmt.Fprintf(w, "FootOps kontrolü — %s · footops %s · %s\n", r.Host, r.FootOps, r.Time.Local().Format("2006-01-02 15:04"))
	for _, s := range Sections {
		var fine []string
		var rest []Finding
		for _, f := range r.Findings {
			if f.Section != s.ID {
				continue
			}
			if f.Level == OK {
				fine = append(fine, f.Title)
			} else {
				rest = append(rest, f)
			}
		}
		if len(fine) == 0 && len(rest) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", s.Title)
		sort.SliceStable(rest, func(i, j int) bool { return rank(rest[i].Level) > rank(rest[j].Level) })
		for _, f := range rest {
			fmt.Fprintf(w, "  %s %s\n", symbol(f.Level), f.Title)
			if f.Detail != "" {
				fmt.Fprintf(w, "      %s\n", f.Detail)
			}
			if f.Fix != "" {
				fmt.Fprintf(w, "      → %s\n", f.Fix)
			}
		}
		if s.ID == "standard" && len(fine) > 1 {
			fmt.Fprintf(w, "  ✓ %d adım standartta\n", len(fine))
			continue
		}
		for _, t := range fine {
			fmt.Fprintf(w, "  ✓ %s\n", t)
		}
	}
	fmt.Fprintf(w, "\nÖzet: %d ciddi, %d uyarı, %d bilgi (%.1f sn)\n", r.Count(Bad), r.Count(Warn), r.Count(Info), float64(r.TookMS)/1000)
}

func rank(level string) int {
	switch level {
	case Bad:
		return 3
	case Warn:
		return 2
	case Info:
		return 1
	}
	return 0
}

func symbol(level string) string {
	switch level {
	case Bad:
		return "✗"
	case Warn:
		return "⚠"
	case Info:
		return "•"
	}
	return "✓"
}

// span says a duration the way the panel does: minutes, hours, then days.
func span(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d dk", max(1, int(d.Minutes())))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d saat", int(d.Hours()))
	}
	return fmt.Sprintf("%d gün", int(d.Hours()/24))
}

// list joins up to n items and says how many more there are.
func list(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" ve %d tane daha", len(items)-n)
}

// bytesTR is a size with one decimal and a Turkish decimal comma: "1,8 GB".
func bytesTR(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	s := fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
	return strings.Replace(s, ".", ",", 1)
}
