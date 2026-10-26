// Package core defines the shared services every FootOps server runs — Caddy and
// MongoDB — and renders their configuration files. The installer writes these files;
// later the agent re-renders them when sites are added.
package core

import (
	"fmt"
	"strconv"
	"strings"
)

// Pinned images. Bumping a version is a FootOps release, never an implicit "latest".
const (
	CaddyImage = "caddy:2.11.7-alpine"
	// MongoDB 7.0 keeps compatibility with the older drivers (Mongoose 5 / Node 14)
	// still used by some sites.
	MongoImage = "mongo:7.0.45"
	// cloudflared connects the panel to Cloudflare Tunnel (outbound only).
	CloudflaredImage = "cloudflare/cloudflared:2026.10.3"
)

// Host paths.
const (
	Dir         = "/srv/core"
	ComposePath = Dir + "/compose.yaml"
	ProjectName = "footops-core"

	CaddyDir       = Dir + "/caddy"
	CaddyEtcDir    = CaddyDir + "/etc" // mounted read-only at /etc/caddy
	CaddyfilePath  = CaddyEtcDir + "/Caddyfile"
	CaddySitesDir  = CaddyEtcDir + "/sites"
	CaddyCertsDir  = CaddyEtcDir + "/certs"
	CaddyDataDir   = CaddyDir + "/data"
	CaddyConfigDir = CaddyDir + "/config"

	MongoDir        = Dir + "/mongo"
	MongoDataDir    = MongoDir + "/data"
	MongoConfigDir  = MongoDir + "/configdb"
	MongoRootUser   = "root"
	MongoSecretPath = "/etc/footops/secrets/mongo.env"

	TunnelSecretPath = "/etc/footops/secrets/cloudflared.env"
)

// Config holds the values that vary between servers.
type Config struct {
	MongoCacheGB float64
	Tunnel       bool // run cloudflared for the panel
}

// Services maps compose service names to their fixed container names.
func Services(c Config) map[string]string {
	m := map[string]string{
		"caddy": "footops-caddy",
		"mongo": "footops-mongo",
	}
	if c.Tunnel {
		m["cloudflared"] = "footops-cloudflared"
	}
	return m
}

// Caddyfile is the main Caddy config. Sites are separate files under sites/, so adding
// or removing one never rewrites this file.
//
// TLS comes from Cloudflare Origin CA certificates referenced by each site block, so
// automatic HTTPS is off: Caddy never contacts Let's Encrypt, whose validation would
// not reach an origin that only accepts Cloudflare.
//
// Requests arrive from Cloudflare: its ranges are trusted proxies, so Caddy takes the
// visitor's address from CF-Connecting-IP and passes it on in X-Forwarded-For.
func Caddyfile() string {
	return `# Managed by FootOps. Site blocks live in sites/*.caddy.
{
	auto_https off
	admin 127.0.0.1:2019
	servers {
		trusted_proxies static ` + strings.Join(CloudflareRanges, " ") + `
		client_ip_headers CF-Connecting-IP X-Forwarded-For
	}
}

import /etc/caddy/sites/*.caddy

# A host name no site claims (e.g. a removed site still covered by a wildcard
# certificate) gets its connection dropped instead of Caddy's default empty 200.
https:// {
	abort
}
`
}

// CloudflareRanges are Cloudflare's published edge ranges (cloudflare.com/ips).
var CloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}

// Compose renders the core compose file.
//
// Notes on choices:
//   - Only Caddy publishes ports to the internet. MongoDB is published on 127.0.0.1 for
//     SSH tunnels (Robo 3T); apps reach it over Docker networks.
//   - Directories are bind-mounted (not single files) so atomically replaced config files
//     are seen by the container.
//   - Healthchecks use 127.0.0.1: in Alpine "localhost" resolves to ::1 first, where
//     Caddy's admin endpoint does not listen.
func Compose(c Config) string {
	cache := strconv.FormatFloat(c.MongoCacheGB, 'f', -1, 64)
	var b strings.Builder
	fmt.Fprintf(&b, `# Managed by FootOps. Changes are overwritten by "footops install".
name: %s

services:
  caddy:
    image: %s
    container_name: %s
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - %s:/etc/caddy:ro
      - %s:/data
      - %s:/config
    cap_drop: [ALL]
    cap_add: [NET_BIND_SERVICE]
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:2019/config/"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s

  mongo:
    image: %s
    container_name: %s
    restart: unless-stopped
    command: ["mongod", "--wiredTigerCacheSizeGB", "%s"]
    env_file: %s
    ports:
      - "127.0.0.1:27017:27017"
    volumes:
      - %s:/data/db
      - %s:/data/configdb
    ulimits:
      nofile: {soft: 64000, hard: 64000}
    healthcheck:
      test: ["CMD", "mongosh", "--quiet", "--eval", "db.adminCommand('ping').ok"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 40s
`,
		ProjectName,
		CaddyImage, "footops-caddy", CaddyEtcDir, CaddyDataDir, CaddyConfigDir,
		MongoImage, "footops-mongo", cache, MongoSecretPath, MongoDataDir, MongoConfigDir,
	)
	if c.Tunnel {
		// Host networking: cloudflared must reach the panel on 127.0.0.1, and it only
		// makes outbound connections, so no port is published. Its metrics endpoint
		// defaults to all interfaces inside containers; keep it on loopback.
		fmt.Fprintf(&b, `
  cloudflared:
    image: %s
    container_name: footops-cloudflared
    restart: unless-stopped
    command: ["tunnel", "--no-autoupdate", "--metrics", "127.0.0.1:20241", "run"]
    env_file: %s
    network_mode: host
    cap_drop: [ALL]
`, CloudflaredImage, TunnelSecretPath)
	}
	return b.String()
}

// TunnelSecretEnv renders the env file holding the tunnel token.
func TunnelSecretEnv(token string) string {
	return "# Managed by FootOps\nTUNNEL_TOKEN=" + token + "\n"
}

// TunnelTokenFromEnv extracts the token from the env file content.
func TunnelTokenFromEnv(env string) string {
	for _, l := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "TUNNEL_TOKEN="); ok {
			return v
		}
	}
	return ""
}

// MongoSecretEnv renders the env file holding the MongoDB root credentials. MongoDB reads
// it only when it initialises an empty data directory.
func MongoSecretEnv(password string) string {
	return fmt.Sprintf(`# Managed by FootOps. Read only when the database is first created:
# changing it later does NOT change the password.
MONGO_INITDB_ROOT_USERNAME=%s
MONGO_INITDB_ROOT_PASSWORD=%s
`, MongoRootUser, password)
}

// MongoPasswordFromEnv extracts the root password from the env file content.
func MongoPasswordFromEnv(env string) string {
	for _, l := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "MONGO_INITDB_ROOT_PASSWORD="); ok {
			return v
		}
	}
	return ""
}
