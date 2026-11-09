package archive

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// A FootOps archive (format 1, panel-tasarim §24.5) is an uncompressed tar, so its members
// can be read in place:
//
//	db.archive.gz   mongodump --archive --gzip of a site's database (if it has one)
//	files.tar.gz    the site's data directory, gzip level 1 (if it has one)
//	env             the site's environment as a .env (an archive made from a backup, if asked)
//	manifest.json   what the archive holds, with every member's size and SHA-256
//
// The manifest comes last because it describes the members before it; each member is
// written straight into the tar (its header is filled in once the size is known), so
// writing an archive needs no second copy on disk. The panel's export and import, a
// backup's İndir and footops-mac all write and read it here: the format is defined once.

const (
	// FootOpsFormat is the manifest's format number this code writes and reads.
	FootOpsFormat = 1

	MemberManifest = "manifest.json"
	MemberDB       = "db.archive.gz"
	MemberFiles    = "files.tar.gz"
	MemberEnv      = "env"

	// MaxManifest and MaxEnv bound the two members read whole into memory.
	MaxManifest = 1 << 20
	MaxEnv      = 256 << 10
)

// Member is one member of a FootOps archive as the manifest records it.
type Member struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is manifest.json.
type Manifest struct {
	Format      int               `json:"format"`
	FootOps     string            `json:"footops"`
	Site        string            `json:"site"`
	Group       string            `json:"group"`
	Created     time.Time         `json:"created"`
	DB          string            `json:"db"`
	MongoServer string            `json:"mongo_server"`
	MongoTool   string            `json:"mongo_tool"`
	Collections map[string]int64  `json:"collections"`
	Documents   int64             `json:"documents"`
	Files       *ManifestFiles    `json:"files,omitempty"`
	Members     map[string]Member `json:"members"`
	// Backup is set when the archive was made from a backup (Created is then the backup's
	// time, when the data was read).
	Backup *ManifestBackup `json:"backup,omitempty"`
}

// ManifestFiles summarises the files member.
type ManifestFiles struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Bytes int64 `json:"bytes"`
}

// ManifestBackup is the manifest's note of the backup an archive was made from.
type ManifestBackup struct {
	Run       string            `json:"run"`
	Kind      string            `json:"kind,omitempty"`
	Time      time.Time         `json:"time"`
	Server    string            `json:"server,omitempty"`
	Snapshots map[string]string `json:"snapshots"` // part → snapshot
}

// MemberAt is a member written into, or found in, an archive: where its body starts in
// the file, its size and checksum.
type MemberAt struct {
	Member
	Offset int64
}

// ---- writing ----

// Writer writes a FootOps archive into a new file: its members one after another (Add),
// then the manifest (Finish), which lists exactly what was added.
type Writer struct {
	f       *os.File
	members map[string]MemberAt
}

func NewWriter(f *os.File) *Writer { return &Writer{f: f, members: map[string]MemberAt{}} }

// Add writes a member whose size is known only afterwards, at the file's end: a
// placeholder header, the body from fill, padding, then the real header. Nothing is
// buffered or written twice. progress, if set, is told the body's bytes every megabyte.
func (w *Writer) Add(name string, mod time.Time, progress func(int64), fill func(io.Writer) error) (MemberAt, error) {
	switch name {
	case MemberDB, MemberFiles, MemberEnv:
	default:
		return MemberAt{}, fmt.Errorf("FootOps arşivinde %s olamaz", name)
	}
	if _, dup := w.members[name]; dup {
		return MemberAt{}, fmt.Errorf("%s iki kez yazılamaz", name)
	}
	start, err := w.f.Seek(0, io.SeekEnd)
	if err != nil {
		return MemberAt{}, err
	}
	if _, err := w.f.Write(make([]byte, 512)); err != nil {
		return MemberAt{}, err
	}
	h := sha256.New()
	cw := &counter{w: io.MultiWriter(w.f, h), report: progress}
	bw := bufio.NewWriterSize(cw, 256<<10)
	if err := fill(bw); err != nil {
		return MemberAt{}, err
	}
	if err := bw.Flush(); err != nil {
		return MemberAt{}, err
	}
	size := cw.n
	if pad := (512 - size%512) % 512; pad > 0 {
		if _, err := w.f.Write(make([]byte, pad)); err != nil {
			return MemberAt{}, err
		}
	}
	hdr, err := tarHeader(name, size, mod)
	if err != nil {
		return MemberAt{}, err
	}
	if _, err := w.f.WriteAt(hdr, start); err != nil {
		return MemberAt{}, err
	}
	m := MemberAt{Member: Member{Size: size, SHA256: hex.EncodeToString(h.Sum(nil))}, Offset: start + 512}
	w.members[name] = m
	return m, nil
}

