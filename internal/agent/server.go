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
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

const (
	DefaultSocket    = "/run/footops/agent.sock"
	DefaultAuditPath = "/var/lib/footops/audit.jsonl"
	DefaultHistory   = "/var/lib/footops/metrics.json"
	PanelUser        = "footops"

	maxBody        = 1 << 20 // site environments can be large
	maxLogLines    = 2000
	restartTimeout = 10 * time.Second
)

// Server serves the agent API.
type Server struct {
	Docker  DockerAPI
	Audit   *Audit
	Sampler *Sampler
	Sites   *SiteOps
	Jobs    *Jobs
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
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // long work runs as jobs, not in requests
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
	mux.HandleFunc("GET /v1/history", s.history)
	mux.HandleFunc("GET /v1/alerts", s.alerts)
	mux.HandleFunc("GET /v1/containers/{name}/logs", s.logs)
	mux.HandleFunc("POST /v1/containers/{name}/restart", s.restart)
	mux.HandleFunc("GET /v1/audit", s.auditTail)
	mux.HandleFunc("POST /v1/audit", s.auditRecord)
	mux.HandleFunc("GET /v1/sites", s.listSites)
	mux.HandleFunc("POST /v1/sites", s.createSite)
	mux.HandleFunc("POST /v1/sites/{name}/redeploy", s.siteJob("site.redeploy"))
	mux.HandleFunc("POST /v1/sites/{name}/update", s.updateSite)
	mux.HandleFunc("POST /v1/sites/{name}/remove", s.siteJob("site.remove"))
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("GET /v1/certs", s.listCerts)
	mux.HandleFunc("POST /v1/certs", s.installCert)
	mux.HandleFunc("POST /v1/certs/{id}/remove", s.removeCert)
	mux.HandleFunc("GET /v1/server", s.serverInfo)
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

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	rng, ok := history.RangeByName(r.URL.Query().Get("range"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "geçersiz aralık")
		return
	}
	name := r.URL.Query().Get("container")
	if name != "" && !docker.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "geçersiz container adı")
		return
	}
	now := time.Now().UTC()
	out := History{Range: rng.Name, Step: int64(rng.Step / time.Second), From: now.Add(-rng.Span), To: now,
		Containers: map[string][]history.Point{}}
	if s.Sampler.History == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	var series map[string][]history.Point
	if name != "" {
		series = s.Sampler.History.Query(rng, now, name)
	} else {
		series = s.Sampler.History.Query(rng, now)
	}
	for k, v := range series {
		if k == history.HostKey {
			out.Host = v
		} else {
			out.Containers[k] = v
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) alerts(w http.ResponseWriter, r *http.Request) {
	list := []Alert{}
	if s.Sampler.Alerts != nil {
		list = s.Sampler.Alerts.Active()
	}
	writeJSON(w, http.StatusOK, list)
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
	Socket      string
	AuditPath   string
	HistoryPath string // metric history; empty keeps none
	PanelUser   string
	PanelHost   string // refused as a site domain
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
	ops := &SiteOps{Store: sites.Store{Root: "/"}, Docker: d, Run: run.Exec{}, PanelHost: cfg.PanelHost}
	sampler := NewSampler(d, nil)
	sampler.Alerts = NewAlerter()
	sampler.Alerts.Certs = ops.Store.Certs
	if cfg.HistoryPath != "" {
		h, err := history.Load(cfg.HistoryPath, time.Now())
		if err != nil {
			logger.Printf("metrik geçmişi okunamadı, boş başlıyor: %v", err)
		}
		sampler.History = h
		go keepHistory(ctx, h, cfg.HistoryPath, logger)
		// The last save runs before Main returns; the process exits right after.
		defer func() {
			if err := h.Save(cfg.HistoryPath); err != nil {
				logger.Printf("metrik geçmişi kaydedilemedi: %v", err)
			}
		}()
	}
	go sampler.Run(ctx, 10*time.Second)

	// Reconcile often: until it runs, a recreated Caddy cannot reach the sites. It only
	// inspects one network per site.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			for _, err := range ops.Reconcile(ctx) {
				logger.Printf("reconcile: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	s := &Server{
		Docker:      d,
		Audit:       NewAudit(cfg.AuditPath),
		Sampler:     sampler,
		Sites:       ops,
		Jobs:        NewJobs(),
		AllowedUIDs: map[uint32]bool{0: true, uint32(uid): true},
		Log:         logger,
	}
	logger.Printf("agent listening on %s (uid %d allowed)", cfg.Socket, uid)
	return s.Serve(ctx, l)
}

// keepHistory saves the metric history every few minutes until ctx is done.
func keepHistory(ctx context.Context, h *history.Store, path string, logger *log.Logger) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.Save(path); err != nil {
				logger.Printf("metrik geçmişi kaydedilemedi: %v", err)
			}
		}
	}
}

