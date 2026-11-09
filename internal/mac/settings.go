package mac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Settings is ~/Library/Application Support/FootOps/ayarlar.json (0600): what the Mac
// knows of each server beyond the Keychain. No secrets; the addresses in it stay on the
// Mac (the public repository holds no infrastructure). FootOps.app's Settings window
// writes it through footops-mac ayar yaz, so the rules stay here; a missing file is no
// settings at all.
type Settings struct {
	Format  int                       `json:"format"`
	Servers map[string]ServerSettings `json:"servers,omitempty"` // by code: "h1"
}

// ServerSettings is one server's.
type ServerSettings struct {
	// Panel is opened in the browser (the app's "Panel" item), SSH in the Terminal as
	// ssh://<SSH>.
	Panel string `json:"panel,omitempty"`
	SSH   string `json:"ssh,omitempty"`
	// Bucket is where its backups are, with any folder below (footops-h1, kova/klasor);
	// default the Keychain's kova-<code>, else footops-<code>.
	Bucket string `json:"bucket,omitempty"`
}

const settingsFormat = 1

// readSettings reads the file; none is no settings. What would read the wrong place
// stops it (a server's code, a bucket); a panel or SSH target not to the rules does not
// — usable leaves it out and fieldProblems names it — so a typo in one hides no server.
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
	if err := checkPlaces(s); err != nil {
		return Settings{}, fmt.Errorf("ayarlar dosyası: %v", err)
	}
	return s, nil
}

// checkSettings holds settings about to be written to every rule: a panel the app opens
// in the browser is a web address, an SSH target is no option in disguise.
func checkSettings(s Settings) error {
	if err := checkPlaces(s); err != nil {
		return err
	}
	if p := fieldProblems(s); len(p) > 0 {
		return errors.New(p[0])
	}
	return nil
}

// checkPlaces holds the servers' codes and buckets to their rules.
func checkPlaces(s Settings) error {
	for code, srv := range s.Servers {
		if !reCode.MatchString(code) {
			return fmt.Errorf("geçersiz sunucu kodu: %q", code)
		}
		if srv.Bucket != "" {
			if err := checkBucket(srv.Bucket); err != nil {
				return fmt.Errorf("%s: %v", code, err)
			}
		}
	}
	return nil
}

