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
	"github.com/swbilisim/FootOps/internal/version"
)

// A FootOps archive (v1) is an uncompressed tar, so its members can be read in place:
//
//	db.archive.gz   mongodump --archive --gzip of the site's database
//	manifest.json   what the archive holds, with every member's size and SHA-256
//
// The manifest comes last because it describes the members before it; the dump is
// written straight into the tar (its header is filled in once the size is known), so an
// export needs no second copy on disk. Files (files.tar.gz) come with F2b.
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
	Members     map[string]ExportMember `json:"members"`
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
}

// Export writes a FootOps archive of the site's database into the staging area, where
// it stays for a day (rule K9) for download.
func (d *DataOps) Export(ctx context.Context, t *Task, site string) (info ExportInfo, err error) {
	d.init()
	spec, creds, err := d.siteDB(site)
	if err != nil {
		return info, err
	}
	st, err := d.stats(ctx, creds)
	if err != nil {
		return info, err
	}
	t.Step(fmt.Sprintf("✓ Veritabanı %s: %d koleksiyon, %d belge, %s veri", creds.DB, st.Collections, st.Objects, humanBytes(st.DataSize)))
	// The compressed dump is smaller than the data; the data size is a safe estimate.
	if err := d.Staging.Allow(st.DataSize + 1<<20); err != nil {
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
	if _, err := f.Write(make([]byte, 512)); err != nil { // the member header, written last
		return info, err
	}

	cfgDir, err := d.toolConfig(t.ID(), creds)
	if err != nil {
		return info, err
	}
	defer func() { _ = d.Staging.Remove(t.ID()) }()
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h), report: func(n int64) { t.SetBytes(n, 0) }}
	bw := bufio.NewWriterSize(cw, 256<<10)
	counts := map[string]int64{}
	t.Step("… mongodump çalışıyor (sınır: " + limitText(d.Limits) + ")")
	cmd := d.toolCmd(ctx, t.ID(), cfgDir, "mongodump", "--config=/run/footops/mongo.yaml",
		"--db="+creds.DB, "--archive", "--gzip", "--numParallelCollections=1")
	cmd.Stdout = bw
	cmd.Stderr = func(line string) {
		if m := reDumped.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			counts[strings.TrimPrefix(m[1], creds.DB+".")] = n
		}
	}
	u, err := d.Stream.Stream(ctx, cmd)
	t.AddUsage(u)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return info, fmt.Errorf("mongodump başarısız: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return info, err
	}
	size := cw.n
	t.SetBytes(size, 0)
	t.Step(fmt.Sprintf("✓ mongodump bitti: %d koleksiyon, %d belge → %s (%s)", len(counts), sum(counts), humanBytes(size), usageNote(u)))

	hdr, err := tarHeader(memberDB, size, now)
	if err != nil {
		return info, err
	}
	if _, err := f.WriteAt(hdr, 0); err != nil {
		return info, err
	}
	// Rule K2 for our own output: the member reads back as an intact mongodump archive.
	p, _, err := readMongoSection(ctxReader{ctx, io.NewSectionReader(f, 512, size)}, true, sha256.New())
	if err != nil {
		return info, fmt.Errorf("arşiv doğrulanamadı: %w", err)
	}
	views := map[string]bool{}
	for _, n := range p.Collections(creds.DB) {
		views[n.Collection] = n.Type == "view"
	}
	man := ExportManifest{Format: exportFormat, FootOps: version.Version, Site: spec.Name, Group: spec.Group, Created: now,
		DB: creds.DB, MongoServer: p.ServerVersion, MongoTool: core.MongoImage, Collections: map[string]int64{},
		Members: map[string]ExportMember{memberDB: {Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}}}
	for c, n := range counts {
		if !views[c] && !strings.HasPrefix(c, "system.") {
			man.Collections[c] = n
			man.Documents += n
		}
	}
	b, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return info, err
	}
	if _, err := f.Seek(512+size+(512-size%512)%512, io.SeekStart); err != nil {
		return info, err
	}
	if err := writeTail(f, size, b, now); err != nil {
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
	ib, err := json.Marshal(info)
	if err != nil {
		return info, err
	}
	if err := writeSynced(filepath.Join(dir, exportInfo), ib); err != nil {
		return info, err
	}
	t.SetResult(info.ID)
	t.Step(fmt.Sprintf("✓ Arşiv hazır: %s (%s); sunucuda %d saat indirilebilir", name, humanBytes(total), int(stagingKeep().Hours())))
	return info, nil
}

// writeTail pads the database member to a full block, then adds the manifest and the
// tar end marker.
func writeTail(f *os.File, size int64, manifest []byte, now time.Time) error {
	if pad := (512 - size%512) % 512; pad > 0 {
		if _, err := f.WriteAt(make([]byte, pad), 512+size); err != nil {
			return err
		}
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
	if err := tw.WriteHeader(&tar.Header{Name: name, Size: size, Mode: 0o644, ModTime: mod,
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
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
