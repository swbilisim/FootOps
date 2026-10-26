// Package web is the unprivileged half of the panel: login, dashboard and the forms that
// ask the agent to act. It runs as the "footops" user, listens on loopback only and
// never touches Docker or the filesystem outside its own state directory.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/auth"
	"github.com/swbilisim/FootOps/internal/cfaccess"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/sites"
)

//go:embed templates/*.html static/*
var assets embed.FS

// AgentAPI is what the web process may ask the agent for.
type AgentAPI interface {
	Overview(ctx context.Context) (agent.Overview, error)
	Restart(ctx context.Context, name string, a agent.Actor) error
	Logs(ctx context.Context, name string, tail int) ([]string, error)
	RecordAuth(ctx context.Context, e agent.AuditEntry) error
	AuditTail(ctx context.Context, limit int) ([]agent.AuditEntry, error)
	Sites(ctx context.Context) ([]agent.SiteStatus, error)
	CreateSite(ctx context.Context, req agent.CreateSiteRequest) (string, error)
	SiteJob(ctx context.Context, name, verb string, a agent.Actor) (string, error)
	Job(ctx context.Context, id string) (agent.Job, error)
	Certs(ctx context.Context) ([]sites.Cert, error)
	InstallCert(ctx context.Context, certPEM, keyPEM string, a agent.Actor) (sites.Cert, error)
	RemoveCert(ctx context.Context, id string, a agent.Actor) error
	Server(ctx context.Context) (agent.ServerInfo, error)
}

// AccessVerifier checks Cloudflare Access tokens. *cfaccess.Verifier implements it.
type AccessVerifier interface {
	Verify(ctx context.Context, token string) (cfaccess.Claims, error)
}

const (
	viaTunnel = "tunnel"
	viaSSH    = "ssh"

	sessionCookie   = "footops_session"
	loginCSRFCookie = "footops_login"
)

type Server struct {
	cfg      config.Panel
	agent    AgentAPI
	store    *auth.Store
	access   AccessVerifier // nil when Cloudflare Access is not configured
	sessions *sessions
	ipLimit  *limiter // login attempts per client IP
	allLimit *limiter // login attempts overall (distributed guessing)
	tmpl     *template.Template
	now      func() time.Time
	log      *log.Logger
}

func NewServer(cfg config.Panel, a AgentAPI, store *auth.Store, access AccessVerifier, logger *log.Logger) (*Server, error) {
	t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	now := time.Now
	return &Server{
		cfg: cfg, agent: a, store: store, access: access, log: logger, tmpl: t, now: now,
		sessions: newSessions(now),
		ipLimit:  newLimiter(10, 10*time.Minute, now),
		allLimit: newLimiter(50, 10*time.Minute, now),
	}, nil
}

type ctxKey int

const keyReq ctxKey = 0

// reqInfo is per-request context set by the listener-specific middleware.
type reqInfo struct {
	via         string
	clientIP    string
	accessEmail string
}

func info(r *http.Request) reqInfo {
	ri, _ := r.Context().Value(keyReq).(reqInfo)
	return ri
}

