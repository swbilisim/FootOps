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
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/sites"
)

// The monthly trial restore (§27.7). The weekly check reads the repository's data; this
// shows a backup still comes back as what it was. Once every trialEvery, right after a
// clean night run, one site's newest backup — the sites take turns — is restored where
// it harms nothing: its database into a temporary database, counted against what
// mongodump counted when it was taken, and dropped; a sample of its files fetched from
// the repository and compared, byte for byte, with the live ones that have not changed
// since. A failure is an alert (and so a Telegram message).
const (
	trialEvery = 28 * 24 * time.Hour
	// trialSample files are compared; files over trialFileMax are left out of the sample
	// while there are smaller ones (each is fetched from the bucket).
	trialSample  = 20
	trialFileMax = 64 << 20
	// trialListMax bounds the files read from a snapshot's listing.
	trialListMax = 200_000
)

// BackupTrial is the last trial restore.
type BackupTrial struct {
	Time   time.Time `json:"time"`
	Site   string    `json:"site"`
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
	Err    string    `json:"error,omitempty"`
	Job    string    `json:"job,omitempty"`
}

// trialDue reports whether the trial is due at now: backups run, at least one part is
// backed up, and the last trial is trialEvery old (or there was none).
func (b *Backups) trialDue(now time.Time) bool {
	every := b.trialEvery
	if every == 0 {
		every = trialEvery
	}
	if _, ok := b.active(); !ok || every < 0 {
		return false
	}
	st := b.state()
	return len(st.Last) > 0 && (st.Trial == nil || now.Sub(st.Trial.Time) >= every)
}

// trialable reports whether site can be tried: it is backed up (not turned off) and has
// a backup of its database or of its files.
func (b *Backups) trialable(s sites.Spec, st backupState, set BackupSettings) bool {
	if set.For(s.Name).Off {
		return false // its last backups are months old by now
	}
	_, db := st.Last[s.Name+"/"+PartDB]
	_, files := st.Last[s.Name+"/"+PartFiles]
	return (db && s.Database) || (files && s.DataMount != "")
}

