package sites

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Hardening applied to every site container. Apps that need more (e.g. an entrypoint
// that switches users) are expected to be fixed in their image, not by loosening this.
const (
	pidsLimit = 512
)

// yamlString quotes s as a JSON string, which is valid YAML and escapes everything.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// composeValue escapes s for a compose file: compose interpolates $VAR, so a literal $
// must be written as $$.
func composeValue(s string) string {
	return yamlString(strings.ReplaceAll(s, "$", "$$"))
}

// Compose renders the site's compose file. env holds every variable the container gets,
// including the database URI; the file is therefore written 0600.
func Compose(s Spec, env map[string]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by FootOps. Changes are overwritten from the panel.
name: %s

services:
  app:
    image: %s
    container_name: %s
    restart: unless-stopped
    networks: [site]
    mem_limit: %dm
    memswap_limit: %dm
    pids_limit: %d
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
`, s.Name, yamlString(s.Image), s.Container(), s.MemoryMB, s.MemoryMB, pidsLimit)
	if s.DataMount != "" {
		fmt.Fprintf(&b, "    volumes:\n      - %s\n", yamlString(s.Dir()+"/"+DataDir+":"+s.DataMount))
	}
	if len(env) > 0 {
		b.WriteString("    environment:\n")
		for _, k := range SortedKeys(env) {
			fmt.Fprintf(&b, "      %s: %s\n", k, composeValue(env[k]))
		}
	}
	fmt.Fprintf(&b, `
networks:
  site:
    name: %s
    external: true
`, s.Network())
	return b.String()
}

// ParseComposeEnv reads the environment back from a compose file written by Compose, so
// a site can be changed without the panel ever holding its secrets. It only understands
// that exact layout and refuses anything else rather than guess.
func ParseComposeEnv(text string) (map[string]string, error) {
	env := map[string]string{}
	in := false
	for i, line := range strings.Split(text, "\n") {
		if line == "    environment:" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if !strings.HasPrefix(line, "      ") {
			break // end of the block
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "      "), ": ")
		var s string
		if !ok || !reEnvKey.MatchString(k) || json.Unmarshal([]byte(v), &s) != nil {
			return nil, fmt.Errorf("compose.yaml satır %d anlaşılamadı (elle mi değiştirildi?)", i+1)
		}
		env[k] = strings.ReplaceAll(s, "$$", "$")
	}
	return env, nil
}

// CaddyBlock renders the site's Caddy configuration.
//
// Addresses carry an explicit https:// so Caddy listens on 443 only (automatic HTTPS
// is off globally). Each domain is matched to the certificate that covers it; one
// block per certificate keeps the tls directive unambiguous.
//
// X-Real-IP carries the visitor's address as Caddy determined it ({client_ip}, see
// core.Caddyfile) and replaces whatever the visitor sent under that name. Apps should
// read it rather than X-Forwarded-For, whose first entries the visitor chooses.
//
// request_body caps a request's body at the site's BodyMB: a larger one, declared or
// streamed, is answered 413 before the app has to hold it.
func CaddyBlock(s Spec, certFor map[string]Cert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by FootOps: site %s (%s)\n", s.Name, s.Group)
	for _, g := range certGroups(s, certFor) {
		fmt.Fprintf(&b, `%s {
	tls %s %s
	encode zstd gzip
	request_body {
		max_size %dMiB
	}
	reverse_proxy %s:%d {
		header_up X-Real-IP {client_ip}
	}
}
`, g.addrs, g.cert.CertPath(), g.cert.KeyPath(), s.BodyMB(), s.Container(), s.Port)
	}
	return b.String()
}

// ParkTitle and ParkPage are what a parked site's domains answer (Faz P1b): a neutral
// maintenance page. Nothing on it says why the site is away, or what serves it. It
// holds no brace, backtick or dollar sign: it goes into the Caddyfile as a backtick
// string, where Caddy would read {…} as a placeholder and {$…} as the environment.
const (
	ParkTitle = "Bakım çalışması"
	ParkPage  = `<!doctype html>
<html lang="tr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>` + ParkTitle + `</title>
</head>
<body style="margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;font-family:system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;text-align:center">
<main style="max-width:32rem;padding:2rem 1.5rem">
<h1 style="font-size:1.6rem;font-weight:600;margin:0 0 0.75rem">` + ParkTitle + `</h1>
<p style="margin:0;line-height:1.6;opacity:0.75">Bu site şu anda bakımda. Kısa süre içinde yeniden yayında olacak.</p>
</main>
</body>
</html>
`
	// ParkRetryAfter (seconds) tells visitors' software, and search engines, that the
	// 503 is for a while.
	ParkRetryAfter = 3600
)

// ParkBlock renders a parked site's Caddy configuration: its domains, under their
// certificates as in CaddyBlock, answer ParkPage with 503 Service Unavailable. No
// container is involved.
func ParkBlock(s Spec, certFor map[string]Cert) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by FootOps: site %s (%s), parked\n", s.Name, s.Group)
	for _, g := range certGroups(s, certFor) {
		fmt.Fprintf(&b, `%s {
	tls %s %s
	header {
		Content-Type "text/html; charset=utf-8"
		Cache-Control no-store
		Retry-After %d
	}
	respond `+"`%s`"+` 503
}
`, g.addrs, g.cert.CertPath(), g.cert.KeyPath(), ParkRetryAfter, ParkPage)
	}
	return b.String()
}

// SiteBlock is the block the site should have now: ParkBlock while it is parked.
func SiteBlock(s Spec, certFor map[string]Cert) string {
	if s.IsParked() {
		return ParkBlock(s, certFor)
	}
	return CaddyBlock(s, certFor)
}

// certGroup is the site's domains one certificate covers, as Caddy addresses.
type certGroup struct {
	addrs string
	cert  Cert
}

func certGroups(s Spec, certFor map[string]Cert) []certGroup {
	byCert := map[string][]string{}
	var order []string
	for _, d := range s.Domains {
		id := certFor[d].ID
		if _, ok := byCert[id]; !ok {
			order = append(order, id)
		}
		byCert[id] = append(byCert[id], d)
	}
	out := make([]certGroup, 0, len(order))
	for _, id := range order {
		addrs := make([]string, len(byCert[id]))
		for i, d := range byCert[id] {
			addrs[i] = "https://" + d
		}
		out = append(out, certGroup{addrs: strings.Join(addrs, ", "), cert: certFor[byCert[id][0]]})
	}
	return out
}
