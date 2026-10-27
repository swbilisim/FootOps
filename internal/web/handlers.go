package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/docker"
)

// liveHeader marks the background requests of live.js: "full", or "1" for a light
// refresh that leaves the charts out.
const liveHeader = "X-FootOps-Live"

func lightRefresh(r *http.Request) bool { return r.Header.Get(liveHeader) == "1" }

const (
	maxForm       = 8 << 10 // login
	maxAuthedForm = 1 << 20 // site environments, certificates
)

type sessKey struct{}

func current(r *http.Request) Session {
	s, _ := r.Context().Value(sessKey{}).(Session)
	return s
}

// requireSession admits requests with a valid session created on the same listener and,
// for the tunnel, by the same Cloudflare Access identity. State-changing requests must
// carry the session's CSRF token: forms in a "csrf" field, upload.js in the X-CSRF-Token
// header (its bodies are JSON or file chunks, never parsed as a form).
func (s *Server) requireSession(h http.HandlerFunc) http.Handler { return s.session(h, false) }

// requireStreamSession is for the one multipart form (an upload without JavaScript):
// the handler reads the CSRF field itself, as the first part of the stream, because
// parsing the form here would buffer the file.
func (s *Server) requireStreamSession(h http.HandlerFunc) http.Handler { return s.session(h, true) }

func (s *Server) session(h http.HandlerFunc, stream bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ri := info(r)
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		lookup := s.sessions.get
		if r.Method == http.MethodGet && r.Header.Get(liveHeader) != "" {
			lookup = s.sessions.peek
		}
		sess, ok := lookup(c.Value)
		if !ok || sess.Via != ri.via || sess.AccessEmail != ri.accessEmail {
			if ok {
				s.sessions.delete(sess.ID)
			}
			s.clearCookie(w, r)
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		switch tok := r.Header.Get(csrfHeader); {
		case r.Method == http.MethodGet || r.Method == http.MethodHead:
		case stream && r.Method == http.MethodPost:
		case tok != "" || r.Method != http.MethodPost:
			if subtle.ConstantTimeCompare([]byte(tok), []byte(sess.CSRF)) != 1 {
				http.Error(w, "geçersiz istek (CSRF); sayfayı yenileyip tekrar dene", http.StatusForbidden)
				return
			}
			if r.Method == http.MethodPost {
				r.Body = http.MaxBytesReader(w, r.Body, maxAuthedForm)
			}
		default:
			r.Body = http.MaxBytesReader(w, r.Body, maxAuthedForm)
			if err := r.ParseForm(); err != nil ||
				subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
				http.Error(w, "geçersiz form (CSRF); sayfayı yenileyip tekrar dene", http.StatusForbidden)
				return
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), sessKey{}, sess)))
	})
}

func (s *Server) cookie(r *http.Request, name, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		// The SSH-tunnel listener is plain HTTP on localhost (the SSH connection is the
		// encryption); Safari drops Secure cookies there, so Secure is tunnel-only.
		Secure: info(r).via == viaTunnel,
	}
}

func (s *Server) clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.cookie(r, sessionCookie, "", -1))
}

func (s *Server) actor(r *http.Request) agent.Actor {
	ri := info(r)
	return agent.Actor{User: current(r).User, IP: ri.clientIP, Via: ri.via}
}

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.agent.AuditTail(r.Context(), 200)
	s.render(w, r, "audit", map[string]any{"Title": "Denetim kaydı", "Entries": entries, "Err": errString(err)})
}

