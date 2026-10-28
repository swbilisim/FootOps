package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/core"
	"github.com/swbilisim/FootOps/internal/sites"
)

const nextInspect = `[{
  "Id": "sha256:4f2a9c1b7d3e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4",
  "RepoDigests": ["ghcr.io/ornek/site@sha256:abcdef0123"],
  "Created": "2026-10-27T12:30:00.123456789Z",
  "Size": 328000000,
  "Os": "linux", "Architecture": "%s",
  "Config": {
    "User": "nextjs",
    "ExposedPorts": {"3000/tcp": {}, "9229/udp": {}},
    "Env": ["PATH=/usr/local/bin", "NODE_ENV=production", "PORT=3000", "HOSTNAME=0.0.0.0", "API_SECRET=s3cr3t-value"],
    "Volumes": {"/app/uploads": {}},
    "WorkingDir": "/app",
    "Entrypoint": ["docker-entrypoint.sh"],
    "Cmd": ["node", "server.js"],
    "Healthcheck": {"Test": ["CMD", "wget", "-q", "http://127.0.0.1:3000/"]},
    "Labels": {"org.opencontainers.image.source": "https://github.com/ornek/site"}
  }
}]`

func inspectFor(arch string) string { return fmt.Sprintf(nextInspect, arch) }

func TestParseImageInspect(t *testing.T) {
	info, err := parseImageInspect(inspectFor("amd64"), "ghcr.io/ornek/site:main")
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != "4f2a9c1b7d3e" || info.Platform != "linux/amd64" || info.User != "nextjs" || info.UID != -1 || info.Size != 328000000 {
		t.Errorf("info = %+v", info)
	}
	if fmt.Sprint(info.Ports) != "[3000]" || fmt.Sprint(info.Volumes) != "[/app/uploads]" || info.EnvPort != "3000" || info.EnvHostname != "0.0.0.0" {
		t.Errorf("ports %v volumes %v env %q %q", info.Ports, info.Volumes, info.EnvPort, info.EnvHostname)
	}
	if info.SuggestPort() != 3000 || info.SuggestMount() != "/app/uploads" {
		t.Errorf("suggest %d %q", info.SuggestPort(), info.SuggestMount())
	}
	// Of the image's environment only PORT and HOSTNAME reach the panel; of its command
	// line (a place for baked-in tokens) nothing.
	b, _ := json.Marshal(info)
	if strings.Contains(string(b), "s3cr3t") || strings.Contains(string(b), "API_SECRET") || strings.Contains(string(b), "server.js") {
		t.Errorf("result: %s", b)
	}
	// Whatever an image says, the result stays small.
	many := `[{"Id":"sha256:1","Os":"linux","Architecture":"amd64","Config":{"User":"` + strings.Repeat("u", 5000) + `","ExposedPorts":{`
	for i := 1; i <= 100; i++ {
		if i > 1 {
			many += ","
		}
		many += fmt.Sprintf(`"%d/tcp":{}`, 1000+i)
	}
	many += `},"Volumes":{"/` + strings.Repeat("v", 5000) + `":{}}}}]`
	big, err := parseImageInspect(many, "x")
	if b, _ := json.Marshal(big); err != nil || len(b) > 2000 || len(big.Ports) != maxInfoList {
		t.Errorf("bounded result: %d bytes, %d ports, %v", len(b), len(big.Ports), err)
	}
	info.Server = "linux/amd64"
	if !info.PlatformOK() {
		t.Error("same platform refused")
	}
	info.Platform = "linux/arm64"
	if info.PlatformOK() {
		t.Error("arm64 image accepted on amd64")
	}
	if _, err := parseImageInspect("[]", "x"); err == nil {
		t.Error("empty inspect accepted")
	}
}

