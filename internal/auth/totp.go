// Package auth holds the panel's credentials: argon2id password hashes, TOTP second
// factor (RFC 6238) and single-use recovery codes.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	totpPeriod = 30 * time.Second
	totpDigits = 6
	// totpSkew accepts the previous and next code as well, for clock drift between the
	// server and the phone.
	totpSkew = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a random 160-bit secret, base32 encoded (the format
// authenticator apps expect).
func NewTOTPSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// TOTPURI is the otpauth:// URI shown as a QR code.
func TOTPURI(secret, issuer, account string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(int(totpPeriod.Seconds())))
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// TOTPCode returns the code for the time step containing t.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("totp secret: %w", err)
	}
	return hotp(key, uint64(t.Unix())/uint64(totpPeriod.Seconds())), nil
}

// VerifyTOTP checks code against the steps around now. It returns the matched step so
// the caller can reject its reuse (a code is valid for up to 90 seconds otherwise).
// Steps at or below lastStep are refused.
func VerifyTOTP(secret, code string, now time.Time, lastStep uint64) (step uint64, ok bool) {
	key, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := uint64(now.Unix()) / uint64(totpPeriod.Seconds())
	for d := -totpSkew; d <= totpSkew; d++ {
		s := cur + uint64(d)
		if s <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(hotp(key, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}

// hotp is RFC 4226 with SHA-1 and 6 digits.
func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, v%1_000_000)
}
