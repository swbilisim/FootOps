package mongo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"
)

// Tests against a real mongod, when FOOTOPS_TEST_MONGO names one with a root user:
//
//	FOOTOPS_TEST_MONGO=mongodb://root:pw@127.0.0.1:27017 go test ./internal/mongo/
//
// CI runs them with a mongo service container.

// liveServer returns the test server's address and root credential, or skips.
func liveServer(t testing.TB) (string, Credential) {
	t.Helper()
	raw := os.Getenv("FOOTOPS_TEST_MONGO")
	if raw == "" {
		t.Skip("FOOTOPS_TEST_MONGO yok: gerçek MongoDB testleri atlandı")
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		t.Fatalf("FOOTOPS_TEST_MONGO: %v", err)
	}
	pw, _ := u.User.Password()
	return u.Host, Credential{User: u.User.Username(), Password: pw, Source: "admin"}
}

// liveDB creates a database with its own readWrite user and returns the user's
// credential; both are dropped at the end of the test.
func liveDB(t *testing.T) (string, Credential, *Conn) {
	t.Helper()
	addr, root := liveServer(t)
	ctx := context.Background()
	rc, err := Dial(ctx, addr, root)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	db := "footops_test_" + hex.EncodeToString(b)
	pw := "Pw" + hex.EncodeToString(b) + "x9"
	run(t, rc, db, NewBuilder().String("createUser", db).String("pwd", pw).
		StartArray("roles").StartDoc("").String("role", "readWrite").String("db", db).End().End().MustBuild())
	t.Cleanup(func() {
		_, _ = rc.Run(ctx, db, NewBuilder().String("dropUser", db).MustBuild(), 1<<20)
		_, _ = rc.Run(ctx, db, NewBuilder().Int32("dropDatabase", 1).MustBuild(), 1<<20)
		rc.Close()
	})
	return addr, Credential{User: db, Password: pw, Source: db}, rc
}

func run(t *testing.T, c *Conn, db string, cmd Doc) Doc {
	t.Helper()
	reply, err := c.Run(context.Background(), db, cmd, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteErr(reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func TestLiveRoundTrip(t *testing.T) {
	addr, cred, _ := liveDB(t)
	ctx := context.Background()
	start := time.Now()
	c, err := Dial(ctx, addr, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t.Logf("bağlantı + giriş: %v", time.Since(start))
	if _, err := Dial(ctx, addr, Credential{User: cred.User, Password: "yanlis1", Source: cred.Source}); err == nil {
		t.Fatal("wrong password accepted")
	}

	// Every type the panel may meet is stored and read back byte for byte, and its
	// text reads back the same.
	d := allTypes(t)
	run(t, c, cred.Source, NewBuilder().String("insert", "t").StartArray("documents").Doc("", d).End().MustBuild())
	id, _ := d.Lookup("_id")
	reply := run(t, c, cred.Source, NewBuilder().String("find", "t").StartDoc("filter").Value("_id", id).End().
		Int32("limit", 1).Bool("singleBatch", true).MustBuild())
	docs, _, err := FirstBatch(reply)
	if err != nil || len(docs) != 1 {
		t.Fatalf("%d docs, %v", len(docs), err)
	}
	if !bytes.Equal(docs[0], d) {
		t.Fatal("stored document differs")
	}
	text, info := DocText(docs[0], true)
	back, err := ParseDoc(text)
	if err != nil || !info.Exact() || !bytes.Equal(back, d) {
		t.Fatalf("text round trip: %v", err)
	}

	// The server's own decimal text matches ours.
	for _, s := range []string{"12.50", "0.000001", "1E-7", "1.0E+3", "-0", "0E+3", "123456789012345678901234567890.1234",
		"9.999999999999999999999999999999999E+6144", "1E-6176", "Infinity", "NaN"} {
		dec, err := parseDecimal(s)
		if err != nil {
			t.Fatal(err)
		}
		cmd := NewBuilder().String("aggregate", "t").StartArray("pipeline").
			StartDoc("").StartDoc("$project").Int32("_id", 0).StartDoc("s").
			StartDoc("$toString").Value("$literal", Value{Type: TypeDecimal, Data: dec[:]}).End().End().End().End().
			StartDoc("").Int32("$limit", 1).End().End().StartDoc("cursor").End().MustBuild()
		docs, _, err := FirstBatch(run(t, c, cred.Source, cmd))
		if err != nil || len(docs) != 1 {
			t.Fatal(err)
		}
		v, _ := docs[0].Lookup("s")
		got, _ := v.Str()
		mine, _ := decimalText(dec[:])
		if got != mine {
			t.Errorf("decimal %s: server %q, panel %q", s, got, mine)
		}
	}

	// Errors carry the server's code.
	_, err = c.Run(ctx, cred.Source, NewBuilder().String("find", "t").StartDoc("filter").
		StartDoc("$where").End().End().MustBuild(), 1<<20)
	var se *Error
	if !errors.As(err, &se) || se.Code == 0 {
		t.Fatalf("bad filter: %v", err)
	}
	// A user of one database cannot read another.
	if _, err := c.Run(ctx, "admin", NewBuilder().Int32("listCollections", 1).MustBuild(), 1<<20); !IsCode(err, CodeUnauthorized) {
		t.Fatalf("other database: %v", err)
	}
}

func TestLiveTooLarge(t *testing.T) {
	addr, cred, _ := liveDB(t)
	ctx := context.Background()
	c, err := Dial(ctx, addr, cred)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	big := NewBuilder().String("s", string(bytes.Repeat([]byte("x"), 200<<10))).MustBuild()
	run(t, c, cred.Source, NewBuilder().String("insert", "t").StartArray("documents").Doc("", big).End().MustBuild())
	_, err = c.Run(ctx, cred.Source, NewBuilder().String("find", "t").MustBuild(), 64<<10)
	if err != ErrTooLarge {
		t.Fatalf("err = %v", err)
	}
}
