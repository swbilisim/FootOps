package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Parking (Faz P1b, panel-tasarim §28.3): a parked site's domains answer a neutral
// maintenance page straight from Caddy (sites.ParkBlock) and its container is stopped —
// kept, with its image, so that bringing the site back needs no download and loses
// nothing the app wrote inside it. The site's record says so (footops.json "parked"):
// whatever renders its Caddy block, or would start its container, reads it there.

var (
	// errParked refuses work that would start a parked site's container.
	errParked = errors.New("site parkta: önce yayına al")
	// errSiteBusy: a site is being made or changed, which takes minutes; a quick change
	// does not wait for it.
	errSiteBusy = errors.New("şu an bu sunucuda bir site işlemi sürüyor; biraz sonra yeniden dene")
	// errExpiryChanged: the end date is no longer the one the change was made from (a
	// second click, another page, the chat).
	errExpiryChanged = errors.New("bitiş tarihi bu arada değişti")
)

// quickLock is how long a quick change (an end date) waits for the site operations'
// lock: the loops hold it for moments, a site being made or changed for minutes.
var quickLock = 3 * time.Second

// lockQuick takes o.mu if it comes free within quickLock.
func (o *SiteOps) lockQuick() bool {
	deadline := time.Now().Add(quickLock)
	for !o.mu.TryLock() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// SetExpiry changes the site's end date ("" : none). With was, only if the end date is
// still that one: "+1 ay" pressed twice, or over a change made elsewhere meanwhile,
// does not extend twice. Only its record changes: nothing is restarted, and a parked
// site stays parked.
func (o *SiteOps) SetExpiry(name, expires string, was *string) (sites.Spec, error) {
	expires = strings.TrimSpace(expires)
	if err := sites.ValidExpiry(expires); err != nil {
		return sites.Spec{}, &sites.ValidationError{Problems: []string{"bitiş tarihi: " + err.Error()}}
	}
	if !o.lockQuick() {
		return sites.Spec{}, errSiteBusy
	}
	defer o.mu.Unlock()
	spec, err := o.Store.Get(name)
	if err != nil {
		return spec, err
	}
	if was != nil && strings.TrimSpace(*was) != spec.Expires {
		now := "süresiz"
		if spec.Expires != "" {
			now = dayTR(spec.Expires)
		}
		return spec, fmt.Errorf("%w (şu an %s): sayfaya bakıp yeniden dene", errExpiryChanged, now)
	}
	spec.Expires = expires
	if err := o.Store.WriteMeta(spec); err != nil {
		return spec, err
	}
	o.dropTerms()
	return spec, nil
}

// Park parks the site: its record first, then the maintenance page in its Caddy block,
// then its container stopped — so visitors never meet a gateway error on the way.
// Parking a parked site again finishes what an earlier try could not (a container
// left running, a block left as it was).
func (o *SiteOps) Park(ctx context.Context, name, by string, progress Progress) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	defer o.dropTerms()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	if spec.IsParked() {
		progress("ℹ Site zaten parkta (" + localTime(spec.Parked.At) + "); park sayfası ve container'ın durması denetleniyor")
		// Also a block removed or changed by hand since: the record decides.
		if err := o.setBlock(ctx, spec); err != nil {
			return fmt.Errorf("park sayfası yazılamadı: %w", err)
		}
	} else {
		prev := spec
		spec.Parked = &sites.ParkState{At: o.now().UTC().Truncate(time.Second), By: by}
		if err := o.Store.WriteMeta(spec); err != nil {
			return err
		}
		if err := o.setBlock(ctx, spec); err != nil {
			if werr := o.Store.WriteMeta(prev); werr != nil {
				return fmt.Errorf("park sayfası yazılamadı: %v; site kaydı da geri alınamadı: %v", err, werr)
			}
			return fmt.Errorf("park sayfası yazılamadı, site değişmedi: %w", err)
		}
		progress("✓ Caddy: " + strings.Join(spec.Domains, ", ") + " artık bakım sayfasını gösteriyor (HTTP 503)")
	}
	in, err := o.Docker.Inspect(ctx, spec.Container())
	switch {
	case docker.IsNotFound(err):
		progress("ℹ Container yok; durdurulacak bir şey yok")
	case err != nil:
		return fmt.Errorf("container'ın durumu okunamadı (park sayfası yayında; Parka al'ı yeniden dene): %w", err)
	case !in.State.Running:
		progress("ℹ Container zaten duruyor")
	default:
		if err := o.compose(ctx, spec, "stop"); err != nil {
			return fmt.Errorf("container durdurulamadı (park sayfası yayında; Parka al'ı yeniden dene): %w", err)
		}
		progress("✓ Container durduruldu (silinmedi; imajı ve verisi yerinde)")
	}
	if code, n, err := o.page(ctx, spec.Domains[0]); err != nil || code != 503 || n == 0 {
		got := fmt.Sprintf("HTTP %d", code)
		if err != nil {
			got = err.Error()
		}
		progress("⚠ " + spec.Domains[0] + " bu sunucuda bakım sayfası yerine şunu verdi: " + got)
	} else {
		progress("✓ " + spec.Domains[0] + " bakım sayfasını veriyor")
	}
	progress("✓ Site parkta. Veri ve yedekler yerinde; geri açmak için: Yayına al")
	return nil
}

