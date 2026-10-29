package mongo

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// OP_MSG (MongoDB 3.6+, the only message 5.1+ accepts for commands): a 16-byte header,
// flag bits, and one section holding the command document.
const (
	opMsg        = 2013
	headerLen    = 16
	msgOverhead  = headerLen + 4 + 1 // header, flag bits, section kind
	flagChecksum = 1 << 0
	flagMore     = 1 << 1
	// DefaultTimeout bounds a command whose context has no deadline.
	DefaultTimeout = 30 * time.Second
)

// Credential is a user and the database it is defined in.
type Credential struct {
	User, Password, Source string
}

// Error is an error the server returned for a command.
type Error struct {
	Code int
	Name string
	Msg  string
}

func (e *Error) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("%s (%s)", e.Msg, e.Name)
	}
	return e.Msg
}

// Error codes the panel explains.
const (
	CodeUserNotFound      = 11
	CodeUnauthorized      = 13
	CodeNamespaceNotFound = 26
	CodeMaxTimeExpired    = 50
	CodeDuplicateKey      = 11000
	CodeNoDiskUse         = 292
)

// IsCode reports whether err is a server error with this code.
func IsCode(err error, code int) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// ErrTooLarge means a reply was bigger than the caller allowed; it was not read.
var ErrTooLarge = errors.New("sunucunun cevabı izin verilenden büyük")

// Conn is one connection, logged in. It is not safe for concurrent use.
type Conn struct {
	nc     net.Conn
	id     int32
	broken error
}

// Dial connects and logs in.
func Dial(ctx context.Context, addr string, cred Credential) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("MongoDB'ye bağlanılamadı: %w", err)
	}
	c := &Conn{nc: nc}
	if err := c.login(ctx, cred); err != nil {
		nc.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the connection.
func (c *Conn) Close() error { return c.nc.Close() }

// withDB adds the $db field every OP_MSG command carries.
func withDB(cmd Doc, db string) (Doc, error) {
	if err := cmd.header(); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(cmd)+len(db)+10)
	out = append(out, cmd[:len(cmd)-1]...)
	out = append(out, byte(TypeString))
	out = append(out, "$db"...)
	out = append(out, 0)
	out = appendString(out, db)
	out = append(out, 0)
	binary.LittleEndian.PutUint32(out, uint32(len(out)))
	return out, nil
}

// Run sends a command to db and returns the reply. A reply larger than maxReply bytes
// is not read (ErrTooLarge) and the connection is closed. A reply with ok: 0 is an
// *Error.
func (c *Conn) Run(ctx context.Context, db string, cmd Doc, maxReply int) (Doc, error) {
	if c.broken != nil {
		return nil, fmt.Errorf("bağlantı kullanılamaz: %w", c.broken)
	}
	full, err := withDB(cmd, db)
	if err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(DefaultTimeout)
	}
	_ = c.nc.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	reply, err := c.roundTrip(full, maxReply)
	if err != nil {
		c.broken = err
		c.nc.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return reply, replyErr(reply)
}

func (c *Conn) roundTrip(cmd Doc, maxReply int) (Doc, error) {
	c.id++
	msg := make([]byte, headerLen, msgOverhead+len(cmd))
	binary.LittleEndian.PutUint32(msg[4:], uint32(c.id))
	binary.LittleEndian.PutUint32(msg[12:], opMsg)
	msg = binary.LittleEndian.AppendUint32(msg, 0)
	msg = append(msg, 0)
	msg = append(msg, cmd...)
	binary.LittleEndian.PutUint32(msg, uint32(len(msg)))
	if _, err := c.nc.Write(msg); err != nil {
		return nil, fmt.Errorf("MongoDB'ye yazılamadı: %w", err)
	}
	var h [headerLen]byte
	if _, err := io.ReadFull(c.nc, h[:]); err != nil {
		return nil, fmt.Errorf("MongoDB'den okunamadı: %w", err)
	}
	n := int64(binary.LittleEndian.Uint32(h[:]))
	switch {
	case binary.LittleEndian.Uint32(h[12:]) != opMsg || int32(binary.LittleEndian.Uint32(h[8:])) != c.id:
		return nil, errors.New("MongoDB'den beklenmeyen cevap")
	case n < msgOverhead+5:
		return nil, errors.New("MongoDB'den bozuk cevap")
	case n > int64(maxReply)+msgOverhead+4:
		return nil, ErrTooLarge
	}
	body := make([]byte, n-headerLen)
	if _, err := io.ReadFull(c.nc, body); err != nil {
		return nil, fmt.Errorf("MongoDB'den okunamadı: %w", err)
	}
	flags := binary.LittleEndian.Uint32(body)
	if flags&flagMore != 0 || flags&^(flagChecksum|flagMore) != 0 {
		return nil, errors.New("MongoDB'den desteklenmeyen cevap")
	}
	body = body[4:]
	if flags&flagChecksum != 0 {
		if len(body) < 4 {
			return nil, errors.New("MongoDB'den bozuk cevap")
		}
		body = body[:len(body)-4]
	}
	if body[0] != 0 {
		return nil, errors.New("MongoDB'den desteklenmeyen cevap bölümü")
	}
	reply := Doc(body[1:])
	if err := reply.header(); err != nil {
		return nil, errors.New("MongoDB'den bozuk cevap")
	}
	if err := reply.Validate(); err != nil {
		return nil, fmt.Errorf("MongoDB'den bozuk cevap: %w", err)
	}
	return reply, nil
}

