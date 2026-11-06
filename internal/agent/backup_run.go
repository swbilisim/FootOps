package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Kinds of runs; snapshots carry theirs as a tag (kind=…), retention ignores it.
const (
	RunDaily  = "daily" // the night run
	RunHourly = "hourly"
	RunManual = "manual"

	// Parts of a site, and the server's own.
	PartDB     = "db"
	PartFiles  = "files"
	PartDef    = "def"
	PartServer = "server"
)

const (
	// RunsKeep: run records older than this are removed (the repository keeps the
	// snapshots themselves for as long as retention says).
	RunsKeep = 62 * 24 * time.Hour
	runsMax  = 2000
	// backupRoom is what restic's cache and temporary packs may need on the disk.
	backupRoom = 512 << 20
	// pruneEvery: prune and check run on the weekly night, or on the next night once
	// this much has passed since the last.
	pruneEvery = 7 * 24 * time.Hour
)

// BackupPart is one snapshot a run took (or tried to).
type BackupPart struct {
	Site     string `json:"site"` // "_server" for the server
	Part     string `json:"part"`
	Snapshot string `json:"snapshot,omitempty"`
	Err      string `json:"error,omitempty"`
	// Warning: saved, but not everything could be read.
	Warning string `json:"warning,omitempty"`
	// Collections → documents as mongodump counted them (database part).
	Collections map[string]int64 `json:"collections,omitempty"`
	Documents   int64            `json:"documents,omitempty"`
	// Files and Bytes are what restic read (for the database: the dump).
	Files       int64         `json:"files,omitempty"`
	Bytes       int64         `json:"bytes,omitempty"`
	Added       int64         `json:"added,omitempty"`
	AddedPacked int64         `json:"added_packed,omitempty"` // what the repository grew by
	Took        time.Duration `json:"took,omitempty"`
	Usage       run.Usage     `json:"usage"`
}

// BackupRun is the record of one run (runs/<id>.json; the panel's history and the
// counts a restore is checked against).
type BackupRun struct {
	ID       string       `json:"id"` // the job's
	Kind     string       `json:"kind"`
	Site     string       `json:"site,omitempty"` // a manual run of one site
	Actor    string       `json:"actor,omitempty"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished"`
	Parts    []BackupPart `json:"parts"`
	// Removed is how many snapshots retention removed (Forgot: it ran clean, ForgetErr:
	// it failed); Pruned / Checked tell the weekly maintenance ran to its end (PruneErr,
	// CheckErr: what went wrong).
	Removed   int       `json:"removed,omitempty"`
	Forgot    bool      `json:"forgot,omitempty"`
	ForgetErr string    `json:"forget_error,omitempty"`
	Pruned    bool      `json:"pruned,omitempty"`
	PruneErr  string    `json:"prune_error,omitempty"`
	Checked   bool      `json:"checked,omitempty"`
	CheckErr  string    `json:"check_error,omitempty"`
	RepoSize  int64     `json:"repo_size,omitempty"`
	Err       string    `json:"error,omitempty"`
	Usage     run.Usage `json:"usage"`
}

// runPlan is what one run does.
type runPlan struct {
	Kind string
	// Night is the night a daily run is for (it may start late: busy, retried).
	Night    time.Time
	Site     string // manual, one site
	Sites    []sitePlan
	Server   bool
	Forget   bool
	Maintain bool // prune + check
	Stats    bool
}

type sitePlan struct {
	Spec           sites.Spec
	DB, Files, Def bool
	Freeze         bool
}

func (p runPlan) empty() bool {
	for _, s := range p.Sites {
		if s.DB || s.Files || s.Def {
			return false
		}
	}
	return !p.Server && !p.Forget && !p.Maintain
}

// Target is the job's target: what the panel shows and what a restart is checked
// against.
func (p runPlan) Target() string {
	switch {
	case p.Site != "":
		return p.Site
	case p.Kind == RunDaily:
		return "gece yedeği"
	case p.Kind == RunHourly:
		var names []string
		for _, s := range p.Sites {
			names = append(names, s.Spec.Name)
		}
		return "saatlik yedek: " + strings.Join(names, ", ")
	}
	return "tüm siteler"
}

