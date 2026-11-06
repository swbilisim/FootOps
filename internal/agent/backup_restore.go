package agent

import (
	"bufio"
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
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
)

// Restoring from the repository (panel-tasarim §27.4). The panel lists a site's backups
// from a local index of the repository's snapshots. A backup's database is opened for a
// look: restored into a temporary database the panel browses read-only, which also
// counts it before anything is restored (rule K2). A restore puts back the opened
// database, the backup's files or both through the import's apply: the site stops, a
// safety copy of what changes is kept, the change is counted, the site starts and must
// answer, and any failure puts the copies back (rules K1, K3). A backup can also be made
// into a FootOps archive to download, and its files listed and fetched one at a time
// (backup_export.go).

const (
	// viewKeep is how long an opened backup stays open; viewsMax how many may be open on
	// the server at once (each holds a temporary database).
	viewKeep = time.Hour
	viewsMax = 3
	// Staging names: an opened backup's dump, and the restic credentials of a job that
	// also runs the MongoDB tools — those tools see the job's own directory, and the
	// repository's keys are none of their business.
	viewPrefix   = "bview-"
	resticPrefix = "rst-"
	// viewDBPrefix names a view's temporary database: apart from a check's (footops_tmp_),
	// which the agent's start removes wholesale after a restart cut a check short.
	viewDBPrefix = "footops_view_"
	// indexMax bounds the index (a few hundred bytes a snapshot).
	indexMax = 50_000
)

var (
	reResticID = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reSiteName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)

	errBackupGone = errors.New("bu yedek listede yok: saklama kuralıyla silinmiş olabilir (listeyi yenile)")
	// errSnapshotGone: the list still has a backup the repository no longer holds
	// (retention removed it after the list was read).
	errSnapshotGone = errors.New("bu yedek depoda artık yok (saklama kuralıyla silinmiş olabilir): yedek listesini yenile")
	errViewGone     = errors.New("bu yedek açık değil (süresi dolmuş ya da kapatılmış olabilir): yeniden aç")
)

// ---- the index ----

// BackupSnapshot is one of the repository's snapshots, as the index keeps it.
type BackupSnapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Site string    `json:"site"` // "_server" for the server's own
	Part string    `json:"part"`
	Kind string    `json:"kind,omitempty"`
	Run  string    `json:"run,omitempty"`
	// Files and Bytes are what the backup read (restic's summary): the regular files and
	// their size, hidden ones included; for a database, its dump.
	Files int64 `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
}

// snapshotIndex lists the repository's snapshots of this server as of Updated.
type snapshotIndex struct {
	Repository string           `json:"repository"`
	Updated    time.Time        `json:"updated"`
	Snapshots  []BackupSnapshot `json:"snapshots"` // newest first
}

func (b *Backups) indexPath() string { return filepath.Join(b.Dir, "snapshots.json") }

// snapshotOf reads a snapshot's tags; ok is false for one FootOps did not take.
func snapshotOf(id string, at time.Time, paths, tags []string) (BackupSnapshot, bool) {
	s := BackupSnapshot{ID: id, Time: at.UTC()}
	ours := false
	for _, tag := range tags {
		k, v, _ := strings.Cut(tag, "=")
		switch k {
		case "footops":
			ours = true
		case "site":
			s.Site = v
		case "part":
			s.Part = v
		case "kind":
			s.Kind = v
		case "run":
			s.Run = v
		}
	}
	switch {
	case !ours || !reResticID.MatchString(id) || len(paths) != 1:
		return s, false
	case s.Site == serverPart && s.Part == PartServer:
	case reSiteName.MatchString(s.Site) && (s.Part == PartDB || s.Part == PartFiles || s.Part == PartDef):
	default:
		return s, false
	}
	return s, paths[0] == partPath(s.Site, s.Part) && (s.Run == "" || reJobID.MatchString(s.Run))
}

// refreshIndex reads the repository's snapshots of this server into the index (restic
// snapshots: its cache keeps the ones it saw before, only new ones are fetched).
func (b *Backups) refreshIndex(ctx context.Context, t *Task, env *resticEnv) (int, error) {
	list := []BackupSnapshot{}
	arr := newJSONArray(func(raw json.RawMessage) error {
		var sn struct {
			ID      string    `json:"id"`
			Time    time.Time `json:"time"`
			Paths   []string  `json:"paths"`
			Tags    []string  `json:"tags"`
			Summary *struct {
				Files int64 `json:"total_files_processed"`
				Bytes int64 `json:"total_bytes_processed"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(raw, &sn); err != nil {
			return err
		}
		s, ok := snapshotOf(sn.ID, sn.Time, sn.Paths, sn.Tags)
		if !ok {
			return nil
		}
		if len(list) >= indexMax {
			return fmt.Errorf("depoda %d anlık görüntüden fazlası var", indexMax)
		}
		if sn.Summary != nil {
			s.Files, s.Bytes = sn.Summary.Files, sn.Summary.Bytes
		}
		list = append(list, s)
		return nil
	})
	_, err := b.restic(ctx, t, env, resticOpts{out: arr}, "snapshots", "--json", "--no-lock", "--host", b.Server)
	derr := arr.result()
	if err != nil {
		return 0, err
	}
	if derr != nil {
		return 0, fmt.Errorf("anlık görüntü listesi okunamadı: %w", derr)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Time.After(list[j].Time) })
	data, err := json.Marshal(snapshotIndex{Repository: env.repo, Updated: b.now().UTC(), Snapshots: list})
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return 0, err
	}
	return len(list), writeSynced(b.indexPath(), data)
}

