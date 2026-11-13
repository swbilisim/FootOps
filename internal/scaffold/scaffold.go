// Package scaffold writes the files a project needs before FootOps can run it: a
// Dockerfile, a .dockerignore and a GitHub Actions workflow that builds the image and
// pushes it to GHCR. The panel shows them and offers them as a zip; the templates in
// files/ are the one source the documentation points to.
//
// Nothing here is escaped for its target (a Dockerfile, shell lines, YAML), so every
// input is checked against a strict allowlist first: the values are plain names, a
// number and slash-separated paths of [A-Za-z0-9._-], with no quotes, spaces, "$" or
// line breaks. An input that fails is refused, never cleaned up.
package scaffold

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// Kinds of project.
const (
	NextJS = "nextjs"
	Node   = "node"
	Static = "static"
)

// Kinds lists the kinds in the order the panel offers them.
var Kinds = []string{NextJS, Node, Static}

// Package managers.
const (
	NPM  = "npm"
	PNPM = "pnpm"
)

// DefaultPort is what each kind listens on unless told otherwise; a static site's nginx
// always listens on 8080.
func DefaultPort(kind string) int {
	if kind == Static {
		return 8080
	}
	return 3000
}

// Input is what the panel asks for.
type Input struct {
	Kind  string // NextJS, Node or Static
	Owner string // GitHub account or organization that owns the repository
	Name  string // repository, image and site name
	PM    string // NPM or PNPM (unused by a static site without a build)
	Port  int    // NextJS and Node; a static site listens on 8080
	// DataDir is where the app writes uploads in the container (NextJS and Node), under
	// /app; "" for none. The server mounts the site's persistent folder there.
	DataDir string
	Start   string // Node: the file node runs, e.g. server.js
	// BuildDir is a static site's build output (dist, out, build); "" publishes the
	// project folder as it is.
	BuildDir string
	// Arch is the server's architecture, which the image is built for: AMD64 ("" too)
	// or ARM64. The panel sets it from its own build, never from the form.
	Arch string
}

// Architectures a server runs on.
const (
	AMD64 = "amd64"
	ARM64 = "arm64"
)

