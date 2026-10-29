package mongo

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SCRAM-SHA-256 (RFC 7677), MongoDB's default since 4.0. Only ASCII passwords are
// accepted: they need no SASLprep (FootOps' generated ones are letters and digits).

const scramMinIterations = 4096

func (c *Conn) login(ctx context.Context, cred Credential) error {
	if !printableASCII(cred.Password) || cred.User == "" {
		return errors.New("MongoDB girişi: kullanıcı adı boş ya da şifrede desteklenmeyen karakter var")
	}
	nonce := make([]byte, 24)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	s := &scram{user: cred.User, password: cred.Password, nonce: base64.StdEncoding.EncodeToString(nonce)}
	start := NewBuilder().Int32("saslStart", 1).String("mechanism", "SCRAM-SHA-256").
		Binary("payload", 0, []byte(s.first())).
		StartDoc("options").Bool("skipEmptyExchange", true).End().MustBuild()
	reply, err := c.Run(ctx, cred.Source, start, 64<<10)
	if err != nil {
		return loginErr(err)
	}
	conv, ok := reply.Lookup("conversationId")
	if !ok {
		return errors.New("MongoDB girişi: sunucunun cevabı geçersiz (conversationId yok)")
	}
	final, err := s.final(payload(reply))
	if err != nil {
		return err
	}
	for range 2 {
		cont := NewBuilder().Int32("saslContinue", 1).Value("conversationId", conv).
			Binary("payload", 0, []byte(final)).MustBuild()
		reply, err = c.Run(ctx, cred.Source, cont, 64<<10)
		if err != nil {
			return loginErr(err)
		}
		if final != "" {
			if err := s.verify(payload(reply)); err != nil {
				return err
			}
			final = "" // an older server may still want one empty round
		}
		if done, _ := lookupBool(reply, "done"); done {
			return nil
		}
	}
	return errors.New("MongoDB girişi tamamlanmadı")
}

func loginErr(err error) error {
	if IsCode(err, 18) { // AuthenticationFailed
		return errors.New("MongoDB girişi reddedildi (kullanıcı ya da şifre yanlış)")
	}
	return fmt.Errorf("MongoDB girişi: %w", err)
}

func lookupBool(d Doc, k string) (bool, bool) {
	v, ok := d.Lookup(k)
	if !ok {
		return false, false
	}
	return v.Bool()
}

func payload(reply Doc) string {
	v, ok := reply.Lookup("payload")
	if !ok || v.Type != TypeBinary || len(v.Data) < 5 {
		return ""
	}
	return string(v.Data[5:])
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return s != ""
}

type scram struct {
	user, password, nonce string
	authMessage           string
	salted                []byte
}

func (s *scram) firstBare() string {
	name := strings.NewReplacer("=", "=3D", ",", "=2C").Replace(s.user)
	return "n=" + name + ",r=" + s.nonce
}

func (s *scram) first() string { return "n,," + s.firstBare() }

// final answers the server's first message with the client proof.
func (s *scram) final(serverFirst string) (string, error) {
	var rnonce, salt string
	iter := 0
	for _, part := range strings.Split(serverFirst, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "r":
			rnonce = v
		case "s":
			salt = v
		case "i":
			iter, _ = strconv.Atoi(v)
		case "m":
			return "", errors.New("MongoDB girişi: desteklenmeyen SCRAM uzantısı")
		}
	}
	if !strings.HasPrefix(rnonce, s.nonce) || len(rnonce) == len(s.nonce) {
		return "", errors.New("MongoDB girişi: sunucunun cevabı geçersiz (nonce)")
	}
	if iter < scramMinIterations || iter > 10_000_000 {
		return "", errors.New("MongoDB girişi: sunucunun cevabı geçersiz (tur sayısı)")
	}
	saltB, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || len(saltB) == 0 {
		return "", errors.New("MongoDB girişi: sunucunun cevabı geçersiz (tuz)")
	}
	s.salted, err = pbkdf2.Key(sha256.New, s.password, saltB, iter, sha256.Size)
	if err != nil {
		return "", err
	}
	clientKey := mac(s.salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + rnonce
	s.authMessage = s.firstBare() + "," + serverFirst + "," + withoutProof
	sig := mac(stored[:], s.authMessage)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), nil
}

// verify checks the server's signature: the server, too, knows the password.
func (s *scram) verify(serverFinal string) error {
	if e, ok := strings.CutPrefix(serverFinal, "e="); ok {
		return fmt.Errorf("MongoDB girişi reddedildi: %s", e)
	}
	v, ok := strings.CutPrefix(serverFinal, "v=")
	if !ok {
		return errors.New("MongoDB girişi: sunucunun son cevabı geçersiz")
	}
	got, err := base64.StdEncoding.DecodeString(v)
	want := mac(mac(s.salted, "Server Key"), s.authMessage)
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return errors.New("MongoDB girişi: sunucunun imzası tutmadı")
	}
	return nil
}

func mac(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}
