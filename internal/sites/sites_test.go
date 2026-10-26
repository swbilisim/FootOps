package sites

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validSpec() Spec {
	s := Spec{Name: "enisparta", Group: "musteri", Domains: []string{"enisparta.com", "www.enisparta.com"},
		Image: "ghcr.io/swbilisim/enisparta:main", Port: 3000, MemoryMB: 384, DataMount: "/app/uploads", Database: true}
	s.Normalize()
	return s
}

func TestValidate(t *testing.T) {
	if err := validSpec().Validate(map[string]string{"NODE_ENV": "production"}); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := map[string]func(*Spec, map[string]string){
		"bad name":         func(s *Spec, _ map[string]string) { s.Name = "En Isparta" },
		"reserved name":    func(s *Spec, _ map[string]string) { s.Name = "mongo" },
		"footops prefix":   func(s *Spec, _ map[string]string) { s.Name = "footops-x" },
		"unknown group":    func(s *Spec, _ map[string]string) { s.Group = "random" },
		"no domains":       func(s *Spec, _ map[string]string) { s.Domains = nil },
		"bad domain":       func(s *Spec, _ map[string]string) { s.Domains = []string{"localhost"} },
		"bad image":        func(s *Spec, _ map[string]string) { s.Image = "ghcr.io/x; rm -rf /" },
		"traversal image":  func(s *Spec, _ map[string]string) { s.Image = "a/../b" },
		"port":             func(s *Spec, _ map[string]string) { s.Port = 0 },
		"memory":           func(s *Spec, _ map[string]string) { s.MemoryMB = 10 },
		"relative mount":   func(s *Spec, _ map[string]string) { s.DataMount = "uploads" },
		"root mount":       func(s *Spec, _ map[string]string) { s.DataMount = "/" },
		"bad env key":      func(_ *Spec, e map[string]string) { e["1BAD"] = "x" },
		"newline in value": func(_ *Spec, e map[string]string) { e["A"] = "x\ny" },
		"db var collision": func(_ *Spec, e map[string]string) { e["MONGODB_URI"] = "mongodb://elsewhere" },
	}
	for name, mutate := range cases {
		s := validSpec()
		env := map[string]string{}
		mutate(&s, env)
		err := s.Validate(env)
		var ve *ValidationError
		if !errors.As(err, &ve) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNormalize(t *testing.T) {
	s := Spec{Name: " Enisparta ", Group: "MUSTERI", Domains: []string{"EnIsparta.com.", "enisparta.com", "", " www.enisparta.com "}, Database: true}
	s.Normalize()
	if s.Name != "enisparta" || s.Group != "musteri" || strings.Join(s.Domains, ",") != "enisparta.com,www.enisparta.com" || s.DBVar != "MONGODB_URI" {
		t.Errorf("normalized = %+v", s)
	}
}

func TestParseEnv(t *testing.T) {
	env, err := ParseEnv("# comment\nNODE_ENV=production\n\nSECRET=a=b=c\nQUOTED=\"kept\"\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if env["SECRET"] != "a=b=c" || env["QUOTED"] != `"kept"` || len(env) != 3 {
		t.Errorf("env = %v", env)
	}
	if _, err := ParseEnv("A=1\nA=2"); err == nil {
		t.Error("duplicate key accepted")
	}
	if _, err := ParseEnv("just text"); err == nil {
		t.Error("line without = accepted")
	}
}

func TestComposeEscapesAndHardens(t *testing.T) {
	s := validSpec()
	c := Compose(s, map[string]string{"PRICE": "costs $5", "QUOTE": `say "hi"`, "MONGODB_URI": "mongodb://u:p@mongo:27017/db"})
	for _, want := range []string{
		`image: "ghcr.io/swbilisim/enisparta:main"`,
		"container_name: enisparta",
		"mem_limit: 384m",
		"cap_drop: [ALL]",
		`"no-new-privileges:true"`,
		`PRICE: "costs $$5"`,
		`QUOTE: "say \"hi\""`,
		`- "/srv/musteri/enisparta/data:/app/uploads"`,
		"name: prj_musteri_enisparta",
		"external: true",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("compose misses %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "ports:") {
		t.Error("a site must never publish ports")
	}
}

func TestComposeIsDeterministic(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1", "C": "3"}
	if Compose(validSpec(), env) != Compose(validSpec(), env) {
		t.Error("map order leaked into the output")
	}
}

// selfSigned makes a certificate for names, valid until notAfter.
func selfSigned(t *testing.T, names []string, notAfter time.Time) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: names[0]},
		Issuer:       pkix.Name{CommonName: "CloudFlare Origin Certificate"},
		DNSNames:     names,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

func TestParseCertPair(t *testing.T) {
	now := time.Now()
	crt, key := selfSigned(t, []string{"enisparta.com", "*.enisparta.com"}, now.Add(365*24*time.Hour))
	c, err := ParseCertPair(crt, key, now)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{"enisparta.com": true, "www.enisparta.com": true, "a.b.enisparta.com": false, "other.com": false} {
		if c.Covers(host) != want {
			t.Errorf("Covers(%s) = %v", host, !want)
		}
	}
	_, otherKey := selfSigned(t, []string{"x.com"}, now.Add(time.Hour*24*365))
	if _, err := ParseCertPair(crt, otherKey, now); err == nil {
		t.Error("mismatched key accepted")
	}
	expiring, ekey := selfSigned(t, []string{"x.com"}, now.Add(48*time.Hour))
	if _, err := ParseCertPair(expiring, ekey, now); err == nil {
		t.Error("certificate expiring in 2 days accepted")
	}
	if _, err := ParseCertPair("garbage", key, now); err == nil {
		t.Error("garbage accepted")
	}
}

func TestCertStoreAndCaddyBlock(t *testing.T) {
	st := Store{Root: t.TempDir()}
	now := time.Now()
	crt, key := selfSigned(t, []string{"enisparta.com", "*.enisparta.com"}, now.Add(365*24*time.Hour))
	c, _ := ParseCertPair(crt, key, now)
	kp, err := KeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InstallCert(c, kp); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(st.Root, c.HostKeyPath())); fi.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", fi.Mode().Perm())
	}
	certs, err := st.Certs()
	if err != nil || len(certs) != 1 || certs[0].ID != c.ID {
		t.Fatalf("certs = %+v, %v", certs, err)
	}
	s := validSpec()
	got, missing := CertFor(append(s.Domains, "other.com"), certs)
	if len(missing) != 1 || missing[0] != "other.com" {
		t.Errorf("missing = %v", missing)
	}
	block := CaddyBlock(s, got)
	for _, want := range []string{
		"https://enisparta.com, https://www.enisparta.com {",
		"tls /etc/caddy/certs/" + c.ID + ".crt /etc/caddy/certs/" + c.ID + ".key",
		"reverse_proxy enisparta:3000",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("caddy block misses %q:\n%s", want, block)
		}
	}
}