// trialSite is whose turn it is: the next site, by name, after the last one tried that
// can be tried.
func (b *Backups) trialSite(st backupState) string {
	list, err := b.Data.Store.List()
	if err != nil {
		return ""
	}
	set, err := b.Settings()
	if err != nil {
		return ""
	}
	var names []string
	for _, s := range list {
		if b.trialable(s, st, set) {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	last := ""
	if st.Trial != nil {
		last = st.Trial.Site
	}
	for _, n := range names {
		if n > last {
			return n
		}
	}
	return names[0]
}

// StartTrial starts a trial restore of site (empty: whose turn it is) as a job.
func (b *Backups) StartTrial(site string, actor Actor) (string, error) {
	b.init()
	if b.Rehearsal.Active() {
		return "", errRehearsal
	}
	if _, err := b.secret(); err != nil {
		return "", err
	}
	if site == "" {
		if site = b.trialSite(b.state()); site == "" {
			return "", errors.New("denenecek bir yedek yok: önce bir yedek alınmalı")
		}
	} else {
		// Asked for by name: one that cannot be tried is said so, not recorded as a
		// failed trial (that would raise an alert and hide the last real result).
		spec, err := b.Data.Store.Get(site)
		if err != nil {
			return "", err
		}
		set, err := b.Settings()
		if err != nil {
			return "", err
		}
		if !b.trialable(spec, b.state(), set) {
			return "", errors.New("bu sitenin denenecek bir yedeği yok (yedeklemesi kapalı ya da henüz yedek alınmadı)")
		}
	}
	return b.Jobs.Start(JobSpec{Kind: "backup.trial", Target: site, Actor: actor, Cancellable: true},
		func(ctx context.Context, t *Task) error {
			_, err := b.Trial(ctx, t, site)
			return err
		}, func(j Job) {
			detail := j.Err
			if detail == "" {
				detail = j.Result
			}
			if b.Audit != nil {
				if err := b.Audit.Record(AuditEntry{Actor: j.Actor, Action: j.Kind, Target: j.Target, OK: j.Err == "", Detail: detail}); err != nil {
					b.logf("audit: %v", err)
				}
			}
			if j.Err != "" {
				b.logf("deneme geri yüklemesi başarısız (%s): %s", j.Target, j.Err)
			}
		})
}

// afterNight starts the trial when a clean night run ends and it is due.
func (b *Backups) afterNight(j Job) {
	if j.Err != "" || !b.trialDue(b.now()) {
		return
	}
	if _, err := b.StartTrial("", schedActor); err != nil && !errors.Is(err, ErrBusy) {
		b.logf("deneme geri yüklemesi başlatılamadı: %v", err)
	}
}

// Trial restores site's newest backup where it harms nothing and checks it.
func (b *Backups) Trial(ctx context.Context, t *Task, site string) (res BackupTrial, err error) {
	b.init()
	defer b.Data.dropStaging(t)
	res = BackupTrial{Time: b.now().UTC(), Site: site, Job: t.ID()}
	defer func() {
		if errors.Is(err, context.Canceled) {
			return // stopped: not a finding either way
		}
		if err != nil {
			res.OK, res.Err = false, clipMsg(err.Error(), 500)
		}
		r := res
		b.updateState(func(st *backupState) { st.Trial = &r })
	}()
	spec, err := b.Data.Store.Get(site)
	if err != nil {
		return res, err
	}
	sec, err := b.secret()
	if err != nil {
		return res, err
	}
	env, done, err := b.prepareIn(ctx, t, sec, resticPrefix+t.ID())
	if err != nil {
		return res, err
	}
	defer done()
	env.readOnly = true // a trial only reads
	st := b.state()
	t.Step("ℹ Deneme geri yüklemesi: " + site + " (sitenin kendisine dokunulmaz)")
	var details []string
	if l, ok := st.Last[site+"/"+PartDB]; ok && spec.Database {
		d, err := b.trialDB(ctx, t, env, site, l)
		if err != nil {
			return res, fmt.Errorf("veritabanı: %w", err)
		}
		details = append(details, d)
	}
	if l, ok := st.Last[site+"/"+PartFiles]; ok && spec.DataMount != "" {
		d, err := b.trialFiles(ctx, t, env, spec, l)
		if err != nil {
			return res, fmt.Errorf("dosyalar: %w", err)
		}
		details = append(details, d)
	}
	if len(details) == 0 {
		return res, errors.New("bu sitenin denenecek bir yedeği yok")
	}
	res.OK, res.Detail = true, strings.Join(details, " · ")
	t.SetResult(res.Detail)
	t.Step("✓ Deneme geri yüklemesi temiz: " + res.Detail)
	return res, nil
}

// trialDB restores the database's newest backup into a temporary database (removed
// again) and compares it with mongodump's own counts.
func (b *Backups) trialDB(ctx context.Context, t *Task, env *resticEnv, site string, l BackupLast) (string, error) {
	var part BackupPart
	found := false
	if rec, ok := b.runRecord(l.Run); ok {
		for _, p := range rec.Parts {
			if p.Site == site && p.Part == PartDB && p.Snapshot == l.Snapshot {
				part, found = p, true
			}
		}
	}
	snap := BackupSnapshot{ID: l.Snapshot, Time: l.Time, Site: site, Part: PartDB, Bytes: part.Bytes}
	if err := b.Data.Staging.Allow(snap.Bytes); err != nil {
		return "", err
	}
	if err := b.Data.checkFree(b.Data.mongoData(), 2*snap.Bytes); err != nil {
		return "", fmt.Errorf("geçici veritabanı için diskte yer yok: %w", err)
	}
	dir, err := b.Data.Staging.Dir(t.ID())
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "trial.db.archive")
	defer os.Remove(path)
	t.Step(fmt.Sprintf("… Veritabanının son yedeği indiriliyor (%s)", localTime(l.Time)))
	size, digest, err := b.dumpTo(ctx, t, env, snap, partPath(site, PartDB), path)
	if err != nil {
		return "", fmt.Errorf("yedek indirilemedi: %w", goneOr(err))
	}
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
		return "veritabanı boş (yedekte de boştu)", nil
	case len(dbs) > 1:
		return "", fmt.Errorf("yedekte tek bir veritabanı bekleniyordu, %d var", len(dbs))
	}
	counts, views, err := b.Data.trialRestore(ctx, t, path, dbSection{Length: size, SHA256: digest}, dbs[0], size)
	if err != nil {
		return "", err
	}
	if !found {
		return fmt.Sprintf("veritabanı açıldı: %d koleksiyon / %d belge (yedek alınırkenki kayıt yok, karşılaştırılamadı)", len(counts), sum(counts)), nil
	}
	if diff := countsDiffer(part.Collections, counts, views); diff != "" {
		return "", fmt.Errorf("sayımlar yedek alınırken sayılanla tutmuyor: %s", clipMsg(diff, 400))
	}
	t.Step("✓ Sayımlar yedek alınırken sayılanla aynı")
	return fmt.Sprintf("veritabanı %d koleksiyon / %d belge aynı", len(counts), sum(counts)), nil
}

// trialNode is a file of a files snapshot.
type trialNode struct {
	Path  string
	Size  int64
	Mtime time.Time
}

