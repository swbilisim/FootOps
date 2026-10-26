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

// CaddyBlock renders the site's Caddy configuration.
//
// Addresses carry an explicit https:// so Caddy listens on 443 only (automatic HTTPS
// is off globally). Each domain is matched to the certificate that covers it; one
// block per certificate keeps the tls directive unambiguous.
func CaddyBlock(s Spec, certFor map[string]Cert) string {
	byCert := map[string][]string{}
	var order []string
	for _, d := range s.Domains {
		id := certFor[d].ID
		if _, ok := byCert[id]; !ok {
			order = append(order, id)
		}
		byCert[id] = append(byCert[id], d)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by FootOps: site %s (%s)\n", s.Name, s.Group)
	for _, id := range order {
		addrs := make([]string, len(byCert[id]))
		for i, d := range byCert[id] {
			addrs[i] = "https://" + d
		}
		c := certFor[byCert[id][0]]
		fmt.Fprintf(&b, `%s {
	tls %s %s
	encode zstd gzip
	reverse_proxy %s:%d
}
`, strings.Join(addrs, ", "), c.CertPath(), c.KeyPath(), s.Container(), s.Port)
	}
	return b.String()
}
