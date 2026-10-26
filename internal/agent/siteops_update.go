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
	DataMount string            `json:"data_mount"`
	Set       map[string]string `json:"set,omitempty"`
	Unset     []string          `json:"unset,omitempty"`
}

// updatePlan is a validated update: the new spec, the full new environment and a
// human-readable list of what changes (names only, never values).
type updatePlan struct {
	cur, next sites.Spec
	env       map[string]string
	changes   []string
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
	env, err := o.Store.Env(cur)
	if err != nil {
		return p, err
	}
	next := cur
	next.Domains, next.Image, next.Port, next.MemoryMB, next.DataMount = u.Domains, u.Image, u.Port, u.MemoryMB, u.DataMount
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
	return updatePlan{cur: cur, next: next, env: user, changes: changes}, nil
}

func orNone(s string) string {
	if s == "" {
		return "yok"
	}
	return s
}

// Update applies a site update. If the site does not come up with the new settings, the
// old files are put back and the old container is started again.
func (o *SiteOps) Update(ctx context.Context, name string, u SiteUpdate, progress Progress) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	p, err := o.planUpdate(name, u)
	if err != nil {
		return err
	}
	cur, next := p.cur, p.next
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

	filesWritten, caddyChanged, createdData := false, false, false
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
		if createdData {
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
	if err := o.Store.WriteCompose(next, p.env); err != nil {
		return err
	}
	if err := o.Store.WriteMeta(next); err != nil {
		return err
	}
	done("compose.yaml ve footops.json güncellendi")

	if next.Image != cur.Image {
		if err := o.compose(ctx, next, "pull", "--quiet"); err != nil {
			return fmt.Errorf("imaj indirilemedi (adı doğru mu, özel bir imaj mı?): %w", err)
		}
		done("İmaj indirildi: " + next.Image)
	}
	// After the pull: the directory is owned by the user the (new) image runs as.
	if next.DataMount != "" {
		if _, err := os.Stat(dataDir); errors.Is(err, fs.ErrNotExist) {
			createdData = true
			if err := o.prepareDataDir(ctx, next); err != nil {
				return err
			}
			done(fmt.Sprintf("Kalıcı klasör: %s/%s → %s", next.Dir(), sites.DataDir, next.DataMount))
		}
	}
	if err := o.compose(ctx, next, "up", "--detach", "--remove-orphans"); err != nil {
		return fmt.Errorf("container güncellenemedi: %w", err)
	}
	done("Container yeni ayarlarla başlatıldı")
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
	return nil
}
