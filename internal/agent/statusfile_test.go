package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// AWS's own examples (Signature Version 4, "Authenticating Requests: Using the
// Authorization Header").
func TestSigV4Vectors(t *testing.T) {
	const keyID, secret = "AKIAIOSFODNN7EXAMPLE", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	empty := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	got := sigV4("GET", "/test.txt", "", [][2]string{{"host", "examplebucket.s3.amazonaws.com"}, {"range", "bytes=0-9"},
		{"x-amz-content-sha256", empty}, {"x-amz-date", "20130524T000000Z"}}, empty, "20130524T000000Z", "us-east-1", keyID, secret)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Errorf("GET Object:\n got %s\nwant %s", got, want)
	}
	payload := "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" // "Welcome to Amazon S3."
	if sum := sha256.Sum256([]byte("Welcome to Amazon S3.")); hex.EncodeToString(sum[:]) != payload {
		t.Fatal("payload hash")
	}
	got = sigV4("PUT", "/"+awsEscape("test$file.text"), "", [][2]string{{"date", "Fri, 24 May 2013 00:00:00 GMT"},
		{"host", "examplebucket.s3.amazonaws.com"}, {"x-amz-content-sha256", payload}, {"x-amz-date", "20130524T000000Z"},
		{"x-amz-storage-class", "REDUCED_REDUNDANCY"}}, payload, "20130524T000000Z", "us-east-1", keyID, secret)
	if !strings.HasSuffix(got, "Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd") {
		t.Errorf("PUT Object: %s", got)
	}
}

func TestS3ObjectOf(t *testing.T) {
	for _, c := range []struct{ repo, endpoint, bucket, key, region string }{
		{"s3:https://acc.r2.cloudflarestorage.com/footops-ornek", "https://acc.r2.cloudflarestorage.com", "footops-ornek", "izleme/durum.json", "auto"},
		{"s3:https://acc.r2.cloudflarestorage.com/kova/a/b", "https://acc.r2.cloudflarestorage.com", "kova", "a/b/izleme/durum.json", "auto"},
		{"s3:http://10.0.0.5:7070/footops-test/", "http://10.0.0.5:7070", "footops-test", "izleme/durum.json", "us-east-1"},
	} {
		o, err := s3ObjectOf(backupSecret{Repository: c.repo, KeyID: "k", SecretKey: "s"}, StatusKey)
		if err != nil || o.endpoint.String() != c.endpoint || o.bucket != c.bucket || o.key != c.key || o.region != c.region {
			t.Errorf("%s: %+v %v", c.repo, o, err)
		}
	}
	if _, err := s3ObjectOf(backupSecret{Repository: "s3:http://example.com/kova"}, StatusKey); err == nil {
		t.Error("plain http to a public host accepted")
	}
}

// fakeS3 takes PUTs, checking each signature with the key it knows.
type fakeS3 struct {
	mu      sync.Mutex
	keyID   string
	secret  string
	region  string // signs in this region ("" us-east-1); another one is refused naming it
	objects map[string][]byte
	refuse  bool
	puts    int
	gets    int
	// refuseGet refuses reads only.
	refuseGet bool
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	stamp := r.Header.Get("X-Amz-Date")
	region := f.region
	if region == "" {
		region = "us-east-1"
	}
	if !strings.Contains(r.Header.Get("Authorization"), "/"+region+"/s3/") {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>AuthorizationHeaderMalformed</Code><Message>the region 'us-east-1' is wrong; expecting '`+region+`'</Message></Error>`)
		return
	}
	headers := [][2]string{{"content-type", r.Header.Get("Content-Type")}, {"host", r.Host}, {"x-amz-content-sha256", payload}, {"x-amz-date", stamp}}
	if r.Method == http.MethodGet {
		headers = headers[1:]
	}
	want := sigV4(r.Method, r.URL.EscapedPath(), "", headers, payload, stamp, region, f.keyID, f.secret)
	if (r.Method != http.MethodPut && r.Method != http.MethodGet) || r.Header.Get("Authorization") != want || r.Header.Get("X-Amz-Content-Sha256") != payload {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>`)
		return
	}
	if f.refuse {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		return
	}
	if r.Method == http.MethodGet {
		f.gets++
		if f.refuseGet {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
			return
		}
		data, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
			return
		}
		w.Write(data)
		return
	}
	f.puts++
	f.objects[r.URL.Path] = body
}

