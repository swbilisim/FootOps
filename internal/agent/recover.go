package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Recovering a server from its backups (Faz Y5, panel-tasarim §27.5). A new server is
// installed with `footops install` in recovery mode; at its end the installer asks the
// agent (as root) to bring back what the repository holds under this server's code:
//
//  1. the server's own part (_server), restored into the job's staging: the panel's
//     login and 2FA, the certificates, the registry logins, the servers sites are moved
//     from and the backup settings are put in place; the tunnel's token, the
//     repository's settings and the Telegram bot are kept aside (held/) for
//     `footops go-live`, with the old panel settings and install answers;
//  2. every site of the newest full run (and any backed up since): its definition, a new
//     database user, then its database and files before its container first starts,
//     then Caddy. A site that fails is undone and the next one goes on; a second run
//     brings back only what is still missing.
//
// The repository is only ever read, and every read is --no-lock: the Mac's read-only
// key will do, and nothing of the old server's — not even a lock — is touched. The
// server is a rehearsal (rehearsal.go) from the first change on, until go-live.
const (
	DefaultRecoverDir = "/var/lib/footops/recover"
	// defMax bounds a definition file read back from the repository.
	defMax = 1 << 20
	// heldMax bounds a file kept for go-live.
	heldMax = 1 << 20
)

// RecoverRequest asks for a recovery: the repository the old server backed up to, a key
// that may read it, the repository's password and, if set, the moment to go back to
// (the newest backups before it; default: the newest).
type RecoverRequest struct {
	Repository string    `json:"repository"`
	KeyID      string    `json:"key_id"`
	SecretKey  string    `json:"secret_key"`
	Password   string    `json:"password"`
	At         time.Time `json:"at,omitzero"`
	Actor      Actor     `json:"actor"`
}

// Check validates the request (no network); the secrets are checked like a setup's.
func (r *RecoverRequest) Check() error {
	s := BackupSetup{Repository: r.Repository, KeyID: r.KeyID, SecretKey: r.SecretKey, Password: r.Password}
	err := s.Check()
	r.Repository, r.KeyID, r.SecretKey = s.Repository, s.KeyID, s.SecretKey
	var p []string
	var ve *validationError
	if errors.As(err, &ve) {
		p = ve.problems
	} else if err != nil {
		return err
	}
	if r.Password == "" {
		p = append(p, "depo şifresi gerekli")
	}
	if !r.At.IsZero() && r.At.After(time.Now().Add(time.Minute)) {
		p = append(p, "dönülecek zaman gelecekte olamaz")
	}
	if len(p) > 0 {
		return &validationError{p}
	}
	return nil
}

// RecoverSite is how one site came back.
type RecoverSite struct {
	Name    string   `json:"name"`
	Domains []string `json:"domains,omitempty"`
	// Taken is when its newest part was backed up.
	Taken time.Time `json:"taken,omitzero"`
	// State: "started" while it is made, then "ok", "warn" (in place, but its app did
	// not answer or a part had no backup), "failed" (undone) or "conflict" (something
	// else has its name).
	State    string   `json:"state"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	// Detail says what came back ("veritabanı 12 koleksiyon / 4512 belge · 312 dosya, 45 MB").
	Detail string `json:"detail,omitempty"`
}

// Recover site states.
const (
	recoverStarted = "started"
	recoverOK      = "ok"
	recoverWarn    = "warn"
	recoverFailed  = "failed"
	// recoverConflict: something not made by this recovery has the site's name; it is
	// left alone, never taken away.
	recoverConflict = "conflict"
)

// RecoverState is the recovery's record (Dir/state.json), kept across attempts.
type RecoverState struct {
	Job        string `json:"job"`
	Repository string `json:"repository"` // bucket and folder
	// RepoID tells the repository apart without its account (a checksum of its address).
	RepoID   string    `json:"repo_id"`
	Server   string    `json:"server"`
	At       time.Time `json:"at,omitzero"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitzero"`
	// ServerTaken is when the server's part came from; ServerDone that it is in place,
	// Applied what of it was put back, Held what waits for go-live.
	ServerTaken time.Time     `json:"server_taken,omitzero"`
	ServerDone  bool          `json:"server_done,omitempty"`
	Applied     []string      `json:"applied,omitempty"`
	Held        []string      `json:"held,omitempty"`
	Sites       []RecoverSite `json:"sites"`
	// Skipped are sites the repository has older backups of only (removed from the old
	// server, or no longer backed up there).
	Skipped []string `json:"skipped,omitempty"`
	Err     string   `json:"error,omitempty"`
}