func TestSuggestPort(t *testing.T) {
	for _, c := range []struct {
		ports []int
		env   string
		want  int
	}{
		{nil, "", 0}, {nil, "8000", 8000}, {nil, "x", 0}, {[]int{5000}, "3000", 5000},
		{[]int{443, 8080}, "", 8080}, {[]int{4000, 9000}, "", 4000}, {[]int{4000, 9000}, "9000", 9000},
	} {
		if got := (ImageInfo{Ports: c.ports, EnvPort: c.env}).SuggestPort(); got != c.want {
			t.Errorf("%v %q: %d, want %d", c.ports, c.env, got, c.want)
		}
	}
}

func TestExplainPull(t *testing.T) {
	for msg, want := range map[string]error{
		"Error response from daemon: Head \"https://ghcr.io/v2/x/y/manifests/main\": unauthorized": errImageAuth,
		"Error response from daemon: denied: requested access to the resource is denied":           errImageAuth,
		"Error response from daemon: manifest unknown":                                             errImageNotFound,
		"Error response from daemon: pull access denied for x, repository does not exist":          errImageAuth,
		// What dockerd says for a private image without a login (seen in the integration test).
		`Error response from daemon: failed to resolve reference "localhost:5000/itapp:1": pull access denied, repository does not exist or may require authorization: authorization failed: no basic auth credentials`: errImageAuth,
		"Error response from daemon: localhost:5000/itapp:2: not found":                                   errImageNotFound,
		"Error response from daemon: Get \"https://ghcr.io/v2/\": dial tcp: lookup ghcr.io: no such host": errImageNetwork,
	} {
		err := explainPull(errors.New("docker image pull: exit status 1: " + msg))
		if !errors.Is(err, want) {
			t.Errorf("%q → %v", msg, err)
		}
	}
	if err := explainPull(errors.New("no matching manifest for linux/amd64 in the manifest list entries")); !strings.Contains(err.Error(), "mimarisi") {
		t.Errorf("platform: %v", err)
	}
	other := errors.New("something else")
	if explainPull(other) != other || explainPull(nil) != nil {
		t.Error("unknown errors must pass through")
	}
}

func TestCheckImage(t *testing.T) {
	e := newOps(t)
	img := "ghcr.io/swbilisim/enisparta:main"
	e.run.out["image inspect "+img] = inspectFor(serverArch())
	task := &Task{j: NewJobs(), job: &Job{ID: "img1"}}
	info, err := e.ops.CheckImage(context.Background(), " "+img+" ", task)
	if err != nil {
		t.Fatalf("check: %v\n%s", err, strings.Join(task.job.Steps, "\n"))
	}
	if info.UID != os.Getuid() || info.GID != os.Getgid() || info.SuggestPort() != 3000 {
		t.Errorf("info = %+v", info)
	}
	if e.run.ran("docker image pull --quiet "+img) != 1 {
		t.Errorf("calls: %v", e.run.calls)
	}
	steps := strings.Join(task.job.Steps, "\n")
	for _, want := range []string{"Port önerisi: 3000 (EXPOSE)", "VOLUME", "Kullanıcı: nextjs"} {
		if !strings.Contains(steps, want) {
			t.Errorf("steps miss %q:\n%s", want, steps)
		}
	}

	// An image for another machine is refused with a way out.
	other := "arm64"
	if serverArch() == "arm64" {
		other = "amd64"
	}
	e.run.out["image inspect "+img] = inspectFor(other)
	if _, err := e.ops.CheckImage(context.Background(), img, task); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Errorf("foreign platform: %v", err)
	}
	// A pull that fails says why.
	e.run.fail["image pull"] = errors.New("exit status 1: unauthorized")
	if _, err := e.ops.CheckImage(context.Background(), img, task); !errors.Is(err, errImageAuth) {
		t.Errorf("pull failure: %v", err)
	}
	if _, err := e.ops.CheckImage(context.Background(), "bad image;rm", task); err == nil {
		t.Error("invalid reference accepted")
	}
}

func serverArch() string { return runtime.GOARCH }

