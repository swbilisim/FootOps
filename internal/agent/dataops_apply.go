package agent

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Applying data (rule K3): the site stops, a copy of what will change is kept (the
// database's safety copy, the previous data directory), the change is made and checked,
// the site starts and must answer. Any failure puts the copies back. A recovery record
// on disk lets the next agent start finish the job if this process dies half-way.

// recovery records a restore in progress.
type recovery struct {
	Job      string         `json:"job"`
	Kind     string         `json:"kind"`
	Site     string         `json:"site"`
	DB       bool           `json:"db"`                 // the database is being replaced
	Snapshot string         `json:"snapshot,omitempty"` // its safety copy
	Files    *filesRecovery `json:"files,omitempty"`    // the data directory is being replaced
	Running  bool           `json:"running"`            // the site ran before
	Phase    string         `json:"phase"`
	Actor    Actor          `json:"actor"`
	Started  time.Time      `json:"started"`
}

// filesRecovery says where the previous data directory goes.
type filesRecovery struct {
	Old   string `json:"old"`   // data.old-<time>
	Moved bool   `json:"moved"` // the swap has started
	// Back is where the new data directory goes when undone; empty: data.failed-<time>
	// (removed after a day). Restoring a backup sends it back to its own name.
	Back string `json:"back,omitempty"`
}

const (
	phasePrepare   = "prepare"      // stopping the site, taking copies, unpacking: nothing changed yet
	phaseRestoring = "restoring"    // the copies are complete; the data is being replaced
	phaseVerified  = "verified"     // the new data is complete and counted
	phaseRollback  = "rolling-back" // the copies are being put back
)

func (d *DataOps) recoveryPath(job string) string { return filepath.Join(d.RecoveryDir, job+".json") }

func (d *DataOps) saveRecovery(r recovery) error {
	if err := os.MkdirAll(d.RecoveryDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writeSynced(d.recoveryPath(r.Job), b)
}

func (d *DataOps) removeRecovery(job string) { _ = os.Remove(d.recoveryPath(job)) }

// Recoveries lists restores an agent stop cut short.
func (d *DataOps) Recoveries() []recovery {
	ents, err := os.ReadDir(d.RecoveryDir)
	if err != nil {
		return nil
	}
	var out []recovery
	for _, e := range ents {
		if !reJobFile.MatchString(e.Name()) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.RecoveryDir, e.Name()))
		var r recovery
		if err != nil || json.Unmarshal(b, &r) != nil || r.Job+".json" != e.Name() {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started.Before(out[b].Started) })
	return out
}

