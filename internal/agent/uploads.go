package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/swbilisim/FootOps/internal/archive"
	"github.com/swbilisim/FootOps/internal/docker"
)

// Uploads arrive in chunks: Cloudflare's free plan cuts a request at 100 MB and 100
// seconds, and a home connection uploads ~0.2 MB/s (measured, 2026-10-27), so 8 MB
// chunks take ~40 s each. Each chunk carries its SHA-256 and is written straight to its
// place in the file (no copy in memory, no second file); a broken upload resumes with
// the chunks it lacks. An upload lives in the staging area (root-only) and is removed
// with it after a day untouched.
const (
	UploadChunk     = 8 << 20
	MaxUploadChunk  = 32 << 20
	MaxUploadSize   = 2 << 30  // one file (operator decision, 2026-10-27)
	MaxSingleUpload = 90 << 20 // one request without JavaScript, under Cloudflare's 100 MB
	uploadPrefix    = "upl-"
	uploadData      = "data"
	uploadMeta      = "meta.json"
)

// Upload is a file being received for a site.
type Upload struct {
	ID       string       `json:"id"`
	Site     string       `json:"site"`
	Name     string       `json:"name"` // as the browser gave it; shown, never used as a path
	Size     int64        `json:"size"`
	Chunk    int64        `json:"chunk"`
	Got      []bool       `json:"got"`
	Created  time.Time    `json:"created"`
	Actor    Actor        `json:"actor"`
	Finished bool         `json:"finished"`
	Kind     archive.Kind `json:"kind,omitempty"` // recognised from the content when finished
}

// Chunks is the number of chunks.
func (u Upload) Chunks() int { return int((u.Size + u.Chunk - 1) / u.Chunk) }

// Missing lists the chunks not received yet.
func (u Upload) Missing() []int {
	out := []int{}
	for i, ok := range u.Got {
		if !ok {
			out = append(out, i)
		}
	}
	return out
}

// UploadStatus is what the panel needs to continue an upload.
type UploadStatus struct {
	Upload
	Missing []int `json:"missing"`
}

func (u Upload) status() UploadStatus { return UploadStatus{Upload: u, Missing: u.Missing()} }

var (
	reUploadID     = regexp.MustCompile(`^[A-Za-z0-9]{24}$`)
	reSHA256       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ErrNoUpload    = errors.New("yükleme bulunamadı (24 saatten eski olabilir)")
	errBadChunk    = errors.New("parça bozuk geldi (sha256 tutmuyor); tekrar gönderilecek")
	errUploadState = errors.New("yükleme tamamlanmış; değiştirilemez")
)