// Unpark brings a parked site back with a new end date ("" : none): its container
// starts and must answer, then its record and its own Caddy block come back, and the
// site must answer through Caddy. If any of it fails, the container is stopped again
// and the maintenance page stays.
func (o *SiteOps) Unpark(ctx context.Context, name, expires string, progress Progress) (err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	expires = strings.TrimSpace(expires)
	if err := UnparkExpiry(expires, o.now()); err != nil {
		return err
	}
	defer o.dropTerms()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	if !spec.IsParked() {
		return errors.New("site parkta değil")
	}
	parked := spec
	unparked, blockSet := false, false
	defer func() {
		if err == nil {
			return
		}
		// Back to parked: the page first, then the container stopped (fresh context: the
		// job's may be why it failed).
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if unparked {
			if werr := o.Store.WriteMeta(parked); werr != nil {
				progress("✗ Site kaydı parka geri alınamadı: " + werr.Error())
			}
		}
		if blockSet {
			if berr := o.setBlock(bg, parked); berr != nil {
				progress("✗ Park sayfası geri konamadı: " + berr.Error())
			}
		}
		if serr := o.compose(bg, parked, "stop"); serr != nil {
			progress("✗ Container yeniden durdurulamadı: " + serr.Error())
		} else {
			progress("ℹ Container yeniden durduruldu; site parkta kaldı")
		}
	}()
	if err := o.syncCompose(spec, progress); err != nil {
		return err
	}
	base := o.restartBaseline(ctx, spec)
	if err := o.compose(ctx, spec, "up", "--detach"); err != nil {
		return fmt.Errorf("container başlatılamadı: %w", err)
	}
	if err := o.waitListening(ctx, spec, base); err != nil {
		return err
	}
	progress(fmt.Sprintf("✓ Uygulama %d portunda cevap veriyor", spec.Port))
	spec.Parked, spec.Expires, spec.Resumed = nil, expires, o.now().UTC().Truncate(time.Second)
	if err := o.Store.WriteMeta(spec); err != nil {
		return err
	}
	unparked = true
	if err := o.setBlock(ctx, spec); err != nil {
		return fmt.Errorf("Caddy: %w", err)
	}
	blockSet = true
	progress("✓ Caddy: " + strings.Join(spec.Domains, ", ") + " yeniden siteye yönlendi")
	if err := o.checkHTTPS(ctx, spec.Domains[0]); err != nil {
		return err
	}
	until := "süresiz"
	if expires != "" {
		until = "bitiş " + dayTR(expires)
	}
	progress("✓ Site yeniden yayında (" + until + ")")
	return nil
}

// FinishParks stops the running containers of parked sites: a park the agent was cut
// off in, or whose stop failed, is finished once the agent runs again. (A site being
// brought back keeps its record parked until it answers: cut off then, it is stopped
// again here and stays parked.)
func (o *SiteOps) FinishParks(ctx context.Context, logf func(string, ...any)) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	list, err := o.Store.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range list {
		if !s.IsParked() {
			continue
		}
		in, err := o.Docker.Inspect(ctx, s.Container())
		switch {
		case docker.IsNotFound(err) || (err == nil && !in.State.Running):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", s.Name, err))
			continue
		}
		if err := o.compose(ctx, s, "stop"); err != nil {
			errs = append(errs, fmt.Errorf("%s: container durdurulamadı: %w", s.Name, err))
			continue
		}
		logf("parktaki %s sitesinin çalışan container'ı durduruldu (yarım kalmış bir park tamamlandı)", s.Name)
	}
	return errors.Join(errs...)
}

