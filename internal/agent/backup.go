package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
)

// Backups (panel-tasarim §27) copy the server to its own S3 bucket (Cloudflare R2) with
// restic: encrypted and deduplicated before anything leaves the server, so a night only
// adds what changed. A site is up to three snapshots, one per path, which is also how
// retention groups them:
//
//	/footops/<site>/db.archive  mongodump --archive, uncompressed (gzip would defeat the
//	                            deduplication) and one collection at a time (parallel
//	                            dumps interleave differently every time, ditto)
//	/footops/<site>/files       the site's data directory, mounted read-only
//	/footops/<site>/def         footops.json and compose.yaml — the environment with its
//	                            secrets, which only ever sits in the encrypted repository
//	/footops/_server            what rebuilding the server takes: /etc/footops,
//	                            certificates, the panel's login, registry logins
//
// restic runs in one-off tool containers like the MongoDB tools do: the server's MongoDB
// image (the database part needs its mongodump), the restic binary taken from a pinned
// image mounted read-only, no capabilities but reading files, the same memory and CPU
// limits, its credentials in root-only files mounted read-only (rule K5). For the
// database restic starts mongodump itself (--stdin-from-command): it saves no snapshot
// unless mongodump exits 0, so a dump cut short never becomes a backup, whoever dies
// first.
const (
	DefaultBackupDir    = "/var/lib/footops/backup"
	DefaultBackupSecret = core.BackupSecretDir + "/repository.json"
	DefaultToolsDir     = "/var/lib/footops/tools"
	DefaultResticCache  = "/var/cache/footops/restic"

	resticVersion = "0.19.1"
	// serverPart is the snapshot of the server itself; site names start with a letter.
	serverPart = "_server"
	// backupRoot is where everything sits inside the tool container (and the repository).
	backupRoot = "/footops"
)

// resticSHA256 pins the binary inside core.ResticImage, per architecture. A variable so
// tests can stand in a fake.
var resticSHA256 = map[string]string{
	"amd64": "20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639",
	"arm64": "2fb45ac6f9071b6f20eb883953a188f9e7c7cb6bbe43c67a2e47ada4e85ee7f0",
}

// Database schedules, file schedules.
const (
	DBDaily  = "daily"
	DB6h     = "6h"
	DBHourly = "hourly"

	FilesDaily  = "daily"
	FilesWeekly = "weekly" // on the night run of filesWeekday
	FilesOff    = "off"

	filesWeekday = time.Sunday // weekly files, prune and check
)

// BackupPolicy is what is backed up and kept, for all sites (the defaults) or one site
// (an override: empty fields take the default).
type BackupPolicy struct {
	// Off leaves a site out (site overrides only).
	Off   bool   `json:"off,omitempty"`
	DB    string `json:"db,omitempty"`
	Files string `json:"files,omitempty"`
	// Freeze pauses the site's container while its files are read (site overrides only):
	// for apps that keep a database in a file (SQLite), so the copy is consistent.
	Freeze bool `json:"freeze,omitempty"`
	// Snapshots are kept hourly within KeepHours of a group's newest, daily within
	// KeepDays, monthly within KeepMonths (restic forget --keep-within-*). Counting from
	// the newest means a stopped site's last backups never age away.
	KeepHours  int `json:"keep_hours,omitempty"`
	KeepDays   int `json:"keep_days,omitempty"`
	KeepMonths int `json:"keep_months,omitempty"`
}

// BackupSettings are the backup schedule and policies (no secrets).
type BackupSettings struct {
	// Confirmed is when the operator said the repository password is kept somewhere
	// else; nothing is backed up before (a repository nobody can open is worthless).
	Confirmed time.Time `json:"confirmed,omitempty"`
	// Paused stops the timetable; "back up now" still works.
	Paused bool `json:"paused,omitempty"`
	// Night is the nightly run's time, "HH:MM" in the server's time zone. Hourly
	// database backups run at the same minute of every other hour.
	Night    string                  `json:"night"`
	Defaults BackupPolicy            `json:"defaults"`
	Sites    map[string]BackupPolicy `json:"sites,omitempty"`
	// WarnGB is the repository size (GB) that raises an alert: free tiers end somewhere
	// (Cloudflare R2: 10 GB an account) and nothing stops a bucket growing past them.
	// Zero means DefaultWarnGB.
	WarnGB int `json:"warn_gb,omitempty"`
}

// DefaultWarnGB leaves room below R2's free 10 GB for the account's other buckets.
const DefaultWarnGB = 8

// DefaultBackupSettings are the operator's choices of 2026-11-06 (§27.3).
func DefaultBackupSettings() BackupSettings {
	return BackupSettings{Night: "04:00", Defaults: BackupPolicy{DB: DBDaily, Files: FilesDaily,
		KeepHours: 48, KeepDays: 30, KeepMonths: 6}, WarnGB: DefaultWarnGB}
}

// Warn is the repository size that raises an alert, in bytes.
func (s BackupSettings) Warn() int64 {
	gb := s.WarnGB
	if gb <= 0 {
		gb = DefaultWarnGB
	}
	return int64(gb) << 30
}

var reNight = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

// Validate checks settings as the panel sends them.
func (s BackupSettings) Validate() error {
	var p []string
	if !reNight.MatchString(s.Night) {
		p = append(p, "gece saati SS:DD biçiminde olmalı (örn. 04:00)")
	}
	check := func(who string, pol BackupPolicy, site bool) {
		switch pol.DB {
		case DBDaily, DB6h, DBHourly:
		case "":
			if !site {
				p = append(p, who+": veritabanı sıklığı gerekli")
			}
		default:
			p = append(p, who+": veritabanı sıklığı günlük, 6 saatte bir ya da saatlik olmalı")
		}
		switch pol.Files {
		case FilesDaily, FilesWeekly, FilesOff:
		case "":
			if !site {
				p = append(p, who+": dosya sıklığı gerekli")
			}
		default:
			p = append(p, who+": dosya sıklığı günlük, haftalık ya da kapalı olmalı")
		}
		for _, k := range []struct {
			v, min, max int
			what        string
		}{{pol.KeepHours, 1, 24 * 31, "saatlik saklama (saat)"}, {pol.KeepDays, 1, 366 * 2, "günlük saklama (gün)"},
			{pol.KeepMonths, 1, 120, "aylık saklama (ay)"}} {
			if (k.v != 0 || !site) && (k.v < k.min || k.v > k.max) {
				p = append(p, fmt.Sprintf("%s: %s %d-%d arasında olmalı", who, k.what, k.min, k.max))
			}
		}
		if !site && (pol.Off || pol.Freeze) {
			p = append(p, "kapatma ve dondurma yalnız site ayarıdır")
		}
	}
	check("genel", s.Defaults, false)
	for name, pol := range s.Sites {
		if !docker.ValidName(name) || strings.HasPrefix(name, "_") {
			p = append(p, fmt.Sprintf("geçersiz site adı %q", name))
			continue
		}
		check(name, pol, true)
	}
	if len(s.Sites) > 500 {
		p = append(p, "en fazla 500 site ayarı")
	}
	if s.WarnGB < 0 || s.WarnGB > 10000 {
		p = append(p, "depo boyutu uyarısı 1-10000 GB arasında olmalı")
	}
	if len(p) > 0 {
		return &validationError{p}
	}
	return nil
}

