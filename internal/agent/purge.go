package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

// Removed sites and their permanent removal (ILERLEME Faz D "Kaldırılan siteyi kalıcı
// sil"). Removing a site keeps its directory under /srv/<group>/_removed/<site>-<when>
// and its database with its user: deleting a customer's data is never one click. Here
// it is a second, confirmed step: the folder, the database and its users go, after a
// last copy of the database (kept SnapshotKeep like every copy: a site made again under
// the same name finds it among its database copies). When a site of the same name
// exists again, the database is that site's: only the folder goes.

// RemovedSite is a removed site's directory.
type RemovedSite struct {
	ID      string    `json:"id"` // the directory's name: <site>-<YYYYMMDD-HHMMSS>
	Site    string    `json:"site"`
	Group   string    `json:"group"`
	Removed time.Time `json:"removed"`
	// Database: the site had a FootOps database; Image its image.
	Database bool   `json:"database"`
	Image    string `json:"image,omitempty"`
	// Current: a site of this name exists again (its database is that site's). Older: a
	// later removal of the same name, also with a database, keeps the database (one
	// database a name): this entry is its folder alone.
	Current bool `json:"current,omitempty"`
	Older   bool `json:"older,omitempty"`
	// With detail only: the folder's size, and the database's (DBBytes -1: none any more;
	// DBErr: MongoDB could not be asked). Without Database, DBBytes > 0 is a database of
	// that name that the site's definition does not claim: it is left alone.
	Bytes   int64  `json:"bytes,omitempty"`
	Files   int64  `json:"files,omitempty"`
	DBBytes int64  `json:"db_bytes,omitempty"`
	DBErr   string `json:"db_error,omitempty"`
	// Broken: its footops.json could not be read (only the folder can go).
	Broken string `json:"broken,omitempty"`
}

var reRemovedID = regexp.MustCompile(`^(.+)-([0-9]{8}-[0-9]{6})$`)

// removedSizeCap bounds how many entries a folder's size counts.
const removedSizeCap = 500_000

func (d *DataOps) removedDir(group, id string) string {
	return filepath.Join(d.Store.Root, sites.SrvDir, group, sites.RemovedDir, id)
}

// Removed lists the removed sites, newest first; detail adds the folder's and the
// database's size (walks the folders, asks MongoDB).
func (d *DataOps) Removed(ctx context.Context, detail bool) ([]RemovedSite, error) {
	d.init()
	current := map[string]bool{}
	list, err := d.Store.List()
	if err != nil {
		return nil, err
	}
	for _, s := range list {
		current[s.Name] = true
	}
	groups, err := d.Store.Groups()
	if err != nil {
		return nil, err
	}
	var out []RemovedSite
	for _, g := range groups {
		ents, err := os.ReadDir(filepath.Join(d.Store.Root, sites.SrvDir, g, sites.RemovedDir))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			r, ok := d.removedOf(g, e)
			if !ok {
				continue
			}
			r.Current = current[r.Site]
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Removed.Equal(out[j].Removed) {
			return out[i].Removed.After(out[j].Removed)
		}
		return out[i].ID < out[j].ID
	})
	claimed := map[string]bool{} // newest first: the first entry with a database owns it
	for i := range out {
		if r := &out[i]; r.Database && !r.Current {
			r.Older = claimed[r.Site]
			claimed[r.Site] = true
		}
	}
	if !detail {
		return out, nil
	}
	var dbs map[string]int64
	var dbErr error
	for i := range out {
		r := &out[i]
		r.Bytes, r.Files = folderSize(d.removedDir(r.Group, r.ID))
		if r.Current {
			continue
		}
		if dbs == nil && dbErr == nil {
			if p, ok := d.mongoOps().(dbPurger); ok {
				dbs, dbErr = p.Databases(ctx)
			} else {
				dbErr = errors.New("MongoDB'ye sorulamıyor")
			}
			if dbs == nil && dbErr == nil {
				dbs = map[string]int64{}
			}
		}
		if dbErr != nil {
			r.DBErr = dbErr.Error()
			continue
		}
		r.DBBytes = -1
		if n, ok := dbs[r.Site]; ok {
			r.DBBytes = n
		} else if !r.Database {
			r.DBBytes = 0
		}
	}
	return out, nil
}