// Complete reports whether everything came back.
func (s RecoverState) Complete() bool {
	if s.Finished.IsZero() || !s.ServerDone || s.Err != "" {
		return false
	}
	for _, x := range s.Sites {
		if x.State != recoverOK && x.State != recoverWarn {
			return false
		}
	}
	return true
}

func (s RecoverState) site(name string) (RecoverSite, bool) {
	for _, x := range s.Sites {
		if x.Name == name {
			return x, true
		}
	}
	return RecoverSite{}, false
}

// RecoverStatus is the recovery's record and whether the server is still a rehearsal.
type RecoverStatus struct {
	State     *RecoverState  `json:"state,omitempty"`
	Rehearsal *RehearsalInfo `json:"rehearsal,omitempty"`
	// Running is the recovery job under way, if one is.
	Running string `json:"running,omitempty"`
	// Newest is the newest snapshot of this server in the repository set up now (once
	// go-live connected it): one taken after the recovery started is the old server's.
	Newest time.Time `json:"newest,omitzero"`
}

// Recovery brings a server back from its backups.
type Recovery struct {
	Backups   *Backups
	Ops       *SiteOps
	Rehearsal *Rehearsal
	// Dir keeps state.json and held/.
	Dir string
	// Where the server's part goes (absolute: below the store's root in tests).
	AuthPath, DockerConfig, RegistriesPath, SourcesDir string
	// PanelUID and PanelGID own the panel's login.
	PanelUID, PanelGID int

	mu sync.Mutex
}

func (r *Recovery) statePath() string { return filepath.Join(r.Dir, "state.json") }

// HeldDir is where what go-live puts in place waits.
func (r *Recovery) HeldDir() string { return filepath.Join(r.Dir, "held") }

// State reads the recovery's record; ok is false when there was none.
func (r *Recovery) State() (RecoverState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked()
}

func (r *Recovery) stateLocked() (RecoverState, bool) {
	var st RecoverState
	data, err := os.ReadFile(r.statePath())
	if err != nil || json.Unmarshal(data, &st) != nil {
		return RecoverState{}, false
	}
	return st, true
}

func (r *Recovery) save(st RecoverState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	return writeSynced(r.statePath(), append(data, '\n'))
}

// Status is what the installer and the panel are told.
func (r *Recovery) Status() RecoverStatus {
	var out RecoverStatus
	if st, ok := r.State(); ok {
		out.State = &st
	}
	if in, ok := r.Rehearsal.Info(); ok {
		out.Rehearsal = &in
	}
	if idx, err := r.Backups.readIndex(); err == nil && len(idx.Snapshots) > 0 {
		out.Newest = idx.Snapshots[0].Time // newest first
	}
	return out
}

// errNotEmpty refuses recovering onto a server that is someone's already.
var errNotEmpty = errors.New("bu sunucu boş değil: yedekten kurtarma yalnız yeni kurulmuş bir sunucuda yapılır")

// repoID is a repository's identity without its account.
func repoID(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:8])
}

// MayRecover reports whether a recovery may start. A first one: no backup repository
// set up (a server that backs up is live) and no site at all. Another run of the same
// recovery — same repository, server code and moment — goes on with what is missing (it
// touches only the sites it plans, and a site of the same name it did not make is left
// alone); one with another repository or moment would mix two points in time.
func (r *Recovery) MayRecover(req RecoverRequest) error {
	if _, err := r.Backups.secret(); !errors.Is(err, errNoBackup) {
		return fmt.Errorf("%w (yedek deposu kurulu)", errNotEmpty)
	}
	st, ok := r.State()
	if ok {
		if st.RepoID != repoID(req.Repository) || st.Server != r.Backups.Server || !st.At.Equal(req.At) {
			at := "en son yedek"
			if !st.At.IsZero() {
				at = localTime(st.At)
			}
			return fmt.Errorf("bu sunucu başka bir kurtarmayla kuruldu (%s, %s, %s): başka bir depo ya da zamandan kurmak için yeni bir sunucu kullan",
				st.Repository, st.Server, at)
		}
		return nil
	}
	list, err := r.Ops.Store.List()
	if err != nil {
		return err
	}
	if len(list) > 0 {
		var names []string
		for _, s := range list {
			names = append(names, s.Name)
		}
		return fmt.Errorf("%w (sitesi var: %s)", errNotEmpty, strings.Join(names, ", "))
	}
	return nil
}

// ---- choosing what comes back ----