// Plan works out a run: the night run (kind daily) of the night at now, an hourly run at
// now's hour, or a manual one (site empty: every site and the server).
func (b *Backups) Plan(kind, site string, now time.Time) (runPlan, error) {
	b.init()
	s, err := b.Settings()
	if err != nil {
		return runPlan{}, err
	}
	all, err := b.Data.Store.List()
	if err != nil {
		return runPlan{}, err
	}
	p := runPlan{Kind: kind, Site: site}
	local := now.In(b.location())
	weekly := local.Weekday() == filesWeekday
	slot := 0
	if night := s.nightAt(local); kind == RunHourly {
		slot = ((local.Hour()-night.Hour())%24 + 24) % 24
	}
	found := false
	for _, spec := range all {
		if site != "" && spec.Name != site {
			continue
		}
		found = true
		pol := s.For(spec.Name)
		if pol.Off && site == "" {
			continue
		}
		sp := sitePlan{Spec: spec, Freeze: pol.Freeze}
		hasFiles := spec.DataMount != "" && pol.Files != FilesOff
		switch kind {
		case RunHourly:
			sp.DB = spec.Database && (pol.DB == DBHourly || (pol.DB == DB6h && slot%6 == 0))
		case RunDaily:
			sp.DB, sp.Def = spec.Database, true
			sp.Files = hasFiles && (pol.Files == FilesDaily || weekly)
		default:
			sp.DB, sp.Files, sp.Def = spec.Database, hasFiles, true
		}
		if sp.DB || sp.Files || sp.Def {
			p.Sites = append(p.Sites, sp)
		}
	}
	if site != "" && !found {
		return p, sites.ErrNotFound
	}
	switch kind {
	case RunDaily:
		p.Night = local
		p.Server, p.Forget, p.Stats = true, true, true
		// Weekly; a failed prune or a check that never finished is tried again the next night.
		st := b.state()
		p.Maintain = weekly || now.Sub(st.LastPrune) >= pruneEvery || now.Sub(st.LastCheck) >= pruneEvery || st.PruneFailed != nil
	case RunManual:
		p.Server = site == ""
	}
	return p, nil
}

// errNotConfirmed: the password was never confirmed as kept elsewhere.
var errNotConfirmed = errors.New("yedek deposunun şifresi henüz onaylanmadı: şifreyi kaydettiğini onayla (yedekleme ondan sonra başlar)")

// Run backs up what p says. A part that fails does not stop the others, unless the
// repository itself cannot be used. The record is kept whatever happens.
func (b *Backups) Run(ctx context.Context, t *Task, p runPlan, actor Actor) (rec BackupRun, err error) {
	b.init()
	rec = BackupRun{ID: t.ID(), Kind: p.Kind, Site: p.Site, Actor: actor.User, Started: b.now().UTC()}
	repo := ""
	defer func() {
		rec.Finished = b.now().UTC()
		rec.Usage = t.Usage()
		if err != nil {
			rec.Err = clipMsg(err.Error(), 2000)
		}
		b.saveRun(rec)
		b.recordState(p, rec, repo, err)
	}()
	// A site an earlier run could not unpause gets another try first.
	b.Recover(ctx)
	sec, err := b.secret()
	if err != nil {
		return rec, err
	}
	repo = sec.Repository
	settings, err := b.Settings()
	if err != nil {
		return rec, err
	}
	if settings.Confirmed.IsZero() {
		return rec, errNotConfirmed
	}
	if err := os.MkdirAll(b.CacheDir, 0o700); err != nil {
		return rec, err
	}
	if err := b.Data.checkFree(b.CacheDir, backupRoom); err != nil {
		return rec, err
	}
	env, done, err := b.prepare(ctx, t, sec)
	if err != nil {
		return rec, err
	}
	defer done()
	t.Step(fmt.Sprintf("… yedek deposu: %s · sınır: %s", repoLabel(sec.Repository), limitText(b.Data.Limits)))

	if !b.unlocked {
		// Only this server writes its repository and nothing of ours runs now (tool
		// containers left by a restart were removed at start), so every lock there is
		// one a cut-off run left behind.
		if _, err := b.restic(ctx, t, env, resticOpts{out: io.Discard}, "unlock", "--remove-all"); err != nil {
			return rec, fmt.Errorf("yedek deposuna ulaşılamadı: %w", err)
		}
		b.unlocked = true
	}

	tags := func(site, part string) string {
		return strings.Join([]string{"footops", "site=" + site, "part=" + part, "kind=" + p.Kind, "run=" + t.ID()}, ",")
	}
	var failed []string
	fail := func(bp BackupPart) {
		failed = append(failed, fmt.Sprintf("%s (%s): %s", bp.Site, partName(bp.Part), bp.Err))
	}
	for _, sp := range p.Sites {
		for _, part := range []string{PartDB, PartFiles, PartDef} {
			want := map[string]bool{PartDB: sp.DB, PartFiles: sp.Files, PartDef: sp.Def}[part]
			if !want {
				continue
			}
			if err := ctx.Err(); err != nil {
				return rec, err
			}
			var bp BackupPart
			var perr error
			switch part {
			case PartDB:
				bp, perr = b.backupDB(ctx, t, env, sp.Spec, tags(sp.Spec.Name, part))
			case PartFiles:
				bp, perr = b.backupFiles(ctx, t, env, sp.Spec, sp.Freeze, tags(sp.Spec.Name, part))
			case PartDef:
				bp, perr = b.backupDef(ctx, t, env, sp.Spec, tags(sp.Spec.Name, part))
			}
			rec.Parts = append(rec.Parts, bp)
			if perr != nil {
				if ctx.Err() != nil {
					return rec, ctx.Err()
				}
				if isFatal(perr) {
					return rec, perr
				}
				fail(bp)
			}
		}
	}
	if p.Server {
		if err := ctx.Err(); err != nil {
			return rec, err
		}
		bp, perr := b.backupServer(ctx, t, env, tags(serverPart, PartServer))
		rec.Parts = append(rec.Parts, bp)
		if perr != nil {
			if ctx.Err() != nil {
				return rec, ctx.Err()
			}
			if isFatal(perr) {
				return rec, perr
			}
			fail(bp)
		}
	}
	if p.Forget {
		n, err := b.forget(ctx, t, env, settings)
		switch {
		case errors.Is(err, errFuture):
			t.Step("⚠ " + err.Error())
			failed = append(failed, err.Error())
			rec.ForgetErr = err.Error()
		case err != nil:
			if ctx.Err() != nil || isFatal(err) {
				return rec, err
			}
			failed = append(failed, "saklama kuralı: "+err.Error())
			rec.ForgetErr = clipMsg("saklama kuralı: "+err.Error(), 1000)
		default:
			rec.Forgot = true
		}
		rec.Removed = n
	}
	if p.Maintain {
		if err := b.maintain(ctx, t, env, &rec); err != nil {
			if ctx.Err() != nil || isFatal(err) {
				return rec, err
			}
			failed = append(failed, err.Error())
		}
	}
	if p.Stats {
		if size, err := b.repoSize(ctx, t, env); err == nil {
			rec.RepoSize = size
			t.Step("✓ Depoda yedeklerin kapladığı: " + humanBytes(size))
		} else if ctx.Err() != nil {
			return rec, ctx.Err()
		} else {
			t.Step("⚠ Depo boyutu ölçülemedi: " + err.Error())
		}
	}
	// The panel lists backups from the index: brought up to date whenever snapshots came
	// or went.
	if rec.Removed > 0 || savedAny(rec) {
		b.updateIndex(ctx, t, env)
	}
	if len(failed) > 0 {
		return rec, fmt.Errorf("%d işlem başarısız: %s", len(failed), strings.Join(failed, " · "))
	}
	var added int64
	for _, bp := range rec.Parts {
		added += bp.AddedPacked
	}
	t.SetResult(fmt.Sprintf("%d parça, depoya %s eklendi", len(rec.Parts), humanBytes(added)))
	return rec, nil
}

