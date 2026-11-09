// Package s3obj stores and reads single small objects in an S3 bucket without a tool
// container or an SDK: a PUT and a GET signed with AWS Signature Version 4, path-style,
// which Cloudflare R2 and the test server both take. The agent writes its status file
// and reads the outside watcher's heartbeat with it; the Mac's footops-mac reads both
// (panel-tasarim §27.8, §29.2).
package s3obj

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Object is one object in a bucket, with the key to write or read it.
type Object struct {
	Endpoint *url.URL // scheme and host
	Bucket   string
	Key      string // inside the bucket, below the repository's folder if it has one
	// Region is the one requests are signed in: guessed from the host, then the one the
	// server names when it refuses the guess (Put and Get keep it).
	Region string
	KeyID  string
	Secret string
}

// At places name next to an S3 repository — in its bucket and folder. addr is the
// repository's address without restic's "s3:" (https://host/bucket[/folder…]), already
// checked by the caller.
func At(addr, name, keyID, secret string) (Object, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return Object{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host == "" || parts[0] == "" {
		return Object{}, errors.New("depo adresinde kova yok")
	}
	key := strings.Join(append(parts[1:], name), "/")
	// R2 signs with "auto"; others (the test server, most S3 services) with
	// us-east-1 unless told otherwise.
	region := "us-east-1"
	if strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
		region = "auto"
	}
	return Object{Endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, Bucket: parts[0], Key: key, Region: region,
		KeyID: keyID, Secret: secret}, nil
}

