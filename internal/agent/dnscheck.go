package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
)

// DomainCheck is what this server can tell about one of a site's domains: where DNS
// points, whether the site answers through Cloudflare, and whether Caddy here serves it.
// Behind Cloudflare's proxy the origin itself cannot be seen from outside.
type DomainCheck struct {
	Domain    string   `json:"domain"`
	DNS       string   `json:"dns"` // cloudflare, here, elsewhere, none
	IPs       []string `json:"ips,omitempty"`
	DNSErr    string   `json:"dns_error,omitempty"`
	Public    int      `json:"public"` // HTTPS status from the internet; 0 when it failed
	PublicErr string   `json:"public_error,omitempty"`
	CFRay     bool     `json:"cf_ray"` // the answer came through Cloudflare
	Local     int      `json:"local"`  // HTTPS status from this server's Caddy
	LocalErr  string   `json:"local_error,omitempty"`
}

var cloudflareNets = func() []netip.Prefix {
	out := make([]netip.Prefix, 0, len(core.CloudflareRanges))
	for _, r := range core.CloudflareRanges {
		out = append(out, netip.MustParsePrefix(r))
	}
	return out
}()

func isCloudflare(a netip.Addr) bool {
	for _, p := range cloudflareNets {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// classify names where a domain's addresses lead.
func classify(ips []netip.Addr, here map[netip.Addr]bool) string {
	if len(ips) == 0 {
		return "none"
	}
	cf, local := true, true
	for _, a := range ips {
		cf = cf && isCloudflare(a)
		local = local && here[a.Unmap()]
	}
	switch {
	case cf:
		return "cloudflare"
	case local:
		return "here"
	}
	return "elsewhere"
}

// dnsResolver asks Cloudflare's public resolver first: it sees a change in a Cloudflare
// zone at once, where the server's own resolver may still cache the old answer.
var dnsResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, "1.1.1.1:53")
}}

// CheckDNS checks every domain of a site at once. It only reads; nothing is cached.
func (o *SiteOps) CheckDNS(ctx context.Context, name string) ([]DomainCheck, error) {
	o.init()
	spec, err := o.Store.Get(name)
	if err != nil {
		return nil, err
	}
	here := map[netip.Addr]bool{}
	info := publicAddrs()
	for _, s := range append(info.IPv4, info.IPv6...) {
		if a, err := netip.ParseAddr(s); err == nil {
			here[a.Unmap()] = true
		}
	}
	out := make([]DomainCheck, len(spec.Domains))
	var wg sync.WaitGroup
	for i, d := range spec.Domains {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = o.checkDomain(ctx, d, here)
		}()
	}
	wg.Wait()
	return out, nil
}

func (o *SiteOps) checkDomain(ctx context.Context, domain string, here map[netip.Addr]bool) DomainCheck {
	c := DomainCheck{Domain: domain}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		lookup := func(r *net.Resolver) ([]netip.Addr, error) {
			lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return r.LookupNetIP(lctx, "ip", domain)
		}
		addrs, err := lookup(dnsResolver)
		if err != nil {
			// The server's own resolver, in case port 53 to 1.1.1.1 is blocked; with time
			// of its own, since a dropped packet uses up the first try's.
			addrs, err = lookup(net.DefaultResolver)
		}
		var dnsErr *net.DNSError
		switch {
		case err == nil:
		case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
			err = nil
		default:
			c.DNSErr = clipMsg(err.Error(), 200)
		}
		for _, a := range addrs {
			c.IPs = append(c.IPs, a.Unmap().String())
		}
		sort.Strings(c.IPs)
		if err == nil {
			c.DNS = classify(addrs, here)
		}
	}()
	go func() {
		defer wg.Done()
		code, _, err := o.page(ctx, domain)
		if err != nil {
			c.LocalErr = clipMsg(err.Error(), 200)
		} else {
			c.Local = code
		}
	}()
	wg.Wait()
	if c.DNS == "cloudflare" || c.DNS == "here" || c.DNS == "elsewhere" {
		c.Public, c.CFRay, c.PublicErr = publicHTTPS(ctx, domain)
	}
	return c
}

// publicHTTPS asks for the site the way a visitor does, with certificates verified.
func publicHTTPS(ctx context.Context, domain string) (int, bool, string) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	if err != nil {
		return 0, false, err.Error()
	}
	req.Header.Set("User-Agent", "FootOps-DNS-check")
	resp, err := c.Do(req)
	if err != nil {
		return 0, false, clipMsg(err.Error(), 200)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header.Get("Cf-Ray") != "", ""
}
