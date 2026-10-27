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
	"github.com/swbilisim/FootOps/internal/staging"
)

const (
	DefaultSocket    = "/run/footops/agent.sock"
	DefaultAuditPath = "/var/lib/footops/audit.jsonl"
	DefaultHistory   = "/var/lib/footops/metrics.json"
	DefaultJobsDir   = "/var/lib/footops/jobs"
	PanelUser        = "footops"

	maxBody        = 1 << 20 // site environments can be large
	maxLogLines    = 2000
	restartTimeout = 10 * time.Second
	// downloadTimeout replaces the 60-second write timeout for file downloads.
	downloadTimeout = 2 * time.Hour
	// ActorHeader carries the acting user on GET requests that are audited (downloads).
	ActorHeader = "X-Footops-Actor"
)

// Server serves the agent API.
type Server struct {
	Docker  DockerAPI
	Audit   *Audit
	Sampler *Sampler
	Sites   *SiteOps
	Data    *DataOps
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
	mux.HandleFunc("GET /v1/sites/{name}/snapshots", s.listSnapshots)
	mux.HandleFunc("POST /v1/sites/{name}/snapshots", s.takeSnapshot)
	mux.HandleFunc("GET /v1/sites/{name}/snapshots/{id}/download", s.downloadSnapshot)
	mux.HandleFunc("POST /v1/sites/{name}/snapshots/{id}/restore", s.restoreSnapshot)
	mux.HandleFunc("POST /v1/sites/{name}/uploads", s.createUpload)
	mux.HandleFunc("POST /v1/sites/{name}/uploads/single", s.singleUpload)
	mux.HandleFunc("GET /v1/uploads/{id}", s.getUpload)
	mux.HandleFunc("PUT /v1/uploads/{id}/chunks/{n}", s.putChunk)
	mux.HandleFunc("POST /v1/uploads/{id}/finish", s.finishUpload)
	mux.HandleFunc("DELETE /v1/uploads/{id}", s.removeUpload)
	mux.HandleFunc("POST /v1/sites/{name}/imports", s.checkImport)
	mux.HandleFunc("GET /v1/sites/{name}/imports/{id}", s.getImport)
	mux.HandleFunc("POST /v1/sites/{name}/imports/{id}/apply", s.applyImport)
	mux.HandleFunc("POST /v1/sites/{name}/exports", s.startExport)
	mux.HandleFunc("GET /v1/sites/{name}/exports", s.listExports)
	mux.HandleFunc("GET /v1/sites/{name}/exports/{id}/download", s.downloadExport)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
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
		r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r))
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
	// A site stopped for a restore must stay stopped until the job starts it.
	if s.Jobs != nil {
		if j, busy := s.Jobs.Running(); busy && j.Target == name {
			err := fmt.Errorf("%s için bir işlem sürüyor (%s); bitince tekrar dene", name, j.Kind)
			s.record(entry, err)
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
	}
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
	JobsDir     string // finished jobs, so results survive a restart; empty keeps none
	SnapshotDir string
	StagingDir  string
	RecoveryDir string // restores in progress, finished after an agent restart
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
	data := &DataOps{Store: ops.Store, Docker: d, Run: run.Exec{KillOnCancel: true}, Stream: run.StreamExec{},
		Staging: staging.New(cfg.StagingDir, staging.DefaultMax), SnapshotDir: cfg.SnapshotDir,
		RecoveryDir: cfg.RecoveryDir, Sites: ops}
	audit := NewAudit(cfg.AuditPath)
	jobs := NewJobs()
	tempDBs := false
	if cfg.JobsDir != "" {
		j, interrupted, err := LoadJobs(cfg.JobsDir)
		if err != nil {
			logger.Printf("iş geçmişi okunamadı: %v", err)
		}
		jobs = j
		for _, job := range interrupted {
			logger.Printf("yarıda kalan iş: %s %s (%s)", job.Kind, job.Target, job.ID)
			tempDBs = tempDBs || job.Kind == "import.check"
			_ = audit.Record(AuditEntry{Actor: job.Actor, Action: job.Kind, Target: job.Target, Detail: job.Err})
			// Its staging may hold a connection file with a password (rule K5).
			if err := data.Staging.Remove(job.ID); err != nil {
				logger.Printf("geçici klasör silinemedi: %v", err)
			}
		}
	}
	// No job runs yet, so any tool container still around was cut off by a restart, and
	// any copy without a manifest is half-written.
	removeTools(ctx, d, run.Exec{}, logger)
	for _, x := range data.SweepSnapshots(true) {
		logger.Printf("yarım kalan veritabanı kopyası silindi: %s", x)
	}
	startRecovery(jobs, data, audit, tempDBs || data.TempLeft(), logger)
	go housekeeping(ctx, jobs, data, logger)
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
		Audit:       audit,
		Sampler:     sampler,
		Sites:       ops,
		Data:        data,
		Jobs:        jobs,
		AllowedUIDs: map[uint32]bool{0: true, uint32(uid): true},
		Log:         logger,
	}
	logger.Printf("agent listening on %s (uid %d allowed)", cfg.Socket, uid)
	return s.Serve(ctx, l)
}

