package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// DefaultRegistriesPath keeps when each registry login was saved. The tokens themselves
// stay where docker keeps them ($DOCKER_CONFIG/config.json); this file has none.
const DefaultRegistriesPath = "/var/lib/footops/registries.json"

// DefaultDockerConfig is the agent's DOCKER_CONFIG, set by its systemd unit (/root is
// hidden from the agent by ProtectHome).
const DefaultDockerConfig = "/var/lib/footops/docker"

// Token lifetimes worth an alert before pulls start failing.
const (
	tokenWarnDays = 14
	tokenBadDays  = 3
)

// RegistryLogin is a saved login to an image registry, without its token.
type RegistryLogin struct {
	Host  string    `json:"host"`
	User  string    `json:"user"`
	Saved time.Time `json:"saved,omitempty"`
	Actor string    `json:"actor,omitempty"`
	// Expires and Scopes are what GitHub reports for a GHCR token (classic tokens only).
	Expires time.Time `json:"expires,omitempty"`
	Scopes  []string  `json:"scopes,omitempty"`
}

// Registries manages the agent's registry logins. The token goes to `docker login`
// through stdin only: it is never on a command line, in a log or sent back to the panel.
// mu orders logins and logouts; listing reads files that are only ever replaced whole,
// so the alerts never wait for a slow login.
type Registries struct {
	ConfigDir string // docker's DOCKER_CONFIG
	MetaPath  string
	Run       run.Runner
	// GitHubAPI answers when a GHCR token expires; empty skips the question.
	GitHubAPI string
	HTTP      *http.Client

	mu  sync.Mutex
	now func() time.Time
}

