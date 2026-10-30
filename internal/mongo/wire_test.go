package mongo

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// The example exchange of RFC 7677 (user "user", password "pencil").
func TestSCRAMExample(t *testing.T) {
	s := &scram{user: "user", password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
	if got := s.first(); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("first: %s", got)
	}
	final, err := s.final("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if err != nil {
		t.Fatal(err)
	}
	if final != "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=" {
		t.Fatalf("final: %s", final)
	}
	if err := s.verify("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="); err != nil {
		t.Fatal(err)
	}
	if err := s.verify("v=AAAATRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="); err == nil {
		t.Fatal("a wrong server signature was accepted")
	}
}

func TestSCRAMRefusesBadServer(t *testing.T) {
	for _, first := range []string{
		"r=other,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",                // nonce not ours
		"r=rOprNGfwEbeRWgbNEkqO,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096", // nothing added to it
		"r=rOprNGfwEbeRWgbNEkqOx,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=10",  // too few rounds
		"r=rOprNGfwEbeRWgbNEkqOx,s=!!,i=4096",
		"m=ext,r=rOprNGfwEbeRWgbNEkqOx,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096",
	} {
		s := &scram{user: "user", password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
		if _, err := s.final(first); err == nil {
			t.Errorf("%s accepted", first)
		}
	}
	for _, pw := range []string{"", "şifre", "a b", "a\x00"} {
		c := &Conn{}
		if err := c.login(context.Background(), Credential{User: "u", Password: pw, Source: "x"}); err == nil {
			t.Errorf("password %q accepted", pw)
		}
	}
}

// fakeServer answers one OP_MSG with whatever reply returns (raw message bytes after the
// header is built by the test).
func fakeServer(t *testing.T, handle func(req []byte, id int32) []byte) *Conn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })
	go func() {
		for {
			var h [headerLen]byte
			if _, err := io.ReadFull(server, h[:]); err != nil {
				return
			}
			n := binary.LittleEndian.Uint32(h[:])
			body := make([]byte, n-headerLen)
			if _, err := io.ReadFull(server, body); err != nil {
				return
			}
			out := handle(body, int32(binary.LittleEndian.Uint32(h[4:])))
			if out == nil {
				server.Close()
				return
			}
			if _, err := server.Write(out); err != nil {
				return
			}
		}
	}()
	return &Conn{nc: client}
}

func message(id, responseTo int32, flags uint32, doc Doc, extra []byte) []byte {
	m := make([]byte, headerLen)
	binary.LittleEndian.PutUint32(m[4:], uint32(id))
	binary.LittleEndian.PutUint32(m[8:], uint32(responseTo))
	binary.LittleEndian.PutUint32(m[12:], opMsg)
	m = binary.LittleEndian.AppendUint32(m, flags)
	m = append(m, 0)
	m = append(m, doc...)
	m = append(m, extra...)
	binary.LittleEndian.PutUint32(m, uint32(len(m)))
	return m
}