// savedAny reports whether a run saved a snapshot.
func savedAny(rec BackupRun) bool {
	for _, p := range rec.Parts {
		if p.Snapshot != "" {
			return true
		}
	}
	return false
}

func partName(p string) string {
	switch p {
	case PartDB:
		return "veritabanı"
	case PartFiles:
		return "dosyalar"
	case PartDef:
		return "site tanımı"
	case PartServer:
		return "sunucu ayarları"
	}
	return p
}

// recordState keeps each part's last success, what failed (for the alerts), and which
// night ran. A night run that backed up nothing, or stopped because the repository could
// not be used, is tried again in an hour; one that was cancelled or ran out of time is
// not (the operator stopped it, or the next try would end the same way). A cancel is
// no failure: what it cut short keeps its earlier state.
func (b *Backups) recordState(p runPlan, rec BackupRun, repo string, runErr error) {
	cancelled := errors.Is(runErr, context.Canceled)
	var current map[string]bool // the sites there are, for the night's tidying
	if p.Kind == RunDaily {
		if all, err := b.Data.Store.List(); err == nil {
			current = map[string]bool{serverPart: true}
			for _, sp := range all {
				current[sp.Name] = true
			}
		}
	}
	b.updateState(func(st *backupState) {
		if repo != "" && st.Repository != repo {
			*st = backupState{Repository: repo, Last: map[string]BackupLast{}, Failed: map[string]BackupFailure{}}
		}
		saved := false
		for _, bp := range rec.Parts {
			key := bp.Site + "/" + bp.Part
			switch {
			case bp.Snapshot != "":
				st.Last[key] = BackupLast{Time: rec.Started, Snapshot: bp.Snapshot, Run: rec.ID}
				saved = true
				if bp.Warning != "" {
					st.Failed[key] = BackupFailure{Time: rec.Started, Err: bp.Warning, Warning: true, Run: rec.ID}
				} else {
					delete(st.Failed, key)
				}
			case bp.Err != "" && !(cancelled && strings.Contains(bp.Err, context.Canceled.Error())):
				// Only the part the cancel cut short is no failure.
				st.Failed[key] = BackupFailure{Time: rec.Finished, Err: bp.Err, Run: rec.ID}
			}
		}
		switch {
		case saved:
			st.RunFailed = nil
		case runErr != nil && !cancelled && (len(rec.Parts) == 0 || isFatal(runErr)):
			st.RunFailed = &BackupFailure{Time: rec.Finished, Err: clipMsg(runErr.Error(), 1000), Run: rec.ID}
		}
		switch {
		case rec.ForgetErr != "":
			st.ForgetFailed = &BackupFailure{Time: rec.Finished, Err: rec.ForgetErr, Run: rec.ID}
		case rec.Forgot:
			st.ForgetFailed = nil
		}
		switch {
		case rec.PruneErr != "":
			st.PruneFailed = &BackupFailure{Time: rec.Finished, Err: rec.PruneErr, Run: rec.ID}
		case rec.Pruned:
			st.PruneFailed = nil
		}
		// Failures of sites that are gone are forgotten (their last backups stay).
		for key := range st.Failed {
			if site, _, _ := strings.Cut(key, "/"); current != nil && !current[site] {
				delete(st.Failed, key)
			}
		}
		if p.Kind == RunDaily {
			night := p.Night
			if night.IsZero() {
				night = rec.Started
			}
			switch {
			case errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded):
				st.NightDone = night.In(b.location()).Format("2006-01-02")
			case isFatal(runErr) || (runErr != nil && !saved):
				b.nightNotBefore = rec.Finished.Add(time.Hour - time.Minute)
			default:
				st.NightDone = night.In(b.location()).Format("2006-01-02")
			}
		}
		if rec.Pruned {
			st.LastPrune = rec.Finished
		}
		if rec.Checked {
			st.LastCheck, st.CheckFailed = rec.Finished, rec.CheckErr
		}
		if rec.RepoSize > 0 {
			st.RepoSize, st.RepoSized = rec.RepoSize, rec.Finished
		}
	})
}