// recoverPlan is what a recovery brings back.
type recoverPlan struct {
	Server  BackupSnapshot
	Sites   []recoverSitePlan // by name
	Skipped []string          // "name (son yedeği 02.01.2006 15:04)"
}

type recoverSitePlan struct {
	Name           string
	Def, DB, Files *BackupSnapshot
}

// taken is when the site's newest part was backed up.
func (p recoverSitePlan) taken() time.Time {
	var t time.Time
	for _, s := range []*BackupSnapshot{p.Def, p.DB, p.Files} {
		if s != nil && s.Time.After(t) {
			t = s.Time
		}
	}
	return t
}

// planRecovery picks, from the snapshots (newest first), the newest server part at or
// before at (zero: the newest of all) and the sites to bring back: those of the run
// that took that part — a full run, it backs up every site the server had — and those
// whose definition was backed up after it (made since). Each site gets its newest
// definition, database and files at or before at.
func planRecovery(list []BackupSnapshot, at time.Time) (recoverPlan, error) {
	var p recoverPlan
	usable := func(s BackupSnapshot) bool { return at.IsZero() || !s.Time.After(at) }
	found := false
	for _, s := range list {
		if s.Site == serverPart && s.Part == PartServer && usable(s) {
			p.Server, found = s, true
			break
		}
	}
	if !found {
		return p, errors.New("depoda bu sunucu kodunun sunucu ayarları yedeği yok")
	}
	type parts struct{ def, db, files *BackupSnapshot }
	bySite := map[string]*parts{}
	inRun := map[string]bool{}
	for i := range list {
		s := list[i]
		if s.Site == serverPart || !usable(s) {
			continue
		}
		if s.Run != "" && s.Run == p.Server.Run {
			inRun[s.Site] = true
		}
		x := bySite[s.Site]
		if x == nil {
			x = &parts{}
			bySite[s.Site] = x
		}
		snap := &list[i]
		switch s.Part { // newest first: the first of each part is its newest
		case PartDef:
			if x.def == nil {
				x.def = snap
			}
		case PartDB:
			if x.db == nil {
				x.db = snap
			}
		case PartFiles:
			if x.files == nil {
				x.files = snap
			}
		}
	}
	var names []string
	for n := range bySite {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		x := bySite[n]
		if x.def == nil {
			// Nothing to make the site from (a site made after the last night run has
			// only its hourly database backups): said, not hidden.
			p.Skipped = append(p.Skipped, n+" (tanımının yedeği yok: yalnız veritabanı ya da dosyalar)")
			continue
		}
		if !inRun[n] && !x.def.Time.After(p.Server.Time) {
			p.Skipped = append(p.Skipped, fmt.Sprintf("%s (son yedeği %s)", n, localTime(x.def.Time)))
			continue
		}
		p.Sites = append(p.Sites, recoverSitePlan{Name: n, Def: x.def, DB: x.db, Files: x.files})
	}
	return p, nil
}

// ---- the job ----