// replyErr turns {ok: 0, errmsg, code, codeName} into an *Error.
func replyErr(reply Doc) error {
	v, ok := reply.Lookup("ok")
	if !ok {
		return errors.New("MongoDB cevabında ok alanı yok")
	}
	if n, isNum := v.Int(); isNum && n == 1 {
		return nil
	}
	if b, isBool := v.Bool(); isBool && b {
		return nil
	}
	e := &Error{Msg: "MongoDB komutu başarısız"}
	if m, ok := reply.Lookup("errmsg"); ok {
		if s, ok := m.Str(); ok {
			e.Msg = s
		}
	}
	if c, ok := reply.Lookup("code"); ok {
		if n, ok := c.Int(); ok {
			e.Code = int(n)
		}
	}
	if c, ok := reply.Lookup("codeName"); ok {
		e.Name, _ = c.Str()
	}
	return e
}

// WriteErr returns the first write error of an insert, update or delete reply (those
// report them with ok: 1).
func WriteErr(reply Doc) error {
	if v, ok := reply.Lookup("writeErrors"); ok {
		if arr, ok := v.Doc(); ok {
			elems, _ := arr.Elems()
			for _, el := range elems {
				d, ok := el.Doc()
				if !ok {
					continue
				}
				return replyErr(appendOK0(d))
			}
		}
	}
	if v, ok := reply.Lookup("writeConcernError"); ok {
		if d, ok := v.Doc(); ok {
			return replyErr(appendOK0(d))
		}
	}
	return nil
}

// appendOK0 adds ok: 0 to an error document so replyErr reads it.
func appendOK0(d Doc) Doc {
	b := NewBuilder().Int32("ok", 0)
	elems, _ := d.Elems()
	for _, e := range elems {
		if e.Key == "errmsg" || e.Key == "code" || e.Key == "codeName" {
			b.Value(e.Key, e.Value)
		}
	}
	return b.MustBuild()
}

// FirstBatch returns a cursor reply's first batch and the cursor's id (0: exhausted).
func FirstBatch(reply Doc) ([]Doc, int64, error) {
	cv, ok := reply.Lookup("cursor")
	if !ok {
		return nil, 0, errors.New("MongoDB cevabında cursor yok")
	}
	cur, ok := cv.Doc()
	if !ok {
		return nil, 0, errors.New("MongoDB cevabında cursor yok")
	}
	var id int64
	if v, ok := cur.Lookup("id"); ok {
		id, _ = v.Int()
	}
	bv, ok := cur.Lookup("firstBatch")
	if !ok {
		return nil, id, errors.New("MongoDB cevabında firstBatch yok")
	}
	arr, ok := bv.Doc()
	if !ok {
		return nil, id, errors.New("MongoDB cevabında firstBatch yok")
	}
	elems, err := arr.Elems()
	if err != nil {
		return nil, id, err
	}
	docs := make([]Doc, 0, len(elems))
	for _, e := range elems {
		d, ok := e.Doc()
		if !ok || e.Type != TypeDocument {
			return nil, id, errors.New("MongoDB cevabında belge olmayan öğe")
		}
		docs = append(docs, d)
	}
	return docs, id, nil
}

// KillCursor closes a cursor left open (best effort).
func (c *Conn) KillCursor(ctx context.Context, db, coll string, id int64) {
	if id == 0 {
		return
	}
	cmd := NewBuilder().String("killCursors", coll).StartArray("cursors").Int64("", id).End().MustBuild()
	_, _ = c.Run(ctx, db, cmd, 64<<10)
}

// SafeName reports whether a database or collection name is one the panel works with:
// no NUL, no "$", not empty, at most 255 bytes.
func SafeName(s string) bool {
	return s != "" && len(s) <= 255 && !strings.ContainsAny(s, "\x00$")
}
