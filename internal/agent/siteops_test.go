package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

// ---- fakes ----

type fakeNet struct {
	mu       sync.Mutex
	networks map[string]map[string][]string // network -> container -> aliases
	running  bool                           // state reported for site containers
	restarts int
	log      []string
}

func newFakeNet() *fakeNet {
	return &fakeNet{networks: map[string]map[string][]string{}, running: true}
}

func (f *fakeNet) rec(s string) { f.log = append(f.log, s) }

func (f *fakeNet) CreateNetwork(_ context.Context, name string, _ map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[name]; !ok {
		f.networks[name] = map[string][]string{}
		f.rec("create " + name)
	}
	return nil
}

func (f *fakeNet) NetworkMembers(_ context.Context, name string) (map[string]bool, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return nil, false, nil
	}
	m := map[string]bool{}
	for c := range n {
		m[c] = true
	}
	return m, true, nil
}

func (f *fakeNet) Connect(_ context.Context, network, container string, aliases ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[network]; !ok {
		return &docker.APIError{Status: 404, Message: "network not found"}
	}
	f.networks[network][container] = aliases
	f.rec("connect " + network + " " + container + " " + strings.Join(aliases, ","))
	return nil
}

func (f *fakeNet) Disconnect(_ context.Context, network, container string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks[network], container)
	f.rec("disconnect " + network + " " + container)
	return nil
}

func (f *fakeNet) RemoveNetwork(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.networks, name)
	f.rec("remove " + name)
	return nil
}

func (f *fakeNet) ContainerIP(_ context.Context, container, network string) (string, error) {
	return "10.200.1.2", nil
}

func (f *fakeNet) Inspect(_ context.Context, name string) (docker.Inspect, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in docker.Inspect
	in.State.Running = f.running
	in.RestartCount = f.restarts
	if !f.running {
		in.State.Status = "exited"
	}
	return in, nil
}

func (f *fakeNet) Logs(context.Context, string, int) ([]string, error) {
	return []string{"Error: Cannot find module 'express'"}, nil
}

type cmdRunner struct {
	mu     sync.Mutex
	calls  []string
	stdins []string
	fail   map[string]error // command prefix -> error
	out    map[string]string
	hook   func(cmd string) // called for every command
}

func (r *cmdRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	return r.RunInput(ctx, "", name, args...)
}

func (r *cmdRunner) RunInput(_ context.Context, stdin, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	if r.hook != nil {
		r.hook(cmd)
	}
	r.stdins = append(r.stdins, stdin)
	out := ""
	for p, o := range r.out {
		if strings.Contains(cmd, p) {
			out = o
		}
	}
	for p, err := range r.fail {
		if strings.Contains(cmd, p) {
			return out, err // like a real command: output and an exit status
		}
	}
	if out != "" {
		return out, nil
	}
	if strings.Contains(cmd, "mongosh") {
		return "FOOTOPS_OK\n", nil
	}
	return "", nil
}

