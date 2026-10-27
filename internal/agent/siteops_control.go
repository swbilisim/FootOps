package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Data work (rule K3) stops a site while its database is replaced and starts it again
// afterwards, then asks it for a page the way a visitor would.

// Running reports whether the site's container runs. A site without a container (never
// started, or removed by hand) is not running.
func (o *SiteOps) Running(ctx context.Context, name string) (bool, error) {
	spec, err := o.Store.Get(name)
	if err != nil {
		return false, err
	}
	in, err := o.Docker.Inspect(ctx, spec.Container())
	if docker.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return in.State.Running, nil
}

// Stop stops the site's container; it keeps its configuration and can be started again.
func (o *SiteOps) Stop(ctx context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	if err := o.compose(ctx, spec, "stop"); err != nil {
		return fmt.Errorf("site durdurulamadı: %w", err)
	}
	return nil
}

// Start starts the site's container and waits until the app accepts connections.
func (o *SiteOps) Start(ctx context.Context, name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.init()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	base := o.restartBaseline(ctx, spec)
	if err := o.compose(ctx, spec, "up", "--detach"); err != nil {
		return fmt.Errorf("site başlatılamadı: %w", err)
	}
	return o.waitListening(ctx, spec, base)
}

// healthTries × healthPause is how long a freshly started app gets to serve its first
// page (frameworks compile or warm caches on the first request).
const (
	healthTries = 15
	healthPause = 2 * time.Second
)

// Health requests the site's first domain from this server's Caddy, up to tries times.
// A status below 500 with a body counts as healthy (a redirect needs no body); 5xx, an
// empty page or no answer at all does not.
func (o *SiteOps) Health(ctx context.Context, name string, tries int) error {
	o.init()
	spec, err := o.Store.Get(name)
	if err != nil {
		return err
	}
	var last error
	for i := 0; i < tries; i++ {
		code, n, err := o.page(ctx, spec.Domains[0])
		switch {
		case err != nil:
			last = err
		case code >= 500:
			last = fmt.Errorf("HTTP %d", code)
		case code >= 300 && code < 400, n > 0:
			return nil
		default:
			last = fmt.Errorf("HTTP %d ama sayfa boş", code)
		}
		if i+1 < tries {
			if err := o.sleep(ctx, healthPause); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("site sağlıklı cevap vermedi (%s): %w", spec.Domains[0], last)
}

// SetDataURL records where the site serves its data directory's files ("/uploads"; ""
// forgets it). Only the site's record changes: nothing is restarted.
func (o *SiteOps) SetDataURL(name, u string) (sites.Spec, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	spec, err := o.Store.Get(name)
	if err != nil {
		return spec, err
	}
	spec.DataURL = u
	spec.Normalize()
	if spec.DataURL != "" && !sites.ValidDataURL(spec.DataURL) {
		return spec, &sites.ValidationError{Problems: []string{"web adresi / ile başlayan bir yol olmalı (örn. /uploads)"}}
	}
	return spec, o.Store.WriteMeta(spec)
}
