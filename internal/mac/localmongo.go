package mac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// backupDBMark is in the name of every database a backup is loaded into by default; only
// those are listed and deleted (yedek acilanlar, yedek sil).
const backupDBMark = "_yedek_"

func defaultDBName(site string, at time.Time) string {
	return strings.ReplaceAll(site, "-", "_") + backupDBMark + at.Format("20060102_1504")
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

// loadDB loads a database's dump (mongodump --archive) into the Mac's MongoDB as to: its
// namespaces renamed from the site's database (named after the site), one of that name
// dropped first when it was there (the owner said so). Cut short or failed, what was
// loaded is dropped: a half database looks whole.
func (e *env) loadDB(ctx context.Context, t mongoTarget, archive, from, to string, replace bool, onBytes func(done, total int64)) (*LoadedDB, error) {
	tool, err := e.look("mongorestore")
	if err != nil {
		return nil, errors.New("mongorestore bulunamadı: brew install mongodb-database-tools")
	}
	if replace {
		if err := t.drop(ctx, to); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	args := []string{"--archive", "--nsInclude=" + from + ".*", "--nsFrom=" + from + ".*", "--nsTo=" + to + ".*", "--drop"}
	if t.cred.Password == "" {
		args = append(args, "--uri="+t.uri)
	} else {
		// A password is not put where ps shows it: a private file mongorestore reads
		// (--config), removed when it ends.
		dir, err := os.MkdirTemp(e.tmpDir(), "footops-mac-mongo-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		quoted, _ := json.Marshal(t.uri) // a JSON string is a YAML one
		conf := filepath.Join(dir, "mongorestore.yaml")
		if err := os.WriteFile(conf, []byte("uri: "+string(quoted)+"\n"), 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--config="+conf)
	}
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdin = &countingReader{r: f, total: st.Size(), on: onBytes}
	out := &tail{max: 16 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	// mongorestore names the URI in its errors, the password in it as written (encoded
	// too): whatever stands before its host goes.
	said := reURICreds.ReplaceAllString(out.String(), "${1}xxxxx@")
	if t.cred.Password != "" {
		said = strings.ReplaceAll(said, t.cred.Password, "xxxxx")
	}
	m := reRestoreTotal.FindStringSubmatch(said)
	if ctx.Err() != nil || err != nil || m == nil {
		// What got in goes: with a fresh clock, the command's own may be over.
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = t.drop(dctx, to)
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil, errStopped
		case err != nil:
			return nil, fmt.Errorf("mongorestore başarısız: %s", toolWords(said, err.Error()))
		}
		return nil, errors.New("mongorestore'un sonucu okunamadı")
	}
	docs, _ := strconv.ParseInt(m[1], 10, 64)
	failed, _ := strconv.ParseInt(m[2], 10, 64)
	return &LoadedDB{Name: to, Documents: docs, Failed: failed, Replaced: replace}, nil
}
