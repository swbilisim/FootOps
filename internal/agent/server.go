package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
)

const (
	DefaultSocket    = "/run/footops/agent.sock"
	DefaultAuditPath = "/var/lib/footops/audit.jsonl"
	PanelUser        = "footops"

	maxBody        = 64 << 10
	maxLogLines    = 2000
	restartTimeout = 10 * time.Second
)

// Server serves the agent API.
type Server struct {
	Docker  DockerAPI
	Audit   *Audit
	Sampler *Sampler
	// AllowedUIDs may connect; everyone else gets 403. Checked against the kernel's
	// peer credentials, not anything the client sends.
	AllowedUIDs map[uint32]bool
	// PeerUID is replaceable in tests; it defaults to SO_PEERCRED.
	PeerUID func(net.Conn) (uint32, error)
	Log     *log.Logger
}

type ctxKey int

const (
	keyUID ctxKey = iota
	keyUIDErr
)

// Serve accepts connections on l until ctx is done.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if s.PeerUID == nil {
		s.PeerUID = peerUID
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			uid, err := s.PeerUID(c)
			if err != nil {
				return context.WithValue(ctx, keyUIDErr, err)
			}
			return context.WithValue(ctx, keyUID, uid)
		},
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Handler is the API. Every route is behind the peer check.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/overview", s.overview)
	mux.HandleFunc("GET /v1/containers/{name}/logs", s.logs)
	mux.HandleFunc("POST /v1/containers/{name}/restart", s.restart)
	mux.HandleFunc("GET /v1/audit", s.auditTail)
	mux.HandleFunc("POST /v1/audit", s.auditRecord)
	return s.checkPeer(mux)
}

func (s *Server) checkPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, ok := r.Context().Value(keyUID).(uint32)
		if !ok || !s.AllowedUIDs[uid] {
			s.logf("refused connection: uid=%v ok=%v err=%v", uid, ok, r.Context().Value(keyUIDErr))
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Sampler.Latest(r.Context()))
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "geçersiz container adı")
		return
	}
	tail := 200
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLines {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("tail 1-%d arasında olmalı", maxLogLines))
			return
		}
		tail = n
	}
	lines, err := s.Docker.Logs(r.Context(), name, tail)
	if err != nil {
		s.dockerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, LogsResponse{Lines: lines})
}

func (s *Server) restart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if !docker.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "geçersiz container adı")
		return
	}
	entry := AuditEntry{Actor: req.Actor, Action: "container.restart", Target: name}
	if _, err := s.Docker.Inspect(r.Context(), name); err != nil {
		s.record(entry, err)
		s.dockerErr(w, err)
		return
	}
	err := s.Docker.Restart(r.Context(), name, restartTimeout)
	s.record(entry, err)
	if err != nil {
		s.dockerErr(w, err)
		return
	}
	// Refresh the dashboard soon, independent of this request's lifetime.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.Sampler.Sample(ctx)
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditTail(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if x, err := strconv.Atoi(v); err == nil && x > 0 && x <= 1000 {
			n = x
		}
	}
	entries, err := s.Audit.Tail(n)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// auditRecord lets the web process log authentication events. Only "auth." actions are
// accepted, so a compromised web process cannot forge records of server actions.
func (s *Server) auditRecord(w http.ResponseWriter, r *http.Request) {
	var e AuditEntry
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	if !strings.HasPrefix(e.Action, "auth.") {
		writeErr(w, http.StatusBadRequest, "sadece auth.* olayları kaydedilebilir")
		return
	}
	e.Time = time.Time{} // the agent's clock, not the caller's
	if err := s.Audit.Record(e); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) record(e AuditEntry, err error) {
	e.OK = err == nil
	if err != nil {
		e.Detail = err.Error()
	}
	if aerr := s.Audit.Record(e); aerr != nil {
		s.logf("audit: %v", aerr)
	}
}

func (s *Server) dockerErr(w http.ResponseWriter, err error) {
	if docker.IsNotFound(err) {
		writeErr(w, http.StatusNotFound, "container bulunamadı")
		return
	}
	writeErr(w, http.StatusBadGateway, err.Error())
}

func (s *Server) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log.Printf(format, args...)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// Config is the agent's runtime configuration.
type Config struct {
	Socket    string
	AuditPath string
	PanelUser string
}

// Main runs the agent until ctx is done.
func Main(ctx context.Context, cfg Config, logger *log.Logger) error {
	u, err := user.Lookup(cfg.PanelUser)
	if err != nil {
		return fmt.Errorf("panel kullanıcısı %q: %w", cfg.PanelUser, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return err
	}

	_ = os.Remove(cfg.Socket) // stale socket from a previous run
	l, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	defer l.Close()
	// root:footops 0660 — the panel's web user can connect, nobody else.
	if err := os.Chown(cfg.Socket, 0, gid); err != nil {
		return err
	}
	if err := os.Chmod(cfg.Socket, 0o660); err != nil {
		return err
	}

	d := docker.New(docker.DefaultSocket)
	sampler := NewSampler(d, nil)
	go sampler.Run(ctx, 10*time.Second)

	s := &Server{
		Docker:      d,
		Audit:       NewAudit(cfg.AuditPath),
		Sampler:     sampler,
		AllowedUIDs: map[uint32]bool{0: true, uint32(uid): true},
		Log:         logger,
	}
	logger.Printf("agent listening on %s (uid %d allowed)", cfg.Socket, uid)
	return s.Serve(ctx, l)
}

// drain discards a response body so the connection can be reused.
func drain(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	r.Close()
}