func (r *cmdRunner) ran(sub string) int {
	n := 0
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func certPair(t *testing.T, names ...string) (string, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}

type opsEnv struct {
	ops   *SiteOps
	net   *fakeNet
	run   *cmdRunner
	root  string
	steps []string
}

func newOps(t *testing.T) *opsEnv {
	t.Helper()
	root := t.TempDir()
	n := newFakeNet()
	n.networks = map[string]map[string][]string{}
	uid, gid := os.Getuid(), os.Getgid()
	r := &cmdRunner{fail: map[string]error{}, out: map[string]string{
		"--entrypoint id ghcr.io/swbilisim/enisparta:main -u": fmt.Sprint(uid),
		"--entrypoint id ghcr.io/swbilisim/enisparta:main -g": fmt.Sprint(gid),
	}}
	e := &opsEnv{net: n, run: r, root: root}
	e.ops = &SiteOps{
		Store: sites.Store{Root: root}, Docker: n, Run: r, PanelHost: "panel-h1.example.com",
		dial:  func(context.Context, string) error { return nil },
		https: func(context.Context, string) (int, error) { return 200, nil },
		sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
	}
	os.MkdirAll(filepath.Join(root, core.CaddySitesDir), 0o755)
	os.MkdirAll(filepath.Join(root, filepath.Dir(core.MongoSecretPath)), 0o700)
	os.WriteFile(filepath.Join(root, core.MongoSecretPath), []byte(core.MongoSecretEnv("RootSecret42")), 0o600)
	crt, key := certPair(t, "enisparta.com", "*.enisparta.com")
	if _, err := e.ops.InstallCert(context.Background(), crt, key); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *opsEnv) progress(s string) { e.steps = append(e.steps, s) }

func specFor(name string) sites.Spec {
	return sites.Spec{Name: name, Group: "musteri", Domains: []string{"enisparta.com", "www.enisparta.com"},
		Image: "ghcr.io/swbilisim/enisparta:main", Port: 3000, MemoryMB: 256, DataMount: "/app/uploads", Database: true}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- tests ----

func TestCreateSite(t *testing.T) {
	e := newOps(t)
	err := e.ops.Create(context.Background(), specFor("enisparta"), map[string]string{"NODE_ENV": "production"}, e.progress)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	dir := filepath.Join(e.root, "/srv/musteri/enisparta")

	compose := readFile(t, filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(compose, `MONGODB_URI: "mongodb://enisparta:`) || !strings.Contains(compose, "@mongo:27017/enisparta?authSource=enisparta") {
		t.Errorf("database URI missing:\n%s", compose)
	}
	if st, _ := os.Stat(filepath.Join(dir, "compose.yaml")); st.Mode().Perm() != 0o600 {
		t.Errorf("compose.yaml mode %v", st.Mode().Perm())
	}
	meta := readFile(t, filepath.Join(dir, "footops.json"))
	if strings.Contains(meta, "mongodb://") || !strings.Contains(meta, `"NODE_ENV"`) {
		t.Errorf("meta must list env keys but no secrets:\n%s", meta)
	}
	caddy := readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"))
	if !strings.Contains(caddy, "https://enisparta.com, https://www.enisparta.com {") || !strings.Contains(caddy, "reverse_proxy enisparta:3000") {
		t.Errorf("caddy block:\n%s", caddy)
	}
	if _, err := os.Stat(filepath.Join(dir, "data")); err != nil {
		t.Error("data dir not created")
	}

	members := e.net.networks["prj_musteri_enisparta"]
	if _, ok := members["footops-caddy"]; !ok {
		t.Error("caddy not attached to the site network")
	}
	if a := members["footops-mongo"]; len(a) != 1 || a[0] != "mongo" {
		t.Errorf("mongo aliases = %v", a)
	}

	// Order: image pulled before start; Caddy validated before reload.
	idx := func(sub string) int {
		for i, c := range e.run.calls {
			if strings.Contains(c, sub) {
				return i
			}
		}
		return -1
	}
	if !(idx(" pull ") < idx(" up ") && idx("caddy validate") < idx("caddy reload")) {
		t.Errorf("command order:\n%s", strings.Join(e.run.calls, "\n"))
	}
	// The root password goes through stdin only.
	for i, c := range e.run.calls {
		if strings.Contains(c, "RootSecret42") {
			t.Errorf("root password in argv: %s", c)
		}
		if strings.Contains(c, "mongosh") && !strings.Contains(e.run.stdins[i], "RootSecret42") {
			t.Error("mongosh did not get the root password via stdin")
		}
	}
	if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "Site hazır") {
		t.Errorf("last step = %q", last)
	}
}

func assertRolledBack(t *testing.T, e *opsEnv) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.root, "/srv/musteri/enisparta")); !errors.Is(err, os.ErrNotExist) {
		t.Error("site directory left behind")
	}
	if _, ok := e.net.networks["prj_musteri_enisparta"]; ok {
		t.Error("network left behind")
	}
	if _, err := os.Stat(filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")); !errors.Is(err, os.ErrNotExist) {
		t.Error("caddy block left behind")
	}
	dropped := false
	for i, c := range e.run.calls {
		if strings.Contains(c, "mongosh") && strings.Contains(e.run.stdins[i], "dropUser") {
			dropped = true
		}
	}
	if !dropped {
		t.Error("database user not dropped")
	}
	if list, _ := e.ops.Store.List(); len(list) != 0 {
		t.Errorf("site still listed: %+v", list)
	}
}

func TestCreateRollsBackWhenAppCrashes(t *testing.T) {
	e := newOps(t)
	e.net.running = false
	err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress)
	if err == nil || !strings.Contains(err.Error(), "Cannot find module") {
		t.Fatalf("err = %v (should include the app's last log lines)", err)
	}
	if e.run.ran("compose --project-name enisparta --file /srv/musteri/enisparta/compose.yaml down") != 1 {
		t.Errorf("container not removed:\n%s", strings.Join(e.run.calls, "\n"))
	}
	assertRolledBack(t, e)
}

func TestCreateRollsBackOnInvalidCaddyConfig(t *testing.T) {
	e := newOps(t)
	e.run.fail["caddy validate"] = errors.New("exit status 1")
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err == nil {
		t.Fatal("expected error")
	}
	if e.run.ran("caddy reload") > 1 { // only the undo reload, never with the bad file
		t.Errorf("reloaded an invalid config:\n%s", strings.Join(e.run.calls, "\n"))
	}
	assertRolledBack(t, e)
}

func TestCreateRollsBackWhenSiteUnreachable(t *testing.T) {
	e := newOps(t)
	e.ops.https = func(context.Context, string) (int, error) { return 502, nil }
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err == nil {
		t.Fatal("expected error")
	}
	assertRolledBack(t, e)
}