// ---- parts ----

func partPath(site, part string) string {
	switch part {
	case PartDB:
		return backupRoot + "/" + site + "/db.archive"
	case PartServer:
		return backupRoot + "/" + serverPart
	}
	return backupRoot + "/" + site + "/" + part
}

// finish fills a part from restic's report and writes its step.
func (b *Backups) finish(t *Task, bp *BackupPart, res resticResult, start time.Time, what string) {
	bp.Took, bp.Usage = b.now().Sub(start), res.Usage
	if s := res.Summary; s != nil {
		bp.Snapshot, bp.Files, bp.Bytes, bp.Added, bp.AddedPacked = s.SnapshotID, s.TotalFiles, s.TotalBytes, s.DataAdded, s.DataAddedPacked
	}
	used := ""
	if res.Usage.PeakMem > 0 {
		used = ", en çok " + humanBytes(int64(res.Usage.PeakMem)) + " bellek"
	}
	mark := "✓"
	if bp.Warning != "" {
		mark = "⚠"
	}
	t.Step(fmt.Sprintf("%s %s · %s: %s → depoya %s eklendi (%s%s)%s", mark, bp.Site, partName(bp.Part), what,
		humanBytes(bp.AddedPacked), took(bp.Took), used, warnText(bp.Warning)))
}

func warnText(w string) string {
	if w == "" {
		return ""
	}
	return " — " + w
}

func (b *Backups) failPart(t *Task, bp *BackupPart, err error) error {
	bp.Err = clipMsg(err.Error(), 1000)
	if errors.Is(err, context.DeadlineExceeded) {
		bp.Err = "süre doldu: yedek işi zaman sınırını aştı"
	}
	t.Step(fmt.Sprintf("✗ %s · %s: %s", bp.Site, partName(bp.Part), bp.Err))
	return err
}

// backupDB backs up the site's database: restic starts mongodump in the tool container
// (MongoDB's network namespace, the site's own user — rule K4) and saves its archive
// only if mongodump finished.
func (b *Backups) backupDB(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, tags string) (BackupPart, error) {
	bp := BackupPart{Site: spec.Name, Part: PartDB}
	start := b.now()
	_, creds, err := b.Data.siteDB(spec.Name)
	if err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	st, err := b.Data.stats(ctx, creds)
	if err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	// The site's credentials sit next to the repository's only while its dump runs.
	cfg := filepath.Join(env.cfgDir, "mongo.yaml")
	uri := (&url.URL{Scheme: "mongodb", User: url.UserPassword(creds.User, creds.Password), Host: "127.0.0.1:27017", Path: "/",
		RawQuery: url.Values{"authSource": {creds.DB}}.Encode()}).String()
	if err := writeFile(cfg, "uri: "+q(uri)+"\n", 0o600); err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	defer os.Remove(cfg)
	// mongodump writes its counts and any failure to a file in the job's temporary
	// directory: restic passes a command's stderr on, but may drop its last lines when
	// the command exits (seen in Y1: the last collection's count went missing).
	logName := "mongodump-" + spec.Name + ".log"
	logPath := filepath.Join(env.tmpDir, logName)
	_ = os.Remove(logPath)
	defer os.Remove(logPath)
	res, err := b.restic(ctx, t, env, resticOpts{mongo: true},
		"backup", "--json", "--host", b.Server, "--tag", tags, "--stdin-filename", partPath(spec.Name, PartDB),
		"--stdin-from-command", "--", "sh", "-c", `exec mongodump "$@" 2>>/tmp/`+logName, "mongodump",
		"--config=/run/footops/mongo.yaml", "--db="+creds.DB, "--archive", "--numParallelCollections=1")
	dumped, failure := readDumpLog(logPath)
	bp.Collections = map[string]int64{}
	for ns, n := range dumped {
		bp.Collections[strings.TrimPrefix(ns, creds.DB+".")] = n
		bp.Documents += n
	}
	if err != nil {
		var re *resticError
		if errors.As(err, &re) && !re.Fatal && ctx.Err() == nil && failure != "" {
			// mongodump's own words say more than restic's "command failed".
			err = &resticError{Code: re.Code, Msg: "mongodump başarısız, yedek alınmadı: " + failure}
		}
		return bp, b.failPart(t, &bp, err)
	}
	if res.Summary == nil || res.Summary.SnapshotID == "" {
		return bp, b.failPart(t, &bp, errors.New("restic anlık görüntü bildirmedi"))
	}
	if bp.Documents != st.Objects {
		t.Step(fmt.Sprintf("ℹ %s: belge sayısı başta %d idi, yedekte %d (site bu sırada yazmış olabilir)", spec.Name, st.Objects, bp.Documents))
	}
	b.finish(t, &bp, res, start, fmt.Sprintf("%d koleksiyon, %d belge (%s döküm)", len(bp.Collections), bp.Documents, humanBytes(res.Summary.TotalBytes)))
	return bp, nil
}