// UnparkExpiry checks the end date a site comes back with: none, or one to come (a
// passed one would ask at once to park it again).
func UnparkExpiry(expires string, now time.Time) error {
	if err := sites.ValidExpiry(expires); err != nil {
		return &sites.ValidationError{Problems: []string{"bitiş tarihi: " + err.Error()}}
	}
	if left, ok := sites.DaysLeft(expires, now.Local()); ok && left <= 0 {
		return &sites.ValidationError{Problems: []string{"yeni bitiş tarihi bugünden sonra olmalı"}}
	}
	return nil
}

// setBlock gives the site the Caddy block its record calls for (sites.SiteBlock) in
// place of the one it has, validates the whole configuration and reloads. If Caddy
// refuses either, the block that was there goes back (none: none), so the files on disk
// stay what Caddy runs.
func (o *SiteOps) setBlock(ctx context.Context, s sites.Spec) error {
	certs, err := o.Store.Certs()
	if err != nil {
		return err
	}
	certFor, missing := sites.CertFor(s.Domains, certs)
	if len(missing) > 0 {
		return fmt.Errorf("sertifika yok: %s", strings.Join(missing, ", "))
	}
	f := o.path(caddyFile(s))
	old, rerr := os.ReadFile(f)
	if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return rerr
	}
	want := sites.SiteBlock(s, certFor)
	if rerr == nil && string(old) == want {
		return nil
	}
	restore := func() {
		if rerr == nil {
			_ = writeFile(f, string(old), 0o644)
		} else {
			_ = os.Remove(f)
		}
	}
	if err := writeFile(f, want, 0o644); err != nil {
		restore()
		return err
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

// SiteTerm is what the alerts and the reminders need of a site: its end date and
// whether it is parked.
type SiteTerm struct {
	Name      string
	Container string
	Domains   []string
	Expires   string
	Parked    *sites.ParkState
}

// termsFresh is how long Terms answers from memory: the alerts ask every sample.
const termsFresh = 30 * time.Second

// siteTerms is Terms' memory, dropped by every change to an end date or a parking.
type siteTerms struct {
	mu   sync.Mutex
	list []SiteTerm
	at   time.Time
	err  error
}

// Terms lists the sites' end dates and parking, from the site files at most termsFresh
// ago. A listing that fails keeps the last one (with its error).
func (o *SiteOps) Terms() ([]SiteTerm, error) {
	o.terms.mu.Lock()
	defer o.terms.mu.Unlock()
	if !o.terms.at.IsZero() && time.Since(o.terms.at) < termsFresh {
		return o.terms.list, o.terms.err
	}
	list, err := o.Store.List()
	o.terms.at, o.terms.err = time.Now(), err
	if err != nil {
		return o.terms.list, err
	}
	out := make([]SiteTerm, 0, len(list))
	for _, s := range list {
		out = append(out, SiteTerm{Name: s.Name, Container: s.Container(), Domains: s.Domains, Expires: s.Expires, Parked: s.Parked})
	}
	o.terms.list = out
	return out, nil
}

func (o *SiteOps) dropTerms() {
	o.terms.mu.Lock()
	o.terms.at = time.Time{}
	o.terms.mu.Unlock()
}

// ParkedContainers names the containers of parked sites: stopped on purpose.
func (o *SiteOps) ParkedContainers() map[string]bool {
	list, _ := o.Terms()
	out := map[string]bool{}
	for _, t := range list {
		if t.Parked != nil {
			out[t.Container] = true
		}
	}
	return out
}

// dayTR writes an end date as the panel and the chat do: "15.12.2026".
func dayTR(day string) string {
	t, err := time.Parse(sites.DayLayout, day)
	if err != nil {
		return day
	}
	return t.Format("02.01.2006")
}