func TestStoreListAndConflicts(t *testing.T) {
	st := Store{Root: t.TempDir()}
	a := validSpec()
	if err := st.WriteMeta(a); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(st.Root, "/srv/musteri/handmade"), 0o755) // no footops.json
	list, err := st.List()
	if err != nil || len(list) != 1 || list[0].Name != "enisparta" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	b := validSpec()
	b.Name, b.Domains = "other", []string{"www.enisparta.com", "panel.example.com"}
	p, _ := st.Conflicts(b, "panel.example.com")
	if len(p) != 2 {
		t.Errorf("conflicts = %v", p)
	}
	p, _ = st.Conflicts(a)
	if len(p) != 1 || !strings.Contains(p[0], "zaten var") {
		t.Errorf("same name: %v", p)
	}
	if _, err := st.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
}

func TestParseComposeEnvRoundTrip(t *testing.T) {
	env := map[string]string{
		"PLAIN": "production", "DOLLAR": "pa$$w0rd$HOME", "QUOTES": `a "quoted" 'value' \ back`,
		"UNICODE": "şifre ğüıöç ✓", "EMPTY": "", "URI": "mongodb://u:p@mongo:27017/db?authSource=db",
	}
	s := Spec{Name: "web", Group: "musteri", Image: "x/y:1", Port: 3000, MemoryMB: 256, DataMount: "/data"}
	got, err := ParseComposeEnv(Compose(s, env))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(env) {
		t.Fatalf("got %d vars, want %d: %v", len(got), len(env), got)
	}
	for k, v := range env {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if got, err := ParseComposeEnv(Compose(s, nil)); err != nil || len(got) != 0 {
		t.Errorf("no environment: %v, %v", got, err)
	}
	broken := strings.Replace(Compose(s, env), `PLAIN: "production"`, `PLAIN: production`, 1)
	if _, err := ParseComposeEnv(broken); err == nil {
		t.Error("hand-edited compose file accepted")
	}
}
