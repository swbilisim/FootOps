package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/doctor"
	"github.com/swbilisim/FootOps/internal/run"
)

// Images no container uses (Faz U4, panel-tasarim §31.5): a redeploy or an image change
// removes the image it replaced, and this job removes what was left before that — the
// server check's "kullanılmayan imajlar". FootOps' own pinned images stay, and Docker
// itself refuses an image a container still uses (stopped ones too): never forced.

// ImageLister reads the images and their use (docker.Client).
type ImageLister interface {
	ImageUsage(ctx context.Context) (docker.ImageUsage, error)
}

// PruneImages removes the unused images and says what it freed.
func PruneImages(ctx context.Context, images ImageLister, r run.Runner, t *Task) error {
	before, err := images.ImageUsage(ctx)
	if err != nil {
		return fmt.Errorf("imajlar okunamadı: %w", err)
	}
	unused := doctor.Unused(before, core.PinnedImages)
	if len(unused) == 0 {
		t.Step("✓ Kullanılmayan imaj yok")
		return nil
	}
	removed, left := 0, 0
	for _, im := range unused {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := doctor.ImageName(im)
		// By ID: a tag may have moved on to another image meanwhile.
		if _, err := r.Run(ctx, "docker", "image", "rm", im.ID); err != nil {
			left++
			t.Step("ℹ Silinmedi (bir container kullanıyor ya da başka bir adı var): " + name)
			continue
		}
		removed++
		t.Step("✓ Silindi: " + name)
	}
	after, err := images.ImageUsage(ctx)
	freed := ""
	if err == nil && before.LayersSize > after.LayersSize {
		freed = ", " + trSize(uint64(before.LayersSize-after.LayersSize)) + " açıldı"
	}
	t.SetResult(fmt.Sprintf("%d", removed))
	t.Step(fmt.Sprintf("✓ %d imaj silindi%s", removed, freed))
	if left > 0 && removed == 0 {
		return errors.New("hiçbir imaj silinemedi")
	}
	return nil
}

func (s *Server) pruneImages(w http.ResponseWriter, r *http.Request) {
	if s.Images == nil {
		writeErr(w, http.StatusNotFound, "imajlar okunamıyor")
		return
	}
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "images.prune", Target: "imajlar", Actor: req.Actor}, func(ctx context.Context, t *Task) error {
		return PruneImages(ctx, s.Images, s.Sites.Run, t)
	}, func(j Job) {
		detail := j.Err
		if detail == "" {
			detail = j.Result + " imaj silindi"
		}
		s.recordJobDetail(j, detail)
	})
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, JobResponse{JobID: id})
}
