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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/version"
)

// A site's export is a FootOps archive (internal/archive): its database and its data
// directory, whichever it has, written into the staging area for a day's download.
const (
	exportPrefix = "exp-"
	exportInfo   = "export.json"
)

var reJobID = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// ExportInfo describes an archive waiting to be downloaded.
type ExportInfo struct {
	ID          string    `json:"id"` // the job that made it
	Site        string    `json:"site"`
	File        string    `json:"file"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	Created     time.Time `json:"created"`
	Collections int       `json:"collections"`
	Documents   int64     `json:"documents"`
	Files       int       `json:"files"`
	FileBytes   int64     `json:"file_bytes"`
	// Backup is the time of the backup the archive was made from (zero: the site as it
	// was); Env tells it holds the site's environment.
	Backup time.Time `json:"backup,omitempty"`
	Env    bool      `json:"env,omitempty"`
}

// Export writes a FootOps archive of the site into the staging area, where it stays for
// a day (rule K9) for download: its database and its data directory, whichever it has.
func (d *DataOps) Export(ctx context.Context, t *Task, site string) (info ExportInfo, err error) {
	d.init()
	spec, err := d.Store.Get(site)
	if err != nil {
		return info, err
	}
	var creds dbCreds
	var st dbStats
	need := int64(1 << 20)
	if spec.Database {
		if _, creds, err = d.siteDB(site); err != nil {
			return info, err
		}
		if st, err = d.stats(ctx, creds); err != nil {
			return info, err
		}
		t.Step(fmt.Sprintf("✓ Veritabanı %s: %d koleksiyon, %d belge, %s veri", creds.DB, st.Collections, st.Objects, humanBytes(st.DataSize)))
		need += st.DataSize // the compressed dump is smaller than the data
	}
	var sf siteFiles
	var fileStats datafs.Stats
	if spec.DataMount != "" {
		if sf, err = d.siteFiles(site); err != nil {
			return info, err
		}
		if fileStats, err = countFiles(sf.Dir); err != nil {
			return info, fmt.Errorf("kalıcı klasör okunamadı: %w", err)
		}
		t.Step(fmt.Sprintf("✓ Kalıcı klasör: %d dosya, %s", fileStats.Files, humanBytes(fileStats.Bytes)))
		need += fileStats.Bytes // photos barely compress
	}
	if !spec.Database && spec.DataMount == "" {
		return info, errors.New("bu sitenin ne veritabanı ne kalıcı klasörü var")
	}
	if err := d.Staging.Allow(need); err != nil {
		return info, err
	}
	dir, err := d.Staging.Dir(exportPrefix + t.ID())
	if err != nil {
		return info, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	now := d.now().UTC().Truncate(time.Second)
	// The name shows the panel's (the server's) time, the manifest UTC.
	name := fmt.Sprintf("%s-%s.footops.tar", spec.Name, now.Local().Format("20060102-150405"))
	part := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return info, err
	}
	defer f.Close()
	aw := archive.NewWriter(f)
	man := archive.Manifest{FootOps: version.Version, Site: spec.Name, Group: spec.Group, Created: now}

	if spec.Database {
		cfgDir, err := d.toolConfig(t.ID(), creds)
		if err != nil {
			return info, err
		}
		defer func() { _ = d.Staging.Remove(t.ID()) }()
		counts := map[string]int64{}
		var u run.Usage
		m, err := aw.Add(archive.MemberDB, now, func(n int64) { t.SetBytes(n, 0) }, func(w io.Writer) error {
			t.Step("… mongodump çalışıyor (sınır: " + limitText(d.Limits) + ")")
			cmd := d.toolCmd(ctx, t.ID(), cfgDir, "mongodump", "--config=/run/footops/mongo.yaml",
				"--db="+creds.DB, "--archive", "--gzip", "--numParallelCollections=1")
			cmd.Stdout = w
			cmd.Stderr = func(line string) {
				if m := reDumped.FindStringSubmatch(line); m != nil {
					n, _ := strconv.ParseInt(m[2], 10, 64)
					counts[strings.TrimPrefix(m[1], creds.DB+".")] = n
				}
			}
			var err error
			u, err = d.Stream.Stream(ctx, cmd)
			t.AddUsage(u)
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				return fmt.Errorf("mongodump başarısız: %w", err)
			}
			return nil
		})
		if err != nil {
			return info, err
		}
		t.Step(fmt.Sprintf("✓ mongodump bitti: %d koleksiyon, %d belge → %s (%s)", len(counts), sum(counts), humanBytes(m.Size), usageNote(u)))
		// Rule K2 for our own output: the member reads back as an intact mongodump archive.
		p, _, err := archive.ReadMongo(ctxReader{ctx, io.NewSectionReader(f, m.Offset, m.Size)}, true, sha256.New())
		if err != nil {
			return info, fmt.Errorf("arşiv doğrulanamadı: %w", err)
		}
		views := map[string]bool{}
		for _, n := range p.Collections(creds.DB) {
			views[n.Collection] = n.Type == "view"
		}
		man.DB, man.MongoServer, man.MongoTool, man.Collections = creds.DB, p.ServerVersion, core.MongoImage, map[string]int64{}
		for c, n := range counts {
			if !views[c] && !strings.HasPrefix(c, "system.") {
				man.Collections[c] = n
				man.Documents += n
			}
		}
	}

	if spec.DataMount != "" {
		src, err := os.Open(sf.Dir)
		if err != nil {
			return info, err
		}
		defer src.Close()
		var hs helperStats
		var u run.Usage
		m, err := aw.Add(archive.MemberFiles, now, nil, func(w io.Writer) error {
			t.Step("… Dosyalar paketleniyor (site kullanıcısı " + strconv.Itoa(sf.UID) + ", sınır: " + limitText(helperLimits) + ")")
			cmd := d.helper(ctx, t.ID(), []*os.File{src}, "data-pack", "--uid="+strconv.Itoa(sf.UID), "--gid="+strconv.Itoa(sf.GID))
			cmd.Stdout = w
			cmd.Stderr = hs.line(t, fileStats.Bytes)
			var err error
			u, err = d.Stream.Stream(ctx, cmd)
			t.AddUsage(u)
			if err == nil {
				err = ctx.Err()
			}
			if err == nil && !hs.done {
				err = errors.New("yardımcı sonuç bildirmedi")
			}
			if err != nil {
				return fmt.Errorf("dosyalar paketlenemedi: %w", err)
			}
			return nil
		})
		if err != nil {
			return info, err
		}
		// Read our own output back like an upload: names, sizes, gzip checksum.
		s, err := archive.CheckFilesMember(ctxReader{ctx, io.NewSectionReader(f, m.Offset, m.Size)}, m.Size)
		if err != nil {
			return info, fmt.Errorf("dosya arşivi doğrulanamadı: %w", err)
		}
		if s.Files != hs.Result.Files || s.Size != hs.Result.Bytes {
			return info, fmt.Errorf("dosya arşivi tutmuyor: %d dosya / %d bayt, paketlenen %d / %d", s.Files, s.Size, hs.Result.Files, hs.Result.Bytes)
		}
		t.Step(fmt.Sprintf("✓ Dosyalar paketlendi: %d dosya, %d klasör, %s → %s (%s)", hs.Result.Files, hs.Result.Dirs,
			humanBytes(hs.Result.Bytes), humanBytes(m.Size), usageNote(u)))
		man.Files = &archive.ManifestFiles{Files: hs.Result.Files, Dirs: hs.Result.Dirs, Bytes: hs.Result.Bytes}
	}

	if err := aw.Finish(man, now); err != nil {
		return info, err
	}
	if err := f.Sync(); err != nil {
		return info, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return info, err
	}
	whole := sha256.New()
	total, err := io.Copy(whole, ctxReader{ctx, bufio.NewReaderSize(f, 256<<10)})
	if err != nil {
		return info, err
	}
	if err := f.Close(); err != nil {
		return info, err
	}
	if err := os.Rename(part, filepath.Join(dir, name)); err != nil {
		return info, err
	}
	info = ExportInfo{ID: t.ID(), Site: spec.Name, File: name, Bytes: total, SHA256: hex.EncodeToString(whole.Sum(nil)),
		Created: now, Collections: len(man.Collections), Documents: man.Documents}
	if man.Files != nil {
		info.Files, info.FileBytes = man.Files.Files, man.Files.Bytes
	}
	ib, err := json.Marshal(info)
	if err != nil {
		return info, err
	}
	if err := writeSynced(filepath.Join(dir, exportInfo), ib); err != nil {
		return info, err
	}
	t.SetBytes(total, 0)
	t.SetResult(info.ID)
	t.Step(fmt.Sprintf("✓ Arşiv hazır: %s (%s); sunucuda %d saat indirilebilir", name, humanBytes(total), int(stagingKeep().Hours())))
	return info, nil
}

func stagingKeep() time.Duration { return 24 * time.Hour }

// Exports lists a site's archives still waiting in the staging area, newest first.
func (d *DataOps) Exports(site string) ([]ExportInfo, error) {
	ents, err := os.ReadDir(d.Staging.Root)
	if err != nil {
		return []ExportInfo{}, nil
	}
	out := []ExportInfo{}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), exportPrefix) {
			continue
		}
		info, err := d.readExport(strings.TrimPrefix(e.Name(), exportPrefix))
		if err == nil && info.Site == site {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out, nil
}

func (d *DataOps) readExport(id string) (ExportInfo, error) {
	var info ExportInfo
	if !reJobID.MatchString(id) {
		return info, errors.New("geçersiz arşiv")
	}
	b, err := os.ReadFile(filepath.Join(d.Staging.Root, exportPrefix+id, exportInfo))
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(b, &info); err != nil || info.ID != id || filepath.Base(info.File) != info.File ||
		!strings.HasSuffix(info.File, ".footops.tar") {
		return ExportInfo{}, errors.New("arşiv bilgisi bozuk")
	}
	return info, nil
}

// ExportFile returns an archive of site and its path.
func (d *DataOps) ExportFile(site, id string) (ExportInfo, string, error) {
	info, err := d.readExport(id)
	if err != nil || info.Site != site {
		return info, "", errors.New("arşiv bulunamadı (24 saatten eski olabilir)")
	}
	return info, filepath.Join(d.Staging.Root, exportPrefix+id, info.File), nil
}
