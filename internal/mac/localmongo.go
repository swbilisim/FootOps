package mac

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/mongo"
)

// The Mac's own MongoDB, where a backup's database is loaded to be looked at (MONGO_URI,
// default mongodb://127.0.0.1:27017): the panel's own client (internal/mongo) for the
// little asked of it here — the databases there are, dropping one — and mongorestore
// for the loading.

const (
	defaultMongoURI = "mongodb://127.0.0.1:27017"
	// mongoDialTimeout: the Mac's MongoDB answers at once, or is not running.
	mongoDialTimeout = 5 * time.Second
)

var (
	// reDBName is a database name footops-mac makes or takes.
	reDBName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	// "N document(s) restored successfully. M document(s) failed to restore." (the
	// agent's reRestoreTotal).
	reRestoreTotal = regexp.MustCompile(`(\d+) document\(s\) restored successfully\. (\d+) document\(s\) failed to restore`)
	// reURICreds is a MongoDB URI's user and password.
	reURICreds = regexp.MustCompile(`(mongodb(?:\+srv)?://)[^@/\s]+@`)
)

// backupDBMark and archiveDBMark are in the name of every database a backup or an archive
// is loaded into by default (<site>_yedek_<date>_<time>): one footops-mac loads under a
// name with a mark goes on its list of what was opened.
const (
	backupDBMark  = "_yedek_"
	archiveDBMark = "_arsiv_"
)

// reDefaultLoad is a default name in full: a database so named is footops-mac's even when
// its line on the list is gone, and only such ones are listed and deleted without one —
// "arşiv" and "yedek" are everyday words, siparis_arsiv_2024 the owner's own. (The bash
// helper footops-mac replaced named them by the day alone.)
var reDefaultLoad = regexp.MustCompile(`^[A-Za-z0-9_-]+_(yedek|arsiv)_[0-9]{8}(_[0-9]{4})?$`)

func defaultDBName(site string, at time.Time) string {
	return strings.ReplaceAll(site, "-", "_") + backupDBMark + at.Format("20060102_1504")
}

// markedDBName tells a name footops-mac puts on its list when it loads a database under it.
func markedDBName(name string) bool {
	return strings.Contains(name, backupDBMark) || strings.Contains(name, archiveDBMark)
}

func checkDBName(name string) error {
	switch {
	case !reDBName.MatchString(name):
		return fmt.Errorf("geçersiz veritabanı adı: %q (harf, rakam, _ ve -; en çok 63)", clip(name, 80))
	case name == "admin" || name == "local" || name == "config":
		return fmt.Errorf("%s MongoDB'nin kendi veritabanı", name)
	}
	return nil
}

// mongoTarget is the Mac's MongoDB as MONGO_URI names it: one server, a login if the URI
// has one.
type mongoTarget struct {
	uri  string
	addr string
	cred mongo.Credential
}

func parseMongoURI(s string) (mongoTarget, error) {
	bad := fmt.Errorf("MONGO_URI anlaşılamadı: tek sunuculu mongodb://[kullanıcı:şifre@]sunucu[:port] olmalı (%s)", redactURI(s))
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "mongodb" || u.Host == "" || strings.Contains(u.Host, ",") || u.Hostname() == "" {
		return mongoTarget{}, bad
	}
	port := u.Port()
	if port == "" {
		port = "27017"
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return mongoTarget{}, bad
	}
	t := mongoTarget{uri: s, addr: net.JoinHostPort(u.Hostname(), port)}
	if u.User != nil && u.User.Username() != "" {
		pw, _ := u.User.Password()
		src := u.Query().Get("authSource")
		if src == "" {
			src = strings.TrimPrefix(u.Path, "/")
		}
		if src == "" {
			src = "admin"
		}
		t.cred = mongo.Credential{User: u.User.Username(), Password: pw, Source: src}
	}
	return t, nil
}

// label is the URI without its password, to be shown.
func (t mongoTarget) label() string { return redactURI(t.uri) }

// toolURI is the URI the tools are given: its database (the login's, when the URI
// names one) moved into authSource — with a database in it, mongodump's --db and
// mongorestore's namespaces would be read against it.
func (t mongoTarget) toolURI() string {
	u, err := url.Parse(t.uri)
	if err != nil || strings.Trim(u.Path, "/") == "" {
		return t.uri
	}
	q := u.Query()
	if q.Get("authSource") == "" && u.User != nil {
		q.Set("authSource", strings.Trim(u.Path, "/"))
	}
	u.Path, u.RawPath, u.RawQuery = "", "", q.Encode()
	return u.String()
}

// toolArgs gives a tool the Mac's MongoDB: on its command line when the URI has no
// password; else in a private file the tool reads (--config) — a password is not put
// where ps shows it. done removes the file.
func (e *env) toolArgs(t mongoTarget) (args []string, done func(), err error) {
	if t.cred.Password == "" {
		return []string{"--uri=" + t.toolURI()}, func() {}, nil
	}
	dir, err := os.MkdirTemp(e.tmpDir(), "footops-mac-mongo-")
	if err != nil {
		return nil, nil, err
	}
	quoted, _ := json.Marshal(t.toolURI()) // a JSON string is a YAML one
	conf := filepath.Join(dir, "mongo.yaml")
	if err := os.WriteFile(conf, []byte("uri: "+string(quoted)+"\n"), 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}
	return []string{"--config=" + conf}, func() { os.RemoveAll(dir) }, nil
}

// masked is what a tool said, the Mac's MongoDB's password out of it: the tools name
// the URI in their errors as written (encoded too), so whatever stands before its host
// goes.
func (t mongoTarget) masked(said string) string {
	said = reURICreds.ReplaceAllString(said, "${1}xxxxx@")
	if t.cred.Password != "" {
		said = strings.ReplaceAll(said, t.cred.Password, "xxxxx")
	}
	return said
}