func TestStatusFile(t *testing.T) {
	e := newBackups(t)
	s3 := &fakeS3{keyID: "KEYID12345", secret: "SeCrEt/Key+123=", objects: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	w := &StatusWriter{Backups: e.b, Sites: e.b.Data.Store.List, Server: "T1", HTTP: srv.Client(), now: func() time.Time { return now },
		Containers: func(context.Context) []ContainerInfo {
			return []ContainerInfo{{Name: "magaza", State: "running", Health: "unhealthy"}, {Name: "footops-mongo", State: "running"}}
		}}
	ctx := context.Background()

	// No repository: nothing to write to, nothing to say.
	if err := w.Write(ctx); err != nil || s3.puts != 0 || w.State().Key != "" {
		t.Fatalf("without a repository: %v %d %+v", err, s3.puts, w.State())
	}
	// A second site without a container.
	blog := specFor("blog")
	blog.Domains, blog.Database, blog.DataMount = []string{"blog.enisparta.com"}, false, ""
	if err := e.ops.Create(ctx, blog, nil, e.progress); err != nil {
		t.Fatal(err)
	}
	sec := backupSecret{Repository: "s3:" + srv.URL + "/footops-t1/klasor", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"}
	if err := saveBackupSecret(e.b.SecretPath, sec); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(ctx); err != nil {
		t.Fatal(err)
	}
	body := s3.objects["/footops-t1/klasor/izleme/durum.json"]
	if body == nil {
		t.Fatalf("not written: %v", s3.objects)
	}
	for _, secret := range []string{sec.Password, sec.SecretKey, sec.KeyID} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("secret in the status file:\n%s", body)
		}
	}
	var got StatusFile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Format != 1 || got.Server != "T1" || !got.Time.Equal(now) || got.Every != 300 || len(got.Sites) != 2 {
		t.Fatalf("status: %s", body)
	}
	if s := got.Sites[0]; s.Name != "blog" || s.State != "missing" || len(s.Domains) == 0 {
		t.Errorf("blog: %+v", s)
	}
	if s := got.Sites[1]; s.Name != "magaza" || s.State != "unhealthy" {
		t.Errorf("magaza: %+v", s)
	}
	if got.Backup.Ready || len(got.Backup.Problems) != 1 || !strings.Contains(got.Backup.Problems[0], "onaylanmadı") {
		t.Errorf("backup before confirming: %+v", got.Backup)
	}
	if st := w.State(); st.Key != "footops-t1/klasor/izleme/durum.json" || !st.LastOK.Equal(now) || st.LastErr != "" {
		t.Errorf("state: %+v", st)
	}

	// Refused: the reason is kept, without the address; an alert after a while.
	s3.refuse = true
	now = now.Add(5 * time.Minute)
	err := w.Write(ctx)
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("refused: %v", err)
	}
	if al := w.Alerts(now.Add(10 * time.Minute)); len(al) != 0 {
		t.Fatalf("alert too soon: %+v", al)
	}
	al := w.Alerts(now.Add(21 * time.Minute))
	if len(al) != 1 || al[0].Key != "monitor:status" || !Notifiable(al[0]) || !strings.Contains(al[0].Detail, "AccessDenied") {
		t.Fatalf("alert: %+v", al)
	}
	// A wrong key: the signature does not match.
	s3.refuse = false
	sec.SecretKey = "Wrong/Key+123="
	saveBackupSecret(e.b.SecretPath, sec)
	if err := w.Write(ctx); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") || strings.Contains(err.Error(), "Wrong") {
		t.Fatalf("wrong key: %v", err)
	}
	sec.SecretKey = "SeCrEt/Key+123="
	saveBackupSecret(e.b.SecretPath, sec)
	if err := e.b.Confirm(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	if err := w.Write(ctx); err != nil {
		t.Fatal(err)
	}
	if al := w.Alerts(now.Add(time.Hour)); len(al) != 0 || w.State().LastErr != "" {
		t.Fatalf("recovered: %+v %+v", al, w.State())
	}
	json.Unmarshal(s3.objects["/footops-t1/klasor/izleme/durum.json"], &got)
	if !got.Backup.Ready {
		t.Errorf("confirmed: %+v", got.Backup)
	}
}