func (s *Server) recordAuth(r *http.Request, user, action string, ok bool, detail string) {
	ri := info(r)
	e := agent.AuditEntry{Actor: agent.Actor{User: user, IP: ri.clientIP, Via: ri.via}, Action: action, OK: ok, Detail: detail}
	if ri.accessEmail != "" {
		e.Detail = fmt.Sprintf("%s (access: %s)", detail, ri.accessEmail)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.agent.RecordAuth(ctx, e); err != nil {
		s.logf("audit %s: %v", action, err)
	}
}

// ---- login / logout ----

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if sess, ok := s.sessions.get(c.Value); ok && sess.Via == info(r).via {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.renderLogin(w, r, "", http.StatusOK)
}

// renderLogin issues a fresh login CSRF token (double-submit cookie): login forms need
// CSRF protection too, or an attacker could log the operator into another session.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, msg string, status int) {
	tok := randomToken()
	http.SetCookie(w, s.cookie(r, loginCSRFCookie, tok, 900))
	s.renderStatus(w, r, status, "login", map[string]any{"CSRF": tok, "Error": msg})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, "Form okunamadı.", http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(loginCSRFCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("csrf"))) != 1 {
		s.renderLogin(w, r, "Oturum formu süresi doldu, tekrar dene.", http.StatusForbidden)
		return
	}
	if !s.ipLimit.allow(ri.clientIP) || !s.allLimit.allow("all") {
		s.recordAuth(r, r.PostFormValue("username"), "auth.login", false, "rate-limited")
		s.renderLogin(w, r, "Çok fazla deneme. Birkaç dakika sonra tekrar dene.", http.StatusTooManyRequests)
		return
	}
	user := r.PostFormValue("username")
	res, err := s.store.Login(user, r.PostFormValue("password"), r.PostFormValue("code"), s.now())
	if errors.Is(err, auth.ErrNoAccount) {
		s.renderLogin(w, r, "Panel hesabı yok. Sunucuda: sudo footops reset-password", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.logf("login: %v", err)
		s.renderLogin(w, r, "Sunucu hatası.", http.StatusInternalServerError)
		return
	}
	if !res.OK {
		s.recordAuth(r, user, "auth.login", false, res.Reason)
		msg := "Kullanıcı adı, şifre veya doğrulama kodu hatalı."
		if !res.LockedUntil.IsZero() {
			msg = fmt.Sprintf("Çok fazla hatalı deneme. Hesap %s'e kadar kilitli.", res.LockedUntil.Local().Format("15:04"))
		}
		s.renderLogin(w, r, msg, http.StatusUnauthorized)
		return
	}

	// A new session ID on every login prevents session fixation.
	sess := s.sessions.create(user, ri.via, ri.accessEmail)
	detail := "totp"
	if res.UsedRecovery {
		detail = "recovery-code"
		a, _ := s.store.Load()
		s.sessions.setFlash(sess.ID, fmt.Sprintf("Kurtarma kodu kullanıldı, %d kod kaldı. Kodlar biterse sunucuda 'sudo footops reset-2fa' çalıştır.", len(a.RecoveryCodes)))
	}
	s.recordAuth(r, user, "auth.login", true, detail)
	http.SetCookie(w, s.cookie(r, sessionCookie, sess.ID, 0))
	http.SetCookie(w, s.cookie(r, loginCSRFCookie, "", -1))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	sess := current(r)
	s.sessions.delete(sess.ID)
	s.recordAuth(r, sess.User, "auth.logout", true, "")
	s.clearCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- pages ----

// restartConfirm is a separate page so a stray click never restarts anything (and the
// panel needs no JavaScript for a confirm dialog).
func (s *Server) restartConfirm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "restart", map[string]any{"Title": name, "Name": name})
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	err := s.agent.Restart(r.Context(), name, s.actor(r))
	msg := name + " yeniden başlatıldı."
	if err != nil {
		msg = name + " yeniden başlatılamadı: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- rendering ----

type page struct {
	Title   string
	Nav     string // highlighted menu entry
	Session Session
	Server  string
	Flash   string
	Alerts  alertSummary
	Data    map[string]any
}

// alertSummary is shown on every page: a count in the menu, the list on the dashboard.
type alertSummary struct {
	List []agent.Alert
	Bad  bool // at least one serious alert
}

// navOf maps a template to its menu entry.
var navOf = map[string]string{
	"dashboard": "server", "container": "server", "logs": "server", "restart": "server",
	"sites": "sites", "site": "sites", "site_new": "sites", "site_edit": "sites", "site_confirm": "sites", "job": "sites",
	"import": "sites", "import_preview": "sites", "restore_confirm": "sites",
	"certs": "certs", "cert_remove": "certs",
	"audit": "audit",
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	s.renderStatus(w, r, http.StatusOK, name, data)
}

// renderStatus executes the template into a buffer first, so a template error never
// leaves a half-written page behind a 200 status.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	sess := current(r)
	p := page{Session: sess, Server: s.cfg.ServerCode, Nav: navOf[name], Data: data}
	p.Title, _ = data["Title"].(string)
	if sess.ID != "" {
		p.Alerts = s.alerts(r)
	}
	// A background refresh must not swallow a flash message meant for a real page load.
	if sess.ID != "" && r.Header.Get(liveHeader) == "" {
		p.Flash = s.sessions.takeFlash(sess.ID)
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name+".html", p); err != nil {
		s.logf("render %s: %v", name, err)
		http.Error(w, "sayfa oluşturulamadı", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// alerts asks the agent for active alerts; an unreachable agent shows up elsewhere on the
// page, so a failure here just means no badge.
func (s *Server) alerts(r *http.Request) alertSummary {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	list, err := s.agent.Alerts(ctx)
	if err != nil {
		return alertSummary{}
	}
	sum := alertSummary{List: list}
	for _, a := range list {
		if a.Level == "bad" {
			sum.Bad = true
		}
	}
	return sum
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var funcs = template.FuncMap{
	"bytes": func(v any) string {
		var b uint64
		switch n := v.(type) {
		case uint64:
			b = n
		case int64:
			b = uint64(max(n, 0))
		case int:
			b = uint64(max(n, 0))
		}
		const unit = 1024
		if b < unit {
			return fmt.Sprintf("%d B", b)
		}
		div, exp := uint64(unit), 0
		for n := b / unit; n >= unit; n /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
	},
	"pct": func(used, total uint64) float64 {
		if total == 0 {
			return 0
		}
		return math.Round(float64(used)/float64(total)*1000) / 10
	},
	"f1": func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) },
	"f2": func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
	"dur": func(d time.Duration) string {
		d = d.Round(time.Minute)
		days := int(d.Hours()) / 24
		h := int(d.Hours()) % 24
		m := int(d.Minutes()) % 60
		if days > 0 {
			return fmt.Sprintf("%d gün %d sa", days, h)
		}
		return fmt.Sprintf("%d sa %d dk", h, m)
	},
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "az önce"
		case d < time.Hour:
			return fmt.Sprintf("%d dk önce", int(d.Minutes()))
		case d < 48*time.Hour:
			return fmt.Sprintf("%d sa önce", int(d.Hours()))
		default:
			return fmt.Sprintf("%d gün önce", int(d.Hours()/24))
		}
	},
	"ts": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
	// took is a short duration for job measurements: "350 ms", "4.2 sn", "3 dk 5 sn".
	"took": func(d time.Duration) string {
		switch {
		case d < time.Second:
			return fmt.Sprintf("%d ms", d.Milliseconds())
		case d < time.Minute:
			return fmt.Sprintf("%.1f sn", d.Seconds())
		}
		return fmt.Sprintf("%d dk %d sn", int(d.Minutes()), int(d.Seconds())%60)
	},
	"datefmt": func(t time.Time) string { return t.Local().Format("02.01.2006") },
	"daysLeft": func(t, now time.Time) int {
		return int(math.Floor(t.Sub(now).Hours() / 24))
	},
	// undoLinks reads a restore job's result ("<copy id> files-<backup id>").
	"undoLinks": func(result string) []undoLink {
		var out []undoLink
		for _, p := range strings.Fields(result) {
			if id, ok := strings.CutPrefix(p, "files-"); ok {
				out = append(out, undoLink{"files/" + id, "Dosyaları geri al…"})
			} else {
				out = append(out, undoLink{"snapshots/" + p, "Veritabanını geri al…"})
			}
		}
		return out
	},
	"jobTitle": func(kind string) string {
		if t, ok := jobTitles[kind]; ok {
			return t
		}
		return kind
	},
	"reasonTR": func(r string) string {
		if t, ok := reasonNames[r]; ok {
			return t
		}
		return r
	},
	"level":  levelOf,
	"icon":   icon,
	"logo":   func() template.HTML { return logo },
	"sprite": func() template.HTML { return sprite },
	"meter":  meter,
	// limited reports whether a container has a memory limit of its own; without one
	// Docker reports the host's memory.
	"limited": func(limit, host uint64) bool { return limit > 0 && (host == 0 || limit < host) },
	"stateTR": func(s string) string {
		if t, ok := stateNames[s]; ok {
			return t
		}
		return s
	},
	"healthTR": func(s string) string {
		if t, ok := healthNames[s]; ok {
			return t
		}
		return s
	},
	"status": func(c agent.ContainerInfo) string {
		switch {
		case c.State != "running":
			return "bad"
		case c.Health == "unhealthy":
			return "bad"
		case c.Health == "starting" || c.RestartCount > 0:
			return "warn"
		default:
			return "ok"
		}
	},
}

// undoLink is a link to undo part of a restore.
type undoLink struct{ Path, Label string }

var (
	jobTitles = map[string]string{
		"site.create": "Site oluşturuluyor", "site.remove": "Site kaldırılıyor", "site.update": "Site güncelleniyor",
		"site.redeploy": "Yeniden dağıtılıyor", "db.snapshot": "Veritabanı kopyası", "import.check": "İçe aktarma: kontrol",
		"import.apply": "İçe aktarılıyor", "db.restore": "Kopyadan geri yükleme", "site.export": "Dışa aktarma",
		"db.recover": "Yarıda kalan işlemi tamamlama", "files.restore": "Dosyaları geri yükleme",
	}
	reasonNames = map[string]string{"manual": "elle", "before-import": "içe aktarmadan önce", "before-restore": "geri yüklemeden önce"}
	stateNames  = map[string]string{"running": "çalışıyor", "exited": "durdu", "restarting": "yeniden başlıyor", "created": "oluşturuldu", "paused": "duraklatıldı", "dead": "ölü", "removing": "siliniyor"}
	healthNames = map[string]string{"healthy": "sağlıklı", "unhealthy": "sağlıksız", "starting": "başlıyor"}
)
