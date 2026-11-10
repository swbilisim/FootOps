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
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/history"
	"github.com/swbilisim/FootOps/internal/release"
	"github.com/swbilisim/FootOps/internal/remote"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
	"github.com/swbilisim/FootOps/internal/update"
	"github.com/swbilisim/FootOps/internal/version"
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
	Backups *Backups
	// Notify sends the alerts to Telegram (nil: off); StatusFile writes the status
	// file for the outside watcher (nil: no backups).
	Notify     *Notifier
	StatusFile *StatusWriter
	// Expiry keeps the sites' end dates and parks them (Faz P1b).
	Expiry *Expiry
	// Recovery brings the server back from its backups; Rehearsal is the state it
	// leaves the server in (nil in tests that do not need them).
	Recovery  *Recovery
	Rehearsal *Rehearsal
	// Registries are the image registry logins docker pulls with.
	Registries *Registries
	// Doctor runs the server check (Faz U1), Updater the updates (Faz U3); nil in tests
	// that do not need them.
	Doctor  *Doctor
	Updater *Updater
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
	mux.HandleFunc("GET /v1/groups", s.listGroups)
	mux.HandleFunc("POST /v1/sites", s.createSite)
	mux.HandleFunc("POST /v1/sites/check", s.checkSite)
	mux.HandleFunc("POST /v1/sites/{name}/move", s.moveSite)
	mux.HandleFunc("GET /v1/sites/{name}/dns", s.siteDNS)
	mux.HandleFunc("GET /v1/removed", s.listRemoved)
	mux.HandleFunc("POST /v1/removed/{id}/purge", s.purgeRemoved)
	mux.HandleFunc("POST /v1/images/check", s.checkImage)
	mux.HandleFunc("GET /v1/registries", s.listRegistries)
	mux.HandleFunc("POST /v1/registries", s.loginRegistry)
	mux.HandleFunc("POST /v1/registries/remove", s.logoutRegistry)
	mux.HandleFunc("POST /v1/sites/{name}/redeploy", s.siteJob("site.redeploy"))
	mux.HandleFunc("POST /v1/sites/{name}/update", s.updateSite)
	mux.HandleFunc("POST /v1/sites/{name}/remove", s.siteJob("site.remove"))
	mux.HandleFunc("POST /v1/sites/{name}/expiry", s.setExpiry)
	mux.HandleFunc("POST /v1/sites/{name}/park", s.parkSite)
	mux.HandleFunc("POST /v1/sites/{name}/unpark", s.unparkSite)
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
	mux.HandleFunc("GET /v1/sites/{name}/files/backups", s.listFilesBackups)
	mux.HandleFunc("POST /v1/sites/{name}/files/backups/{id}/restore", s.restoreFiles)
	mux.HandleFunc("POST /v1/sites/{name}/exports", s.startExport)
	mux.HandleFunc("GET /v1/sites/{name}/exports", s.listExports)
	mux.HandleFunc("GET /v1/sites/{name}/exports/{id}/download", s.downloadExport)
	mux.HandleFunc("GET /v1/sites/{name}/files", s.listFiles)
	mux.HandleFunc("GET /v1/sites/{name}/files/dirs", s.fileDirs)
	mux.HandleFunc("GET /v1/sites/{name}/files/download", s.downloadFile)
	mux.HandleFunc("POST /v1/sites/{name}/files/zip", s.zipFiles)
	mux.HandleFunc("POST /v1/sites/{name}/files/change", s.changeFiles)
	mux.HandleFunc("POST /v1/sites/{name}/files/place", s.placeUpload)
	mux.HandleFunc("POST /v1/sites/{name}/files/unpack", s.unpackFile)
	mux.HandleFunc("POST /v1/sites/{name}/files/url", s.setDataURL)
	mux.HandleFunc("GET /v1/sites/{name}/trash", s.listTrash)
	mux.HandleFunc("POST /v1/sites/{name}/trash/{id}/restore", s.restoreTrash)
	mux.HandleFunc("POST /v1/sites/{name}/trash/empty", s.emptyTrash)
	mux.HandleFunc("GET /v1/sites/{name}/db", s.dbCollections)
	mux.HandleFunc("GET /v1/sites/{name}/db/find", s.dbFind)
	mux.HandleFunc("GET /v1/sites/{name}/db/doc", s.dbDoc)
	mux.HandleFunc("POST /v1/sites/{name}/db/preview", s.dbPreview)
	mux.HandleFunc("POST /v1/sites/{name}/db/replace", s.dbChange("edit"))
	mux.HandleFunc("POST /v1/sites/{name}/db/delete", s.dbChange("delete"))
	mux.HandleFunc("GET /v1/sites/{name}/db/edits", s.dbEdits)
	mux.HandleFunc("GET /v1/sites/{name}/db/edits/{id}", s.dbEdit)
	mux.HandleFunc("POST /v1/sites/{name}/db/edits/{id}/undo", s.dbUndo)
	mux.HandleFunc("GET /v1/sources", s.listSources)
	mux.HandleFunc("POST /v1/sources", s.createSource)
	mux.HandleFunc("POST /v1/sources/{id}/test", s.testSource)
	mux.HandleFunc("POST /v1/sources/{id}/reset-hostkey", s.resetHostKey)
	mux.HandleFunc("POST /v1/sources/{id}/remove", s.removeSource)
	mux.HandleFunc("GET /v1/sites/{name}/migration", s.getMigration)
	mux.HandleFunc("POST /v1/sites/{name}/migration", s.setMigration)
	mux.HandleFunc("POST /v1/sites/{name}/migration/forget", s.forgetMigration)
	mux.HandleFunc("POST /v1/sites/{name}/migration/discover", s.migrationJob("migrate.discover"))
	mux.HandleFunc("POST /v1/sites/{name}/migration/scan", s.migrationJob("migrate.scan"))
	mux.HandleFunc("POST /v1/sites/{name}/migration/copy", s.migrationJob("migrate.copy"))
	mux.HandleFunc("GET /v1/backup", s.backupStatus)
	mux.HandleFunc("POST /v1/backup/setup", s.backupSetup)
	mux.HandleFunc("GET /v1/backup/key", s.backupKey)
	mux.HandleFunc("POST /v1/backup/confirm", s.backupConfirm)
	mux.HandleFunc("GET /v1/backup/settings", s.backupSettings)
	mux.HandleFunc("POST /v1/backup/settings", s.setBackupSettings)
	mux.HandleFunc("POST /v1/backup/run", s.backupRun)
	mux.HandleFunc("POST /v1/backup/trial", s.backupTrial)
	mux.HandleFunc("GET /v1/backup/runs", s.backupRuns)
	mux.HandleFunc("POST /v1/backup/list", s.backupList)
	mux.HandleFunc("GET /v1/sites/{name}/backups", s.siteBackups)
	mux.HandleFunc("GET /v1/sites/{name}/backups/{run}", s.siteBackup)
	mux.HandleFunc("GET /v1/sites/{name}/backups/{run}/compare", s.backupCompare)
	mux.HandleFunc("POST /v1/sites/{name}/backups/{run}/open", s.backupOpen)
	mux.HandleFunc("POST /v1/sites/{name}/backups/{run}/restore", s.backupRestore)
	mux.HandleFunc("POST /v1/sites/{name}/backups/{run}/export", s.backupExport)
	mux.HandleFunc("GET /v1/sites/{name}/backups/{run}/files", s.backupFiles)
	mux.HandleFunc("GET /v1/sites/{name}/backups/{run}/files/download", s.backupFileDownload)
	mux.HandleFunc("POST /v1/sites/{name}/views/{id}/close", s.viewClose)
	mux.HandleFunc("GET /v1/sites/{name}/views/{id}/db", s.inView(s.dbCollections))
	mux.HandleFunc("GET /v1/sites/{name}/views/{id}/db/find", s.inView(s.dbFind))
	mux.HandleFunc("GET /v1/sites/{name}/views/{id}/db/doc", s.inView(s.dbDoc))
	mux.HandleFunc("GET /v1/recover", s.recoverStatus)
	mux.HandleFunc("POST /v1/recover", s.recoverStart)
	mux.HandleFunc("POST /v1/rehearsal/end", s.rehearsalEnd)
	mux.HandleFunc("GET /v1/notify", s.notifyStatus)
	mux.HandleFunc("POST /v1/notify/telegram", s.notifyStart)
	mux.HandleFunc("POST /v1/notify/telegram/link", s.notifyLink)
	mux.HandleFunc("POST /v1/notify/telegram/cancel", s.notifyCancel)
	mux.HandleFunc("POST /v1/notify/telegram/remove", s.notifyRemove)
	mux.HandleFunc("POST /v1/notify/test", s.notifyTest)
	mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	mux.HandleFunc("GET /v1/certs", s.listCerts)
	mux.HandleFunc("POST /v1/certs", s.installCert)
	mux.HandleFunc("POST /v1/certs/{id}/remove", s.removeCert)
	mux.HandleFunc("GET /v1/server", s.serverInfo)
	mux.HandleFunc("GET /v1/doctor", s.doctorStatus)
	mux.HandleFunc("POST /v1/doctor/run", s.doctorRun)
	mux.HandleFunc("GET /v1/update", s.updateStatus)
	mux.HandleFunc("POST /v1/update/check", s.updateCheck)
	mux.HandleFunc("POST /v1/update/apply", s.updateApply)
	mux.HandleFunc("POST /v1/update/back", s.updateBack)
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
	// A parked site's container stays stopped until the site is brought back.
	if s.Sites != nil && s.Sites.ParkedContainers()[name] {
		s.record(entry, errParked)
		writeErr(w, http.StatusConflict, errParked.Error())
		return
	}
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
	// SourcesDir keeps the servers sites are moved from (their keys); MigrationsDir each
	// site's move (F4).
	SourcesDir    string
	MigrationsDir string
	// DBEditsDir keeps the previous versions of documents changed from the panel (F5).
	DBEditsDir string
	PanelUser  string
	PanelHost  string // refused as a site domain
	// DockerConfig is docker's DOCKER_CONFIG (registry logins); RegistriesPath keeps
	// when each login was saved.
	DockerConfig   string
	RegistriesPath string
	// Backups (Faz Y): settings and records, the repository's secrets, the restic
	// binary, restic's cache; ServerCode is restic's host name. BackupDir empty: off.
	BackupDir    string
	BackupSecret string
	ToolsDir     string
	ResticCache  string
	ServerCode   string
	// NotifySecret keeps the Telegram bot and chat, NotifyState what the chat was told;
	// NotifySecret empty: no notifications. NotifyAPI replaces Telegram's address (the
	// integration test's stand-in; empty: Telegram).
	NotifySecret string
	NotifyState  string
	NotifyAPI    string
	// RehearsalPath marks a server recovered from its backups and not yet live;
	// RecoverDir keeps the recovery's record and what go-live puts in place (Faz Y5).
	RehearsalPath string
	RecoverDir    string
	// ExpiryState keeps what the chat was told of the sites' end dates (Faz P1b);
	// empty: memory only.
	ExpiryState string
	// DoctorPath keeps the last server check (Faz U1); empty: memory only.
	DoctorPath string
	// UpdateDir keeps the releases and the updates' records (Faz U3); empty: no
	// updates. ReleaseURL replaces GitHub's releases (the integration test's stand-in).
	UpdateDir  string
	ReleaseURL string
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
	ops := &SiteOps{Store: sites.Store{Root: "/"}, Docker: d, Run: run.Exec{}, Kill: run.Exec{KillOnCancel: true}, PanelHost: cfg.PanelHost}
	data := &DataOps{Store: ops.Store, Docker: d, Run: run.Exec{KillOnCancel: true}, Stream: run.StreamExec{},
		Staging: staging.New(cfg.StagingDir, staging.DefaultMax), SnapshotDir: cfg.SnapshotDir,
		RecoveryDir: cfg.RecoveryDir, Sites: ops, MigrationsDir: cfg.MigrationsDir, DBEditsDir: cfg.DBEditsDir}
	// Made at start so the web process, which starts after the agent, has them hidden
	// (InaccessiblePaths only covers paths that exist when a service starts).
	for _, dir := range []string{cfg.DBEditsDir, cfg.BackupDir, cfg.ResticCache, cfg.RecoverDir, cfg.UpdateDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Printf("%s oluşturulamadı: %v", dir, err)
		}
	}
	host, _ := os.Hostname()
	data.Sources = &remote.Store{Dir: cfg.SourcesDir, Run: run.Exec{KillOnCancel: true}, Stream: run.StreamExec{}, Name: host}
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
			if job.Kind == "server.update" {
				continue // the update restarts the agent: the Updater records how it went
			}
			// These restore into a temporary database (the trial restore too).
			tempDBs = tempDBs || job.Kind == "import.check" || job.Kind == "migrate.copy" || job.Kind == "backup.trial"
			_ = audit.Record(AuditEntry{Actor: job.Actor, Action: job.Kind, Target: job.Target, Detail: job.Err})
			// Its staging may hold a connection file with a password (rule K5), a move's
			// staging the site's database as fetched from the source.
			if err := data.Staging.Remove(job.ID); err != nil {
				logger.Printf("geçici klasör silinemedi: %v", err)
			}
			if job.Kind == "site.export" || job.Kind == "backup.export" {
				_ = data.Staging.Remove(exportPrefix + job.ID) // a half-written archive
			}
			if job.Kind == "migrate.copy" {
				_ = data.Staging.Remove(migratePrefix + job.ID)
				// Its ssh and file helper run in scopes of their own and outlive the agent:
				// stopped now, they cannot keep writing into a data.new the recovery removes.
				stopScopes(ctx, run.Exec{}, job.ID, logger)
			}
		}
	}
	// No job runs yet, so any tool container still around was cut off by a restart, and
	// any copy without a manifest is half-written.
	removeTools(ctx, d, run.Exec{}, logger)
	for _, x := range data.SweepSnapshots(true) {
		logger.Printf("yarım kalan veritabanı kopyası silindi: %s", x)
	}
	var rehearsal *Rehearsal
	if cfg.RehearsalPath != "" {
		rehearsal = &Rehearsal{Path: cfg.RehearsalPath, Run: run.Exec{KillOnCancel: true}, Log: logger}
		// Before any site gets a chance to reach out: Docker may have rebuilt its chains.
		rehearsal.Ensure(ctx, true)
	}
	var backups *Backups
	if cfg.BackupDir != "" {
		code := cfg.ServerCode
		if code == "" {
			code = host
		}
		backups = &Backups{Data: data, Jobs: jobs, Audit: audit, Dir: cfg.BackupDir, SecretPath: cfg.BackupSecret,
			ToolsDir: cfg.ToolsDir, CacheDir: cfg.ResticCache, Server: code, Log: logger, Rehearsal: rehearsal}
		// A site a cut-off backup froze is unpaused before anything else.
		backups.Recover(ctx)
		backups.StartViews(ctx)
	}
	startRecovery(jobs, data, audit, tempDBs || data.TempLeft(), logger)
	go housekeeping(ctx, jobs, data, backups, logger)
	if backups != nil {
		go backups.Schedule(ctx)
	}
	regs := &Registries{ConfigDir: cfg.DockerConfig, MetaPath: cfg.RegistriesPath, Run: run.Exec{},
		GitHubAPI: "https://api.github.com", HTTP: &http.Client{Timeout: 10 * time.Second}}
	sampler := NewSampler(d, nil)
	sampler.cgroupRoot = run.CgroupRoot // between lists, what runs is read from the cgroup tree
	sampler.Alerts = NewAlerter()
	sampler.Alerts.Certs = ops.Store.Certs
	sampler.Alerts.Registries = regs.List
	if backups != nil {
		sampler.Alerts.Backups = backups.Alerts
	}
	var notify *Notifier
	if cfg.NotifySecret != "" {
		code := cfg.ServerCode
		if code == "" {
			code = host
		}
		notify = &Notifier{SecretPath: cfg.NotifySecret, StatePath: cfg.NotifyState, Server: code, PanelHost: cfg.PanelHost,
			API: cfg.NotifyAPI, Active: sampler.Alerts.Active, Log: logger, Rehearsal: rehearsal,
			Report: NewReport(sampler, ops.Store.List, backups), Audit: audit}
		sampler.Alerts.Extra = append(sampler.Alerts.Extra, notify.Alerts)
	}
	doc := &Doctor{Path: cfg.DoctorPath, Log: logger}
	sampler.Alerts.Extra = append(sampler.Alerts.Extra, doc.Alerts)
	var statusFile *StatusWriter
	if backups != nil {
		statusFile = &StatusWriter{Backups: backups, Sites: ops.Store.List, Server: backups.Server, Log: logger,
			Overview: sampler.Latest, Doctor: doc.StatusPart}
		sampler.Alerts.Extra = append(sampler.Alerts.Extra, statusFile.Alerts)
		doc.Changed = statusFile.Kick
	}
	go doc.Run(ctx)
	var upd *Updater
	if cfg.UpdateDir != "" {
		base := cfg.ReleaseURL
		if base == "" {
			base = update.DefaultBase
		}
		code := cfg.ServerCode
		if code == "" {
			code = host
		}
		upd = &Updater{Dir: cfg.UpdateDir, Installed: update.InstalledBin, Source: update.Source{Base: base, HTTP: &http.Client{}, Keys: release.Trusted()},
			Running: version.Version, Arch: runtime.GOARCH, Jobs: jobs, Audit: audit, Notify: notify, Server: code, Log: logger,
			Plan: systemdPlan, Verify: verifyBinary, Start: startUnit}
		sampler.Alerts.Extra = append(sampler.Alerts.Extra, upd.Alerts)
		if statusFile != nil {
			statusFile.Update = upd.StatusPart
		}
		if notify != nil && notify.Report != nil {
			// /durum says a newer release is ready (set before the notifier runs).
			report := notify.Report
			notify.Report = func(ctx context.Context) ServerReport {
				r := report(ctx)
				if p := upd.StatusPart(); p != nil {
					r.Available, r.AvailableReady = p.Available, p.Ready
				}
				return r
			}
		}
		go upd.Run(ctx)
	}
	// End dates and parking (Faz P1b): the panel and the chat park and bring back sites
	// through it; a parked site's stopped container is no alert.
	expiry := &Expiry{Sites: ops, Jobs: jobs, Backups: backups, Audit: audit, StatePath: cfg.ExpiryState, Log: logger,
		Changed: func() {
			sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sampler.Sample(sctx)
			if statusFile != nil {
				statusFile.Kick()
			}
		}}
	sampler.Alerts.Extra = append(sampler.Alerts.Extra, expiry.Alerts)
	sampler.Alerts.Parked = ops.ParkedContainers
	var readDone chan struct{}
	if notify != nil {
		expiry.Notify, notify.Sites, notify.OnLink = notify, expiry, expiry.Kick
		go notify.Run(ctx)
		readDone = make(chan struct{})
		go func() {
			notify.Read(ctx)
			close(readDone)
		}()
	}
	if statusFile != nil {
		go statusFile.Run(ctx)
	}
	go expiry.Run(ctx)
	if rehearsal != nil {
		// Its own ticker: Reconcile waits behind a site being made (a recovery's restore).
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					rehearsal.Ensure(ctx, false)
				}
			}
		}()
	}
	var recovery *Recovery
	if backups != nil && rehearsal != nil && cfg.RecoverDir != "" {
		recovery = &Recovery{Backups: backups, Ops: ops, Rehearsal: rehearsal, Dir: cfg.RecoverDir,
			AuthPath: config.AuthPath, DockerConfig: cfg.DockerConfig, RegistriesPath: cfg.RegistriesPath,
			SourcesDir: cfg.SourcesDir, PanelUID: int(uid), PanelGID: gid}
		sampler.Alerts.Extra = append(sampler.Alerts.Extra, rehearsal.Alerts)
	}
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
	// inspects one network per site. Once per start, as soon as Caddy runs, the site
	// blocks are brought up to this version's rendering (a no-op unless FootOps changed),
	// and parked sites' containers left running are stopped.
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		// parks paces FinishParks as resync paces SyncCaddy: once per start, again
		// later after a failure.
		var resync, parks caddyResync
		for {
			for _, err := range ops.Reconcile(ctx) {
				logger.Printf("reconcile: %v", err)
			}
			if now := time.Now(); resync.due(now) {
				ran, err := ops.SyncCaddy(ctx)
				if err != nil {
					logger.Printf("Caddy site blokları güncellenemedi (eski halleri yerinde, yeniden denenecek): %v", err)
				}
				resync.result(ran, err, now)
			}
			if now := time.Now(); parks.due(now) {
				err := ops.FinishParks(ctx, logger.Printf)
				if err != nil {
					logger.Printf("parktaki sitelerin container'ları denetlenemedi (yeniden denenecek): %v", err)
				}
				parks.result(true, err, now)
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
		Backups:     backups,
		Notify:      notify,
		StatusFile:  statusFile,
		Expiry:      expiry,
		Recovery:    recovery,
		Rehearsal:   rehearsal,
		Registries:  regs,
		Doctor:      doc,
		Updater:     upd,
		AllowedUIDs: map[uint32]bool{0: true, uint32(uid): true},
		Log:         logger,
	}
	logger.Printf("agent listening on %s (uid %d allowed)", cfg.Socket, uid)
	err = s.Serve(ctx, l)
	if readDone != nil {
		// The reader tells Telegram what it handled before the process goes (a restart
		// would answer it again).
		select {
		case <-readDone:
		case <-time.After(4 * time.Second):
		}
	}
	if backups != nil {
		// The jobs die with the process: a site paused for its copy must not stay so.
		stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		backups.Recover(stop)
		cancel()
	}
	return err
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
			// MongoDB is only needed for databases: files are put back without it.
			needMongo := tempDBs
			for _, r := range recs {
				needMongo = needMongo || r.DB
			}
			mongoErr := error(nil)
			if needMongo {
				mongoErr = data.WaitMongo(ctx)
			}
			if tempDBs && mongoErr == nil {
				n, err := data.DropTempDatabases(ctx)
				if err != nil {
					t.Step("⚠ Geçici veritabanları silinemedi: " + err.Error())
				} else {
					t.Step("✓ Yarıda kalan kontrolün geçici veritabanı silindi (" + n + ")")
				}
			}
			var errs []error
			for _, r := range recs {
				if r.DB && mongoErr != nil {
					// The files can still go back; the site stays stopped (its database may be
					// half-written). Records are tried once: an old one must never be replayed
					// over newer data later.
					if r.Files != nil {
						filesOnly := r
						filesOnly.DB, filesOnly.Running = false, false
						if err := data.Recover(ctx, t, filesOnly); err != nil {
							t.Step("✗ " + err.Error())
						}
					}
					t.Step(fmt.Sprintf("✗ %s: MongoDB cevap vermediği için veritabanı kurtarılamadı; site durmuş, veritabanı yarım olabilir. Veritabanı kopyaları → %s → Geri yükle",
						r.Site, orDash(copyLabel(r.Snapshot))))
					data.removeRecovery(r.Job)
					errs = append(errs, mongoErr)
					continue
				}
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
// past their keep time, old backup records. It runs hourly and skips a round while a
// job runs.
func housekeeping(ctx context.Context, jobs *Jobs, data *DataOps, backups *Backups, logger *log.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if !jobs.Busy() {
			for _, x := range data.Staging.Sweep() {
				logger.Printf("eski geçici klasör silindi: %s", x)
			}
			for _, x := range data.SweepSnapshots(false) {
				logger.Printf("süresi dolan veritabanı kopyası silindi: %s", x)
			}
			for _, x := range data.SweepFiles() {
				logger.Printf("süresi dolan dosya kopyası silindi: %s", x)
			}
			for _, x := range data.SweepTrash() {
				logger.Printf("çöp kutusundan silindi (süresi doldu): %s", x)
			}
			for _, x := range data.SweepDBEdits() {
				logger.Printf("süresi dolan belge değişikliği kaydı silindi: %s", x)
			}
			if backups != nil {
				if n := len(backups.SweepRuns()); n > 0 {
					logger.Printf("eski yedek kayıtları silindi: %d", n)
				}
				for _, v := range backups.SweepViews() {
					logger.Printf("açılan yedek kapatıldı (süresi doldu): %s", v)
				}
				backups.Recover(ctx)
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

// stopScopes kills what a job left running in systemd scopes (its ssh streams and its
// file helper), after an agent restart.
func stopScopes(ctx context.Context, r run.Runner, job string, logger *log.Logger) {
	out, err := r.Run(ctx, "systemctl", "list-units", "--plain", "--no-legend", "--all", "--type=scope",
		"footops-ssh-"+job+"-*", helperPrefix+job+".scope")
	if err != nil {
		logger.Printf("scope listesi alınamadı: %v", err)
		return
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasSuffix(f[0], ".scope") || !strings.Contains(f[0], job) {
			continue
		}
		if _, err := r.Run(ctx, "systemctl", "kill", "--signal=SIGKILL", f[0]); err == nil {
			logger.Printf("yarıda kalan süreç durduruldu: %s", f[0])
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

// listGroups lists the groups (the folders under /srv) for the forms that name one.
func (s *Server) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.Sites.Store.Groups()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, append([]string{}, groups...))
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

// checkSite validates a new site without creating it, for the review page.
func (s *Server) checkSite(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, spec)
}

// checkImage pulls and inspects an image as a job; the result is an ImageInfo in JSON.
func (s *Server) checkImage(w http.ResponseWriter, r *http.Request) {
	var req CheckImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	image := strings.TrimSpace(req.Image)
	if !sites.ValidImage(image) {
		writeJSON(w, http.StatusUnprocessableEntity, errorResponse{Error: "imaj adı geçersiz",
			Problems: []string{"imaj adı geçersiz (örn. ghcr.io/kullanici/site:main)"}})
		return
	}
	id, err := s.Jobs.Start(JobSpec{Kind: "image.check", Target: image, Actor: req.Actor, Cancellable: true}, func(ctx context.Context, t *Task) error {
		info, err := s.Sites.CheckImage(ctx, image, t)
		if err != nil {
			return err
		}
		b, err := json.Marshal(info)
		if err != nil {
			return err
		}
		t.SetResult(string(b))
		return nil
	}, s.recordJob)
	s.jobResponse(w, id, err)
}

// moveSite puts a site into another group, as a job.
func (s *Server) moveSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req MoveSiteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	cur, next, err := s.Sites.CheckMove(name, req.Group)
	switch {
	case errors.Is(err, sites.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	case req.Confirm != name:
		writeErr(w, http.StatusUnprocessableEntity, "Onay için site adını aynen yaz.")
		return
	}
	for _, rec := range s.Data.Recoveries() {
		if rec.Site == name {
			writeErr(w, http.StatusConflict, "bu sitede yarım kalmış bir geri yükleme var; önce o bitmeli")
			return
		}
	}
	detail := cur.Group + " → " + next.Group
	id, err := s.Jobs.Start(JobSpec{Kind: "site.move", Target: name, Actor: req.Actor}, func(ctx context.Context, t *Task) error {
		return s.Sites.Move(ctx, name, next.Group, t.Step)
	}, func(j Job) {
		if j.Err != "" {
			detail += " — hata: " + j.Err
		}
		s.recordJobDetail(j, detail)
	})
	s.jobResponse(w, id, err)
}

// siteDNS reports where a site's domains point and whether it answers.
func (s *Server) siteDNS(w http.ResponseWriter, r *http.Request) {
	list, err := s.Sites.CheckDNS(r.Context(), r.PathValue("name"))
	if errors.Is(err, sites.ErrNotFound) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---- registry logins ----

func (s *Server) listRegistries(w http.ResponseWriter, r *http.Request) {
	list, err := s.Registries.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// loginRegistry saves a login. The audit log gets the registry and the user, never the
// token.
func (s *Server) loginRegistry(w http.ResponseWriter, r *http.Request) {
	var req RegistryLoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	l, notes, err := s.Registries.Login(r.Context(), req.Host, req.User, req.Token, req.Actor.User)
	// Only what passed validation is recorded: a token pasted as the user name must not
	// end up in the audit log.
	host, user, verr := CheckLogin(req.Host, req.User)
	target := host + " " + user
	if verr != nil {
		target = "(geçersiz giriş bilgisi)"
		if reRegistryHost.MatchString(host) {
			target = host + " (geçersiz kullanıcı adı)"
		}
	}
	s.record(AuditEntry{Actor: req.Actor, Action: "registry.login", Target: target}, err)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, RegistryLoginResponse{Login: l, Notes: notes})
}

func (s *Server) logoutRegistry(w http.ResponseWriter, r *http.Request) {
	var req RegistryLogoutRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "geçersiz istek")
		return
	}
	err := s.Registries.Logout(r.Context(), req.Host)
	s.record(AuditEntry{Actor: req.Actor, Action: "registry.logout", Target: req.Host}, err)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
	if errors.Is(err, errParked) {
		writeErr(w, http.StatusConflict, err.Error())
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
	// The outside watcher learns of a new, changed or removed site (or a repository just
	// set up) at once — after the refresh, so the file has the container's state.
	if s.StatusFile != nil && (strings.HasPrefix(j.Kind, "site.") || j.Kind == "backup.setup") {
		s.StatusFile.Kick()
	}
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
	info := publicAddrs()
	info.FootOps = version.Version
	writeJSON(w, http.StatusOK, info)
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