// fieldProblems names the panels and SSH targets not to the rules, a server at a time.
func fieldProblems(s Settings) []string {
	codes := make([]string, 0, len(s.Servers))
	for c := range s.Servers {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	var out []string
	for _, c := range codes {
		srv := s.Servers[c]
		if srv.Panel != "" {
			if err := checkPanel(srv.Panel); err != nil {
				out = append(out, fmt.Sprintf("%s: %v", c, err))
			}
		}
		if srv.SSH != "" {
			if err := checkSSH(srv.SSH); err != nil {
				out = append(out, fmt.Sprintf("%s: %v", c, err))
			}
		}
	}
	return out
}

// usable is a server's settings with what is not to the rules left out: nothing the app
// opens comes of a file edited by hand.
func (srv ServerSettings) usable() ServerSettings {
	if srv.Panel != "" && checkPanel(srv.Panel) != nil {
		srv.Panel = ""
	}
	if srv.SSH != "" && checkSSH(srv.SSH) != nil {
		srv.SSH = ""
	}
	return srv
}

// checkPanel takes a web address of a host: http too (a panel reached through an SSH
// tunnel, http://localhost:7701), no name and password in it.
func checkPanel(p string) error {
	u, err := url.Parse(p)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Hostname() == "" || u.User != nil ||
		len(p) > 2048 || strings.ContainsFunc(p, unicode.IsSpace) {
		return fmt.Errorf("panel adresi geçersiz: %q (https://… biçiminde olmalı)", clip(p, 80))
	}
	return nil
}

// reSSH is [user@]host[:port]: the user and the host start with a letter or a digit, so
// neither is read by ssh as an option.
var reSSH = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]{0,63}@)?[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?(?::([0-9]{1,5}))?$`)

func checkSSH(s string) error {
	m := reSSH.FindStringSubmatch(s)
	if m == nil {
		return fmt.Errorf("SSH hedefi geçersiz: %q (kullanıcı@sunucu biçiminde olmalı)", clip(s, 80))
	}
	if m[1] != "" {
		if port, err := strconv.Atoi(m[1]); err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("SSH hedefinin portu geçersiz: %q", clip(s, 80))
		}
	}
	return nil
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

// server is one server as the Mac knows it; Password: its repository's password is in
// the Keychain (restic-<code>), so its backups can be opened.
type server struct {
	Code     string
	Bucket   string
	Panel    string
	SSH      string
	Password bool
}

// access is what reading the servers' buckets takes: the account and the Mac's
// read-only key, from the Keychain.
type access struct {
	Account, KeyID, Secret string
}

// known is what the Mac knows of its servers without reading a secret: the settings,
// the Keychain's item names, and the codes of both.
func (e *env) known(ctx context.Context) (Settings, map[string]bool, []string, error) {
	set, err := readSettings(e.settings)
	if err != nil {
		return Settings{}, nil, nil, err
	}
	names, err := e.keychain.Names(ctx)
	if err != nil {
		return Settings{}, nil, nil, err
	}
	has, codes := serverCodes(names, set)
	return set, has, codes, nil
}

// serverCodes are the servers of the Keychain's items — those with a repository
// password (restic-<code>, as footops-yedek finds them) — and of the settings, sorted;
// has tells the items there are.
func serverCodes(names []string, set Settings) (map[string]bool, []string) {
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
	list := make([]string, 0, len(codes))
	for c := range codes {
		list = append(list, c)
	}
	sort.Strings(list)
	return has, list
}

// servers lists the servers the Mac knows and reads the key that reads their buckets.
func (e *env) servers(ctx context.Context) ([]server, access, error) {
	set, has, codes, err := e.known(ctx)
	if err != nil {
		return nil, access{}, err
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
	for _, c := range codes {
		s := server{Code: c, Bucket: "footops-" + c, Password: has["restic-"+c]}
		if has["kova-"+c] {
			b, err := e.keychain.Get(ctx, "kova-"+c)
			if err != nil {
				return nil, access{}, err
			}
			s.Bucket = strings.Trim(strings.TrimSpace(b), "/")
		}
		if st, ok := set.Servers[c]; ok {
			st = st.usable()
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
	return out, acc, nil
}

// SettingsView is footops-mac ayar's answer (its --json), and ayar yaz's once written:
// where the file is and every server the Mac knows, with what is set for it. Error: the
// file or the Keychain could not be read (what could be is still listed; writing
// replaces the file).
type SettingsView struct {
	Path    string          `json:"path"`
	Error   string          `json:"error,omitempty"`
	Servers []ServerSetting `json:"servers"`
}

// ServerSetting is one server's settings.
type ServerSetting struct {
	Code   string `json:"code"`
	Panel  string `json:"panel,omitempty"`
	SSH    string `json:"ssh,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	// DefaultBucket is where its backups are read when Bucket is not set: the Keychain's
	// kova-<code>, else footops-<code>.
	DefaultBucket string `json:"default_bucket"`
	// Keychain: its repository password is in the Keychain (restic-<code>); a server only
	// in the file is read all the same, with the same key.
	Keychain bool `json:"keychain"`
}