func TestRunRoundTrip(t *testing.T) {
	var gotDB string
	c := fakeServer(t, func(req []byte, id int32) []byte {
		cmd := Doc(req[5:])
		v, _ := cmd.Lookup("$db")
		gotDB, _ = v.Str()
		return message(1, id, 0, NewBuilder().Double("ok", 1).Int32("n", 3).MustBuild(), nil)
	})
	reply, err := c.Run(context.Background(), "site", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := mustLookup(t, reply, "n").Int(); n != 3 || gotDB != "site" {
		t.Fatalf("reply %v, db %q", n, gotDB)
	}
	// A checksum at the end is skipped.
	c = fakeServer(t, func(req []byte, id int32) []byte {
		return message(1, id, flagChecksum, NewBuilder().Int32("ok", 1).MustBuild(), []byte{1, 2, 3, 4})
	})
	if _, err := c.Run(context.Background(), "x", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10); err != nil {
		t.Fatal(err)
	}
}

func TestRunServerError(t *testing.T) {
	c := fakeServer(t, func(req []byte, id int32) []byte {
		return message(1, id, 0, NewBuilder().Double("ok", 0).String("errmsg", "operation exceeded time limit").
			Int32("code", 50).String("codeName", "MaxTimeMSExpired").MustBuild(), nil)
	})
	_, err := c.Run(context.Background(), "x", NewBuilder().Int32("find", 1).MustBuild(), 1<<10)
	if !IsCode(err, CodeMaxTimeExpired) || !strings.Contains(err.Error(), "time limit") {
		t.Fatalf("err = %v", err)
	}
	// A server error keeps the connection usable.
	if _, err := c.Run(context.Background(), "x", NewBuilder().Int32("find", 1).MustBuild(), 1<<10); !IsCode(err, 50) {
		t.Fatalf("second: %v", err)
	}
}

func TestRunRefusesBadReplies(t *testing.T) {
	ok := NewBuilder().Int32("ok", 1).MustBuild()
	cases := map[string]func(req []byte, id int32) []byte{
		"wrong responseTo": func(_ []byte, id int32) []byte { return message(1, id+1, 0, ok, nil) },
		"more to come":     func(_ []byte, id int32) []byte { return message(1, id, flagMore, ok, nil) },
		"unknown flag":     func(_ []byte, id int32) []byte { return message(1, id, 1<<5, ok, nil) },
		"broken document":  func(_ []byte, id int32) []byte { return message(1, id, 0, Doc{9, 0, 0, 0, 0x20, 'a', 0, 0, 0}, nil) },
		"closed":           func([]byte, int32) []byte { return nil },
		"too large": func(_ []byte, id int32) []byte {
			return message(1, id, 0, NewBuilder().String("s", strings.Repeat("x", 4<<10)).Int32("ok", 1).MustBuild(), nil)
		},
	}
	for name, h := range cases {
		c := fakeServer(t, h)
		_, err := c.Run(context.Background(), "x", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if name == "too large" && !errors.Is(err, ErrTooLarge) {
			t.Errorf("too large: %v", err)
		}
		// The connection is not used again after a transport error.
		if _, err := c.Run(context.Background(), "x", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10); err == nil {
			t.Errorf("%s: broken connection reused", name)
		}
	}
}

func TestRunStopsOnContext(t *testing.T) {
	c := fakeServer(t, func([]byte, int32) []byte {
		time.Sleep(5 * time.Second)
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Run(ctx, "x", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10)
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

// lateCtx reports its deadline but is done only later: a context whose timer lags behind
// the socket deadline set from it.
type lateCtx struct {
	context.Context
	deadline time.Time
	done     chan struct{}
}

func (c lateCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c lateCtx) Done() <-chan struct{}       { return c.done }
func (c lateCtx) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// The socket's deadline fires before the context's timer: the error is still the
// context's, not an i/o timeout (seen on CI: a limit's own message got lost).
func TestRunDeadlineBeatsContextTimer(t *testing.T) {
	c := fakeServer(t, func([]byte, int32) []byte {
		time.Sleep(5 * time.Second)
		return nil
	})
	ctx := lateCtx{Context: context.Background(), deadline: time.Now().Add(50 * time.Millisecond), done: make(chan struct{})}
	time.AfterFunc(150*time.Millisecond, func() { close(ctx.done) })
	if _, err := c.Run(ctx, "x", NewBuilder().Int32("ping", 1).MustBuild(), 1<<10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteErr(t *testing.T) {
	reply := NewBuilder().Int32("ok", 1).Int32("n", 0).StartArray("writeErrors").
		StartDoc("").Int32("index", 0).Int32("code", 11000).String("errmsg", "E11000 duplicate key").End().End().MustBuild()
	if err := WriteErr(reply); !IsCode(err, CodeDuplicateKey) {
		t.Fatalf("err = %v", err)
	}
	if err := WriteErr(NewBuilder().Int32("ok", 1).Int32("n", 1).MustBuild()); err != nil {
		t.Fatal(err)
	}
}

func TestFirstBatch(t *testing.T) {
	reply := NewBuilder().StartDoc("cursor").Int64("id", 0).String("ns", "a.b").
		StartArray("firstBatch").StartDoc("").Int32("x", 1).End().StartDoc("").Int32("x", 2).End().End().End().
		Int32("ok", 1).MustBuild()
	docs, id, err := FirstBatch(reply)
	if err != nil || len(docs) != 2 || id != 0 {
		t.Fatalf("%d docs, id %d, %v", len(docs), id, err)
	}
	if _, _, err := FirstBatch(NewBuilder().Int32("ok", 1).MustBuild()); err == nil {
		t.Fatal("reply without cursor accepted")
	}
}