// uploadName keeps a display name: the last path element, printable characters only.
func uploadName(s string) string {
	s = path.Base(strings.ReplaceAll(s, `\`, "/"))
	if s == "." || s == "/" {
		return "dosya"
	}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsPrint(r) && b.Len() < 120 {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "dosya"
	}
	return b.String()
}

func (d *DataOps) uploadDir(id string) string {
	return filepath.Join(d.Staging.Root, uploadPrefix+id)
}

// uploadLock serialises the work on one upload (two requests for the same chunk, a
// chunk racing the finish). Entries live only while someone holds or waits for them.
type uploadLock struct {
	sync.Mutex
	users int
}

func (d *DataOps) lockUpload(id string) func() {
	d.init()
	d.mu.Lock()
	l := d.uploadLocks[id]
	if l == nil {
		l = &uploadLock{}
		d.uploadLocks[id] = l
	}
	l.users++
	d.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		d.mu.Lock()
		if l.users--; l.users == 0 {
			delete(d.uploadLocks, id)
		}
		d.mu.Unlock()
	}
}

// CreateUpload reserves room for a file of size bytes for a site.
func (d *DataOps) CreateUpload(site, name string, size int64, actor Actor) (UploadStatus, error) {
	d.init()
	if _, err := d.Store.Get(site); err != nil {
		return UploadStatus{}, err
	}
	if size <= 0 || size > MaxUploadSize {
		return UploadStatus{}, fmt.Errorf("dosya boyutu 1 bayt ile %s arasında olmalı", humanBytes(MaxUploadSize))
	}
	if err := d.Staging.Allow(size); err != nil {
		return UploadStatus{}, err
	}
	id, err := randomSecret(24)
	if err != nil {
		return UploadStatus{}, err
	}
	u := Upload{ID: id, Site: site, Name: uploadName(name), Size: size, Chunk: d.chunk,
		Created: d.now().UTC(), Actor: actor}
	u.Got = make([]bool, u.Chunks())
	dir, err := d.Staging.Dir(uploadPrefix + id)
	if err != nil {
		return UploadStatus{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, uploadData), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return UploadStatus{}, err
	}
	// Full length at once (sparse): the staging quota counts the whole file from now on.
	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = d.saveUpload(u)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return UploadStatus{}, err
	}
	return u.status(), nil
}

func (d *DataOps) saveUpload(u Upload) error {
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return writeSynced(filepath.Join(d.uploadDir(u.ID), uploadMeta), b)
}

func (d *DataOps) loadUpload(id string) (Upload, error) {
	var u Upload
	if !reUploadID.MatchString(id) {
		return u, ErrNoUpload
	}
	b, err := os.ReadFile(filepath.Join(d.uploadDir(id), uploadMeta))
	if errors.Is(err, fs.ErrNotExist) {
		return u, ErrNoUpload
	}
	if err != nil {
		return u, err
	}
	if err := json.Unmarshal(b, &u); err != nil || u.ID != id || !docker.ValidName(u.Site) ||
		u.Chunk <= 0 || u.Chunk > MaxUploadChunk || u.Size <= 0 || u.Size > MaxUploadSize || len(u.Got) != u.Chunks() {
		return Upload{}, errors.New("yükleme bilgisi bozuk")
	}
	return u, nil
}

// GetUpload returns an upload's state.
func (d *DataOps) GetUpload(id string) (UploadStatus, error) {
	u, err := d.loadUpload(id)
	return u.status(), err
}

// PutChunk writes chunk n from body, checking it against sum (hex SHA-256). A chunk
// that does not match is marked missing, so the client sends it again.
func (d *DataOps) PutChunk(id string, n int, sum string, body io.Reader) (UploadStatus, error) {
	if !reUploadID.MatchString(id) {
		return UploadStatus{}, ErrNoUpload
	}
	defer d.lockUpload(id)()
	u, err := d.loadUpload(id)
	if err != nil {
		return UploadStatus{}, err
	}
	if u.Finished {
		return u.status(), errUploadState
	}
	if n < 0 || n >= u.Chunks() {
		return u.status(), fmt.Errorf("parça numarası 0-%d arasında olmalı", u.Chunks()-1)
	}
	if !reSHA256.MatchString(sum) {
		return u.status(), errors.New("parçanın sha256'sı eksik")
	}
	off := int64(n) * u.Chunk
	want := min(u.Chunk, u.Size-off)
	dir := d.uploadDir(id)
	if err := d.checkFree(dir, want); err != nil {
		return u.status(), err
	}
	// A chunk sent again is missing until it has arrived intact: a failed or interrupted
	// write must not leave the old mark on new bytes.
	if u.Got[n] {
		u.Got[n] = false
		if err := d.saveUpload(u); err != nil {
			return u.status(), err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, uploadData), os.O_WRONLY, 0)
	if err != nil {
		return u.status(), err
	}
	defer f.Close()
	h := sha256.New()
	got, err := io.Copy(io.MultiWriter(io.NewOffsetWriter(f, off), h), io.LimitReader(body, want))
	if err != nil {
		return u.status(), fmt.Errorf("parça okunamadı: %w", err)
	}
	var extra [1]byte
	if k, _ := io.ReadFull(body, extra[:]); got != want || k > 0 {
		return u.status(), fmt.Errorf("parça %d bayt olmalı", want)
	}
	if err := f.Sync(); err != nil {
		return u.status(), err
	}
	if hex.EncodeToString(h.Sum(nil)) != sum {
		return u.status(), errBadChunk
	}
	u.Got[n] = true
	return u.status(), d.saveUpload(u)
}

// FinishUpload closes an upload once every chunk is in; from then on it cannot change.
func (d *DataOps) FinishUpload(id string) (UploadStatus, error) {
	if !reUploadID.MatchString(id) {
		return UploadStatus{}, ErrNoUpload
	}
	defer d.lockUpload(id)()
	u, err := d.loadUpload(id)
	if err != nil || u.Finished {
		return u.status(), err
	}
	if m := u.Missing(); len(m) > 0 {
		return u.status(), fmt.Errorf("%d parça eksik", len(m))
	}
	u.Kind, _ = archive.Detect(filepath.Join(d.uploadDir(id), uploadData))
	u.Finished = true
	return u.status(), d.saveUpload(u)
}

// SingleUpload receives a whole file in one request (the panel without JavaScript), up
// to MaxSingleUpload bytes.
func (d *DataOps) SingleUpload(site, name string, body io.Reader, actor Actor) (UploadStatus, error) {
	d.init()
	if _, err := d.Store.Get(site); err != nil {
		return UploadStatus{}, err
	}
	if err := d.Staging.Allow(MaxSingleUpload); err != nil {
		return UploadStatus{}, err
	}
	id, err := randomSecret(24)
	if err != nil {
		return UploadStatus{}, err
	}
	dir, err := d.Staging.Dir(uploadPrefix + id)
	if err != nil {
		return UploadStatus{}, err
	}
	u, err := func() (Upload, error) {
		f, err := os.OpenFile(filepath.Join(dir, uploadData), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return Upload{}, err
		}
		defer f.Close()
		n, err := io.Copy(f, io.LimitReader(body, MaxSingleUpload+1))
		switch {
		case err != nil:
			return Upload{}, fmt.Errorf("dosya okunamadı: %w", err)
		case n == 0:
			return Upload{}, errors.New("dosya boş")
		case n > MaxSingleUpload:
			return Upload{}, fmt.Errorf("tek seferde en çok %s yüklenebilir; daha büyük dosyalar için sayfayı JavaScript açık bir tarayıcıda kullan", humanBytes(MaxSingleUpload))
		}
		if err := f.Sync(); err != nil {
			return Upload{}, err
		}
		u := Upload{ID: id, Site: site, Name: uploadName(name), Size: n, Chunk: MaxUploadChunk,
			Created: d.now().UTC(), Actor: actor, Finished: true}
		for i := 0; i < u.Chunks(); i++ {
			u.Got = append(u.Got, true)
		}
		u.Kind, _ = archive.Detect(filepath.Join(dir, uploadData))
		return u, d.saveUpload(u)
	}()
	if err != nil {
		_ = os.RemoveAll(dir)
		return UploadStatus{}, err
	}
	return u.status(), nil
}

// RemoveUpload deletes an upload.
func (d *DataOps) RemoveUpload(id string) error {
	if !reUploadID.MatchString(id) {
		return ErrNoUpload
	}
	defer d.lockUpload(id)()
	if _, err := d.loadUpload(id); err != nil {
		return err
	}
	return d.Staging.Remove(uploadPrefix + id)
}

// uploadFile returns a finished upload of site and the path of its data.
func (d *DataOps) uploadFile(site, id string) (Upload, string, error) {
	u, err := d.loadUpload(id)
	if err != nil {
		return u, "", err
	}
	if u.Site != site {
		return u, "", ErrNoUpload
	}
	if !u.Finished {
		return u, "", errors.New("yükleme henüz bitmedi")
	}
	p := filepath.Join(d.uploadDir(id), uploadData)
	st, err := os.Stat(p)
	if err != nil || st.Size() != u.Size {
		return u, "", errors.New("yüklenen dosya kayıp ya da boyutu değişmiş")
	}
	return u, p, nil
}