// Recover is the job (server.recover).
func (r *Recovery) Recover(ctx context.Context, t *Task, req RecoverRequest) (err error) {
	b := r.Backups
	b.init()
	defer b.Data.dropStaging(t)
	if err := r.MayRecover(req); err != nil {
		return err
	}
	sec := backupSecret{Repository: req.Repository, KeyID: req.KeyID, SecretKey: req.SecretKey, Password: req.Password}
	label := repoLabel(sec.Repository)
	env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
	if err != nil {
		return err
	}
	defer done()
	env.readOnly = true
	t.Step("… Yedek deposu açılıyor: " + label + " (yalnız okunur: depoya hiçbir şey yazılmaz)")
	if err := r.probe(ctx, t, env); err != nil {
		return err
	}
	list, err := b.listSnapshots(ctx, t, env, b.Server)
	if err != nil {
		return err
	}
	plan, err := planRecovery(list, req.At)
	if err != nil {
		return fmt.Errorf("%w: sunucu kodu (%s) eski sunucununkiyle aynı mı, dönülecek zaman doğru mu?", err, b.Server)
	}
	t.Step(fmt.Sprintf("✓ Depo açıldı: %s sunucusunun %d anlık görüntüsü; sunucu ayarları %s, %d site", b.Server, len(list),
		localTime(plan.Server.Time), len(plan.Sites)))
	if len(plan.Skipped) > 0 {
		t.Step("ℹ Son tam yedekte olmayan, daha eski yedekleri olan siteler getirilmiyor (eski sunucudan kaldırılmış ya da yedeği kapatılmış): " +
			strings.Join(plan.Skipped, ", "))
	}

	// The first change: from here on the server is a rehearsal, its sites shut off from
	// the outside before any of them starts.
	if err := r.Rehearsal.Begin(ctx, RehearsalInfo{Repository: label, Server: b.Server}); err != nil {
		return fmt.Errorf("%w — siteler kurulmadı", err)
	}
	t.Step("✓ Prova kipi: tünel, yedekleme ve Telegram kapalı; siteler dışarıya bağlantı açamaz (canlıya almak: sudo footops go-live)")

	st, _ := r.State()
	st.Job, st.Repository, st.RepoID, st.Server, st.At = t.ID(), label, repoID(req.Repository), b.Server, req.At
	st.Started, st.Finished, st.Err, st.Skipped = b.now().UTC(), time.Time{}, "", plan.Skipped
	// Records of sites this plan no longer has (the newest backups moved on) go, but for
	// the sites the recovery made: they are on the server.
	planned := map[string]bool{}
	for _, sp := range plan.Sites {
		planned[sp.Name] = true
	}
	kept := st.Sites[:0]
	for _, x := range st.Sites {
		if planned[x.Name] || x.State == recoverOK || x.State == recoverWarn {
			kept = append(kept, x)
		}
	}
	st.Sites = kept
	if err := r.save(st); err != nil {
		return err
	}
	defer func() {
		st.Finished = b.now().UTC()
		if err != nil {
			st.Err = clipMsg(err.Error(), 500)
		}
		if serr := r.save(st); serr != nil && err == nil {
			err = serr
		}
	}()

	if st.ServerDone {
		t.Step("ℹ Sunucu ayarları önceki denemede geri geldi (" + localTime(st.ServerTaken) + ")")
	} else {
		applied, held, err := r.server(ctx, t, env, plan.Server)
		if err != nil {
			return fmt.Errorf("sunucu ayarları geri gelmedi (siteler kurulmadı): %w", err)
		}
		st.ServerDone, st.ServerTaken, st.Applied, st.Held = true, plan.Server.Time, applied, held
		if err := r.save(st); err != nil {
			return err
		}
	}
	if err := b.Data.WaitMongo(ctx); err != nil {
		return fmt.Errorf("MongoDB cevap vermiyor: %w", err)
	}

	var failed []string
	for i, sp := range plan.Sites {
		if err := ctx.Err(); err != nil {
			return err
		}
		if prev, ok := st.site(sp.Name); ok && (prev.State == recoverOK || prev.State == recoverWarn) {
			t.Step(fmt.Sprintf("ℹ [%d/%d] %s önceki denemede kuruldu", i+1, len(plan.Sites), sp.Name))
			continue
		}
		res := r.site(ctx, t, env, sp, i+1, len(plan.Sites), &st)
		if res.State == recoverFailed || res.State == recoverConflict {
			failed = append(failed, sp.Name)
		}
	}
	ok, warned := 0, 0
	for _, x := range st.Sites {
		switch x.State {
		case recoverOK:
			ok++
		case recoverWarn:
			ok++
			warned++
		}
	}
	t.SetResult(fmt.Sprintf("%d/%d site", ok, len(plan.Sites)))
	if len(failed) > 0 {
		return fmt.Errorf("%d site kurulamadı: %s (kurulumu yeniden çalıştırınca yalnız bunlar denenir)", len(failed), strings.Join(failed, ", "))
	}
	note := ""
	if warned > 0 {
		note = fmt.Sprintf(" (%d sitenin uygulaması cevap vermedi: prova kipinde dışarıya bağlanamadığı için olabilir)", warned)
	}
	t.Step(fmt.Sprintf("✓ Kurtarma bitti: %d site geri geldi%s. Sunucu prova kipinde.", ok, note))
	return nil
}

// probe opens the repository (a read-only look at its config, bounded like a setup's).
func (r *Recovery) probe(ctx context.Context, t *Task, env *resticEnv) error {
	probe, stop := context.WithTimeout(ctx, setupProbe)
	res, err := r.Backups.restic(probe, t, env, resticOpts{out: io.Discard}, "--no-cache", "--no-lock", "--json", "cat", "config")
	stop()
	var re *resticError
	switch {
	case err == nil:
		return nil
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		why := ""
		if len(res.Tail) > 0 {
			why = ": " + res.Tail[len(res.Tail)-1]
		}
		return fmt.Errorf("yedek deposuna %s içinde ulaşılamadı (ağ ya da adres sorunu)%s", took(setupProbe), why)
	case errors.As(err, &re) && re.Code == 10:
		return errors.New("bu adreste yedek deposu yok: hesap kimliğini, kova adını ve klasörü kontrol et")
	case errors.As(err, &re) && re.Code == 12:
		return errors.New("depo şifresi yanlış")
	}
	return err
}