// updateIndex refreshes the index at the end of a job that changed the repository; a
// failure only leaves the list behind until the next one.
func (b *Backups) updateIndex(ctx context.Context, t *Task, env *resticEnv) {
	if ctx.Err() != nil {
		return
	}
	if n, err := b.refreshIndex(ctx, t, env); err != nil {
		t.Step("⚠ Yedek listesi yenilenemedi (sonraki yedekte yeniden denenir): " + err.Error())
	} else {
		t.Step(fmt.Sprintf("✓ Yedek listesi yenilendi: depoda bu sunucunun %d anlık görüntüsü var", n))
	}
}

// ListBackups is the job that reads the index again from the repository.
func (b *Backups) ListBackups(ctx context.Context, t *Task) error {
	b.init()
	sec, err := b.secret()
	if err != nil {
		return err
	}
	env, done, err := b.prepare(ctx, t, sec)
	if err != nil {
		return err
	}
	defer done()
	t.Step("… Yedek deposu okunuyor: " + repoLabel(sec.Repository))
	n, err := b.refreshIndex(ctx, t, env)
	if err != nil {
		return err
	}
	t.Step(fmt.Sprintf("✓ Yedek listesi yenilendi: depoda bu sunucunun %d anlık görüntüsü var", n))
	t.SetResult(fmt.Sprintf("%d anlık görüntü", n))
	return nil
}

// readIndex returns the index of the repository set up now: one of another repository
// (or of this one at its old address, until the next refresh) is no index.
func (b *Backups) readIndex() (snapshotIndex, error) {
	var idx snapshotIndex
	sec, err := b.secret()
	if err != nil {
		return idx, err
	}
	b.mu.Lock()
	data, err := os.ReadFile(b.indexPath())
	b.mu.Unlock()
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return idx, err
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return snapshotIndex{}, fmt.Errorf("yedek listesi okunamadı: %w", err)
	}
	if idx.Repository != sec.Repository {
		return snapshotIndex{}, nil
	}
	return idx, nil
}

// BackupPoint is one of a site's backups as the panel lists it: the parts one run took
// that the repository still keeps.
type BackupPoint struct {
	Run   string                    `json:"run"`
	Time  time.Time                 `json:"time"` // the earliest part's
	Kind  string                    `json:"kind"`
	Parts map[string]BackupSnapshot `json:"parts"`
}

// BackupList is a site's backups, newest first.
type BackupList struct {
	// Updated is when the list was read from the repository; zero: never.
	Updated time.Time     `json:"updated,omitempty"`
	Points  []BackupPoint `json:"points"`
	// Views are the site's backups open for a look.
	Views []BackupView `json:"views,omitempty"`
}

// SiteBackups lists a site's backups from the index.
func (b *Backups) SiteBackups(site string) (BackupList, error) {
	b.init()
	idx, err := b.readIndex()
	if err != nil {
		return BackupList{Points: []BackupPoint{}}, err
	}
	return BackupList{Updated: idx.Updated, Points: pointsOf(idx.Snapshots, site), Views: b.siteViews(site)}, nil
}

// pointsOf groups a site's snapshots by the run that took them, newest first.
func pointsOf(list []BackupSnapshot, site string) []BackupPoint {
	byRun := map[string]int{}
	points := []BackupPoint{}
	for _, s := range list {
		if s.Site != site {
			continue
		}
		key := s.Run
		if key == "" {
			key = s.ID
		}
		i, ok := byRun[key]
		if !ok {
			i = len(points)
			byRun[key] = i
			points = append(points, BackupPoint{Run: key, Time: s.Time, Kind: s.Kind, Parts: map[string]BackupSnapshot{}})
		}
		p := &points[i]
		if _, dup := p.Parts[s.Part]; dup {
			continue
		}
		p.Parts[s.Part] = s
		if s.Time.Before(p.Time) {
			p.Time = s.Time
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].Time.After(points[j].Time) })
	return points
}

// BackupDetail is one backup of a site with what its run recorded about it.
type BackupDetail struct {
	BackupPoint
	// Record is the run's record of the site's parts (counts, sizes), while it is kept
	// (RunsKeep); Repository is the repository's label.
	Record     []BackupPart `json:"record,omitempty"`
	Repository string       `json:"repository,omitempty"`
	// View is the backup's database, if it is open for a look.
	View *BackupView `json:"view,omitempty"`
}

// SiteBackup returns one of a site's backups.
func (b *Backups) SiteBackup(site, run string) (BackupDetail, error) {
	b.init()
	idx, err := b.readIndex()
	if err != nil {
		return BackupDetail{}, err
	}
	var d BackupDetail
	found := false
	for _, p := range pointsOf(idx.Snapshots, site) {
		if p.Run == run {
			d.BackupPoint, found = p, true
			break
		}
	}
	if !found {
		// Retention may take a backup off the list while it is open: its view still has
		// what a look, a database restore and a close need.
		for _, v := range b.siteViews(site) {
			if v.Run == run {
				d.BackupPoint = BackupPoint{Run: run, Time: v.Taken, Parts: map[string]BackupSnapshot{
					PartDB: {ID: v.Snapshot, Time: v.Taken, Site: site, Part: PartDB, Bytes: v.Size}}}
				found = true
			}
		}
	}
	if !found {
		return d, errBackupGone
	}
	if sec, err := b.secret(); err == nil {
		d.Repository = repoLabel(sec.Repository)
	}
	if rec, ok := b.runRecord(run); ok {
		for _, part := range rec.Parts {
			if part.Site == site {
				d.Record = append(d.Record, part)
			}
		}
	}
	for _, v := range b.siteViews(site) {
		if v.Run == run {
			v := v
			d.View = &v
		}
	}
	return d, nil
}

// recordPart is the run's record of one of the site's parts, if kept.
func (d BackupDetail) recordPart(part string) (BackupPart, bool) {
	for _, p := range d.Record {
		if p.Part == part && p.Snapshot == d.Parts[part].ID {
			return p, true
		}
	}
	return BackupPart{}, false
}

