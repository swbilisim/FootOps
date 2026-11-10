package install

import (
	"context"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/update"
)

// The core's containers are up to date with the compose file on disk; a release whose
// compose file differs recreates them all the same, and its plan says so beforehand.
func TestPlanUpdateCoreCompose(t *testing.T) {
	h, _ := newTestHost(t, func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "config --hash"):
			return "caddy hc\nmongo hm\n", nil
		case strings.HasPrefix(cmd, "docker inspect") && strings.HasSuffix(cmd, "footops-caddy"):
			return "hc true healthy", nil
		case strings.HasPrefix(cmd, "docker inspect") && strings.HasSuffix(cmd, "footops-mongo"):
			return "hm true healthy", nil
		}
		return "", errExit
	})
	a := validAnswers()
	data, _ := a.Marshal()
	put(t, h, AnswersPath, data)
	plan := func() (update.PlanStep, update.Plan) {
		t.Helper()
		p, err := PlanUpdate(context.Background(), h, "/nonexistent/footops")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Steps {
			if s.Name == "core-stack" {
				return s, p
			}
		}
		t.Fatal("no core-stack step")
		return update.PlanStep{}, p
	}

	put(t, h, core.ComposePath, core.Compose(coreConfig(a)))
	if s, _ := plan(); s.Change {
		t.Errorf("same compose: core-stack changes: %+v", s)
	}
	put(t, h, core.ComposePath, strings.Replace(core.Compose(coreConfig(a)), "interval: 60s", "interval: 30s", 2))
	s, p := plan()
	if !s.Change || !strings.Contains(strings.Join(p.Impact, " "), "Caddy ve/veya MongoDB yeniden oluşturulur") {
		t.Errorf("new compose: %+v, impact %v", s, p.Impact)
	}
}