// ---- the server's part ----

// heldFiles are what go-live puts in place: name under held/ → path in the server's part.
var heldFiles = []struct{ name, from string }{
	{"footops.json", "etc/footops.json"},
	{"install.json", "etc/install.json"},
	{"cloudflared.env", "etc/secrets/cloudflared.env"},
	{"repository.json", "etc/secrets/backup/repository.json"},
	{"telegram.json", "etc/secrets/notify/telegram.json"},
}

// server restores the server's part into the job's staging and puts in place what the
// sites and the panel need; what waits for go-live goes to held/. The rest (the old
// MongoDB root password, Caddy's own files) is of no use here and goes with the staging.
func (r *Recovery) server(ctx context.Context, t *Task, env *resticEnv, snap BackupSnapshot) (applied, held []string, err error) {
	b := r.Backups
	t.Step(fmt.Sprintf("… Sunucu ayarları yedekten açılıyor (%s, %d dosya)", localTime(snap.Time), snap.Files))
	dir, err := b.Data.Staging.Dir(resticPrefix + t.ID())
	if err != nil {
		return nil, nil, err
	}
	root := filepath.Join(dir, "server")
	if err := os.RemoveAll(root); err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(root)
	if err := b.Data.Staging.Allow(snap.Bytes); err != nil {
		return nil, nil, err
	}
	var restored, total int64
	lines := &jsonLines{each: func(line []byte) {
		var m struct {
			Type          string `json:"message_type"`
			TotalFiles    int64  `json:"total_files"`
			FilesRestored int64  `json:"files_restored"`
		}
		if json.Unmarshal(line, &m) == nil && m.Type == "summary" {
			restored, total = m.FilesRestored, m.TotalFiles
		}
	}}
	_, err = b.restic(ctx, t, env, resticOpts{caps: []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"},
		rw: []bindMount{{root, "/restore"}}, out: lines},
		"restore", "--json", "--no-lock", snap.ID+":"+partPath(serverPart, PartServer), "--target", "/restore")
	lines.flush()
	if err != nil {
		return nil, nil, fmt.Errorf("açılamadı: %w", goneOr(err))
	}
	if restored != total {
		return nil, nil, fmt.Errorf("restic %d öğeden %d tanesini açtı", total, restored)
	}
	// Everything is read inside the restored tree: no link in it leads out.
	tree, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer tree.Close()
	read := func(name string, max int64) ([]byte, error) { return readIn(tree, name, max) }

	// Certificates: every pair that is still valid.
	n, bad := 0, []string{}
	var ents []fs.DirEntry
	if d, err := tree.Open("caddy/etc/certs"); err == nil {
		ents, _ = d.ReadDir(-1)
		d.Close()
	}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasSuffix(name, ".crt") {
			continue
		}
		crt, cerr := read("caddy/etc/certs/"+name, heldMax)
		key, kerr := read("caddy/etc/certs/"+strings.TrimSuffix(name, ".crt")+".key", heldMax)
		if cerr != nil || kerr != nil {
			bad = append(bad, name+": anahtarı yok")
			continue
		}
		if _, err := r.Ops.InstallCert(ctx, string(crt), string(key)); err != nil {
			bad = append(bad, name+": "+err.Error())
			continue
		}
		n++
	}
	if n > 0 {
		applied = append(applied, fmt.Sprintf("%d sertifika", n))
		t.Step(fmt.Sprintf("✓ %d sertifika yerine kondu", n))
	}
	if len(bad) > 0 {
		t.Step("⚠ Yerine konmayan sertifikalar (bunlara bağlı siteler kurulamaz; panelden yeniden yükle): " + strings.Join(bad, "; "))
	}

	// The panel's login, the registry logins, the backup settings: put back only where
	// this server has none of its own.
	for _, f := range []struct {
		from, to, what string
		uid, gid       int
	}{
		{"web/auth.json", r.AuthPath, "panel girişi (şifre + 2FA + kurtarma kodları)", r.PanelUID, r.PanelGID},
		{"docker/config.json", filepath.Join(r.DockerConfig, "config.json"), "imaj kayıt defteri girişleri", -1, -1},
		{"registries.json", r.RegistriesPath, "kayıt defteri bilgileri", -1, -1},
		{"backup/settings.json", b.settingsPath(), "yedek ayarları (site ayarları, gece saati, uyarı eşiği)", -1, -1},
	} {
		if _, err := tree.Lstat(f.from); err != nil {
			if f.from == "web/auth.json" {
				t.Step("⚠ Yedekte panel girişi yok: sunucuda `sudo footops reset-password` ile hesap aç")
			}
			continue
		}
		if _, err := os.Lstat(f.to); err == nil {
			t.Step("ℹ Bu sunucuda zaten var, yedektekine dokunulmadı: " + f.what)
			continue
		}
		data, err := read(f.from, 16<<20)
		if err == nil && f.from == "backup/settings.json" {
			var s BackupSettings
			if err = json.Unmarshal(data, &s); err == nil {
				err = s.Validate()
			}
		}
		if err == nil {
			err = placeFile(f.to, data, f.uid, f.gid)
		}
		if err != nil {
			return applied, nil, fmt.Errorf("%s yerine konamadı: %w", f.what, err)
		}
		applied = append(applied, f.what)
		t.Step("✓ Yerine kondu: " + f.what)
	}
	// The servers sites were moved from, with their keys.
	if src := filepath.Join(root, "sources"); dirHas(src) {
		if dirHas(r.SourcesDir) {
			t.Step("ℹ Bu sunucuda zaten var, yedektekine dokunulmadı: kaynak sunucular")
		} else {
			cnt, err := copyTree(tree, "sources", r.SourcesDir)
			if err != nil {
				return applied, nil, fmt.Errorf("kaynak sunucular yerine konamadı: %w", err)
			}
			applied = append(applied, "kaynak sunucular")
			t.Step(fmt.Sprintf("✓ Yerine kondu: kaynak sunucular (%d dosya)", cnt))
		}
	}

	// What go-live puts in place.
	hd := r.HeldDir()
	if err := os.MkdirAll(hd, 0o700); err != nil {
		return applied, nil, err
	}
	if err := os.Chmod(hd, 0o700); err != nil {
		return applied, nil, err
	}
	for _, h := range heldFiles {
		data, err := read(h.from, heldMax)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			err = placeFile(filepath.Join(hd, h.name), data, -1, -1)
		}
		if err != nil {
			return applied, nil, fmt.Errorf("canlıya alma için saklanamadı (%s): %w", h.name, err)
		}
		held = append(held, h.name)
	}
	t.Step("✓ Canlıya almaya kadar bekletiliyor (yalnız root okur): " + heldText(held))
	return applied, held, nil
}