// runRecord reads a run's record by its job.
func (b *Backups) runRecord(id string) (BackupRun, bool) {
	if !reJobID.MatchString(id) {
		return BackupRun{}, false
	}
	names, err := b.runFiles()
	if err != nil {
		return BackupRun{}, false
	}
	for _, n := range names {
		if !strings.HasSuffix(n, "-"+id+".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(b.runsDir(), n))
		var rec BackupRun
		if err != nil || json.Unmarshal(data, &rec) != nil || rec.ID != id {
			return BackupRun{}, false
		}
		return rec, true
	}
	return BackupRun{}, false
}

// BackupCurrent is the site as it is now, beside a backup: what a restore replaces.
type BackupCurrent struct {
	DB          string           `json:"db,omitempty"`
	Collections map[string]int64 `json:"collections,omitempty"`
	Views       []string         `json:"views,omitempty"`
	Documents   int64            `json:"documents"`
	DBErr       string           `json:"db_error,omitempty"`
	// Files counts every regular file of the data directory, hidden ones too, as a
	// backup does.
	Files    *datafs.Stats `json:"files,omitempty"`
	FilesErr string        `json:"files_error,omitempty"`
	Running  bool          `json:"running"`
}

// Current reads the site's database counts and data directory now.
func (b *Backups) Current(ctx context.Context, site string) (BackupCurrent, error) {
	b.init()
	spec, err := b.Data.Store.Get(site)
	if err != nil {
		return BackupCurrent{}, err
	}
	var cur BackupCurrent
	if spec.Database {
		// The database browser's overview: counts from each collection's own statistics,
		// nothing read through (a full count of a large database on every preview would
		// churn MongoDB's cache). -1: not known.
		if ov, err := b.Data.DBCollections(ctx, site); err != nil {
			cur.DBErr = err.Error()
		} else {
			cur.DB, cur.Collections = ov.DB, map[string]int64{}
			for _, c := range ov.Collections {
				if c.Type == "view" {
					cur.Views = append(cur.Views, c.Name)
					continue
				}
				cur.Collections[c.Name] = c.Count
				cur.Documents += max(c.Count, 0)
			}
		}
	}
	if spec.DataMount != "" {
		if sf, err := b.Data.siteFiles(site); err != nil {
			cur.FilesErr = err.Error()
		} else if sv, err := survey(sf.Dir); err != nil {
			cur.FilesErr = err.Error()
		} else {
			cur.Files = &sv.Stats
		}
	}
	cur.Running, _ = b.Data.Sites.Running(ctx, site)
	return cur, nil
}

// dirSurvey is what a directory holds, hidden entries included, and who owns it.
type dirSurvey struct {
	datafs.Stats
	// Owners counts the entries below the top per owner (uid); Special the regular files
	// with a set-user-ID or set-group-ID bit.
	Owners  map[int]int
	Special int
}

// survey walks a directory as root without following links: every regular file and
// its size, folders, other entries and their owners.
func survey(dir string) (dirSurvey, error) {
	sv := dirSurvey{Owners: map[int]int{}}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return sv, err
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if sv.Files+sv.Dirs+sv.Skipped >= datafsMaxCount {
			return fmt.Errorf("klasörde çok fazla dosya var (sınır %d)", datafsMaxCount)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && len(sv.Owners) < 64 {
			sv.Owners[int(st.Uid)]++
		}
		switch {
		case d.IsDir():
			sv.Dirs++
		case info.Mode().IsRegular():
			sv.Files++
			sv.Bytes += info.Size()
			if info.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
				sv.Special++
			}
		default:
			sv.Skipped++
		}
		return nil
	})
	return sv, err
}

// soleOwner is the one owner of every entry, if there is one.
func soleOwner(owners map[int]int) (int, bool) {
	if len(owners) != 1 {
		return 0, false
	}
	for uid := range owners {
		return uid, true
	}
	return 0, false
}

// clearSetID takes the set-user-ID and set-group-ID bits off a directory's regular
// files (through an os.Root, links not followed).
func clearSetID(dir string) (int, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	n := 0
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if m := info.Mode(); m&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
			n++
			return root.Chmod(p, m.Perm())
		}
		return nil
	})
	return n, err
}

// backupRootUID is the owner of a backup's files folder itself (restic ls lists it
// first), which a restore of what is below it does not bring back.
func (b *Backups) backupRootUID(ctx context.Context, t *Task, env *resticEnv, snap BackupSnapshot) (int, error) {
	root := partPath(snap.Site, PartFiles)
	uid := -1
	lines := &jsonLines{each: func(line []byte) {
		var n struct {
			Type string `json:"struct_type"`
			Path string `json:"path"`
			UID  int    `json:"uid"`
		}
		if json.Unmarshal(line, &n) == nil && n.Type == "node" && n.Path == root {
			uid = n.UID
		}
	}}
	_, err := b.restic(ctx, t, env, resticOpts{out: lines}, "ls", "--json", "--no-lock", snap.ID, root)
	lines.flush()
	if err == nil && uid < 0 {
		err = errors.New("klasör listede yok")
	}
	return uid, err
}

// datafsMaxCount is the most entries a survey walks (as datafs.Count).
const datafsMaxCount = 1_000_000

// ---- opening a backup's database ----