// An image for another machine is refused before a container ever runs it.
func TestCreateRefusesForeignPlatform(t *testing.T) {
	e := newOps(t)
	arch := "arm64"
	if serverArch() == "arm64" {
		arch = "amd64"
	}
	e.run.out["image inspect ghcr.io/swbilisim/enisparta:main"] = inspectFor(arch)
	err := e.ops.Create(context.Background(), specFor("enisparta"), nil, e.progress)
	if err == nil || !strings.Contains(err.Error(), "platform") {
		t.Fatalf("err = %v", err)
	}
	if e.run.ran(" up ") != 0 {
		t.Error("container started anyway")
	}
	assertRolledBack(t, e)
}

// ---- registry logins ----

func newRegistries(t *testing.T) (*Registries, *cmdRunner) {
	t.Helper()
	dir := t.TempDir()
	r := &cmdRunner{fail: map[string]error{}, out: map[string]string{}}
	return &Registries{ConfigDir: filepath.Join(dir, "docker"), MetaPath: filepath.Join(dir, "registries.json"), Run: r,
		now: func() time.Time { return time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC) }}, r
}

func TestRegistriesListNeverLeaksTokens(t *testing.T) {
	reg, _ := newRegistries(t)
	if list, err := reg.List(); err != nil || len(list) != 0 {
		t.Fatalf("no config: %v %v", list, err)
	}
	os.MkdirAll(reg.ConfigDir, 0o700)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	cfg := fmt.Sprintf(`{"auths":{"ghcr.io":{"auth":%q},"https://index.docker.io/v1/":{"auth":%q},"registry.example.com:5000":{}}}`,
		b64("furkan:ghp_TOPSECRET"), b64("hub-user:dckr_pat_SECRET2"))
	os.WriteFile(filepath.Join(reg.ConfigDir, "config.json"), []byte(cfg), 0o600)
	list, err := reg.List()
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if list[0].Host != "docker.io" || list[0].User != "hub-user" || list[1].Host != "ghcr.io" || list[1].User != "furkan" || list[2].User != "" {
		t.Errorf("list = %+v", list)
	}
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "SECRET") {
		t.Errorf("token leaked: %s", b)
	}
}

func TestRegistryLogin(t *testing.T) {
	var gotAuth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-OAuth-Scopes", "read:packages, repo")
		w.Header().Set("GitHub-Authentication-Token-Expiration", "2026-11-05 10:00:00 UTC")
		fmt.Fprint(w, `{"login":"furkan"}`)
	}))
	defer gh.Close()
	reg, r := newRegistries(t)
	reg.GitHubAPI = gh.URL
	l, notes, err := reg.Login(context.Background(), " GHCR.io ", "furkan", " ghp_TOKEN123\n", "furkan")
	if err != nil {
		t.Fatal(err)
	}
	if l.Host != "ghcr.io" || !l.Expires.Equal(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)) || len(l.Scopes) != 2 {
		t.Errorf("login = %+v", l)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "repo") {
		t.Errorf("notes = %v", notes)
	}
	// The token goes to docker through stdin only, trimmed.
	if len(r.calls) != 1 || r.calls[0] != "docker login ghcr.io --username furkan --password-stdin" || r.stdins[0] != "ghp_TOKEN123" {
		t.Errorf("calls %q stdins %q", r.calls, r.stdins)
	}
	if gotAuth != "token ghp_TOKEN123" {
		t.Errorf("GitHub auth = %q", gotAuth)
	}
	meta, _ := os.ReadFile(reg.MetaPath)
	if strings.Contains(string(meta), "ghp_") || !strings.Contains(string(meta), "2026-11-05") {
		t.Errorf("meta = %s", meta)
	}
	if st, _ := os.Stat(reg.MetaPath); st.Mode().Perm() != 0o600 {
		t.Errorf("meta mode %v", st.Mode().Perm())
	}
	// Ten days left: an alert.
	alerts := tokenAlerts([]RegistryLogin{l}, reg.now())
	if len(alerts) != 1 || alerts[0].Level != "warn" || alerts[0].Link != "/registries" {
		t.Errorf("alerts = %+v", alerts)
	}

	// GitHub unreachable: the login still counts, with a note.
	gh.Close()
	_, notes, err = reg.Login(context.Background(), "ghcr.io", "furkan", "ghp_TOKEN123", "furkan")
	if err != nil || len(notes) != 1 || !strings.Contains(notes[0], "öğrenilemedi") {
		t.Errorf("offline GitHub: %v %v", notes, err)
	}
}

