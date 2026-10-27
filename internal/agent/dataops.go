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
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/run"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
	"github.com/swbilisim/FootOps/internal/version"
)

const (
	DefaultSnapshotDir = "/var/lib/footops/snapshots"
	// SnapshotKeep: copies older than this are removed, except each site's newest
	// (rule K9).
	SnapshotKeep     = 7 * 24 * time.Hour
	snapshotFile     = "db.archive.gz"
	snapshotManifest = "manifest.json"
	snapshotFormat   = 1
	// toolPrefix names the one-off tool containers, so leftovers can be found.
	toolPrefix = "footops-job-"
)

// Snapshot is a copy of a site's database taken on this server.
type Snapshot struct {
	Format      int              `json:"format"`
	ID          string           `json:"id"` // YYYYMMDD-HHMMSS, UTC
	Site        string           `json:"site"`
	DB          string           `json:"db"`
	Created     time.Time        `json:"created"`
	Reason      string           `json:"reason"` // "manual", later "before-import", …
	Actor       string           `json:"actor,omitempty"`
	Bytes       int64            `json:"bytes"`
	SHA256      string           `json:"sha256"`
	Collections map[string]int64 `json:"collections"` // collection -> documents
	Documents   int64            `json:"documents"`
	DataSize    int64            `json:"data_size"` // uncompressed BSON, from dbStats
	Tool        string           `json:"tool"`      // image that wrote it
	FootOps     string           `json:"footops"`
	Usage       run.Usage        `json:"usage"`
}

// DataOps handles site data: database copies now; imports and exports later. It never
// holds a site's secrets longer than one operation, and its heavy work runs in
// resource-limited tool containers.
type DataOps struct {
	Store  sites.Store
	Docker interface {
		Inspect(context.Context, string) (docker.Inspect, error)
	}
	Run         run.Runner
	Stream      run.Streamer
	Staging     *staging.Area
	SnapshotDir string
	Limits      run.Limits

	now       func() time.Time
	checkFree func(path string, need int64) error
	initOnce  sync.Once
}

func (d *DataOps) init() {
	d.initOnce.Do(func() {
		if d.now == nil {
			d.now = time.Now
		}
		if d.checkFree == nil {
			d.checkFree = staging.CheckFree
		}
		if d.Limits == (run.Limits{}) {
			d.Limits = run.DefaultLimits
		}
	})
}

var reSnapshotID = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)

// dbCreds are a site's own database credentials (rule K4: data work uses the site's
// user, which can only reach the site's database).
type dbCreds struct {
	User, Password, DB string
}

// siteDB returns the site's spec and its database credentials, read from the
// connection string FootOps wrote into the site's environment.
func (d *DataOps) siteDB(name string) (sites.Spec, dbCreds, error) {
	spec, err := d.Store.Get(name)
	if err != nil {
		return spec, dbCreds{}, err
	}
	if !spec.Database {
		return spec, dbCreds{}, errors.New("bu sitenin FootOps veritabanı yok")
	}
	env, err := d.Store.Env(spec)
	if err != nil {
		return spec, dbCreds{}, fmt.Errorf("site ayarları okunamadı: %w", err)
	}
	u, err := url.Parse(env[spec.DBVar])
	if err != nil || u.Scheme != "mongodb" || u.Host != mongoAlias+":27017" || u.User == nil {
		return spec, dbCreds{}, fmt.Errorf("%s değişkeni FootOps'un yerel MongoDB adresi değil", spec.DBVar)
	}
	pw, _ := u.User.Password()
	c := dbCreds{User: u.User.Username(), Password: pw, DB: spec.DBName()}
	if c.User != spec.DBUser() || pw == "" || strings.Trim(u.Path, "/") != c.DB {
		return spec, dbCreds{}, fmt.Errorf("%s değişkenindeki kullanıcı/veritabanı beklenenden farklı", spec.DBVar)
	}
	return spec, c, nil
}

// dbStats are the parts of dbStats used to plan a copy.
type dbStats struct {
	DataSize    int64 `json:"dataSize"`
	Objects     int64 `json:"objects"`
	Collections int   `json:"collections"`
}