// BackupView is a backup's database opened for a look: restored into a temporary
// database the panel browses read-only, for viewKeep.
type BackupView struct {
	ID       string    `json:"id"` // the job that opened it
	Site     string    `json:"site"`
	Run      string    `json:"run"`
	Snapshot string    `json:"snapshot"`
	Taken    time.Time `json:"taken"`
	Opened   time.Time `json:"opened"`
	Expires  time.Time `json:"expires,omitempty"`
	Ready    bool      `json:"ready"`
	// DB is the database the backup holds (the site's at the time); Collections its
	// documents per collection as restored (views apart).
	DB          string           `json:"db,omitempty"`
	Collections map[string]int64 `json:"collections,omitempty"`
	Views       []string         `json:"views,omitempty"`
	Documents   int64            `json:"documents,omitempty"`
	Size        int64            `json:"size,omitempty"` // the dump
	Actor       string           `json:"actor,omitempty"`
	// RecordChecked: the run's record (mongodump's own counts as it dumped) was there to
	// compare with; RecordDiff says how the restored counts differ from it ("" agree). A
	// difference holds a restore back until the operator accepts it.
	RecordChecked bool   `json:"record_checked,omitempty"`
	RecordDiff    string `json:"record_diff,omitempty"`
}

// viewRecord is a view with what reaching it takes; root-only, never sent to the panel.
type viewRecord struct {
	BackupView
	SHA256   string `json:"sha256,omitempty"`
	TempDB   string `json:"temp_db"` // also its user's name
	Password string `json:"password"`
}

func (v viewRecord) creds() dbCreds {
	return dbCreds{User: v.TempDB, Password: v.Password, DB: v.TempDB}
}

func (b *Backups) viewsDir() string          { return filepath.Join(b.Dir, "views") }
func (b *Backups) viewPath(id string) string { return filepath.Join(b.viewsDir(), id+".json") }
func (b *Backups) viewDump(id string) string {
	return filepath.Join(b.Data.Staging.Root, viewPrefix+id, "db.archive")
}
func (b *Backups) viewStaging(id string) string { return viewPrefix + id }

func (b *Backups) saveView(v viewRecord) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.viewsDir(), 0o700); err != nil {
		return err
	}
	return writeSynced(b.viewPath(v.ID), data)
}

func (b *Backups) loadView(id string) (viewRecord, error) {
	var v viewRecord
	if !reJobID.MatchString(id) {
		return v, errViewGone
	}
	b.mu.Lock()
	data, err := os.ReadFile(b.viewPath(id))
	b.mu.Unlock()
	if err != nil {
		return v, errViewGone
	}
	if err := json.Unmarshal(data, &v); err != nil || v.ID != id || !strings.HasPrefix(v.TempDB, viewDBPrefix) {
		return viewRecord{}, errors.New("açılan yedeğin kaydı bozuk")
	}
	return v, nil
}

// viewRecords lists the views' records, oldest first.
func (b *Backups) viewRecords() []viewRecord {
	b.mu.Lock()
	ents, err := os.ReadDir(b.viewsDir())
	b.mu.Unlock()
	if err != nil {
		return nil
	}
	var out []viewRecord
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() {
			continue
		}
		if v, err := b.loadView(id); err == nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Opened.Before(out[j].Opened) })
	return out
}

// siteViews are the site's views ready to look at.
func (b *Backups) siteViews(site string) []BackupView {
	var out []BackupView
	now := b.now()
	for _, v := range b.viewRecords() {
		if v.Site == site && v.Ready && now.Before(v.Expires) {
			out = append(out, v.BackupView)
		}
	}
	return out
}