// readDumpLog reads mongodump's log line by line: the "done dumping" counts and its
// last failure.
func readDumpLog(path string) (map[string]int64, string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	counts := map[string]int64{}
	failure := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		if m := reDumped.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			counts[m[1]] = n
			continue
		}
		if i := strings.Index(line, "Failed"); i >= 0 {
			failure = clipMsg(strings.TrimSpace(line[i:]), 500)
		}
	}
	return counts, failure
}

// backupFiles backs up the site's data directory, mounted read-only; the container may
// read every file (the app's own user owns them, some 0600) and nothing else. With
// freeze the site is paused only while its directory is copied (seconds for the small
// directories of apps that keep a database in a file, SQLite), and the copy is what is
// backed up: the pause never waits for the network.
func (b *Backups) backupFiles(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, freeze bool, tags string) (BackupPart, error) {
	bp := BackupPart{Site: spec.Name, Part: PartFiles}
	start := b.now()
	sf, err := b.Data.siteFiles(spec.Name)
	if err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	src := sf.Dir
	args := []string{"backup", "--json", "--host", b.Server, "--tag", tags}
	if freeze {
		copied, err := b.frozenCopy(ctx, t, env, spec, sf)
		if err != nil {
			return bp, b.failPart(t, &bp, err)
		}
		if copied != "" {
			defer os.RemoveAll(copied)
			// A fresh copy has new inode numbers and change times: unchanged files are
			// told by their size and modification time (cp -a keeps both).
			src, args = copied, append(args, "--ignore-inode")
		}
	}
	path := partPath(spec.Name, PartFiles)
	res, err := b.restic(ctx, t, env, resticOpts{caps: []string{"DAC_READ_SEARCH"}, mounts: []bindMount{{src, path}}},
		append(args, path)...)
	err = partialOK(&bp, res, err)
	if err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	s := res.Summary
	b.finish(t, &bp, res, start, fmt.Sprintf("%d dosya, %s (yeni %d, değişen %d)", s.TotalFiles, humanBytes(s.TotalBytes), s.FilesNew, s.FilesChanged))
	return bp, nil
}

// partialOK accepts a backup that saved a snapshot but could not read everything
// (restic exit 3) as a part with a warning — if it read anything at all.
func partialOK(bp *BackupPart, res resticResult, err error) error {
	var re *resticError
	if errors.As(err, &re) && re.Code == 3 && res.Summary != nil && res.Summary.SnapshotID != "" && res.Summary.TotalFiles > 0 {
		bp.Warning = re.Msg
		return nil
	}
	if err == nil && (res.Summary == nil || res.Summary.SnapshotID == "") {
		return errors.New("restic anlık görüntü bildirmedi")
	}
	return err
}

// backupDef backs up what recreating the site takes: footops.json and compose.yaml
// (with the environment's secrets; inside the encrypted repository only).
func (b *Backups) backupDef(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, tags string) (BackupPart, error) {
	bp := BackupPart{Site: spec.Name, Part: PartDef}
	start := b.now()
	dir := filepath.Join(b.Data.Store.Root, spec.Dir())
	path := partPath(spec.Name, PartDef)
	var mounts []bindMount
	for _, f := range []string{sites.MetaFile, sites.ComposeFile} {
		if st, err := os.Lstat(filepath.Join(dir, f)); err != nil || !st.Mode().IsRegular() {
			return bp, b.failPart(t, &bp, fmt.Errorf("%s okunamadı", f))
		}
		mounts = append(mounts, bindMount{filepath.Join(dir, f), path + "/" + f})
	}
	res, err := b.restic(ctx, t, env, resticOpts{mounts: mounts}, "backup", "--json", "--host", b.Server, "--tag", tags, path)
	if err == nil && (res.Summary == nil || res.Summary.SnapshotID == "") {
		err = errors.New("restic anlık görüntü bildirmedi")
	}
	if err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	b.finish(t, &bp, res, start, "footops.json + compose.yaml")
	return bp, nil
}

