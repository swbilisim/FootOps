package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/swbilisim/FootOps/internal/sites"
)

// CheckMove validates moving a site to another group without changing anything.
func (o *SiteOps) CheckMove(name, group string) (sites.Spec, sites.Spec, error) {
	cur, err := o.Store.Get(name)
	if err != nil {
		return cur, cur, err
	}
	next := cur
	next.Group = strings.ToLower(strings.TrimSpace(group))
	switch {
	case !sites.ValidGroup(next.Group):
		return cur, next, fmt.Errorf("grup şunlardan biri olmalı: %s", strings.Join(sites.Groups, ", "))
	case next.Group == cur.Group:
		return cur, next, fmt.Errorf("%s zaten %s grubunda", name, cur.Group)
	}
	if _, err := os.Lstat(o.path(next.Dir())); !errors.Is(err, fs.ErrNotExist) {
		return cur, next, fmt.Errorf("%s zaten var (elle mi oluşturuldu?)", next.Dir())
	}
	return cur, next, nil
}

// Move puts a site into another group: its directory goes under the new group, it gets
// that group's network, and the container is created again there. The container is
// stopped before the directory is renamed: Docker resolves a bind mount's path whenever
// a container starts, so one restarting meanwhile would get an empty directory at the
// old path. The site is away for a few seconds, as on an edit. If any step fails,
// everything this move did is undone and the old container started again.
func (o *SiteOps) Move(ctx context.Context, name, group string, progress Progress) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	cur, next, err := o.CheckMove(name, group)
	if err != nil {
		return err
	}
	if err := o.checkTag(ctx, cur); err != nil {
		return err
	}
	env, err := o.Store.Env(cur)
	if err != nil {
		return err
	}
	curDir, nextDir := o.path(cur.Dir()), o.path(next.Dir())
	oldCompose, err := os.ReadFile(filepath.Join(curDir, sites.ComposeFile))
	if err != nil {
		return err
	}
	oldMeta, err := os.ReadFile(filepath.Join(curDir, sites.MetaFile))
	if err != nil {
		return err
	}
	caddyPath := o.path(caddyFile(cur))
	oldCaddy, err := os.ReadFile(caddyPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	base := o.restartBaseline(ctx, cur)

	// What this move did, so a failure undoes exactly that: the new group's network may
	// already exist (an earlier move cut short), and is then left as it was.
	var createdNet bool
	var connected []string
	stopped, moved, written, recreated, caddyChanged := false, false, false, false, false
	defer func() {
		if err == nil {
			return
		}
		progress("✗ " + err.Error())
		if !createdNet && len(connected) == 0 && !stopped {
			return
		}
		progress("Eski hâline dönülüyor...")
		bg := context.Background()
		var errs []string
		fail := func(what string, e error) {
			if e != nil {
				errs = append(errs, what+": "+e.Error())
			}
		}
		if caddyChanged {
			if oldCaddy != nil {
				fail("Caddy dosyası", writeFile(caddyPath, string(oldCaddy), 0o644))
			} else if rm := os.Remove(caddyPath); rm != nil && !errors.Is(rm, fs.ErrNotExist) {
				fail("Caddy dosyası", rm)
			}
			fail("Caddy", o.caddy(bg, "reload"))
		}
		if recreated {
			// The new container mounts the new path: stopped first, so it cannot start
			// again (and create that path) after the directory goes back.
			fail("yeni container", o.compose(bg, next, "stop"))
		}
		if written {
			fail("compose.yaml", writeFile(filepath.Join(nextDir, sites.ComposeFile), string(oldCompose), 0o600))
			fail("footops.json", writeFile(filepath.Join(nextDir, sites.MetaFile), string(oldMeta), 0o600))
		}
		if moved {
			fail("klasör", os.Rename(nextDir, curDir))
		}
		if stopped {
			if e := o.compose(bg, cur, "up", "--detach", "--remove-orphans"); e != nil {
				fail("eski container", e)
			} else {
				fail("eski container", o.waitListening(bg, cur, base))
			}
		}
		for _, c := range connected {
			fail("yeni ağ", o.Docker.Disconnect(bg, next.Network(), c))
		}
		if createdNet {
			fail("yeni ağ", o.Docker.RemoveNetwork(bg, next.Network()))
		}
		if len(errs) > 0 {
			progress("✗ Geri alma tamamlanamadı: " + strings.Join(errs, "; "))
			return
		}
		progress("✓ Eski hâline döndü; site " + cur.Group + " grubunda çalışıyor")
	}()
	done := func(msg string) { progress("✓ " + msg) }

	progress(fmt.Sprintf("%s: %s → %s", name, cur.Dir(), next.Dir()))
	members, exists, err := o.Docker.NetworkMembers(ctx, next.Network())
	if err != nil {
		return err
	}
	if !exists {
		createdNet = true
		if err := o.Docker.CreateNetwork(ctx, next.Network(), map[string]string{"footops.site": next.Name}); err != nil {
			return fmt.Errorf("ağ oluşturulamadı: %w", err)
		}
	}
	attach := []struct {
		container string
		aliases   []string
	}{{caddyContainer, nil}}
	if next.Database {
		attach = append(attach, struct {
			container string
			aliases   []string
		}{mongoContainer, []string{mongoAlias}})
	}
	for _, a := range attach {
		if members[a.container] {
			continue
		}
		if err := o.Docker.Connect(ctx, next.Network(), a.container, a.aliases...); err != nil {
			return fmt.Errorf("%s yeni ağa bağlanamadı: %w", a.container, err)
		}
		connected = append(connected, a.container)
	}
	done("Yeni Docker ağı: " + next.Network())

	stopped = true
	if err := o.compose(ctx, cur, "stop"); err != nil {
		return fmt.Errorf("site durdurulamadı: %w", err)
	}
	done("Site durduruldu (birkaç saniye cevap vermeyecek)")
	if err := os.MkdirAll(filepath.Dir(nextDir), 0o755); err != nil {
		return err
	}
	if err := os.Rename(curDir, nextDir); err != nil {
		return fmt.Errorf("klasör taşınamadı: %w", err)
	}
	moved = true
	done("Klasör taşındı: " + next.Dir())

	next.UpdatedAt = o.now().UTC()
	written = true
	if err := o.Store.WriteCompose(next, env); err != nil {
		return err
	}
	if err := o.Store.WriteMeta(next); err != nil {
		return err
	}
	done("compose.yaml ve footops.json güncellendi")

	recreated = true
	if err := o.compose(ctx, next, "up", "--detach", "--remove-orphans"); err != nil {
		return fmt.Errorf("container yeni ağda başlatılamadı: %w", err)
	}
	if err := o.waitListening(ctx, next, base); err != nil {
		return err
	}
	done(fmt.Sprintf("Container yeni ağda, %d portunda cevap veriyor", next.Port))

	certs, err := o.Store.Certs()
	if err != nil {
		return err
	}
	certFor, _ := sites.CertFor(next.Domains, certs)
	if string(oldCaddy) != sites.CaddyBlock(next, certFor) {
		caddyChanged = true
		if err := o.writeCaddy(ctx, next); err != nil {
			return err
		}
	}
	if err := o.checkHTTPS(ctx, next.Domains[0]); err != nil {
		return err
	}
	done("Site yayında")

	// The old network is empty now; failing to remove it costs nothing but a name.
	for _, c := range []string{caddyContainer, mongoContainer} {
		_ = o.Docker.Disconnect(ctx, cur.Network(), c)
	}
	if err := o.Docker.RemoveNetwork(ctx, cur.Network()); err != nil {
		progress("⚠ Eski ağ silinemedi (zararsız): " + err.Error())
	} else {
		done("Eski ağ silindi: " + cur.Network())
	}
	return nil
}