func (e *env) settingsView(ctx context.Context) SettingsView {
	v := SettingsView{Path: e.settings, Servers: []ServerSetting{}}
	// What can be read is listed all the same: the file not understood, the Keychain's
	// servers, for the file to be written anew; the Keychain locked, the file's.
	var problems []string
	set, err := readSettings(e.settings)
	if err != nil {
		problems = append(problems, err.Error())
	}
	// A value not to the rules is shown as written, for the window to put right.
	for _, p := range fieldProblems(set) {
		problems = append(problems, "ayarlar dosyası: "+p)
	}
	names, err := e.keychain.Names(ctx)
	if err != nil {
		problems = append(problems, err.Error())
	}
	has, codes := serverCodes(names, set)
	for _, c := range codes {
		s := ServerSetting{Code: c, DefaultBucket: "footops-" + c, Keychain: has["restic-"+c]}
		if has["kova-"+c] {
			if b, err := e.keychain.Get(ctx, "kova-"+c); err == nil {
				s.DefaultBucket = strings.Trim(strings.TrimSpace(b), "/")
			}
		}
		if st, ok := set.Servers[c]; ok {
			s.Panel, s.SSH, s.Bucket = st.Panel, st.SSH, st.Bucket
		}
		v.Servers = append(v.Servers, s)
	}
	v.Error = strings.Join(problems, "; ")
	return v
}

// settingsMax is the most read of new settings from stdin.
const settingsMax = 64 << 10

// readNewSettings reads the settings to write: the file's JSON, unknown fields refused
// (a misspelt one would be lost silently), the values trimmed, servers with nothing set
// left out, then held to the reader's rules.
func readNewSettings(in io.Reader) (Settings, error) {
	data, err := io.ReadAll(io.LimitReader(in, settingsMax+1))
	if err != nil {
		return Settings{}, fmt.Errorf("ayarlar okunamadı: %v", err)
	}
	if len(data) > settingsMax {
		return Settings{}, errors.New("ayarlar çok büyük")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s Settings
	if err := dec.Decode(&s); err != nil {
		return Settings{}, fmt.Errorf("ayarlar anlaşılamadı: %v", err)
	}
	if dec.More() {
		return Settings{}, errors.New("ayarlar anlaşılamadı: tek bir JSON nesnesi olmalı")
	}
	if s.Format != settingsFormat {
		return Settings{}, fmt.Errorf("ayarların biçimi %d olmalı", settingsFormat)
	}
	out := Settings{Format: settingsFormat}
	for code, srv := range s.Servers {
		srv = ServerSettings{Panel: strings.TrimSpace(srv.Panel), SSH: strings.TrimSpace(srv.SSH),
			Bucket: strings.Trim(strings.TrimSpace(srv.Bucket), "/")}
		if srv == (ServerSettings{}) {
			continue
		}
		// As written again: HTTPS:// is https:// (the app takes the scheme as written).
		if u, err := url.Parse(srv.Panel); srv.Panel != "" && err == nil && checkPanel(srv.Panel) == nil {
			srv.Panel = u.String()
		}
		if out.Servers == nil {
			out.Servers = map[string]ServerSettings{}
		}
		out.Servers[code] = srv
	}
	if err := checkSettings(out); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// saveSettings writes the file whole or not at all: a temporary file beside it (0600),
// then renamed over it; the folder 0700.
func saveSettings(path string, s Settings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ayarlar klasörü oluşturulamadı: %v", err)
	}
	// One there before (made by hand) is closed too.
	os.Chmod(dir, 0o700)
	f, err := os.CreateTemp(dir, ".ayarlar-*.json")
	if err != nil {
		return fmt.Errorf("ayarlar yazılamadı: %v", err)
	}
	tmp := f.Name()
	_, err = f.Write(append(data, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ayarlar yazılamadı: %v", err)
	}
	return nil
}

// settingsCmd is footops-mac ayar [yaz].
func (e *env) settingsCmd(ctx context.Context, args []string, jsonOut bool) error {
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "yaz":
		s, err := readNewSettings(e.in)
		if err != nil {
			return err
		}
		if err := saveSettings(e.settings, s); err != nil {
			return err
		}
		if !jsonOut {
			fmt.Fprintf(e.out, "✓ Ayarlar yazıldı: %s\n", e.settings)
			return nil
		}
	default:
		return errUsage
	}
	v := e.settingsView(ctx)
	if jsonOut {
		e.event(v)
		if v.Error != "" {
			return errReported
		}
		return nil
	}
	printSettings(e.out, v)
	if v.Error != "" {
		return errors.New(v.Error)
	}
	return nil
}
