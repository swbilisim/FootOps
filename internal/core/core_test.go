package core

import (
	"encoding/binary"
	"strconv"
	"strings"
	"testing"

	"github.com/swbilisim/FootOps/internal/mongo"
)

func TestComposePinsImagesAndBindsMongoToLoopback(t *testing.T) {
	c := Compose(Config{MongoCacheGB: 0.5})
	for _, want := range []string{
		"image: " + CaddyImage,
		"image: " + MongoImage,
		`"127.0.0.1:27017:27017"`,
		`"--wiredTigerCacheSizeGB", "0.5"`,
		"env_file: " + MongoSecretPath,
		"restart: unless-stopped",
		`      - "443:443"`,
		"    stop_grace_period: 60s\n    env_file: " + MongoSecretPath, // on the mongo service
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose misses %q", want)
		}
	}
	if strings.Contains(c, "latest") {
		t.Error("unpinned image")
	}
	// Only 443 is published: nothing answers on 80.
	if strings.Contains(c, "80:80") {
		t.Error("port 80 published")
	}
	// Mongo must never be published on all interfaces.
	if strings.Contains(c, `"27017:27017"`) || strings.Contains(c, "0.0.0.0:27017") {
		t.Error("MongoDB exposed beyond loopback")
	}
}

func TestComposeIsDeterministic(t *testing.T) {
	if Compose(Config{MongoCacheGB: 1}) != Compose(Config{MongoCacheGB: 1}) {
		t.Error("same config renders differently")
	}
	if Compose(Config{MongoCacheGB: 1}) == Compose(Config{MongoCacheGB: 0.5}) {
		t.Error("cache size not rendered")
	}
}

func TestCaddyfileNeverRequestsCertificates(t *testing.T) {
	c := Caddyfile()
	if !strings.Contains(c, "auto_https off") || !strings.Contains(c, "import /etc/caddy/sites/*.caddy") ||
		!strings.Contains(c, "trusted_proxies static 173.245.48.0/20") || !strings.Contains(c, "client_ip_headers CF-Connecting-IP\n") ||
		!strings.Contains(c, "https:// {\n\tabort\n}") {
		t.Errorf("Caddyfile = %s", c)
	}
}

func TestMongoSecretRoundTrip(t *testing.T) {
	env := MongoSecretEnv("abcDEF123")
	if got := MongoPasswordFromEnv(env); got != "abcDEF123" {
		t.Errorf("password = %q", got)
	}
	if MongoPasswordFromEnv("# nothing\n") != "" {
		t.Error("password found in empty env")
	}
}

func TestComposeTunnel(t *testing.T) {
	without := Compose(Config{MongoCacheGB: 0.5})
	if strings.Contains(without, "cloudflared") || len(Services(Config{})) != 2 {
		t.Error("cloudflared present without tunnel")
	}
	with := Compose(Config{MongoCacheGB: 0.5, Tunnel: true})
	for _, want := range []string{"image: " + CloudflaredImage, "network_mode: host", "env_file: " + TunnelSecretPath, "--no-autoupdate", `"--metrics", "127.0.0.1:20241"`} {
		if !strings.Contains(with, want) {
			t.Errorf("tunnel compose misses %q", want)
		}
	}
	if Services(Config{Tunnel: true})["cloudflared"] != "footops-cloudflared" {
		t.Error("cloudflared service not listed")
	}
	if TunnelTokenFromEnv(TunnelSecretEnv("abc")) != "abc" {
		t.Error("token round trip")
	}
}

// The healthcheck's bytes are a well-formed OP_MSG holding {ping: 1, $db: "admin"}, and
// the script survives the compose file's quoting.
func TestMongoPing(t *testing.T) {
	_, rest, _ := strings.Cut(MongoPing, `printf "`)
	esc, _, ok := strings.Cut(rest, `" >&3`)
	if !ok {
		t.Fatal("no printf argument")
	}
	var msg []byte
	for i := 0; i < len(esc); i++ {
		if esc[i] == '\\' && i+3 < len(esc) && esc[i+1] == 'x' {
			n, err := strconv.ParseUint(esc[i+2:i+4], 16, 8)
			if err != nil {
				t.Fatal(err)
			}
			msg = append(msg, byte(n))
			i += 3
			continue
		}
		msg = append(msg, esc[i])
	}
	le := binary.LittleEndian
	if int(le.Uint32(msg)) != len(msg) || le.Uint32(msg[12:]) != 2013 || le.Uint32(msg[16:]) != 0 || msg[20] != 0 {
		t.Fatalf("header: % x", msg[:21])
	}
	doc := mongo.Doc(msg[21:])
	if err := doc.Validate(); err != nil || int(le.Uint32(doc)) != len(doc) {
		t.Fatalf("document: %v", err)
	}
	text, _ := mongo.DocText(doc, false)
	if text != `{"ping": 1, "$db": "admin"}` {
		t.Errorf("command = %s", text)
	}
	if strings.ContainsAny(MongoPing, `'$%`) {
		t.Error("the script holds a quote, $ or % (compose quoting, substitution, Sprintf)")
	}
	if !strings.Contains(Compose(Config{MongoCacheGB: 0.5}), `test: ["CMD", "bash", "-c", '`+MongoPing+`']`) {
		t.Error("compose lacks the healthcheck")
	}
}