// TestLiveS3Put writes to a real bucket when FOOTOPS_TEST_S3_REPO (s3:https://…/bucket),
// FOOTOPS_TEST_S3_KEY and FOOTOPS_TEST_S3_SECRET are set (araclar/yedek-y4/r2.sh), and
// skips otherwise; the script reads the object back with curl and removes it.
func TestLiveS3Put(t *testing.T) {
	repo := os.Getenv("FOOTOPS_TEST_S3_REPO")
	if repo == "" {
		t.Skip("FOOTOPS_TEST_S3_REPO not set")
	}
	sec := backupSecret{Repository: repo, KeyID: os.Getenv("FOOTOPS_TEST_S3_KEY"), SecretKey: os.Getenv("FOOTOPS_TEST_S3_SECRET")}
	o, err := s3ObjectOf(sec, os.Getenv("FOOTOPS_TEST_S3_NAME"))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := o.putIn(context.Background(), &http.Client{Timeout: 30 * time.Second}, []byte(`{"format":1,"deneme":true}`+"\n"), "application/json", time.Now()); err != nil {
		t.Fatal(maskRepo(repo, err.Error()))
	}
	t.Logf("PUT %s/%s %s (region %s)", o.bucket, o.key, time.Since(start).Round(time.Millisecond), o.region)
	// Read back with the same signing (the watcher's heartbeat is read this way).
	start = time.Now()
	data, err := o.get(context.Background(), &http.Client{Timeout: 30 * time.Second}, 1<<10, time.Now())
	if err != nil || string(data) != `{"format":1,"deneme":true}`+"\n" {
		t.Fatalf("GET: %q %v", data, err)
	}
	t.Logf("GET %s", time.Since(start).Round(time.Millisecond))
	missing := o
	missing.key += ".yok"
	if _, err := missing.get(context.Background(), &http.Client{Timeout: 30 * time.Second}, 1<<10, time.Now()); !errors.Is(err, errNoObject) {
		t.Fatalf("GET of a missing object: %v", err)
	}
	// The wrong secret is refused, and the error does not show it.
	o.secret = "wrong" + o.secret
	err = o.put(context.Background(), &http.Client{Timeout: 30 * time.Second}, []byte("x"), "text/plain", time.Now())
	if err == nil || strings.Contains(err.Error(), o.secret) {
		t.Fatalf("wrong secret: %v", err)
	}
	t.Logf("wrong secret: %s", maskRepo(repo, err.Error()))
}

// A server in another region says which: the write is tried once more there, and the
// region is kept for the next ones.
func TestStatusFileRegion(t *testing.T) {
	e := newBackups(t)
	s3 := &fakeS3{keyID: "KEYID12345", secret: "SeCrEt/Key+123=", region: "auto", objects: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:" + srv.URL + "/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"})
	w := &StatusWriter{Backups: e.b, Sites: e.b.Data.Store.List, Server: "T1", HTTP: srv.Client()}
	for i := 0; i < 2; i++ {
		if err := w.Write(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if s3.puts != 2 || w.region != "auto" || s3.objects["/footops-t1/izleme/durum.json"] == nil {
		t.Fatalf("puts %d, region %q", s3.puts, w.region)
	}
}

// A redirect is an answer, not a way: a redirected PUT would arrive as a GET without its
// body, and its 2xx would read as written.
func TestStatusFileNoRedirect(t *testing.T) {
	e := newBackups(t)
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			http.Redirect(w, r, "/baska-yer", http.StatusTemporaryRedirect)
			return
		}
		gets++
	}))
	defer srv.Close()
	saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:" + srv.URL + "/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"})
	w := &StatusWriter{Backups: e.b, Sites: e.b.Data.Store.List, Server: "T1", HTTP: srv.Client()}
	if err := w.Write(context.Background()); err == nil || !strings.Contains(err.Error(), "307") || gets != 0 {
		t.Fatalf("redirect: %v (%d GETs)", err, gets)
	}
}