// validationError lists every problem, so a form can show them together.
type validationError struct{ problems []string }

func (e *validationError) Error() string { return strings.Join(e.problems, "; ") }

// For returns a site's effective policy: its override over the defaults.
func (s BackupSettings) For(site string) BackupPolicy {
	p := s.Defaults
	p.Off, p.Freeze = false, false
	o, ok := s.Sites[site]
	if !ok {
		return p
	}
	p.Off, p.Freeze = o.Off, o.Freeze
	if o.DB != "" {
		p.DB = o.DB
	}
	if o.Files != "" {
		p.Files = o.Files
	}
	if o.KeepHours != 0 {
		p.KeepHours = o.KeepHours
	}
	if o.KeepDays != 0 {
		p.KeepDays = o.KeepDays
	}
	if o.KeepMonths != 0 {
		p.KeepMonths = o.KeepMonths
	}
	return p
}

// nightAt is today's night time in loc.
func (s BackupSettings) nightAt(day time.Time) time.Time {
	h, m := 4, 0
	if mm := reNight.FindStringSubmatch(s.Night); mm != nil {
		h, _ = strconv.Atoi(mm[1])
		m, _ = strconv.Atoi(mm[2])
	}
	y, mo, d := day.Date()
	return time.Date(y, mo, d, h, m, 0, 0, day.Location())
}

// backupSecret is what reaching the repository takes; root-only (0600).
type backupSecret struct {
	// Repository is restic's: s3:https://<account>.r2.cloudflarestorage.com/<bucket>.
	Repository string `json:"repository"`
	KeyID      string `json:"key_id"`
	SecretKey  string `json:"secret_key"`
	// Password is the repository's own, made at setup (or the existing one's).
	Password string    `json:"password"`
	Created  time.Time `json:"created"`
}

// BackupLast is a part's last successful backup.
type BackupLast struct {
	Time     time.Time `json:"time"`
	Snapshot string    `json:"snapshot"`
	Run      string    `json:"run"`
}

// BackupFailure is something that went wrong and has not been put right since: a part
// whose last try failed (or saved with a warning), a run that saved nothing, a night
// that ended with an error.
type BackupFailure struct {
	Time time.Time `json:"time"`
	Err  string    `json:"error"`
	// Warning: the part was saved, but not everything could be read.
	Warning bool   `json:"warning,omitempty"`
	Run     string `json:"run,omitempty"`
}

// backupState is what the timetable and the panel need between runs.
type backupState struct {
	// Repository is the repository the rest belongs to; another one starts afresh.
	Repository string `json:"repository,omitempty"`
	// NightDone is the night run (its date, YYYY-MM-DD in the server's time zone) that
	// ran last; NightFailed tells it ended without reaching the repository.
	NightDone   string    `json:"night_done,omitempty"`
	LastPrune   time.Time `json:"last_prune,omitempty"`
	LastCheck   time.Time `json:"last_check,omitempty"`
	CheckFailed string    `json:"check_failed,omitempty"`
	// Last is every part's last successful backup, keyed "site/part".
	Last map[string]BackupLast `json:"last,omitempty"`
	// Failed is every part whose last try failed or saved with a warning ("site/part"):
	// its next clean backup strikes it off, the night's tidying the parts of sites that
	// are gone. RunFailed is the last run that saved nothing for a reason of its own (the
	// repository unreachable, the disk full), cleared by a run that saves something.
	// ForgetFailed and PruneFailed are a failed retention or prune, each cleared when it
	// next runs clean (a failed prune is tried again the next night).
	Failed       map[string]BackupFailure `json:"failed,omitempty"`
	RunFailed    *BackupFailure           `json:"run_failed,omitempty"`
	ForgetFailed *BackupFailure           `json:"forget_failed,omitempty"`
	PruneFailed  *BackupFailure           `json:"prune_failed,omitempty"`
	// RepoSize is what the snapshots take in the repository (packed), measured nightly.
	RepoSize  int64     `json:"repo_size,omitempty"`
	RepoSized time.Time `json:"repo_sized,omitempty"`
	// Trial is the last monthly trial restore (backup_trial.go).
	Trial *BackupTrial `json:"trial,omitempty"`
}

// Backups owns the repository settings, the timetable and the runs. Its heavy work
// runs as jobs (one at a time with everything else) through DataOps' tool runners.
type Backups struct {
	Data  *DataOps
	Jobs  *Jobs
	Audit *Audit
	// Dir keeps settings.json, state.json, runs/ and the record of paused containers.
	Dir        string
	SecretPath string
	ToolsDir   string // the restic binary, checked against its pinned checksum
	CacheDir   string // restic's cache: the index is not fetched again every run
	// Server is restic's --host, the server code: a new container finds its parent
	// snapshot through it.
	Server string
	Log    *log.Logger
	// Rehearsal, while the server is one, keeps the repository and the timetable off.
	Rehearsal *Rehearsal

	now      func() time.Time
	loc      *time.Location // the night time's time zone (default: the server's)
	initOnce sync.Once
	mu       sync.Mutex // the files in Dir, and the timetable's fields below
	wake     chan struct{}
	started  time.Time
	// nightNotBefore holds back an owed night run (busy, or the repository was
	// unreachable); lastSlot is the hourly slot last handled.
	nightNotBefore time.Time
	lastSlot       time.Time
	// unlocked: stale repository locks were removed since the agent started (a cut-off
	// run leaves its lock behind). Only jobs touch it, one at a time.
	unlocked bool
	// lastAlerts are the alerts last worked out, kept while a file cannot be read.
	lastAlerts []Alert
	// trialPick orders a trial's candidate files (tests make it predictable); trialEvery
	// replaces how often the trial runs (0: trialEvery; below 0: never — tests whose
	// night runs must not be followed by one).
	trialPick  func(n int) []int
	trialEvery time.Duration

	// viewMu guards viewUse (opened backups in use: opening, or a restore reads them —
	// no sweep or close takes them), viewClosing (being closed: no restore takes them),
	// viewTimer (closes the next one when its time is up) and viewWait (the timer's
	// floor, longer while closes fail).
	viewMu      sync.Mutex
	viewUse     map[string]int
	viewClosing map[string]bool
	viewTimer   *time.Timer
	viewWait    time.Duration
	// reads lets one listing of a backup's files run at a time, fetches one single-file
	// download: a slow download does not hold the listings up.
	reads, fetches chan struct{}
}

