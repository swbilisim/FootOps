package mac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Settings is ~/Library/Application Support/FootOps/ayarlar.json (0600): what the Mac
// knows of each server beyond the Keychain. No secrets; the addresses in it stay on the
// Mac (the public repository holds no infrastructure). FootOps.app's Settings window
// writes it (M2); a missing file is no settings at all.
type Settings struct {
	Format  int                       `json:"format"`
	Servers map[string]ServerSettings `json:"servers,omitempty"` // by code: "h1"
}

// ServerSettings is one server's.
type ServerSettings struct {
	// Panel is opened in the browser (the app's "Panel" item), SSH in the Terminal.
	Panel string `json:"panel,omitempty"`
	SSH   string `json:"ssh,omitempty"`
	// Bucket is where its backups are, with any folder below (footops-h1, kova/klasor);
	// default the Keychain's kova-<code>, else footops-<code>.
	Bucket string `json:"bucket,omitempty"`
}

const settingsFormat = 1

// readSettings reads the file; none is no settings.
func readSettings(path string) (Settings, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Settings{Format: settingsFormat}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("ayarlar dosyası okunamadı: %v", err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("ayarlar dosyası anlaşılamadı (%s): %v", path, err)
	}
	if s.Format != settingsFormat {
		return Settings{}, fmt.Errorf("ayarlar dosyasının biçimi tanınmadı (%d): bu footops-mac daha eski olabilir", s.Format)
	}
	for code, srv := range s.Servers {
		if !reCode.MatchString(code) {
			return Settings{}, fmt.Errorf("ayarlar dosyasında geçersiz sunucu kodu: %q", code)
		}
		if srv.Bucket != "" {
			if err := checkBucket(srv.Bucket); err != nil {
				return Settings{}, fmt.Errorf("ayarlar dosyası, %s: %v", code, err)
			}
		}
	}
	return s, nil
}

var (
	// reCode is a server's code as the Keychain's names carry it (footops-yedek's rule).
	reCode = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// The account is R2's: 32 hex digits. Buckets and folders as the agent takes them
	// (internal/agent, checkRepository).
	reAccount = regexp.MustCompile(`^[0-9a-f]{32}$`)
	reBucket  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	rePrefix  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// checkBucket accepts a bucket with up to three folders below it.
func checkBucket(b string) error {
	parts := strings.Split(b, "/")
	if !reBucket.MatchString(parts[0]) {
		return fmt.Errorf("kova adı geçersiz: %q", b)
	}
	for _, p := range parts[1:] {
		if !rePrefix.MatchString(p) || p == "." || p == ".." {
			return fmt.Errorf("kovadaki klasör adı geçersiz: %q", b)
		}
	}
	if len(parts) > 4 {
		return fmt.Errorf("kova altında en çok üç klasör: %q", b)
	}
	return nil
}

// server is one server as the Mac knows it.
type server struct {
	Code   string
	Bucket string
	Panel  string
	SSH    string
}

// access is what reading the servers' buckets takes: the account and the Mac's
// read-only key, from the Keychain.
type access struct {
	Account, KeyID, Secret string
}

// servers lists the servers — those with a repository password in the Keychain
// (restic-<code>, as footops-yedek finds them) and those in the settings — and reads
// the key that reads their buckets.
func (e *env) servers(ctx context.Context) ([]server, access, error) {
	set, err := readSettings(e.settings)
	if err != nil {
		return nil, access{}, err
	}
	names, err := e.keychain.Names(ctx)
	if err != nil {
		return nil, access{}, err
	}
	has := map[string]bool{}
	codes := map[string]bool{}
	for _, n := range names {
		has[n] = true
		if c, ok := strings.CutPrefix(n, "restic-"); ok && reCode.MatchString(c) {
			codes[c] = true
		}
	}
	for c := range set.Servers {
		codes[c] = true
	}
	if len(codes) == 0 {
		return nil, access{}, errors.New("sunucu yok: Anahtar Zinciri'nde footops-yedek / restic-<sunucu> kaydı da ayarlar dosyasında sunucu da yok")
	}
	var acc access
	for _, it := range []struct {
		name, what string
		to         *string
	}{{"r2-hesap", "R2 hesap kimliği", &acc.Account}, {"r2-mac-erisim", "salt okunur anahtar", &acc.KeyID},
		{"r2-mac-gizli", "salt okunur anahtarın gizlisi", &acc.Secret}} {
		v, err := e.keychain.Get(ctx, it.name)
		if errors.Is(err, errNoItem) {
			return nil, access{}, fmt.Errorf("Anahtar Zinciri'nde footops-yedek / %s yok (%s)", it.name, it.what)
		}
		if err != nil {
			return nil, access{}, err
		}
		*it.to = strings.TrimSpace(v)
	}
	if !reAccount.MatchString(acc.Account) {
		return nil, access{}, errors.New("Anahtar Zinciri'ndeki r2-hesap bir R2 hesap kimliğine benzemiyor (32 onaltılık rakam)")
	}
	var out []server
	for c := range codes {
		s := server{Code: c, Bucket: "footops-" + c}
		if has["kova-"+c] {
			b, err := e.keychain.Get(ctx, "kova-"+c)
			if err != nil {
				return nil, access{}, err
			}
			s.Bucket = strings.Trim(strings.TrimSpace(b), "/")
		}
		if st, ok := set.Servers[c]; ok {
			s.Panel, s.SSH = st.Panel, st.SSH
			if st.Bucket != "" {
				s.Bucket = strings.Trim(st.Bucket, "/")
			}
		}
		if err := checkBucket(s.Bucket); err != nil {
			return nil, access{}, fmt.Errorf("%s: %v", c, err)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, acc, nil
}
