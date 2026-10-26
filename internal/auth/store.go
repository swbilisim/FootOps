package auth

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Account is the single panel administrator.
type Account struct {
	Username      string    `json:"username"`
	PasswordHash  string    `json:"password_hash"`
	TOTPSecret    string    `json:"totp_secret"`
	LastTOTPStep  uint64    `json:"last_totp_step"`
	RecoveryCodes []string  `json:"recovery_codes"` // hashes of unused codes
	FailedLogins  int       `json:"failed_logins"`
	LockedUntil   time.Time `json:"locked_until,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Lockout policy: after maxFailures consecutive failed logins the account is locked for
// lockDuration, doubling for each further failure (capped). This bounds online guessing
// even from many IP addresses.
const (
	maxFailures     = 5
	lockDuration    = 5 * time.Minute
	maxLockDuration = 2 * time.Hour
)

// Store persists the account in a JSON file with atomic writes.
type Store struct {
	path string
	mu   sync.Mutex
}

func NewStore(path string) *Store { return &Store{path: path} }

var ErrNoAccount = errors.New("panel hesabı yok; sunucuda 'sudo footops reset-password' çalıştır")

func (s *Store) Load() (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() (Account, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return Account{}, ErrNoAccount
	}
	if err != nil {
		return Account{}, err
	}
	var a Account
	if err := json.Unmarshal(b, &a); err != nil {
		return Account{}, err
	}
	return a, nil
}

// Save writes the account atomically with mode 0600.
func (s *Store) Save(a Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(a)
}

func (s *Store) save(a Account) error {
	a.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".auth-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// LoginResult explains a login outcome. Callers show the same message for every
// failure, so the reason is for the audit log only.
type LoginResult struct {
	OK           bool
	Reason       string // audit only: "bad-credentials", "bad-second-factor", "locked"
	UsedRecovery bool
	LockedUntil  time.Time
}

// Login verifies username, password and either a TOTP code or a recovery code, and
// records the outcome (failure counter, lockout, used TOTP step, consumed recovery code).
// The whole check runs under the store lock, so concurrent attempts cannot reuse a code.
func (s *Store) Login(username, password, secondFactor string, now time.Time) (LoginResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.load()
	if err != nil {
		return LoginResult{}, err
	}
	if now.Before(a.LockedUntil) {
		return LoginResult{Reason: "locked", LockedUntil: a.LockedUntil}, nil
	}

	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.Username)) == 1
	// The password is always hashed, even for a wrong username, so timing does not
	// reveal whether the username exists.
	passOK, err := VerifyPassword(a.PasswordHash, password)
	if err != nil {
		return LoginResult{}, err
	}
	res := LoginResult{}
	switch {
	case !userOK || !passOK:
		res.Reason = "bad-credentials"
	default:
		if step, ok := VerifyTOTP(a.TOTPSecret, secondFactor, now, a.LastTOTPStep); ok {
			a.LastTOTPStep = step
			res.OK = true
		} else if i := a.recoveryIndex(secondFactor); i >= 0 {
			a.RecoveryCodes = append(a.RecoveryCodes[:i], a.RecoveryCodes[i+1:]...)
			res.OK, res.UsedRecovery = true, true
		} else {
			res.Reason = "bad-second-factor"
		}
	}

	if res.OK {
		a.FailedLogins = 0
		a.LockedUntil = time.Time{}
	} else {
		a.FailedLogins++
		if a.FailedLogins >= maxFailures {
			d := lockDuration << (a.FailedLogins - maxFailures)
			if d > maxLockDuration || d <= 0 {
				d = maxLockDuration
			}
			a.LockedUntil = now.Add(d)
			res.LockedUntil = a.LockedUntil
		}
	}
	if err := s.save(a); err != nil {
		return LoginResult{}, err
	}
	return res, nil
}

func (a Account) recoveryIndex(code string) int {
	if len(code) < 10 {
		return -1
	}
	h := HashRecoveryCode(code)
	for i, c := range a.RecoveryCodes {
		if subtle.ConstantTimeCompare([]byte(c), []byte(h)) == 1 {
			return i
		}
	}
	return -1
}