// startRecovery puts back what an agent stop interrupted, as a job (so it shows in the
// panel and no other work starts meanwhile): temporary databases of a check are
// dropped, restores cut short are rolled back to their safety copy.
func startRecovery(jobs *Jobs, data *DataOps, audit *Audit, tempDBs bool, logger *log.Logger) {
	recs := data.Recoveries()
	if len(recs) == 0 && !tempDBs {
		return
	}
	var targets []string
	for _, r := range recs {
		targets = append(targets, r.Site)
	}
	target := strings.Join(targets, ", ")
	if target == "" {
		target = "geçici veritabanları"
	}
	_, err := jobs.Start(JobSpec{Kind: "db.recover", Target: target, Actor: Actor{User: "footops", Via: "agent"}},
		func(ctx context.Context, t *Task) error {
			if err := data.WaitMongo(ctx); err != nil {
				// Records are tried once: an old one must never be replayed over newer data.
				for _, r := range recs {
					t.Step(fmt.Sprintf("✗ %s: kurtarılamadı; site durmuş, veritabanı yarım olabilir. Veritabanı kopyaları → %s → Geri yükle",
						r.Site, orDash(r.Snapshot)))
					data.removeRecovery(r.Job)
				}
				return err
			}
			if tempDBs {
				n, err := data.DropTempDatabases(ctx)
				if err != nil {
					t.Step("⚠ Geçici veritabanları silinemedi: " + err.Error())
				} else {
					t.Step("✓ Yarıda kalan kontrolün geçici veritabanı silindi (" + n + ")")
				}
			}
			var errs []error
			for _, r := range recs {
				if err := data.Recover(ctx, t, r); err != nil {
					t.Step("✗ " + err.Error())
					errs = append(errs, err)
				}
			}
			return errors.Join(errs...)
		}, func(j Job) {
			_ = audit.Record(AuditEntry{Actor: j.Actor, Action: j.Kind, Target: j.Target, OK: j.Err == "", Detail: j.Err})
			logger.Printf("kurtarma bitti: %s %s", j.Target, map[bool]string{true: "başarılı", false: "HATA: " + j.Err}[j.Err == ""])
		})
	if err != nil {
		logger.Printf("kurtarma başlatılamadı: %v", err)
	}
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

// housekeeping removes what data work leaves behind: staging older than a day, copies
// past their keep time. It runs hourly and skips a round while a job runs.
func housekeeping(ctx context.Context, jobs *Jobs, data *DataOps, logger *log.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if _, busy := jobs.Running(); !busy {
			for _, x := range data.Staging.Sweep() {
				logger.Printf("eski geçici klasör silindi: %s", x)
			}
			for _, x := range data.SweepSnapshots(false) {
				logger.Printf("süresi dolan veritabanı kopyası silindi: %s", x)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// removeTools removes tool containers left running by an agent that stopped mid-job.
func removeTools(ctx context.Context, d *docker.Client, r run.Runner, logger *log.Logger) {
	list, err := d.Containers(ctx)
	if err != nil {
		logger.Printf("container listesi alınamadı: %v", err)
		return
	}
	for _, c := range list {
		if name := c.Name(); strings.HasPrefix(name, toolPrefix) && docker.ValidName(name) {
			if _, err := r.Run(ctx, "docker", "rm", "--force", name); err != nil {
				logger.Printf("%s silinemedi: %v", name, err)
			} else {
				logger.Printf("yarıda kalan araç container'ı silindi: %s", name)
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
	id, err := s.Jobs.Start(JobSpec{Kind: "site.create", Target: spec.Name, Actor: req.Actor}, func(ctx context.Context, t *Task) error {
		return s.Sites.Create(ctx, spec, req.Env, t.Step)
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
		id, err := s.Jobs.Start(JobSpec{Kind: kind, Target: name, Actor: req.Actor}, func(ctx context.Context, t *Task) error {
			if kind == "site.remove" {
				return s.Sites.Remove(ctx, name, t.Step)
			}
			return s.Sites.Redeploy(ctx, name, t.Step)
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
	id, err := s.Jobs.Start(JobSpec{Kind: "site.update", Target: name, Actor: req.Actor}, func(ctx context.Context, t *Task) error {
		return s.Sites.Update(ctx, name, req.Update, t.Step)
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
		writeErr(w, http.StatusNotFound, "işlem bulunamadı (çok eski olabilir)")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	id := r.PathValue("id")
	j, _ := s.Jobs.Get(id)
	err := s.Jobs.Cancel(id)
	if errors.Is(err, ErrJobNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "job.cancel", Target: j.Kind + " " + j.Target}, err)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- database copies ----

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	list, err := s.Data.Snapshots(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) takeSnapshot(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req RestartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	spec, err := s.Sites.Store.Get(name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if !spec.Database {
		writeErr(w, http.StatusBadRequest, "bu sitenin veritabanı yok")
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "db.snapshot", Target: name, Actor: req.Actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := s.Data.Snapshot(ctx, t, name, "manual", req.Actor)
			return err
		}, func(j Job) {
			detail := j.Err
			if j.Err == "" {
				detail = fmt.Sprintf("kopya %s, %s", j.Result, humanBytes(j.Bytes))
			}
			s.recordJobDetail(j, detail)
		})
	s.jobResponse(w, id, err)
}

// downloadSnapshot serves a copy (see serveDownload).
func (s *Server) downloadSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, path, err := s.Data.SnapshotFile(r.PathValue("name"), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	s.serveDownload(w, r, path, download{Name: snap.Site + "-" + snap.ID + ".archive.gz", Type: "application/gzip",
		SHA256: snap.SHA256, Action: "db.snapshot.download", Target: snap.Site + " " + snap.ID})
}

// resumes reports whether a Range header only asks for the rest of a file from a
// positive offset ("bytes=1048576-"): the continuation of a download already audited.
// Anything else (no range, "bytes=0-", suffix or multiple ranges) is audited.
func resumes(rg string) bool {
	from, ok := strings.CutPrefix(rg, "bytes=")
	if !ok {
		return false
	}
	start, rest, ok := strings.Cut(from, "-")
	n, err := strconv.ParseInt(start, 10, 64)
	return ok && err == nil && n > 0 && !strings.Contains(rest, ",")
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