func TestRegistryLoginRejects(t *testing.T) {
	reg, r := newRegistries(t)
	for name, c := range map[string][3]string{
		"scheme":         {"https://ghcr.io", "u", "t"},
		"path":           {"ghcr.io/x", "u", "t"},
		"user":           {"ghcr.io", "a b", "t"},
		"empty token":    {"ghcr.io", "u", "   "},
		"spaced token":   {"ghcr.io", "u", "two words"},
		"option as user": {"ghcr.io", "--password=x", "t"},
	} {
		if _, _, err := reg.Login(context.Background(), c[0], c[1], c[2], "x"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("docker ran for invalid input: %v", r.calls)
	}
	r.fail["docker login"] = errors.New("exit status 1: Error response from daemon: Get \"https://ghcr.io/v2/\": denied: denied")
	_, _, err := reg.Login(context.Background(), "ghcr.io", "u", "t0ken", "x")
	if err == nil || !strings.Contains(err.Error(), "reddedildi") {
		t.Errorf("refused login: %v", err)
	}
	if _, err := os.Stat(reg.MetaPath); err == nil {
		t.Error("a refused login was recorded")
	}
}

func TestRegistryLogout(t *testing.T) {
	reg, r := newRegistries(t)
	os.MkdirAll(reg.ConfigDir, 0o700)
	cfgPath := filepath.Join(reg.ConfigDir, "config.json")
	os.WriteFile(cfgPath, []byte(`{"auths":{"ghcr.io":{"auth":"dTp0"},"https://index.docker.io/v1/":{"auth":"dTp0"}}}`), 0o600)
	reg.writeMeta(map[string]RegistryLogin{"ghcr.io": {Host: "ghcr.io", User: "u"}, "docker.io": {Host: "docker.io", User: "u"}})
	// docker removes the entry it is asked about; `docker logout` alone means Docker Hub.
	r.hook = func(cmd string) {
		key := map[string]string{"docker logout ghcr.io": "ghcr.io", "docker logout": "https://index.docker.io/v1/"}[cmd]
		var c map[string]map[string]any
		b, _ := os.ReadFile(cfgPath)
		json.Unmarshal(b, &c)
		delete(c["auths"], key)
		b, _ = json.Marshal(c)
		os.WriteFile(cfgPath, b, 0o600)
	}
	if err := reg.Logout(context.Background(), "quay.io"); err == nil {
		t.Error("unknown registry logged out")
	}
	for _, host := range []string{"ghcr.io", "docker.io"} {
		if err := reg.Logout(context.Background(), host); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
	}
	if r.ran("docker logout ghcr.io") != 1 || r.ran("docker logout docker.io") != 0 || len(reg.readMeta()) != 0 {
		t.Errorf("calls %v meta %v", r.calls, reg.readMeta())
	}
	if list, _ := reg.List(); len(list) != 0 {
		t.Errorf("left: %v", list)
	}
	// A logout docker did not carry out is not reported as done.
	os.WriteFile(cfgPath, []byte(`{"auths":{"ghcr.io":{"auth":"dTp0"}}}`), 0o600)
	r.hook = nil
	if err := reg.Logout(context.Background(), "ghcr.io"); err == nil || !strings.Contains(err.Error(), "silmedi") {
		t.Errorf("logout that did nothing: %v", err)
	}
}

// A token pasted into the user name field never reaches docker, the list or the audit log.
func TestRegistryTokenAsUserName(t *testing.T) {
	for _, user := range []string{"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_11ABCDEFG0123456789", "GHP_X", "dckr_pat_x", "a_b", "ornek--ad-" + strings.Repeat("x", 30)} {
		if _, _, err := CheckLogin("ghcr.io", user); err == nil {
			t.Errorf("ghcr.io user %q accepted", user)
		}
	}
	if _, u, err := CheckLogin("ghcr.io", " ornek-kullanici "); err != nil || u != "ornek-kullanici" {
		t.Errorf("real user: %q %v", u, err)
	}
	if _, _, err := CheckLogin("registry.example.com:5000", "deploy+bot@example"); err != nil {
		t.Errorf("other registries keep their wider user names: %v", err)
	}
	reg, r := newRegistries(t)
	dir := t.TempDir()
	c, _ := startServer(t, 999, func(s *Server) {
		s.Registries = reg
		s.Audit = NewAudit(filepath.Join(dir, "audit.jsonl"))
	})
	var ae *Error
	if _, err := c.RegistryLogin(ctx, "ghcr.io", "ghp_SECRETTOKEN123", "ghp_SECRETTOKEN123", Actor{User: "u"}); !errors.As(err, &ae) || ae.Status != 422 {
		t.Fatalf("login: %v", err)
	}
	if len(r.calls) != 0 {
		t.Errorf("docker ran: %v", r.calls)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if strings.Contains(string(b), "SECRETTOKEN") || !strings.Contains(string(b), "registry.login") {
		t.Errorf("audit: %s", b)
	}
}

func TestTokenAlerts(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	list := []RegistryLogin{
		{Host: "a.io"}, // no expiry known
		{Host: "b.io", Expires: now.Add(60 * 24 * time.Hour)},
		{Host: "c.io", Expires: now.Add(10 * 24 * time.Hour)},
		{Host: "d.io", Expires: now.Add(2 * 24 * time.Hour)},
		{Host: "e.io", Expires: now.Add(-time.Hour)},
	}
	got := map[string]string{}
	for _, a := range tokenAlerts(list, now) {
		got[a.Key] = a.Level
	}
	want := map[string]string{"registry:c.io": "warn", "registry:d.io": "bad", "registry:e.io": "bad"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("alerts = %v", got)
	}
}

// ---- moving a site to another group ----

func createdSite(t *testing.T) *opsEnv {
	t.Helper()
	e := newOps(t)
	if err := e.ops.Create(context.Background(), specFor("enisparta"), map[string]string{"NODE_ENV": "production", "PEM": "a\nb"}, e.progress); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.root, "/srv/musteri/enisparta/data/photo.jpg"), []byte("jpeg"), 0o644)
	e.steps, e.run.calls, e.run.stdins = nil, nil, nil
	return e
}

