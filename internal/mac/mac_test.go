package mac

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/s3obj"
)

// Made-up values throughout: no real account, key, domain or address.
const (
	testAccount = "0123456789abcdef0123456789abcdef"
	testKeyID   = "MACKEYID0001"
	testSecret  = "Mac/Secret+Key="
)

// fakeKeychain is the Keychain's service as a map.
type fakeKeychain struct {
	items map[string]string
	err   error // every call fails so
	// cmd prints an item named by its last argument (Command); none: one that fails.
	cmd []string
}

func (k fakeKeychain) Command(name string) []string {
	if len(k.cmd) == 0 {
		return []string{"/usr/bin/false", name}
	}
	return append(append([]string{}, k.cmd...), name)
}

func (k fakeKeychain) Get(_ context.Context, name string) (string, error) {
	if k.err != nil {
		return "", k.err
	}
	v, ok := k.items[name]
	if !ok {
		return "", fmt.Errorf("footops-yedek / %s: %w", name, errNoItem)
	}
	return v, nil
}

func (k fakeKeychain) Names(context.Context) ([]string, error) {
	if k.err != nil {
		return nil, k.err
	}
	var out []string
	for n := range k.items {
		out = append(out, n)
	}
	return out, nil
}

// fakeR2 serves objects by path to requests signed with the Mac's key (in the region
// the request names: s3obj picks it), GETs only, each object with its own Last-Modified.
type fakeR2 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	modified map[string]time.Time
	status   int  // answer every request with this (an S3 error), when set
	cut      bool // cut every answer short
	gets     []string
}

func (f *fakeR2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, r.URL.Path)
	sum := sha256.Sum256(nil)
	payload := hex.EncodeToString(sum[:])
	region := "?"
	if _, scope, ok := strings.Cut(r.Header.Get("Authorization"), "Credential="+testKeyID+"/"); ok {
		if parts := strings.Split(scope, "/"); len(parts) > 1 {
			region = parts[1]
		}
	}
	want := s3obj.Sign(r.Method, r.URL.EscapedPath(), "", [][2]string{{"host", r.Host}, {"x-amz-content-sha256", payload},
		{"x-amz-date", r.Header.Get("X-Amz-Date")}}, payload, r.Header.Get("X-Amz-Date"), region, testKeyID, testSecret)
	if r.Method != http.MethodGet || r.Header.Get("Authorization") != want {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>SignatureDoesNotMatch</Code><Message>bad signature</Message></Error>`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
		return
	}
	if f.cut {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, `{"format":`)
		return
	}
	data, ok := f.objects[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `<?xml version="1.0"?><Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>`)
		return
	}
	if t := f.modified[r.URL.Path]; !t.IsZero() {
		w.Header().Set("Last-Modified", t.UTC().Format(http.TimeFormat))
	}
	w.Write(data)
}

func (f *fakeR2) put(path string, v any, modified time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, _ := json.Marshal(v)
	if s, ok := v.(string); ok {
		data = []byte(s)
	}
	f.objects[path], f.modified[path] = data, modified
}

// testEnv is an env on the fakes: the Keychain with two servers (h1's bucket from a
// kova item, with a folder), R2 at srv, the clock at now.
func testEnv(t *testing.T, r2 *fakeR2, now time.Time) (*env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(r2)
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	e := &env{out: &out, errw: &errb,
		keychain: fakeKeychain{items: map[string]string{"r2-hesap": testAccount, "r2-mac-erisim": testKeyID, "r2-mac-gizli": testSecret,
			"restic-h1": "RepoPassword-h1", "restic-a": "RepoPassword-a", "kova-h1": "footops-h1/kasa", "r2-test-erisim": "x"}},
		settings: filepath.Join(t.TempDir(), "ayarlar.json"),
		r2:       func(account string) string { return srv.URL },
		client:   srv.Client(), probeTimeout: 300 * time.Millisecond, now: func() time.Time { return now }}
	return e, &out, &errb
}

var testNow = time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)

func writeSettings(t *testing.T, e *env, s string) {
	t.Helper()
	if err := os.WriteFile(e.settings, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The commands as the Terminal and the app call them: the usage, an unknown command or
// option, the version.
func TestRunUsage(t *testing.T) {
	e, out, errb := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	ctx := context.Background()
	for _, c := range []struct {
		args []string
		code int
		in   string // in stdout+stderr
	}{
		{nil, 2, "Kullanım: footops-mac"},
		{[]string{"yok-boyle"}, 2, "bilinmeyen komut: yok-boyle"},
		{[]string{"durum", "--hizli"}, 2, "bilinmeyen seçenek: --hizli"},
		{[]string{"durum", "fazla"}, 2, "Kullanım:"},
		{[]string{"surum"}, 0, "footops-mac "},
		{[]string{"yardim"}, 0, "yokla [sunucu…]"},
	} {
		out.Reset()
		errb.Reset()
		if code := e.run(ctx, c.args); code != c.code || !strings.Contains(out.String()+errb.String(), c.in) {
			t.Errorf("%v: %d\n%s%s", c.args, code, out, errb)
		}
	}
}

// The folder's attribute is Time Machine's sticky exclusion, as tmutil sees it.
func TestTimeMachineExclude(t *testing.T) {
	const tmutil = "/usr/bin/tmutil"
	if _, err := os.Stat(tmutil); err != nil {
		t.Skip("tmutil yok")
	}
	if len(tmExcluded) != 61 {
		t.Fatalf("%d bytes", len(tmExcluded))
	}
	// In the home folder: the system's temporary folders are left out of Time Machine
	// whatever their attributes.
	home, err := os.MkdirTemp(os.Getenv("HOME"), ".footops-tm-test-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(home)
	dir, other := filepath.Join(home, "acilan"), filepath.Join(home, "baska")
	os.Mkdir(dir, 0o700)
	os.Mkdir(other, 0o700)
	timeMachineExclude(dir)
	out, err := exec.Command(tmutil, "isexcluded", dir, other).Output()
	if err != nil || !strings.Contains(string(out), "[Excluded]") || !strings.Contains(string(out), "[Included]") ||
		strings.Index(string(out), "[Excluded]") > strings.Index(string(out), "[Included]") {
		t.Errorf("%v %s", err, out)
	}
}
