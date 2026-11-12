package web

import (
	"archive/zip"
	"bytes"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// "Projeni hazırla": the form, the files on the page, the zip.
func TestProjectPage(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)

	// The account defaults to the one this server's sites pull from.
	resp, body := c.do("GET", "/sites/new/project", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="owner" value="x"`) || !strings.Contains(body, "/static/copy.js") ||
		strings.Contains(body, `id="dosyalar"`) {
		t.Fatalf("form: %d\n%s", resp.StatusCode, body)
	}
	// Switching kind keeps the account and the name.
	if !strings.Contains(body, `href="/sites/new/project?kind=node&amp;name=&amp;owner=x&amp;pm=npm"`) {
		t.Errorf("kind links:\n%s", body)
	}

	q := url.Values{"go": {"1"}, "kind": {"nextjs"}, "owner": {"Ornek"}, "name": {"kahve-dukkani"}, "pm": {"npm"}, "port": {"3000"}, "data": {"/app/public/uploads"}}
	resp, body = c.do("GET", "/sites/new/project?"+q.Encode(), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("files: %d\n%s", resp.StatusCode, body)
	}
	text := html.UnescapeString(body)
	for _, want := range []string{
		`id="dosya-0"`, `id="dosya-1"`, `id="dosya-2"`, `data-copy="dosya-0"`, "Zip indir",
		`LABEL footops.data-dir="/app/public/uploads"`, "images: ghcr.io/ornek/kahve-dukkani", "group: docker-${{ github.ref }}",
		"gh repo create Ornek/kahve-dukkani --private --source . --push", "ghcr.io/ornek/kahve-dukkani:main",
		`href="/sites/new?image=ghcr.io%2Fornek%2Fkahve-dukkani%3Amain&tpl=nextjs"`,
		"output: 'standalone'", "!.env.production",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("page misses %q", want)
		}
	}
	zipURL := "/sites/new/project/zip?" + url.Values{"kind": {"nextjs"}, "owner": {"Ornek"}, "name": {"kahve-dukkani"}, "pm": {"npm"},
		"port": {"3000"}, "data": {"/app/public/uploads"}}.Encode()
	if !strings.Contains(text, `href="`+zipURL+`"`) {
		t.Errorf("zip link missing (%s)", zipURL)
	}

	// The zip holds exactly what the page shows.
	resp, zbody := c.do("GET", zipURL, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" ||
		resp.Header.Get("Content-Disposition") != `attachment; filename=kahve-dukkani-footops.zip` {
		t.Fatalf("zip: %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader([]byte(zbody)), int64(len(zbody)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if !strings.Contains(text, string(b)) {
			t.Errorf("%s in the zip is not the page's", f.Name)
		}
	}
	if strings.Join(names, " ") != "Dockerfile .dockerignore .github/workflows/docker.yml" {
		t.Errorf("zip entries %v", names)
	}

	// The other kinds.
	for _, c2 := range []struct {
		q    url.Values
		want []string
	}{
		{url.Values{"kind": {"node"}, "owner": {"o"}, "name": {"api-x"}, "port": {"8081"}, "start": {"src/app.js"}, "pm": {"pnpm"}},
			[]string{`CMD ["node", "src/app.js"]`, "pnpm install --frozen-lockfile --prod", "process.env.PORT"}},
		{url.Values{"kind": {"static"}, "owner": {"o"}, "name": {"web-x"}, "build": {"dist"}},
			[]string{"COPY --from=builder /app/dist /usr/share/nginx/html", "EXPOSE 8080"}},
	} {
		c2.q.Set("go", "1")
		resp, body := c.do("GET", "/sites/new/project?"+c2.q.Encode(), nil)
		for _, w := range c2.want {
			if resp.StatusCode != 200 || !strings.Contains(html.UnescapeString(body), w) {
				t.Errorf("%s: %d, misses %q", c2.q.Get("kind"), resp.StatusCode, w)
			}
		}
	}
}

// Input that would mean something in a Dockerfile, YAML or HTML is refused and shown
// back escaped; the zip is not made.
func TestProjectRefusesInput(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	for _, q := range []url.Values{
		{"owner": {"<script>alert(1)</script>"}, "name": {"kahve"}},
		{"owner": {"o"}, "name": {"kahve\nRUN curl x"}},
		{"owner": {"o"}, "name": {"kahve"}, "data": {"/app/x\"; rm -rf /"}},
		{"owner": {"o"}, "name": {"kahve"}, "data": {"/app/../etc"}},
		{"owner": {"o"}, "name": {"kahve"}, "port": {"80"}},
		{"owner": {"o"}, "name": {"kahve"}, "kind": {"node"}, "start": {"x.js\"]\nUSER root"}},
		{"owner": {"o"}, "name": {"kahve"}, "kind": {"static"}, "build": {"../.."}},
		{"owner": {"o"}, "name": {"kahve"}, "pm": {"yarn"}},
	} {
		if !q.Has("kind") {
			q.Set("kind", "nextjs")
		}
		if !q.Has("port") {
			q.Set("port", "3000")
		}
		resp, zbody := c.do("GET", "/sites/new/project/zip?"+q.Encode(), nil)
		if resp.StatusCode != http.StatusBadRequest || strings.HasPrefix(zbody, "PK") {
			t.Errorf("zip for %v: %d", q, resp.StatusCode)
		}
		q.Set("go", "1")
		resp, body := c.do("GET", "/sites/new/project?"+q.Encode(), nil)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Düzeltilmesi gerekenler") || strings.Contains(body, `id="dosya-0"`) {
			t.Errorf("page for %v: %d", q, resp.StatusCode)
		}
		if strings.Contains(body, "<script>alert") {
			t.Errorf("unescaped input for %v", q)
		}
	}
}

func TestProjectNeedsSession(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	for _, p := range []string{"/sites/new/project", "/sites/new/project/zip?kind=nextjs&owner=o&name=kahve&port=3000"} {
		resp, _ := c.do("GET", p, nil)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s without a session: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// Without sites the account comes from the server's GHCR login.
func TestProjectOwnerFromLogin(t *testing.T) {
	if o, ok := ghcrOwner("ghcr.io/swbilisim/x:main"); !ok || o != "swbilisim" {
		t.Errorf("ghcrOwner = %q %v", o, ok)
	}
	for _, img := range []string{"docker.io/x/y", "ghcr.io/", "ghcr.io/x", "nginx"} {
		if _, ok := ghcrOwner(img); ok {
			t.Errorf("ghcrOwner(%q) ok", img)
		}
	}
}

// A new site's page opens with what is left to do, for a week.
func TestSiteNextSteps(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	e.agent.dbSite = true

	e.agent.siteBorn = e.clock.Add(-time.Hour)
	_, body := c.do("GET", "/sites/enisparta", nil)
	for _, want := range []string{`id="sonraki"`, "Sıradaki adımlar", `href="/sites/enisparta/import"`, "eksik", "enisparta.com</span> için Origin sertifikası yok",
		`href="/sites/enisparta/dns"`, "7 gün kaldı"} {
		if !strings.Contains(body, want) {
			t.Errorf("card misses %q", want)
		}
	}
	// The job page of a new site points to it.
	e.agent.jobDone = true
	_, body = c.do("GET", "/jobs/job123", nil)
	if !strings.Contains(body, `href="/sites/enisparta#sonraki"`) {
		t.Error("job page does not link the next steps")
	}

	for name, set := range map[string]func(){
		"old":     func() { e.agent.siteBorn = e.clock.Add(-8 * 24 * time.Hour) },
		"unknown": func() { e.agent.siteBorn = time.Time{} },
		"parked":  func() { e.agent.siteBorn = *e.clock; e.agent.pk.parked = &sites.ParkState{At: *e.clock} },
	} {
		set()
		_, body := c.do("GET", "/sites/enisparta", nil)
		if strings.Contains(body, `id="sonraki"`) {
			t.Errorf("%s site shows the card", name)
		}
	}
}

// Registry logins link GitHub's token form with the one scope a server needs; a
// refused image check explains them.
func TestImageAccessHelp(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	_, body := c.do("GET", "/registries", nil)
	if !strings.Contains(body, `href="https://github.com/settings/tokens/new?description=FootOps&#43;H1&amp;scopes=read%3Apackages" rel="noopener noreferrer" target="_blank"`) {
		t.Errorf("token link:\n%s", body)
	}

	e.agent.job = imageJob(t, agent.ImageInfo{Image: "ghcr.io/o/x:main"}, "imaja erişilemedi: özel bir imajsa bu sunucuda o kayıt defterine giriş yok ya da token'ın bu pakete okuma izni yok (İmaj girişleri sayfası); adını ve etiketini de kontrol et (denied)")
	_, body = c.do("GET", "/jobs/imgjob1?tpl=nextjs", nil)
	if !strings.Contains(body, "Özel imajsa") || !strings.Contains(body, `href="/sites/new/project?kind=nextjs"`) {
		t.Errorf("no help for a refused image:\n%s", body)
	}
	e.agent.job = imageJob(t, agent.ImageInfo{Image: "ghcr.io/o/x:main"}, "imaj linux/arm64 için derlenmiş")
	_, body = c.do("GET", "/jobs/imgjob1?tpl=nextjs", nil)
	if strings.Contains(body, "Özel imajsa") {
		t.Error("registry help for another error")
	}
}

// The new-site steps carry their help column and the way to Projeni hazırla.
func TestNewSiteHelp(t *testing.T) {
	e := newEnv(t)
	c := e.client(t, true)
	e.login(t, c)
	_, body := c.do("GET", "/sites/new?tpl=node", nil)
	if !strings.Contains(body, `class="help"`) || !strings.Contains(body, `href="/sites/new/project?kind=node"`) {
		t.Errorf("step 1:\n%s", body)
	}
	_, body = c.do("GET", "/sites/new/form?tpl=nextjs", nil)
	if !strings.Contains(body, `class="help"`) || !strings.Contains(body, "Kalıcı klasör</dt>") {
		t.Errorf("step 2 has no help")
	}
	_, body = c.do("GET", "/sites", nil)
	if !strings.Contains(body, `href="/sites/new/project"`) {
		t.Error("sites page does not link Projeni hazırla")
	}
}
