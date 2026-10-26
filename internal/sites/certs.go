package sites

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// CertsDir is on the host; Caddy sees it as /etc/caddy/certs.
	CertsDir          = "/srv/core/caddy/etc/certs"
	caddyCertsDir     = "/etc/caddy/certs"
	minCertValidity   = 7 * 24 * time.Hour
	certExpiryWarning = 30 * 24 * time.Hour
)

// Cert is an installed TLS certificate (normally a Cloudflare Origin CA certificate).
type Cert struct {
	ID       string    `json:"id"` // first 16 hex chars of the certificate's SHA-256
	Names    []string  `json:"names"`
	Issuer   string    `json:"issuer"`
	NotAfter time.Time `json:"not_after"`
	leaf     *x509.Certificate
}

func (c Cert) CertPath() string { return caddyCertsDir + "/" + c.ID + ".crt" }
func (c Cert) KeyPath() string  { return caddyCertsDir + "/" + c.ID + ".key" }

// HostCertPath and HostKeyPath are where the files live on the host.
func (c Cert) HostCertPath() string { return CertsDir + "/" + c.ID + ".crt" }
func (c Cert) HostKeyPath() string  { return CertsDir + "/" + c.ID + ".key" }

// ExpiresSoon reports whether the certificate needs replacing within 30 days.
func (c Cert) ExpiresSoon(now time.Time) bool { return now.Add(certExpiryWarning).After(c.NotAfter) }

// Covers reports whether the certificate is valid for host (exact name or one-level wildcard).
func (c Cert) Covers(host string) bool {
	if c.leaf != nil {
		return c.leaf.VerifyHostname(host) == nil
	}
	for _, n := range c.Names {
		if n == host || (strings.HasPrefix(n, "*.") && strings.Count(host, ".") == strings.Count(n, ".") &&
			strings.HasSuffix(host, n[1:])) {
			return true
		}
	}
	return false
}

// ParseCertPair validates a PEM certificate and private key pasted by the operator:
// the key must match the certificate and the certificate must be valid for at least a
// week. Only the leaf certificate is used: Cloudflare is the only client and trusts its
// Origin CA directly.
func ParseCertPair(certPEM, keyPEM string, now time.Time) (Cert, error) {
	leaf, err := firstCert([]byte(certPEM))
	if err != nil {
		return Cert{}, err
	}
	key, err := parseKey([]byte(keyPEM))
	if err != nil {
		return Cert{}, err
	}
	if !publicKeysMatch(leaf.PublicKey, key.Public()) {
		return Cert{}, errors.New("özel anahtar bu sertifikaya ait değil")
	}
	if now.Before(leaf.NotBefore) {
		return Cert{}, errors.New("sertifika henüz geçerli değil")
	}
	if now.Add(minCertValidity).After(leaf.NotAfter) {
		return Cert{}, fmt.Errorf("sertifikanın süresi dolmuş ya da bir haftadan az kalmış (%s)", leaf.NotAfter.Format("2006-01-02"))
	}
	if len(leaf.DNSNames) == 0 {
		return Cert{}, errors.New("sertifikada alan adı (SAN) yok")
	}
	return newCert(leaf), nil
}

// LoadCert parses an installed certificate file.
func LoadCert(certPEM []byte) (Cert, error) {
	leaf, err := firstCert(certPEM)
	if err != nil {
		return Cert{}, err
	}
	return newCert(leaf), nil
}

func newCert(leaf *x509.Certificate) Cert {
	sum := sha256.Sum256(leaf.Raw)
	names := append([]string(nil), leaf.DNSNames...)
	sort.Strings(names)
	return Cert{
		ID:       hex.EncodeToString(sum[:8]),
		Names:    names,
		Issuer:   leaf.Issuer.CommonName + strings.Join(leaf.Issuer.Organization, " "),
		NotAfter: leaf.NotAfter,
		leaf:     leaf,
	}
}

// PEM returns the leaf certificate in PEM form.
func (c Cert) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.leaf.Raw})
}

func firstCert(b []byte) (*x509.Certificate, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("PEM sertifika bulunamadı (-----BEGIN CERTIFICATE----- ile başlamalı)")
		}
		if blk.Type == "CERTIFICATE" {
			return x509.ParseCertificate(blk.Bytes)
		}
	}
}

func parseKey(b []byte) (crypto.Signer, error) {
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			return nil, errors.New("PEM özel anahtar bulunamadı (-----BEGIN PRIVATE KEY----- ile başlamalı)")
		}
		switch blk.Type {
		case "PRIVATE KEY":
			k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
			if err != nil {
				return nil, err
			}
			s, ok := k.(crypto.Signer)
			if !ok {
				return nil, errors.New("desteklenmeyen anahtar türü")
			}
			return s, nil
		case "RSA PRIVATE KEY":
			return x509.ParsePKCS1PrivateKey(blk.Bytes)
		case "EC PRIVATE KEY":
			return x509.ParseECPrivateKey(blk.Bytes)
		}
	}
}

func publicKeysMatch(a, b crypto.PublicKey) bool {
	switch k := a.(type) {
	case *rsa.PublicKey:
		return k.Equal(b)
	case *ecdsa.PublicKey:
		return k.Equal(b)
	case ed25519.PublicKey:
		return k.Equal(b)
	}
	return false
}

// KeyPEM re-encodes the private key as PKCS#8 so stored keys have one format.
func KeyPEM(keyPEM string) ([]byte, error) {
	k, err := parseKey([]byte(keyPEM))
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// CertFor picks, for each domain, the certificate that covers it and expires last.
// Domains without a covering certificate are returned in missing.
func CertFor(domains []string, certs []Cert) (map[string]Cert, []string) {
	out := map[string]Cert{}
	var missing []string
	for _, d := range domains {
		var best *Cert
		for i := range certs {
			if certs[i].Covers(d) && (best == nil || certs[i].NotAfter.After(best.NotAfter)) {
				best = &certs[i]
			}
		}
		if best == nil {
			missing = append(missing, d)
			continue
		}
		out[d] = *best
	}
	return out, missing
}