// serverPaths are what rebuilding the server takes, below the store's root: host path
// → name under /footops/_server. Missing ones are left out.
var serverPaths = []struct{ host, name string }{
	{"/etc/footops", "etc"},                // panel settings, install answers, secrets (MongoDB root, tunnel, this backup)
	{"/srv/core/caddy/etc", "caddy/etc"},   // Caddyfile, site blocks, Origin certificates
	{"/srv/core/caddy/data", "caddy/data"}, // Caddy's own state
	{"/var/lib/footops/web", "web"},        // the panel's login and 2FA
	{"/var/lib/footops/docker", "docker"},  // registry logins
	{"/var/lib/footops/registries.json", "registries.json"},
	{"/var/lib/footops/sources", "sources"}, // servers sites are moved from (their keys)
	{"/var/lib/footops/backup/settings.json", "backup/settings.json"},
}

// backupServer backs up the server's own settings.
func (b *Backups) backupServer(ctx context.Context, t *Task, env *resticEnv, tags string) (BackupPart, error) {
	bp := BackupPart{Site: serverPart, Part: PartServer}
	start := b.now()
	path := partPath(serverPart, PartServer)
	var mounts []bindMount
	for _, p := range serverPaths {
		host := filepath.Join(b.Data.Store.Root, p.host)
		if p.host == "/var/lib/footops/backup/settings.json" {
			host = b.settingsPath()
		}
		if _, err := os.Lstat(host); err == nil {
			mounts = append(mounts, bindMount{host, path + "/" + p.name})
		}
	}
	if len(mounts) == 0 {
		return bp, b.failPart(t, &bp, errors.New("sunucu ayarları bulunamadı"))
	}
	// The panel's login belongs to the panel's user.
	res, err := b.restic(ctx, t, env, resticOpts{caps: []string{"DAC_READ_SEARCH"}, mounts: mounts},
		"backup", "--json", "--host", b.Server, "--tag", tags, path)
	if err = partialOK(&bp, res, err); err != nil {
		return bp, b.failPart(t, &bp, err)
	}
	b.finish(t, &bp, res, start, fmt.Sprintf("%d dosya", res.Summary.TotalFiles))
	return bp, nil
}

// ---- freezing ----

// freezeMax bounds how long a site stays paused while its directory is copied.
var freezeMax = 2 * time.Minute

// frozenCopy pauses a running site, copies its data directory into the job's staging
// directory (cp -a: owners, modes and times kept) and unpauses it at once; it returns
// the copy ("" when the site does not run: nothing to freeze). The paused container is
// written down before the pause and struck off only once it is surely running again, so
// a failed unpause is retried by the next run and an agent that stopped in between
// unpauses it when it starts (or stops).
func (b *Backups) frozenCopy(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, sf siteFiles) (string, error) {
	name := spec.Container()
	if !docker.ValidName(name) {
		return "", errors.New("geçersiz container adı")
	}
	in, err := b.Data.Docker.Inspect(ctx, name)
	if docker.IsNotFound(err) || (err == nil && !in.State.Running) {
		t.Step("ℹ " + name + " çalışmıyor; dondurmaya gerek yok")
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("sitenin durumu okunamadı: %w", err)
	}
	st, err := countFiles(sf.Dir)
	if err != nil {
		return "", fmt.Errorf("kalıcı klasör okunamadı: %w", err)
	}
	if err := b.Data.Staging.Allow(st.Bytes); err != nil {
		return "", err
	}
	dst := filepath.Join(filepath.Dir(env.tmpDir), "frozen-"+spec.Name)
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	if err := b.markPaused(name, true); err != nil {
		return "", err
	}
	paused := b.now()
	if !in.State.Paused {
		if _, err := b.Data.Run.Run(ctx, "docker", "pause", name); err != nil {
			// A cancel may have stopped the command after the daemon paused it.
			_ = b.unpause(name)
			return "", fmt.Errorf("site dondurulamadı: %w", err)
		}
	}
	copyCtx, cancel := context.WithTimeout(ctx, freezeMax)
	_, cerr := b.Data.Run.Run(copyCtx, "cp", "-a", "-T", "--", sf.Dir, dst)
	cancel()
	uerr := b.unpause(name)
	held := b.now().Sub(paused)
	if cerr != nil {
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("dondurulmuş kopya alınamadı (%s donuk kaldı): %w", took(held), cerr)
	}
	if uerr != nil {
		t.Step("✗ " + name + " çözülemedi: " + uerr.Error() + " — sonraki yedekte ve agent yeniden başlarken tekrar denenir")
	}
	t.Step(fmt.Sprintf("✓ %s %s donuk kaldı: dosyaları kopyalandı (%d dosya, %s)", name, took(held), st.Files, humanBytes(st.Bytes)))
	return dst, nil
}

