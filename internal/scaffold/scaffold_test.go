package scaffold

import (
	"archive/zip"
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"
)

func next() Input {
	return Input{Kind: NextJS, Owner: "Ornek-Kullanici", Name: "kahve-dukkani", PM: NPM, Port: 3000, DataDir: "/app/public/uploads"}
}

func render(t *testing.T, in Input) map[string]string {
	t.Helper()
	fs, err := Files(in)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range fs {
		if strings.Contains(f.Body, "[[") || strings.Contains(f.Body, "<no value>") {
			t.Fatalf("%s: template left over:\n%s", f.Path, f.Body)
		}
		out[f.Path] = f.Body
	}
	if len(out) != 3 || out["Dockerfile"] == "" || out[".dockerignore"] == "" || out[".github/workflows/docker.yml"] == "" {
		t.Fatalf("files: %v", out)
	}
	return out
}

func lines(s string) map[string]bool {
	m := map[string]bool{}
	for _, l := range strings.Split(s, "\n") {
		m[strings.TrimSpace(l)] = true
	}
	return m
}

func mustHave(t *testing.T, name, body string, want ...string) {
	t.Helper()
	have := lines(body)
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s: no line %q in\n%s", name, w, body)
		}
	}
}

func mustNotHave(t *testing.T, name, body string, bad ...string) {
	t.Helper()
	have := lines(body)
	for _, b := range bad {
		if have[b] {
			t.Errorf("%s: unexpected line %q in\n%s", name, b, body)
		}
	}
}

func TestNextJS(t *testing.T) {
	f := render(t, next())
	mustHave(t, "Dockerfile", f["Dockerfile"],
		"COPY package.json package-lock.json .npmrc* ./",
		"RUN npm ci",
		"RUN npm run build",
		"HOSTNAME=0.0.0.0",
		"PORT=3000 \\",
		"COPY --from=builder /app/.next/standalone ./",
		"COPY --from=builder /app/public ./public",
		"RUN mkdir -p .next/cache /app/public/uploads && chown app:app .next/cache /app/public/uploads",
		`LABEL footops.data-dir="/app/public/uploads"`,
		"USER app",
		"EXPOSE 3000",
		`CMD wget -q -O /dev/null "http://127.0.0.1:3000/" || exit 1`,
		`CMD ["node", "server.js"]`)
	// The image name is lower case; the workflow pushes exactly it.
	mustHave(t, "workflow", f[".github/workflows/docker.yml"], "images: ghcr.io/ornek-kullanici/kahve-dukkani")
	mustHave(t, ".dockerignore", f[".dockerignore"], ".env*", "!.env.production", "public/uploads", ".git", "node_modules", ".next")
	// USER comes after everything that needs root.
	d := f["Dockerfile"]
	if strings.Index(d, "USER app") < strings.LastIndex(d, "RUN ") {
		t.Errorf("a RUN after USER:\n%s", d)
	}

	in := next()
	in.DataDir, in.PM, in.Port = "", PNPM, 4000
	f = render(t, in)
	mustHave(t, "Dockerfile", f["Dockerfile"],
		"COPY package.json pnpm-lock.yaml .npmrc* ./",
		"RUN corepack enable pnpm && pnpm install --frozen-lockfile",
		"RUN corepack enable pnpm && pnpm run build",
		"RUN mkdir -p .next/cache && chown app:app .next/cache",
		"EXPOSE 4000")
	if strings.Contains(f["Dockerfile"], "LABEL") {
		t.Errorf("label without a data folder:\n%s", f["Dockerfile"])
	}
	mustNotHave(t, ".dockerignore", f[".dockerignore"], "public/uploads")
}