func (b *Backups) init() {
	b.initOnce.Do(func() {
		if b.now == nil {
			b.now = time.Now
		}
		b.wake = make(chan struct{}, 1)
		b.reads, b.fetches = make(chan struct{}, 1), make(chan struct{}, 1)
		b.started = b.now()
		b.Data.init()
	})
}

func (b *Backups) logf(format string, args ...any) {
	if b.Log != nil {
		b.Log.Printf(format, args...)
	}
}

// ---- files ----

func (b *Backups) settingsPath() string { return filepath.Join(b.Dir, "settings.json") }
func (b *Backups) statePath() string    { return filepath.Join(b.Dir, "state.json") }
func (b *Backups) runsDir() string      { return filepath.Join(b.Dir, "runs") }
func (b *Backups) pausedPath() string   { return filepath.Join(b.Dir, "paused.json") }

// Settings returns the saved settings, or the defaults.
func (b *Backups) Settings() (BackupSettings, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settingsLocked()
}

func (b *Backups) settingsLocked() (BackupSettings, error) {
	s := DefaultBackupSettings()
	data, err := os.ReadFile(b.settingsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return DefaultBackupSettings(), fmt.Errorf("yedek ayarları okunamadı: %w", err)
	}
	return s, nil
}

func (b *Backups) saveSettingsLocked(s BackupSettings) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	return writeSynced(b.settingsPath(), append(data, '\n'))
}

// SetSettings replaces the schedule and policies; when the password was confirmed is
// kept (only Confirm sets it).
func (b *Backups) SetSettings(s BackupSettings) error {
	b.init()
	if err := s.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	cur, err := b.settingsLocked()
	if err == nil {
		s.Confirmed = cur.Confirmed
		err = b.saveSettingsLocked(s)
	}
	b.mu.Unlock()
	b.poke()
	return err
}

// Confirm records that the operator keeps the repository password elsewhere: the
// timetable starts — with an owed night run at once, whatever held back the nights of
// a repository before.
func (b *Backups) Confirm() error {
	b.init()
	if _, err := b.secret(); err != nil {
		return err
	}
	b.mu.Lock()
	s, err := b.settingsLocked()
	if err == nil {
		s.Confirmed = b.now().UTC()
		err = b.saveSettingsLocked(s)
	}
	b.nightNotBefore = time.Time{}
	b.mu.Unlock()
	b.poke()
	return err
}

// poke makes the timetable look again (settings changed).
func (b *Backups) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *Backups) stateLocked() backupState {
	var st backupState
	if data, err := os.ReadFile(b.statePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Last == nil {
		st.Last = map[string]BackupLast{}
	}
	if st.Failed == nil {
		st.Failed = map[string]BackupFailure{}
	}
	return st
}

func (b *Backups) state() backupState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked()
}

// updateState changes the state file under the lock. A failed write costs only a
// repeated night run or a stale "last backup" until the next one.
func (b *Backups) updateState(fn func(*backupState)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.stateLocked()
	fn(&st)
	b.saveStateLocked(st)
}

func (b *Backups) saveStateLocked(st backupState) {
	data, err := json.MarshalIndent(st, "", "  ")
	if err == nil {
		if err = os.MkdirAll(b.Dir, 0o700); err == nil {
			err = writeSynced(b.statePath(), append(data, '\n'))
		}
	}
	if err != nil {
		b.logf("yedek durumu kaydedilemedi: %v", err)
	}
}

var errNoBackup = errors.New("yedekleme kurulmamış: önce yedek deposunu bağla")

// secret reads the repository settings (root-only file).
func (b *Backups) secret() (backupSecret, error) {
	var s backupSecret
	data, err := os.ReadFile(b.SecretPath)
	if errors.Is(err, fs.ErrNotExist) {
		return s, errNoBackup
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil || s.Repository == "" || s.Password == "" {
		return backupSecret{}, errors.New("yedek deposu ayarları bozuk; depoyu yeniden bağla")
	}
	return s, nil
}

func saveBackupSecret(path string, s backupSecret) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeSynced(path, append(data, '\n'))
}

// ---- repository addresses and credentials ----