func TestMoveSite(t *testing.T) {
	e := createdSite(t)
	if err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress); err != nil {
		t.Fatalf("move: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	dir := filepath.Join(e.root, "/srv/kisisel/enisparta")
	if b, err := os.ReadFile(filepath.Join(dir, "data/photo.jpg")); err != nil || string(b) != "jpeg" {
		t.Errorf("data not moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "/srv/musteri/enisparta")); !errors.Is(err, os.ErrNotExist) {
		t.Error("old directory left")
	}
	compose := readFile(t, filepath.Join(dir, "compose.yaml"))
	if !strings.Contains(compose, "name: prj_kisisel_enisparta") || !strings.Contains(compose, `"/srv/kisisel/enisparta/data:/app/uploads"`) ||
		!strings.Contains(compose, `PEM: "a\nb"`) || !strings.Contains(compose, "MONGODB_URI") {
		t.Errorf("compose:\n%s", compose)
	}
	sp, err := e.ops.Store.Get("enisparta")
	if err != nil || sp.Group != "kisisel" {
		t.Errorf("spec %+v %v", sp, err)
	}
	if meta := readFile(t, filepath.Join(dir, "footops.json")); !strings.Contains(meta, `"group": "kisisel"`) {
		t.Errorf("meta:\n%s", meta)
	}
	if caddy := readFile(t, filepath.Join(e.root, core.CaddySitesDir, "enisparta.caddy")); !strings.Contains(caddy, "(kisisel)") {
		t.Errorf("caddy:\n%s", caddy)
	}
	members := e.net.networks["prj_kisisel_enisparta"]
	if _, ok := members["footops-caddy"]; !ok || len(members["footops-mongo"]) != 1 {
		t.Errorf("new network members = %v", members)
	}
	if _, ok := e.net.networks["prj_musteri_enisparta"]; ok {
		t.Error("old network left")
	}
	if e.run.ran("--file /srv/kisisel/enisparta/compose.yaml up --detach --remove-orphans") != 1 {
		t.Errorf("calls:\n%s", strings.Join(e.run.calls, "\n"))
	}
	// The old container is stopped before its directory moves, so no restart can create
	// an empty directory at the old path.
	stop, up := callIndex(e.run.calls, "--file /srv/musteri/enisparta/compose.yaml stop"), callIndex(e.run.calls, "/srv/kisisel/enisparta/compose.yaml up")
	if stop < 0 || stop > up {
		t.Errorf("stop %d, up %d:\n%s", stop, up, strings.Join(e.run.calls, "\n"))
	}
}

func callIndex(calls []string, sub string) int {
	for i, c := range calls {
		if strings.Contains(c, sub) {
			return i
		}
	}
	return -1
}

// The new container crashes: it is stopped before the directory goes back, the old one
// starts again from the old path.
func TestMoveRollsBackWhenNewContainerCrashes(t *testing.T) {
	e := createdSite(t)
	e.run.hook = func(cmd string) {
		switch {
		case strings.Contains(cmd, "/srv/kisisel/enisparta/compose.yaml up"):
			e.net.running = false
		case strings.Contains(cmd, "/srv/musteri/enisparta/compose.yaml up"):
			e.net.running = true
		}
	}
	err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress)
	if err == nil || !strings.Contains(err.Error(), "Cannot find module") {
		t.Fatalf("err = %v", err)
	}
	stopNew := callIndex(e.run.calls, "--file /srv/kisisel/enisparta/compose.yaml stop")
	upOld := callIndex(e.run.calls, "--file /srv/musteri/enisparta/compose.yaml up")
	if stopNew < 0 || upOld < stopNew {
		t.Errorf("new container not stopped before the old one started:\n%s", strings.Join(e.run.calls, "\n"))
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "/srv/musteri/enisparta/data/photo.jpg")); err != nil || string(b) != "jpeg" {
		t.Errorf("data not back: %v", err)
	}
	if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "Eski hâline döndü") {
		t.Errorf("steps:\n%s", strings.Join(e.steps, "\n"))
	}
}