// The outside watcher's heartbeat is read after each status file: none, fresh, old (an
// alert, to Telegram too, judged on fresh reads only and kept while reads fail),
// unreadable, garbage, a watcher in trouble.
func TestStatusFileWatcher(t *testing.T) {
	e := newBackups(t)
	s3 := &fakeS3{keyID: "KEYID12345", secret: "SeCrEt/Key+123=", objects: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:" + srv.URL + "/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"})
	w := &StatusWriter{Backups: e.b, Sites: e.b.Data.Store.List, Server: "T1", HTTP: srv.Client(), now: func() time.Time { return now }}
	ctx := context.Background()
	const key = "/footops-t1/izleme/nobetci.json"
	at := func(h, m int) time.Time { return time.Date(2026, 11, 7, h, m, 0, 0, time.UTC) }
	keys := func(al []Alert) (out []string) {
		for _, a := range al {
			out = append(out, a.Key)
		}
		return out
	}

	if err := w.Write(ctx); err != nil {
		t.Fatal(err)
	}
	if st := w.State(); st.Watcher != nil || !st.WatcherNone || st.WatcherErr != "" || s3.gets != 1 {
		t.Fatalf("no heartbeat: %+v (%d GETs)", st, s3.gets)
	}
	if al := w.Alerts(now.Add(5 * time.Minute)); len(al) != 0 {
		t.Fatalf("an alert without a watcher: %+v", al)
	}

	beat := `{"format":1,"time":"2026-11-07T11:58:00Z","every":300,"interval":120,"domains":[
		{"name":"magaza.example","site":"magaza","ok":true},
		{"name":"blog.example","site":"blog","ok":false,"down":true,"code":"502","reason":"502 — site yanıt vermiyor","since":"2026-11-07T11:40:00Z"},
		{"name":"kasa.example","site":"kasa","ok":true,"note":"cloudflare-challenge"}]}`
	s3.objects[key] = []byte(beat)
	if err := w.Write(ctx); err != nil {
		t.Fatal(err)
	}
	st := w.State()
	if st.Watcher == nil || st.WatcherNone || st.Watcher.Minutes() != 2 || len(st.Watcher.Domains) != 3 || st.Watcher.Trouble() != "" {
		t.Fatalf("heartbeat: %+v", st)
	}
	if bad := st.Watcher.Failing(); len(bad) != 1 || bad[0].Name != "blog.example" || !bad[0].Down || bad[0].Since.IsZero() {
		t.Fatalf("failing: %+v", bad)
	}
	if blind := st.Watcher.Blind(); len(blind) != 1 || blind[0].Name != "kasa.example" {
		t.Fatalf("blind: %+v", blind)
	}
	if al := w.Alerts(now); len(al) != 0 {
		t.Fatalf("fresh heartbeat, alert: %+v", al)
	}

	// 30 minutes after its last beat, read afresh: an alert that goes to Telegram.
	now = at(12, 28)
	w.Write(ctx)
	al := w.Alerts(now)
	if len(al) != 1 || al[0].Key != "monitor:watcher" || !Notifiable(al[0]) || !strings.Contains(al[0].Detail, "30 dk önce") || !al[0].Since.Equal(at(12, 28)) {
		t.Fatalf("old heartbeat: %+v", al)
	}
	// Reads refused: the last heartbeat, and its alert, stand, also once the reads are old.
	s3.refuseGet = true
	now = at(12, 33)
	w.Write(ctx)
	if st := w.State(); st.Watcher == nil || !strings.Contains(st.WatcherErr, "okumayı reddetti") {
		t.Fatalf("refused read: %+v", st)
	}
	if k := keys(w.Alerts(at(12, 50))); len(k) != 1 || k[0] != "monitor:watcher" {
		t.Fatalf("alert gone with a failed read: %v", k)
	}
	// A connection that fails says so without the address.
	srv.Close()
	now = at(12, 51)
	w.Write(ctx)
	if st := w.State(); strings.Contains(st.LastErr, "127.0.0.1") || st.LastErr == "" {
		t.Fatalf("connection error: %q", st.LastErr)
	}
	srv = httptest.NewServer(s3)
	defer srv.Close()
	saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:" + srv.URL + "/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"})
	w.HTTP = srv.Client()

	// A new heartbeat clears it; garbage is an error, not a heartbeat.
	s3.refuseGet = false
	now = at(13, 0)
	s3.objects[key] = []byte(strings.Replace(beat, "11:58", "12:59", 1))
	w.Write(ctx)
	if al := w.Alerts(now); len(al) != 0 {
		t.Fatalf("fresh again: %+v", al)
	}
	s3.objects[key] = []byte(`{"format":0}`)
	w.Write(ctx)
	if st := w.State(); st.Watcher == nil || st.WatcherErr != "biçimi tanınmadı" {
		t.Fatalf("garbage: %+v", st)
	}

	// A watcher that cannot send: an alert after 15 minutes, from when it began failing.
	trouble := strings.Replace(beat, `"interval":120,`, `"interval":120,"send_error":"Telegram reddetti (HTTP 403: Forbidden)","send_failing":"2026-11-07T12:50:00Z",`, 1)
	trouble = strings.Replace(trouble, "11:58", "12:59", 1)
	s3.objects[key] = []byte(trouble)
	w.Write(ctx)
	if k := keys(w.Alerts(now)); len(k) != 0 {
		t.Fatalf("trouble too soon: %v", k)
	}
	now = at(13, 6)
	w.Write(ctx)
	al = w.Alerts(now)
	if len(al) != 1 || al[0].Key != "monitor:watcher-trouble" || !Notifiable(al[0]) || !strings.Contains(al[0].Detail, "Telegram'a mesaj gönderemiyor") ||
		!al[0].Since.Equal(at(13, 5)) {
		t.Fatalf("trouble: %+v", al)
	}
	s3.objects[key] = []byte(strings.Replace(beat, "11:58", "13:06", 1))
	w.Write(ctx)
	if al := w.Alerts(now); len(al) != 0 {
		t.Fatalf("trouble over: %+v", al)
	}

	// The copy given out is the writer's own no more.
	st = w.State()
	st.Watcher.Domains[0].Name = "x"
	if w.State().Watcher.Domains[0].Name != "magaza.example" {
		t.Fatal("the state shares its heartbeat")
	}
	// Gone from the bucket: no watcher, no alert.
	delete(s3.objects, key)
	w.Write(ctx)
	if st := w.State(); st.Watcher != nil || !st.WatcherNone || len(w.Alerts(now.Add(time.Hour))) != 0 {
		t.Fatalf("removed: %+v", st)
	}
}

// What the watcher reads must hold no traps: a backup never saved is no time at all
// (Go would write year 1), and a site list that could not be read says so instead of
// claiming no sites.
func TestStatusFileForTheWatcher(t *testing.T) {
	e := newBackups(t)
	s3 := &fakeS3{keyID: "KEYID12345", secret: "SeCrEt/Key+123=", objects: map[string][]byte{}}
	srv := httptest.NewServer(s3)
	defer srv.Close()
	saveBackupSecret(e.b.SecretPath, backupSecret{Repository: "s3:" + srv.URL + "/footops-t1", KeyID: "KEYID12345", SecretKey: "SeCrEt/Key+123=",
		Password: "RepoPassword0123456789abcdefghijklmnop"})
	broken := errors.New("footops.json okunamadı")
	listErr := error(nil)
	w := &StatusWriter{Backups: e.b, Server: "T1", HTTP: srv.Client(),
		Sites: func() ([]sites.Spec, error) { return nil, listErr }}
	if err := w.Write(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := string(s3.objects["/footops-t1/izleme/durum.json"])
	if strings.Contains(body, "last_ok") || strings.Contains(body, "0001-01-01") || strings.Contains(body, "sites_error") {
		t.Fatalf("no backup yet:\n%s", body)
	}
	listErr = broken
	w.Write(context.Background())
	var got StatusFile
	json.Unmarshal(s3.objects["/footops-t1/izleme/durum.json"], &got)
	if got.SitesError != "footops.json okunamadı" || len(got.Sites) != 0 {
		t.Fatalf("site list error: %+v", got)
	}
}