func TestNode(t *testing.T) {
	in := Input{Kind: Node, Owner: "ornek", Name: "api-sunucu", Port: 8081, DataDir: "/app/uploads", Start: "src/index.mjs"}
	f := render(t, in)
	mustHave(t, "Dockerfile", f["Dockerfile"],
		"RUN npm ci --omit=dev && npm cache clean --force",
		"RUN mkdir -p /app/uploads && chown app:app /app/uploads",
		`LABEL footops.data-dir="/app/uploads"`,
		"PORT=8081",
		`CMD ["node", "src/index.mjs"]`)
	// A Node server reads .env.production at run time: it may hold secrets.
	mustNotHave(t, ".dockerignore", f[".dockerignore"], "!.env.production")
	mustHave(t, ".dockerignore", f[".dockerignore"], ".env*", "uploads")

	in.Start, in.DataDir = "", ""
	f = render(t, in)
	mustHave(t, "Dockerfile", f["Dockerfile"], `CMD ["node", "server.js"]`)
	if strings.Contains(f["Dockerfile"], "mkdir") {
		t.Errorf("a writable folder without a data folder:\n%s", f["Dockerfile"])
	}
}

func TestStatic(t *testing.T) {
	plain := Input{Kind: Static, Owner: "ornek", Name: "tanitim", Port: 3000, DataDir: "/app/x", Start: "x.js"}
	f := render(t, plain)
	mustHave(t, "Dockerfile", f["Dockerfile"], "FROM nginxinc/nginx-unprivileged:stable-alpine", "COPY . /usr/share/nginx/html", "EXPOSE 8080")
	if strings.Contains(f["Dockerfile"], "node:") || strings.Contains(f["Dockerfile"], "LABEL") {
		t.Errorf("plain static site with a build or label:\n%s", f["Dockerfile"])
	}
	// Everything in the folder is published: no .env at all, nor the build files.
	mustHave(t, ".dockerignore", f[".dockerignore"], ".env*", "Dockerfile", ".dockerignore", ".git")
	mustNotHave(t, ".dockerignore", f[".dockerignore"], "!.env.production")

	built := Input{Kind: Static, Owner: "ornek", Name: "tanitim", PM: PNPM, BuildDir: "dist/"}
	f = render(t, built)
	mustHave(t, "Dockerfile", f["Dockerfile"], "RUN corepack enable pnpm && pnpm run build", "COPY --from=builder /app/dist /usr/share/nginx/html")
	mustHave(t, ".dockerignore", f[".dockerignore"], "!.env.production", "dist", "Dockerfile")
	mustNotHave(t, ".dockerignore", f[".dockerignore"], "README*")
}

// The workflow is the one the live sites build with (gulpinar-pos, 2026-11), comments
// and that site's paths-ignore aside.
func TestWorkflowMatchesLiveSites(t *testing.T) {
	want := `name: Docker
on:
push:
branches: [main]
workflow_dispatch:
permissions:
contents: read
packages: write
concurrency:
group: docker-${{ github.ref }}
cancel-in-progress: true
jobs:
imaj:
runs-on: ubuntu-latest
steps:
- uses: actions/checkout@v7
- uses: docker/setup-buildx-action@v4
- uses: docker/login-action@v4
with:
registry: ghcr.io
username: ${{ github.actor }}
password: ${{ secrets.GITHUB_TOKEN }}
- id: meta
uses: docker/metadata-action@v6
with:
images: ghcr.io/swbilisim/gulpinar-pos
tags: |
type=raw,value=main,enable={{is_default_branch}}
type=sha,prefix=sha-
- uses: docker/build-push-action@v7
with:
context: .
platforms: linux/amd64
push: true
tags: ${{ steps.meta.outputs.tags }}
labels: ${{ steps.meta.outputs.labels }}
cache-from: type=gha
cache-to: type=gha,mode=max`
	f := render(t, Input{Kind: NextJS, Owner: "swbilisim", Name: "gulpinar-pos", Port: 3000})
	var got []string
	for _, l := range strings.Split(f[".github/workflows/docker.yml"], "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			got = append(got, l)
		}
	}
	if g := strings.Join(got, "\n"); g != want {
		t.Errorf("workflow:\n%s\nwant:\n%s", g, want)
	}
	// Only pushes to the repository (and a manual run) build with the package token; a
	// pull request from a fork never does.
	if strings.Contains(f[".github/workflows/docker.yml"], "pull_request") {
		t.Error("workflow runs on pull requests")
	}
}