var (
	reRegistryHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	reRegistryUser = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,99}$`)
	// reGitHubUser is GitHub's rule for user names (ghcr.io logins).
	reGitHubUser = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	// tokenPrefixes mark a token pasted into the user name field: it must not reach the
	// command line, the audit log or the list.
	tokenPrefixes = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "glpat-", "dckr_pat_"}
)

// CheckLogin validates a registry and user name; the audit log only ever records what
// passed it.
func CheckLogin(host, user string) (string, string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	user = strings.TrimSpace(user)
	if !reRegistryHost.MatchString(host) {
		return host, "", errors.New("kayıt defteri adresi geçersiz (örn. ghcr.io; https:// ve yol olmadan)")
	}
	for _, p := range tokenPrefixes {
		if strings.HasPrefix(strings.ToLower(user), p) {
			return host, "", errors.New("kullanıcı adı alanına bir token yazılmış gibi: oraya kullanıcı adını (GitHub için GitHub kullanıcı adını), token'ı alttaki alana yaz")
		}
	}
	if !reRegistryUser.MatchString(user) || (host == "ghcr.io" && !reGitHubUser.MatchString(user)) {
		return host, "", errors.New("kullanıcı adı geçersiz")
	}
	return host, user, nil
}

// dockerHubKey is how docker names Docker Hub in config.json.
const dockerHubKey = "https://index.docker.io/v1/"

func (r *Registries) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// List returns the saved logins, sorted by host.
func (r *Registries) List() ([]RegistryLogin, error) { return r.list() }

func (r *Registries) list() ([]RegistryLogin, error) {
	cfg, err := r.readConfig()
	if err != nil {
		return nil, err
	}
	meta := r.readMeta()
	out := []RegistryLogin{}
	for key, a := range cfg.Auths {
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://"), "/")
		if key == dockerHubKey {
			host = "docker.io"
		}
		l := meta[host]
		l.Host, l.User = host, authUser(a.Auth)
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out, nil
}

type dockerConfig struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
}

func (r *Registries) readConfig() (dockerConfig, error) {
	var c dockerConfig
	b, err := os.ReadFile(filepath.Join(r.ConfigDir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s/config.json okunamadı: %w", r.ConfigDir, err)
	}
	return c, nil
}

// authUser takes the user name out of docker's base64 "user:token"; the token is dropped.
func authUser(auth string) string {
	b, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return ""
	}
	u, _, ok := strings.Cut(string(b), ":")
	if !ok || !reRegistryUser.MatchString(u) {
		return ""
	}
	return u
}

func (r *Registries) readMeta() map[string]RegistryLogin {
	m := map[string]RegistryLogin{}
	b, err := os.ReadFile(r.MetaPath)
	if err != nil {
		return m
	}
	var list []RegistryLogin
	if json.Unmarshal(b, &list) == nil {
		for _, l := range list {
			m[l.Host] = l
		}
	}
	return m
}

func (r *Registries) writeMeta(m map[string]RegistryLogin) error {
	list := make([]RegistryLogin, 0, len(m))
	for _, l := range m {
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Host < list[j].Host })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.MetaPath), 0o700); err != nil {
		return err
	}
	return writeFile(r.MetaPath, string(b)+"\n", 0o600)
}

// cleanToken trims what copying a token tends to add and refuses anything that cannot
// be a token, so a pasted sentence never reaches docker.
func cleanToken(t string) (string, error) {
	t = strings.TrimSpace(t)
	if t == "" || len(t) > 4096 {
		return "", errors.New("token boş ya da çok uzun")
	}
	for _, c := range t {
		if c <= ' ' || c == 0x7f {
			return "", errors.New("token boşluk ya da satır sonu içeremez")
		}
	}
	return t, nil
}

// Login saves a login after docker has checked it against the registry. For GHCR it
// also asks GitHub when the token expires and what it may do; failing to learn that
// never fails the login.
func (r *Registries) Login(ctx context.Context, host, user, token, actor string) (RegistryLogin, []string, error) {
	host, user, err := CheckLogin(host, user)
	if err != nil {
		return RegistryLogin{}, nil, err
	}
	token, err = cleanToken(token)
	if err != nil {
		return RegistryLogin{}, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(r.ConfigDir, 0o700); err != nil {
		return RegistryLogin{}, nil, err
	}
	if _, err := r.Run.RunInput(ctx, token, "docker", "login", host, "--username", user, "--password-stdin"); err != nil {
		m := strings.ToLower(err.Error())
		if strings.Contains(m, "unauthorized") || strings.Contains(m, "denied") || strings.Contains(m, "incorrect") {
			return RegistryLogin{}, nil, errors.New("giriş reddedildi: kullanıcı adı ya da token yanlış, süresi dolmuş ya da token'ın paket okuma izni (read:packages) yok")
		}
		return RegistryLogin{}, nil, fmt.Errorf("giriş yapılamadı: %s", clipMsg(lastLine(err.Error()), 300))
	}
	l := RegistryLogin{Host: host, User: user, Saved: r.clock().UTC(), Actor: actor}
	var notes []string
	if host == "ghcr.io" && r.GitHubAPI != "" {
		exp, scopes, err := r.githubToken(ctx, token)
		switch {
		case err != nil:
			notes = append(notes, "token'ın bitiş tarihi GitHub'dan öğrenilemedi: "+err.Error())
		default:
			l.Expires, l.Scopes = exp, scopes
			if extra := extraScopes(scopes); len(extra) > 0 {
				notes = append(notes, "token gereğinden geniş yetkili ("+strings.Join(extra, ", ")+"); sunucuda yalnız read:packages yeter, daha dar bir token oluşturup yenile")
			}
			if exp.IsZero() {
				notes = append(notes, "token'ın bitiş tarihi yok")
			}
		}
	}
	meta := r.readMeta()
	meta[host] = l
	if err := r.writeMeta(meta); err != nil {
		notes = append(notes, "giriş kaydedildi ama tarih bilgisi yazılamadı: "+err.Error())
	}
	return l, notes, nil
}

// extraScopes lists what a token may do beyond reading packages.
func extraScopes(scopes []string) []string {
	var out []string
	for _, s := range scopes {
		if s != "read:packages" {
			out = append(out, s)
		}
	}
	return out
}

// githubToken asks GitHub about a classic token: its expiry and scopes come back as
// response headers of any authenticated request.
func (r *Registries) githubToken(ctx context.Context, token string) (time.Time, []string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.GitHubAPI, "/")+"/user", nil)
	if err != nil {
		return time.Time{}, nil, err
	}
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	c := r.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return time.Time{}, nil, errors.New("GitHub'a ulaşılamadı")
	}
	drain(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, nil, fmt.Errorf("GitHub %d döndü", resp.StatusCode)
	}
	var scopes []string
	for _, s := range strings.Split(resp.Header.Get("X-OAuth-Scopes"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			scopes = append(scopes, s)
		}
	}
	var exp time.Time
	if v := strings.TrimSpace(resp.Header.Get("GitHub-Authentication-Token-Expiration")); v != "" {
		for _, layout := range []string{"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", time.RFC3339} {
			if t, err := time.Parse(layout, v); err == nil {
				exp = t.UTC()
				break
			}
		}
	}
	return exp, scopes, nil
}

// Logout removes a login: docker forgets the token, FootOps its dates.
func (r *Registries) Logout(ctx context.Context, host string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.list()
	if err != nil {
		return err
	}
	found := false
	for _, l := range list {
		found = found || l.Host == host
	}
	if !found || !reRegistryHost.MatchString(host) {
		return errors.New("böyle bir giriş yok")
	}
	// docker files Docker Hub under its index address, which `docker logout docker.io`
	// does not look for; plain `docker logout` does.
	args := []string{"logout", host}
	if host == "docker.io" {
		args = []string{"logout"}
	}
	if _, err := r.Run.Run(ctx, "docker", args...); err != nil {
		return fmt.Errorf("çıkış yapılamadı: %s", clipMsg(lastLine(err.Error()), 300))
	}
	after, err := r.list()
	if err != nil {
		return err
	}
	for _, l := range after {
		if l.Host == host {
			return errors.New("çıkış yapılamadı: docker girişi silmedi")
		}
	}
	meta := r.readMeta()
	delete(meta, host)
	return r.writeMeta(meta)
}

// tokenAlerts warns before a saved token expires: pulls (new sites, redeploys) fail
// from that day on.
func tokenAlerts(list []RegistryLogin, now time.Time) []Alert {
	var out []Alert
	for _, l := range list {
		if l.Expires.IsZero() {
			continue
		}
		days := int(l.Expires.Sub(now).Hours() / 24)
		key := "registry:" + l.Host
		switch {
		case !now.Before(l.Expires):
			out = append(out, Alert{Key: key, Level: "bad", Title: l.Host + " token'ının süresi doldu",
				Detail: "İmaj indirilemez (yeni site, yeniden dağıtım). Yeni token oluşturup İmaj girişleri sayfasından kaydet.", Link: "/registries", Since: now})
		case days < tokenWarnDays:
			lv := "warn"
			if days < tokenBadDays {
				lv = "bad"
			}
			out = append(out, Alert{Key: key, Level: lv, Title: fmt.Sprintf("%s token'ının bitmesine %d gün var", l.Host, days),
				Detail: "Yeni token oluşturup İmaj girişleri sayfasından kaydet.", Link: "/registries", Since: now})
		}
	}
	return out
}