// OpenBackup restores a backup's database into a temporary one for a look — the
// restore's trial too: what it counts is what a restore must bring. The site is not
// touched. An earlier view of the same site is closed first.
func (b *Backups) OpenBackup(ctx context.Context, t *Task, site, run string, actor Actor) (_ BackupView, err error) {
	b.init()
	defer b.Data.dropStaging(t)
	spec, err := b.Data.Store.Get(site)
	if err != nil {
		return BackupView{}, err
	}
	if !spec.Database {
		return BackupView{}, errors.New("bu sitenin veritabanı yok")
	}
	pt, err := b.SiteBackup(site, run)
	if err != nil {
		return BackupView{}, err
	}
	snap, ok := pt.Parts[PartDB]
	if !ok {
		return BackupView{}, errors.New("bu yedekte veritabanı yok")
	}
	sec, err := b.secret()
	if err != nil {
		return BackupView{}, err
	}
	for _, old := range b.viewRecords() {
		if old.Site == site {
			if err := b.closeView(old.ID); err != nil {
				return BackupView{}, fmt.Errorf("sitenin önceden açılmış yedeği kapatılamadı: %w", err)
			}
			t.Step("ℹ Sitenin önceden açılmış yedeği kapatıldı")
		}
	}
	// What is past its time does not count; only what the panel shows as open does.
	b.SweepViews()
	open := 0
	for _, v := range b.viewRecords() {
		if v.Ready && b.now().Before(v.Expires) {
			open++
		}
	}
	if open >= viewsMax {
		return BackupView{}, fmt.Errorf("sunucuda en çok %d yedek aynı anda açık olabilir: önce birini kapat", viewsMax)
	}
	if err := b.Data.Staging.Allow(snap.Bytes); err != nil {
		return BackupView{}, err
	}
	if err := b.Data.checkFree(b.Data.mongoData(), 2*snap.Bytes); err != nil {
		return BackupView{}, fmt.Errorf("geçici veritabanı için diskte yer yok: %w", err)
	}
	pw, err := randomSecret(32)
	if err != nil {
		return BackupView{}, err
	}
	rec := viewRecord{BackupView: BackupView{ID: t.ID(), Site: site, Run: run, Snapshot: snap.ID, Taken: pt.Time,
		Opened: b.now().UTC(), Actor: actor.User}, TempDB: viewDBPrefix + strings.ToLower(t.ID()), Password: pw}
	// Held while it opens: neither a sweep nor a close takes it half-made.
	release := b.holdView(rec.ID)
	defer release()
	// Written before anything is made: whatever follows can be found and removed again.
	if err := b.saveView(rec); err != nil {
		return BackupView{}, err
	}
	defer func() {
		if err != nil {
			if derr := b.dropView(rec); derr != nil {
				t.Step("⚠ Açılan yedek temizlenemedi (sonraki temizlikte yeniden denenir): " + derr.Error())
			}
		}
	}()
	env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
	if err != nil {
		return BackupView{}, err
	}
	defer done()
	if _, err := b.Data.Staging.Dir(b.viewStaging(rec.ID)); err != nil {
		return BackupView{}, err
	}
	t.Step(fmt.Sprintf("… Yedekteki veritabanı indiriliyor: %s, %s döküm", localTime(snap.Time), humanBytes(snap.Bytes)))
	start := b.now()
	path := b.viewDump(rec.ID)
	size, digest, err := b.dumpTo(ctx, t, env, snap, partPath(site, PartDB), path)
	if err != nil {
		return BackupView{}, fmt.Errorf("yedek indirilemedi: %w", goneOr(err))
	}
	t.Step(fmt.Sprintf("✓ İndirildi: %s (%s)", humanBytes(size), took(b.now().Sub(start))))
	f, err := os.Open(path)
	if err != nil {
		return BackupView{}, err
	}
	p, err := archive.ReadMongoPrelude(bufio.NewReader(f))
	f.Close()
	if err != nil {
		return BackupView{}, fmt.Errorf("yedekteki döküm okunamadı: %w", err)
	}
	dbs := importable(p.Databases())
	switch {
	case len(dbs) == 0:
		// The database had no collection when it was backed up: restoring it empties it.
		dbs = []string{spec.DBName()}
		t.Step("ℹ Yedekteki veritabanı boş (hiç koleksiyonu yoktu)")
	case len(dbs) > 1:
		return BackupView{}, fmt.Errorf("yedekte tek bir veritabanı bekleniyordu, %d var: %s", len(dbs), strings.Join(dbs, ", "))
	}
	rec.DB, rec.Size, rec.SHA256 = dbs[0], size, digest
	if err := b.saveView(rec); err != nil {
		return BackupView{}, err
	}
	t.Step("… Geçici veritabanına yükleniyor (site etkilenmez)")
	counts, views, err := b.Data.restoreTemp(ctx, t, rec.creds(), path, dbSection{Length: size, SHA256: digest}, rec.DB)
	if err != nil {
		return BackupView{}, err
	}
	rec.Collections, rec.Views, rec.Documents = counts, views, sum(counts)
	// mongodump counted the same documents as it wrote them, while the run's record is kept.
	if part, ok := pt.recordPart(PartDB); ok {
		rec.RecordChecked = true
		if rec.RecordDiff = clipMsg(countsDiffer(part.Collections, counts, views), 1000); rec.RecordDiff != "" {
			t.Step("⚠ Yedek alınırken sayılanla tutmuyor: " + rec.RecordDiff + " — geri yükleme ancak bu fark onaylanırsa yapılır")
		} else {
			t.Step("✓ Sayımlar yedek alınırken sayılanla aynı")
		}
	} else {
		t.Step("ℹ Yedek alınırken sayılanla karşılaştırılamadı (çalıştırmanın kaydı yok: 62 günden eski ya da başka sunucuda)")
	}
	rec.Ready, rec.Expires = true, b.now().UTC().Add(viewKeep)
	if err := b.saveView(rec); err != nil {
		return BackupView{}, err
	}
	b.armViews()
	t.SetResult(run) // the panel goes back to the backup's page (the view is this job's)
	t.Step(fmt.Sprintf("✓ Yedek açıldı: %d koleksiyon, %d belge%s — salt okunur incelenebilir; %s kendiliğinden kapanır",
		len(counts), rec.Documents, viewNote(views), rec.Expires.In(b.location()).Format("15:04")))
	return rec.BackupView, nil
}

// goneOr turns restic's words for a snapshot or its data missing from the repository
// into errSnapshotGone (restic's message kept for the record); other errors pass.
func goneOr(err error) error {
	if err == nil {
		return nil
	}
	l := strings.ToLower(err.Error())
	for _, p := range []string{"not found in repository", "failed to find snapshot", "no matching id found", "unable to load snapshot"} {
		if strings.Contains(l, p) {
			return fmt.Errorf("%w (%s)", errSnapshotGone, clipMsg(err.Error(), 200))
		}
	}
	return err
}

// countsDiffer compares a run's record (mongodump's counts, system collections and
// views included) with what a restore counted; "" when they agree.
func countsDiffer(recorded, restored map[string]int64, views []string) string {
	var diff []string
	for c, n := range recorded {
		if strings.HasPrefix(c, "system.") || contains(views, c) {
			continue
		}
		if restored[c] != n {
			diff = append(diff, fmt.Sprintf("%s: kayıtta %d, yüklenen %d", c, n, restored[c]))
		}
	}
	for c := range restored {
		if _, ok := recorded[c]; !ok {
			diff = append(diff, c+": kayıtta yok")
		}
	}
	sort.Strings(diff)
	return strings.Join(diff, "; ")
}

// dumpTo writes one file of a snapshot to path (restic dump), returning its size and
// SHA-256.
func (b *Backups) dumpTo(ctx context.Context, t *Task, env *resticEnv, snap BackupSnapshot, file, path string) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, "", err
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h), report: func(n int64) { t.SetBytes(n, snap.Bytes) }}
	bw := bufio.NewWriterSize(cw, 256<<10)
	_, err = b.restic(ctx, t, env, resticOpts{out: bw}, "dump", "--no-lock", snap.ID, file)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && snap.Bytes > 0 && cw.n != snap.Bytes {
		err = fmt.Errorf("indirilen %s, yedekte %s", humanBytes(cw.n), humanBytes(snap.Bytes))
	}
	if err != nil {
		_ = os.Remove(path)
		return 0, "", err
	}
	return cw.n, hex.EncodeToString(h.Sum(nil)), nil
}