var (
	reOwner = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	// A relative path of plain segments: none starts with "." (so no "." or ".."), and
	// none holds anything a shell, a Dockerfile or YAML would read specially.
	reRelPath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}(/[A-Za-z0-9_][A-Za-z0-9._-]{0,63}){0,7}$`)
	reStart   = regexp.MustCompile(`\.(js|mjs|cjs)$`)
)

// AppDir is where the Node images keep the app.
const AppDir = "/app"

// Check normalizes in (trims, picks defaults) and reports every problem in Turkish, for
// the panel's form. The returned Input is the one to render.
func Check(in Input) (Input, []string) {
	in.Owner = strings.TrimSpace(in.Owner)
	in.Name = strings.TrimSpace(in.Name)
	in.Start = strings.TrimSpace(in.Start)
	rawData, rawBuild := strings.TrimSpace(in.DataDir), strings.TrimSpace(in.BuildDir)
	in.DataDir = strings.TrimRight(rawData, "/")
	in.BuildDir = strings.Trim(rawBuild, "/")
	if in.PM == "" {
		in.PM = NPM
	}
	if in.Arch == "" {
		in.Arch = AMD64
	}
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	// "/" alone is a folder, not "none".
	if rawData != "" && in.DataDir == "" {
		in.DataDir = rawData
	}
	if rawBuild != "" && in.BuildDir == "" {
		in.BuildDir = rawBuild
	}

	switch in.Kind {
	case NextJS, Node, Static:
	default:
		return in, []string{"proje türü seçilmeli"}
	}
	if !reOwner.MatchString(in.Owner) || strings.Contains(in.Owner, "--") || strings.HasSuffix(in.Owner, "-") {
		add("GitHub hesabı: harf, rakam ve tek tire; en çok 39 karakter (ör. ornek-kullanici)")
	}
	if !sites.ValidName(in.Name) {
		add("proje adı 3-32 karakter: küçük harf, rakam ve tire; harfle başlamalı (ayrılmış adlar olmaz)")
	}
	if in.PM != NPM && in.PM != PNPM {
		add("paket yöneticisi npm ya da pnpm olmalı")
	}
	if in.Arch != AMD64 && in.Arch != ARM64 {
		add("sunucunun mimarisi amd64 ya da arm64 olmalı")
	}
	switch in.Kind {
	case Static:
		in.Port, in.DataDir, in.Start = DefaultPort(Static), "", ""
		if in.BuildDir != "" && !reRelPath.MatchString(in.BuildDir) {
			add("derleme klasörü proje içinde bir yol olmalı (ör. dist, out, build)")
		}
	default:
		in.BuildDir = ""
		if in.Port < 1024 || in.Port > 65535 {
			add("port 1024-65535 arasında olmalı (genelde 3000)")
		}
		if in.DataDir != "" {
			if msg := checkDataDir(in.DataDir); msg != "" {
				add("kalıcı klasör: %s", msg)
			}
		}
		if in.Kind == Node {
			if in.Start == "" {
				in.Start = "server.js"
			}
			if !reRelPath.MatchString(in.Start) || !reStart.MatchString(in.Start) {
				add("başlatma dosyası proje içinde bir .js / .mjs / .cjs dosyası olmalı (ör. server.js, src/index.js)")
			}
		} else {
			in.Start = ""
		}
	}
	return in, problems
}

// checkDataDir accepts a folder strictly under /app that does not hold the app itself:
// the persistent folder hides whatever the image has there.
func checkDataDir(d string) string {
	rel, ok := strings.CutPrefix(d, AppDir+"/")
	if !ok || !reRelPath.MatchString(rel) {
		return "/app altında bir yol olmalı (ör. /app/public/uploads, /app/uploads)"
	}
	first, _, _ := strings.Cut(rel, "/")
	switch {
	case rel == "public":
		return "/app/public'in tamamı olamaz (imajın kendi dosyalarını gizler); içinde bir klasör seç, ör. /app/public/uploads"
	case first == "node_modules":
		return "node_modules içinde olamaz"
	case !strings.Contains(rel, "/") && reStart.MatchString(rel):
		return "bir klasör olmalı"
	}
	return ""
}

// ImageOf is the image the workflow pushes, without a tag. GHCR wants it lower case.
func ImageOf(owner, name string) string {
	return "ghcr.io/" + strings.ToLower(owner) + "/" + name
}

// File is one generated file.
type File struct {
	Path string // relative to the project's root
	Body string
}

//go:embed files/*
var files embed.FS

// The templates use [[ ]]: the workflow itself is full of ${{ }}. They are parsed on
// first use: every FootOps process links this package, only the panel's page needs it.
var templates = sync.OnceValues(func() (*template.Template, error) {
	return template.New("").Delims("[[", "]]").Option("missingkey=error").ParseFS(files, "files/*")
})

// view is what the templates see.
type view struct {
	Input
	Image                      string
	Platform, Runner           string // what the image is built for, on which GitHub runner
	Lock, Install, InstallProd string
	Build                      string
	PublicEnv, Plain           bool
	DataIgnore                 string
}

func viewOf(in Input) view {
	v := view{Input: in, Image: ImageOf(in.Owner, in.Name), Platform: "linux/" + in.Arch, Runner: "ubuntu-latest",
		Lock: "package-lock.json", Install: "npm ci", InstallProd: "npm ci --omit=dev && npm cache clean --force", Build: "npm run build"}
	// An arm64 server's image is built on GitHub's arm64 runner, natively: emulating
	// arm64 on ubuntu-latest would take a Next.js build many times as long.
	if in.Arch == ARM64 {
		v.Runner = "ubuntu-24.04-arm"
	}
	if in.PM == PNPM {
		v.Lock = "pnpm-lock.yaml"
		v.Install = "corepack enable pnpm && pnpm install --frozen-lockfile"
		v.InstallProd = "corepack enable pnpm && pnpm install --frozen-lockfile --prod"
		v.Build = "corepack enable pnpm && pnpm run build"
	}
	// .env.production carries what a build bakes into the browser's code (NEXT_PUBLIC_*,
	// VITE_*). A Node server reads it at run time like any other .env, so it may hold
	// secrets; a static site without a build would publish it.
	v.PublicEnv = in.Kind == NextJS || (in.Kind == Static && in.BuildDir != "")
	v.Plain = in.Kind == Static && in.BuildDir == ""
	if rel, ok := strings.CutPrefix(in.DataDir, AppDir+"/"); ok {
		v.DataIgnore = rel
	}
	return v
}

// Files renders the three files. It checks in again and refuses rather than render an
// input Check did not pass.
func Files(in Input) ([]File, error) {
	in, problems := Check(in)
	if len(problems) > 0 {
		return nil, fmt.Errorf("geçersiz girdi: %s", strings.Join(problems, "; "))
	}
	tmpl, err := templates()
	if err != nil {
		return nil, err
	}
	v := viewOf(in)
	out := []File{{Path: "Dockerfile"}, {Path: ".dockerignore"}, {Path: ".github/workflows/docker.yml"}}
	for i, name := range []string{in.Kind + ".Dockerfile", "dockerignore", "docker.yml"} {
		var b bytes.Buffer
		if err := tmpl.ExecuteTemplate(&b, name, v); err != nil {
			return nil, err
		}
		out[i].Body = b.String()
	}
	return out, nil
}

// Zip writes the files as a zip to unpack in the project's root.
func Zip(w io.Writer, fs []File, modified time.Time) error {
	zw := zip.NewWriter(w)
	for _, f := range fs {
		h := &zip.FileHeader{Name: f.Path, Method: zip.Deflate, Modified: modified}
		h.SetMode(0o644)
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(fw, f.Body); err != nil {
			return err
		}
	}
	return zw.Close()
}
