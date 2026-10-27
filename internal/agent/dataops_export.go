package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
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

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/datafs"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/version"
)

// A FootOps archive (v1) is an uncompressed tar, so its members can be read in place:
//
//	db.archive.gz   mongodump --archive --gzip of the site's database (if it has one)
//	files.tar.gz    the site's data directory, gzip level 1 (if it has one)
//	manifest.json   what the archive holds, with every member's size and SHA-256
//
// The manifest comes last because it describes the members before it; each member is
// written straight into the tar (its header is filled in once the size is known), so an
// export needs no second copy on disk.
const (
	exportFormat = 1
	exportPrefix = "exp-"
	exportInfo   = "export.json"
)

var reJobID = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

// ExportMember is one member of a FootOps archive.
type ExportMember struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ExportManifest is manifest.json.
type ExportManifest struct {
	Format      int                     `json:"format"`
	FootOps     string                  `json:"footops"`
	Site        string                  `json:"site"`
	Group       string                  `json:"group"`
	Created     time.Time               `json:"created"`
	DB          string                  `json:"db"`
	MongoServer string                  `json:"mongo_server"`
	MongoTool   string                  `json:"mongo_tool"`
	Collections map[string]int64        `json:"collections"`
	Documents   int64                   `json:"documents"`
	Files       *ExportFiles            `json:"files,omitempty"`
	Members     map[string]ExportMember `json:"members"`
}

// ExportFiles summarises the files member.
type ExportFiles struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Bytes int64 `json:"bytes"`
}

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
	name := fmt.Sprintf("%s-%s.footops.tar", spec.Name, now.Format("20060102-150405"))
	part := filepath.Join(dir, name+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return info, err
	}
	defer f.Close()
	man := ExportManifest{Format: exportFormat, FootOps: version.Version, Site: spec.Name, Group: spec.Group, Created: now,
		Members: map[string]ExportMember{}}

	if spec.Database {
		cfgDir, err := d.toolConfig(t.ID(), creds)
		if err != nil {
			return info, err
		}
		defer func() { _ = d.Staging.Remove(t.ID()) }()
		counts := map[string]int64{}
		var u run.Usage
		m, err := writeMember(f, memberDB, now, func(w io.Writer, cw *countingWriter) error {
			cw.report = func(n int64) { t.SetBytes(n, 0) }
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
		p, _, err := readMongoSection(ctxReader{ctx, io.NewSectionReader(f, m.offset, m.Size)}, true, sha256.New())
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
		man.Members[memberDB] = m.ExportMember
	}

	if spec.DataMount != "" {
		src, err := os.Open(sf.Dir)
		if err != nil {
			return info, err
		}
		defer src.Close()
		var hs helperStats
		var u run.Usage
		m, err := writeMember(f, memberFiles, now, func(w io.Writer, _ *countingWriter) error {
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
		s, err := checkFilesMember(ctxReader{ctx, io.NewSectionReader(f, m.offset, m.Size)}, m.Size)
		if err != nil {
			return info, fmt.Errorf("dosya arşivi doğrulanamadı: %w", err)
		}
		if s.Files != hs.Result.Files || s.Size != hs.Result.Bytes {
			return info, fmt.Errorf("dosya arşivi tutmuyor: %d dosya / %d bayt, paketlenen %d / %d", s.Files, s.Size, hs.Result.Files, hs.Result.Bytes)
		}
		t.Step(fmt.Sprintf("✓ Dosyalar paketlendi: %d dosya, %d klasör, %s → %s (%s)", hs.Result.Files, hs.Result.Dirs,
			humanBytes(hs.Result.Bytes), humanBytes(m.Size), usageNote(u)))
		man.Files = &ExportFiles{Files: hs.Result.Files, Dirs: hs.Result.Dirs, Bytes: hs.Result.Bytes}
		man.Members[memberFiles] = m.ExportMember
	}

	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return info, err
	}
	if err := writeTail(f, b, now); err != nil {
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

// member is a written tar member: where its body starts, its size and checksum.
type member struct {
	ExportMember
	offset int64
}

// writeMember writes a tar member whose size is known only afterwards, at the file's
// current end: a placeholder header, the body from fill, padding, then the real header.
// Nothing is buffered or written twice.
func writeMember(f *os.File, name string, mod time.Time, fill func(w io.Writer, cw *countingWriter) error) (member, error) {
	start, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return member{}, err
	}
	if _, err := f.Write(make([]byte, 512)); err != nil {
		return member{}, err
	}
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h)}
	bw := bufio.NewWriterSize(cw, 256<<10)
	if err := fill(bw, cw); err != nil {
		return member{}, err
	}
	if err := bw.Flush(); err != nil {
		return member{}, err
	}
	size := cw.n
	if pad := (512 - size%512) % 512; pad > 0 {
		if _, err := f.Write(make([]byte, pad)); err != nil {
			return member{}, err
		}
	}
	hdr, err := tarHeader(name, size, mod)
	if err != nil {
		return member{}, err
	}
	if _, err := f.WriteAt(hdr, start); err != nil {
		return member{}, err
	}
	return member{ExportMember: ExportMember{Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, offset: start + 512}, nil
}

// writeTail adds the manifest and the tar end marker at the file's end.
func writeTail(f *os.File, manifest []byte, now time.Time) error {
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{Name: memberManifest, Size: int64(len(manifest)), Mode: 0o644, ModTime: now,
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}
	return tw.Close()
}

// tarHeader renders the 512-byte header of a regular file.
func tarHeader(name string, size int64, mod time.Time) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// USTAR stores sizes below 8 GiB; GNU stores larger ones in the same single block.
	format := tar.FormatUSTAR
	if size >= 8<<30 {
		format = tar.FormatGNU
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: 0o644, ModTime: mod,
		Typeflag: tar.TypeReg, Format: format}); err != nil {
		return nil, fmt.Errorf("tar başlığı: %w", err)
	}
	if buf.Len() != 512 {
		return nil, errors.New("tar başlığı beklenmeyen uzunlukta")
	}
	return buf.Bytes(), nil
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
