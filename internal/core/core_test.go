package core

import (
	"strings"
	"testing"
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
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose misses %q", want)
		}
	}
	if strings.Contains(c, "latest") {
		t.Error("unpinned image")
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
	if !strings.Contains(c, "auto_https off") || !strings.Contains(c, "import /etc/caddy/sites/*.caddy") {
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