// A network of the new group that was there before (an earlier move cut short) is left
// as it was: the rollback only undoes what this move did.
func TestMoveKeepsNetworkItDidNotCreate(t *testing.T) {
	e := createdSite(t)
	e.net.networks["prj_kisisel_enisparta"] = map[string][]string{"footops-caddy": nil}
	e.run.fail["/srv/kisisel/enisparta/compose.yaml up"] = errors.New("exit status 1")
	if err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress); err == nil {
		t.Fatal("move succeeded")
	}
	members, ok := e.net.networks["prj_kisisel_enisparta"]
	if !ok {
		t.Fatal("a network the move did not create was removed")
	}
	if _, ok := members["footops-caddy"]; !ok || len(members) != 1 {
		t.Errorf("members = %v (Caddy must stay, the move's own Mongo connection must go)", members)
	}
	if _, ok := e.net.networks["prj_musteri_enisparta"]["footops-caddy"]; !ok {
		t.Error("old network lost Caddy")
	}
	if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "Eski hâline döndü") {
		t.Errorf("steps:\n%s", strings.Join(e.steps, "\n"))
	}
}

// A tag that now names a newer build (an image check pulled it) is not recreated by a
// move or an edit: the site would change version and could not go back.
func TestMoveAndUpdateRefuseMovedTag(t *testing.T) {
	e := createdSite(t)
	e.net.image = "sha256:old"
	e.run.out["image inspect --format {{.Id}} ghcr.io/swbilisim/enisparta:main"] = "sha256:new\n"
	if err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress); !errors.Is(err, errTagMoved) {
		t.Fatalf("move: %v", err)
	}
	u := updateFrom(specFor("enisparta"))
	u.MemoryMB = 512
	if err := e.ops.Update(context.Background(), "enisparta", u, e.progress); !errors.Is(err, errTagMoved) {
		t.Fatalf("update: %v", err)
	}
	if e.run.ran(" stop") != 0 || e.run.ran(" up ") != 0 {
		t.Errorf("site touched:\n%s", strings.Join(e.run.calls, "\n"))
	}
	if sp, _ := e.ops.Store.Get("enisparta"); sp.Group != "musteri" || sp.MemoryMB != 256 {
		t.Errorf("spec changed: %+v", sp)
	}
	e.run.out["image inspect --format {{.Id}} ghcr.io/swbilisim/enisparta:main"] = "sha256:old\n"
	if err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress); err != nil {
		t.Errorf("same image refused: %v", err)
	}
}

