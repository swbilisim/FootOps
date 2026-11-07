package agent

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
)

// Enough of S3 to store one small object (the status file) and read one back (the
// outside watcher's heartbeat) without a tool container or an SDK: a PUT and a GET
// signed with AWS Signature Version 4, path-style, which Cloudflare R2 and the test
// server both take.

// s3Object is one object in the backup repository's bucket, with the key to write it.
type s3Object struct {
	endpoint *url.URL // scheme and host
	bucket   string
	key      string // inside the bucket, below the repository's folder if it has one
	region   string
	keyID    string
	secret   string
}

// s3ObjectOf places name next to the repository: in its bucket and folder.
func s3ObjectOf(sec backupSecret, name string) (s3Object, error) {
	if err := checkRepository(sec.Repository); err != nil {
		return s3Object{}, err
	}
	u, err := url.Parse(strings.TrimPrefix(sec.Repository, "s3:"))
	if err != nil {
		return s3Object{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	key := strings.Join(append(parts[1:], name), "/")
	// R2 signs with "auto"; others (the test server, most S3 services) with
	// us-east-1 unless told otherwise.
	region := "us-east-1"
	if strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
		region = "auto"
	}
	return s3Object{endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, bucket: parts[0], key: key, region: region,
		keyID: sec.KeyID, secret: sec.SecretKey}, nil
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

// sigV4 is the Authorization header of an S3 request: headers are the signed ones,
// lower-case and in order, payload the body's SHA-256 (hex), stamp the request time
// (20060102T150405Z).
func sigV4(method, path, query string, headers [][2]string, payload, stamp, region, keyID, secret string) string {
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

// path is the object's path-style address, escaped as SigV4 signs it.
func (o s3Object) path() string {
	segs := strings.Split(o.bucket+"/"+o.key, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

// put stores body as the object. The secret only ever signs: it is not sent, and no
// error carries it (the key ID shows in S3's own messages at most).
func (o s3Object) put(ctx context.Context, c *http.Client, body []byte, contentType string, now time.Time) error {
	path := o.path()
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	stamp := now.UTC().Format("20060102T150405Z")
	host := o.endpoint.Host
	auth := sigV4(http.MethodPut, path, "", [][2]string{{"content-type", contentType}, {"host", host},
		{"x-amz-content-sha256", payload}, {"x-amz-date", stamp}}, payload, stamp, o.region, o.keyID, o.secret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, o.endpoint.Scheme+"://"+host+path, bytes.NewReader(body))
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

// errNoObject is S3's 404: there is no such object.
var errNoObject = errors.New("depoda böyle bir dosya yok")

// get reads the object, at most max bytes of it; errNoObject when there is none.
func (o s3Object) get(ctx context.Context, c *http.Client, max int64, now time.Time) ([]byte, error) {
	path := o.path()
	stamp := now.UTC().Format("20060102T150405Z")
	host := o.endpoint.Host
	auth := sigV4(http.MethodGet, path, "", [][2]string{{"host", host}, {"x-amz-content-sha256", emptySHA256},
		{"x-amz-date", stamp}}, emptySHA256, stamp, o.region, o.keyID, o.secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint.Scheme+"://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	req.Header.Set("Authorization", auth)
	resp, err := do(c, req)
	if err != nil {
		return nil, err
	}
	defer drain(resp.Body)
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNoObject
	case resp.StatusCode/100 != 2:
		return nil, refusal(resp, "okumayı")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("depodan okunamadı: %v", err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("depodaki dosya %d bayttan büyük", max)
	}
	return data, nil
}

// do sends an S3 request; its error says why without the address (which names the
// account).
func do(c *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := c.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("depoya ulaşılamadı: %v", err)
	}
	return resp, nil
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
		return fmt.Errorf("depo %s reddetti (HTTP %d)", op, resp.StatusCode)
	}
	se := &s3Error{op: op, status: resp.StatusCode, code: e.Code, msg: clipMsg(e.Message, 200), region: e.Region}
	if m := reExpectRegion.FindStringSubmatch(e.Message); se.region == "" && m != nil {
		se.region = m[1]
	}
	return se
}

// s3Error is S3's refusal; region is the one it asks for, when it says.
type s3Error struct {
	op                string
	status            int
	code, msg, region string
}

func (e *s3Error) Error() string {
	return fmt.Sprintf("depo %s reddetti (HTTP %d): %s %s", e.op, e.status, e.code, e.msg)
}

// reExpectRegion reads the region from a message such as "the region 'us-east-1' is
// wrong; expecting 'auto'" (S3 itself sends a Region element).
var reExpectRegion = regexp.MustCompile(`expecting '([a-z0-9-]{1,32})'`)

// putIn stores the object and, when the server refuses the region guessed and names its
// own, tries once more in that one; o keeps the region that worked.
func (o *s3Object) putIn(ctx context.Context, c *http.Client, body []byte, contentType string, now time.Time) error {
	err := o.put(ctx, c, body, contentType, now)
	var se *s3Error
	if errors.As(err, &se) && se.code == "AuthorizationHeaderMalformed" && se.region != "" && se.region != o.region {
		o.region = se.region
		err = o.put(ctx, c, body, contentType, now)
	}
	return err
}
