package web

import (
	"bytes"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/scaffold"
)

// "Projeni hazırla" (Faz R1): the files a project needs before the panel can run it.
// Nothing is kept: the page and the zip are rendered from the query on each request,
// and nothing on the server changes, so both are plain GETs.

// projectForm is the form as typed, shown again as it was.
type projectForm struct {
	Kind, Owner, Name, PM, Port, Data, Start, Build string
}

func readProjectForm(q url.Values) projectForm {
	kind := q.Get("kind")
	if !isKind(kind) {
		kind = scaffold.NextJS
	}
	f := projectForm{Kind: kind, Owner: q.Get("owner"), Name: q.Get("name"), PM: q.Get("pm"), Port: q.Get("port"),
		Data: q.Get("data"), Start: q.Get("start"), Build: q.Get("build")}
	if f.PM == "" {
		f.PM = scaffold.NPM
	}
	if f.Port == "" {
		f.Port = strconv.Itoa(scaffold.DefaultPort(kind))
	}
	return f
}

func isKind(k string) bool {
	for _, v := range scaffold.Kinds {
		if v == k {
			return true
		}
	}
	return false
}

func (f projectForm) input() scaffold.Input {
	port, err := strconv.Atoi(strings.TrimSpace(f.Port))
	if err != nil {
		port = 0 // Check says what a port must be
	}
	return scaffold.Input{Kind: f.Kind, Owner: f.Owner, Name: f.Name, PM: f.PM, Port: port, DataDir: f.Data, Start: f.Start, BuildDir: f.Build}
}

// query is the form as a URL query, for the kind links and the zip.
func (f projectForm) query() url.Values {
	q := url.Values{"kind": {f.Kind}, "owner": {f.Owner}, "name": {f.Name}, "pm": {f.PM}}
	switch f.Kind {
	case scaffold.Static:
		q.Set("build", f.Build)
	default:
		q.Set("port", f.Port)
		q.Set("data", f.Data)
		if f.Kind == scaffold.Node {
			q.Set("start", f.Start)
		}
	}
	return q
}

type kindLink struct {
	Label, URL string
	Selected   bool
}

var kindLabels = map[string]string{scaffold.NextJS: "Next.js", scaffold.Node: "Node.js / Express", scaffold.Static: "Statik site"}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := readProjectForm(q)
	if !q.Has("owner") {
		f.Owner = s.defaultOwner(r)
	}
	var kinds []kindLink
	for _, k := range scaffold.Kinds {
		// Switching kind keeps the account and the name; the rest belongs to a kind.
		u := url.Values{"kind": {k}, "owner": {f.Owner}, "name": {f.Name}, "pm": {f.PM}}
		kinds = append(kinds, kindLink{kindLabels[k], "/sites/new/project?" + u.Encode(), k == f.Kind})
	}
	data := map[string]any{"Title": "Projeni hazırla", "F": f, "Kinds": kinds, "KindLabel": kindLabels[f.Kind], "Copy": true}
	status := http.StatusOK
	if q.Get("go") != "" {
		in, problems := scaffold.Check(f.input())
		if len(problems) > 0 {
			data["Problems"], status = problems, http.StatusUnprocessableEntity
		} else if files, err := scaffold.Files(in); err != nil {
			data["Problems"], status = []string{err.Error()}, http.StatusUnprocessableEntity
		} else {
			image := scaffold.ImageOf(in.Owner, in.Name) + ":main"
			data["In"], data["Files"], data["Image"] = in, files, image
			data["ZipURL"] = "/sites/new/project/zip?" + f.query().Encode()
			data["NewSiteURL"] = "/sites/new?" + url.Values{"tpl": {in.Kind}, "image": {image}}.Encode()
			data["PublicEnv"] = in.Kind == scaffold.NextJS || in.BuildDir != ""
		}
	}
	s.renderStatus(w, r, status, "project", data)
}

// projectZip answers the three files as a zip to unpack in the project's root.
func (s *Server) projectZip(w http.ResponseWriter, r *http.Request) {
	in, problems := scaffold.Check(readProjectForm(r.URL.Query()).input())
	if len(problems) > 0 {
		http.Error(w, "geçersiz girdi: "+strings.Join(problems, "; "), http.StatusBadRequest)
		return
	}
	files, err := scaffold.Files(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var b bytes.Buffer
	if err := scaffold.Zip(&b, files, s.now()); err != nil {
		http.Error(w, "zip oluşturulamadı", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	// The name passed Check: lower-case letters, digits and hyphens.
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": in.Name + "-footops.zip"}))
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	_, _ = b.WriteTo(w)
}

// defaultOwner guesses the GitHub account new projects go under: the one most of this
// server's sites pull from, else the account of its GHCR login.
func (s *Server) defaultOwner(r *http.Request) string {
	if list, err := s.agent.Sites(r.Context()); err == nil {
		count := map[string]int{}
		for _, st := range list {
			if o, ok := ghcrOwner(st.Spec.Image); ok {
				count[o]++
			}
		}
		owners := make([]string, 0, len(count))
		for o := range count {
			owners = append(owners, o)
		}
		sort.Slice(owners, func(i, j int) bool {
			if count[owners[i]] != count[owners[j]] {
				return count[owners[i]] > count[owners[j]]
			}
			return owners[i] < owners[j]
		})
		if len(owners) > 0 {
			return owners[0]
		}
	}
	if regs, err := s.agent.Registries(r.Context()); err == nil {
		for _, l := range regs {
			if l.Host == "ghcr.io" && l.User != "" {
				return l.User
			}
		}
	}
	return ""
}

func ghcrOwner(image string) (string, bool) {
	rest, ok := strings.CutPrefix(image, "ghcr.io/")
	if !ok {
		return "", false
	}
	owner, _, ok := strings.Cut(rest, "/")
	return owner, ok && owner != ""
}

// tokenURL opens GitHub's form for a classic token that can only read packages, named
// after this server.
func (s *Server) tokenURL() string {
	return "https://github.com/settings/tokens/new?" + url.Values{"scopes": {"read:packages"}, "description": {"FootOps " + s.cfg.ServerCode}}.Encode()
}