// unpause unpauses a container a backup paused and strikes it off the record once it
// is surely not paused (unpaused now, or already, or gone).
func (b *Backups) unpause(name string) error {
	stop, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := b.Data.Run.Run(stop, "docker", "unpause", name)
	if err != nil && !strings.Contains(out, "is not paused") && !strings.Contains(out, "No such container") {
		return err
	}
	return b.markPaused(name, false)
}

func (b *Backups) markPaused(name string, on bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	var list []string
	if data, err := os.ReadFile(b.pausedPath()); err == nil {
		_ = json.Unmarshal(data, &list)
	}
	out := list[:0]
	for _, n := range list {
		if n != name {
			out = append(out, n)
		}
	}
	if on {
		out = append(out, name)
	}
	if len(out) == 0 {
		err := os.Remove(b.pausedPath())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	data, _ := json.Marshal(out)
	if err := os.MkdirAll(b.Dir, 0o700); err != nil {
		return err
	}
	return writeSynced(b.pausedPath(), data)
}

// Recover unpauses the containers a backup paused and could not unpause (the agent
// stopped in between, or the unpause failed). It runs when the agent starts and stops,
// and before every run.
func (b *Backups) Recover(ctx context.Context) {
	b.init()
	data, err := os.ReadFile(b.pausedPath())
	if err != nil {
		return
	}
	var list []string
	_ = json.Unmarshal(data, &list)
	for _, name := range list {
		if !docker.ValidName(name) {
			continue
		}
		if err := b.unpause(name); err != nil {
			b.logf("yedek için dondurulan %s çözülemedi: %v", name, err)
			continue
		}
		b.logf("yedek için dondurulan site çözüldü: %s", name)
	}
}

// ---- retention and maintenance ----

// errFuture holds retention back: --keep-within counts from each group's newest
// snapshot, so one taken while the clock ran ahead would make forget drop real ones.
var errFuture = errors.New("saklama kuralı atlandı: depoda ileri tarihli bir anlık görüntü var (sunucunun saati ileri gitmiş olabilir); saat düzelince elle bakılmalı")

// forget applies retention: one restic forget per distinct policy, each over the sites
// that share it (tags OR). Sites that are gone keep the defaults; a group is the host
// and path, so every part of every site ages on its own, counted from its newest.
func (b *Backups) forget(ctx context.Context, t *Task, env *resticEnv, s BackupSettings) (int, error) {
	current, err := b.Data.Store.List()
	if err != nil {
		return 0, err
	}
	future, err := b.futureSnapshot(ctx, t, env)
	if err != nil {
		return 0, err
	}
	if future {
		return 0, errFuture
	}
	names := map[string]bool{serverPart: true}
	for _, sp := range current {
		names[sp.Name] = true
	}
	for _, site := range sortedSites(b.state().Last) {
		names[site] = true
	}
	groups := map[[3]int][]string{}
	for name := range names {
		pol := s.For(name)
		if name == serverPart {
			pol = s.Defaults
		}
		k := [3]int{pol.KeepHours, pol.KeepDays, pol.KeepMonths}
		groups[k] = append(groups[k], name)
	}
	keys := make([][3]int, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	removed := 0
	for _, k := range keys {
		list := groups[k]
		sort.Strings(list)
		args := []string{"forget", "--json", "--host", b.Server}
		for _, n := range list {
			args = append(args, "--tag", "site="+n)
		}
		args = append(args, "--keep-within-hourly", strconv.Itoa(k[0])+"h", "--keep-within-daily", strconv.Itoa(k[1])+"d",
			"--keep-within-monthly", strconv.Itoa(k[2])+"m")
		n := 0
		arr := newJSONArray(func(raw json.RawMessage) error {
			var g struct {
				Remove []json.RawMessage `json:"remove"`
			}
			if err := json.Unmarshal(raw, &g); err != nil {
				return err
			}
			n += len(g.Remove)
			return nil
		})
		_, err := b.restic(ctx, t, env, resticOpts{out: arr}, args...)
		derr := arr.result()
		if err != nil {
			return removed, err
		}
		if derr != nil && !errors.Is(derr, io.EOF) {
			t.Step("ℹ saklama raporu okunamadı: " + derr.Error())
		}
		removed += n
	}
	t.Step(fmt.Sprintf("✓ Saklama kuralı uygulandı: %d eski anlık görüntü silindi", removed))
	return removed, nil
}

// futureSnapshot reports whether any group's newest snapshot is more than an hour
// ahead of now (restic snapshots --latest 1: one per host and path).
func (b *Backups) futureSnapshot(ctx context.Context, t *Task, env *resticEnv) (bool, error) {
	limit := b.now().Add(time.Hour)
	future := false
	arr := newJSONArray(func(raw json.RawMessage) error {
		var sn struct {
			Time time.Time `json:"time"`
		}
		if json.Unmarshal(raw, &sn) == nil && sn.Time.After(limit) {
			future = true
		}
		return nil
	})
	_, err := b.restic(ctx, t, env, resticOpts{out: arr}, "snapshots", "--json", "--host", b.Server, "--latest", "1")
	derr := arr.result()
	if err != nil {
		return false, err
	}
	if derr != nil && !errors.Is(derr, io.EOF) {
		return false, fmt.Errorf("anlık görüntü listesi okunamadı: %w", derr)
	}
	return future, nil
}

// maintain is the weekly work: prune (frees what forget left unreferenced, repacking
// only when over a tenth is unused) and a check that reads back a sample of the data.
func (b *Backups) maintain(ctx context.Context, t *Task, env *resticEnv, rec *BackupRun) error {
	start := b.now()
	if _, err := b.restic(ctx, t, env, resticOpts{out: io.Discard}, "prune", "--max-unused", "10%"); err != nil {
		if ctx.Err() == nil && !isFatal(err) {
			rec.PruneErr = clipMsg("depo temizliği (prune): "+err.Error(), 1000)
		}
		return fmt.Errorf("depo temizliği (prune): %w", err)
	}
	rec.Pruned = true
	t.Step("✓ Depo temizlendi (prune, " + took(b.now().Sub(start)) + ")")
	start = b.now()
	// Only a check that ran to its end counts, clean or not: one cut short (cancel, the
	// job's deadline, the repository gone) must not clear an earlier finding.
	_, err := b.restic(ctx, t, env, resticOpts{out: io.Discard}, "check", "--read-data-subset", "5%")
	if err != nil {
		if ctx.Err() != nil || isFatal(err) {
			return err
		}
		rec.Checked, rec.CheckErr = true, clipMsg(err.Error(), 1000)
		return fmt.Errorf("depo denetimi (check) sorun buldu: %w", err)
	}
	rec.Checked = true
	t.Step("✓ Depo denetlendi (check, verinin %5'i okundu, " + took(b.now().Sub(start)) + ")")
	return nil
}

// repoSize is what the snapshots take in the repository, packed (restic stats
// raw-data; the index is in the local cache).
func (b *Backups) repoSize(ctx context.Context, t *Task, env *resticEnv) (int64, error) {
	var size int64
	lines := &jsonLines{each: func(line []byte) {
		var m struct {
			Total int64 `json:"total_size"`
		}
		if json.Unmarshal(line, &m) == nil && m.Total > 0 {
			size = m.Total
		}
	}}
	_, err := b.restic(ctx, t, env, resticOpts{out: lines}, "stats", "--json", "--mode", "raw-data")
	lines.flush()
	if err != nil {
		return 0, err
	}
	return size, nil
}

// ---- run records ----

// Run records are runs/<start, UTC>-<job>.json: listing the directory orders them, so the
// newest are read without opening the rest (A gathers ~1.500 in two months).
var reRunFile = regexp.MustCompile(`^([0-9]{8}-[0-9]{6})-([A-Za-z0-9]{1,64})\.json$`)

func runFile(rec BackupRun) string {
	return rec.Started.UTC().Format("20060102-150405") + "-" + rec.ID + ".json"
}

func (b *Backups) saveRun(rec BackupRun) {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		if err = os.MkdirAll(b.runsDir(), 0o700); err == nil {
			err = writeSynced(filepath.Join(b.runsDir(), runFile(rec)), append(data, '\n'))
		}
	}
	if err != nil {
		b.logf("yedek kaydı yazılamadı: %v", err)
	}
}

// runFiles lists the record files, newest first.
func (b *Backups) runFiles() ([]string, error) {
	ents, err := os.ReadDir(b.runsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && reRunFile.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// runsScan bounds how many records a search for one site's runs opens.
const runsScan = 400

// Runs returns up to limit run records, newest first; site narrows them to runs that
// touched it (looking through the newest runsScan records at most).
func (b *Backups) Runs(site string, limit int) ([]BackupRun, error) {
	names, err := b.runFiles()
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > runsMax {
		limit = runsMax
	}
	out := []BackupRun{}
	for i, name := range names {
		if len(out) >= limit || (site != "" && i >= max(runsScan, limit)) {
			break
		}
		data, err := os.ReadFile(filepath.Join(b.runsDir(), name))
		if err != nil {
			continue
		}
		var r BackupRun
		if json.Unmarshal(data, &r) != nil || runFile(r) != name {
			continue
		}
		if site != "" {
			keep := r.Site == site // its own run, even one that never reached a part
			for _, p := range r.Parts {
				keep = keep || p.Site == site
			}
			if !keep {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// SweepRuns removes records past RunsKeep, and the oldest beyond runsMax, by their
// names alone.
func (b *Backups) SweepRuns() []string {
	b.init()
	names, err := b.runFiles()
	if err != nil {
		return nil
	}
	var removed []string
	cutoff := b.now().UTC().Add(-RunsKeep).Format("20060102-150405")
	for i, name := range names {
		m := reRunFile.FindStringSubmatch(name)
		if i >= runsMax || m[1] < cutoff {
			if os.Remove(filepath.Join(b.runsDir(), name)) == nil {
				removed = append(removed, m[2])
			}
		}
	}
	return removed
}