// snapshotSource describes one of the site's copies as a restore source.
func (d *DataOps) snapshotSource(snap Snapshot) (restoreSource, error) {
	_, path, err := d.SnapshotFile(snap.Site, snap.ID)
	if err != nil {
		return restoreSource{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return restoreSource{}, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(bufio.NewReader(f))
	if err != nil {
		return restoreSource{}, fmt.Errorf("kopya okunamadı: %w", err)
	}
	p, err := archive.ReadMongoPrelude(zr)
	if err != nil {
		return restoreSource{}, fmt.Errorf("kopya okunamadı: %w", err)
	}
	src := restoreSource{Label: "kopya " + snap.ID, Path: path, DB: snap.DB, Expect: map[string]int64{}, Size: snap.DataSize,
		Sec: dbSection{Offset: 0, Length: snap.Bytes, Gzip: true, SHA256: snap.SHA256}}
	views := map[string]bool{}
	for _, n := range p.Collections(snap.DB) {
		src.Keep = append(src.Keep, n.Collection)
		views[n.Collection] = n.Type == "view"
	}
	for coll, n := range snap.Collections {
		// System collections (system.js, …) are never restored (rule K4), so not counted.
		if !views[coll] && !strings.HasPrefix(coll, "system.") {
			src.Expect[coll] = n
		}
	}
	return src, nil
}

// restorePlan is what one apply changes: the database, the data directory, or both.
type restorePlan struct {
	DB        *restoreSource
	DBMode    string
	Files     *filesSource
	FilesMode string // ModeReplace, or ModeAdd: the archive's files over the current ones
	Reason    string // why the safety copy is taken
}

// Applied is what a successful apply leaves for undoing it.
type Applied struct {
	Snapshot string // the database's safety copy
	FilesOld string // the previous data directory (data.old-<id>)
}

// result is the job result: the undo targets, space-separated ("files-<id>" for files).
func (a Applied) result() string {
	var parts []string
	if a.Snapshot != "" {
		parts = append(parts, a.Snapshot)
	}
	if a.FilesOld != "" {
		parts = append(parts, "files-"+a.FilesOld)
	}
	return strings.Join(parts, " ")
}

func modeText(m string) string {
	return map[string]string{ModeReplace: "tamamen değiştir", ModeAdd: "yalnız olmayanları ekle"}[m]
}

func filesModeText(m string) string {
	return map[string]string{ModeReplace: "tamamen değiştir", ModeAdd: "ekle ve üzerine yaz"}[m]
}

func (d *DataOps) apply(ctx context.Context, t *Task, site string, plan restorePlan, actor Actor) (Applied, error) {
	d.init()
	var (
		res   Applied
		creds dbCreds
		sf    siteFiles
		err   error
	)
	if plan.DB == nil && plan.Files == nil {
		return res, errors.New("aktarılacak bir şey seçilmedi")
	}
	if plan.DB != nil {
		if plan.DBMode != ModeReplace && plan.DBMode != ModeAdd {
			return res, fmt.Errorf("bilinmeyen mod %q", plan.DBMode)
		}
		if _, creds, err = d.siteDB(site); err != nil {
			return res, err
		}
		if err := d.checkFree(d.mongoData(), 2*plan.DB.Size); err != nil {
			return res, fmt.Errorf("veritabanı için diskte yer yok, hiçbir şey değiştirilmedi: %w", err)
		}
	}
	if plan.Files != nil {
		if plan.FilesMode != ModeReplace && plan.FilesMode != ModeAdd {
			return res, fmt.Errorf("bilinmeyen mod %q", plan.FilesMode)
		}
		if sf, err = d.siteFiles(site); err != nil {
			return res, err
		}
		if err := d.checkFree(sf.Dir, plan.Files.Expect.Bytes); err != nil {
			return res, fmt.Errorf("dosyalar için diskte yer yok, hiçbir şey değiştirilmedi: %w", err)
		}
	}
	running, err := d.Sites.Running(ctx, site)
	if err != nil {
		return res, err
	}
	healthy := false
	if running {
		healthy = d.Sites.Health(ctx, site, 1) == nil
		if !healthy {
			t.Step("ℹ Site şu an da sağlıklı cevap vermiyor; sonradan sağlık kontrolü geri alma sebebi sayılmayacak")
		}
	}
	rec := recovery{Job: t.ID(), Kind: "restore", Site: site, DB: plan.DB != nil, Running: running, Phase: phasePrepare,
		Actor: actor, Started: d.now().UTC()}
	if plan.Files != nil {
		rec.Files = &filesRecovery{Old: d.freshOld(sf)}
		defer func() { _ = os.RemoveAll(sf.path(dataNew)) }() // whatever happens, no half-built directory stays
	}
	if err := d.saveRecovery(rec); err != nil {
		return res, err
	}
	defer d.removeRecovery(t.ID())
	// Before anything is changed, a failure only has to bring the site back.
	untouched := func(err error) (Applied, error) {
		if running {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if serr := d.Sites.Start(bg, site); serr != nil {
				return Applied{}, fmt.Errorf("%v; site yeniden başlatılamadı: %v", err, serr)
			}
		}
		return Applied{}, err
	}

	// Replacing files: unpack while the site still runs (data/ is not touched).
	var filesWant datafs.Stats
	if plan.Files != nil && plan.FilesMode == ModeReplace {
		if filesWant, err = d.extract(ctx, t, sf, *plan.Files, false); err != nil {
			return Applied{}, err
		}
	}
	if running {
		if err := d.Sites.Stop(ctx, site); err != nil {
			return untouched(err)
		}
		t.Step("✓ Site durduruldu (bu sırada ziyaretçiler hata sayfası görür)")
	} else {
		t.Step("ℹ Site zaten çalışmıyordu; işlemden sonra da başlatılmayacak")
	}
	var back restoreSource
	if plan.DB != nil {
		snap, err := d.Snapshot(ctx, t, site, plan.Reason, actor)
		if err != nil {
			return untouched(fmt.Errorf("güvenlik kopyası alınamadı, hiçbir şey değiştirilmedi: %w", err))
		}
		if back, err = d.snapshotSource(snap); err != nil {
			return untouched(fmt.Errorf("güvenlik kopyası okunamadı, hiçbir şey değiştirilmedi: %w", err))
		}
		rec.Snapshot, res.Snapshot = snap.ID, snap.ID
	}
	// Adding files: the current ones are linked in, so the site must not write meanwhile.
	if plan.Files != nil && plan.FilesMode == ModeAdd {
		if filesWant, err = d.extract(ctx, t, sf, *plan.Files, true); err != nil {
			return untouched(err)
		}
	}
	rec.Phase = phaseRestoring
	if err := d.saveRecovery(rec); err != nil {
		return untouched(err)
	}

	if plan.DB != nil {
		t.Step(fmt.Sprintf("… Veritabanı: %s yükleniyor (%s)", plan.DB.Label, modeText(plan.DBMode)))
		_, _, err = d.load(ctx, t, creds, *plan.DB, plan.DBMode)
	}
	if err == nil && plan.Files != nil {
		if err = d.swap(sf, rec.Files.Old, &rec); err == nil {
			res.FilesOld = strings.TrimPrefix(rec.Files.Old, dataOldPrefix)
			err = d.checkFiles(sf, filesWant)
			if err == nil {
				t.Step(fmt.Sprintf("✓ Kalıcı klasör değiştirildi (%s); önceki hali %s, %d gün saklanır",
					filesModeText(plan.FilesMode), rec.Files.Old, int(FilesKeep.Hours()/24)))
			}
		}
	}
	if err == nil {
		rec.Phase = phaseVerified
		err = d.saveRecovery(rec)
	}
	if err == nil && running {
		if err = d.Sites.Start(ctx, site); err == nil {
			t.Step("✓ Site başlatıldı")
			if healthy {
				if err = d.Sites.Health(ctx, site, healthTries); err == nil {
					t.Step("✓ Site sağlıklı cevap veriyor")
				}
			}
		}
	}
	if err == nil {
		t.SetResult(res.result())
		return res, nil
	}

	// Roll back with a fresh context: the job's own may be what ran out. The record says
	// so first: data being rolled back is never "verified" for a recovery.
	t.Step("✗ " + err.Error())
	t.Step("… Geri alınıyor")
	rec.Phase = phaseRollback
	if serr := d.saveRecovery(rec); serr != nil {
		t.Step("⚠ Kurtarma kaydı güncellenemedi: " + serr.Error())
	}
	bg, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if running {
		_ = d.Sites.Stop(bg, site)
	}
	var failed []string
	if plan.DB != nil {
		if rerr := d.rollback(bg, t, creds, back); rerr != nil {
			failed = append(failed, fmt.Sprintf("veritabanı: %v (Veritabanı kopyaları → %s → Geri yükle)", rerr, back.Label))
		} else {
			t.Step("✓ Veritabanı eski haline döndü")
		}
	}
	if plan.Files != nil && rec.Files.Moved {
		if rerr := d.unswap(sf, rec.Files.Old, ""); rerr != nil {
			failed = append(failed, fmt.Sprintf("dosyalar: %v (önceki klasör %s)", rerr, rec.Files.Old))
		} else {
			t.Step("✓ Kalıcı klasör eski haline döndü")
		}
	}
	if len(failed) > 0 {
		return Applied{}, fmt.Errorf("%v — GERİ ALMA DA BAŞARISIZ, site durduruldu: %s", err, strings.Join(failed, "; "))
	}
	if running {
		if serr := d.Sites.Start(bg, site); serr != nil {
			return Applied{}, fmt.Errorf("%v — veri eski haline döndü ama site başlatılamadı: %v", err, serr)
		}
	}
	t.Step("✓ Geri alındı: veri işlemden önceki halinde" + map[bool]string{true: ", site çalışıyor", false: ""}[running])
	return Applied{}, fmt.Errorf("%v (geri alındı, veri değişmedi)", err)
}

// checkFiles counts the new data directory: it must hold exactly what was put into it
// (the archive's files, plus the kept ones when adding).
func (d *DataOps) checkFiles(sf siteFiles, want datafs.Stats) error {
	st, err := countFiles(sf.Dir)
	if err != nil {
		return fmt.Errorf("yeni klasör sayılamadı: %w", err)
	}
	if st.Files != want.Files || st.Bytes != want.Bytes {
		return fmt.Errorf("dosya sayımı tutmuyor: klasörde %d dosya / %s, beklenen %d / %s",
			st.Files, humanBytes(st.Bytes), want.Files, humanBytes(want.Bytes))
	}
	return nil
}

// rollback restores the safety copy. A tool container left by the failed attempt is
// removed first (it may still be writing, and it holds the job's container name); a
// brief MongoDB outage gets one more try.
func (d *DataOps) rollback(ctx context.Context, t *Task, creds dbCreds, back restoreSource) error {
	var err error
	for try := 0; try < 2; try++ {
		if try > 0 {
			t.Step("… Geri alma tekrar deneniyor: " + err.Error())
		}
		_, _ = d.Run.Run(ctx, "docker", "rm", "--force", toolPrefix+t.ID())
		if werr := d.WaitMongo(ctx); werr != nil {
			err = werr
			continue
		}
		if _, _, err = d.load(ctx, t, creds, back, ModeReplace); err == nil {
			return nil
		}
	}
	return err
}

// ApplyOptions choose what of a checked upload is applied, and how.
type ApplyOptions struct {
	DB        bool   `json:"db"`
	DBMode    string `json:"db_mode"`
	Files     bool   `json:"files"`
	FilesMode string `json:"files_mode"`
	Strip     bool   `json:"strip"` // remove the archive's single top-level folder
}

// ApplyImport applies a checked upload to the site.
func (d *DataOps) ApplyImport(ctx context.Context, t *Task, site, uploadID string, opt ApplyOptions, actor Actor) (Applied, error) {
	d.init()
	defer d.dropStaging(t)
	ic, err := d.LoadCheck(site, uploadID)
	if err != nil {
		return Applied{}, err
	}
	u, path, err := d.uploadFile(site, uploadID)
	if err != nil {
		return Applied{}, err
	}
	if u.Size != ic.Size {
		return Applied{}, errors.New("yüklenen dosya kontrolden sonra değişmiş")
	}
	plan := restorePlan{Reason: "before-import", DBMode: opt.DBMode, FilesMode: opt.FilesMode}
	if opt.DB {
		if !ic.HasDB || ic.SourceDB == "" || ic.Archive == nil {
			return Applied{}, errors.New("veritabanı kısmı kontrol edilmedi (veritabanı seçilmedi ya da arşivde yok)")
		}
		plan.DB = &restoreSource{Label: "yüklenen dosya " + ic.File, Path: path, Sec: ic.Section, DB: ic.SourceDB,
			Keep: append(sortedKeys(ic.Archive), ic.Views...), Expect: ic.Archive, Size: ic.DataSize}
	}
	if opt.Files {
		if ic.Files == nil {
			return Applied{}, errors.New("arşivde içe aktarılacak dosya yok")
		}
		fs := filesSource{Label: ic.File, Path: path, Sec: ic.Files.Section, Format: ic.Files.Format,
			Expect: ic.Files.Expect()}
		if opt.Strip {
			fs.Strip = ic.Files.Archive.Prefix
		}
		plan.Files = &fs
	}
	res, err := d.apply(ctx, t, site, plan, actor)
	if err == nil {
		if rerr := d.Staging.Remove(uploadPrefix + uploadID); rerr == nil {
			t.Step("✓ Yüklenen dosya sunucudan silindi")
		}
		if res.Snapshot != "" {
			t.Step(fmt.Sprintf("ℹ Veritabanını geri almak için: site sayfası → Veritabanı kopyaları → %s → Geri yükle", res.Snapshot))
		}
		if res.FilesOld != "" {
			t.Step(fmt.Sprintf("ℹ Dosyaları geri almak için: site sayfası → Dosya kopyaları → %s → Geri yükle", res.FilesOld))
		}
	}
	return res, err
}

// RestoreSnapshot puts one of the site's database copies back.
func (d *DataOps) RestoreSnapshot(ctx context.Context, t *Task, site, id string, actor Actor) (Applied, error) {
	d.init()
	defer d.dropStaging(t)
	snap, _, err := d.SnapshotFile(site, id)
	if err != nil {
		return Applied{}, err
	}
	src, err := d.snapshotSource(snap)
	if err != nil {
		return Applied{}, err
	}
	t.Step(fmt.Sprintf("ℹ Geri yüklenecek kopya: %s (%d koleksiyon, %d belge)", snap.ID, len(snap.Collections), snap.Documents))
	res, err := d.apply(ctx, t, site, restorePlan{DB: &src, DBMode: ModeReplace, Reason: "before-restore"}, actor)
	if err == nil {
		t.Step(fmt.Sprintf("ℹ Bu işlemi geri almak için: Veritabanı kopyaları → %s → Geri yükle", res.Snapshot))
	}
	return res, err
}

// RestoreFiles puts a previous data directory back; the current one is kept in its
// place (so this can be undone too).
func (d *DataOps) RestoreFiles(ctx context.Context, t *Task, site, id string, actor Actor) (Applied, error) {
	d.init()
	if !reBackupID.MatchString(id) {
		return Applied{}, errors.New("geçersiz dosya kopyası")
	}
	sf, err := d.siteFiles(site)
	if err != nil {
		return Applied{}, err
	}
	chosen := sf.path(dataOldPrefix + id)
	if st, err := os.Lstat(chosen); err != nil || !st.IsDir() {
		return Applied{}, errors.New("dosya kopyası bulunamadı (7 günden eski olabilir)")
	}
	want, err := countFiles(chosen)
	if err != nil {
		return Applied{}, err
	}
	t.Step(fmt.Sprintf("ℹ Geri yüklenecek dosya kopyası: %s (%d dosya, %s)", id, want.Files, humanBytes(want.Bytes)))
	running, err := d.Sites.Running(ctx, site)
	if err != nil {
		return Applied{}, err
	}
	healthy := running && d.Sites.Health(ctx, site, 1) == nil
	rec := recovery{Job: t.ID(), Kind: "files", Site: site, Running: running, Phase: phasePrepare, Actor: actor,
		Started: d.now().UTC(), Files: &filesRecovery{Old: d.freshOld(sf), Back: dataOldPrefix + id}}
	// Checked before the site stops: never over an existing copy.
	if _, err := os.Lstat(sf.path(rec.Files.Old)); err == nil {
		return Applied{}, fmt.Errorf("%s zaten var; biraz sonra tekrar dene", rec.Files.Old)
	}
	if err := d.saveRecovery(rec); err != nil {
		return Applied{}, err
	}
	defer d.removeRecovery(t.ID())
	if running {
		if err := d.Sites.Stop(ctx, site); err != nil {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = d.Sites.Start(bg, site)
			return Applied{}, err
		}
		t.Step("✓ Site durduruldu")
	}
	rec.Phase = phaseRestoring
	rec.Files.Moved = true
	if err := d.saveRecovery(rec); err != nil {
		if running {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			_ = d.Sites.Start(bg, site)
		}
		return Applied{}, err
	}
	err = os.Rename(sf.Dir, sf.path(rec.Files.Old))
	if err == nil {
		if err = os.Rename(chosen, sf.Dir); err != nil {
			_ = os.Rename(sf.path(rec.Files.Old), sf.Dir)
		}
	}
	if err == nil {
		err = d.checkFiles(sf, want)
	}
	if err == nil {
		t.Step(fmt.Sprintf("✓ Kalıcı klasör %s kopyasından geri yüklendi; önceki hali %s", id, rec.Files.Old))
		rec.Phase = phaseVerified
		err = d.saveRecovery(rec)
	}
	if err == nil && running {
		if err = d.Sites.Start(ctx, site); err == nil && healthy {
			err = d.Sites.Health(ctx, site, healthTries)
		}
	}
	if err == nil {
		res := Applied{FilesOld: strings.TrimPrefix(rec.Files.Old, dataOldPrefix)}
		t.SetResult(res.result())
		t.Step(fmt.Sprintf("ℹ Bu işlemi geri almak için: Dosya kopyaları → %s → Geri yükle", res.FilesOld))
		return res, nil
	}
	t.Step("✗ " + err.Error())
	rec.Phase = phaseRollback // a crash while undoing must not count as "verified"
	if serr := d.saveRecovery(rec); serr != nil {
		t.Step("⚠ Kurtarma kaydı güncellenemedi: " + serr.Error())
	}
	bg, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if running {
		_ = d.Sites.Stop(bg, site)
	}
	// Back as it was: the current directory returns, the chosen one is the backup again.
	if rerr := d.unswap(sf, rec.Files.Old, rec.Files.Back); rerr != nil {
		return Applied{}, fmt.Errorf("%v — GERİ ALMA DA BAŞARISIZ: %v (önceki klasör %s)", err, rerr, rec.Files.Old)
	}
	if running {
		_ = d.Sites.Start(bg, site)
	}
	return Applied{}, fmt.Errorf("%v (geri alındı, dosyalar değişmedi)", err)
}

// Recover finishes what an agent stop interrupted: unless nothing had changed yet or
// the new data was already complete, the copies are put back; the site is started if it
// ran before. A record is tried once: if this fails, it is removed and the error says
// how to restore by hand, so an old record can never be replayed over newer data later.
func (d *DataOps) Recover(ctx context.Context, t *Task, r recovery) error {
	d.init()
	defer d.dropStaging(t)
	defer d.removeRecovery(r.Job)
	t.Step(fmt.Sprintf("ℹ Agent, %s üzerinde bir veri işlemi sürerken durdu (iş %s, %s)", r.Site, r.Job, r.Started.Local().Format("02.01.2006 15:04:05")))
	var sf siteFiles
	if r.Files != nil {
		var err error
		if sf, err = d.siteFiles(r.Site); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			// data/ itself is missing when the agent stopped between the two renames.
			spec, gerr := d.Store.Get(r.Site)
			if gerr != nil {
				return gerr
			}
			sf = siteFiles{Spec: spec, Dir: filepath.Join(d.Store.Root, spec.Dir(), sites.DataDir)}
		}
		defer func() { _ = os.RemoveAll(sf.path(dataNew)) }()
	}
	switch r.Phase {
	case phaseVerified:
		t.Step("ℹ Yeni veri tamamen yüklenmiş ve sayılmıştı; korunuyor")
	case phasePrepare:
		t.Step("ℹ Veriye henüz dokunulmamıştı")
	default:
		if running, _ := d.Sites.Running(ctx, r.Site); running {
			if err := d.Sites.Stop(ctx, r.Site); err != nil {
				return err
			}
		}
		if r.DB {
			_, creds, err := d.siteDB(r.Site)
			if err != nil {
				return err
			}
			snap, _, err := d.SnapshotFile(r.Site, r.Snapshot)
			if err != nil {
				return fmt.Errorf("güvenlik kopyası %s bulunamadı: %w — site durduruldu, veritabanı yarım olabilir", r.Snapshot, err)
			}
			back, err := d.snapshotSource(snap)
			if err != nil {
				return err
			}
			t.Step("… Veritabanı " + snap.ID + " kopyasından eski haline getiriliyor")
			if err := d.rollback(ctx, t, creds, back); err != nil {
				return fmt.Errorf("geri alma başarısız: %w — site durduruldu; panelde Veritabanı kopyaları → %s → Geri yükle", err, snap.ID)
			}
		}
		if r.Files != nil && r.Files.Moved {
			if err := d.unswap(sf, r.Files.Old, r.Files.Back); err != nil {
				return fmt.Errorf("kalıcı klasör geri konamadı: %w — site durduruldu; önceki klasör %s", err, r.Files.Old)
			}
			t.Step("✓ Kalıcı klasör eski haline döndü")
		}
	}
	if r.Running {
		if err := d.Sites.Start(ctx, r.Site); err != nil {
			return err
		}
		t.Step("✓ Site başlatıldı")
	}
	return nil
}