func (d *DataOps) stats(ctx context.Context, c dbCreds) (dbStats, error) {
	out, err := mongosh(ctx, d.Run, c.User, c.Password, c.DB,
		fmt.Sprintf(`const s=c.getDB(%s).stats();print("FOOTOPS_JSON "+JSON.stringify({dataSize:Number(s.dataSize),objects:Number(s.objects),collections:Number(s.collections)}))`, q(c.DB)))
	if err != nil {
		return dbStats{}, fmt.Errorf("veritabanı bilgisi alınamadı: %w", err)
	}
	var st dbStats
	_, js, ok := strings.Cut(out, "FOOTOPS_JSON ")
	if !ok || json.Unmarshal([]byte(strings.TrimSpace(firstLine(js))), &st) != nil {
		return dbStats{}, errors.New("veritabanı bilgisi anlaşılamadı")
	}
	return st, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Snapshot copies a site's database to the snapshot directory: mongodump in a limited
// tool container, streamed straight to disk, then checked (gzip checksum, archive
// type) before it counts. A failed or cancelled copy leaves nothing behind.
func (d *DataOps) Snapshot(ctx context.Context, t *Task, site, reason string, actor Actor) (snap Snapshot, err error) {
	d.init()
	spec, creds, err := d.siteDB(site)
	if err != nil {
		return snap, err
	}
	st, err := d.stats(ctx, creds)
	if err != nil {
		return snap, err
	}
	t.Step(fmt.Sprintf("✓ Veritabanı %s: %d koleksiyon, %d belge, %s veri", creds.DB, st.Collections, st.Objects, humanBytes(st.DataSize)))

	// The compressed copy is smaller than the data; the data size is a safe estimate.
	if err := os.MkdirAll(d.SnapshotDir, 0o700); err != nil {
		return snap, err
	}
	if err := d.checkFree(d.SnapshotDir, st.DataSize); err != nil {
		return snap, err
	}

	now := d.now().UTC()
	snap = Snapshot{Format: snapshotFormat, ID: now.Format("20060102-150405"), Site: spec.Name, DB: creds.DB,
		Created: now, Reason: reason, Actor: actor.User, DataSize: st.DataSize, Tool: core.MongoImage,
		FootOps: version.Version, Collections: map[string]int64{}}
	dir := filepath.Join(d.SnapshotDir, spec.Name, snap.ID)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return snap, err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return snap, errors.New("bu saniyede zaten bir kopya alındı; biraz sonra tekrar dene")
		}
		return snap, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()

	cfgDir, err := d.toolConfig(t.ID(), creds)
	if err != nil {
		return snap, err
	}
	defer func() { _ = d.Staging.Remove(t.ID()) }()

	f, err := os.OpenFile(filepath.Join(dir, snapshotFile+".part"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return snap, err
	}
	defer f.Close()
	h := sha256.New()
	cw := &countingWriter{w: io.MultiWriter(f, h), report: func(n int64) { t.SetBytes(n, 0) }}
	bw := bufio.NewWriterSize(cw, 256<<10)

	t.Step("… mongodump çalışıyor (sınır: " + limitText(d.Limits) + ")")
	// One collection at a time: slower on big databases, but memory stays low (K7).
	cmd := d.toolCmd(ctx, t.ID(), cfgDir, "mongodump", "--config=/run/footops/mongo.yaml",
		"--db="+creds.DB, "--archive", "--gzip", "--numParallelCollections=1")
	cmd.Stdout = bw
	cmd.Stderr = func(line string) {
		if m := reDumped.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(m[2], 10, 64)
			snap.Collections[strings.TrimPrefix(m[1], creds.DB+".")] = n
		}
	}
	u, err := d.Stream.Stream(ctx, cmd)
	t.AddUsage(u)
	snap.Usage = u
	if err == nil {
		err = ctx.Err() // cancelled just as the tool finished: still cancelled
	}
	if err != nil {
		return snap, fmt.Errorf("mongodump başarısız: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return snap, err
	}
	if err := f.Sync(); err != nil {
		return snap, err
	}
	if err := f.Close(); err != nil {
		return snap, err
	}
	snap.Bytes, snap.SHA256 = cw.n, hex.EncodeToString(h.Sum(nil))
	t.SetBytes(snap.Bytes, 0)
	for _, n := range snap.Collections {
		snap.Documents += n
	}
	used := "kaynak kullanımı ölçülemedi (çok kısa sürdü)"
	if u.PeakMem > 0 {
		used = fmt.Sprintf("en çok %s bellek, %s CPU", humanBytes(int64(u.PeakMem)), took(u.CPU))
	}
	t.Step(fmt.Sprintf("✓ mongodump bitti: %d koleksiyon, %d belge → %s (%s, %s)",
		len(snap.Collections), snap.Documents, humanBytes(snap.Bytes), took(u.Wall), used))

	// Rule K2 for our own output: the copy is only kept if it reads back intact.
	part := filepath.Join(dir, snapshotFile+".part")
	kind, err := archive.Detect(part)
	if err != nil || kind != archive.MongoArchiveGz {
		return snap, fmt.Errorf("kopya beklenen biçimde değil (%s): %v", kind, err)
	}
	if err := verifyGzip(ctx, part); err != nil {
		return snap, err
	}
	if snap.Documents != st.Objects {
		t.Step(fmt.Sprintf("ℹ Belge sayısı başta %d idi, kopyada %d (site bu sırada yazmış olabilir)", st.Objects, snap.Documents))
	}
	t.Step("✓ Kopya doğrulandı (gzip bütünlüğü, mongodump arşivi)")

	if err := ctx.Err(); err != nil {
		return snap, err
	}
	if err := os.Rename(part, filepath.Join(dir, snapshotFile)); err != nil {
		return snap, err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return snap, err
	}
	// The manifest is what makes a copy count, so it reaches the disk before the job
	// reports success (a power cut must not leave an empty one).
	if err := writeSynced(filepath.Join(dir, snapshotManifest), append(b, '\n')); err != nil {
		return snap, err
	}
	t.SetResult(snap.ID)
	t.Step(fmt.Sprintf("✓ Kopya hazır: %s (sunucuda %d gün saklanır)", snap.ID, int(SnapshotKeep.Hours()/24)))
	return snap, nil
}

// writeSynced writes a file atomically and durably: data, then the rename, are synced.
func writeSynced(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// "2026-10-27T11:00:00.000+0000\tdone dumping `swbilisim.users` (12 documents)"; older
// tools print the namespace without backticks.
var reDumped = regexp.MustCompile("done dumping `?([^`\\s]+)`? \\((\\d+) documents?\\)")

// verifyGzip reads our own copy back to its checksum. It has no size limit (that is for
// untrusted uploads) but stops when ctx ends.
func verifyGzip(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := archive.GzipSize(bufio.NewReader(ctxReader{ctx, f}), math.MaxInt64-1); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("kopya doğrulanamadı: %w", err)
	}
	return nil
}

// ctxReader stops reading once ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// toolConfig writes the tool's connection settings to a root-only file in the job's
// staging directory (rule K5): the password never appears in a process list, a log or
// the container's configuration. The tool container shares MongoDB's network
// namespace, hence 127.0.0.1.
func (d *DataOps) toolConfig(jobID string, c dbCreds) (string, error) {
	dir, err := d.Staging.Dir(jobID)
	if err != nil {
		return "", err
	}
	uri := (&url.URL{Scheme: "mongodb", User: url.UserPassword(c.User, c.Password), Host: "127.0.0.1:27017", Path: "/",
		RawQuery: url.Values{"authSource": {c.DB}}.Encode()}).String()
	if err := os.WriteFile(filepath.Join(dir, "mongo.yaml"), []byte("uri: "+q(uri)+"\n"), 0o600); err != nil {
		return "", err
	}
	return dir, nil
}

// toolCmd runs a MongoDB tool in a one-off container: the server's own MongoDB image
// (same tool version as the server), MongoDB's network namespace only, no Linux
// capabilities, read-only root, memory/CPU/PID limits, lowest I/O and CPU priority.
// Its output is not logged by Docker (--log-driver none): it is the data itself.
func (d *DataOps) toolCmd(ctx context.Context, jobID, cfgDir string, args ...string) run.Cmd {
	name := toolPrefix + jobID
	full := []string{"run", "--rm", "--name", name, "--label", "footops.job=" + jobID,
		"--network", "container:" + mongoContainer, "--log-driver", "none",
		"--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	full = append(full, d.Limits.DockerFlags()...)
	full = append(full, "--volume", cfgDir+":/run/footops:ro", "--entrypoint", "ionice", core.MongoImage,
		"-c", "3", "nice", "-n", "10")
	full = append(full, args...)
	var cg string
	return run.Cmd{
		Name: "docker", Args: full,
		Stop: func() {
			stop, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = d.Run.Run(stop, "docker", "kill", name)
		},
		Cgroup: func() string {
			if cg == "" {
				if in, err := d.Docker.Inspect(ctx, name); err == nil && in.State.Running {
					cg = run.CgroupOfPID(in.State.Pid)
				}
			}
			return cg
		},
	}
}

func limitText(l run.Limits) string {
	return fmt.Sprintf("%d MB bellek, %%%d CPU", l.MemoryMB, l.CPUPercent)
}

// countingWriter counts bytes and reports them every megabyte.
type countingWriter struct {
	w      io.Writer
	n      int64
	last   int64
	report func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.report != nil && c.n-c.last >= 1<<20 {
		c.last = c.n
		c.report(c.n)
	}
	return n, err
}

// Snapshots lists a site's copies, newest first. Copies without a readable manifest
// (an interrupted one) are left out.
func (d *DataOps) Snapshots(site string) ([]Snapshot, error) {
	if !docker.ValidName(site) {
		return nil, errors.New("geçersiz site")
	}
	ents, err := os.ReadDir(filepath.Join(d.SnapshotDir, site))
	if errors.Is(err, fs.ErrNotExist) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, e := range ents {
		if !e.IsDir() || !reSnapshotID.MatchString(e.Name()) {
			continue
		}
		s, err := d.readSnapshot(site, e.Name())
		if err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out, nil
}

func (d *DataOps) readSnapshot(site, id string) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(filepath.Join(d.SnapshotDir, site, id, snapshotManifest))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil || s.ID != id || s.Site != site {
		return s, errors.New("kopya bilgisi bozuk")
	}
	return s, nil
}

// SnapshotFile returns the path of a copy's archive after checking that it exists.
func (d *DataOps) SnapshotFile(site, id string) (Snapshot, string, error) {
	if !docker.ValidName(site) || !reSnapshotID.MatchString(id) {
		return Snapshot{}, "", errors.New("geçersiz kopya")
	}
	s, err := d.readSnapshot(site, id)
	if err != nil {
		return s, "", fmt.Errorf("kopya bulunamadı: %w", err)
	}
	return s, filepath.Join(d.SnapshotDir, site, id, snapshotFile), nil
}

// SweepSnapshots removes copies older than SnapshotKeep, keeping each site's newest,
// and half-written ones from interrupted jobs. Those are only removed once surely not
// being written, unless idle is set (no job can be running, as at agent start). It
// returns what it removed.
func (d *DataOps) SweepSnapshots(idle bool) []string {
	d.init()
	sitesDirs, err := os.ReadDir(d.SnapshotDir)
	if err != nil {
		return nil
	}
	var removed []string
	cutoff := d.now().UTC().Add(-SnapshotKeep).Format("20060102-150405")
	for _, sd := range sitesDirs {
		if !sd.IsDir() || !docker.ValidName(sd.Name()) {
			continue
		}
		ents, err := os.ReadDir(filepath.Join(d.SnapshotDir, sd.Name()))
		if err != nil {
			continue
		}
		var ids []string
		for _, e := range ents {
			if e.IsDir() && reSnapshotID.MatchString(e.Name()) {
				ids = append(ids, e.Name())
			}
		}
		sort.Strings(ids)
		complete := map[string]bool{}
		newest := ""
		for _, id := range ids {
			if _, err := d.readSnapshot(sd.Name(), id); err == nil {
				complete[id], newest = true, id
			}
		}
		for _, id := range ids {
			// A half-written copy (no archive yet) is removed once surely not being written.
			// One whose archive is there but whose manifest is unreadable (a crash at the
			// very end) is not listed, but is kept for its normal time: the data is fine.
			stale := idle || id < d.now().UTC().Add(-jobTimeouts["db.snapshot"]).Format("20060102-150405")
			_, aerr := os.Stat(filepath.Join(d.SnapshotDir, sd.Name(), id, snapshotFile))
			partial := !complete[id] && aerr != nil
			if (!partial && id != newest && id < cutoff) || (partial && stale) {
				if os.RemoveAll(filepath.Join(d.SnapshotDir, sd.Name(), id)) == nil {
					removed = append(removed, sd.Name()+"/"+id)
				}
			}
		}
	}
	return removed
}

// took formats a measured duration in Turkish units: "420 ms", "1.8 sn", "3 dk 5 sn".
func took(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%d ms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1f sn", d.Seconds())
	}
	return fmt.Sprintf("%d dk %d sn", int(d.Minutes()), int(d.Seconds())%60)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