// CloseView closes a view of site now (the panel's "close").
func (b *Backups) CloseView(site, id string) error {
	b.init()
	v, err := b.loadView(id)
	if err != nil || v.Site != site {
		return errViewGone
	}
	return b.closeView(id)
}

// closeView removes a view unless it is in use (opening, or a restore reads it). The
// close is marked in the same lock that checks for use, so a restore cannot take the
// view while it goes.
func (b *Backups) closeView(id string) error {
	b.viewMu.Lock()
	switch {
	case b.viewUse[id] > 0:
		b.viewMu.Unlock()
		return errors.New("bu yedek şu an kullanılıyor (açılıyor ya da ondan geri yükleniyor); bitince yeniden dene")
	case b.viewClosing[id]:
		b.viewMu.Unlock()
		return nil // being closed already
	}
	if b.viewClosing == nil {
		b.viewClosing = map[string]bool{}
	}
	b.viewClosing[id] = true
	b.viewMu.Unlock()
	defer func() {
		b.viewMu.Lock()
		delete(b.viewClosing, id)
		b.viewMu.Unlock()
	}()
	v, err := b.loadView(id)
	if err != nil {
		return nil // already gone
	}
	err = b.dropView(v)
	b.armViews()
	return err
}

// holdView marks a view in use while it opens: no sweep or close takes it.
func (b *Backups) holdView(id string) func() {
	b.viewMu.Lock()
	if b.viewUse == nil {
		b.viewUse = map[string]int{}
	}
	b.viewUse[id]++
	b.viewMu.Unlock()
	return b.releaser(id)
}

// releaser lets a view in use go (once), and sets the timer again: one in use was left
// out of it.
func (b *Backups) releaser(id string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			b.viewMu.Lock()
			if b.viewUse[id]--; b.viewUse[id] <= 0 {
				delete(b.viewUse, id)
			}
			b.viewMu.Unlock()
			b.armViews()
		})
	}
}

// dropView removes what a view holds: its temporary database and user, its dump and,
// last, its record — a failure leaves the record for the next sweep.
func (b *Backups) dropView(v viewRecord) error {
	bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := b.Data.mongoOps().DropTemp(bg, v.TempDB); err != nil {
		return fmt.Errorf("geçici veritabanı silinemedi: %w", err)
	}
	if err := b.Data.Staging.Remove(b.viewStaging(v.ID)); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.Remove(b.viewPath(v.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// useView holds a ready view of site and run for a restore: no sweep or close takes it
// meanwhile (one being closed is refused). release lets it go.
func (b *Backups) useView(site, id, run string) (viewRecord, func(), error) {
	b.viewMu.Lock()
	if b.viewClosing[id] {
		b.viewMu.Unlock()
		return viewRecord{}, nil, errViewGone
	}
	v, err := b.loadView(id)
	switch {
	case err != nil:
		b.viewMu.Unlock()
		return v, nil, err
	case v.Site != site || v.Run != run || !v.Ready:
		b.viewMu.Unlock()
		return viewRecord{}, nil, errViewGone
	case !b.now().Before(v.Expires):
		b.viewMu.Unlock()
		return viewRecord{}, nil, errors.New("açılan yedeğin süresi doldu: yedeği yeniden aç (önizleme yeniden hazırlanır)")
	}
	if b.viewUse == nil {
		b.viewUse = map[string]int{}
	}
	b.viewUse[id]++
	b.viewMu.Unlock()
	return v, b.releaser(id), nil
}

// viewFor returns a ready view of site to browse.
func (b *Backups) viewFor(site, id string) (viewRecord, error) {
	v, err := b.loadView(id)
	if err != nil || v.Site != site || !v.Ready {
		return viewRecord{}, errViewGone
	}
	if !b.now().Before(v.Expires) {
		return viewRecord{}, errViewGone // the timer closes it
	}
	return v, nil
}

// SweepViews closes views past their time, and leftovers of an opening that never
// finished (not ready and not held: the agent stopped while it opened): their temporary
// databases and dumps. A close that fails (MongoDB down) is tried again by the timer,
// less and less often.
func (b *Backups) SweepViews() []string {
	b.init()
	var closed []string
	failed := false
	now := b.now()
	for _, v := range b.viewRecords() {
		b.viewMu.Lock()
		skip := b.viewUse[v.ID] > 0 || b.viewClosing[v.ID]
		b.viewMu.Unlock()
		if skip || (v.Ready && now.Before(v.Expires)) {
			continue
		}
		if err := b.dropView(v); err != nil {
			failed = true
			b.logf("açılan yedek kapatılamadı (%s, %s): %v", v.Site, v.ID, err)
			continue
		}
		closed = append(closed, v.Site+" ("+localTime(v.Taken)+")")
	}
	b.viewMu.Lock()
	if failed {
		b.viewWait = min(max(2*b.viewWait, 2*time.Minute), 30*time.Minute)
	} else {
		b.viewWait = 0
	}
	b.viewMu.Unlock()
	b.armViews()
	return closed
}

// armViews sets the timer for the next view to close: the earliest end of the ready
// views not in use (a restore that lets one go sets it again), never sooner than a
// minute from now — longer while closes keep failing.
func (b *Backups) armViews() {
	b.viewMu.Lock()
	used := map[string]bool{}
	for id, n := range b.viewUse {
		used[id] = n > 0
	}
	b.viewMu.Unlock()
	var next time.Time
	for _, v := range b.viewRecords() {
		if v.Ready && !used[v.ID] && (next.IsZero() || v.Expires.Before(next)) {
			next = v.Expires
		}
	}
	b.viewMu.Lock()
	defer b.viewMu.Unlock()
	if b.viewTimer != nil {
		b.viewTimer.Stop()
		b.viewTimer = nil
	}
	if next.IsZero() {
		return
	}
	b.viewTimer = time.AfterFunc(viewTimerWait(next, b.now(), b.viewWait), func() { b.SweepViews() })
}

// viewTimerWait is how long the timer waits for a view that ends at next: a second past
// its end, but never under a minute nor under floor (closes failing).
func viewTimerWait(next, now time.Time, floor time.Duration) time.Duration {
	return max(next.Sub(now)+time.Second, time.Minute, floor)
}

// StartViews is the agent's start: credentials a cut-off restore, export or read left in
// the staging area are removed (no job runs yet), and a minute later — once MongoDB is
// up — the views past their time and those of an opening cut short are closed and the
// timer is set for the rest.
func (b *Backups) StartViews(ctx context.Context) {
	b.init()
	if ents, err := os.ReadDir(b.Data.Staging.Root); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), resticPrefix) && e.IsDir() {
				if err := b.Data.Staging.Remove(e.Name()); err == nil {
					b.logf("yarıda kalan işin yedek deposu bilgileri silindi: %s", e.Name())
				}
			}
		}
	}
	go func() {
		if sleepCtx(ctx, time.Minute) != nil {
			return
		}
		for _, v := range b.SweepViews() {
			b.logf("açılan yedek kapatıldı: %s", v)
		}
	}()
}