// drain discards a response body so the connection can be reused.
func drain(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	r.Close()
}

// ---- sites ----

func (s *Server) listSites(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sites.Store.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byName := map[string]ContainerInfo{}
	for _, c := range s.Sampler.Latest(r.Context()).Containers {
		byName[c.Name] = c
	}
	out := make([]SiteStatus, len(list))
	for i, sp := range list {
		out[i].Spec = sp
		if c, ok := byName[sp.Container()]; ok {
			out[i].Container = &c
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSite(w http.ResponseWriter, r *http.Request) {
	var req CreateSiteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, err := s.Sites.Check(req.Spec, req.Env)
	if err != nil {
		writeProblems(w, err)
		return
	}
	id, err := s.Jobs.Start("site.create", spec.Name, req.Actor, func(ctx context.Context, p Progress) error {
		return s.Sites.Create(ctx, spec, req.Env, p)
	}, s.recordJob)
	s.jobResponse(w, id, err)
}

func (s *Server) siteJob(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var req RestartRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "geçersiz istek")
			return
		}
		if _, err := s.Sites.Store.Get(name); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		id, err := s.Jobs.Start(kind, name, req.Actor, func(ctx context.Context, p Progress) error {
			if kind == "site.remove" {
				return s.Sites.Remove(ctx, name, p)
			}
			return s.Sites.Redeploy(ctx, name, p)
		}, s.recordJob)
		s.jobResponse(w, id, err)
	}
}

// updateSite validates at once (so the form can show problems) and then applies the
// change as a job. The audit log records what changed, by name only.
func (s *Server) updateSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req UpdateSiteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	changes, err := s.Sites.CheckUpdate(name, req.Update)
	if errors.Is(err, sites.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeProblems(w, err)
		return
	}
	summary := strings.Join(changes, "; ")
	id, err := s.Jobs.Start("site.update", name, req.Actor, func(ctx context.Context, p Progress) error {
		return s.Sites.Update(ctx, name, req.Update, p)
	}, func(j Job) {
		if j.Err != "" {
			summary += " — hata: " + j.Err
		}
		s.recordJobDetail(j, summary)
	})
	s.jobResponse(w, id, err)
}

func (s *Server) jobResponse(w http.ResponseWriter, id string, err error) {
	if errors.Is(err, ErrBusy) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, JobResponse{JobID: id})
}

func (s *Server) recordJob(j Job) { s.recordJobDetail(j, j.Err) }

func (s *Server) recordJobDetail(j Job, detail string) {
	e := AuditEntry{Actor: j.Actor, Action: j.Kind, Target: j.Target, OK: j.Err == "", Detail: detail}
	if err := s.Audit.Record(e); err != nil {
		s.logf("audit: %v", err)
	}
	// Refresh the dashboard so the new or removed container shows up.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.Sampler.Sample(ctx)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "işlem bulunamadı (agent yeniden başlamış olabilir)")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// ---- certificates ----

func (s *Server) listCerts(w http.ResponseWriter, r *http.Request) {
	certs, err := s.Sites.Store.Certs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if certs == nil {
		certs = []sites.Cert{}
	}
	writeJSON(w, http.StatusOK, certs)
}

func (s *Server) installCert(w http.ResponseWriter, r *http.Request) {
	var req InstallCertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	c, err := s.Sites.InstallCert(r.Context(), req.Cert, req.Key)
	s.record(AuditEntry{Actor: req.Actor, Action: "cert.install", Target: strings.Join(c.Names, " ")}, err)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) removeCert(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	id := r.PathValue("id")
	err := s.Sites.RemoveCert(r.Context(), id)
	s.record(AuditEntry{Actor: req.Actor, Action: "cert.remove", Target: id}, err)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, publicAddrs())
}

// publicAddrs lists public addresses of this host, for DNS instructions.
func publicAddrs() ServerInfo {
	var info ServerInfo
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || !ipn.IP.IsGlobalUnicast() || ipn.IP.IsPrivate() {
			continue
		}
		if ipn.IP.To4() != nil {
			info.IPv4 = append(info.IPv4, ipn.IP.String())
		} else {
			info.IPv6 = append(info.IPv6, ipn.IP.String())
		}
	}
	return info
}

func writeProblems(w http.ResponseWriter, err error) {
	var ve *sites.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "site tanımında sorunlar var", Problems: ve.Problems})
		return
	}
	writeErr(w, http.StatusInternalServerError, err.Error())
}
