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
	"io/fs"
	"math/big"
	"net/url"
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
	status   string                         // overrides the reported status ("restarting")
	health   string                         // reported healthcheck status, if set
	restarts int
	image    string // image ID reported for site containers
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
	if _, ok := f.networks[network][container]; ok { // as dockerd answers
		return &docker.APIError{Status: 403, Message: "endpoint with name " + container + " already exists in network " + network}
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
	if len(f.networks[name]) > 0 { // as dockerd answers
		return &docker.APIError{Status: 403, Message: "error while removing network: network " + name + " has active endpoints"}
	}
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
	in.Image = f.image
	in.State.Status = "running"
	if !f.running {
		in.State.Status = "exited"
	}
	if f.status != "" {
		in.State.Status = f.status
	}
	if f.health != "" {
		in.State.Health = &struct {
			Status string `json:"Status"`
		}{f.health}
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
	return out, nil
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

// fakeUsers plays MongoDB's user management for site operations.
type fakeUsers struct {
	mu    sync.Mutex
	users map[string]string // "db user" -> password
	calls []string
	fail  error
}

func (f *fakeUsers) rec(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.fail
}

func (f *fakeUsers) Ping(context.Context) error { return f.rec("ping") }

func (f *fakeUsers) SetUser(_ context.Context, db, user, pw string) error {
	if err := f.rec("set " + db + " " + user); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[db+" "+user] = pw
	return nil
}

func (f *fakeUsers) DropUser(_ context.Context, db, user string) error {
	if err := f.rec("drop " + db + " " + user); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.users, db+" "+user)
	return nil
}

func (f *fakeUsers) DropTemp(_ context.Context, db string) error { return f.rec("droptemp " + db) }
func (f *fakeUsers) DropTemps(context.Context) (int, error)      { return 0, f.rec("droptemps") }
func (f *fakeUsers) Stats(context.Context, dbCreds) (dbStats, error) {
	return dbStats{}, f.rec("stats")
}
func (f *fakeUsers) Counts(context.Context, dbCreds) (map[string]int64, []string, error) {
	return map[string]int64{}, nil, f.rec("counts")
}
func (f *fakeUsers) DropCollections(context.Context, dbCreds, []string) error {
	return f.rec("dropcolls")
}

type opsEnv struct {
	ops   *SiteOps
	net   *fakeNet
	run   *cmdRunner
	users *fakeUsers
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
		"--entrypoint id ghcr.io/swbilisim/enisparta:main": fmt.Sprintf("uid=%d(app) gid=%d(app) groups=%d(app)", uid, gid, gid),
	}}
	e := &opsEnv{net: n, run: r, users: &fakeUsers{users: map[string]string{}}, root: root}
	e.ops = &SiteOps{
		Store: sites.Store{Root: root}, Docker: n, Run: r, Mongo: e.users, PanelHost: "panel-h1.example.com",
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
	// No command sees the root password; the site's user gets the password of its URI.
	for i, c := range e.run.calls {
		if strings.Contains(c, "RootSecret42") || strings.Contains(e.run.stdins[i], "RootSecret42") {
			t.Errorf("root password given to a command: %s", c)
		}
	}
	env, _ := e.ops.Store.Env(specFor("enisparta"))
	u, err := url.Parse(env["MONGODB_URI"])
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw == "" || e.users.users["enisparta enisparta"] != pw {
		t.Errorf("database user %v, URI %s", e.users.users, env["MONGODB_URI"])
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
	if len(e.users.users) != 0 || !contains(e.users.calls, "drop enisparta enisparta") {
		t.Errorf("database user not dropped: %v", e.users.calls)
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

// After an update the site blocks on disk are the previous version's rendering. Once
// Caddy runs, SyncCaddy rewrites them; when Caddy refuses, the old files go back.
func TestSyncCaddy(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(ctx, specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")
	current := readFile(t, f)
	old := "# Managed by FootOps: site enisparta (musteri)\nhttps://enisparta.com {\n\treverse_proxy enisparta:3000\n}\n"
	if err := os.WriteFile(f, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	// Caddy not up yet (boot; restarting reports Running too; admin endpoint not
	// answering yet): nothing is touched, the caller tries again.
	calls := len(e.run.calls)
	for _, down := range []func(){
		func() { e.net.running = false },
		func() { e.net.status = "restarting" },
		func() { e.net.health = "starting" },
	} {
		down()
		if done, err := e.ops.SyncCaddy(ctx); done || err != nil || readFile(t, f) != old || len(e.run.calls) != calls {
			t.Fatalf("Caddy not up: done=%v err=%v calls=%v", done, err, e.run.calls[calls:])
		}
		e.net.running, e.net.status, e.net.health = true, "", ""
	}
	e.net.health = "healthy"

	// Caddy refuses the configuration, or the reload: the old block is back.
	for _, verb := range []string{"caddy validate", "caddy reload"} {
		e.run.fail[verb] = errors.New("exit status 1")
		if done, err := e.ops.SyncCaddy(ctx); !done || err == nil || readFile(t, f) != old {
			t.Errorf("%s fails: done=%v err=%v\n%s", verb, done, err, readFile(t, f))
		}
		delete(e.run.fail, verb)
	}

	// Accepted: the block is this version's, validated before the reload.
	validates, reloads := e.run.ran("caddy validate"), e.run.ran("caddy reload")
	if done, err := e.ops.SyncCaddy(ctx); !done || err != nil || readFile(t, f) != current {
		t.Fatalf("done=%v err=%v\n%s", done, err, readFile(t, f))
	}
	if e.run.ran("caddy validate") != validates+1 || e.run.ran("caddy reload") != reloads+1 {
		t.Error("not validated and reloaded once")
	}
	// Up to date: no Caddy command at all.
	calls = len(e.run.calls)
	if done, err := e.ops.SyncCaddy(ctx); !done || err != nil || len(e.run.calls) != calls {
		t.Errorf("up to date: done=%v err=%v calls=%v", done, err, e.run.calls[calls:])
	}
	// A site without a block (a removal stopped halfway) stays offline.
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if done, err := e.ops.SyncCaddy(ctx); !done || err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); !errors.Is(err, fs.ErrNotExist) {
		t.Error("block of a site without one written back")
	}
}

// Removing a certificate moves the blocks to the remaining one before its files go; when
// Caddy refuses that, nothing changes, so no block ever names a deleted file.
func TestRemoveCertWhileAnotherCovers(t *testing.T) {
	e := newOps(t)
	if err := e.ops.Create(ctx, specFor("enisparta"), nil, e.progress); err != nil {
		t.Fatal(err)
	}
	crt, key := certPair(t, "enisparta.com", "*.enisparta.com") // also covers the site
	if _, err := e.ops.InstallCert(ctx, crt, key); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")
	block := readFile(t, f)
	certs, _ := e.ops.Store.Certs()
	var used sites.Cert
	for _, c := range certs {
		if strings.Contains(block, c.ID) {
			used = c
		}
	}
	if len(certs) != 2 || used.ID == "" {
		t.Fatalf("block names no certificate:\n%s", block)
	}

	e.run.fail["caddy validate"] = errors.New("exit status 1")
	if err := e.ops.RemoveCert(ctx, used.ID); err == nil {
		t.Fatal("removal succeeded although Caddy refused")
	}
	if readFile(t, f) != block {
		t.Errorf("block changed:\n%s", readFile(t, f))
	}
	if _, err := os.Stat(filepath.Join(e.root, used.HostCertPath())); err != nil {
		t.Errorf("certificate deleted although its blocks stayed: %v", err)
	}
	delete(e.run.fail, "caddy validate")

	if err := e.ops.RemoveCert(ctx, used.ID); err != nil {
		t.Fatal(err)
	}
	if now := readFile(t, f); strings.Contains(now, used.ID) || now == block {
		t.Errorf("block still names the removed certificate:\n%s", now)
	}
	if _, err := os.Stat(filepath.Join(e.root, used.HostCertPath())); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("certificate files left: %v", err)
	}
}

func TestCaddyResyncPacing(t *testing.T) {
	var r caddyResync
	now := time.Now()
	if !r.due(now) {
		t.Fatal("not due at start")
	}
	r.result(false, nil, now) // Caddy not up: next tick
	if !r.due(now) {
		t.Error("not due while Caddy is not up")
	}
	fail := errors.New("refused")
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		r.result(true, fail, now)
		if r.due(now.Add(want-time.Second)) || !r.due(now.Add(want)) {
			t.Errorf("after a failure: due again after %v, wait %v", want, r.wait)
		}
	}
	for i := 0; i < 10; i++ {
		r.result(true, fail, now)
	}
	if r.wait != time.Hour {
		t.Errorf("wait grows to %v", r.wait)
	}
	r.result(true, nil, now)
	if r.due(now.Add(48 * time.Hour)) {
		t.Error("due again after success")
	}
}

func TestMongoErrorSurfaces(t *testing.T) {
	e := newOps(t)
	e.users.fail = errors.New("MongoDB girişi reddedildi (kullanıcı ya da şifre yanlış)")
	err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress)
	if err == nil || !strings.Contains(err.Error(), "MongoDB kullanıcısı oluşturulamadı: MongoDB girişi reddedildi") {
		t.Fatalf("err = %v", err)
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