// heldText names the held files for people.
func heldText(held []string) string {
	words := map[string]string{"cloudflared.env": "tünel token'ı", "repository.json": "yedek deposu",
		"telegram.json": "Telegram botu", "footops.json": "panel ayarları", "install.json": "kurulum cevapları"}
	var out []string
	for _, h := range held {
		out = append(out, words[h])
	}
	if len(out) == 0 {
		return "yok"
	}
	return strings.Join(out, ", ")
}

// placeFile puts data at path (0600, durably), owned by uid:gid (-1: as made, root's).
// Neither name is followed if it is a link — the panel's directory belongs to another
// user: the file is made under a fresh random name, owned through its handle, then
// renamed over path.
func placeFile(path string, data []byte, uid, gid int) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+randomToken())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	_, err = f.Write(data)
	if err == nil && (uid >= 0 || gid >= 0) {
		err = f.Chown(uid, gid)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// readIn reads a regular file of at most max bytes inside tree (no link followed out of
// it, none at its last name).
func readIn(tree *os.Root, name string, max int64) ([]byte, error) {
	f, err := tree.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s düz bir dosya değil", name)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s beklenenden büyük", name)
	}
	return b, nil
}

// dirHas reports whether dir exists and holds something.
func dirHas(dir string) bool {
	ents, err := os.ReadDir(dir)
	return err == nil && len(ents) > 0
}

// copyTree copies the folders and regular files below src inside tree to dst, owned by
// root, folders 0700 and files 0600 (they hold private keys); anything else is left out.
// It returns how many files it copied.
func copyTree(tree *os.Root, src, dst string) (int, error) {
	n := 0
	err := fs.WalkDir(tree.FS(), src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, src), "/")
		to := filepath.Join(dst, filepath.FromSlash(rel))
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, 0o700); err != nil {
				return err
			}
			return os.Chmod(to, 0o700)
		case d.Type().IsRegular():
			data, err := readIn(tree, p, 16<<20)
			if err != nil {
				return err
			}
			if err := placeFile(to, data, -1, -1); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// ---- the sites ----

