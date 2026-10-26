// Package cfaccess verifies Cloudflare Access JWTs (the Cf-Access-Jwt-Assertion header).
//
// Cloudflare Access already blocks unauthenticated requests before they reach the
// tunnel. The panel verifies the token anyway, so that a misconfigured or deleted Access
// application does not silently expose the login page to the internet.
package cfaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	Header       = "Cf-Access-Jwt-Assertion"
	clockSkew    = time.Minute
	keysMaxAge   = time.Hour
	refetchLimit = time.Minute
)

// Claims are the fields the panel uses.
type Claims struct {
	Email   string   `json:"email"`
	Subject string   `json:"sub"`
	Issuer  string   `json:"iss"`
	Aud     []string `json:"aud"`
	Expiry  int64    `json:"exp"`
	NotBef  int64    `json:"nbf"`
	Issued  int64    `json:"iat"`
	Country string   `json:"country"`
}

// Verifier checks tokens for one Access application.
type Verifier struct {
	issuer   string // https://<team>.cloudflareaccess.com
	aud      string
	certsURL string
	http     *http.Client
	now      func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	triedAt   time.Time
}

// New returns a verifier for team (e.g. "swbilisim" or "swbilisim.cloudflareaccess.com")
// and the application's AUD tag.
func New(team, aud string) *Verifier {
	host := strings.TrimSuffix(strings.TrimPrefix(team, "https://"), "/")
	if !strings.Contains(host, ".") {
		host += ".cloudflareaccess.com"
	}
	return &Verifier{
		issuer:   "https://" + host,
		aud:      aud,
		certsURL: "https://" + host + "/cdn-cgi/access/certs",
		http:     &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
	}
}

var (
	ErrMissing = errors.New("Cloudflare Access token yok")
	ErrInvalid = errors.New("Cloudflare Access token geçersiz")
)

// Verify validates signature (RS256 only), issuer, audience and time claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	if token == "" {
		return Claims{}, ErrMissing
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, fmt.Errorf("%w: biçim", ErrInvalid)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return Claims{}, fmt.Errorf("%w: başlık", ErrInvalid)
	}
	// Only RS256: never let the token choose "none" or an HMAC algorithm.
	if hdr.Alg != "RS256" {
		return Claims{}, fmt.Errorf("%w: algoritma %q", ErrInvalid, hdr.Alg)
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return Claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Claims{}, fmt.Errorf("%w: imza", ErrInvalid)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return Claims{}, fmt.Errorf("%w: imza doğrulanmadı", ErrInvalid)
	}

	var c Claims
	if err := decodeClaims(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("%w: içerik", ErrInvalid)
	}
	now := v.now()
	switch {
	case c.Issuer != v.issuer:
		return Claims{}, fmt.Errorf("%w: issuer %q", ErrInvalid, c.Issuer)
	case !contains(c.Aud, v.aud):
		return Claims{}, fmt.Errorf("%w: audience", ErrInvalid)
	case c.Expiry == 0 || now.After(time.Unix(c.Expiry, 0).Add(clockSkew)):
		return Claims{}, fmt.Errorf("%w: süresi dolmuş", ErrInvalid)
	case c.NotBef != 0 && now.Add(clockSkew).Before(time.Unix(c.NotBef, 0)):
		return Claims{}, fmt.Errorf("%w: henüz geçerli değil", ErrInvalid)
	}
	return c, nil
}

// decodeClaims accepts "aud" as a string or an array, as the JWT spec allows.
func decodeClaims(seg string, c *Claims) error {
	var raw map[string]json.RawMessage
	if err := decodeSegment(seg, &raw); err != nil {
		return err
	}
	if a, ok := raw["aud"]; ok && len(a) > 0 && a[0] == '"' {
		var s string
		if err := json.Unmarshal(a, &s); err != nil {
			return err
		}
		b, _ := json.Marshal([]string{s})
		raw["aud"] = b
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, c)
}

func decodeSegment(s string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// key returns the public key for kid, refreshing the key set when it is old or the kid
// is unknown (Cloudflare rotates keys). Refreshes are rate-limited so random kids in
// forged tokens cannot make the panel hammer Cloudflare.
func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	k, ok := v.keys[kid]
	fresh := now.Sub(v.fetchedAt) < keysMaxAge
	if ok && fresh {
		return k, nil
	}
	if now.Sub(v.triedAt) >= refetchLimit || v.keys == nil {
		v.triedAt = now
		keys, err := v.fetch(ctx)
		if err != nil {
			if ok { // keep using a known key if Cloudflare is briefly unreachable
				return k, nil
			}
			return nil, fmt.Errorf("Access anahtarları alınamadı: %w", err)
		}
		v.keys, v.fetchedAt = keys, now
		if k, ok = keys[kid]; ok {
			return k, nil
		}
	}
	if ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: bilinmeyen anahtar", ErrInvalid)
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.certsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", v.certsURL, resp.Status)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(e) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("anahtar kümesinde RSA anahtarı yok")
	}
	return keys, nil
}