// Nothing that would mean something to a shell, a Dockerfile or YAML gets through.
func TestCheckRefuses(t *testing.T) {
	bad := []string{"", " ", "a b", "a\nb", "a\rb", `a"b`, "a'b", "a$b", "a`b", "a;b", "a&b", "a|b", "a\\b", "a:b", "a#b",
		"a{b", "a}b", "a[b", "a]b", "a*b", "a?b", "a=b", "a,b", "../x", "x/../y", "./x", ".x", "x/.y", "/x", "x//y", "\x00", "ğ"}
	for _, s := range bad {
		in := next()
		in.Owner = s
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("owner %q accepted", s)
		}
		in = next()
		in.Name = s
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("name %q accepted", s)
		}
		in = next()
		in.DataDir = "/app/" + s
		if _, p := Check(in); len(p) == 0 && s != "" && s != " " {
			t.Errorf("data folder %q accepted", in.DataDir)
		}
		in = Input{Kind: Node, Owner: "o", Name: "api-x", Port: 3000, Start: s + ".js"}
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("start %q accepted", in.Start)
		}
		// "" and " " mean no build; "/x" is "x" with the slash trimmed.
		in = Input{Kind: Static, Owner: "o", Name: "web-x", BuildDir: s}
		if _, p := Check(in); len(p) == 0 && strings.TrimSpace(s) != "" && s != "/x" {
			t.Errorf("build folder %q accepted", in.BuildDir)
		}
	}
	for _, d := range []string{"/app", "/app/", "/", "//", "/data", "/app/public", "/app/node_modules/x", "/app/.next/cache", "/app/server.js", "/etc/app", "/appx/uploads", "app/uploads"} {
		in := next()
		in.DataDir = d
		if _, p := Check(in); len(p) == 0 && d != "/app/" {
			t.Errorf("data folder %q accepted", d)
		}
	}
	for _, name := range []string{"Kahve", "ab", "-ab", "core", "app", "footops-x", strings.Repeat("a", 33)} {
		in := next()
		in.Name = name
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("name %q accepted", name)
		}
	}
	for _, o := range []string{"a--b", "a-", "-a", strings.Repeat("a", 40)} {
		in := next()
		in.Owner = o
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("owner %q accepted", o)
		}
	}
	for _, port := range []int{0, 80, 1023, 65536, -1} {
		in := next()
		in.Port = port
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("port %d accepted", port)
		}
	}
	for _, in := range []Input{{Kind: "php", Owner: "o", Name: "web-x"}, {Kind: NextJS, Owner: "o", Name: "web-x", Port: 3000, PM: "yarn"}} {
		if _, p := Check(in); len(p) == 0 {
			t.Errorf("%+v accepted", in)
		}
		if _, err := Files(in); err == nil {
			t.Errorf("%+v rendered", in)
		}
	}
	// "/app/" is "/app" once trimmed: refused too.
	in := next()
	in.DataDir = "/app/"
	if _, p := Check(in); len(p) == 0 {
		t.Error(`data folder "/app/" accepted`)
	}
}

// Whatever Check lets through renders only characters the templates expect.
func TestRenderedValuesArePlain(t *testing.T) {
	safe := regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	for _, in := range []Input{next(), {Kind: Node, Owner: "A1-b", Name: "x-y-z", Port: 65535, DataDir: "/app/a/b_c.d", Start: "a/b.cjs"},
		{Kind: Static, Owner: "o", Name: "web-x", BuildDir: "a/b"}} {
		got, p := Check(in)
		if len(p) > 0 {
			t.Fatalf("%+v: %v", in, p)
		}
		for _, v := range []string{got.Owner, got.Name, got.DataDir, got.Start, got.BuildDir, ImageOf(got.Owner, got.Name)} {
			if !safe.MatchString(v) {
				t.Errorf("value %q", v)
			}
		}
	}
}

func TestZip(t *testing.T) {
	fs, err := Files(next())
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := Zip(&b, fs, time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != len(fs) {
		t.Fatalf("%d files", len(zr.File))
	}
	for i, zf := range zr.File {
		if zf.Name != fs[i].Path || strings.HasPrefix(zf.Name, "/") || strings.Contains(zf.Name, "..") || zf.Mode().Perm() != 0o644 {
			t.Errorf("entry %q mode %v", zf.Name, zf.Mode())
		}
		rc, _ := zf.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		if string(body) != fs[i].Body {
			t.Errorf("%s differs", zf.Name)
		}
	}
}
