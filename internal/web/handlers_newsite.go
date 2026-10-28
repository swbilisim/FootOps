package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/sites"
)

// A new site takes three steps: the image (checked, if wanted, by pulling it), the form
// (filled in from the template and the check) and a review of what will be created.

// siteTemplate fills in the form for a kind of app. Nothing of it is stored.
type siteTemplate struct {
	ID, Label, Hint string
	Port, MemoryMB  int
	Database        bool
}

var siteTemplates = []siteTemplate{
	{"nextjs", "Next.js", "standalone derleme (node server.js), veritabanı MONGODB_URI'de", 3000, 512, true},
	{"node", "Node.js / Express", "Express ya da benzeri bir Node sunucusu", 3000, 384, true},
	{"static", "Statik site (nginx)", "nginx-unprivileged; kalıcı klasör web kökü olabilir", 8080, 64, false},
	{"other", "Diğer", "", 3000, 512, false},
}

func templateByID(id string) siteTemplate {
	for _, t := range siteTemplates {
		if t.ID == id {
			return t
		}
	}
	return siteTemplates[len(siteTemplates)-1]
}

// siteForm is the new-site form, kept as strings so a rejected form is shown again
// exactly as typed.
type siteForm struct {
	Template, Check                                                    string
	Name, Group, Domains, Image, Port, MemoryMB, DataMount, DBVar, Env string
	Database                                                           bool
}

func formFor(tpl siteTemplate, image string, info *agent.ImageInfo) siteForm {
	f := siteForm{Template: tpl.ID, Image: image, Port: strconv.Itoa(tpl.Port), MemoryMB: strconv.Itoa(tpl.MemoryMB),
		Database: tpl.Database, DBVar: "MONGODB_URI"}
	if info != nil {
		f.Image = info.Image
		if p := info.SuggestPort(); p > 0 {
			f.Port = strconv.Itoa(p)
		}
		if m := info.SuggestMount(); m != "" {
			f.DataMount = m
		}
	}
	return f
}

// ---- step 1: the image ----

func (s *Server) siteNew(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tpl := siteTemplates[0].ID // most sites here are Next.js
	if q.Has("tpl") {
		tpl = templateByID(q.Get("tpl")).ID
	}
	s.render(w, r, "site_new", map[string]any{"Title": "Yeni site", "Templates": siteTemplates, "Tpl": tpl, "Image": q.Get("image")})
}

// siteNewImage starts the image check, or skips it.
func (s *Server) siteNewImage(w http.ResponseWriter, r *http.Request) {
	tpl := templateByID(r.PostFormValue("template")).ID
	image := strings.TrimSpace(r.PostFormValue("image"))
	if r.PostFormValue("action") == "skip" {
		http.Redirect(w, r, "/sites/new/form?"+url.Values{"tpl": {tpl}, "image": {image}}.Encode(), http.StatusSeeOther)
		return
	}
	id, err := s.agent.CheckImage(r.Context(), image, s.actor(r))
	if err != nil {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_new", map[string]any{"Title": "Yeni site",
			"Templates": siteTemplates, "Tpl": tpl, "Image": image, "Problems": problemsOf(err)})
		return
	}
	http.Redirect(w, r, "/jobs/"+id+"?"+url.Values{"tpl": {tpl}}.Encode(), http.StatusSeeOther)
}

// imageCheck reads a finished image check job of this image; nil if there is none, it
// failed, or the image was changed in the form after the check.
func (s *Server) imageCheck(r *http.Request, id, image string) *agent.ImageInfo {
	if id == "" || !reJobID.MatchString(id) {
		return nil
	}
	j, err := s.agent.Job(r.Context(), id)
	if err != nil || j.Kind != "image.check" || !j.Done || j.Err != "" {
		return nil
	}
	var info agent.ImageInfo
	if json.Unmarshal([]byte(j.Result), &info) != nil || (image != "" && info.Image != strings.TrimSpace(image)) {
		return nil
	}
	return &info
}

// ---- step 2: the form ----

func (s *Server) siteNewForm(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	info := s.imageCheck(r, q.Get("check"), "")
	f := formFor(templateByID(q.Get("tpl")), strings.TrimSpace(q.Get("image")), info)
	if info != nil {
		f.Check = q.Get("check")
	}
	s.renderForm(w, r, http.StatusOK, f, info, nil)
}

func (s *Server) renderForm(w http.ResponseWriter, r *http.Request, status int, f siteForm, info *agent.ImageInfo, problems []string) {
	s.renderStatus(w, r, status, "site_form", map[string]any{"Title": "Yeni site", "F": f, "Groups": sites.Groups,
		"DBVars": sites.DBVars, "Info": info, "Tpl": templateByID(f.Template), "Problems": problems, "EnvFile": true})
}

func readSiteForm(r *http.Request) siteForm {
	return siteForm{
		Template: r.PostFormValue("template"), Check: r.PostFormValue("check"),
		Name: r.PostFormValue("name"), Group: r.PostFormValue("group"), Domains: r.PostFormValue("domains"),
		Image: r.PostFormValue("image"), Port: r.PostFormValue("port"), MemoryMB: r.PostFormValue("memory_mb"),
		DataMount: r.PostFormValue("data_mount"), DBVar: r.PostFormValue("db_var"), Env: r.PostFormValue("env"),
		Database: r.PostFormValue("database") == "on",
	}
}

