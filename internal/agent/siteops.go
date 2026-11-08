package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

const (
	caddyContainer = "footops-caddy"
	mongoContainer = "footops-mongo"
	mongoAlias     = "mongo" // sites reach MongoDB as mongo:27017
	caddySitesDir  = core.CaddySitesDir
	listenTimeout  = 90 * time.Second
)

// SiteDocker is the Docker API surface site operations use. *docker.Client implements it.
type SiteDocker interface {
	CreateNetwork(ctx context.Context, name string, labels map[string]string) error
	NetworkMembers(ctx context.Context, name string) (map[string]bool, bool, error)
	Connect(ctx context.Context, network, container string, aliases ...string) error
	Disconnect(ctx context.Context, network, container string) error
	RemoveNetwork(ctx context.Context, name string) error
	ContainerIP(ctx context.Context, container, network string) (string, error)
	Inspect(ctx context.Context, name string) (docker.Inspect, error)
	Logs(ctx context.Context, name string, tail int) ([]string, error)
}

// SiteOps creates, updates and removes sites. Operations are serialised: two at once
// could race on Caddy's configuration and on network attachments.
type SiteOps struct {
	Store  sites.Store
	Docker SiteDocker
	Run    run.Runner
	// Kill runs commands that are safe to interrupt (an image check's pull and probe), so
	// a cancelled or timed-out check never keeps the job slot; nil uses Run.
	Kill      run.Runner
	PanelHost string // never served by a site
	// Mongo creates and drops the sites' database users (default: package mongo, as root).
	Mongo mongoAdmin

	mu  sync.Mutex
	now func() time.Time
	// terms keeps the sites' end dates and parking for the alerts (Terms).
	terms siteTerms
	// probes are replaceable in tests.
	dial  func(ctx context.Context, addr string) error
	https func(ctx context.Context, domain string) (int, error)
	page  func(ctx context.Context, domain string) (int, int64, error) // status, body bytes
	sleep func(ctx context.Context, d time.Duration) error
}

// Progress receives human-readable step messages.
type Progress func(string)

