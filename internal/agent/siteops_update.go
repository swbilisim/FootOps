package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/swbilisim/FootOps/internal/sites"
)

// SiteUpdate is what can change on an existing site. Name, group and the database stay
// as they are. Environment changes are applied on top of the current values, which the
// panel never sees: Set adds or replaces, Unset removes.
type SiteUpdate struct {
	Domains   []string          `json:"domains"`
	Image     string            `json:"image"`
	Port      int               `json:"port"`
	MemoryMB  int               `json:"memory_mb"`
	MaxBodyMB int               `json:"max_body_mb"` // 0: sites.DefaultBodyMB
	DataMount string            `json:"data_mount"`
	Set       map[string]string `json:"set,omitempty"`
	Unset     []string          `json:"unset,omitempty"`
}

// updatePlan is a validated update: the new spec, the full new environment and a
// human-readable list of what changes (names only, never values).
type updatePlan struct {
	cur, next sites.Spec
	env       map[string]string
	curEnv    map[string]string
	changes   []string
}

// fills is the first persistent folder an update gives a site, which it fills from the
// container (Faz L4): nothing under it is at stake.
func (p updatePlan) fills() []string {
	if p.next.DataMount != "" && p.cur.DataMount == "" {
		return []string{p.next.DataMount}
	}
	return nil
}

// recreates reports whether an update makes the container anew: what compose is given
// changes (not merely how this FootOps writes it), or the image, or the container is off
// its network (compose is forced then).
func (o *SiteOps) recreates(ctx context.Context, p updatePlan) bool {
	return p.next.Image != p.cur.Image || sites.Compose(p.next, p.env) != sites.Compose(p.cur, p.curEnv) || o.offNetwork(ctx, p.cur)
}

// CheckUpdate validates an update without changing anything and describes it.
func (o *SiteOps) CheckUpdate(name string, u SiteUpdate) ([]string, error) {
	p, err := o.planUpdate(name, u)
	return p.changes, err
}

func (o *SiteOps) planUpdate(name string, u SiteUpdate) (updatePlan, error) {
	var p updatePlan
	cur, err := o.Store.Get(name)
	if err != nil {
		return p, err
	}
	if cur.IsParked() {
		return p, errParked // the change starts the container again
	}
	env, err := o.Store.Env(cur)
	if err != nil {
		return p, err
	}
	next := cur
	next.Domains, next.Image, next.Port, next.MemoryMB, next.DataMount = u.Domains, u.Image, u.Port, u.MemoryMB, u.DataMount
	next.MaxBodyMB = u.MaxBodyMB
	next.Normalize()

	var problems []string
	user := copyEnv(env)
	if cur.Database {
		delete(user, cur.DBVar) // managed by FootOps, never by the form
	}
	var changes []string
	for _, k := range u.Unset {
		if cur.Database && k == cur.DBVar {
			problems = append(problems, k+" veritabanı bağlantısı; silinemez")
			continue
		}
		if _, ok := user[k]; ok {
			delete(user, k)
			changes = append(changes, "-"+k)
		}
	}
	for _, k := range sites.SortedKeys(u.Set) {
		if old, ok := user[k]; !ok {
			changes = append(changes, "+"+k)
		} else if old != u.Set[k] {
			changes = append(changes, "~"+k)
		}
		user[k] = u.Set[k]
	}
	if len(changes) > 0 {
		changes = []string{"ortam değişkenleri " + strings.Join(changes, " ")}
	}
	if !slices.Equal(next.Domains, cur.Domains) {
		changes = append(changes, "alan adları: "+strings.Join(next.Domains, ", "))
	}
	if next.Image != cur.Image {
		changes = append(changes, "imaj: "+next.Image)
	}
	if next.Port != cur.Port {
		changes = append(changes, fmt.Sprintf("port %d → %d", cur.Port, next.Port))
	}
	if next.MemoryMB != cur.MemoryMB {
		changes = append(changes, fmt.Sprintf("bellek %d → %d MB", cur.MemoryMB, next.MemoryMB))
	}
	if next.BodyMB() != cur.BodyMB() {
		changes = append(changes, fmt.Sprintf("en büyük istek %d → %d MB", cur.BodyMB(), next.BodyMB()))
	}
	if next.DataMount != cur.DataMount {
		changes = append(changes, "kalıcı klasör: "+orNone(next.DataMount))
	}

	if err := next.Validate(user); err != nil {
		var ve *sites.ValidationError
		if !errors.As(err, &ve) {
			return p, err
		}
		problems = append(problems, ve.Problems...)
	}
	conflicts, err := o.Store.UpdateConflicts(next, o.PanelHost)
	if err != nil {
		return p, err
	}
	problems = append(problems, conflicts...)
	certs, err := o.Store.Certs()
	if err != nil {
		return p, err
	}
	if _, missing := sites.CertFor(next.Domains, certs); len(missing) > 0 {
		problems = append(problems, "şu alan adları için sertifika yok (önce Sertifikalar sayfasından yükle): "+strings.Join(missing, ", "))
	}
	if len(problems) == 0 && len(changes) == 0 {
		problems = append(problems, "değişiklik yok")
	}
	if len(problems) > 0 {
		return p, &sites.ValidationError{Problems: problems}
	}

	next.EnvKeys = sites.SortedKeys(user)
	if cur.Database {
		user[cur.DBVar] = env[cur.DBVar]
	}
	return updatePlan{cur: cur, next: next, env: user, curEnv: env, changes: changes}, nil
}