// removedOf reads one entry of a group's _removed directory.
func (d *DataOps) removedOf(group string, e fs.DirEntry) (RemovedSite, bool) {
	m := reRemovedID.FindStringSubmatch(e.Name())
	if !e.IsDir() || m == nil || !docker.ValidName(m[1]) {
		return RemovedSite{}, false
	}
	when, err := time.Parse("20060102-150405", m[2])
	if err != nil {
		return RemovedSite{}, false
	}
	r := RemovedSite{ID: e.Name(), Site: m[1], Group: group, Removed: when}
	spec, err := d.removedSpec(group, e.Name())
	if err != nil {
		r.Broken = err.Error()
		return r, true
	}
	r.Database, r.Image = spec.Database, spec.Image
	return r, true
}

func (d *DataOps) removedSpec(group, id string) (sites.Spec, error) {
	var spec sites.Spec
	b, err := os.ReadFile(filepath.Join(d.removedDir(group, id), sites.MetaFile))
	if err != nil {
		return spec, fmt.Errorf("site tanımı okunamadı: %w", err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return spec, errors.New("site tanımı bozuk")
	}
	return spec, nil
}

// folderSize adds up a folder's regular files (stops counting at removedSizeCap
// entries; links are not followed).
func folderSize(dir string) (bytes, files int64) {
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if n++; n > removedSizeCap {
			return filepath.SkipAll
		}
		if e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				bytes += info.Size()
				files++
			}
		}
		return nil
	})
	return bytes, files
}

// removedKept reports whether a removed site of this name is still kept (not removed
// for good): its database copies keep their newest, like a site's.
func (d *DataOps) removedKept(site string) bool {
	groups, _ := d.Store.Groups()
	for _, g := range groups {
		ents, err := os.ReadDir(filepath.Join(d.Store.Root, sites.SrvDir, g, sites.RemovedDir))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if m := reRemovedID.FindStringSubmatch(e.Name()); m != nil && m[1] == site && e.IsDir() {
				return true
			}
		}
	}
	return false
}

// FindRemoved returns one removed site by its directory's name.
func (d *DataOps) FindRemoved(ctx context.Context, id string) (RemovedSite, error) {
	list, err := d.Removed(ctx, true)
	if err != nil {
		return RemovedSite{}, err
	}
	for _, r := range list {
		if r.ID == id {
			return r, nil
		}
	}
	return RemovedSite{}, errRemovedGone
}

var errRemovedGone = errors.New("kaldırılan site bulunamadı (zaten silinmiş olabilir)")

