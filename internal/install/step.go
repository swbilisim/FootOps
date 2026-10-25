// Package install prepares a fresh Ubuntu host: system packages, swap, SSH hardening,
// firewall, Docker, core containers and the FootOps services.
//
// Every step is idempotent: Check reports whether the host already matches the desired
// state, and Apply is only called when it does not. Re-running the installer after a
// failure therefore resumes where it stopped.
package install

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Step is one unit of host preparation.
type Step interface {
	// Name is a short, stable identifier shown in logs, e.g. "swap".
	Name() string
	// Describe explains in one sentence what Apply will change.
	Describe() string
	// Check returns true when the host is already in the desired state.
	Check(ctx context.Context) (bool, error)
	// Apply brings the host into the desired state.
	Apply(ctx context.Context) error
}

// Result records what happened to a step during a run.
type Result struct {
	Step     string
	Skipped  bool // already in the desired state
	Err      error
	Duration time.Duration
}

// Run executes steps in order and stops at the first failure.
// Progress is written to out; the returned slice covers every step that was attempted.
func Run(ctx context.Context, steps []Step, out io.Writer) ([]Result, error) {
	results := make([]Result, 0, len(steps))
	for i, s := range steps {
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(out, "Durduruldu; kalan adımlar yapılmadı. Tekrar çalıştırınca kaldığı yerden devam eder.\n")
			return results, err
		}
		start := time.Now()
		res := Result{Step: s.Name()}
		fmt.Fprintf(out, "[%d/%d] %s: ", i+1, len(steps), s.Name())

		done, err := s.Check(ctx)
		if err != nil {
			res.Err = fmt.Errorf("check %s: %w", s.Name(), err)
		} else if done {
			res.Skipped = true
		} else if err := s.Apply(ctx); err != nil {
			res.Err = fmt.Errorf("apply %s: %w", s.Name(), err)
		}
		res.Duration = time.Since(start)
		results = append(results, res)

		switch {
		case res.Err != nil:
			fmt.Fprintf(out, "FAILED (%v)\n", res.Err)
			return results, res.Err
		case res.Skipped:
			fmt.Fprintln(out, "already done")
		default:
			fmt.Fprintf(out, "ok (%s)\n", res.Duration.Round(time.Millisecond))
		}
	}
	return results, nil
}
