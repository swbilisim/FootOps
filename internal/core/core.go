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
)

// Services maps compose service names to their fixed container names.
var Services = map[string]string{
	"caddy": "footops-caddy",
	"mongo": "footops-mongo",
}

// Config holds the values that vary between servers.
type Config struct {
	MongoCacheGB float64
}

// Caddyfile is the main Caddy config. Sites are separate files under sites/, so adding
// or removing one never rewrites this file.
//
// TLS comes from Cloudflare Origin CA certificates referenced by each site block, so
// automatic HTTPS is off: Caddy never contacts Let's Encrypt, whose validation would
// not reach an origin that only accepts Cloudflare.
func Caddyfile() string {
	return `# Managed by FootOps. Site blocks live in sites/*.caddy.
{
	auto_https off
	admin 127.0.0.1:2019
}

import /etc/caddy/sites/*.caddy
`
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
		CaddyImage, Services["caddy"], CaddyEtcDir, CaddyDataDir, CaddyConfigDir,
		MongoImage, Services["mongo"], cache, MongoSecretPath, MongoDataDir, MongoConfigDir,
	)
	return b.String()
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