// Handler returns the handler for one listener.
func (s *Server) Handler(via string) http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.Handle("POST /logout", s.requireSession(s.logout))
	mux.Handle("GET /{$}", s.requireSession(s.dashboard))
	mux.Handle("GET /containers/{name}/logs", s.requireSession(s.logs))
	mux.Handle("GET /containers/{name}/restart", s.requireSession(s.restartConfirm))
	mux.Handle("POST /containers/{name}/restart", s.requireSession(s.restart))
	mux.Handle("GET /audit", s.requireSession(s.audit))
	mux.Handle("GET /sites", s.requireSession(s.sitesList))
	mux.Handle("GET /sites/new", s.requireSession(s.siteNew))
	mux.Handle("POST /sites/new", s.requireSession(s.siteCreate))
	mux.Handle("GET /sites/{name}", s.requireSession(s.site))
	mux.Handle("GET /sites/{name}/redeploy", s.requireSession(s.siteConfirm("redeploy")))
	mux.Handle("POST /sites/{name}/redeploy", s.requireSession(s.siteAction("redeploy")))
	mux.Handle("GET /sites/{name}/remove", s.requireSession(s.siteConfirm("remove")))
	mux.Handle("POST /sites/{name}/remove", s.requireSession(s.siteAction("remove")))
	mux.Handle("GET /jobs/{id}", s.requireSession(s.job))
	mux.Handle("GET /certs", s.requireSession(s.certs))
	mux.Handle("POST /certs", s.requireSession(s.certInstall))
	mux.Handle("GET /certs/{id}/remove", s.requireSession(s.certRemoveConfirm))
	mux.Handle("POST /certs/{id}/remove", s.requireSession(s.certRemove))

	var h http.Handler = mux
	h = s.checkOrigin(h)
	if via == viaTunnel {
		h = s.accessGate(h)
	} else {
		h = s.localGate(h)
	}
	return securityHeaders(h)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// No JavaScript at all: the strictest policy that still renders the pages.
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// accessGate admits requests from cloudflared only with a valid Cloudflare Access token
// for the panel's own hostname.
func (s *Server) accessGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(stripPort(r.Host), s.cfg.PanelHost) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		claims, err := s.access.Verify(r.Context(), r.Header.Get(cfaccess.Header))
		if err != nil {
			s.logf("access denied from %s: %v", r.Header.Get("Cf-Connecting-Ip"), err)
			http.Error(w, "Cloudflare Access doğrulaması başarısız", http.StatusForbidden)
			return
		}
		ip := r.Header.Get("Cf-Connecting-Ip") // trustworthy only because the token checked out
		if net.ParseIP(ip) == nil {
			ip = "unknown"
		}
		ctx := context.WithValue(r.Context(), keyReq, reqInfo{via: viaTunnel, clientIP: ip, accessEmail: claims.Email})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// localGate serves the SSH-tunnel listener. It only answers to localhost host names,
// which blocks DNS-rebinding pages in the operator's browser from reaching the panel
// through an open tunnel.
func (s *Server) localGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch stripPort(r.Host) {
		case "localhost", "127.0.0.1", "[::1]", "::1":
		default:
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
			return
		}
		ctx := context.WithValue(r.Context(), keyReq, reqInfo{via: viaSSH, clientIP: "ssh-tunnel"})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// checkOrigin rejects cross-site state-changing requests before they reach a handler.
// The CSRF token is the primary defence; this is a second, independent one.
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
			if o := r.Header.Get("Origin"); o != "" && o != "null" {
				if stripScheme(o) != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return host
}

func stripScheme(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		return origin[i+3:]
	}
	return origin
}

func (s *Server) logf(format string, args ...any) {
	if s.log != nil {
		s.log.Printf(format, args...)
	}
}

// Run starts the listeners and blocks until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	type listener struct{ addr, via string }
	ls := []listener{{s.cfg.ListenLocal, viaSSH}}
	if s.access != nil {
		ls = append(ls, listener{s.cfg.ListenTunnel, viaTunnel})
	}
	errc := make(chan error, len(ls))
	var servers []*http.Server
	for _, l := range ls {
		srv := &http.Server{
			Addr:              l.addr,
			Handler:           s.Handler(l.via),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    32 << 10,
		}
		servers = append(servers, srv)
		s.logf("web listening on %s (%s)", l.addr, l.via)
		go func() { errc <- srv.ListenAndServe() }()
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sessions.sweep()
			}
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shut)
	}
	return nil
}

// Main loads configuration and runs the web process.
func Main(ctx context.Context, logger *log.Logger) error {
	cfg, err := config.Load(config.Path)
	if err != nil {
		return err
	}
	var access AccessVerifier
	if cfg.TunnelEnabled() {
		access = cfaccess.New(cfg.AccessTeam, cfg.AccessAUD)
	}
	s, err := NewServer(cfg, agent.NewClient(agent.DefaultSocket), auth.NewStore(config.AuthPath), access, logger)
	if err != nil {
		return err
	}
	return s.Run(ctx)
}
