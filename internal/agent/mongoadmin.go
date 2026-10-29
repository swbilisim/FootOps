package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/mongo"
)

// ---- MongoDB outside the database browser ----
//
// Users and databases of sites and of import checks, and the counts copies and imports
// are verified with. These ran as mongosh scripts, each a ~266 MB process for half a
// second (panel-tasarim.md §24.8.1); the agent now sends the commands itself (package
// mongo), as the browser does: BSON, no generated JavaScript, no password in any
// process's input. Root only manages users and databases (rule K4); everything inside a
// site's database runs as the site's own user.

const (
	// mongoAdminTimeout bounds managing a user or a temporary database. Work inside a
	// site's database (counting a large one can take long) runs on the job's deadline,
	// or mongoDataTimeout without one (package mongo's default is 30 s a command).
	mongoAdminTimeout = 2 * time.Minute
	mongoDataTimeout  = time.Hour
	mongoAdminReply   = 1 << 20
	mongoListReply    = 4 << 20
)

// mongoAdmin is that work; tests use a fake.
type mongoAdmin interface {
	// Ping logs in as root and pings: MongoDB answers and the root password works.
	Ping(ctx context.Context) error
	// SetUser creates user in db with readWrite on db alone, or gives an existing one
	// this password and only that role.
	SetUser(ctx context.Context, db, user, pw string) error
	// DropUser removes a user; one that is not there is no error.
	DropUser(ctx context.Context, db, user string) error
	// DropTemp removes a temporary database and its users.
	DropTemp(ctx context.Context, db string) error
	// DropTemps removes every temporary database and user left behind and returns how
	// many it removed (databases and users).
	DropTemps(ctx context.Context) (int, error)
	// Stats reads the parts of dbStats a copy is planned with.
	Stats(ctx context.Context, c dbCreds) (dbStats, error)
	// Counts lists the collections with their document counts, and the views. System
	// collections are left out (the site's user may not even read them).
	Counts(ctx context.Context, c dbCreds) (map[string]int64, []string, error)
	// DropCollections drops collections (or views); one already gone is no error.
	DropCollections(ctx context.Context, c dbCreds, names []string) error
}

// mongoClient is the real mongoAdmin.
type mongoClient struct {
	Addr       string // the server's MongoDB
	SecretFile string // env file holding the root password
}

// asRoot runs fn on a connection logged in as root, closed afterwards.
func (m mongoClient) asRoot(ctx context.Context, fn func(context.Context, *mongo.Conn) error) error {
	env, err := os.ReadFile(m.SecretFile)
	if err != nil {
		return err
	}
	pw := core.MongoPasswordFromEnv(string(env))
	if pw == "" {
		return errors.New("MongoDB root şifresi bulunamadı")
	}
	return m.as(ctx, mongo.Credential{User: core.MongoRootUser, Password: pw, Source: "admin"}, mongoAdminTimeout, fn)
}

// asSite runs fn on a connection logged in as the site's own user.
func (m mongoClient) asSite(ctx context.Context, c dbCreds, fn func(context.Context, *mongo.Conn) error) error {
	return m.as(ctx, mongo.Credential{User: c.User, Password: c.Password, Source: c.DB}, 0, fn)
}

// as runs fn within limit (0: the caller's deadline). Running out of its own time is an
// error of its own: one wrapping context.DeadlineExceeded would read as the job's
// timeout.
func (m mongoClient) as(ctx context.Context, cred mongo.Credential, limit time.Duration, fn func(context.Context, *mongo.Conn) error) error {
	if _, ok := ctx.Deadline(); !ok && limit == 0 {
		limit = mongoDataTimeout
	}
	slow := fmt.Errorf("MongoDB %s içinde cevap vermedi", took(limit))
	if limit > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, limit, slow)
		defer cancel()
	}
	c, err := mongo.Dial(ctx, m.Addr, cred)
	if err == nil {
		err = fn(ctx, c)
		c.Close()
	}
	if err != nil && context.Cause(ctx) == slow {
		return slow
	}
	return err
}

func mongoCmd(name string) mongo.Doc { return mongo.NewBuilder().Int32(name, 1).MustBuild() }

