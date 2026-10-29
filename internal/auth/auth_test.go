package auth

import (
	"encoding/base32"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B test vectors (SHA-1, 8 digits there; we compare the last 6).
func TestTOTPRFCVectors(t *testing.T) {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))
	vectors := map[int64]string{
		59:          "94287082",
		1111111109:  "07081804",
		1111111111:  "14050471",
		1234567890:  "89005924",
		2000000000:  "69279037",
		20000000000: "65353130",
	}
	for unix, want8 := range vectors {
		got, err := TOTPCode(secret, time.Unix(unix, 0))
		if err != nil {
			t.Fatal(err)
		}
		if got != want8[2:] {
			t.Errorf("t=%d: got %s, want %s", unix, got, want8[2:])
		}
	}
}

func TestVerifyTOTPWindowAndReplay(t *testing.T) {
	secret, err := NewTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(secret, now)
	step, ok := VerifyTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Error("replayed code accepted")
	}
	prev, _ := TOTPCode(secret, now.Add(-30*time.Second))
	if _, ok := VerifyTOTP(secret, prev, now, 0); !ok {
		t.Error("previous step (clock drift) rejected")
	}
	old, _ := TOTPCode(secret, now.Add(-90*time.Second))
	if _, ok := VerifyTOTP(secret, old, now, 0); ok {
		t.Error("code from 3 steps ago accepted")
	}
	if _, ok := VerifyTOTP(secret, "12345", now, 0); ok {
		t.Error("short code accepted")
	}
	spaced := code[:3] + " " + code[3:]
	if _, ok := VerifyTOTP(secret, spaced, now, 0); !ok {
		t.Error("code typed with a space rejected")
	}
}

func TestTOTPURI(t *testing.T) {
	u := TOTPURI("ABC", "FootOps H1", "furkan")
	if !strings.HasPrefix(u, "otpauth://totp/FootOps%20H1:furkan?") || !strings.Contains(u, "secret=ABC") {
		t.Errorf("uri = %s", u)
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("hash = %s", h)
	}
	if ok, err := VerifyPassword(h, "correct horse"); !ok || err != nil {
		t.Errorf("right password: %v %v", ok, err)
	}
	if ok, _ := VerifyPassword(h, "wrong horse"); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse")
	if h == h2 {
		t.Error("salt not random")
	}
	tampered := strings.Replace(h, "m=65536", "m=99999999", 1)
	if _, err := VerifyPassword(tampered, "correct horse"); err == nil {
		t.Error("absurd memory parameter accepted")
	}
}

// After a login check, argon2id's 64 MiB is back with the operating system instead of
// staying with the Go runtime: the panel idles at ~20 MB.
func TestVerifyPasswordReturnsMemory(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword(h, "correct horse"); !ok || err != nil {
		t.Fatalf("right password: %v %v", ok, err)
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if held := m.HeapSys - m.HeapReleased; held > 32<<20 {
		t.Errorf("the heap still holds %d MB of the operating system's memory", held>>20)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, hashes, err := NewRecoveryCodes(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 10 || len(hashes) != 10 {
		t.Fatal("wrong count")
	}
	if len(codes[0]) != 11 || codes[0][5] != '-' {
		t.Errorf("code format %q", codes[0])
	}
	if HashRecoveryCode(strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))) != hashes[0] {
		t.Error("normalisation differs")
	}
}

func newAccountStore(t *testing.T) (*Store, string, string, []string) {
	t.Helper()
	secret, _ := NewTOTPSecret()
	hash, _ := HashPassword("pw-123456789012")
	codes, hashes, _ := NewRecoveryCodes(3)
	s := NewStore(filepath.Join(t.TempDir(), "auth.json"))
	if err := s.Save(Account{Username: "furkan", PasswordHash: hash, TOTPSecret: secret, RecoveryCodes: hashes}); err != nil {
		t.Fatal(err)
	}
	return s, secret, "pw-123456789012", codes
}

func TestLoginFlow(t *testing.T) {
	s, secret, pw, codes := newAccountStore(t)
	now := time.Unix(1_800_000_000, 0)
	code, _ := TOTPCode(secret, now)

	if r, _ := s.Login("furkan", pw, code, now); !r.OK {
		t.Fatalf("valid login rejected: %+v", r)
	}
	if r, _ := s.Login("furkan", pw, code, now); r.OK {
		t.Error("same TOTP code accepted twice")
	}
	if r, _ := s.Login("other", pw, code, now); r.OK || r.Reason != "bad-credentials" {
		t.Errorf("wrong user: %+v", r)
	}
	r, _ := s.Login("furkan", pw, codes[1], now)
	if !r.OK || !r.UsedRecovery {
		t.Fatalf("recovery code rejected: %+v", r)
	}
	if r, _ := s.Login("furkan", pw, codes[1], now); r.OK {
		t.Error("recovery code used twice")
	}
	a, _ := s.Load()
	if len(a.RecoveryCodes) != 2 {
		t.Errorf("recovery codes left = %d", len(a.RecoveryCodes))
	}
	st, _ := os.Stat(s.path)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("auth file mode %v", st.Mode().Perm())
	}
}

func TestLoginLockout(t *testing.T) {
	s, secret, pw, _ := newAccountStore(t)
	now := time.Unix(1_800_000_000, 0)
	for i := 0; i < maxFailures; i++ {
		if r, _ := s.Login("furkan", "wrong", "000000", now); r.OK {
			t.Fatal("wrong password accepted")
		}
	}
	code, _ := TOTPCode(secret, now)
	if r, _ := s.Login("furkan", pw, code, now); r.OK || r.Reason != "locked" {
		t.Fatalf("locked account accepted correct credentials: %+v", r)
	}
	later := now.Add(lockDuration + time.Second)
	code, _ = TOTPCode(secret, later)
	if r, _ := s.Login("furkan", pw, code, later); !r.OK {
		t.Fatalf("not unlocked after lock duration: %+v", r)
	}
	if a, _ := s.Load(); a.FailedLogins != 0 {
		t.Error("failure counter not reset after success")
	}
}

func TestLoginWithoutAccount(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "missing.json"))
	if _, err := s.Login("a", "b", "c", time.Now()); err != ErrNoAccount {
		t.Errorf("err = %v", err)
	}
}