// Finish ends the archive: the manifest — this format's number and the members added,
// with their sizes and checksums — and the tar's end marker.
func (w *Writer) Finish(m Manifest, mod time.Time) error {
	_, db := w.members[MemberDB]
	_, files := w.members[MemberFiles]
	if !db && !files {
		return errors.New("arşivde ne veritabanı ne dosya var")
	}
	m.Format = FootOpsFormat
	m.Members = map[string]Member{}
	for name, at := range w.members {
		m.Members[name] = at.Member
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if _, err := w.f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	tw := tar.NewWriter(w.f)
	if err := tw.WriteHeader(&tar.Header{Name: MemberManifest, Size: int64(len(data)), Mode: 0o644, ModTime: mod,
		Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
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

// counter counts the bytes written through it and reports them every megabyte.
type counter struct {
	w      io.Writer
	n      int64
	last   int64
	report func(int64)
}

func (c *counter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.report != nil && c.n-c.last >= 1<<20 {
		c.last = c.n
		c.report(c.n)
	}
	return n, err
}

// ---- reading ----

// ErrNotFootOps: a tar whose first member is none of a FootOps archive's — an archive of
// files.
var ErrNotFootOps = errors.New("FootOps arşivi değil")

// Contents is what reading a FootOps archive found, every member checked against the
// manifest.
type Contents struct {
	Manifest Manifest
	// Members are where each member lies in the file (to be read in place later), the
	// manifest's own included.
	Members map[string]MemberAt
	// Size and SHA256 are the whole file's.
	Size   int64
	SHA256 string
	// DB is the database member's mongodump prelude and DBSize its uncompressed size;
	// nil: the archive has no database.
	DB     *MongoPrelude
	DBSize int64
	// Files is the files member's summary; nil: the archive has no files.
	Files *Summary
	// Env tells the archive holds the site's environment (its secrets too), EnvVars how
	// many variables.
	Env     bool
	EnvVars int
}

// ReadFootOps reads a FootOps archive once, from its start to its end: every member's
// SHA-256 against the manifest, the database member's gzip and mongodump prelude, the
// files member like an upload (names, sizes, links, bombs: CheckFilesMember), the
// environment's syntax. A tar whose first member is none of ours is ErrNotFootOps.
func ReadFootOps(r io.Reader) (Contents, error) {
	var c Contents
	total := sha256.New()
	pos := &position{r: r}
	tr := tar.NewReader(io.TeeReader(pos, total))
	members := map[string]MemberAt{}
	var man *Manifest
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return c, fmt.Errorf("tar bozuk: %w", err)
		}
		name, err := CleanName(h.Name)
		if err != nil {
			return c, err
		}
		if h.Typeflag != tar.TypeReg {
			return c, fmt.Errorf("%s: FootOps arşivinde yalnız dosya olabilir", name)
		}
		for k := range h.PAXRecords {
			if strings.HasPrefix(k, "GNU.sparse") {
				return c, fmt.Errorf("%s: seyrek (sparse) dosya kabul edilmez", name)
			}
		}
		if _, dup := members[name]; dup {
			return c, fmt.Errorf("%s: arşivde iki kez var", name)
		}
		if len(members) == 0 && name != MemberManifest && name != MemberDB && name != MemberFiles {
			return c, ErrNotFootOps
		}
		// The tar reader has read exactly the header: the body starts here.
		m := MemberAt{Member: Member{Size: h.Size}, Offset: pos.n}
		mh := sha256.New()
		switch name {
		case MemberManifest:
			if h.Size > MaxManifest {
				return c, errors.New("manifest.json çok büyük")
			}
			b, err := io.ReadAll(io.TeeReader(tr, mh))
			if err != nil {
				return c, err
			}
			man = &Manifest{}
			if err := json.Unmarshal(b, man); err != nil {
				return c, fmt.Errorf("manifest.json okunamadı: %w", err)
			}
		case MemberDB:
			p, size, err := ReadMongo(tr, true, mh)
			if err != nil {
				return c, fmt.Errorf("%s: %w", MemberDB, err)
			}
			c.DB, c.DBSize = &p, size
		case MemberFiles:
			s, err := CheckFilesMember(io.TeeReader(tr, mh), h.Size)
			if err != nil {
				return c, fmt.Errorf("%s: %w", MemberFiles, err)
			}
			c.Files = &s
		case MemberEnv:
			// A backup's environment (İndir): checked and counted, never applied.
			if h.Size > MaxEnv {
				return c, errors.New("env çok büyük")
			}
			b, err := io.ReadAll(io.TeeReader(tr, mh))
			if err != nil {
				return c, err
			}
			vars, err := sites.ParseDotenv(string(b))
			if err != nil {
				return c, fmt.Errorf("env okunamadı: %w", err)
			}
			c.Env, c.EnvVars = true, len(vars)
		default:
			return c, fmt.Errorf("FootOps arşivinde beklenmeyen dosya: %s", name)
		}
		// What was checked must be exactly the bytes at the recorded place in the file, as
		// they will be read from there later.
		if pos.n-m.Offset != h.Size {
			return c, fmt.Errorf("%s: üye boyutu tutmuyor", name)
		}
		m.SHA256 = hex.EncodeToString(mh.Sum(nil))
		members[name] = m
	}
	if _, err := io.Copy(total, pos); err != nil { // the tar's own padding after its end marker
		return c, err
	}
	c.Size, c.SHA256 = pos.n, hex.EncodeToString(total.Sum(nil))
	if man == nil {
		return c, errors.New("manifest.json yok: bu bir FootOps arşivi değil. Klasör halindeki mongodump (tar/zip) okunamıyor; " +
			"mongodump --archive=dosya.gz --gzip ile tek dosya üretebilirsin")
	}
	if man.Format != FootOpsFormat {
		return c, fmt.Errorf("FootOps arşiv biçimi %d tanınmıyor (bu sürüm %d okur)", man.Format, FootOpsFormat)
	}
	for name, want := range man.Members {
		got, ok := members[name]
		if !ok {
			return c, fmt.Errorf("manifest'te olan %q arşivde yok", name)
		}
		if got.Size != want.Size || got.SHA256 != want.SHA256 {
			return c, fmt.Errorf("%s manifest'le tutmuyor (boyut ya da sha256): dosya bozulmuş", name)
		}
	}
	for name := range members {
		if _, ok := man.Members[name]; !ok && name != MemberManifest {
			return c, fmt.Errorf("%s manifest'te yok", name)
		}
	}
	c.Manifest, c.Members = *man, members
	if c.DB == nil && c.Files == nil {
		return c, errors.New("FootOps arşivinde ne veritabanı ne dosya var")
	}
	return c, nil
}

// ReadMongo reads a mongodump archive (gzip'd or not) to its end through h, returning its
// prelude and its uncompressed size. Reading to the end verifies the gzip checksum; a
// decompression bomb is refused like in Check.
func ReadMongo(r io.Reader, gz bool, h hash.Hash) (MongoPrelude, int64, error) {
	in := &position{r: r}
	br := bufio.NewReaderSize(io.TeeReader(in, h), 256<<10)
	var plain io.Reader = br
	if gz {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return MongoPrelude{}, 0, fmt.Errorf("gzip başlığı bozuk: %w", err)
		}
		plain = zr
	}
	l := DefaultLimits
	lim := &io.LimitedReader{R: plain, N: l.MaxTotal + 1}
	p, err := ReadMongoPrelude(lim)
	if err != nil {
		return p, 0, err
	}
	if _, err := io.Copy(io.Discard, lim); err != nil {
		return p, 0, fmt.Errorf("arşiv bozuk: %w", err)
	}
	size := l.MaxTotal + 1 - lim.N
	if lim.N <= 0 {
		return p, size, fmt.Errorf("arşivin açılmış hali %d GB sınırını aşıyor", l.MaxTotal>>30)
	}
	if _, err := io.Copy(io.Discard, br); err != nil { // anything after the gzip stream
		return p, size, err
	}
	if gz && size > ratioFloor && size > in.n*l.MaxRatio {
		return p, size, fmt.Errorf("sıkıştırma oranı şüpheli (%d:1, sınır %d:1) — sıkıştırma bombası olabilir", size/max(in.n, 1), l.MaxRatio)
	}
	return p, size, nil
}

// CheckFilesMember checks a FootOps archive's files.tar.gz member like Check checks a
// whole file: integrity, names, limits, compression ratio.
func CheckFilesMember(r io.Reader, size int64) (Summary, error) {
	s := Summary{Kind: TarGz, Compressed: size}
	zr, err := gzip.NewReader(bufio.NewReaderSize(r, 256<<10))
	if err != nil {
		return s, fmt.Errorf("gzip başlığı bozuk: %w", err)
	}
	l := DefaultLimits
	if err := CheckTar(zr, l, &s); err != nil {
		return s, err
	}
	// The tar's padding and the gzip checksum follow; nothing much else may.
	n, err := io.Copy(io.Discard, io.LimitReader(zr, 1<<20+1))
	if err != nil {
		return s, fmt.Errorf("gzip bozuk: %w", err)
	}
	if n > 1<<20 {
		return s, errors.New("tar'ın sonundan sonra beklenmeyen veri var")
	}
	return s, CheckRatio(s.Size, size, l)
}

// position counts what is read through it.
type position struct {
	r io.Reader
	n int64
}

func (p *position) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	return n, err
}