// trialFiles fetches a sample of the files' newest backup and compares each with what
// it should be: its size in the backup, and the live file's bytes when that has not
// changed since (same size and modification time).
func (b *Backups) trialFiles(ctx context.Context, t *Task, env *resticEnv, spec sites.Spec, l BackupLast) (string, error) {
	root := partPath(spec.Name, PartFiles)
	var nodes []trialNode
	lines := &jsonLines{each: func(line []byte) {
		var n struct {
			StructType string    `json:"struct_type"`
			Type       string    `json:"type"`
			Path       string    `json:"path"`
			Size       int64     `json:"size"`
			Mtime      time.Time `json:"mtime"`
		}
		if json.Unmarshal(line, &n) != nil || n.StructType != "node" || n.Type != "file" || len(nodes) >= trialListMax {
			return
		}
		nodes = append(nodes, trialNode{Path: n.Path, Size: n.Size, Mtime: n.Mtime})
	}}
	_, err := b.restic(ctx, t, env, resticOpts{out: lines}, "ls", "--json", "--no-lock", "--recursive", l.Snapshot, root)
	lines.flush()
	if err != nil {
		return "", fmt.Errorf("yedekteki dosyalar listelenemedi: %w", goneOr(err))
	}
	if len(nodes) == 0 {
		return "dosyalar: yedekteki klasör boş", nil
	}
	sf, err := b.Data.siteFiles(spec.Name)
	if err != nil {
		return "", err
	}
	// The live files are read inside the site's folder: no link in it leads out.
	live, err := os.OpenRoot(sf.Dir)
	if err != nil {
		return "", err
	}
	defer live.Close()
	sample := b.trialSample(nodes)
	t.Step(fmt.Sprintf("… %d dosyadan %d tanesi yedekten açılıp karşılaştırılıyor", len(nodes), len(sample)))
	same := 0
	for _, n := range sample {
		rel := strings.TrimPrefix(n.Path, root+"/")
		if rel == n.Path || !filepath.IsLocal(rel) {
			return "", fmt.Errorf("yedekte beklenmeyen yol: %q", n.Path)
		}
		h := sha256.New()
		cw := &countingWriter{w: h}
		if _, err := b.restic(ctx, t, env, resticOpts{out: cw}, "dump", "--no-lock", l.Snapshot, n.Path); err != nil {
			return "", fmt.Errorf("%s yedekten açılamadı: %w", rel, goneOr(err))
		}
		if cw.n != n.Size {
			return "", fmt.Errorf("%s: yedekte %s, açılan %s", rel, humanBytes(n.Size), humanBytes(cw.n))
		}
		sum, unchanged := liveSHA256(live, rel, n)
		switch {
		case !unchanged:
		case sum == hex.EncodeToString(h.Sum(nil)):
			same++
		default:
			// Changed while it was read? Then it is a changed file, not a bad backup.
			if _, still := liveSHA256(live, rel, n); still {
				return "", fmt.Errorf("%s yedekte canlıdakinden farklı (canlı dosya yedekten beri değişmemiş görünüyor)", rel)
			}
		}
	}
	changed := len(sample) - same
	note := ""
	if changed > 0 {
		note = fmt.Sprintf(" (%d tanesi yedekten beri değiştiği için yalnız boyutu)", changed)
	}
	t.Step(fmt.Sprintf("✓ %d dosyanın %d tanesi canlıdakiyle bayt bayt aynı%s", len(sample), same, note))
	return fmt.Sprintf("%d dosyanın %d tanesi aynı%s", len(sample), same, note), nil
}

// trialSample picks the files to compare: at random, those up to trialFileMax first.
func (b *Backups) trialSample(nodes []trialNode) []trialNode {
	var small, large []trialNode
	for _, n := range nodes {
		if n.Size <= trialFileMax {
			small = append(small, n)
		} else {
			large = append(large, n)
		}
	}
	pick := b.trialPick
	if pick == nil {
		pick = func(n int) []int { return rand.Perm(n) }
	}
	var out []trialNode
	for _, list := range [][]trialNode{small, large} {
		for _, i := range pick(len(list)) {
			if len(out) == trialSample {
				return out
			}
			out = append(out, list[i])
		}
		if len(out) > 0 {
			break // a large file only when there is no small one
		}
	}
	return out
}

// liveSHA256 hashes the live file rel (inside the site's folder) if it still is what
// the backup took (a regular file of the same size and modification time); unchanged
// is false otherwise. No link is followed out of the folder, none at its last name.
func liveSHA256(live *os.Root, rel string, n trialNode) (sum string, unchanged bool) {
	f, err := live.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() != n.Size || !st.ModTime().Equal(n.Mtime) {
		return "", false
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, n.Size+1)); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}
