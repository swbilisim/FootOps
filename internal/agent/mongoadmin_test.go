package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/mongo"
)

// liveAdmin is a mongoClient on the test MongoDB (FOOTOPS_TEST_MONGO, its root user
// named as FootOps names it), and a root connection to check what it did. Use a MongoDB
// of the tests' own (CI's service container, a throwaway local one): DropTemps removes
// every temporary database on it, an integration run's included.
func liveAdmin(t *testing.T) (mongoClient, *mongo.Conn, string) {
	t.Helper()
	raw := os.Getenv("FOOTOPS_TEST_MONGO")
	if raw == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok: gerçek MongoDB testleri atlandı")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatal(err)
	}
	if u.User.Username() != core.MongoRootUser {
		t.Skipf("FOOTOPS_TEST_MONGO'nun kullanıcısı %s değil", core.MongoRootUser)
	}
	rootPW, _ := u.User.Password()
	secret := filepath.Join(t.TempDir(), "mongo.env")
	if err := os.WriteFile(secret, []byte(core.MongoSecretEnv(rootPW)), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := mongo.Dial(context.Background(), u.Host, mongo.Credential{User: core.MongoRootUser, Password: rootPW, Source: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return mongoClient{Addr: u.Host, SecretFile: secret}, root, hex.EncodeToString(b)
}

func canLogin(m mongoClient, db, user, pw string) error {
	c, err := mongo.Dial(context.Background(), m.Addr, mongo.Credential{User: user, Password: pw, Source: db})
	if err == nil {
		c.Close()
	}
	return err
}

func usersOf(t *testing.T, root *mongo.Conn, db string) []mongo.Doc {
	t.Helper()
	reply := mustRun(t, root, db, mongo.NewBuilder().Int32("usersInfo", 1).MustBuild())
	users, err := array(reply, "users")
	if err != nil {
		t.Fatal(err)
	}
	return users
}

func databases(t *testing.T, root *mongo.Conn) []string {
	t.Helper()
	reply := mustRun(t, root, "admin", mongo.NewBuilder().Int32("listDatabases", 1).Bool("nameOnly", true).MustBuild())
	dbs, err := array(reply, "databases")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range dbs {
		out = append(out, str(d, "name"))
	}
	return out
}

func TestLiveMongoAdminSiteUser(t *testing.T) {
	m, root, id := liveAdmin(t)
	ctx := context.Background()
	db := "mat-" + id
	t.Cleanup(func() {
		_, _ = root.Run(ctx, db, mongoCmd("dropAllUsersFromDatabase"), 1<<20)
		_, _ = root.Run(ctx, db, mongoCmd("dropDatabase"), 1<<20)
	})
	if err := m.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// Created, then given a new password (the site's env was rewritten); a role added
	// behind the panel's back is gone afterwards.
	if err := m.SetUser(ctx, db, db, "Birinci1"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, root, db, mongo.NewBuilder().String("grantRolesToUser", db).StartArray("roles").
		StartDoc("").String("role", "read").String("db", "admin").End().End().MustBuild())
	if err := m.SetUser(ctx, db, db, "Ikinci22"); err != nil {
		t.Fatal(err)
	}
	if canLogin(m, db, db, "Birinci1") == nil || canLogin(m, db, db, "Ikinci22") != nil {
		t.Error("the user does not have the new password only")
	}
	users := usersOf(t, root, db)
	if len(users) != 1 {
		t.Fatalf("users: %d", len(users))
	}
	roles, _ := users[0].Lookup("roles")
	if text, _ := mongo.ValueText(roles); text != `[{"role": "readWrite", "db": "`+db+`"}]` {
		t.Errorf("roles = %s", text)
	}

	// Counts as the site's user: documents counted (a time series' measurements too),
	// views apart, system collections out (the time series' buckets among them), and a
	// list longer than a default first batch (101) comes whole.
	creds := dbCreds{User: db, Password: "Ikinci22", DB: db}
	mustRun(t, root, db, mongo.NewBuilder().String("create", "olcumler").StartDoc("timeseries").
		String("timeField", "t").End().MustBuild())
	mustRun(t, root, db, mustText(`{"insert": "olcumler", "documents": [{"t": {"$date": "2026-10-29T10:00:00Z"}, "v": 1},
		{"t": {"$date": "2026-10-29T11:00:00Z"}, "v": 2}]}`))
	mustRun(t, root, db, mongo.NewBuilder().String("insert", "urunler").StartArray("documents").
		StartDoc("").Int32("a", 1).End().StartDoc("").Int32("a", 2).End().StartDoc("").Int32("a", 3).End().End().MustBuild())
	mustRun(t, root, db, mongo.NewBuilder().String("create", "bos").MustBuild())
	mustRun(t, root, db, mongo.NewBuilder().String("create", "ucuz").String("viewOn", "urunler").
		StartArray("pipeline").StartDoc("").StartDoc("$match").Int32("a", 1).End().End().End().MustBuild())
	mustRun(t, root, db, mongo.NewBuilder().String("insert", "system.js").StartArray("documents").
		StartDoc("").String("_id", "f").End().End().MustBuild())
	want := map[string]int64{"urunler": 3, "bos": 0, "olcumler": 2}
	for i := range 110 {
		name := fmt.Sprintf("k%03d", i)
		mustRun(t, root, db, mongo.NewBuilder().String("create", name).MustBuild())
		want[name] = 0
	}
	counts, views, err := m.Counts(ctx, creds)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts, want) || !reflect.DeepEqual(views, []string{"ucuz"}) {
		t.Errorf("counts %v views %v", counts, views)
	}
	st, err := m.Stats(ctx, creds)
	if err != nil || st.Objects < 3 || st.DataSize <= 0 || st.Collections < 2 {
		t.Errorf("stats %+v %v", st, err)
	}

	if err := m.DropCollections(ctx, creds, []string{"bos", "ucuz", "olcumler", "yok-boyle-bir-sey"}); err != nil {
		t.Fatal(err)
	}
	counts, views, err = m.Counts(ctx, creds)
	if err != nil || counts["urunler"] != 3 || len(counts) != 111 || len(views) != 0 {
		t.Errorf("after drop: %d collections, views %v, %v", len(counts), views, err)
	}
	for _, c := range listNames(t, root, db) {
		if strings.Contains(c, "olcumler") {
			t.Errorf("time series left: %s", c)
		}
	}

	// Wrong passwords are refused, not taken for an empty database.
	if _, _, err := m.Counts(ctx, dbCreds{User: db, Password: "Birinci1", DB: db}); err == nil || !strings.Contains(err.Error(), "reddedildi") {
		t.Errorf("old password: %v", err)
	}
	if err := (mongoClient{Addr: m.Addr, SecretFile: writeSecret(t, "yanlis")}).Ping(ctx); err == nil || !strings.Contains(err.Error(), "reddedildi") {
		t.Errorf("wrong root password: %v", err)
	}

	// Dropping the user twice is fine; the data stays (a removed site keeps it).
	if err := m.DropUser(ctx, db, db); err != nil {
		t.Fatal(err)
	}
	if err := m.DropUser(ctx, db, db); err != nil {
		t.Errorf("second drop: %v", err)
	}
	if len(usersOf(t, root, db)) != 0 || !contains(databases(t, root), db) {
		t.Error("user kept or database dropped")
	}
	if err := m.DropTemp(ctx, db); err == nil || !contains(databases(t, root), db) {
		t.Errorf("a site's database taken for a temporary one: %v", err)
	}
}

func listNames(t *testing.T, root *mongo.Conn, db string) []string {
	t.Helper()
	reply := mustRun(t, root, db, mongo.NewBuilder().Int32("listCollections", 1).Bool("nameOnly", true).
		StartDoc("cursor").End().MustBuild())
	docs, _, err := mongo.FirstBatch(reply)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range docs {
		out = append(out, str(d, "name"))
	}
	return out
}

func writeSecret(t *testing.T, pw string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mongo.env")
	if err := os.WriteFile(p, []byte(core.MongoSecretEnv(pw)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLiveMongoAdminTemporary(t *testing.T) {
	m, root, id := liveAdmin(t)
	ctx := context.Background()
	full := tmpDBPrefix + "full" + id  // a check's database and user
	user := tmpDBPrefix + "user" + id  // cut short before anything was restored
	data := tmpDBPrefix + "data" + id  // its user already gone
	single := tmpDBPrefix + "one" + id // dropped on its own
	other := "mat-keep-" + id          // not temporary: left alone
	t.Cleanup(func() {
		for _, db := range []string{full, user, data, single, other} {
			_, _ = root.Run(ctx, db, mongoCmd("dropAllUsersFromDatabase"), 1<<20)
			_, _ = root.Run(ctx, db, mongoCmd("dropDatabase"), 1<<20)
		}
	})
	for _, db := range []string{full, user, single, other} {
		if err := m.SetUser(ctx, db, db, "Gecici123"); err != nil {
			t.Fatal(err)
		}
	}
	for _, db := range []string{full, data, single, other} {
		mustRun(t, root, db, mongo.NewBuilder().String("insert", "c").StartArray("documents").
			StartDoc("").Int32("x", 1).End().End().MustBuild())
	}

	if err := m.DropTemp(ctx, single); err != nil {
		t.Fatal(err)
	}
	if len(usersOf(t, root, single)) != 0 || contains(databases(t, root), single) {
		t.Error("temporary database or user left")
	}

	n, err := m.DropTemps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // full: user + database, user: user, data: database
		t.Errorf("removed %d, want 4", n)
	}
	for _, db := range databases(t, root) {
		if strings.HasPrefix(db, tmpDBPrefix) {
			t.Errorf("temporary database left: %s", db)
		}
	}
	for _, db := range []string{full, user, data} {
		if len(usersOf(t, root, db)) != 0 {
			t.Errorf("temporary user left in %s", db)
		}
	}
	if len(usersOf(t, root, other)) != 1 || !contains(databases(t, root), other) {
		t.Error("a database that is not temporary was touched")
	}
	if n, err := m.DropTemps(ctx); err != nil || n != 0 {
		t.Errorf("second run: %d %v", n, err)
	}
}

// A MongoDB that accepts connections but never answers: an operation running out of
// its own time says so, and is not taken for the job's timeout; the job's own deadline
// still reads as one.
func TestMongoClientOwnTimeout(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	m := mongoClient{Addr: l.Addr().String()}
	cred := mongo.Credential{User: "magaza", Password: "Sifre123", Source: "magaza"}
	noop := func(context.Context, *mongo.Conn) error { return nil }

	err = m.as(context.Background(), cred, 100*time.Millisecond, noop)
	if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "cevap vermedi") {
		t.Errorf("own limit: %v", err)
	}
	job, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.as(job, cred, mongoAdminTimeout, noop); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("job's deadline: %v", err)
	}
}
