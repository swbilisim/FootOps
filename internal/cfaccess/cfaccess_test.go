package cfaccess

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const aud = "4714c1358e65fe4b408ad6d432a5f878f08194bdb18ccad9b2a8bfa3a0ca4e5d"

type testIdP struct {
	key     *rsa.PrivateKey
	kid     string
	fetches atomic.Int32
	srv     *httptest.Server
}

func newIdP(t *testing.T) *testIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIdP{key: k, kid: "kid-1"}
	idp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp.fetches.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": idp.kid, "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		}}})
	}))
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *testIdP) sign(t *testing.T, hdr, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signing := enc(hdr) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

var now = time.Unix(1_800_000_000, 0)

func goodClaims() map[string]any {
	return map[string]any{
		"aud": []string{aud}, "email": "furkan@example.com", "iss": "https://swbilisim.cloudflareaccess.com",
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nbf": now.Unix(), "sub": "u1",
	}
}

func verifier(idp *testIdP) *Verifier {
	v := New("swbilisim", aud)
	v.certsURL = idp.srv.URL
	v.now = func() time.Time { return now }
	return v
}

func TestVerifyValid(t *testing.T) {
	idp := newIdP(t)
	v := verifier(idp)
	tok := idp.sign(t, map[string]any{"alg": "RS256", "kid": "kid-1"}, goodClaims())
	c, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Email != "furkan@example.com" {
		t.Errorf("email = %q", c.Email)
	}
	// Keys are cached.
	_, _ = v.Verify(context.Background(), tok)
	if idp.fetches.Load() != 1 {
		t.Errorf("fetches = %d", idp.fetches.Load())
	}
}

func TestVerifyRejects(t *testing.T) {
	idp := newIdP(t)
	hdr := map[string]any{"alg": "RS256", "kid": "kid-1"}
	mut := func(f func(map[string]any)) map[string]any {
		c := goodClaims()
		f(c)
		return c
	}
	cases := map[string]string{
		"wrong audience": idp.sign(t, hdr, mut(func(c map[string]any) { c["aud"] = []string{"other"} })),
		"wrong issuer":   idp.sign(t, hdr, mut(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })),
		"expired":        idp.sign(t, hdr, mut(func(c map[string]any) { c["exp"] = now.Add(-2 * time.Minute).Unix() })),
		"no expiry":      idp.sign(t, hdr, mut(func(c map[string]any) { delete(c, "exp") })),
		"not yet valid":  idp.sign(t, hdr, mut(func(c map[string]any) { c["nbf"] = now.Add(5 * time.Minute).Unix() })),
		"alg none":       idp.sign(t, map[string]any{"alg": "none", "kid": "kid-1"}, goodClaims()),
		"alg HS256":      idp.sign(t, map[string]any{"alg": "HS256", "kid": "kid-1"}, goodClaims()),
		"garbage":        "a.b",
	}
	good := idp.sign(t, hdr, goodClaims())
	p := strings.Split(good, ".")
	cases["tampered payload"] = p[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"aud":["`+aud+`"],"email":"attacker@x","iss":"https://swbilisim.cloudflareaccess.com","exp":9999999999}`)) + "." + p[2]

	for name, tok := range cases {
		if _, err := verifier(idp).Verify(context.Background(), tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := verifier(idp).Verify(context.Background(), ""); !errors.Is(err, ErrMissing) {
		t.Errorf("empty token: %v", err)
	}
}

func TestStringAudience(t *testing.T) {
	idp := newIdP(t)
	c := goodClaims()
	c["aud"] = aud
	tok := idp.sign(t, map[string]any{"alg": "RS256", "kid": "kid-1"}, c)
	if _, err := verifier(idp).Verify(context.Background(), tok); err != nil {
		t.Errorf("string aud rejected: %v", err)
	}
}

func TestUnknownKidRefetchIsRateLimited(t *testing.T) {
	idp := newIdP(t)
	v := verifier(idp)
	good := idp.sign(t, map[string]any{"alg": "RS256", "kid": "kid-1"}, goodClaims())
	if _, err := v.Verify(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		forged := idp.sign(t, map[string]any{"alg": "RS256", "kid": "random-" + string(rune('a'+i))}, goodClaims())
		_, _ = v.Verify(context.Background(), forged)
	}
	if idp.fetches.Load() > 1 {
		t.Errorf("forged kids caused %d fetches", idp.fetches.Load())
	}
	// After the limit passes, a rotated key is picked up.
	idp.kid = "kid-2"
	now = now.Add(2 * time.Minute)
	defer func() { now = now.Add(-2 * time.Minute) }()
	rotated := idp.sign(t, map[string]any{"alg": "RS256", "kid": "kid-2"}, goodClaims())
	if _, err := v.Verify(context.Background(), rotated); err != nil {
		t.Errorf("rotated key: %v", err)
	}
}

func TestTeamDomainForms(t *testing.T) {
	for _, team := range []string{"swbilisim", "swbilisim.cloudflareaccess.com", "https://swbilisim.cloudflareaccess.com/"} {
		v := New(team, aud)
		if v.issuer != "https://swbilisim.cloudflareaccess.com" || v.certsURL != "https://swbilisim.cloudflareaccess.com/cdn-cgi/access/certs" {
			t.Errorf("%s -> %s %s", team, v.issuer, v.certsURL)
		}
	}
}
