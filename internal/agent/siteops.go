package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	Store     sites.Store
	Docker    SiteDocker
	Run       run.Runner
	PanelHost string // never served by a site

	mu  sync.Mutex
	now func() time.Time
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
func (o *SiteOps) Create(ctx context.Context, spec sites.Spec, env map[string]string, progress Progress) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	spec, err = o.Check(spec, env)
	if err != nil {
		return err
	}
	env = copyEnv(env)
	userKeys := sites.SortedKeys(env)

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
		return err
	}
	if err := os.Mkdir(dir, 0o750); err != nil {
		return err
	}
	undo = append(undo, func() { _ = os.RemoveAll(dir) })
	done("Klasör: " + spec.Dir())

	if err := o.Docker.CreateNetwork(ctx, spec.Network(), map[string]string{"footops.site": spec.Name}); err != nil {
		return fmt.Errorf("ağ oluşturulamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.Docker.RemoveNetwork(bg(), spec.Network()) })
	if err := o.Docker.Connect(ctx, spec.Network(), caddyContainer); err != nil {
		return fmt.Errorf("Caddy ağa bağlanamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.Docker.Disconnect(bg(), spec.Network(), caddyContainer) })
	done("Docker ağı: " + spec.Network())

	if spec.Database {
		if err := o.Docker.Connect(ctx, spec.Network(), mongoContainer, mongoAlias); err != nil {
			return fmt.Errorf("MongoDB ağa bağlanamadı: %w", err)
		}
		undo = append(undo, func() { _ = o.Docker.Disconnect(bg(), spec.Network(), mongoContainer) })
		pw, err := randomSecret(32)
		if err != nil {
			return err
		}
		if err := o.mongoUser(ctx, spec, pw); err != nil {
			return err
		}
		undo = append(undo, func() { _ = o.dropMongoUser(bg(), spec) })
		env[spec.DBVar] = fmt.Sprintf("mongodb://%s:%s@%s:27017/%s?authSource=%s", spec.DBUser(), pw, mongoAlias, spec.DBName(), spec.DBName())
		done(fmt.Sprintf("MongoDB: veritabanı %s, kullanıcı %s (%s değişkeninde)", spec.DBName(), spec.DBUser(), spec.DBVar))
	}

	now := o.now().UTC()
	spec.EnvKeys, spec.CreatedAt, spec.UpdatedAt = userKeys, now, now
	if err := o.Store.WriteCompose(spec, env); err != nil {
		return err
	}
	if err := o.Store.WriteMeta(spec); err != nil {
		return err
	}
	done("compose.yaml ve footops.json yazıldı")

	if err := o.compose(ctx, spec, "pull", "--quiet"); err != nil {
		return fmt.Errorf("imaj indirilemedi (adı doğru mu, özel bir imaj mı?): %w", err)
	}
	done("İmaj indirildi: " + spec.Image)

	if spec.DataMount != "" {
		if err := o.prepareDataDir(ctx, spec); err != nil {
			return err
		}
		done(fmt.Sprintf("Kalıcı klasör: %s/%s → %s", spec.Dir(), sites.DataDir, spec.DataMount))
	}

	if err := o.compose(ctx, spec, "up", "--detach"); err != nil {
		return fmt.Errorf("container başlatılamadı: %w", err)
	}
	undo = append(undo, func() { _ = o.compose(bg(), spec, "down", "--remove-orphans") })
	if err := o.waitListening(ctx, spec, restartBase{}); err != nil {
		return err
	}
	done(fmt.Sprintf("Uygulama %d portunda cevap veriyor", spec.Port))

	if err := o.writeCaddy(ctx, spec); err != nil {
		return err
	}
	undo = append(undo, func() {
		_ = os.Remove(o.path(caddyFile(spec)))
		_ = o.caddy(bg(), "reload")
	})
	done("Caddy: " + strings.Join(spec.Domains, ", "))

	if err := o.checkHTTPS(ctx, spec.Domains[0]); err != nil {
		return err
	}
	done("HTTPS üzerinden cevap alındı. Site hazır; DNS kaydı Cloudflare'de bu sunucuyu göstermeli.")
	return nil
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
	if err := o.compose(ctx, spec, "pull", "--quiet"); err != nil {
		return fmt.Errorf("imaj indirilemedi: %w", err)
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
	return c, o.rerenderCaddy(ctx)
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
	if err := o.Store.RemoveCert(*target); err != nil {
		return err
	}
	return o.rerenderCaddy(ctx)
}

func (o *SiteOps) rerenderCaddy(ctx context.Context) error {
	list, err := o.Store.List()
	if err != nil {
		return err
	}
	certs, err := o.Store.Certs()
	if err != nil {
		return err
	}
	changed := false
	for _, s := range list {
		certFor, missing := sites.CertFor(s.Domains, certs)
		if len(missing) > 0 {
			continue
		}
		want := sites.CaddyBlock(s, certFor)
		have, _ := os.ReadFile(o.path(caddyFile(s)))
		if string(have) == want {
			continue
		}
		if err := writeFile(o.path(caddyFile(s)), want, 0o644); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	if err := o.caddy(ctx, "validate"); err != nil {
		return fmt.Errorf("Caddy yapılandırması geçersiz: %w", err)
	}
	return o.caddy(ctx, "reload")
}

// ---- helpers ----

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
	if err := writeFile(f, sites.CaddyBlock(s, certFor), 0o644); err != nil {
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

// mongoUser creates (or resets the password of) the site's database user. The script,
// which contains the root password, goes through stdin: it never appears in a process
// list. --file makes mongosh stop at the first error, unlike its interactive mode.
func (o *SiteOps) mongoUser(ctx context.Context, s sites.Spec, pw string) error {
	js := fmt.Sprintf(`const t=c.getDB(%s);const r=[{role:"readWrite",db:%s}];`+
		`if(t.getUser(%s)){t.updateUser(%s,{pwd:%s,roles:r})}else{t.createUser({user:%s,pwd:%s,roles:r})}`,
		q(s.DBName()), q(s.DBName()), q(s.DBUser()), q(s.DBUser()), q(pw), q(s.DBUser()), q(pw))
	if err := o.mongo(ctx, js); err != nil {
		return fmt.Errorf("MongoDB kullanıcısı oluşturulamadı: %w", err)
	}
	return nil
}

func (o *SiteOps) dropMongoUser(ctx context.Context, s sites.Spec) error {
	return o.mongo(ctx, fmt.Sprintf(`const t=c.getDB(%s);if(t.getUser(%s)){t.dropUser(%s)}`, q(s.DBName()), q(s.DBUser()), q(s.DBUser())))
}

func (o *SiteOps) mongo(ctx context.Context, body string) error {
	_, err := rootMongosh(ctx, o.Run, o.path(core.MongoSecretPath), body)
	return err
}

// rootMongosh runs a script as MongoDB's root user, whose password is read from the
// secrets file. Root is only for managing users and databases (rule K4).
func rootMongosh(ctx context.Context, r run.Runner, secretFile, body string) (string, error) {
	env, err := os.ReadFile(secretFile)
	if err != nil {
		return "", err
	}
	rootPW := core.MongoPasswordFromEnv(string(env))
	if rootPW == "" {
		return "", errors.New("MongoDB root şifresi bulunamadı")
	}
	return mongosh(ctx, r, core.MongoRootUser, rootPW, "admin", body)
}

// mongosh runs a script in the MongoDB container as the given user. The script, which
// contains the password, goes through stdin, so it never appears in a process list; the
// connection is c. Output before "FOOTOPS_OK" is returned for scripts that print.
func mongosh(ctx context.Context, r run.Runner, user, pw, authDB, body string) (string, error) {
	script := fmt.Sprintf(`(function(){try{const c=new Mongo("mongodb://127.0.0.1:27017");`+
		`c.getDB(%s).auth(%s,%s);%s;print("FOOTOPS_OK")}catch(e){print("FOOTOPS_ERR "+e.message);quit(3)}})()`,
		q(authDB), q(user), q(pw), body)
	out, err := r.RunInput(ctx, script+"\n", "docker", "exec", "-i", mongoContainer, "mongosh", "--quiet", "--nodb", "--file", "/dev/stdin")
	if i := strings.Index(out, "FOOTOPS_ERR "); i >= 0 {
		return out, errors.New(strings.TrimSpace(out[i+len("FOOTOPS_ERR "):]))
	}
	if err != nil {
		return out, err
	}
	if !strings.Contains(out, "FOOTOPS_OK") {
		return out, errors.New("mongosh beklenmeyen çıktı verdi")
	}
	return out, nil
}

// q quotes a string for JavaScript (JSON is valid JS).
func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// prepareDataDir creates the persistent directory owned by the user the image runs as,
// so the app can write to it without running as root.
func (o *SiteOps) prepareDataDir(ctx context.Context, s sites.Spec) error {
	d := o.path(filepath.Join(s.Dir(), sites.DataDir))
	if err := os.MkdirAll(d, 0o750); err != nil {
		return err
	}
	uid, gid := o.imageUser(ctx, s.Image)
	if err := os.Chown(d, uid, gid); err != nil {
		return err
	}
	return nil
}

// imageUser returns the uid/gid the image runs as. It asks the image itself (`id`), which
// resolves user names; images without `id` (distroless) fall back to a numeric USER or root.
func (o *SiteOps) imageUser(ctx context.Context, image string) (int, int) {
	uid, uerr := o.idIn(ctx, image, "-u")
	gid, gerr := o.idIn(ctx, image, "-g")
	if uerr == nil && gerr == nil {
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

func (o *SiteOps) idIn(ctx context.Context, image, flag string) (int, error) {
	out, err := o.Run.Run(ctx, "docker", "run", "--rm", "--network", "none", "--entrypoint", "id", image, flag)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

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
