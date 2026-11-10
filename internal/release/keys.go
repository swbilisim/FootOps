package release

import (
	"crypto/ed25519"
	"encoding/base64"
)

// builtinKeys are the public halves of the release key the Mac keeps (footops-mac yayin
// anahtar), base64. A new key comes in with a release the old one signs; the old one goes
// with a later release.
var builtinKeys = []string{
	"yBME5onXmZME6cI+mM/t967k2m85RP0XzNzeeQTWV9k=", // c849499b9b6e7bd8, 2026-11-10 (Mac: Anahtar Zinciri footops-imza)
}

// testKey is set only by the integration test's build (-ldflags "-X …/release.testKey=…"):
// a key whose private half the test holds. Empty in every other build.
var testKey string

// Trusted returns the keys this build trusts, by key id.
func Trusted() map[string]ed25519.PublicKey {
	out := map[string]ed25519.PublicKey{}
	for _, k := range append(append([]string{}, builtinKeys...), testKey) {
		raw, err := base64.StdEncoding.DecodeString(k)
		if k == "" || err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		pub := ed25519.PublicKey(raw)
		out[KeyID(pub)] = pub
	}
	return out
}