func orNone(s string) string {
	if s == "" {
		return "yok"
	}
	return s
}

// Update applies a site update. If the site does not come up with the new settings, the
// old files are put back and the old container is started again. A change compose is not
// given (domains, port, body limit: Caddy's) leaves the compose file and the container as
// they are (Faz L): they may be an older FootOps's, and making the container anew loses
// what its layer holds. One that makes it anew stops, unless force, over files the check
// before it did not see.
func (o *SiteOps) Update(ctx context.Context, name string, u SiteUpdate, force bool, progress Progress) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	p, err := o.planUpdate(name, u)
	if err != nil {
		return err
	}
	cur, next := p.cur, p.next
	recreate := o.recreates(ctx, p)
	if recreate && !force {
		if r := o.Layer(ctx, cur, p.fills()...); r.Risky() {
			return layerError(r)
		}
	}
	if next.Image == cur.Image {
		if err := o.checkTag(ctx, cur); err != nil {
			return err
		}
	}
	dir := o.path(cur.Dir())
	oldCompose, err := os.ReadFile(filepath.Join(dir, sites.ComposeFile))
	if err != nil {
		return err
	}
	oldMeta, err := os.ReadFile(filepath.Join(dir, sites.MetaFile))
	if err != nil {
		return err
	}
	caddyPath := o.path(caddyFile(cur))
	oldCaddy, err := os.ReadFile(caddyPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	// filled: the persistent folder took the container's files; upStarted: the container
	// may have been made anew, with the folder, and the originals gone with the old one.
	filesWritten, caddyChanged, createdData, filled, upStarted := false, false, false, false, false
	dataDir := filepath.Join(dir, sites.DataDir)
	base := o.restartBaseline(ctx, cur)
	// On failure every restore step runs, even if an earlier one failed: a half-restored
	// site (e.g. Caddy still routing the new domains) is worse than a reported error.
	defer func() {
		if err == nil {
			return
		}
		progress("✗ " + err.Error())
		if !filesWritten {
			return
		}
		progress("Eski ayarlara dönülüyor...")
		bg := context.Background()
		var errs []string
		fail := func(what string, e error) {
			if e != nil {
				errs = append(errs, what+": "+e.Error())
			}
		}
		fail("compose.yaml", writeFile(filepath.Join(dir, sites.ComposeFile), string(oldCompose), 0o600))
		fail("footops.json", writeFile(filepath.Join(dir, sites.MetaFile), string(oldMeta), 0o600))
		switch {
		case createdData && !upStarted:
			_ = os.RemoveAll(dataDir) // only a copy: the container still holds its files
		case createdData && !filled:
			_ = os.Remove(dataDir) // only if still empty
		}
		if caddyChanged {
			if oldCaddy != nil {
				fail("Caddy dosyası", writeFile(caddyPath, string(oldCaddy), 0o644))
			} else if rm := os.Remove(caddyPath); rm != nil && !errors.Is(rm, fs.ErrNotExist) {
				fail("Caddy dosyası", rm)
			}
			fail("Caddy", o.caddy(bg, "reload"))
		}
		if e := o.compose(bg, cur, "up", "--detach", "--remove-orphans"); e != nil {
			fail("eski container", e)
		} else {
			// Made anew without the folder: what went into it goes back into the container.
			if filled && upStarted {
				if e := o.copyBack(bg, cur, next.DataMount, dataDir); e != nil {
					fail("dosyalar container'a geri konamadı, "+dataDir+" klasöründeler", e)
				} else {
					progress("✓ Kalıcı klasöre taşınan dosyalar container'a geri kondu")
					if createdData {
						_ = os.RemoveAll(dataDir)
					}
				}
			}
			fail("eski container", o.waitListening(bg, cur, base))
		}
		if len(errs) > 0 {
			progress("✗ Geri alma tamamlanamadı: " + strings.Join(errs, "; "))
			return
		}
		progress("✓ Eski ayarlar geri yüklendi; site önceki hâliyle çalışıyor")
	}()
	done := func(msg string) { progress("✓ " + msg) }

	progress("Değişiklikler: " + strings.Join(p.changes, "; "))
	next.UpdatedAt = o.now().UTC()
	filesWritten = true
	if recreate {
		if err := o.Store.WriteCompose(next, p.env); err != nil {
			return err
		}
	}
	if err := o.Store.WriteMeta(next); err != nil {
		return err
	}
	if recreate {
		done("compose.yaml ve footops.json güncellendi")
	} else {
		done("footops.json güncellendi; container ve compose.yaml olduğu gibi")
	}

	if next.Image != cur.Image {
		if err := o.compose(ctx, next, "pull", "--quiet"); err != nil {
			return fmt.Errorf("imaj indirilemedi: %w", explainPull(err))
		}
		if err := o.checkPlatform(ctx, next.Image); err != nil {
			return err
		}
		done("İmaj indirildi: " + next.Image)
	}
	// After the pull: the directory is owned by the user the (new) image runs as. A first
	// persistent folder takes what the container held there, which it would hide (Faz L4):
	// the site stops for the copy, so nothing written meanwhile is left behind.
	if next.DataMount != "" {
		_, serr := os.Stat(dataDir)
		createdData = errors.Is(serr, fs.ErrNotExist)
		if len(p.fills()) > 0 {
			if err := o.roomFor(ctx, cur, dir); err != nil {
				return err
			}
			if err := o.compose(ctx, next, "stop"); err != nil {
				return fmt.Errorf("site durdurulamadı: %w", err)
			}
			done("Site, container'daki dosyalar kopyalanırken durduruldu")
			n, size, err := o.fillDataDir(ctx, cur, next.DataMount, dataDir)
			if err != nil {
				return err
			}
			if filled = n > 0; filled {
				what := "taşındı"
				if !createdData {
					what = "oradaki klasörle birleştirildi"
				}
				done(fmt.Sprintf("Container'daki %s kalıcı klasöre %s: %d dosya, %s", next.DataMount, what, n, humanBytes(size)))
			}
		}
		if createdData || filled {
			uid, err := o.prepareDataDir(ctx, next)
			if err != nil {
				return err
			}
			if filled {
				if err := chownTree(dataDir); err != nil {
					return err
				}
			}
			done(fmt.Sprintf("Kalıcı klasör: %s/%s → %s", next.Dir(), sites.DataDir, next.DataMount))
			if a := o.loginAccount(uid); a != "" {
				progress(uidWarning(uid, a))
			}
		}
	}
	old := o.containerImage(ctx, cur)
	up := []string{"up", "--detach", "--no-recreate"}
	if recreate {
		up = o.upArgs(ctx, next, "--detach", "--remove-orphans")
	}
	upStarted = true
	if err := o.compose(ctx, next, up...); err != nil {
		return fmt.Errorf("container güncellenemedi: %w", err)
	}
	if recreate {
		done("Container yeni ayarlarla başlatıldı")
	}
	if err := o.waitListening(ctx, next, base); err != nil {
		return err
	}
	done(fmt.Sprintf("Uygulama %d portunda cevap veriyor", next.Port))

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
		done("Caddy: " + strings.Join(next.Domains, ", "))
	}
	if err := o.checkHTTPS(ctx, next.Domains[0]); err != nil {
		return err
	}
	done("Site yeni ayarlarla yayında")
	// Only now: until here a failure goes back to the old image.
	if next.Image != cur.Image {
		o.dropReplaced(ctx, next, old, progress)
	}
	return nil
}
