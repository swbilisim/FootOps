// Package web is the unprivileged half of the panel: login, dashboard and the forms that
// ask the agent to act. It runs as the "footops" user, listens on loopback only and
// never touches Docker or the filesystem outside its own state directory.
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"io"
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
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/sites"
)

//go:embed templates/*.html static/*
var assets embed.FS

// AgentAPI is what the web process may ask the agent for.
type AgentAPI interface {
	Overview(ctx context.Context) (agent.Overview, error)
	History(ctx context.Context, rng, container string) (agent.History, error)
	Alerts(ctx context.Context) ([]agent.Alert, error)
	Restart(ctx context.Context, name string, a agent.Actor) error
	Logs(ctx context.Context, name string, tail int) ([]string, error)
	RecordAuth(ctx context.Context, e agent.AuditEntry) error
	AuditTail(ctx context.Context, limit int) ([]agent.AuditEntry, error)
	Sites(ctx context.Context) ([]agent.SiteStatus, error)
	Groups(ctx context.Context) ([]string, error)
	CreateSite(ctx context.Context, req agent.CreateSiteRequest) (string, error)
	CheckSite(ctx context.Context, req agent.CreateSiteRequest) (sites.Spec, error)
	CheckImage(ctx context.Context, image string, a agent.Actor) (string, error)
	MoveSite(ctx context.Context, name, group, confirm string, force bool, a agent.Actor) (string, error)
	SiteDNS(ctx context.Context, name string) ([]agent.DomainCheck, error)
	SiteLayer(ctx context.Context, name string) (agent.LayerReport, error)
	Registries(ctx context.Context) ([]agent.RegistryLogin, error)
	RegistryLogin(ctx context.Context, host, user, token string, a agent.Actor) (agent.RegistryLoginResponse, error)
	RegistryLogout(ctx context.Context, host string, a agent.Actor) error
	UpdateSite(ctx context.Context, name string, req agent.UpdateSiteRequest) (string, error)
	SetExpiry(ctx context.Context, name, expires string, was *string, a agent.Actor) error
	ParkSite(ctx context.Context, name string, a agent.Actor) (string, error)
	UnparkSite(ctx context.Context, name, expires string, a agent.Actor) (string, error)
	SiteJob(ctx context.Context, name, verb string, force bool, a agent.Actor) (string, error)
	Job(ctx context.Context, id string) (agent.Job, error)
	CancelJob(ctx context.Context, id string, a agent.Actor) error
	Snapshots(ctx context.Context, site string) ([]agent.Snapshot, error)
	TakeSnapshot(ctx context.Context, site string, a agent.Actor) (string, error)
	DownloadSnapshot(ctx context.Context, site, id string, a agent.Actor, hdr http.Header) (*http.Response, error)
	RestoreSnapshot(ctx context.Context, site, id, confirm string, a agent.Actor) (string, error)
	CreateUpload(ctx context.Context, site, name string, size int64, a agent.Actor) (agent.UploadStatus, error)
	Upload(ctx context.Context, id string) (agent.UploadStatus, error)
	PutChunk(ctx context.Context, id string, n int, sum string, body io.Reader, length int64) (agent.UploadStatus, error)
	FinishUpload(ctx context.Context, id string, a agent.Actor) (agent.UploadStatus, error)
	RemoveUpload(ctx context.Context, id string) error
	SingleUpload(ctx context.Context, site, name string, body io.Reader, a agent.Actor) (agent.UploadStatus, error)
	CheckImport(ctx context.Context, site, upload, sourceDB string, a agent.Actor) (string, error)
	Import(ctx context.Context, site, upload string) (agent.ImportCheck, error)
	ApplyImport(ctx context.Context, site, upload string, opt agent.ApplyOptions, confirm string, a agent.Actor) (string, error)
	FilesBackups(ctx context.Context, site string) ([]agent.FilesBackup, error)
	RestoreFiles(ctx context.Context, site, id, confirm string, a agent.Actor) (string, error)
	Export(ctx context.Context, site string, a agent.Actor) (string, error)
	Exports(ctx context.Context, site string) ([]agent.ExportInfo, error)
	DownloadExport(ctx context.Context, site, id string, a agent.Actor, hdr http.Header) (*http.Response, error)
	Files(ctx context.Context, site, dir string, opt datafs.ListOptions) (datafs.Listing, error)
	FileDirs(ctx context.Context, site string) ([]string, error)
	DownloadFile(ctx context.Context, site, p string, a agent.Actor, hdr http.Header) (*http.Response, error)
	ZipFiles(ctx context.Context, site, dir string, names []string, a agent.Actor) (*http.Response, error)
	ChangeFiles(ctx context.Context, site string, ch agent.FilesChange) (*datafs.TrashEntry, error)
	PlaceUpload(ctx context.Context, site string, req agent.PlaceRequest) (datafs.Placed, error)
	Unpack(ctx context.Context, site string, req agent.UnpackRequest) (string, error)
	Trash(ctx context.Context, site string) ([]datafs.TrashEntry, error)
	RestoreTrash(ctx context.Context, site, id string, a agent.Actor) (datafs.Restored, error)
	EmptyTrash(ctx context.Context, site, confirm string, a agent.Actor) (agent.EmptyTrashResult, error)
	SetDataURL(ctx context.Context, site, u string, a agent.Actor) error
	DBCollections(ctx context.Context, site string) (agent.DBOverview, error)
	DBFind(ctx context.Context, site string, q agent.DBQuery) (agent.DBPage, error)
	DBDoc(ctx context.Context, site, coll, id string) (agent.DBDocument, error)
	DBPreview(ctx context.Context, site string, req agent.DBEditRequest) (agent.DBPreview, error)
	DBReplace(ctx context.Context, site string, req agent.DBEditRequest) (agent.DBEdit, error)
	DBDelete(ctx context.Context, site string, req agent.DBEditRequest) (agent.DBEdit, error)
	DBEdits(ctx context.Context, site string) ([]agent.DBEdit, error)
	DBEdit(ctx context.Context, site, id string) (agent.DBEditView, error)
	DBUndo(ctx context.Context, site, id string, a agent.Actor) (agent.DBEdit, error)
	Certs(ctx context.Context) ([]sites.Cert, error)
	InstallCert(ctx context.Context, certPEM, keyPEM string, a agent.Actor) (sites.Cert, error)
	RemoveCert(ctx context.Context, id string, a agent.Actor) error
	Server(ctx context.Context) (agent.ServerInfo, error)
	Sources(ctx context.Context) ([]agent.SourceView, error)
	CreateSource(ctx context.Context, label, host string, port int, user string, a agent.Actor) (agent.SourceView, error)
	TestSource(ctx context.Context, id string, a agent.Actor) (agent.SourceView, error)
	ResetHostKey(ctx context.Context, id string, a agent.Actor) (agent.SourceView, error)
	RemoveSource(ctx context.Context, id string, cleanup bool, a agent.Actor) error
	Migration(ctx context.Context, site string) (agent.MigrationView, error)
	SetMigration(ctx context.Context, site string, ch agent.MigrationChoice, a agent.Actor) (agent.Migration, error)
	ForgetMigration(ctx context.Context, site string, a agent.Actor) error
	MigrationJob(ctx context.Context, site, verb, confirm string, a agent.Actor) (string, error)
	BackupStatus(ctx context.Context) (agent.BackupStatus, error)
	BackupSetup(ctx context.Context, req agent.BackupSetupRequest) (string, error)
	BackupKey(ctx context.Context, a agent.Actor) (string, error)
	BackupConfirm(ctx context.Context, a agent.Actor) error
	BackupSettings(ctx context.Context) (agent.BackupSettings, error)
	SetBackupSettings(ctx context.Context, s agent.BackupSettings, a agent.Actor) error
	BackupRun(ctx context.Context, site string, a agent.Actor) (string, error)
	BackupTrial(ctx context.Context, site string, a agent.Actor) (string, error)
	BackupRuns(ctx context.Context, site string, limit int) ([]agent.BackupRun, error)
	ListBackups(ctx context.Context, site string, a agent.Actor) (string, error)
	SiteBackups(ctx context.Context, site string) (agent.BackupList, error)
	SiteBackup(ctx context.Context, site, run string) (agent.BackupDetail, error)
	BackupCompare(ctx context.Context, site, run string) (agent.BackupCompare, error)
	OpenBackup(ctx context.Context, site, run string, a agent.Actor) (string, error)
	CloseView(ctx context.Context, site, id string, a agent.Actor) error
	RestoreBackup(ctx context.Context, site string, req agent.BackupRestore, confirm string, a agent.Actor) (string, error)
	ExportBackup(ctx context.Context, site string, req agent.BackupExport, a agent.Actor) (string, error)
	BackupFiles(ctx context.Context, site, run, dir string) (agent.BackupFolder, error)
	DownloadBackupFile(ctx context.Context, site, run, p string, a agent.Actor) (*http.Response, error)
	ViewCollections(ctx context.Context, site, view string) (agent.DBOverview, error)
	ViewFind(ctx context.Context, site, view string, q agent.DBQuery) (agent.DBPage, error)
	ViewDoc(ctx context.Context, site, view, coll, id string) (agent.DBDocument, error)
	Removed(ctx context.Context, detail bool) ([]agent.RemovedSite, error)
	PurgeRemoved(ctx context.Context, id, confirm string, a agent.Actor) (string, error)
	NotifyStatus(ctx context.Context) (agent.NotifyStatus, error)
	NotifyStart(ctx context.Context, token string, keep bool, a agent.Actor) (agent.NotifyLink, error)
	NotifyLink(ctx context.Context, a agent.Actor) (agent.NotifyStatus, error)
	NotifyCancel(ctx context.Context, a agent.Actor) error
	NotifyTest(ctx context.Context, a agent.Actor) error
	NotifyRemove(ctx context.Context, a agent.Actor) error
	DoctorStatus(ctx context.Context) (agent.DoctorStatus, error)
	DoctorRun(ctx context.Context, a agent.Actor) error
	UpdateStatus(ctx context.Context, brief bool) (agent.UpdateStatus, error)
	UpdateCheck(ctx context.Context, a agent.Actor) error
	UpdateApply(ctx context.Context, version string, a agent.Actor) (string, error)
	UpdateBack(ctx context.Context, a agent.Actor) (string, error)
	PruneImages(ctx context.Context, a agent.Actor) (string, error)
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
	pending  *pendingStore // new sites between the form and their review
	ipLimit  *limiter      // login attempts per client IP
	allLimit *limiter      // login attempts overall (distributed guessing)
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
		pending:  newPendingStore(now),
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
	mux.Handle("GET /containers/{name}", s.requireSession(s.container))
	mux.Handle("GET /containers/{name}/logs", s.requireSession(s.logs))
	mux.Handle("GET /containers/{name}/restart", s.requireSession(s.restartConfirm))
	mux.Handle("POST /containers/{name}/restart", s.requireSession(s.restart))
	mux.Handle("GET /server/check", s.requireSession(s.checkPage))
	mux.Handle("POST /server/check", s.requireSession(s.checkRun))
	mux.Handle("POST /server/images/prune", s.requireSession(s.imagesPrune))
	mux.Handle("GET /server/update", s.requireSession(s.updatePage))
	mux.Handle("POST /server/update/check", s.requireSession(s.updateCheck))
	mux.Handle("POST /server/update/apply", s.requireSession(s.updateApply))
	mux.Handle("POST /server/update/back", s.requireSession(s.updateBack))
	mux.Handle("GET /audit", s.requireSession(s.audit))
	mux.Handle("GET /sites", s.requireSession(s.sitesList))
	mux.Handle("GET /sites/new", s.requireSession(s.siteNew))
	mux.Handle("POST /sites/new", s.requireSession(s.siteNewImage))
	mux.Handle("GET /sites/new/form", s.requireSession(s.siteNewForm))
	mux.Handle("POST /sites/new/form", s.requireSession(s.siteNewReview))
	mux.Handle("GET /sites/new/project", s.requireSession(s.projectPage))
	mux.Handle("GET /sites/new/project/zip", s.requireSession(s.projectZip))
	mux.Handle("GET /sites/new/review/{id}", s.requireSession(s.siteNewReviewPage))
	mux.Handle("POST /sites/new/review/{id}", s.requireSession(s.siteNewCreate))
	mux.Handle("GET /sites/{name}/move", s.requireSession(s.siteMoveForm))
	mux.Handle("POST /sites/{name}/move", s.requireSession(s.siteMove))
	mux.Handle("GET /sites/{name}/dns", s.requireSession(s.siteDNS))
	mux.Handle("GET /registries", s.requireSession(s.registries))
	mux.Handle("POST /registries", s.requireSession(s.registryLogin))
	mux.Handle("GET /registries/remove", s.requireSession(s.registryRemoveConfirm))
	mux.Handle("POST /registries/remove", s.requireSession(s.registryRemove))
	mux.Handle("GET /sites/{name}", s.requireSession(s.site))
	mux.Handle("GET /sites/{name}/edit", s.requireSession(s.siteEdit))
	mux.Handle("POST /sites/{name}/edit", s.requireSession(s.siteUpdate))
	mux.Handle("GET /sites/{name}/redeploy", s.requireSession(s.siteConfirm("redeploy")))
	mux.Handle("POST /sites/{name}/redeploy", s.requireSession(s.siteAction("redeploy")))
	mux.Handle("GET /sites/{name}/remove", s.requireSession(s.siteConfirm("remove")))
	mux.Handle("POST /sites/{name}/remove", s.requireSession(s.siteAction("remove")))
	mux.Handle("POST /sites/{name}/expiry", s.requireSession(s.siteExpiry))
	mux.Handle("GET /sites/{name}/park", s.requireSession(s.siteConfirm("park")))
	mux.Handle("POST /sites/{name}/park", s.requireSession(s.sitePark))
	mux.Handle("GET /sites/{name}/unpark", s.requireSession(s.siteUnparkForm))
	mux.Handle("POST /sites/{name}/unpark", s.requireSession(s.siteUnpark))
	mux.Handle("POST /sites/{name}/snapshots", s.requireSession(s.snapshotTake))
	mux.Handle("GET /sites/{name}/snapshots/{id}/download", s.requireSession(s.snapshotDownload))
	mux.Handle("GET /sites/{name}/snapshots/{id}/restore", s.requireSession(s.restoreConfirm))
	mux.Handle("POST /sites/{name}/snapshots/{id}/restore", s.requireSession(s.restoreSnapshot))
	mux.Handle("GET /sites/{name}/import", s.requireSession(s.importPage))
	mux.Handle("POST /sites/{name}/import/form", s.requireStreamSession(s.importForm))
	mux.Handle("GET /sites/{name}/import/{id}", s.requireSession(s.importPreview))
	mux.Handle("POST /sites/{name}/import/{id}/check", s.requireSession(s.importRecheck))
	mux.Handle("POST /sites/{name}/import/{id}/apply", s.requireSession(s.importApply))
	mux.Handle("POST /sites/{name}/uploads", s.requireSession(s.uploadCreate))
	mux.Handle("GET /uploads/{id}", s.requireSession(s.uploadStatus))
	mux.Handle("PUT /uploads/{id}/{n}", s.requireSession(s.uploadChunk))
	mux.Handle("POST /uploads/{id}/finish", s.requireSession(s.uploadFinish))
	mux.Handle("POST /uploads/{id}/cancel", s.requireSession(s.uploadCancel))
	mux.Handle("GET /sites/{name}/files/{id}/restore", s.requireSession(s.filesRestoreConfirm))
	mux.Handle("POST /sites/{name}/files/{id}/restore", s.requireSession(s.filesRestore))
	mux.Handle("POST /sites/{name}/exports", s.requireSession(s.exportStart))
	mux.Handle("GET /sites/{name}/exports/{id}/download", s.requireSession(s.exportDownload))
	mux.Handle("GET /sites/{name}/files", s.requireSession(s.files))
	mux.Handle("GET /sites/{name}/files/download", s.requireSession(s.filesDownload))
	mux.Handle("GET /sites/{name}/files/zip", s.requireSession(s.filesZip))
	mux.Handle("POST /sites/{name}/files/act", s.requireSession(s.filesAct))
	mux.Handle("POST /sites/{name}/files/mkdir", s.requireSession(s.filesMkdir))
	mux.Handle("GET /sites/{name}/files/rename", s.requireSession(s.filesRenameForm))
	mux.Handle("POST /sites/{name}/files/rename", s.requireSession(s.filesRename))
	mux.Handle("POST /sites/{name}/files/move", s.requireSession(s.filesMove))
	mux.Handle("POST /sites/{name}/files/trash", s.requireSession(s.filesTrash))
	mux.Handle("GET /sites/{name}/files/unpack", s.requireSession(s.filesUnpackConfirm))
	mux.Handle("POST /sites/{name}/files/unpack", s.requireSession(s.filesUnpack))
	mux.Handle("POST /sites/{name}/files/place", s.requireSession(s.filesPlace))
	mux.Handle("POST /sites/{name}/files/upload", s.requireStreamSession(s.filesUploadForm))
	mux.Handle("POST /sites/{name}/files/url", s.requireSession(s.filesURLSet))
	mux.Handle("GET /sites/{name}/trash", s.requireSession(s.trash))
	mux.Handle("POST /sites/{name}/trash/{id}/restore", s.requireSession(s.trashRestore))
	mux.Handle("GET /sites/{name}/trash/empty", s.requireSession(s.trashEmptyConfirm))
	mux.Handle("POST /sites/{name}/trash/empty", s.requireSession(s.trashEmpty))
	mux.Handle("GET /sites/{name}/db", s.requireSession(s.dbPage))
	mux.Handle("GET /sites/{name}/db/coll", s.requireSession(s.dbColl))
	mux.Handle("GET /sites/{name}/db/doc", s.requireSession(s.dbDocPage))
	mux.Handle("GET /sites/{name}/db/edit", s.requireSession(s.dbEditForm))
	mux.Handle("POST /sites/{name}/db/edit", s.requireSession(s.dbEditPost))
	mux.Handle("GET /sites/{name}/db/delete", s.requireSession(s.dbDeleteConfirm))
	mux.Handle("POST /sites/{name}/db/delete", s.requireSession(s.dbDeletePost))
	mux.Handle("GET /sites/{name}/db/undo/{id}", s.requireSession(s.dbUndoConfirm))
	mux.Handle("POST /sites/{name}/db/undo/{id}", s.requireSession(s.dbUndoPost))
	mux.Handle("GET /sites/{name}/migrate", s.requireSession(s.migratePage))
	mux.Handle("POST /sites/{name}/migrate/source", s.requireSession(s.migrateSource))
	mux.Handle("POST /sites/{name}/migrate/test", s.requireSession(s.migrateTest))
	mux.Handle("POST /sites/{name}/migrate/choose", s.requireSession(s.migrateChoose))
	mux.Handle("POST /sites/{name}/migrate/discover", s.requireSession(s.migrateJob("discover")))
	mux.Handle("POST /sites/{name}/migrate/scan", s.requireSession(s.migrateJob("scan")))
	mux.Handle("POST /sites/{name}/migrate/copy", s.requireSession(s.migrateJob("copy")))
	mux.Handle("POST /sites/{name}/migrate/forget", s.requireSession(s.migrateForget))
	mux.Handle("GET /sources", s.requireSession(s.sourcesPage))
	mux.Handle("POST /sources/{id}/test", s.requireSession(s.sourceTest))
	mux.Handle("GET /sources/{id}/remove", s.requireSession(s.sourceConfirm("remove")))
	mux.Handle("POST /sources/{id}/remove", s.requireSession(s.sourceRemove))
	mux.Handle("GET /sources/{id}/reset", s.requireSession(s.sourceConfirm("reset")))
	mux.Handle("POST /sources/{id}/reset", s.requireSession(s.sourceReset))
	mux.Handle("GET /backup", s.requireSession(s.backupPage))
	mux.Handle("POST /backup/setup", s.requireSession(s.backupSetup))
	mux.Handle("GET /backup/repo", s.requireSession(s.backupRepoPage))
	mux.Handle("POST /backup/key", s.requireSession(s.backupKey))
	mux.Handle("POST /backup/confirm", s.requireSession(s.backupConfirm))
	mux.Handle("POST /backup/settings", s.requireSession(s.backupSettings))
	mux.Handle("POST /backup/pause", s.requireSession(s.backupPause))
	mux.Handle("POST /backup/run", s.requireSession(s.backupRun))
	mux.Handle("POST /backup/trial", s.requireSession(s.backupTrial))
	mux.Handle("GET /sites/{name}/backup", s.requireSession(s.siteBackupPage))
	mux.Handle("POST /sites/{name}/backup", s.requireSession(s.siteBackupSave))
	mux.Handle("POST /sites/{name}/backups/list", s.requireSession(s.backupListRefresh))
	mux.Handle("GET /sites/{name}/backups/{run}", s.requireSession(s.backupPoint))
	mux.Handle("POST /sites/{name}/backups/{run}/open", s.requireSession(s.backupOpen))
	mux.Handle("POST /sites/{name}/backups/{run}/close", s.requireSession(s.backupClose))
	mux.Handle("GET /sites/{name}/backups/{run}/restore", s.requireSession(s.backupRestorePage))
	mux.Handle("POST /sites/{name}/backups/{run}/restore", s.requireSession(s.backupRestorePost))
	mux.Handle("POST /sites/{name}/backups/{run}/export", s.requireSession(s.backupExportPost))
	mux.Handle("GET /sites/{name}/backups/{run}/files", s.requireSession(s.backupFilesPage))
	mux.Handle("GET /sites/{name}/backups/{run}/files/download", s.requireSession(s.backupFileDownload))
	mux.Handle("GET /sites/{name}/backups/{run}/db", s.requireSession(s.viewDBPage))
	mux.Handle("GET /sites/{name}/backups/{run}/db/coll", s.requireSession(s.viewDBColl))
	mux.Handle("GET /sites/{name}/backups/{run}/db/doc", s.requireSession(s.viewDBDoc))
	mux.Handle("POST /sites/{name}/backup/run", s.requireSession(s.backupRun))
	mux.Handle("GET /removed", s.requireSession(s.removedPage))
	mux.Handle("GET /removed/{id}", s.requireSession(s.removedConfirm))
	mux.Handle("POST /removed/{id}", s.requireSession(s.removedPurge))
	mux.Handle("GET /notify", s.requireSession(s.notifyPage))
	mux.Handle("POST /notify/telegram", s.requireSession(s.notifyStart))
	mux.Handle("POST /notify/link", s.requireSession(s.notifyLink))
	mux.Handle("POST /notify/cancel", s.requireSession(s.notifyCancel))
	mux.Handle("POST /notify/test", s.requireSession(s.notifyTest))
	mux.Handle("POST /notify/remove", s.requireSession(s.notifyRemove))
	mux.Handle("GET /jobs/{id}", s.requireSession(s.job))
	mux.Handle("POST /jobs/{id}/cancel", s.requireSession(s.jobCancel))
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
		// Only the panel's own files: one small script (live updates), no inline script or
		// style, no third parties.
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
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
				s.pending.sweep() // an abandoned review's secrets do not stay in memory
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