// site brings one site back and records how it went.
func (r *Recovery) site(ctx context.Context, t *Task, env *resticEnv, sp recoverSitePlan, i, n int, st *RecoverState) RecoverSite {
	res := RecoverSite{Name: sp.Name, Taken: sp.taken(), State: recoverStarted}
	record := func() {
		for j := range st.Sites {
			if st.Sites[j].Name == res.Name {
				st.Sites[j] = res
				_ = r.save(*st)
				return
			}
		}
		st.Sites = append(st.Sites, res)
		_ = r.save(*st)
	}
	fail := func(err error) RecoverSite {
		res.State, res.Error = recoverFailed, clipMsg(err.Error(), 500)
		t.Step(fmt.Sprintf("✗ %s kurulamadı: %s", sp.Name, res.Error))
		record()
		return res
	}
	conflict := func(what string) RecoverSite {
		res.State = recoverConflict
		res.Error = "sunucuda bu adla " + what + " var ve bu kurtarmanın kurduğu değil: dokunulmadı (kaldırılırsa sonraki çalıştırma yedekten getirir)"
		t.Step(fmt.Sprintf("✗ %s: %s", sp.Name, res.Error))
		record()
		return res
	}
	t.Step(fmt.Sprintf("… [%d/%d] %s (yedek %s)", i, n, sp.Name, localTime(res.Taken)))
	spec, env2, err := r.definition(ctx, t, env, sp)
	if err != nil {
		return fail(err)
	}
	res.Domains = spec.Domains
	prev, _ := st.site(sp.Name)
	record()

	// Something has the name already. A site this recovery left half-made — the agent
	// stopped while making it ("started": Create's own undo never ran) — is taken away
	// and made again, but only when it surely is that one (halfMade). Anything else is
	// someone's: left alone, never taken away.
	cur, gerr := r.Ops.Store.Get(sp.Name)
	dir := r.Ops.path(spec.Dir())
	_, derr := os.Lstat(dir)
	if gerr == nil || derr == nil {
		if prev.State != recoverStarted || !halfMade(spec, cur, gerr == nil, dir) {
			if gerr == nil {
				return conflict("bir site")
			}
			return conflict("bir klasör (" + spec.Dir() + ")")
		}
		t.Step("  … Önceki denemeden yarım kalan site temizleniyor")
		if err := r.Ops.discard(ctx, spec); err != nil {
			return fail(fmt.Errorf("yarım kalan site temizlenemedi: %w", err))
		}
	}

	var details, notes []string
	fill := func(ctx context.Context, s sites.Spec) error {
		if s.Database {
			if sp.DB == nil {
				notes = append(notes, "veritabanının yedeği yok: boş başlıyor")
				t.Step("  ⚠ Veritabanının yedeği yok: boş başlıyor")
			} else {
				d, err := r.loadDB(ctx, t, env, s, *sp.DB)
				if err != nil {
					return err
				}
				details = append(details, d)
			}
		}
		if s.DataMount != "" {
			if sp.Files == nil {
				notes = append(notes, "dosyaların yedeği yok: kalıcı klasör boş")
				t.Step("  ⚠ Dosyaların yedeği yok: kalıcı klasör boş")
			} else {
				sf, err := r.Backups.Data.siteFiles(s.Name)
				if err != nil {
					return err
				}
				// restoreFiles checks that every file the backup read is back, hidden ones too.
				if _, err := r.Backups.restoreFiles(ctx, t, env, *sp.Files, sf, sf.Dir, ""); err != nil {
					return err
				}
				details = append(details, fmt.Sprintf("%d dosya, %s", sp.Files.Files, humanBytes(sp.Files.Bytes)))
			}
		}
		return nil
	}
	warnings, err := r.Ops.CreateWith(ctx, spec, env2, func(s string) { t.Step("  " + s) },
		CreateOptions{Fill: fill, Keep: true, Lenient: true})
	if err != nil {
		return fail(err)
	}
	res.Detail = strings.Join(details, " · ")
	res.Warnings = append(notes, warnings...)
	res.State = recoverOK
	if len(res.Warnings) > 0 {
		res.State = recoverWarn
	}
	record()
	return res
}

// halfMade reports whether what has the site's name is surely this recovery's, left
// half-made: a definition with the backup's own creation time (CreateWith keeps it) in
// the backup's group and with its image, or — no definition yet — a directory that holds
// no more than what CreateWith writes before the definition.
func halfMade(spec, cur sites.Spec, hasDef bool, dir string) bool {
	if hasDef {
		return !spec.CreatedAt.IsZero() && cur.CreatedAt.Equal(spec.CreatedAt) && cur.Group == spec.Group && cur.Image == spec.Image
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.Name() != sites.ComposeFile || !e.Type().IsRegular() {
			return false
		}
	}
	return true
}