func TestCheck(t *testing.T) {
	e := newOps(t)
	s := specFor("other")
	s.Domains = []string{"other.com", "panel-h1.example.com"}
	_, err := e.ops.Check(s, nil)
	var ve *sites.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v", err)
	}
	joined := strings.Join(ve.Problems, "\n")
	if !strings.Contains(joined, "sertifika yok") || !strings.Contains(joined, "panelin adresi") {
		t.Errorf("problems:\n%s", joined)
	}
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	dup := specFor("enisparta2")
	if _, err := e.ops.Check(dup, nil); err == nil || !strings.Contains(err.Error(), "zaten enisparta sitesinde") {
		t.Errorf("domain conflict: %v", err)
	}
}

func TestRemoveKeepsData(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, "/srv/musteri/enisparta/data/upload.jpg"), []byte("x"), 0o644)
	if err := e.ops.Remove(context.Background(), "enisparta", e.progress); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.ops.Store.List(); len(list) != 0 {
		t.Error("removed site still listed")
	}
	matches, _ := filepath.Glob(filepath.Join(e.root, "/srv/musteri/_removed/enisparta-*/data/upload.jpg"))
	if len(matches) != 1 {
		t.Error("data not kept under _removed")
	}
	if _, ok := e.net.networks["prj_musteri_enisparta"]; ok {
		t.Error("network not removed")
	}
	if _, err := os.Stat(filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")); !errors.Is(err, os.ErrNotExist) {
		t.Error("caddy block not removed")
	}
	// The name is free again.
	if _, err := e.ops.Check(specFor("enisparta"), nil); err != nil {
		t.Errorf("name not reusable after removal: %v", err)
	}
}

func TestReconcileReattaches(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	// Recreating the core containers drops their extra network attachments.
	delete(e.net.networks["prj_musteri_enisparta"], "footops-caddy")
	delete(e.net.networks["prj_musteri_enisparta"], "footops-mongo")
	if errs := e.ops.Reconcile(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	m := e.net.networks["prj_musteri_enisparta"]
	if _, ok := m["footops-caddy"]; !ok {
		t.Error("caddy not reattached")
	}
	if a := m["footops-mongo"]; len(a) != 1 || a[0] != "mongo" {
		t.Errorf("mongo not reattached with alias: %v", a)
	}
}

func TestCertRenewalUpdatesSites(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"))
	reloads := e.run.ran("caddy reload")

	// A renewed certificate expires later, so it is preferred.
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "enisparta.com"},
		DNSNames: []string{"enisparta.com", "*.enisparta.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(15 * 365 * 24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	c, err := e.ops.InstallCert(context.Background(),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})))
	if err != nil {
		t.Fatal(err)
	}
	after := readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy"))
	if after == before || !strings.Contains(after, c.ID) {
		t.Errorf("site not moved to the renewed certificate:\n%s", after)
	}
	if e.run.ran("caddy reload") != reloads+1 {
		t.Error("caddy not reloaded after renewal")
	}

	// The old certificate is now unused and can go; the new one cannot.
	certs, _ := e.ops.Store.Certs()
	for _, old := range certs {
		if old.ID != c.ID {
			if err := e.ops.RemoveCert(context.Background(), old.ID); err != nil {
				t.Errorf("unused cert: %v", err)
			}
		}
	}
	if err := e.ops.RemoveCert(context.Background(), c.ID); err == nil || !strings.Contains(err.Error(), "enisparta") {
		t.Errorf("removing the cert a site uses: %v", err)
	}
}

func TestMongoErrorSurfaces(t *testing.T) {
	e := newOps(t)
	e.run.out["mongosh"] = "FOOTOPS_ERR Authentication failed.\n"
	e.run.fail["mongosh"] = errors.New("exit status 3")
	err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress)
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestJobsAreExclusive(t *testing.T) {
	j := NewJobs()
	release := make(chan struct{})
	finished := make(chan Job, 1)
	id, err := j.Start("site.create", "a", Actor{User: "furkan"}, func(ctx context.Context, p Progress) error {
		p("step one")
		<-release
		return errors.New("failed")
	}, func(job Job) { finished <- job })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Start("site.remove", "b", Actor{}, func(context.Context, Progress) error { return nil }, nil); !errors.Is(err, ErrBusy) {
		t.Errorf("second job: %v", err)
	}
	close(release)
	job := <-finished
	if !job.Done || job.Err != "failed" || len(job.Steps) != 1 {
		t.Errorf("job = %+v", job)
	}
	if got, ok := j.Get(id); !ok || !got.Done {
		t.Error("job not retrievable")
	}
	if _, err := j.Start("site.remove", "b", Actor{}, func(context.Context, Progress) error { return nil }, nil); err != nil {
		t.Errorf("not free after finishing: %v", err)
	}
}

func TestRandomSecret(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := randomSecret(32)
		if err != nil || len(s) != 32 || strings.Trim(s, secretAlphabet) != "" || seen[s] {
			t.Fatalf("bad secret %q %v", s, err)
		}
		seen[s] = true
	}
}