func (m mongoClient) Ping(ctx context.Context) error {
	return m.asRoot(ctx, func(ctx context.Context, c *mongo.Conn) error {
		_, err := c.Run(ctx, "admin", mongoCmd("ping"), mongoAdminReply)
		return err
	})
}

func (m mongoClient) SetUser(ctx context.Context, db, user, pw string) error {
	return m.asRoot(ctx, func(ctx context.Context, c *mongo.Conn) error {
		info, err := c.Run(ctx, db, mongo.NewBuilder().StartDoc("usersInfo").String("user", user).String("db", db).End().
			MustBuild(), mongoAdminReply)
		if err != nil {
			return err
		}
		users, err := array(info, "users")
		if err != nil {
			return err
		}
		verb := "createUser"
		if len(users) > 0 {
			verb = "updateUser"
		}
		cmd := mongo.NewBuilder().String(verb, user).String("pwd", pw).
			StartArray("roles").StartDoc("").String("role", "readWrite").String("db", db).End().End().MustBuild()
		_, err = c.Run(ctx, db, cmd, mongoAdminReply)
		return err
	})
}

func (m mongoClient) DropUser(ctx context.Context, db, user string) error {
	return m.asRoot(ctx, func(ctx context.Context, c *mongo.Conn) error {
		_, err := c.Run(ctx, db, mongo.NewBuilder().String("dropUser", user).MustBuild(), mongoAdminReply)
		if mongo.IsCode(err, mongo.CodeUserNotFound) {
			return nil
		}
		return err
	})
}

// DropTemp only takes names with the temporary prefix: a site's database is never
// dropped here.
func (m mongoClient) DropTemp(ctx context.Context, db string) error {
	if !strings.HasPrefix(db, tmpDBPrefix) {
		return fmt.Errorf("%q geçici bir veritabanı değil", db)
	}
	return m.asRoot(ctx, func(ctx context.Context, c *mongo.Conn) error { return dropTempDB(ctx, c, db) })
}

func dropTempDB(ctx context.Context, c *mongo.Conn, db string) error {
	if _, err := c.Run(ctx, db, mongoCmd("dropAllUsersFromDatabase"), mongoAdminReply); err != nil {
		return err
	}
	_, err := c.Run(ctx, db, mongoCmd("dropDatabase"), mongoAdminReply)
	return err
}

func (m mongoClient) DropTemps(ctx context.Context) (int, error) {
	n := 0
	err := m.asRoot(ctx, func(ctx context.Context, c *mongo.Conn) error {
		temp := map[string]bool{}
		// A check cut short may have left a user without a database, or a database
		// without its user.
		info, err := c.Run(ctx, "admin", mongo.NewBuilder().StartDoc("usersInfo").Bool("forAllDBs", true).End().
			MustBuild(), mongoListReply)
		if err != nil {
			return err
		}
		users, err := array(info, "users")
		if err != nil {
			return err
		}
		for _, u := range users {
			if db := str(u, "db"); strings.HasPrefix(db, tmpDBPrefix) {
				temp[db] = true
				n++
			}
		}
		list, err := c.Run(ctx, "admin", mongo.NewBuilder().Int32("listDatabases", 1).Bool("nameOnly", true).MustBuild(),
			mongoListReply)
		if err != nil {
			return err
		}
		dbs, err := array(list, "databases")
		if err != nil {
			return err
		}
		for _, d := range dbs {
			if name := str(d, "name"); strings.HasPrefix(name, tmpDBPrefix) {
				temp[name] = true
				n++
			}
		}
		for _, db := range sortedSet(temp) {
			if err := dropTempDB(ctx, c, db); err != nil {
				return fmt.Errorf("%s: %w", db, err)
			}
		}
		return nil
	})
	return n, err
}

func (m mongoClient) Stats(ctx context.Context, cr dbCreds) (dbStats, error) {
	var st dbStats
	err := m.asSite(ctx, cr, func(ctx context.Context, c *mongo.Conn) error {
		r, err := c.Run(ctx, cr.DB, mongoCmd("dbStats"), mongoAdminReply)
		if err != nil {
			return err
		}
		size, ok1 := whole(r, "dataSize")
		objects, ok2 := whole(r, "objects")
		colls, ok3 := whole(r, "collections")
		if !ok1 || !ok2 || !ok3 {
			return errors.New("dbStats cevabı anlaşılamadı")
		}
		st = dbStats{DataSize: size, Objects: objects, Collections: int(colls)}
		return nil
	})
	return st, err
}