// A move cut short after the rename leaves compose.yaml naming the old group's paths:
// the next start or redeploy writes it again from the record before the container runs.
func TestRedeployRewritesComposeAfterCutShortMove(t *testing.T) {
	e := createdSite(t)
	os.MkdirAll(filepath.Join(e.root, "/srv/kisisel"), 0o755)
	if err := os.Rename(filepath.Join(e.root, "/srv/musteri/enisparta"), filepath.Join(e.root, "/srv/kisisel/enisparta")); err != nil {
		t.Fatal(err)
	}
	e.ops.Reconcile(context.Background())
	if err := e.ops.Redeploy(context.Background(), "enisparta", e.progress); err != nil {
		t.Fatalf("redeploy: %v\n%s", err, strings.Join(e.steps, "\n"))
	}
	compose := readFile(t, filepath.Join(e.root, "/srv/kisisel/enisparta/compose.yaml"))
	if !strings.Contains(compose, "name: prj_kisisel_enisparta") || !strings.Contains(compose, `"/srv/kisisel/enisparta/data:/app/uploads"`) ||
		!strings.Contains(compose, `PEM: "a\nb"`) || strings.Contains(compose, "/srv/musteri/") {
		t.Errorf("compose:\n%s", compose)
	}
	if !strings.Contains(strings.Join(e.steps, "\n"), "yeniden yazıldı") {
		t.Errorf("steps:\n%s", strings.Join(e.steps, "\n"))
	}
	// Nothing to rewrite the next time.
	e.steps = nil
	e.ops.Redeploy(context.Background(), "enisparta", e.progress)
	if strings.Contains(strings.Join(e.steps, "\n"), "yeniden yazıldı") {
		t.Error("an unchanged compose.yaml was rewritten")
	}
}