// errTagMoved refuses recreating a container whose image tag now names a newer build
// (an image check or another site pulled it): the site would quietly change version,
// and a rollback could not bring the running one back.
var errTagMoved = errors.New("bu sitenin imaj etiketi sunucuda artık daha yeni bir sürümü gösteriyor (bir imaj kontrolü ya da aynı imajı kullanan başka bir site indirmiş); container yeniden oluşturulursa o sürüm çalışır ve geri alma eskisine dönemez. Önce “Yeniden dağıt” ile yeni sürüme geç, sonra tekrar dene")

// checkTag compares the image the site's container runs with what its tag names now.
// Anything it cannot read (no container, no image) is no reason to stop.
func (o *SiteOps) checkTag(ctx context.Context, s sites.Spec) error {
	in, err := o.Docker.Inspect(ctx, s.Container())
	if err != nil || in.Image == "" {
		return nil
	}
	out, err := o.Run.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", s.Image)
	if id := strings.TrimSpace(out); err == nil && strings.HasPrefix(id, "sha256:") && id != in.Image {
		return errTagMoved
	}
	return nil
}

// syncCompose writes compose.yaml again from footops.json and the current environment
// if it differs (the directory's group decides paths and network: a move cut short
// between the rename and the rewrite is put right before the container starts again).
// A compose file the panel cannot read is left alone.
func (o *SiteOps) syncCompose(s sites.Spec, progress Progress) error {
	env, err := o.Store.Env(s)
	if err != nil {
		return nil
	}
	p := o.path(filepath.Join(s.Dir(), sites.ComposeFile))
	have, err := os.ReadFile(p)
	if err != nil || string(have) == sites.Compose(s, env) {
		return nil
	}
	if err := o.Store.WriteCompose(s, env); err != nil {
		return err
	}
	if progress != nil {
		progress("ℹ compose.yaml site kaydına göre yeniden yazıldı (klasör, ağ)")
	}
	return nil
}