// ViewContext makes ctx browse a ready view of site: the database browser logs in with
// its temporary user instead of the site's own.
func (b *Backups) ViewContext(ctx context.Context, site, id string) (BackupView, context.Context, error) {
	v, err := b.viewFor(site, id)
	if err != nil {
		return BackupView{}, ctx, err
	}
	return v.BackupView, withDBCreds(ctx, v.creds()), nil
}

// ---- restoring ----

// BackupRestore says what of a backup is restored.
type BackupRestore struct {
	Run   string `json:"run"`
	DB    bool   `json:"db"`
	Files bool   `json:"files"`
	// View is the opened backup whose database is restored: what was counted there is
	// what must arrive. AcceptDiff accepts that it differs from the run's record.
	View       string `json:"view,omitempty"`
	AcceptDiff bool   `json:"accept_diff,omitempty"`
}

// Restore puts a backup's database, its files or both back into the site, through the
// same apply as an import (rules K1, K3). The opened view is closed afterwards.
func (b *Backups) Restore(ctx context.Context, t *Task, site string, req BackupRestore, actor Actor) (Applied, error) {
	b.init()
	defer b.Data.dropStaging(t)
	pt, err := b.SiteBackup(site, req.Run)
	if err != nil {
		return Applied{}, err
	}
	plan := restorePlan{Reason: "before-restore", DBMode: ModeReplace, FilesMode: ModeReplace}
	var what []string
	release := func() {}
	if req.DB {
		v, rel, err := b.useView(site, req.View, req.Run)
		if err != nil {
			return Applied{}, err
		}
		release = rel
		defer rel()
		if v.RecordDiff != "" && !req.AcceptDiff {
			return Applied{}, fmt.Errorf("açılan yedeğin sayımları yedek alınırken sayılanla tutmuyor (%s): yine de geri yüklemek için önizlemede farkı onayla", v.RecordDiff)
		}
		// Its dump is checked before the site stops: whatever happened to it, nothing is
		// dropped for a file that is not there.
		if st, err := os.Stat(b.viewDump(v.ID)); err != nil || st.Size() != v.Size {
			return Applied{}, errors.New("açılan yedeğin dökümü bulunamadı: yedeği yeniden aç")
		}
		plan.DB = &restoreSource{Label: "yedek " + localTime(v.Taken), Path: b.viewDump(v.ID),
			Sec: dbSection{Length: v.Size, SHA256: v.SHA256}, DB: v.DB, Keep: append(sortedKeys(v.Collections), v.Views...),
			Expect: v.Collections, Size: v.Size}
		what = append(what, fmt.Sprintf("veritabanı (%d koleksiyon, %d belge)", len(v.Collections), v.Documents))
	}
	if req.Files {
		snap, ok := pt.Parts[PartFiles]
		if !ok {
			return Applied{}, errors.New("bu yedekte dosyalar yok")
		}
		sec, err := b.secret()
		if err != nil {
			return Applied{}, err
		}
		env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
		if err != nil {
			return Applied{}, err
		}
		defer done()
		plan.Files = &filesSource{Label: "yedek " + localTime(snap.Time), Expect: datafs.Stats{Files: int(snap.Files), Bytes: snap.Bytes},
			Fill: func(ctx context.Context, t *Task, sf siteFiles, dir string) (datafs.Stats, error) {
				return b.restoreFiles(ctx, t, env, snap, sf, dir)
			}}
		what = append(what, fmt.Sprintf("dosyalar (%d dosya, %s)", snap.Files, humanBytes(snap.Bytes)))
	}
	if plan.DB == nil && plan.Files == nil {
		return Applied{}, errors.New("geri yüklenecek bir şey seçilmedi")
	}
	t.Step(fmt.Sprintf("ℹ Geri yüklenecek yedek: %s (%s) — %s", localTime(pt.Time), runKindText(pt.Kind), strings.Join(what, ", ")))
	res, err := b.Data.apply(ctx, t, site, plan, actor)
	if err != nil {
		return res, err
	}
	if req.DB {
		release()
		if err := b.closeView(req.View); err != nil {
			t.Step("⚠ Açılan yedek kapatılamadı (süresi dolunca kapanır): " + err.Error())
		} else {
			t.Step("✓ Açılan yedek kapatıldı (geçici veritabanı silindi)")
		}
	}
	if res.Snapshot != "" {
		t.Step(fmt.Sprintf("ℹ Veritabanını geri almak için: site sayfası → Veritabanı kopyaları → %s → Geri yükle", res.Snapshot))
	}
	if res.FilesOld != "" {
		t.Step(fmt.Sprintf("ℹ Dosyaları geri almak için: site sayfası → Dosya kopyaları → %s → Geri yükle", res.FilesOld))
	}
	return res, nil
}