// awsEscape is SigV4's URI encoding of one path segment: everything but the unreserved
// characters, in upper-case hex.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// Sign is the Authorization header of an S3 request: headers are the signed ones,
// lower-case and in order, payload the body's SHA-256 (hex), stamp the request time
// (20060102T150405Z). Test servers check requests with it.
func Sign(method, path, query string, headers [][2]string, payload, stamp, region, keyID, secret string) string {
	day := stamp[:8]
	var canon, names strings.Builder
	for i, h := range headers {
		canon.WriteString(h[0] + ":" + strings.TrimSpace(h[1]) + "\n")
		if i > 0 {
			names.WriteByte(';')
		}
		names.WriteString(h[0])
	}
	canonical := method + "\n" + path + "\n" + query + "\n" + canon.String() + "\n" + names.String() + "\n" + payload
	scope := day + "/" + region + "/s3/aws4_request"
	ch := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA256([]byte("AWS4"+secret), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return "AWS4-HMAC-SHA256 Credential=" + keyID + "/" + scope + ", SignedHeaders=" + names.String() +
		", Signature=" + hex.EncodeToString(hmacSHA256(k, toSign))
}

// Path is the object's path-style address, escaped as SigV4 signs it.
func (o Object) Path() string {
	segs := strings.Split(o.Bucket+"/"+o.Key, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

// put stores body as the object. The secret only ever signs: it is not sent, and no
// error carries it (the key ID shows in S3's own messages at most).
func (o Object) put(ctx context.Context, c *http.Client, body []byte, contentType string, now time.Time) error {
	path := o.Path()
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	stamp := now.UTC().Format("20060102T150405Z")
	host := o.Endpoint.Host
	auth := Sign(http.MethodPut, path, "", [][2]string{{"content-type", contentType}, {"host", host},
		{"x-amz-content-sha256", payload}, {"x-amz-date", stamp}}, payload, stamp, o.Region, o.KeyID, o.Secret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, o.Endpoint.Scheme+"://"+host+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	req.Header.Set("Authorization", auth)
	resp, err := do(c, req)
	if err != nil {
		return err
	}
	defer drain(resp.Body)
	if resp.StatusCode/100 == 2 {
		return nil
	}
	return refusal(resp, "yazmayı")
}

// emptySHA256 is the payload hash of a request without a body.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// ErrNoObject is S3's 404: there is no such object.
var ErrNoObject = errors.New("depoda böyle bir dosya yok")

// get reads the object, at most max bytes of it, and when it was last written.
func (o Object) get(ctx context.Context, c *http.Client, max int64, now time.Time) ([]byte, time.Time, error) {
	path := o.Path()
	stamp := now.UTC().Format("20060102T150405Z")
	host := o.Endpoint.Host
	auth := Sign(http.MethodGet, path, "", [][2]string{{"host", host}, {"x-amz-content-sha256", emptySHA256},
		{"x-amz-date", stamp}}, emptySHA256, stamp, o.Region, o.KeyID, o.Secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.Endpoint.Scheme+"://"+host+path, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	req.Header.Set("Authorization", auth)
	resp, err := do(c, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer drain(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, time.Time{}, ErrNoObject
	case resp.StatusCode/100 != 2:
		return nil, time.Time{}, refusal(resp, "okumayı")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, time.Time{}, netError{fmt.Errorf("depodan okunamadı: %v", err)}
	}
	if int64(len(data)) > max {
		return nil, time.Time{}, fmt.Errorf("depodaki dosya %d bayttan büyük", max)
	}
	modified, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
	return data, modified, nil
}

// ErrNetwork is the way to the server failing — no answer, or one cut off — rather
// than anything the server said: errors.Is tells them apart.
var ErrNetwork = errors.New("depoyla bağlantı sorunu")

type netError struct{ error }

func (netError) Is(target error) bool { return target == ErrNetwork }

// do sends an S3 request; its error says why without the address (which names the
// account).
func do(c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, netError{fmt.Errorf("depoya ulaşılamadı: %v", err)}
	}
	return resp, nil
}

// drain discards a response body so the connection can be reused.
func drain(r io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	r.Close()
}

// refusal is S3's answer other than 2xx, as an error; op is what was refused.
func refusal(resp *http.Response, op string) error {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
		Region  string `xml:"Region"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if xml.Unmarshal(data, &e) != nil || e.Code == "" {
		return &Error{op: op, Status: resp.StatusCode}
	}
	se := &Error{op: op, Status: resp.StatusCode, Code: e.Code, msg: clip(e.Message, 200), region: e.Region}
	if m := reExpectRegion.FindStringSubmatch(e.Message); se.region == "" && m != nil {
		se.region = m[1]
	}
	return se
}

// Error is S3's refusal: the HTTP status and S3's own code (AccessDenied,
// SignatureDoesNotMatch…), when it sent one; region is the one it asks for, when it says.
type Error struct {
	op     string
	Status int
	Code   string
	msg    string
	region string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("depo %s reddetti (HTTP %d)", e.op, e.Status)
	}
	return fmt.Sprintf("depo %s reddetti (HTTP %d): %s %s", e.op, e.Status, e.Code, e.msg)
}

// reExpectRegion reads the region from a message such as "the region 'us-east-1' is
// wrong; expecting 'auto'" (S3 itself sends a Region element).
var reExpectRegion = regexp.MustCompile(`expecting '([a-z0-9-]{1,32})'`)

// otherRegion is the region a refusal asks for, when it is one not tried yet.
func (o *Object) otherRegion(err error) bool {
	var se *Error
	if errors.As(err, &se) && se.Code == "AuthorizationHeaderMalformed" && se.region != "" && se.region != o.Region {
		o.Region = se.region
		return true
	}
	return false
}

// Put stores body as the object and, when the server refuses the region guessed and
// names its own, tries once more in that one; o keeps the region that worked.
func (o *Object) Put(ctx context.Context, c *http.Client, body []byte, contentType string, now time.Time) error {
	err := o.put(ctx, c, body, contentType, now)
	if o.otherRegion(err) {
		err = o.put(ctx, c, body, contentType, now)
	}
	return err
}

// Get reads the object, at most max bytes of it, and when the server last wrote it (by
// its own clock; zero when it does not say). ErrNoObject when there is none; a region
// refused is tried once more as Put does.
func (o *Object) Get(ctx context.Context, c *http.Client, max int64, now time.Time) ([]byte, time.Time, error) {
	data, modified, err := o.get(ctx, c, max, now)
	if o.otherRegion(err) {
		data, modified, err = o.get(ctx, c, max, now)
	}
	return data, modified, err
}

// clip keeps at most n bytes of a message (whole characters).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