func (o *SiteOps) init() {
	if o.now == nil {
		o.now = time.Now
	}
	if o.dial == nil {
		o.dial = dialTCP
	}
	if o.https == nil {
		o.https = localHTTPS
	}
	if o.page == nil {
		o.page = localPage
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
}

func (o *SiteOps) path(p string) string { return filepath.Join(o.Store.Root, p) }

func (o *SiteOps) killer() run.Runner {
	if o.Kill != nil {
		return o.Kill
	}
	return o.Run
}

// Check validates a request without changing anything, so the form can show problems
// before a job starts.
func (o *SiteOps) Check(spec sites.Spec, env map[string]string) (sites.Spec, error) {
	spec.Normalize()
	var problems []string
	if err := spec.Validate(env); err != nil {
		var ve *sites.ValidationError
		if !errors.As(err, &ve) {
			return spec, err
		}
		problems = append(problems, ve.Problems...)
	}
	conflicts, err := o.Store.Conflicts(spec, o.PanelHost)
	if err != nil {
		return spec, err
	}
	problems = append(problems, conflicts...)
	if spec.Name != "" && len(conflicts) == 0 {
		if _, err := os.Stat(o.path(spec.Dir())); err == nil {
			problems = append(problems, spec.Dir()+" klasörü zaten var (elle mi oluşturuldu?)")
		}
	}
	certs, err := o.Store.Certs()
	if err != nil {
		return spec, err
	}
	if _, missing := sites.CertFor(spec.Domains, certs); len(missing) > 0 {
		problems = append(problems, "şu alan adları için sertifika yok (önce Sertifikalar sayfasından yükle): "+strings.Join(missing, ", "))
	}
	if len(problems) > 0 {
		return spec, &sites.ValidationError{Problems: problems}
	}
	return spec, nil
}

// Create builds a site end to end. If any step fails, every earlier step is undone, so
// a failed attempt leaves nothing behind.
func (o *SiteOps) Create(ctx context.Context, spec sites.Spec, env map[string]string, progress Progress) error {
	_, err := o.CreateWith(ctx, spec, env, progress, CreateOptions{})
	return err
}

// CreateOptions change how a site is built; a recovery from backups uses them.
type CreateOptions struct {
	// Fill puts the site's data in place once its directory, database user and data
	// directory are made, before its container first starts: an app never meets an
	// empty database it might seed. Its database is dropped again if the site is undone.
	Fill func(ctx context.Context, spec sites.Spec) error
	// Keep leaves CreatedAt and UpdatedAt as the spec has them (a site brought back).
	Keep bool
	// Lenient: once the container has started, an app that does not answer, or HTTPS
	// that does not, is a warning rather than a reason to undo a site whose data are
	// in place (it may need what is shut off for now).
	Lenient bool
}

// CreateWith is Create with options; warnings are what a lenient create let pass.
func (o *SiteOps) CreateWith(ctx context.Context, spec sites.Spec, env map[string]string, progress Progress, opt CreateOptions) (warnings []string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	keep := spec
	if !opt.Keep {
		spec.Parked = nil // parked only as a recovery brings a parked site back
	}
	spec, err = o.Check(spec, env)
	if err != nil {
		return nil, err
	}
	defer o.dropTerms()
	env = copyEnv(env)
	userKeys := sites.SortedKeys(env)
	warn := func(msg string) {
		warnings = append(warnings, msg)
		progress("⚠ " + msg)
	}

	var undo []func()
	defer func() {
		if err != nil {
			progress("✗ " + err.Error())
			if len(undo) > 0 {
				progress("Yapılanlar geri alınıyor...")
			}
			// Undo steps use a fresh context: the request's may be the reason we failed.
			for i := len(undo) - 1; i >= 0; i-- {
				undo[i]()
			}
		}
	}()
	done := func(msg string) { progress("✓ " + msg) }
	bg := func() context.Context { return context.Background() }

	dir := o.path(spec.Dir())
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return nil, err
	}
	undo = append(undo, func() { _ = os.RemoveAll(dir) })
	done("Klasör: " + spec.Dir())

	if err := o.Docker.CreateNetwork(ctx, spec.Network(), map[string]string{"footops.site": spec.Name}); err != nil {
		return nil, fmt.Errorf("ağ oluşturulamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.Docker.RemoveNetwork(bg(), spec.Network()) })
	if err := o.Docker.Connect(ctx, spec.Network(), caddyContainer); err != nil {
		return nil, fmt.Errorf("Caddy ağa bağlanamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.Docker.Disconnect(bg(), spec.Network(), caddyContainer) })
	done("Docker ağı: " + spec.Network())

	if spec.Database {
		if err := o.Docker.Connect(ctx, spec.Network(), mongoContainer, mongoAlias); err != nil {
			return nil, fmt.Errorf("MongoDB ağa bağlanamadı: %w", err)
		}
		undo = append(undo, func() { _ = o.Docker.Disconnect(bg(), spec.Network(), mongoContainer) })
		pw, err := randomSecret(32)
		if err != nil {
			return nil, err
		}
		if err := o.mongoUser(ctx, spec, pw); err != nil {
			return nil, err
		}
		undo = append(undo, func() { _ = o.dropMongoUser(bg(), spec) })
		if opt.Fill != nil {
			// What Fill loaded goes with the site; a plain create's database is still empty.
			undo = append(undo, func() {
				if p, ok := o.mongoOps().(dbPurger); ok {
					_ = p.DropSiteDB(bg(), spec.DBName())
				}
			})
		}
		env[spec.DBVar] = fmt.Sprintf("mongodb://%s:%s@%s:27017/%s?authSource=%s", spec.DBUser(), pw, mongoAlias, spec.DBName(), spec.DBName())
		done(fmt.Sprintf("MongoDB: veritabanı %s, kullanıcı %s (%s değişkeninde)", spec.DBName(), spec.DBUser(), spec.DBVar))
	}

	now := o.now().UTC()
	spec.EnvKeys, spec.CreatedAt, spec.UpdatedAt = userKeys, now, now
	if opt.Keep && !keep.CreatedAt.IsZero() {
		spec.CreatedAt, spec.UpdatedAt = keep.CreatedAt.UTC(), keep.UpdatedAt.UTC()
	}
	if err := o.Store.WriteCompose(spec, env); err != nil {
		return nil, err
	}
	if err := o.Store.WriteMeta(spec); err != nil {
		return nil, err
	}
	done("compose.yaml ve footops.json yazıldı")

	if err := o.compose(ctx, spec, "pull", "--quiet"); err != nil {
		return nil, fmt.Errorf("imaj indirilemedi: %w", explainPull(err))
	}
	if err := o.checkPlatform(ctx, spec.Image); err != nil {
		return nil, err
	}
	done("İmaj indirildi: " + spec.Image)

	if spec.DataMount != "" {
		uid, err := o.prepareDataDir(ctx, spec)
		if err != nil {
			return nil, err
		}
		done(fmt.Sprintf("Kalıcı klasör: %s/%s → %s", spec.Dir(), sites.DataDir, spec.DataMount))
		if a := o.loginAccount(uid); a != "" {
			progress(uidWarning(uid, a))
		}
	}

	if opt.Fill != nil {
		if err := opt.Fill(ctx, spec); err != nil {
			return nil, err
		}
	}

	if spec.IsParked() {
		// Brought back as it was: parked. Its container is made when it is brought back
		// into service (the image is here already).
		if err := o.writeCaddy(ctx, spec); err != nil {
			return nil, err
		}
		undo = append(undo, func() {
			_ = os.Remove(o.path(caddyFile(spec)))
			_ = o.caddy(bg(), "reload")
		})
		done("Site parkta (" + localTime(spec.Parked.At) + " tarihinden beri): alan adları bakım sayfasını gösteriyor, container açılmadı")
		return warnings, nil
	}

	if err := o.compose(ctx, spec, "up", "--detach"); err != nil {
		return nil, fmt.Errorf("container başlatılamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.compose(bg(), spec, "down", "--remove-orphans") })
	if err := o.waitListening(ctx, spec, restartBase{}); err != nil {
		if !opt.Lenient || ctx.Err() != nil {
			return nil, err
		}
		warn("Uygulama cevap vermedi: " + err.Error())
	} else {
		done(fmt.Sprintf("Uygulama %d portunda cevap veriyor", spec.Port))
	}

	if err := o.writeCaddy(ctx, spec); err != nil {
		return nil, err
	}
	undo = append(undo, func() {
		_ = os.Remove(o.path(caddyFile(spec)))
		_ = o.caddy(bg(), "reload")
	})
	done("Caddy: " + strings.Join(spec.Domains, ", "))

	if len(warnings) > 0 {
		// HTTPS reaches the app through Caddy: it cannot answer either.
		return warnings, nil
	}
	if err := o.checkHTTPS(ctx, spec.Domains[0]); err != nil {
		if !opt.Lenient || ctx.Err() != nil {
			return nil, err
		}
		warn("HTTPS üzerinden cevap alınamadı: " + err.Error())
		return warnings, nil
	}
	done("HTTPS üzerinden cevap alındı. Site hazır; DNS kaydı Cloudflare'de bu sunucuyu göstermeli.")
	return warnings, nil
}

// discard takes a half-made site away entirely: container, Caddy block, network,
// database with its users, directory. Only for what a recovery left half-made when the
// agent stopped in the middle of making it (Create's own undo never ran): none of it is
// worth keeping, the backup it came from is still there.
func (o *SiteOps) discard(ctx context.Context, s sites.Spec) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	dir := o.path(s.Dir())
	if _, err := os.Stat(filepath.Join(dir, sites.ComposeFile)); err == nil {
		if err := o.compose(ctx, s, "down", "--remove-orphans"); err != nil {
			return fmt.Errorf("container durdurulamadı: %w", err)
		}
	}
	if err := os.Remove(o.path(caddyFile(s))); err == nil {
		if err := o.caddy(ctx, "reload"); err != nil {
			return fmt.Errorf("Caddy yeniden yüklenemedi: %w", err)
		}
	}
	_ = o.Docker.Disconnect(ctx, s.Network(), caddyContainer)
	_ = o.Docker.Disconnect(ctx, s.Network(), mongoContainer)
	_ = o.Docker.RemoveNetwork(ctx, s.Network())
	if s.Database {
		if p, ok := o.mongoOps().(dbPurger); ok {
			if err := p.DropSiteDB(ctx, s.DBName()); err != nil {
				return fmt.Errorf("veritabanı silinemedi: %w", err)
			}
		} else if err := o.dropMongoUser(ctx, s); err != nil {
			return err
		}
	}
	return os.RemoveAll(dir)
}

// Redeploy pulls the image again and recreates the container if it changed.
func (o *SiteOps) Redeploy(ctx context.Context, name string, progress Progress) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	if spec.IsParked() {
		return errParked
	}
	if err := o.syncCompose(spec, progress); err != nil {
		return err
	}
	if err := o.compose(ctx, spec, "pull", "--quiet"); err != nil {
		return fmt.Errorf("imaj indirilemedi: %w", explainPull(err))
	}
	if err := o.checkPlatform(ctx, spec.Image); err != nil {
		return err
	}
	progress("✓ İmaj indirildi: " + spec.Image)
	base := o.restartBaseline(ctx, spec)
	if err := o.compose(ctx, spec, "up", "--detach", "--remove-orphans"); err != nil {
		return err
	}
	progress("✓ Container güncellendi")
	if err := o.waitListening(ctx, spec, base); err != nil {
		return err
	}
	if err := o.checkHTTPS(ctx, spec.Domains[0]); err != nil {
		return err
	}
	spec.UpdatedAt = o.now().UTC()
	if err := o.Store.WriteMeta(spec); err != nil {
		return err
	}
	progress("✓ Site yayında")
	return nil
}

// Remove takes a site offline and moves its directory aside. Data and the database are
// kept: deleting customer data is never a single click.
func (o *SiteOps) Remove(ctx context.Context, name string, progress Progress) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	defer o.dropTerms()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	if err := os.Remove(o.path(caddyFile(spec))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := o.caddy(ctx, "reload"); err != nil {
		return fmt.Errorf("Caddy yeniden yüklenemedi: %w", err)
	}
	progress("✓ Caddy'den çıkarıldı")
	if err := o.compose(ctx, spec, "down", "--remove-orphans"); err != nil {
		return err
	}
	progress("✓ Container durduruldu ve silindi")
	for _, c := range []string{caddyContainer, mongoContainer} {
		_ = o.Docker.Disconnect(ctx, spec.Network(), c)
	}
	if err := o.Docker.RemoveNetwork(ctx, spec.Network()); err != nil {
		return err
	}
	progress("✓ Ağ silindi")
	dst := filepath.Join(sites.SrvDir, spec.Group, "_removed", spec.Name+"-"+o.now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(o.path(filepath.Dir(dst)), 0o750); err != nil {
		return err
	}
	if err := os.Rename(o.path(spec.Dir()), o.path(dst)); err != nil {
		return err
	}
	progress("✓ Dosyalar " + dst + " altına taşındı (silinmedi)")
	if spec.Database {
		progress(fmt.Sprintf("ℹ MongoDB veritabanı %s ve kullanıcısı silinmedi", spec.DBName()))
	}
	return nil
}

// Reconcile re-attaches Caddy and MongoDB to every site network. Attachments made with
// `docker network connect` are lost when those containers are recreated (e.g. on an
// upgrade of the core stack); the site files say what should be connected.
func (o *SiteOps) Reconcile(ctx context.Context) []error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	list, err := o.Store.List()
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, s := range list {
		members, exists, err := o.Docker.NetworkMembers(ctx, s.Network())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !exists {
			if err := o.Docker.CreateNetwork(ctx, s.Network(), map[string]string{"footops.site": s.Name}); err != nil {
				errs = append(errs, err)
				continue
			}
			members = map[string]bool{}
		}
		if !members[caddyContainer] {
			if err := o.Docker.Connect(ctx, s.Network(), caddyContainer); err != nil {
				errs = append(errs, fmt.Errorf("%s: caddy: %w", s.Name, err))
			}
		}
		if s.Database && !members[mongoContainer] {
			if err := o.Docker.Connect(ctx, s.Network(), mongoContainer, mongoAlias); err != nil {
				errs = append(errs, fmt.Errorf("%s: mongo: %w", s.Name, err))
			}
		}
	}
	return errs
}

// ---- certificates ----

// InstallCert stores a certificate and re-renders the Caddy blocks of sites that should
// now use it (e.g. a renewed certificate replacing an expiring one).
func (o *SiteOps) InstallCert(ctx context.Context, certPEM, keyPEM string) (sites.Cert, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	c, err := sites.ParseCertPair(certPEM, keyPEM, o.now())
	if err != nil {
		return c, err
	}
	key, err := sites.KeyPEM(keyPEM)
	if err != nil {
		return c, err
	}
	if err := o.Store.InstallCert(c, key); err != nil {
		return c, err
	}
	certs, err := o.Store.Certs()
	if err != nil {
		return c, err
	}
	return c, o.rerenderCaddy(ctx, certs)
}

// RemoveCert deletes a certificate unless a site still depends on it.
func (o *SiteOps) RemoveCert(ctx context.Context, id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	certs, err := o.Store.Certs()
	if err != nil {
		return err
	}
	var target *sites.Cert
	var rest []sites.Cert
	for i := range certs {
		if certs[i].ID == id {
			target = &certs[i]
		} else {
			rest = append(rest, certs[i])
		}
	}
	if target == nil {
		return errors.New("sertifika bulunamadı")
	}
	list, err := o.Store.List()
	if err != nil {
		return err
	}
	var users []string
	for _, s := range list {
		if _, missing := sites.CertFor(s.Domains, rest); len(missing) > 0 {
			users = append(users, s.Name)
		}
	}
	if len(users) > 0 {
		return fmt.Errorf("bu sertifikayı kullanan siteler var: %s", strings.Join(users, ", "))
	}
	// Blocks still naming it move to the other certificates first; its files go only once
	// Caddy runs without them, so a refused change never leaves a block naming a
	// deleted file (Caddy would not start from that configuration).
	if err := o.rerenderCaddy(ctx, rest); err != nil {
		return err
	}
	return o.Store.RemoveCert(*target)
}

// SyncCaddy brings every site's Caddy block up to what this version renders: after an
// update of FootOps the blocks on disk are the old version's until something rewrites
// them. It reports false, changing nothing, until Caddy runs and its healthcheck (the
// admin endpoint) passes; caddyResync paces the calls.
func (o *SiteOps) SyncCaddy(ctx context.Context) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	// Running is also true while the container restarts; Status is not.
	in, err := o.Docker.Inspect(ctx, caddyContainer)
	if err != nil || in.State.Status != "running" || (in.State.Health != nil && in.State.Health.Status != "healthy") {
		return false, nil
	}
	certs, err := o.Store.Certs()
	if err != nil {
		return true, err
	}
	return true, o.rerenderCaddy(ctx, certs)
}

// caddyResync paces SyncCaddy from the reconcile loop: every tick until Caddy is up, then
// once; after a failure again in a minute, waiting twice as long each time up to an hour.
type caddyResync struct {
	synced bool
	next   time.Time
	wait   time.Duration
}

func (c *caddyResync) due(now time.Time) bool { return !c.synced && !now.Before(c.next) }

func (c *caddyResync) result(ran bool, err error, now time.Time) {
	switch {
	case !ran: // Caddy not up yet: the next tick
	case err == nil:
		c.synced = true
	default:
		c.wait = min(max(2*c.wait, time.Minute), time.Hour)
		c.next = now.Add(c.wait)
	}
}

// rerenderCaddy rewrites, for the given certificates, the site blocks that differ from
// their rendering (a parked site's is its maintenance page), validates the whole configuration and reloads. If Caddy refuses
// either, the previous files go back: the files on disk always match what Caddy runs,
// and a later change does not trip over a block Caddy never accepted. A site without a
// block (a removal stopped halfway, or taken offline by hand) stays without one.
func (o *SiteOps) rerenderCaddy(ctx context.Context, certs []sites.Cert) error {
	list, err := o.Store.List()
	if err != nil {
		return err
	}
	prev := map[string][]byte{} // file -> content before
	restore := func() {
		for f, b := range prev {
			_ = writeFile(f, string(b), 0o644)
		}
	}
	for _, s := range list {
		certFor, missing := sites.CertFor(s.Domains, certs)
		if len(missing) > 0 {
			continue
		}
		f := o.path(caddyFile(s))
		have, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			restore()
			return err
		}
		want := sites.SiteBlock(s, certFor)
		if string(have) == want {
			continue
		}
		prev[f] = have
		if err := writeFile(f, want, 0o644); err != nil {
			restore()
			return err
		}
	}
	if len(prev) == 0 {
		return nil
	}
	if err := o.caddy(ctx, "validate"); err != nil {
		restore()
		return fmt.Errorf("Caddy yapılandırması geçersiz: %w", err)
	}
	if err := o.caddy(ctx, "reload"); err != nil {
		restore()
		return fmt.Errorf("Caddy yeniden yüklenemedi: %w", err)
	}
	return nil
}

// ---- helpers ----

// checkPlatform refuses an image built for another machine before a container runs it
// (it would only crash with "exec format error"). An unreadable inspect is not a reason
// to stop: the pull itself succeeded.
func (o *SiteOps) checkPlatform(ctx context.Context, image string) error {
	info, err := o.inspectImage(ctx, image)
	if err != nil || info.PlatformOK() {
		return nil
	}
	return errPlatform(info)
}

func caddyFile(s sites.Spec) string { return filepath.Join(caddySitesDir, s.Name+".caddy") }

func (o *SiteOps) compose(ctx context.Context, s sites.Spec, args ...string) error {
	full := append([]string{"compose", "--project-name", s.Name, "--file", filepath.Join(s.Dir(), sites.ComposeFile)}, args...)
	_, err := o.Run.Run(ctx, "docker", full...)
	return err
}

// caddy runs `caddy validate` or `caddy reload` inside the Caddy container.
func (o *SiteOps) caddy(ctx context.Context, verb string) error {
	_, err := o.Run.Run(ctx, "docker", "exec", caddyContainer, "caddy", verb,
		"--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile")
	return err
}

// writeCaddy adds the site block, validates the whole configuration and reloads. An
// invalid configuration is removed again before Caddy ever loads it.
func (o *SiteOps) writeCaddy(ctx context.Context, s sites.Spec) error {
	certs, err := o.Store.Certs()
	if err != nil {
		return err
	}
	certFor, missing := sites.CertFor(s.Domains, certs)
	if len(missing) > 0 {
		return fmt.Errorf("sertifika yok: %s", strings.Join(missing, ", "))
	}
	f := o.path(caddyFile(s))
	if err := writeFile(f, sites.SiteBlock(s, certFor), 0o644); err != nil {
		return err
	}
	if err := o.caddy(ctx, "validate"); err != nil {
		_ = os.Remove(f)
		return fmt.Errorf("Caddy yapılandırması geçersiz: %w", err)
	}
	if err := o.caddy(ctx, "reload"); err != nil {
		_ = os.Remove(f)
		return fmt.Errorf("Caddy yeniden yüklenemedi: %w", err)
	}
	return nil
}

// mongoUser creates (or resets the password of) the site's database user, with
// readWrite on the site's database alone.
func (o *SiteOps) mongoUser(ctx context.Context, s sites.Spec, pw string) error {
	if err := o.mongoOps().SetUser(ctx, s.DBName(), s.DBUser(), pw); err != nil {
		return fmt.Errorf("MongoDB kullanıcısı oluşturulamadı: %w", err)
	}
	return nil
}

func (o *SiteOps) dropMongoUser(ctx context.Context, s sites.Spec) error {
	return o.mongoOps().DropUser(ctx, s.DBName(), s.DBUser())
}

// mongoOps manages database users as MongoDB's root user, whose password is read from
// the secrets file. Root is only for managing users and databases (rule K4).
func (o *SiteOps) mongoOps() mongoAdmin {
	if o.Mongo != nil {
		return o.Mongo
	}
	return mongoClient{Addr: defaultMongoAddr, SecretFile: o.path(core.MongoSecretPath)}
}

// prepareDataDir creates the persistent directory owned by the user the image runs as,
// so the app can write to it without running as root; it returns that uid.
func (o *SiteOps) prepareDataDir(ctx context.Context, s sites.Spec) (int, error) {
	d := o.path(filepath.Join(s.Dir(), sites.DataDir))
	if err := os.MkdirAll(d, 0o750); err != nil {
		return 0, err
	}
	uid, gid := o.imageUser(ctx, s.Image)
	return uid, os.Chown(d, uid, gid)
}

// imageUser returns the uid/gid the image runs as. It asks the image itself (`id`), which
// resolves user names; images without `id` (distroless) fall back to a numeric USER or root.
func (o *SiteOps) imageUser(ctx context.Context, image string) (int, int) {
	if uid, gid, err := imageIDs(ctx, o.Run, image); err == nil {
		return uid, gid
	}
	out, err := o.Run.Run(ctx, "docker", "image", "inspect", "--format", "{{.Config.User}}", image)
	if err == nil {
		u, g, _ := strings.Cut(strings.TrimSpace(out), ":")
		if n, err := strconv.Atoi(u); err == nil {
			m, err := strconv.Atoi(g)
			if err != nil {
				m = n
			}
			return n, m
		}
	}
	return 0, 0
}

// loginAccount names the server account with uid that a person can log in with. A
// container's processes running as that uid are that account's on the server: it reads
// their environment — the site's secrets — and stops them without sudo (2026-11-03:
// the admin accounts had 1000 and 1001, the uids of Node and Next.js images). System
// accounts, nobody and accounts without a shell are left out.
func (o *SiteOps) loginAccount(uid int) string {
	if uid < 1000 || uid == 65534 {
		return ""
	}
	data, err := os.ReadFile(o.path("/etc/passwd"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 7 || f[2] != strconv.Itoa(uid) {
			continue
		}
		// An empty shell is /bin/sh (passwd(5)): a login too.
		if sh := strings.TrimSpace(f[6]); strings.HasSuffix(sh, "nologin") || strings.HasSuffix(sh, "/false") {
			continue
		}
		return f[0]
	}
	return ""
}

// uidWarning is the step that says so.
func uidWarning(uid int, account string) string {
	return fmt.Sprintf("⚠ İmaj uid %d ile çalışıyor; sunucuda bu numara %s hesabının: o hesap sudo'suz bu sitenin süreçlerinin ortam değişkenlerini (sırlarını) okuyabilir. Hesabı 2000 ve üstü bir numaraya taşı ya da kullanılmıyorsa sil.", uid, account)
}

// imageIDs runs `id` once in the image, as a site would run it (no network, no
// capabilities, small limits); it exits at once.
func imageIDs(ctx context.Context, r run.Runner, image string) (int, int, error) {
	out, err := r.Run(ctx, "docker", "run", "--rm", "--network", "none", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--memory", "64m", "--pids-limit", "64", "--entrypoint", "id", image)
	if err != nil {
		return 0, 0, err
	}
	m := reIDOut.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, errors.New("id çıktısı anlaşılamadı")
	}
	uid, uerr := strconv.Atoi(m[1])
	gid, gerr := strconv.Atoi(m[2])
	if uerr != nil || gerr != nil {
		return 0, 0, errors.New("id çıktısı anlaşılamadı")
	}
	return uid, gid, nil
}

// reIDOut reads "uid=1001(nextjs) gid=1001(nodejs) groups=…" (names are optional).
var reIDOut = regexp.MustCompile(`uid=(\d{1,10})\S*\s+gid=(\d{1,10})`)

// restartBase is a container's restart count before an operation. An unchanged
// container keeps its old count; a recreated one (new ID) starts again at 0.
type restartBase struct {
	id string
	n  int
}

func (o *SiteOps) restartBaseline(ctx context.Context, s sites.Spec) restartBase {
	in, err := o.Docker.Inspect(ctx, s.Container())
	if err != nil {
		return restartBase{}
	}
	return restartBase{in.ID, in.RestartCount}
}

// waitListening waits until the app accepts TCP connections on its port, and fails
// early (with its last log lines) if the container stops or restarts again.
func (o *SiteOps) waitListening(ctx context.Context, s sites.Spec, base restartBase) error {
	deadline := o.now().Add(listenTimeout)
	for {
		in, err := o.Docker.Inspect(ctx, s.Container())
		allowed := 0
		if err == nil && in.ID == base.id {
			allowed = base.n
		}
		if err == nil && (!in.State.Running || in.RestartCount > allowed) {
			lines, _ := o.Docker.Logs(ctx, s.Container(), 20)
			return fmt.Errorf("container çalışmıyor (%s, %d kez yeniden başladı). Son loglar:\n%s",
				in.State.Status, in.RestartCount, strings.Join(lines, "\n"))
		}
		if ip, err := o.Docker.ContainerIP(ctx, s.Container(), s.Network()); err == nil {
			if o.dial(ctx, net.JoinHostPort(ip, strconv.Itoa(s.Port))) == nil {
				return nil
			}
		}
		if o.now().After(deadline) {
			lines, _ := o.Docker.Logs(ctx, s.Container(), 20)
			return fmt.Errorf("uygulama %s içinde %d portunu dinlemeye başlamadı (port doğru mu?). Son loglar:\n%s",
				listenTimeout, s.Port, strings.Join(lines, "\n"))
		}
		if err := o.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// checkHTTPS asks Caddy for the site over TLS on this server. Any answer from the app
// counts (even 404); 502-504 mean Caddy cannot reach it.
func (o *SiteOps) checkHTTPS(ctx context.Context, domain string) error {
	var last error
	for i := 0; i < 10; i++ {
		code, err := o.https(ctx, domain)
		if err == nil && code != 502 && code != 503 && code != 504 {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("HTTP %d", code)
		}
		if err := o.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return fmt.Errorf("Caddy üzerinden cevap alınamadı: %w", last)
}

func dialTCP(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

// localHTTPS requests https://domain/ from this server's own Caddy and returns the status.
func localHTTPS(ctx context.Context, domain string) (int, error) {
	code, _, err := localPage(ctx, domain)
	return code, err
}

// localPage requests https://domain/ from this server's own Caddy and returns the status
// and how many body bytes came (read up to 64 KB). Certificate checks are skipped on
// purpose: Origin CA certificates are trusted only by Cloudflare, and this probe only
// verifies routing on 127.0.0.1.
func localPage(ctx context.Context, domain string) (int, int64, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", "127.0.0.1:443")
		},
		TLSClientConfig: &tls.Config{ServerName: domain, InsecureSkipVerify: true}, //nolint:gosec // local routing probe
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, n, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func copyEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	return out
}

func writeFile(path, content string, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

const secretAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randomSecret returns letters and digits only, so it is safe inside connection strings.
func randomSecret(n int) (string, error) {
	b := make([]byte, n)
	r := make([]byte, n*2)
	if _, err := randRead(r); err != nil {
		return "", err
	}
	j := 0
	for i := range b {
		// Rejection sampling keeps the distribution uniform (256 is not a multiple of 62).
		for {
			if j >= len(r) {
				if _, err := randRead(r); err != nil {
					return "", err
				}
				j = 0
			}
			v := r[j]
			j++
			if int(v) < 256-256%len(secretAlphabet) {
				b[i] = secretAlphabet[int(v)%len(secretAlphabet)]
				break
			}
		}
	}
	return string(b), nil
}

var randRead = rand.Read