// restoreFiles fills dir (a fresh data.new) with a backup's files. restic restores them
// with their owners, modes and times (its container may change owners and override
// permissions, nothing else), and they must add up to what the backup read. The top
// takes data/'s owner and mode; if every file belonged to one other user — the image's
// user changed since — they are given to the site's user now.
func (b *Backups) restoreFiles(ctx context.Context, t *Task, env *resticEnv, snap BackupSnapshot, sf siteFiles, dir string) (datafs.Stats, error) {
	t.Step(fmt.Sprintf("… Dosyalar yedekten açılıyor: %d dosya, %s (site bu sırada çalışmaya devam eder)", snap.Files, humanBytes(snap.Bytes)))
	start := b.now()
	var restored, total int64
	lines := &jsonLines{each: func(line []byte) {
		var m struct {
			Type          string `json:"message_type"`
			TotalFiles    int64  `json:"total_files"`
			FilesRestored int64  `json:"files_restored"`
			TotalBytes    int64  `json:"total_bytes"`
			BytesRestored int64  `json:"bytes_restored"`
		}
		if json.Unmarshal(line, &m) != nil {
			return
		}
		switch m.Type {
		case "status":
			t.SetBytes(m.BytesRestored, m.TotalBytes)
		case "summary":
			restored, total = m.FilesRestored, m.TotalFiles
		}
	}}
	res, err := b.restic(ctx, t, env, resticOpts{caps: []string{"CHOWN", "FOWNER", "DAC_OVERRIDE"},
		rw: []bindMount{{dir, "/restore"}}, out: lines},
		"restore", "--json", "--no-lock", snap.ID+":"+partPath(snap.Site, PartFiles), "--target", "/restore")
	lines.flush()
	if err != nil {
		return datafs.Stats{}, fmt.Errorf("dosyalar yedekten açılamadı: %w", goneOr(err))
	}
	if restored != total {
		return datafs.Stats{}, fmt.Errorf("restic %d öğeden %d tanesini açtı", total, restored)
	}
	top, err := os.Lstat(sf.Dir)
	if err != nil {
		return datafs.Stats{}, err
	}
	if err := os.Chown(dir, sf.UID, sf.GID); err != nil {
		return datafs.Stats{}, err
	}
	if err := os.Chmod(dir, top.Mode().Perm()); err != nil {
		return datafs.Stats{}, err
	}
	sv, err := survey(dir)
	if err != nil {
		return datafs.Stats{}, fmt.Errorf("açılan dosyalar sayılamadı: %w", err)
	}
	if int64(sv.Files) != snap.Files || sv.Bytes != snap.Bytes {
		return datafs.Stats{}, fmt.Errorf("açılan dosyalar yedektekinden farklı: %d dosya / %s, yedekte %d / %s",
			sv.Files, humanBytes(sv.Bytes), snap.Files, humanBytes(snap.Bytes))
	}
	// One owner for everything that is not the folder's owner now: the site's user
	// changed since (the site was made again with another image) only if the folder was
	// that user's in the backup too; a folder kept deliberately apart (root's, say) keeps
	// its files' owners.
	if owner, ok := soleOwner(sv.Owners); ok && owner != sf.UID {
		was, err := b.backupRootUID(ctx, t, env, snap)
		switch {
		case err != nil:
			t.Step("ℹ Yedekteki klasörün sahibi okunamadı; dosyaların sahibi değiştirilmedi: " + err.Error())
		case was == owner:
			if err := reown(dir, sf.UID, sf.GID); err != nil {
				return datafs.Stats{}, err
			}
			t.Step(fmt.Sprintf("ℹ Yedekte klasör ve dosyalar kullanıcı %d'indi; sitenin şu anki kullanıcısına (%d) verildi", owner, sf.UID))
		default:
			t.Step(fmt.Sprintf("ℹ Dosyalar yedekteki gibi kullanıcı %d'in (klasör yedekte %d'indi)", owner, was))
		}
	}
	// Set-user-ID and set-group-ID bits do not come back with a site's files.
	if sv.Special > 0 {
		n, err := clearSetID(dir)
		if err != nil {
			return datafs.Stats{}, err
		}
		t.Step(fmt.Sprintf("ℹ %d dosyanın setuid / setgid izni kaldırıldı", n))
	}
	t.Step(fmt.Sprintf("✓ Dosyalar açıldı: %d dosya, %d klasör, %s; sahip ve izinler yedekteki gibi (%s%s)", sv.Files, sv.Dirs,
		humanBytes(sv.Bytes), took(b.now().Sub(start)), memNote(res.Usage.PeakMem)))
	st, err := countFiles(dir)
	if err != nil {
		return datafs.Stats{}, err
	}
	return st, nil
}

func memNote(peak uint64) string {
	if peak == 0 {
		return ""
	}
	return ", en çok " + humanBytes(int64(peak)) + " bellek"
}

// localTime writes a time as the panel does, in the server's time zone.
func localTime(t time.Time) string { return t.Local().Format("02.01.2006 15:04") }

func runKindText(kind string) string {
	switch kind {
	case RunDaily:
		return "gece yedeği"
	case RunHourly:
		return "saatlik yedek"
	case RunManual:
		return "elle alınan yedek"
	}
	return "yedek"
}

// ---- browsing a view ----

type dbCredsKey struct{}

// withDBCreds makes the database browser log in with c instead of the site's own user.
func withDBCreds(ctx context.Context, c dbCreds) context.Context {
	return context.WithValue(ctx, dbCredsKey{}, c)
}

func dbCredsFrom(ctx context.Context) (dbCreds, bool) {
	c, ok := ctx.Value(dbCredsKey{}).(dbCreds)
	return c, ok
}
