package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func manifestBytes(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sample() Manifest {
	return Manifest{Format: 1, Version: "v0.2.0", Commit: "abc1234", Date: "2026-11-12T10:00:00Z", Notes: "Eski imajlar silinir.",
		Files: map[string]File{BinaryName("amd64"): {SHA256: strings.Repeat("ab", 32), Size: 15 << 20}}}
}

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	other, otherPriv, _ := ed25519.GenerateKey(nil)
	keys := map[string]ed25519.PublicKey{KeyID(pub): pub}
	man := manifestBytes(t, sample())
	sig := Sign(priv, man)
	if !strings.HasPrefix(string(sig), "footops-imza-1 "+KeyID(pub)+" ") || len(KeyID(pub)) != 16 {
		t.Fatalf("signature file = %q", sig)
	}
	m, err := Verify(man, sig, keys)
	if err != nil || m.Version != "v0.2.0" || m.Files[BinaryName("amd64")].Size != 15<<20 {
		t.Fatalf("verify = %+v, %v", m, err)
	}
	// A changed byte, another key, a key nobody trusts.
	bad := append([]byte{}, man...)
	bad[len(bad)-2] ^= 1
	if _, err := Verify(bad, sig, keys); !errors.Is(err, ErrSignature) {
		t.Errorf("tampered manifest: %v", err)
	}
	forged := Sign(otherPriv, man)
	if _, err := Verify(man, forged, keys); !errors.Is(err, ErrUntrusted) {
		t.Errorf("unknown key: %v", err)
	}
	// The other key's signature under the trusted key's id.
	swapped := strings.Replace(string(forged), KeyID(other), KeyID(pub), 1)
	if _, err := Verify(man, []byte(swapped), keys); !errors.Is(err, ErrSignature) {
		t.Errorf("relabelled signature: %v", err)
	}
	for _, s := range []string{"", "footops-imza-2 " + KeyID(pub) + " AAAA", "footops-imza-1 " + KeyID(pub) + " !!!", "footops-imza-1 " + KeyID(pub) + " AAAA"} {
		if _, err := Verify(man, []byte(s), keys); err == nil {
			t.Errorf("signature file %q accepted", s)
		}
	}
}

func TestValidate(t *testing.T) {
	for name, change := range map[string]func(*Manifest){
		"newer format": func(m *Manifest) { m.Format = 2 },
		"no version":   func(m *Manifest) { m.Version = "2be8807" },
		"dev version":  func(m *Manifest) { m.Version = "v0.2.0-3-gabc1234" },
		"no files":     func(m *Manifest) { m.Files = nil },
		"path in a name": func(m *Manifest) {
			m.Files = map[string]File{"../footops": {SHA256: strings.Repeat("ab", 32), Size: 1}}
		},
		"short hash": func(m *Manifest) { m.Files = map[string]File{"x": {SHA256: "ab", Size: 1}} },
		"too big": func(m *Manifest) {
			m.Files = map[string]File{"x": {SHA256: strings.Repeat("ab", 32), Size: MaxBinary + 1}}
		},
		"long notes": func(m *Manifest) { m.Notes = strings.Repeat("ç", MaxNotes) },
	} {
		m := sample()
		change(&m)
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := sample().Validate(); err != nil {
		t.Error(err)
	}
}

func TestVersions(t *testing.T) {
	for s, want := range map[string]Version{
		"v0.1.0":                   {0, 1, 0, 0, false},
		"v1.12.3":                  {1, 12, 3, 0, false},
		"v0.1.0-3-gabc1234":        {0, 1, 0, 3, false},
		"v0.1.0-12-g2be8807-dirty": {0, 1, 0, 12, true},
		"v0.1.0-dirty":             {0, 1, 0, 0, true},
	} {
		if v, ok := ParseVersion(s); !ok || v != want {
			t.Errorf("%s = %+v %v", s, v, ok)
		}
	}
	for _, s := range []string{"", "dev", "2be8807", "it-2be8807", "0.1.0", "v01.1.0", "v0.1", "v0.1.0-rc1", "v0.1.0-0-gabc1234"} {
		if _, ok := ParseVersion(s); ok {
			t.Errorf("%q parsed", s)
		}
	}
	for _, c := range []struct {
		release, running string
		want             bool
	}{
		{"v0.1.0", "2be8807", true}, // the first release over a commit's build
		{"v0.1.0", "it-2be8807", true},
		{"v0.1.0", "dev", true},
		{"v0.2.0", "v0.1.0", true},
		{"v0.1.1", "v0.1.0-5-gabc1234", true},
		{"v0.10.0", "v0.9.9", true},
		{"v0.1.0", "v0.1.0", false},
		{"v0.1.0", "v0.1.0-1-gabc1234", false}, // a build after it
		{"v0.1.0", "v0.2.0", false},
		{"v0.2.0-1-gabc1234", "v0.1.0", false}, // not a release
		{"bozuk", "v0.1.0", false},
	} {
		if got := Newer(c.release, c.running); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.release, c.running, got)
		}
	}
}

func TestFileCheckAndTrusted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "footops")
	if err := os.WriteFile(p, []byte("ikili"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := FileOf(p)
	if err != nil || f.Size != 5 {
		t.Fatal(f, err)
	}
	if err := f.Check(p); err != nil {
		t.Error(err)
	}
	if err := (File{SHA256: f.SHA256, Size: 6}).Check(p); err == nil {
		t.Error("size not checked")
	}
	if err := (File{SHA256: strings.Repeat("0", 64), Size: 5}).Check(p); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("hash not checked: %v", err)
	}

	pub, _, _ := ed25519.GenerateKey(nil)
	saved, savedTest := builtinKeys, testKey
	defer func() { builtinKeys, testKey = saved, savedTest }()
	builtinKeys, testKey = []string{base64.StdEncoding.EncodeToString(pub), "bozuk", ""}, ""
	if k := Trusted(); len(k) != 1 || !k[KeyID(pub)].Equal(pub) {
		t.Errorf("trusted = %v", k)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	testKey = base64.StdEncoding.EncodeToString(other)
	if k := Trusted(); len(k) != 2 {
		t.Errorf("with the test key: %v", k)
	}
}