// definition reads a site's footops.json and compose.yaml back from its backup. The
// database's connection is left out: the site gets a new database user here.
func (r *Recovery) definition(ctx context.Context, t *Task, env *resticEnv, sp recoverSitePlan) (sites.Spec, map[string]string, error) {
	read := func(name string) ([]byte, error) {
		buf := &capBuffer{max: defMax}
		if _, err := r.Backups.restic(ctx, t, env, resticOpts{out: buf}, "dump", "--no-lock", sp.Def.ID, partPath(sp.Name, PartDef)+"/"+name); err != nil {
			return nil, fmt.Errorf("site tanımı (%s) okunamadı: %w", name, goneOr(err))
		}
		return buf.Bytes(), nil
	}
	meta, err := read(sites.MetaFile)
	if err != nil {
		return sites.Spec{}, nil, err
	}
	compose, err := read(sites.ComposeFile)
	if err != nil {
		return sites.Spec{}, nil, err
	}
	var spec sites.Spec
	if err := json.Unmarshal(meta, &spec); err != nil {
		return sites.Spec{}, nil, fmt.Errorf("site tanımı okunamadı: %w", err)
	}
	spec.Normalize()
	if spec.Name != sp.Name {
		return sites.Spec{}, nil, fmt.Errorf("yedekteki tanım başka bir siteye ait (%q)", spec.Name)
	}
	env2, err := sites.ParseComposeEnv(string(compose))
	if err != nil {
		return sites.Spec{}, nil, err
	}
	if spec.Database {
		delete(env2, spec.DBVar)
	}
	return spec, env2, nil
}

// loadDB fetches a database backup into the job's staging and restores it into the
// site's (new, empty) database as the site's own user, counted.
func (r *Recovery) loadDB(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, snap BackupSnapshot) (string, error) {
	d := r.Backups.Data
	if err := d.Staging.Allow(snap.Bytes); err != nil {
		return "", err
	}
	if err := d.checkFree(d.mongoData(), 2*snap.Bytes); err != nil {
		return "", fmt.Errorf("veritabanı için diskte yer yok: %w", err)
	}
	dir, err := d.Staging.Dir(t.ID())
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, spec.Name+".db.archive")
	_ = os.Remove(path)
	defer os.Remove(path)
	t.Step(fmt.Sprintf("  … Veritabanı yedeği indiriliyor: %s, %s döküm", localTime(snap.Time), humanBytes(snap.Bytes)))
	start := r.Backups.now()
	size, digest, err := r.Backups.dumpTo(ctx, t, env, snap, partPath(spec.Name, PartDB), path)
	if err != nil {
		return "", fmt.Errorf("veritabanı yedeği indirilemedi: %w", goneOr(err))
	}
	t.Step(fmt.Sprintf("  ✓ İndirildi: %s (%s)", humanBytes(size), took(r.Backups.now().Sub(start))))
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	p, err := archive.ReadMongoPrelude(bufio.NewReader(f))
	f.Close()
	if err != nil {
		return "", fmt.Errorf("yedekteki döküm okunamadı: %w", err)
	}
	dbs := importable(p.Databases())
	switch {
	case len(dbs) == 0:
		t.Step("  ℹ Yedekteki veritabanı boş (hiç koleksiyonu yoktu)")
		return "veritabanı boş", nil
	case len(dbs) > 1:
		return "", fmt.Errorf("yedekte tek bir veritabanı bekleniyordu, %d var: %s", len(dbs), strings.Join(dbs, ", "))
	}
	_, creds, err := d.siteDB(spec.Name)
	if err != nil {
		return "", err
	}
	counts, views, u, err := d.restoreCounted(ctx, t, creds,
		restoreSource{Path: path, Sec: dbSection{Length: size, SHA256: digest}, DB: dbs[0]})
	if err != nil {
		return "", err
	}
	t.Step(fmt.Sprintf("  ✓ Veritabanı yüklendi ve sayıldı: %d koleksiyon, %d belge%s (%s)", len(counts), sum(counts),
		viewNote(views), usageNote(u)))
	return fmt.Sprintf("veritabanı %d koleksiyon / %d belge", len(counts), sum(counts)), nil
}

// capBuffer is a buffer that refuses to grow past max.
type capBuffer struct {
	bytes.Buffer
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.Len()+len(p) > c.max {
		return 0, fmt.Errorf("%s'tan büyük", humanBytes(int64(c.max)))
	}
	return c.Buffer.Write(p)
}