func TestMoveRollsBack(t *testing.T) {
	e := createdSite(t)
	e.run.fail["/srv/kisisel/enisparta/compose.yaml up"] = errors.New("exit status 1: network not found")
	err := e.ops.Move(context.Background(), "enisparta", "kisisel", e.progress)
	if err == nil {
		t.Fatal("move succeeded")
	}
	dir := filepath.Join(e.root, "/srv/musteri/enisparta")
	if b, err := os.ReadFile(filepath.Join(dir, "data/photo.jpg")); err != nil || string(b) != "jpeg" {
		t.Errorf("data not back: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "/srv/kisisel/enisparta")); !errors.Is(err, os.ErrNotExist) {
		t.Error("new directory left")
	}
	if compose := readFile(t, filepath.Join(dir, "compose.yaml")); !strings.Contains(compose, "name: prj_musteri_enisparta") {
		t.Errorf("compose not restored:\n%s", compose)
	}
	if sp, _ := e.ops.Store.Get("enisparta"); sp.Group != "musteri" {
		t.Errorf("group = %s", sp.Group)
	}
	if _, ok := e.net.networks["prj_kisisel_enisparta"]; ok {
		t.Error("new network left")
	}
	if _, ok := e.net.networks["prj_musteri_enisparta"]["footops-caddy"]; !ok {
		t.Error("old network lost Caddy")
	}
	if e.run.ran("--file /srv/musteri/enisparta/compose.yaml up --detach --remove-orphans") != 1 {
		t.Errorf("old container not started again:\n%s", strings.Join(e.run.calls, "\n"))
	}
	if last := e.steps[len(e.steps)-1]; !strings.Contains(last, "Eski hâline döndü") {
		t.Errorf("last step %q", last)
	}
}

func TestCheckMove(t *testing.T) {
	e := createdSite(t)
	for group, want := range map[string]string{"musteri": "zaten", "random": "grup şunlardan", "": "grup şunlardan"} {
		if _, _, err := e.ops.CheckMove("enisparta", group); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", group, err)
		}
	}
	os.MkdirAll(filepath.Join(e.root, "/srv/kisisel/enisparta"), 0o755)
	if _, _, err := e.ops.CheckMove("enisparta", "kisisel"); err == nil || !strings.Contains(err.Error(), "zaten var") {
		t.Errorf("existing target: %v", err)
	}
	if _, _, err := e.ops.CheckMove("nope", "kisisel"); !errors.Is(err, sites.ErrNotFound) {
		t.Errorf("missing site: %v", err)
	}
}

func TestMoveHandler(t *testing.T) {
	e := createdSite(t)
	recDir := t.TempDir()
	c, _ := startServer(t, 999, func(s *Server) {
		s.Sites, s.Jobs = e.ops, NewJobs()
		s.Data = &DataOps{RecoveryDir: recDir}
		s.Audit = NewAudit(filepath.Join(recDir, "audit.jsonl"))
	})
	var ae *Error
	if _, err := c.MoveSite(ctx, "enisparta", "kisisel", "enisparta-x", Actor{User: "u"}); !errors.As(err, &ae) || ae.Status != 422 {
		t.Errorf("wrong confirmation: %v", err)
	}
	if _, err := c.MoveSite(ctx, "nope", "kisisel", "nope", Actor{User: "u"}); !errors.As(err, &ae) || ae.Status != 404 {
		t.Errorf("missing site: %v", err)
	}
	os.WriteFile(filepath.Join(recDir, "job9.json"), []byte(`{"job":"job9","kind":"restore","site":"enisparta"}`), 0o600)
	if _, err := c.MoveSite(ctx, "enisparta", "kisisel", "enisparta", Actor{User: "u"}); !errors.As(err, &ae) || ae.Status != 409 {
		t.Errorf("recovery pending: %v", err)
	}
	if _, err := c.CheckImage(ctx, "not an image", Actor{User: "u"}); !errors.As(err, &ae) || ae.Status != 422 {
		t.Errorf("bad image: %v", err)
	}
}

// ---- DNS ----

func TestClassify(t *testing.T) {
	here := map[netip.Addr]bool{netip.MustParseAddr("203.0.113.10"): true}
	ip := func(s ...string) []netip.Addr {
		var out []netip.Addr
		for _, x := range s {
			out = append(out, netip.MustParseAddr(x))
		}
		return out
	}
	for want, ips := range map[string][]netip.Addr{
		"cloudflare": ip("104.21.32.1", "172.67.150.2", "2606:4700:3030::6815:2001"),
		"here":       ip("203.0.113.10"),
		"elsewhere":  ip("198.51.100.20"),
		"none":       nil,
	} {
		if got := classify(ips, here); got != want {
			t.Errorf("%v: %s, want %s", ips, got, want)
		}
	}
	if got := classify(ip("104.21.32.1", "198.51.100.20"), here); got != "elsewhere" {
		t.Errorf("mixed: %s", got)
	}
	if got := classify(ip("::ffff:203.0.113.10"), here); got != "here" {
		t.Errorf("mapped: %s", got)
	}
}