func redactURI(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		if i := strings.LastIndex(s, "@"); i >= 0 {
			return "mongodb://…" + s[i:] // not understood: nothing before the host shown
		}
		return s
	}
	if _, has := u.User.Password(); has {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

func (t mongoTarget) dial(ctx context.Context) (*mongo.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, mongoDialTimeout)
	defer cancel()
	var c *mongo.Conn
	var err error
	if t.cred.User == "" {
		c, err = mongo.DialNoLogin(ctx, t.addr)
	} else {
		c, err = mongo.Dial(ctx, t.addr, t.cred)
	}
	if err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errStopped
		}
		return nil, fmt.Errorf("Mac'teki MongoDB'ye bağlanılamadı (%s): çalışıyor mu? brew services start mongodb-community@7.0 — %v", t.label(), err)
	}
	return c, nil
}

// databases are the Mac's MongoDB's databases and their size on disk.
func (t mongoTarget) databases(ctx context.Context) (map[string]int64, error) {
	c, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	reply, err := c.Run(rctx, "admin", mongo.NewBuilder().Int32("listDatabases", 1).MustBuild(), 4<<20)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errStopped
		}
		return nil, fmt.Errorf("Mac'teki MongoDB'nin veritabanları okunamadı: %v", err)
	}
	v, ok := reply.Lookup("databases")
	arr, isDoc := v.Doc()
	if !ok || !isDoc || v.Type != mongo.TypeArray {
		return nil, errors.New("MongoDB cevabında databases yok")
	}
	elems, err := arr.Elems()
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, el := range elems {
		d, ok := el.Doc()
		if !ok {
			continue
		}
		nv, ok := d.Lookup("name")
		name, isStr := nv.Str()
		if !ok || !isStr {
			continue
		}
		var size int64
		if sv, ok := d.Lookup("sizeOnDisk"); ok {
			size, _ = sv.Int()
		}
		out[name] = size
	}
	return out, nil
}

// drop drops a database (none of that name: nothing happens).
func (t mongoTarget) drop(ctx context.Context, name string) error {
	c, err := t.dial(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if _, err := c.Run(rctx, name, mongo.NewBuilder().Int32("dropDatabase", 1).MustBuild(), 64<<10); err != nil {
		return fmt.Errorf("%s silinemedi: %v", name, err)
	}
	return nil
}

// dumpSource is a mongodump archive to load: a file, or a part of one (a FootOps archive's
// database member), gzip'd or not; sha256, if set, is its checksum as checked: the bytes
// loaded must still be the same.
type dumpSource struct {
	path string
	// offset and length: where it lies in the file (length 0: the whole file).
	offset, length int64
	gzip           bool
	sha256         string
	// own: a dump of the Mac's own database (a pull's copy, put back): its system
	// collections (system.js) are its own data and come along; MongoDB's profile does not.
	own bool
}

// loadDB loads a database's dump (mongodump --archive) into the Mac's MongoDB as to: its
// namespaces renamed from the dump's database from, its system collections left out as
// the panel leaves them out (rule K4), one of that name dropped first when it was there
// (the owner said so). Cut short or failed, what was loaded is dropped: a half database
// looks whole.
func (e *env) loadDB(ctx context.Context, t mongoTarget, src dumpSource, from, to string, replace bool, onBytes func(done, total int64)) (*LoadedDB, error) {
	tool, err := e.look("mongorestore")
	if err != nil {
		return nil, errors.New("mongorestore bulunamadı: brew install mongodb-database-tools")
	}
	if replace {
		if err := t.drop(ctx, to); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(src.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var in io.Reader = f
	total := src.length
	if src.length > 0 {
		in = io.NewSectionReader(f, src.offset, src.length)
	} else {
		st, err := f.Stat()
		if err != nil {
			return nil, err
		}
		total = st.Size()
	}
	h := sha256.New()
	in = io.TeeReader(in, h)
	conn, done, err := e.toolArgs(t)
	if err != nil {
		return nil, err
	}
	defer done()
	exclude := "--nsExclude=" + from + ".system.*"
	if src.own {
		exclude = "--nsExclude=" + from + ".system.profile"
	}
	args := append(conn, "--archive", "--nsInclude="+from+".*", exclude, "--nsFrom="+from+".*", "--nsTo="+to+".*", "--drop")
	if src.gzip {
		args = append(args, "--gzip")
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	// A process group of its own: the Terminal's ^C reaches footops-mac only, which stops
	// the tool itself — or, while a database is being replaced, lets it finish.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = &countingReader{r: in, total: total, on: onBytes}
	out := &tail{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	said := t.masked(out.String())
	m := reRestoreTotal.FindStringSubmatch(said)
	changed := false
	if err == nil && m != nil && src.sha256 != "" {
		// What mongorestore did not read of it is read too: all of it is what was checked.
		_, rerr := io.Copy(io.Discard, in)
		changed = rerr != nil || hex.EncodeToString(h.Sum(nil)) != src.sha256
	}
	if ctx.Err() != nil || err != nil || m == nil || changed {
		// What got in goes: with a fresh clock, the command's own may be over.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = t.drop(dctx, to)
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil, errStopped
		case err != nil:
			return nil, fmt.Errorf("mongorestore başarısız: %s", toolWords(said, err.Error()))
		case changed:
			return nil, errArchiveChanged
		}
		return nil, errors.New("mongorestore'un sonucu okunamadı")
	}
	docs, _ := strconv.ParseInt(m[1], 10, 64)
	failed, _ := strconv.ParseInt(m[2], 10, 64)
	return &LoadedDB{Name: to, Documents: docs, Failed: failed, Replaced: replace}, nil
}
