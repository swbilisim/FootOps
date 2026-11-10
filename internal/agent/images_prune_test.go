package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
)

// imageLister answers ImageUsage from a list it shrinks as the runner removes images.
type imageLister struct {
	mu     sync.Mutex
	images []docker.ImageEntry
	layers int64
}

func (l *imageLister) ImageUsage(context.Context) (docker.ImageUsage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return docker.ImageUsage{LayersSize: l.layers, Images: append([]docker.ImageEntry{}, l.images...)}, nil
}

func TestPruneImages(t *testing.T) {
	l := &imageLister{layers: 3 << 30, images: []docker.ImageEntry{
		{ID: "sha256:kullanilan", RepoTags: []string{"ghcr.io/x/magaza:main"}, Size: 1 << 30, Containers: 1},
		{ID: "sha256:eski", RepoTags: []string{"<none>:<none>"}, Size: 1 << 30},
		{ID: "sha256:eskietiket", RepoTags: []string{"ghcr.io/x/blog:v1"}, Size: 500 << 20},
		{ID: "sha256:inatci", RepoTags: []string{"ghcr.io/x/a:1", "ghcr.io/x/b:1"}, Size: 100 << 20},
		{ID: "sha256:restic", RepoDigests: []string{strings.Replace(core.ResticImage, ":0.19.1@", "@", 1)}, Size: 30 << 20},
		{ID: "sha256:caddy", RepoTags: []string{core.CaddyImage}, Size: 50 << 20},
	}}
	r := &cmdRunner{fail: map[string]error{"docker image rm sha256:inatci": errors.New("conflict: image is referenced in multiple repositories")}, out: map[string]string{}}
	r.hook = func(cmd string) {
		id, ok := strings.CutPrefix(cmd, "docker image rm ")
		if !ok || id == "sha256:inatci" {
			return
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, im := range l.images {
			if im.ID == id {
				l.layers -= im.Size
				l.images = append(l.images[:i], l.images[i+1:]...)
				break
			}
		}
	}
	j := NewJobs()
	id, err := j.Start(JobSpec{Kind: "images.prune", Target: "imajlar"}, func(ctx context.Context, t *Task) error {
		return PruneImages(ctx, l, r, t)
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, j, id)
	if job.Err != "" || job.Result != "2" {
		t.Fatalf("job: %+v", job)
	}
	steps := strings.Join(job.Steps, "\n")
	for _, want := range []string{"✓ Silindi: etiketsiz eski", "✓ Silindi: ghcr.io/x/blog:v1", "ℹ Silinmedi (bir container kullanıyor ya da başka bir adı var): ghcr.io/x/a:1",
		"✓ 2 imaj silindi, 1,5 GB açıldı"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps lack %q:\n%s", want, steps)
		}
	}
	// In use, FootOps' own pinned ones: never even asked.
	for _, never := range []string{"sha256:kullanilan", "sha256:restic", "sha256:caddy"} {
		if r.ran("docker image rm "+never) != 0 {
			t.Errorf("%s removed", never)
		}
	}
	// Nothing left to do: said so.
	l.images = l.images[:1]
	id, _ = j.Start(JobSpec{Kind: "images.prune"}, func(ctx context.Context, t *Task) error { return PruneImages(ctx, l, r, t) }, nil)
	if job := waitJob(t, j, id); job.Err != "" || !strings.Contains(strings.Join(job.Steps, " "), "Kullanılmayan imaj yok") {
		t.Errorf("nothing to remove: %+v", job)
	}
}

func TestRedeployDropsReplacedImage(t *testing.T) {
	e := newOps(t)
	spec := specFor("enisparta")
	if err := e.ops.Create(context.Background(), spec, map[string]string{}, e.progress); err != nil {
		t.Fatal(err)
	}
	// The pull brings a new image for the tag: the container is made anew from it.
	e.net.mu.Lock()
	e.net.image = "sha256:eski"
	e.net.mu.Unlock()
	e.run.hook = func(cmd string) {
		if strings.Contains(cmd, " up ") {
			e.net.mu.Lock()
			e.net.image = "sha256:yeni"
			e.net.mu.Unlock()
		}
	}
	e.steps = nil
	if err := e.ops.Redeploy(context.Background(), "enisparta", false, e.progress); err != nil {
		t.Fatal(err)
	}
	if e.run.ran("docker image rm sha256:eski") != 1 || !strings.Contains(strings.Join(e.steps, "\n"), "✓ Önceki imaj silindi") {
		t.Fatalf("old image not removed:\n%s\n%s", strings.Join(e.run.calls, "\n"), strings.Join(e.steps, "\n"))
	}
	// The same image again (nothing new pulled): nothing to remove.
	e.run.hook = nil
	e.steps = nil
	if err := e.ops.Redeploy(context.Background(), "enisparta", false, e.progress); err != nil {
		t.Fatal(err)
	}
	if e.run.ran("docker image rm") != 1 {
		t.Error("removed an image still in use")
	}
	// Docker refuses (another container uses it): said, the deploy still succeeds.
	e.net.mu.Lock()
	e.net.image = "sha256:paylasilan"
	e.net.mu.Unlock()
	e.run.hook = func(cmd string) {
		if strings.Contains(cmd, " up ") {
			e.net.mu.Lock()
			e.net.image = "sha256:yeni2"
			e.net.mu.Unlock()
		}
	}
	e.run.fail["docker image rm sha256:paylasilan"] = errors.New("conflict: image is being used by stopped container")
	e.steps = nil
	if err := e.ops.Redeploy(context.Background(), "enisparta", false, e.progress); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(e.steps, "\n"), "ℹ Önceki imaj bırakıldı") {
		t.Errorf("refusal not said:\n%s", strings.Join(e.steps, "\n"))
	}
}
