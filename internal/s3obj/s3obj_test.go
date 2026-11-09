package s3obj

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's own examples (Signature Version 4, "Authenticating Requests: Using the
// Authorization Header").
func TestSignVectors(t *testing.T) {
	const keyID, secret = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	got := Sign("GET", "/test.txt", "", [][2]string{{"host", "examplebucket.s3.amazonaws.com"}, {"range", "bytes=0-9"},
		{"x-amz-content-sha256", emptySHA256}, {"x-amz-date", "20130524T000000Z"}}, emptySHA256, "20130524T000000Z", "us-east-1", keyID, secret)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Errorf("GET Object:\n got %s\nwant %s", got, want)
	}
	payload := "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" // "Welcome to Amazon S3."
	if sum := sha256.Sum256([]byte("Welcome to Amazon S3.")); hex.EncodeToString(sum[:]) != payload {
		t.Fatal("payload hash")
	}
	got = Sign("PUT", "/"+awsEscape("test$file.text"), "", [][2]string{{"date", "Fri, 24 May 2013 00:00:00 GMT"},
		{"host", "examplebucket.s3.amazonaws.com"}, {"x-amz-content-sha256", payload}, {"x-amz-date", "20130524T000000Z"},
		{"x-amz-storage-class", "REDUCED_REDUNDANCY"}}, payload, "20130524T000000Z", "us-east-1", keyID, secret)
	if !strings.HasSuffix(got, "Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd") {
		t.Errorf("PUT Object: %s", got)
	}
}

func TestAt(t *testing.T) {
	for _, c := range []struct{ addr, endpoint, bucket, key, region string }{
		{"https://acc.r2.cloudflarestorage.com/footops-ornek", "https://acc.r2.cloudflarestorage.com", "footops-ornek", "izleme/durum.json", "auto"},
		{"https://acc.r2.cloudflarestorage.com/kova/a/b", "https://acc.r2.cloudflarestorage.com", "kova", "a/b/izleme/durum.json", "auto"},
		{"http://10.0.0.5:7070/footops-test/", "http://10.0.0.5:7070", "footops-test", "izleme/durum.json", "us-east-1"},
	} {
		o, err := At(c.addr, "izleme/durum.json", "k", "s")
		if err != nil || o.Endpoint.String() != c.endpoint || o.Bucket != c.bucket || o.Key != c.key || o.Region != c.region {
			t.Errorf("%s: %+v %v", c.addr, o, err)
		}
	}
	for _, bad := range []string{"https://acc.r2.cloudflarestorage.com/", "/kova", "%"} {
		if _, err := At(bad, "x", "k", "s"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fake is an S3 server that checks each request's signature with the key it knows and
// signs in region (another one is refused, naming it).
type fake struct {
	region  string
	objects map[string][]byte
	gets    int
}

const keyID, secret = "KEYID12345", "SeCrEt/Key+123="

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	if !strings.Contains(r.Header.Get("Authorization"), "/"+f.region+"/s3/") {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>AuthorizationHeaderMalformed</Code><Message>the region 'us-east-1' is wrong; expecting '`+f.region+`'</Message></Error>`)
		return
	}
	headers := [][2]string{{"content-type", r.Header.Get("Content-Type")}, {"host", r.Host}, {"x-amz-content-sha256", payload}, {"x-amz-date", r.Header.Get("X-Amz-Date")}}
	if r.Method == http.MethodGet {
		headers = headers[1:]
	}
	if r.Header.Get("Authorization") != Sign(r.Method, r.URL.EscapedPath(), "", headers, payload, r.Header.Get("X-Amz-Date"), f.region, keyID, secret) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>`)
		return
	}
	switch r.Method {
	case http.MethodPut:
		f.objects[r.URL.Path] = body
	case http.MethodGet:
		f.gets++
		data, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Last-Modified", "Mon, 09 Nov 2026 00:51:43 GMT")
		w.Write(data)
	}
}

// Written and read back in the region the server asks for; a missing object, one too
// big, a wrong key and a server that is not there each say so, without the secret or the
// address.
func TestPutGet(t *testing.T) {
	f := &fake{region: "auto", objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx, now := context.Background(), time.Now()
	o, err := At(srv.URL+"/kova/klasor", "izleme/durum.json", keyID, secret)
	if err != nil || o.Region != "us-east-1" {
		t.Fatal(o, err)
	}
	if err := o.Put(ctx, srv.Client(), []byte(`{"format":1}`), "application/json", now); err != nil || o.Region != "auto" {
		t.Fatalf("put: %v (region %q)", err, o.Region)
	}
	if string(f.objects["/kova/klasor/izleme/durum.json"]) != `{"format":1}` {
		t.Fatalf("stored: %v", f.objects)
	}
	r := o
	r.Region = "us-east-1"
	data, modified, err := r.Get(ctx, srv.Client(), 1<<10, now)
	if err != nil || string(data) != `{"format":1}` || !modified.Equal(time.Date(2026, 11, 9, 0, 51, 43, 0, time.UTC)) || r.Region != "auto" {
		t.Fatalf("get: %q %v %v (region %q)", data, modified, err, r.Region)
	}
	if _, _, err := o.Get(ctx, srv.Client(), 5, now); err == nil || !strings.Contains(err.Error(), "5 bayttan büyük") {
		t.Errorf("too big: %v", err)
	}
	missing := o
	missing.Key += ".yok"
	if _, _, err := missing.Get(ctx, srv.Client(), 1<<10, now); !errors.Is(err, ErrNoObject) {
		t.Errorf("missing: %v", err)
	}
	wrong := o
	wrong.Secret = "Wrong/Key+123="
	_, _, err = wrong.Get(ctx, srv.Client(), 1<<10, now)
	var se *Error
	if !errors.As(err, &se) || se.Status != 403 || se.Code != "SignatureDoesNotMatch" || strings.Contains(err.Error(), "Wrong") {
		t.Errorf("wrong key: %v", err)
	}
	// The address may show in the network's own words: callers mask it (the agent's
	// maskRepo, the Mac's mask).
	srv.Close()
	if _, _, err := o.Get(ctx, srv.Client(), 1<<10, now); !errors.Is(err, ErrNetwork) || !strings.HasPrefix(err.Error(), "depoya ulaşılamadı: dial tcp") {
		t.Errorf("no server: %v", err)
	}
	// An answer cut off is the network's trouble too; a refusal is not.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, `{"format":`)
	}))
	defer cut.Close()
	o.Endpoint.Host = strings.TrimPrefix(cut.URL, "http://")
	if _, _, err := o.Get(ctx, cut.Client(), 1<<10, now); !errors.Is(err, ErrNetwork) || !strings.HasPrefix(err.Error(), "depodan okunamadı") {
		t.Errorf("cut off: %v", err)
	}
	if errors.Is(&Error{Status: 403}, ErrNetwork) || errors.Is(ErrNoObject, ErrNetwork) {
		t.Error("a refusal taken for the network's trouble")
	}
}