// Purge removes a removed site for good: the database's last copy, then the database
// with its users (unless a site of the same name uses it now), the records kept for it
// and the folder; the image too if nothing else uses it. Each step only runs once the
// one before it worked: a database that cannot be copied stays, and so does everything
// after it.
func (d *DataOps) Purge(ctx context.Context, t *Task, r RemovedSite, actor Actor) (string, error) {
	d.init()
	dir := d.removedDir(r.Group, r.ID)
	if _, err := os.Lstat(dir); err != nil {
		return "", errRemovedGone
	}
	t.Step(fmt.Sprintf("ℹ Kalıcı silinecek: %s (%s grubu, %s kaldırıldı) — klasör %s, %d dosya",
		r.Site, r.Group, r.Removed.Local().Format("2006-01-02 15:04"), humanBytes(r.Bytes), r.Files))
	var detail []string
	live, err := d.Store.List()
	if err != nil {
		return "", fmt.Errorf("siteler okunamadı: %w — hiçbir şey silinmedi", err)
	}
	current, imageInUse := false, false
	for _, sp := range live {
		current = current || sp.Name == r.Site
		imageInUse = imageInUse || sp.Image == r.Image
	}
	if !current && d.Docker != nil {
		// A container of that name without a site: something runs that FootOps does not
		// know of, perhaps on this database. Removal stops there, as the script did.
		if _, err := d.Docker.Inspect(ctx, r.Site); err == nil {
			return "", fmt.Errorf("%s adında bir container çalışıyor ama bu adla site yok — hiçbir şey silinmedi (sunucuda elle bakılmalı)", r.Site)
		} else if !docker.IsNotFound(err) {
			return "", fmt.Errorf("Docker'a sorulamadı: %w — hiçbir şey silinmedi", err)
		}
	}
	switch {
	case current:
		t.Step("ℹ Bu adla bir site yeniden kurulmuş: veritabanı, kayıtlar ve kopyalar onun; dokunulmadı, yalnız eski klasör silinir")
		detail = append(detail, "yeni site var: veritabanına dokunulmadı")
	case r.Older:
		t.Step("ℹ Bu adın daha sonra kaldırılan bir kaydı var: veritabanı onunla birlikte silinir; burada yalnız eski klasör silinir")
		detail = append(detail, "daha yeni kayıt var: veritabanına dokunulmadı")
	case r.Database || r.Broken != "":
		p, ok := d.mongoOps().(dbPurger)
		if !ok {
			return "", errors.New("MongoDB'ye sorulamıyor — hiçbir şey silinmedi")
		}
		dbs, err := p.Databases(ctx)
		if err != nil {
			return "", fmt.Errorf("MongoDB'ye ulaşılamadı: %w — hiçbir şey silinmedi", err)
		}
		_, hasDB := dbs[r.Site]
		if r.Broken != "" {
			// Whether the site had a database is only told by MongoDB now.
			if hasDB {
				return "", fmt.Errorf("%s; MongoDB'de %s veritabanı var, kopyası bu tanımsız alınamaz — hiçbir şey silinmedi (sunucuda elle bakılmalı)", r.Broken, r.Site)
			}
			t.Step("ℹ Site tanımı okunamadı; MongoDB'de bu adla veritabanı yok: yalnız klasör silinir")
			break
		}
		if hasDB {
			spec, creds, err := d.lastCopyAccess(ctx, r)
			if err != nil {
				return "", fmt.Errorf("veritabanının son kopyası alınamadı: %w — hiçbir şey silinmedi", err)
			}
			snap, err := d.snapshotOf(ctx, t, spec, creds, "before-purge", actor)
			if err != nil {
				return "", fmt.Errorf("veritabanının son kopyası alınamadı: %w — hiçbir şey silinmedi", err)
			}
			detail = append(detail, "son kopya "+snap.ID)
		} else {
			t.Step("ℹ Veritabanı zaten yok; varsa kullanıcıları silinir")
		}
		if err := p.DropSiteDB(ctx, r.Site); err != nil {
			return "", fmt.Errorf("veritabanı silinemedi: %w — klasör yerinde duruyor", err)
		}
		t.Step("✓ MongoDB: " + r.Site + " veritabanı ve kullanıcıları silindi")
		detail = append(detail, "veritabanı silindi")
	}
	if !current {
		for _, base := range []string{d.DBEditsDir, d.MigrationsDir} {
			if base == "" {
				continue
			}
			if err := os.RemoveAll(filepath.Join(base, r.Site)); err != nil {
				t.Step("⚠ " + err.Error())
			}
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return strings.Join(detail, ", "), fmt.Errorf("klasör silinemedi: %w", err)
	}
	t.Step(fmt.Sprintf("✓ Klasör silindi: %s (%s)", filepath.Join(sites.SrvDir, r.Group, sites.RemovedDir, r.ID), humanBytes(r.Bytes)))
	if d.Store.DropGroup(r.Group) { // the group's _removed goes once empty, the group with it
		t.Step("✓ " + r.Group + " grubunda site kalmadı; grubun klasörü de silindi: " + sites.GroupDir(r.Group))
	}
	detail = append(detail, "klasör "+humanBytes(r.Bytes))
	if r.Image != "" && imageInUse {
		t.Step("ℹ İmaj bırakıldı (çalışan bir site de kullanıyor): " + r.Image)
	} else if r.Image != "" && sites.ValidImage(r.Image) {
		if _, err := d.Run.Run(ctx, "docker", "image", "rm", r.Image); err == nil {
			t.Step("✓ İmaj silindi: " + r.Image)
			detail = append(detail, "imaj silindi")
		} else {
			t.Step("ℹ İmaj silinmedi (başka bir container kullanıyor ya da zaten yok): " + r.Image)
		}
	}
	return strings.Join(detail, ", "), nil
}

// lastCopyAccess opens the removed site's database for its last copy as the site's own
// user (rule K4), with a new password root gives it: the password kept in compose.yaml
// may no longer work (a site made again under that name changes it; one whose creation
// failed drops the user), and the user goes with the database right after.
func (d *DataOps) lastCopyAccess(ctx context.Context, r RemovedSite) (sites.Spec, dbCreds, error) {
	spec, err := d.removedSpec(r.Group, r.ID)
	if err != nil {
		return spec, dbCreds{}, err
	}
	if spec.Name != r.Site || spec.DBName() != r.Site {
		return spec, dbCreds{}, fmt.Errorf("site tanımındaki ad (%s) klasörün adıyla tutmuyor", spec.Name)
	}
	pw, err := randomSecret(32)
	if err != nil {
		return spec, dbCreds{}, err
	}
	if err := d.mongoOps().SetUser(ctx, spec.DBName(), spec.DBUser(), pw); err != nil {
		return spec, dbCreds{}, fmt.Errorf("geçici erişim verilemedi: %w", err)
	}
	return spec, dbCreds{User: spec.DBUser(), Password: pw, DB: spec.DBName()}, nil
}