func (m mongoClient) Counts(ctx context.Context, cr dbCreds) (map[string]int64, []string, error) {
	counts := map[string]int64{}
	var views []string
	err := m.asSite(ctx, cr, func(ctx context.Context, c *mongo.Conn) error {
		// Names and types only (small, and no collection locks). Without a batch size
		// MongoDB sends the whole list at once (measured with 150 collections); a cursor
		// left open would mean a list cut short, so it is an error.
		cmd := mongo.NewBuilder().Int32("listCollections", 1).Bool("nameOnly", true).
			Bool("authorizedCollections", true).StartDoc("cursor").End().MustBuild()
		reply, err := c.Run(ctx, cr.DB, cmd, mongoListReply)
		if err != nil {
			return err
		}
		docs, id, err := mongo.FirstBatch(reply)
		if id != 0 {
			c.KillCursor(ctx, cr.DB, "$cmd.listCollections", id)
			return errors.New("koleksiyon listesi tek seferde gelmedi")
		}
		if err != nil {
			return err
		}
		for _, d := range docs {
			name := str(d, "name")
			switch {
			case name == "" || strings.HasPrefix(name, "system."):
			case str(d, "type") == "view":
				views = append(views, name)
			default:
				n, err := countDocuments(ctx, c, cr.DB, name)
				if err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
				counts[name] = n
			}
		}
		return nil
	})
	sort.Strings(views)
	return counts, views, err
}

// countDocuments counts the documents themselves, as mongosh's countDocuments did; the
// count a collection keeps in its metadata can be off after an unclean shutdown.
func countDocuments(ctx context.Context, c *mongo.Conn, db, coll string) (int64, error) {
	cmd := mongo.NewBuilder().String("aggregate", coll).StartArray("pipeline").
		StartDoc("").StartDoc("$group").Int32("_id", 1).StartDoc("n").Int32("$sum", 1).End().End().End().
		End().StartDoc("cursor").End().MustBuild()
	reply, err := c.Run(ctx, db, cmd, mongoAdminReply)
	if err != nil {
		return 0, err
	}
	docs, _, err := mongo.FirstBatch(reply)
	if err != nil || len(docs) == 0 {
		return 0, err // an empty collection groups nothing
	}
	n, ok := whole(docs[0], "n")
	if !ok {
		return 0, errors.New("sayım cevabı anlaşılamadı")
	}
	return n, nil
}

func (m mongoClient) DropCollections(ctx context.Context, cr dbCreds, names []string) error {
	return m.asSite(ctx, cr, func(ctx context.Context, c *mongo.Conn) error {
		for _, name := range names {
			_, err := c.Run(ctx, cr.DB, mongo.NewBuilder().String("drop", name).MustBuild(), mongoAdminReply)
			// MongoDB 7.0 answers ok for a collection that is not there; older ones did not.
			if err != nil && !mongo.IsCode(err, mongo.CodeNamespaceNotFound) {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	})
}

// array returns the documents of an array field of a reply.
func array(d mongo.Doc, k string) ([]mongo.Doc, error) {
	v, ok := d.Lookup(k)
	if !ok {
		return nil, fmt.Errorf("MongoDB cevabında %s yok", k)
	}
	arr, ok := v.Doc()
	if !ok || v.Type != mongo.TypeArray {
		return nil, fmt.Errorf("MongoDB cevabında %s dizi değil", k)
	}
	elems, err := arr.Elems()
	if err != nil {
		return nil, err
	}
	out := make([]mongo.Doc, 0, len(elems))
	for _, e := range elems {
		doc, ok := e.Doc()
		if !ok || e.Type != mongo.TypeDocument {
			return nil, fmt.Errorf("MongoDB cevabında %s içinde belge olmayan öğe", k)
		}
		out = append(out, doc)
	}
	return out, nil
}

// whole returns a numeric field as an integer.
func whole(d mongo.Doc, k string) (int64, bool) {
	v, ok := d.Lookup(k)
	if !ok {
		return 0, false
	}
	return v.Int()
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