var (
	reS3Host   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?(:[0-9]{1,5})?$`)
	reBucket   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	rePrefix   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	reKeyID    = regexp.MustCompile(`^[A-Za-z0-9]{8,128}$`)
	reS3Secret = regexp.MustCompile(`^[A-Za-z0-9/+=_-]{8,256}$`)
)

// CheckRepository is checkRepository for the installer (a recovery's repository).
func CheckRepository(repo string) error { return checkRepository(repo) }

// RepoLabel names a repository without its account (the bucket and any folder).
func RepoLabel(repo string) string { return repoLabel(repo) }

// checkRepository accepts an S3 address restic can use: s3:https://host/bucket with an
// optional folder below it (plain http only for a test server on a private address).
func checkRepository(repo string) error {
	rest, ok := strings.CutPrefix(repo, "s3:")
	u, err := url.Parse(rest)
	if !ok || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || !reS3Host.MatchString(u.Host) {
		return errors.New("depo adresi s3:https://<sunucu>/<kova> biçiminde olmalı")
	}
	if u.Scheme == "http" && !privateHost(u.Hostname()) {
		return errors.New("depo adresi https olmalı")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if !reBucket.MatchString(parts[0]) {
		return errors.New("kova adı 3-63 karakter: küçük harf, rakam, nokta ve tire")
	}
	for _, p := range parts[1:] {
		if !rePrefix.MatchString(p) || p == "." || p == ".." {
			return errors.New("kovadaki klasör adı geçersiz")
		}
	}
	if len(parts) > 4 {
		return errors.New("kova altında en çok üç klasör")
	}
	return nil
}

// privateHost reports whether a host is a private or loopback address (a local S3
// server for tests; R2 is always https).
func privateHost(h string) bool {
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsPrivate() || ip.IsLoopback())
}

// maskRepo replaces the repository's address in restic's messages with its label, so
// the account (in the host name) stays out of steps, records and logs.
func maskRepo(repo, s string) string {
	if u, err := url.Parse(strings.TrimPrefix(repo, "s3:")); err == nil && u.Host != "" {
		s = strings.ReplaceAll(s, repo, repoLabel(repo))
		s = strings.ReplaceAll(s, strings.TrimPrefix(repo, "s3:"), repoLabel(repo))
		s = strings.ReplaceAll(s, u.Host, "<depo>")
		s = strings.ReplaceAll(s, u.Hostname(), "<depo>")
	}
	return s
}

// repoLabel names a repository for steps and logs without its account: the bucket and
// any folder.
func repoLabel(repo string) string {
	u, err := url.Parse(strings.TrimPrefix(repo, "s3:"))
	if err != nil {
		return "?"
	}
	return strings.Trim(u.Path, "/")
}

// ---- the restic binary ----

// resticBinary returns the path of the restic binary, taking it out of the pinned image
// the first time (and whenever the file no longer matches its checksum).
func (b *Backups) resticBinary(ctx context.Context, t *Task) (string, error) {
	want, ok := resticSHA256[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("bu mimari (%s) için restic yok", runtime.GOARCH)
	}
	path := filepath.Join(b.ToolsDir, "restic-"+resticVersion)
	if sum, err := fileSHA256(path); err == nil && sum == want {
		return path, nil
	}
	t.Step("… restic aracı hazırlanıyor (" + core.ResticImage[:strings.IndexByte(core.ResticImage, '@')] + ")")
	if err := os.MkdirAll(b.ToolsDir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".part-" + t.ID() // a job and a read may both take it out at once
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	h := sha256.New()
	name := toolPrefix + t.ID() + "-restic"
	// cat in the image itself, with nothing else: no network, no capabilities.
	args := []string{"run", "--rm", "--name", name, "--label", "footops.job=" + t.ID(), "--network", "none",
		"--log-driver", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	args = append(args, b.Data.Limits.DockerFlags()...)
	args = append(args, "--entrypoint", "cat", core.ResticImage, "/usr/bin/restic")
	u, err := b.Data.Stream.Stream(ctx, run.Cmd{Name: "docker", Args: args, Stdout: io.MultiWriter(f, h),
		Stop: func() { b.killTool(name) }})
	t.AddUsage(u)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("restic aracı alınamadı: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("restic aracının özeti beklenenden farklı (%s); kullanılmadı", got[:12])
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	t.Step("✓ restic " + resticVersion + " hazır (özet doğrulandı)")
	return path, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (b *Backups) killTool(name string) {
	stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = b.Data.Run.Run(stop, "docker", "kill", name)
}

// ---- running restic ----

// resticEnv is one job's way to the repository: the binary, and the credentials in a
// root-only staging directory that is removed with the job.
type resticEnv struct {
	bin    string
	cfgDir string // mounted read-only at /run/footops
	tmpDir string // restic's temporary pack files
	n      int    // containers started, for unique names
	repo   string
	// relocked: a stale lock was removed in this job already (once is enough).
	relocked bool
	// readOnly: the repository is someone else's (a recovery reads the old server's)
	// or only read here (a trial): every command must be a read without a lock, and no
	// lock is ever removed.
	readOnly bool
}

// prepare readies a job to run restic: binary, cache, credentials. done removes the
// credentials again.
func (b *Backups) prepare(ctx context.Context, t *Task, sec backupSecret) (env *resticEnv, done func(), err error) {
	return b.prepareIn(ctx, t, sec, t.ID())
}

// prepareIn is prepare with the credentials in the staging directory name: a job that
// also runs the MongoDB tools keeps them apart from the directory those tools see.
func (b *Backups) prepareIn(ctx context.Context, t *Task, sec backupSecret, name string) (env *resticEnv, done func(), err error) {
	bin, err := b.resticBinary(ctx, t)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(b.CacheDir, 0o700); err != nil {
		return nil, nil, err
	}
	dir, err := b.Data.Staging.Dir(name)
	if err != nil {
		return nil, nil, err
	}
	done = func() { _ = b.Data.Staging.Remove(name) }
	env = &resticEnv{bin: bin, cfgDir: filepath.Join(dir, "cfg"), tmpDir: filepath.Join(dir, "tmp"), repo: sec.Repository}
	for _, d := range []string{env.cfgDir, env.tmpDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			done()
			return nil, nil, err
		}
	}
	// The secret key is a token of letters, digits and /+=_- (checked at setup), so it
	// needs no quoting in the credentials file.
	files := map[string]string{
		"repository": sec.Repository,
		"password":   sec.Password,
		"s3":         "[default]\naws_access_key_id = " + sec.KeyID + "\naws_secret_access_key = " + sec.SecretKey + "\n",
	}
	for name, content := range files {
		if err := writeFile(filepath.Join(env.cfgDir, name), content, 0o600); err != nil {
			done()
			return nil, nil, err
		}
	}
	return env, done, nil
}

// resticOpts are a run's differences: the database part joins MongoDB's network
// namespace (mongodump connects to 127.0.0.1), file parts may read any file.
type resticOpts struct {
	mongo  bool
	caps   []string
	mounts []bindMount
	// rw are mounted writable: a restore's target.
	rw []bindMount
	// limits replace the data tools' limits (reads outside the jobs take less).
	limits *run.Limits
	// out receives restic's stdout (default: JSON lines, parsed into the result).
	out io.Writer
}

type bindMount struct{ host, ctr string }

// resticCmd is the docker run of one restic command.
func (b *Backups) resticCmd(ctx context.Context, t *Task, env *resticEnv, o resticOpts, args ...string) (run.Cmd, string) {
	env.n++
	name := fmt.Sprintf("%s%s-%d", toolPrefix, t.ID(), env.n)
	full := []string{"run", "--rm", "--name", name, "--label", "footops.job=" + t.ID()}
	if o.mongo {
		full = append(full, "--network", "container:"+mongoContainer)
	}
	full = append(full, "--log-driver", "none", "--read-only", "--cap-drop", "ALL")
	for _, c := range o.caps {
		full = append(full, "--cap-add", c)
	}
	full = append(full, "--security-opt", "no-new-privileges")
	limits := b.Data.Limits
	if o.limits != nil {
		limits = *o.limits
	}
	full = append(full, limits.DockerFlags()...)
	for _, e := range []string{"RESTIC_REPOSITORY_FILE=/run/footops/repository", "RESTIC_PASSWORD_FILE=/run/footops/password",
		"AWS_SHARED_CREDENTIALS_FILE=/run/footops/s3", "RESTIC_CACHE_DIR=/cache", "TMPDIR=/tmp",
		// Progress lines every five seconds instead of 60 a second (--json's default).
		"RESTIC_PROGRESS_FPS=0.2"} {
		full = append(full, "--env", e)
	}
	// --mount, not --volume: a source that is missing is an error instead of a new
	// directory Docker creates in its place.
	mount := func(src, dst string, ro bool) {
		m := "type=bind,source=" + src + ",target=" + dst
		if ro {
			m += ",readonly"
		}
		full = append(full, "--mount", m)
	}
	mount(env.cfgDir, "/run/footops", true)
	mount(b.CacheDir, "/cache", false)
	mount(env.tmpDir, "/tmp", false)
	mount(env.bin, "/usr/local/bin/restic", true)
	for _, m := range o.mounts {
		mount(m.host, m.ctr, true)
	}
	for _, m := range o.rw {
		mount(m.host, m.ctr, false)
	}
	full = append(full, "--entrypoint", "ionice", core.MongoImage, "-c", "3", "nice", "-n", "10", "restic")
	full = append(full, args...)
	var cg string
	return run.Cmd{
		Name: "docker", Args: full,
		Stop: func() { b.killTool(name) },
		Cgroup: func() string {
			if cg == "" {
				if in, err := b.Data.Docker.Inspect(ctx, name); err == nil && in.State.Running {
					cg = run.CgroupOfPID(in.State.Pid)
				}
			}
			return cg
		},
	}, name
}

// resticResult is what one restic command reported.
type resticResult struct {
	Summary *resticSummary
	// Errors are the items restic could not read (a backup with them still saves a
	// snapshot and exits 3).
	Errors  []string
	Code    int // exit status; 0 also when the command never started
	Message string
	// Tail is the last lines that were not JSON (for the error message).
	Tail  []string
	Usage run.Usage
}

// resticSummary is backup --json's last line.
type resticSummary struct {
	FilesNew        int64   `json:"files_new"`
	FilesChanged    int64   `json:"files_changed"`
	FilesUnmodified int64   `json:"files_unmodified"`
	DataAdded       int64   `json:"data_added"`
	DataAddedPacked int64   `json:"data_added_packed"`
	TotalFiles      int64   `json:"total_files_processed"`
	TotalBytes      int64   `json:"total_bytes_processed"`
	Duration        float64 `json:"total_duration"`
	SnapshotID      string  `json:"snapshot_id"`
}

// resticError is a failed restic command. Fatal ones are about the repository itself
// (unreachable, refused, missing, wrong password): every other part would fail the same
// way, so a run stops at the first.
type resticError struct {
	Code  int
	Msg   string
	Fatal bool
}

func (e *resticError) Error() string { return e.Msg }

// isFatal reports whether err means the repository cannot be used at all.
func isFatal(err error) bool {
	var re *resticError
	return errors.As(err, &re) && re.Fatal
}

// errRefused is the cause of a command stopped on an access error from the bucket:
// restic would keep retrying a wrong or revoked key for about 15 minutes (Y0).
var errRefused = errors.New("yedek deposu erişimi reddetti")

// refusedPhrases appear in restic's messages when the bucket refuses the key. Seen from
// R2 (Y1): "The request signature we calculated does not match …" for a wrong secret,
// "Unauthorized" for an unknown key ID; elsewhere InvalidAccessKeyId, AccessDenied.
var refusedPhrases = []string{"signature we calculated does not match", "signaturedoesnotmatch",
	"invalidaccesskeyid", "access key id you provided does not exist", "access denied", "accessdenied",
	"unauthorized", "specified bucket does not exist", "nosuchbucket"}

// refusedLine reports whether a stderr line is the bucket refusing access. Items that
// could not be read (JSON lines naming a file) never count: a file may be called
// anything.
func refusedLine(line string) bool {
	if strings.HasPrefix(line, "{") && !strings.Contains(line, `"exit_error"`) {
		return false
	}
	if strings.HasPrefix(line, "subprocess ") {
		return false
	}
	l := strings.ToLower(line)
	for _, p := range refusedPhrases {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// restic runs one restic command and reads its report: stdout's JSON lines (progress,
// the summary), stderr's errors and, for the database part, mongodump's own lines. An
// access error from the bucket stops it at once.
func (b *Backups) restic(ctx context.Context, t *Task, env *resticEnv, o resticOpts, args ...string) (resticResult, error) {
	var res resticResult
	if env.readOnly && !readCommand(args) {
		return res, fmt.Errorf("yalnız okunan bir depoda restic %s çalıştırılmaz", strings.Join(args, " "))
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	cmd, _ := b.resticCmd(rctx, t, env, o, args...)
	lines := &jsonLines{each: func(line []byte) {
		var m struct {
			Type       string  `json:"message_type"`
			BytesDone  int64   `json:"bytes_done"`
			TotalBytes int64   `json:"total_bytes"`
			Percent    float64 `json:"percent_done"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		switch m.Type {
		case "status":
			t.SetBytes(m.BytesDone, m.TotalBytes)
		case "summary":
			var s resticSummary
			if json.Unmarshal(line, &s) == nil {
				res.Summary = &s
			}
		}
	}}
	cmd.Stdout = lines
	if o.out != nil {
		cmd.Stdout = o.out
	}
	var last []string
	cmd.Stderr = func(line string) {
		line = maskRepo(env.repo, line)
		if refusedLine(line) && context.Cause(rctx) == nil {
			cancel(fmt.Errorf("%w: %s", errRefused, clipMsg(line, 300)))
		}
		switch {
		case strings.HasPrefix(line, "subprocess "):
			return // a command restic started (the database part logs to a file instead)
		case strings.HasPrefix(line, "{"):
			var m struct {
				Type  string `json:"message_type"`
				Msg   string `json:"message"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(line), &m) == nil {
				switch m.Type {
				case "exit_error":
					res.Message = maskRepo(env.repo, m.Msg)
				case "error":
					if len(res.Errors) < 20 && m.Error.Message != "" && m.Error.Message != "no result" {
						res.Errors = append(res.Errors, clipMsg(maskRepo(env.repo, m.Error.Message), 300))
					}
				}
				return
			}
		}
		last = append(last, clipMsg(line, 300))
		if len(last) > 6 {
			last = last[1:]
		}
	}
	u, err := b.Data.Stream.Stream(rctx, cmd)
	lines.flush()
	res.Usage, res.Tail = u, last
	t.AddUsage(u)
	if (rctx.Err() != nil || exitCode(err) == 137) && !contains(args, "--no-lock") {
		// Stopped or killed, restic left its lock in the repository: the next job removes
		// it first (an exclusive command — forget, prune — would fail on it). Reads outside
		// the jobs take no lock and leave this alone.
		b.unlocked = false
	}
	if cause := context.Cause(rctx); errors.Is(cause, errRefused) {
		return res, &resticError{Code: -1, Fatal: true, Msg: "Yedek deposu (" + repoLabel(env.repo) +
			") erişimi reddetti: anahtar yanlış, iptal edilmiş ya da bu kovaya yetkisi yok — " + maskRepo(env.repo, strings.TrimPrefix(cause.Error(), errRefused.Error()+": "))}
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err == nil {
		return res, nil
	}
	res.Code = exitCode(err)
	msg := res.Message
	if msg == "" {
		msg = strings.Join(last, " · ")
	}
	if msg == "" {
		msg = err.Error()
	}
	msg = maskRepo(env.repo, strings.TrimPrefix(strings.ReplaceAll(msg, "\n", " "), "Fatal: "))
	re := &resticError{Code: res.Code, Msg: clipMsg(msg, 600)}
	backup := len(args) > 0 && args[0] == "backup"
	switch {
	case res.Code == 3 && backup:
		// Some files could not be read; the snapshot exists. The caller decides.
		re.Msg = "bazı dosyalar okunamadı: " + strings.Join(res.Errors, "; ")
	case res.Code == 10:
		re.Fatal, re.Msg = true, "yedek deposu bulunamadı ("+repoLabel(env.repo)+"): kurulmamış ya da silinmiş"
	case res.Code == 11 && !env.relocked && !env.readOnly && ctx.Err() == nil:
		// A lock left by a restic that was stopped (cancel, timeout, an agent restart).
		// Nothing else writes this repository and only one job runs at a time, so every
		// lock there is stale: removed, and the command tried once more.
		env.relocked = true
		t.Step("… depoda yarıda kalmış bir işin kilidi var; kaldırılıyor")
		if _, uerr := b.restic(ctx, t, env, resticOpts{out: io.Discard}, "unlock", "--remove-all"); uerr != nil {
			return res, uerr
		}
		return b.restic(ctx, t, env, o, args...)
	case res.Code == 11:
		re.Fatal, re.Msg = true, "yedek deposu kilitli (başka bir restic işi sürüyor ya da yarıda kalmış): "+re.Msg
	case res.Code == 12:
		re.Fatal, re.Msg = true, "yedek deposunun şifresi yanlış"
	default:
		l := strings.ToLower(msg)
		re.Fatal = strings.Contains(l, "unable to open config file") || strings.Contains(l, "unable to open repository") ||
			strings.Contains(l, "unable to create lock") || strings.Contains(l, "no such host") ||
			strings.Contains(l, "connection refused") || strings.Contains(l, "i/o timeout")
	}
	return res, re
}

// readCommand reports whether a restic command line only reads, taking no lock: one of
// the reading commands, with --no-lock.
func readCommand(args []string) bool {
	if !contains(args, "--no-lock") {
		return false
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "cat", "snapshots", "dump", "restore", "ls", "find", "stats":
			return true
		}
		return false
	}
	return false
}

func exitCode(err error) int {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// jsonLines calls each for every complete line written to it. A line longer than
// maxJSONLine is dropped (restic's lines are a few hundred bytes).
type jsonLines struct {
	buf  []byte
	each func([]byte)
	skip bool
}

const maxJSONLine = 1 << 20

func (j *jsonLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !j.skip {
			if len(j.buf)+len(chunk) > maxJSONLine {
				j.buf, j.skip = j.buf[:0], true
			} else {
				j.buf = append(j.buf, chunk...)
			}
		}
		if i < 0 {
			break
		}
		if !j.skip {
			j.each(j.buf)
		}
		j.buf, j.skip = j.buf[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (j *jsonLines) flush() {
	if len(j.buf) > 0 && !j.skip {
		j.each(j.buf)
	}
	j.buf = j.buf[:0]
}

// jsonArray decodes the JSON array a command writes to its stdout as it arrives, one
// element at a time: forget's and snapshots' reports list every snapshot, and the agent
// does not hold them all at once.
type jsonArray struct {
	pw   *io.PipeWriter
	done chan error
}

func newJSONArray(each func(json.RawMessage) error) *jsonArray {
	pr, pw := io.Pipe()
	a := &jsonArray{pw: pw, done: make(chan error, 1)}
	go func() {
		err := func() error {
			dec := json.NewDecoder(pr)
			tok, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := tok.(json.Delim); !ok || d != '[' {
				return errors.New("restic çıktısı beklenen biçimde değil")
			}
			for dec.More() {
				var raw json.RawMessage
				if err := dec.Decode(&raw); err != nil {
					return err
				}
				if err := each(raw); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}()
		_, _ = io.Copy(io.Discard, pr) // the writer must never block on what is left
		a.done <- err
	}()
	return a
}

func (a *jsonArray) Write(p []byte) (int, error) { return a.pw.Write(p) }

// result ends the stream and returns the decoding error, if any.
func (a *jsonArray) result() error {
	_ = a.pw.Close()
	return <-a.done
}

// ---- setup ----

// BackupSetup connects the server to a repository. Password empty: a new repository is
// made with a new password; otherwise the repository there must open with it.
type BackupSetup struct {
	Repository string `json:"repository"`
	KeyID      string `json:"key_id"`
	SecretKey  string `json:"secret_key"`
	Password   string `json:"password,omitempty"`
	// Current is the password of the repository already set up: replacing it with
	// another repository takes it, unless root asks (sudo footops backup setup) or it was
	// never confirmed (the panel may read it until then anyway). So a web process taken
	// over cannot send the backups — the server's secrets among them — to a bucket of its
	// own.
	Current string `json:"current,omitempty"`
	// Keep connects with the password the server already has: a new key, or a new
	// address, for the same repository. It needs no Current: the repository there has to
	// open with that password, which whoever does not know it cannot arrange elsewhere.
	Keep bool `json:"keep,omitempty"`
	// GoLive is `footops go-live` putting a recovered server's repository back: the one
	// setup a rehearsal allows, and only to root.
	GoLive bool `json:"go_live,omitempty"`
}

// Check validates the request (no network).
func (s *BackupSetup) Check() error {
	s.Repository = strings.TrimSpace(s.Repository)
	s.KeyID = strings.TrimSpace(s.KeyID)
	s.SecretKey = strings.TrimSpace(s.SecretKey)
	var p []string
	if err := checkRepository(s.Repository); err != nil {
		p = append(p, err.Error())
	}
	if !reKeyID.MatchString(s.KeyID) {
		p = append(p, "erişim anahtarı kimliği (Access Key ID) harf ve rakamlardan oluşmalı")
	}
	if !reS3Secret.MatchString(s.SecretKey) {
		p = append(p, "gizli erişim anahtarı (Secret Access Key) geçersiz")
	}
	if s.Password != "" && (len(s.Password) < 8 || len(s.Password) > 256 || strings.TrimSpace(s.Password) != s.Password ||
		strings.ContainsAny(s.Password, "\r\n\x00")) {
		p = append(p, "depo şifresi 8-256 karakter olmalı, boşlukla başlayıp bitemez")
	}
	if s.Keep && s.Password != "" {
		p = append(p, "aynı depoya bağlanırken şifre verilmez (sunucudaki kullanılır)")
	}
	if len(p) > 0 {
		return &validationError{p}
	}
	return nil
}

// newRepoPassword is a new repository's password: 40 letters and digits (~238 bits).
func newRepoPassword() (string, error) { return randomSecret(40) }

// setupProbe bounds the first look at a repository during setup. A variable for tests.
var setupProbe = 3 * time.Minute

// Setup checks the repository with the given settings, makes it if there is none (only
// with a new password), and saves the settings. It returns "created" or "connected".
// The password never reaches a step, a log or the job's result.
func (b *Backups) Setup(ctx context.Context, t *Task, req BackupSetup, root bool) (string, error) {
	b.init()
	if err := b.MayReplace(req, root); err != nil {
		return "", err
	}
	prev, _ := b.secret()
	if req.Keep {
		req.Password = prev.Password
	}
	sec := backupSecret{Repository: req.Repository, KeyID: req.KeyID, SecretKey: req.SecretKey,
		Password: req.Password, Created: b.now().UTC()}
	fresh := req.Password == ""
	if fresh {
		pw, err := newRepoPassword()
		if err != nil {
			return "", err
		}
		sec.Password = pw
	}
	env, done, err := b.prepare(ctx, t, sec)
	if err != nil {
		return "", err
	}
	defer done()
	t.Step("… yedek deposu deneniyor: " + repoLabel(sec.Repository))
	// restic retries an unreachable address for about 15 minutes; someone is waiting here.
	probe, stop := context.WithTimeout(ctx, setupProbe)
	res, err := b.restic(probe, t, env, resticOpts{out: io.Discard}, "--no-cache", "--json", "cat", "config")
	stop()
	if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		why := ""
		if len(res.Tail) > 0 {
			why = ": " + res.Tail[len(res.Tail)-1]
		}
		return "", fmt.Errorf("yedek deposuna %s içinde ulaşılamadı (ağ ya da adres sorunu)%s", took(setupProbe), why)
	}
	var re *resticError
	outcome := "connected"
	switch {
	case err == nil:
		if fresh {
			return "", errors.New("depo yeni bir şifreyle açıldı; bu olmamalı") // cannot happen
		}
		t.Step("✓ Var olan depo bu şifreyle açıldı")
	case errors.As(err, &re) && re.Code == 10:
		if req.Keep {
			return "", errors.New("bu adreste bu sunucunun deposu yok: adresi kontrol et (başka bir depoya geçmek için: yeni, boş bir depo)")
		}
		if !fresh {
			return "", errors.New("bu adreste yedek deposu yok; yeni bir depo için şifreyi boş bırak")
		}
		t.Step("… depo yok, yeni depo oluşturuluyor")
		if _, err := b.restic(ctx, t, env, resticOpts{out: io.Discard}, "--json", "init"); err != nil {
			return "", fmt.Errorf("depo oluşturulamadı: %w", err)
		}
		outcome = "created"
		t.Step("✓ Yeni depo oluşturuldu (şifreli; şifre panelde bir kez gösterilir)")
	case errors.As(err, &re) && re.Code == 12:
		if fresh {
			return "", errors.New("bu adreste başka bir şifreyle açılmış bir yedek deposu var; ona bağlanmak için şifresini gir, yeni depo için başka bir kova ya da klasör seç")
		}
		if req.Keep {
			return "", errors.New("bu adresteki depo bu sunucunun şifresiyle açılmıyor: başka bir depo (ona bağlanmak için sunucuda: sudo footops backup setup --existing)")
		}
		return "", errors.New("depo şifresi yanlış")
	default:
		return "", err
	}
	if err := b.saveSetup(prev, sec, fresh); err != nil {
		return "", err
	}
	if outcome == "connected" {
		// Its backups are listed at once (a server rebuilt from them needs them first).
		b.updateIndex(ctx, t, env)
	}
	return outcome, nil
}

// saveSetup keeps a repository that was checked: its settings, whether its password is
// confirmed, and the state, which belongs to the repository.
func (b *Backups) saveSetup(prev, sec backupSecret, fresh bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := saveBackupSecret(b.SecretPath, sec); err != nil {
		return fmt.Errorf("depo ayarları kaydedilemedi: %w", err)
	}
	s, err := b.settingsLocked()
	if err != nil {
		s = DefaultBackupSettings()
	}
	same := prev.Password != "" && prev.Password == sec.Password
	switch {
	case fresh:
		s.Confirmed = time.Time{} // a new password has to be seen and saved first
	case same:
		// The repository's own password again (a new key or address): known no better than
		// before — an unconfirmed one was never seen, whoever passed it back.
	default:
		s.Confirmed = b.now().UTC() // typed: already known
	}
	if err := b.saveSettingsLocked(s); err != nil {
		return err
	}
	// The state belongs to the repository: another one starts afresh (its first night
	// run is owed at once, no old part counts as backed up there), the same one at a
	// new address keeps it.
	st := b.stateLocked()
	if !same {
		st = backupState{Last: map[string]BackupLast{}, Failed: map[string]BackupFailure{}}
	}
	st.Repository = sec.Repository
	b.saveStateLocked(st)
	b.unlocked = false
	b.nightNotBefore = time.Time{} // a retry delay was the old repository's
	b.poke()
	return nil
}

// errReplace refuses replacing a repository without its password.
var errReplace = errors.New("bu sunucuda kurulu bir yedek deposu var: değiştirmek için şu anki deponun şifresi gerekir (ya da sunucuda: sudo footops backup setup)")

// MayReplace reports whether a setup may go ahead: nothing is set up yet, root asks, the
// password was never confirmed, the same repository is kept, or the current
// repository's password is given.
func (b *Backups) MayReplace(req BackupSetup, root bool) error {
	if b.Rehearsal.Active() && !(root && req.GoLive) {
		return errRehearsal
	}
	sec, err := b.secret()
	switch {
	case req.Keep && errors.Is(err, errNoBackup):
		return errors.New("bu sunucuda kurulu bir yedek deposu yok: önce depoyu bağla")
	case req.Keep:
		return err
	case errors.Is(err, errNoBackup) || root:
		return nil
	case err != nil:
		return err
	}
	if s, err := b.Settings(); err == nil && s.Confirmed.IsZero() {
		return nil // until it is confirmed the panel may read the password anyway
	}
	if subtle.ConstantTimeCompare([]byte(req.Current), []byte(sec.Password)) != 1 {
		return errReplace
	}
	return nil
}

// Key returns the repository password (the panel shows it once; root asks for it
// again with `footops backup show-key`).
func (b *Backups) Key() (string, error) {
	sec, err := b.secret()
	if err != nil {
		return "", err
	}
	return sec.Password, nil
}

// BackupStatus is what the panel and the CLI show.
type BackupStatus struct {
	Configured bool           `json:"configured"`
	Repository string         `json:"repository,omitempty"` // bucket and folder only
	Settings   BackupSettings `json:"settings"`
	// Last is every part's last successful backup ("site/part").
	Last      map[string]BackupLast `json:"last,omitempty"`
	NightDone string                `json:"night_done,omitempty"`
	NextNight time.Time             `json:"next_night,omitempty"`
	LastPrune time.Time             `json:"last_prune,omitempty"`
	LastCheck time.Time             `json:"last_check,omitempty"`
	// CheckFailed is the last weekly check's finding, if it found something.
	CheckFailed string `json:"check_failed,omitempty"`
	// Failed, RunFailed, ForgetFailed and PruneFailed are what has not been put right yet
	// (backupState).
	Failed       map[string]BackupFailure `json:"failed,omitempty"`
	RunFailed    *BackupFailure           `json:"run_failed,omitempty"`
	ForgetFailed *BackupFailure           `json:"forget_failed,omitempty"`
	PruneFailed  *BackupFailure           `json:"prune_failed,omitempty"`
	RepoSize     int64                    `json:"repo_size,omitempty"`
	RepoSized    time.Time                `json:"repo_sized,omitempty"`
	// Trial is the last monthly trial restore.
	Trial *BackupTrial `json:"trial,omitempty"`
	// Running is the backup job under way, if one is (without its steps).
	Running *Job   `json:"running,omitempty"`
	Error   string `json:"error,omitempty"`
	// Rehearsal: the server was recovered from backups and is not live yet; no
	// repository can be set up until `footops go-live`.
	Rehearsal bool `json:"rehearsal,omitempty"`
}

// Status summarises the backups.
func (b *Backups) Status() BackupStatus {
	b.init()
	var st BackupStatus
	s, err := b.Settings()
	st.Settings, st.Rehearsal = s, b.Rehearsal.Active()
	if err != nil {
		st.Error = err.Error()
	}
	if sec, err := b.secret(); err == nil {
		st.Configured, st.Repository = true, repoLabel(sec.Repository)
	} else if !errors.Is(err, errNoBackup) {
		st.Error = err.Error()
	}
	state := b.state()
	st.Last, st.NightDone, st.LastPrune, st.LastCheck = state.Last, state.NightDone, state.LastPrune, state.LastCheck
	st.CheckFailed, st.Failed, st.RunFailed = state.CheckFailed, state.Failed, state.RunFailed
	st.ForgetFailed, st.PruneFailed = state.ForgetFailed, state.PruneFailed
	st.RepoSize, st.RepoSized, st.Trial = state.RepoSize, state.RepoSized, state.Trial
	if b.Jobs != nil {
		// A backup or a setup; restoring, exporting and listing are not "backing up".
		if j, ok := b.Jobs.Running(); ok && (j.Kind == "backup.run" || j.Kind == "backup.setup") {
			j.Steps = nil
			st.Running = &j
		}
	}
	if st.Configured && !s.Confirmed.IsZero() && !s.Paused {
		now := b.now().In(b.location())
		next := s.nightAt(now)
		if !next.After(now) {
			next = s.nightAt(now.AddDate(0, 0, 1))
		}
		st.NextNight = next
	}
	return st
}

// sortedSites returns the sites in the keys of the "site/part" map's sites, in order.
func sortedSites(last map[string]BackupLast) []string {
	seen := map[string]bool{}
	for k := range last {
		if site, _, ok := strings.Cut(k, "/"); ok {
			seen[site] = true
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