// siteNewReview validates the form and keeps it for the review page.
func (s *Server) siteNewReview(w http.ResponseWriter, r *http.Request) {
	f := readSiteForm(r)
	again := func(problems ...string) {
		s.renderForm(w, r, http.StatusUnprocessableEntity, f, s.imageCheck(r, f.Check, f.Image), problems)
	}
	entries, err := sites.ParseDotenv(f.Env)
	if err != nil {
		again("Ortam değişkenleri: " + err.Error())
		return
	}
	port, perr := strconv.Atoi(strings.TrimSpace(f.Port))
	mem, merr := strconv.Atoi(strings.TrimSpace(f.MemoryMB))
	if perr != nil || merr != nil {
		again("Port ve bellek sayı olmalı")
		return
	}
	spec := sites.Spec{
		Name: f.Name, Group: f.Group, Domains: strings.Fields(f.Domains), Image: f.Image,
		Port: port, MemoryMB: mem, DataMount: f.DataMount, Database: f.Database, DBVar: f.DBVar,
	}
	spec.Normalize()
	// A local .env usually has its own database address (localhost): the site gets the
	// panel's instead, in the variable the form names.
	var notes []string
	if spec.Database {
		entries = slices.DeleteFunc(entries, func(e sites.EnvEntry) bool {
			if e.Key != spec.DBVar {
				return false
			}
			notes = append(notes, fmt.Sprintf("%s ortam değişkenlerinde vardı (satır %d); site veritabanı adresini panelden alacak, o satır kullanılmadı", e.Key, e.Line))
			return true
		})
	}
	req := agent.CreateSiteRequest{Spec: spec, Env: sites.EnvMap(entries), Actor: s.actor(r)}
	checked, err := s.agent.CheckSite(r.Context(), req)
	if err != nil {
		again(problemsOf(err)...)
		return
	}
	req.Spec = checked
	id := s.pending.put(&pendingSite{session: current(r).ID, form: f, req: req, entries: entries, notes: notes})
	http.Redirect(w, r, "/sites/new/review/"+id, http.StatusSeeOther)
}

// ---- step 3: review and create ----

// reviewOf finds the pending request named in the URL, or answers itself.
func (s *Server) reviewOf(w http.ResponseWriter, r *http.Request, take bool) (*pendingSite, bool) {
	get := s.pending.get
	if take {
		get = s.pending.take
	}
	ps, ok := get(current(r).ID, r.PathValue("id"))
	if !ok {
		s.sessions.setFlash(current(r).ID, "Bu özetin süresi doldu ya da kullanıldı; formu yeniden doldur.")
		http.Redirect(w, r, "/sites/new", http.StatusSeeOther)
		return nil, false
	}
	return ps, true
}

// domainCert pairs a domain with the certificate that covers it.
type domainCert struct {
	Domain string
	Cert   string
}

func (s *Server) siteNewReviewPage(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.reviewOf(w, r, false)
	if !ok {
		return
	}
	sp := ps.req.Spec
	certs, _ := s.agent.Certs(r.Context())
	certFor, _ := sites.CertFor(sp.Domains, certs)
	var dc []domainCert
	for _, d := range sp.Domains {
		dc = append(dc, domainCert{d, strings.Join(certFor[d].Names, ", ")})
	}
	info := s.imageCheck(r, ps.form.Check, sp.Image)
	var warnings []string
	if info != nil {
		if len(info.Ports) > 0 && !slices.Contains(info.Ports, sp.Port) {
			warnings = append(warnings, fmt.Sprintf("İmaj %s portunu bildiriyor (EXPOSE), formdaki uygulama portu %d: uygulama hangisini dinliyorsa o olmalı.", joinInts(info.Ports), sp.Port))
		}
		if info.UID == 0 && sp.DataMount != "" {
			warnings = append(warnings, "Uygulama container'da root olarak çalışıyor: kalıcı klasör root'a ait olur (yetkiler yine kısıtlı).")
		}
	}
	srv, _ := s.agent.Server(r.Context())
	s.render(w, r, "site_review", map[string]any{"Title": "Yeni site: özet", "ID": r.PathValue("id"), "S": sp,
		"Env": sites.DescribeEnv(ps.entries, sp), "Notes": ps.notes, "Info": info, "Warnings": warnings,
		"Domains": dc, "DNS": dnsRecords(sp.Domains, certs, firstOf(srv.IPv4))})
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}

func firstOf(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

// siteNewCreate creates the reviewed site, or goes back to the form to change it.
func (s *Server) siteNewCreate(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("action") == "edit" {
		ps, ok := s.reviewOf(w, r, true)
		if !ok {
			return
		}
		s.renderForm(w, r, http.StatusOK, ps.form, s.imageCheck(r, ps.form.Check, ps.form.Image), nil)
		return
	}
	ps, ok := s.reviewOf(w, r, true)
	if !ok {
		return
	}
	ps.req.Actor = s.actor(r)
	id, err := s.agent.CreateSite(r.Context(), ps.req)
	if err != nil {
		// Something changed since the review (a name or domain taken meanwhile): the
		// form comes back as typed.
		s.renderForm(w, r, http.StatusUnprocessableEntity, ps.form, s.imageCheck(r, ps.form.Check, ps.form.Image), problemsOf(err))
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}
