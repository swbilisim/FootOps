package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
)

var (
	reJobID  = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	reCertID = regexp.MustCompile(`^[a-f0-9]{16}$`)
)

func (s *Server) sitesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.Sites(r.Context())
	s.render(w, r, "sites", map[string]any{"Title": "Siteler", "Sites": list, "Err": errString(err)})
}

// siteEditForm is the edit form, kept as strings like siteForm. Env holds only new or
// changed variables: current values are never sent to the browser.
type siteEditForm struct {
	Domains, Image, Port, MemoryMB, DataMount, Env string
	Unset                                          map[string]bool
	EnvAck                                         bool // the notes on Env were seen
}

func editFormFor(sp sites.Spec) siteEditForm {
	return siteEditForm{Domains: strings.Join(sp.Domains, "\n"), Image: sp.Image, Port: strconv.Itoa(sp.Port),
		MemoryMB: strconv.Itoa(sp.MemoryMB), DataMount: sp.DataMount, Unset: map[string]bool{}}
}

func (s *Server) siteEdit(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	s.render(w, r, "site_edit", map[string]any{"Title": st.Spec.Name + " düzenle", "S": st.Spec, "F": editFormFor(st.Spec), "EnvFile": true})
}

func (s *Server) siteUpdate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	f := siteEditForm{
		Domains: r.PostFormValue("domains"), Image: r.PostFormValue("image"), Port: r.PostFormValue("port"),
		MemoryMB: r.PostFormValue("memory_mb"), DataMount: r.PostFormValue("data_mount"), Env: r.PostFormValue("env"),
		Unset: map[string]bool{}, EnvAck: r.PostFormValue("env_ack") == "on",
	}
	for _, k := range r.PostForm["unset"] {
		f.Unset[k] = true
	}
	var notes []sites.EnvRow
	again := func(problems ...string) {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_edit",
			map[string]any{"Title": st.Spec.Name + " düzenle", "S": st.Spec, "F": f, "Problems": problems, "EnvFile": true, "EnvNotes": notes})
	}
	entries, err := sites.ParseDotenv(f.Env)
	if err != nil {
		again("Ortam değişkenleri: " + err.Error())
		return
	}
	set := sites.EnvMap(entries)
	port, perr := strconv.Atoi(strings.TrimSpace(f.Port))
	mem, merr := strconv.Atoi(strings.TrimSpace(f.MemoryMB))
	if perr != nil || merr != nil {
		again("Port ve bellek sayı olmalı")
		return
	}
	// A live site gets no review page: what the new site flow would warn about (a cut
	// comment, turned escapes, localhost, a development mode) is shown here and must be
	// acknowledged once.
	sp := st.Spec
	sp.Port = port
	for _, row := range sites.DescribeEnv(entries, sp) {
		if len(row.Notes) > 0 {
			notes = append(notes, row)
		}
	}
	if len(notes) > 0 && !f.EnvAck {
		again("Ortam değişkenlerinde dikkat edilecekler var: aşağıdaki notlara bak, uygunsa “Gördüm” kutusunu işaretleyip tekrar kaydet.")
		return
	}
	u := agent.SiteUpdate{Domains: strings.Fields(f.Domains), Image: f.Image, Port: port, MemoryMB: mem,
		DataMount: f.DataMount, Set: set, Unset: sites.SortedKeys(boolKeys(f.Unset))}
	id, err := s.agent.UpdateSite(r.Context(), st.Spec.Name, agent.UpdateSiteRequest{Update: u, Actor: s.actor(r)})
	if err != nil {
		var ae *agent.Error
		if errors.As(err, &ae) && len(ae.Problems) > 0 {
			again(ae.Problems...)
			return
		}
		again(err.Error())
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// boolKeys turns a set into a map SortedKeys accepts.
func boolKeys(m map[string]bool) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v {
			out[k] = ""
		}
	}
	return out
}

func (s *Server) site(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	srv, _ := s.agent.Server(r.Context())
	data := map[string]any{"Title": st.Spec.Name, "S": st, "Server": srv, "Live": liveEvery,
		"History": s.containerHistory(r, st.Spec.Container(), uint64(st.Spec.MemoryMB)<<20)}
	if st.Spec.Database && !lightRefresh(r) {
		snaps, err := s.agent.Snapshots(r.Context(), st.Spec.Name)
		data["Snapshots"], data["SnapshotsErr"], data["KeepDays"] = snaps, errString(err), int(agent.SnapshotKeep.Hours()/24)
	}
	if (st.Spec.Database || st.Spec.DataMount != "") && !lightRefresh(r) {
		data["Exports"], _ = s.agent.Exports(r.Context(), st.Spec.Name)
	}
	if st.Spec.DataMount != "" && !lightRefresh(r) {
		data["FilesBackups"], _ = s.agent.FilesBackups(r.Context(), st.Spec.Name)
		data["FilesKeepDays"] = int(agent.FilesKeep.Hours() / 24)
	}
	if !lightRefresh(r) {
		certs, _ := s.agent.Certs(r.Context())
		data["DNS"] = dnsRecords(st.Spec.Domains, certs, firstOf(srv.IPv4))
	}
	s.render(w, r, "site", data)
}

// findSite looks the site up via the agent; it writes a 404 if there is none.
func (s *Server) findSite(w http.ResponseWriter, r *http.Request) (agent.SiteStatus, bool) {
	name := r.PathValue("name")
	if !docker.ValidName(name) {
		http.NotFound(w, r)
		return agent.SiteStatus{}, false
	}
	list, err := s.agent.Sites(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return agent.SiteStatus{}, false
	}
	for _, st := range list {
		if st.Spec.Name == name {
			return st, true
		}
	}
	http.NotFound(w, r)
	return agent.SiteStatus{}, false
}

// ---- moving a site to another group ----

func (s *Server) siteMoveForm(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	s.render(w, r, "site_move", map[string]any{"Title": st.Spec.Name + " grubunu değiştir", "S": st, "Groups": sites.Groups})
}

func (s *Server) siteMove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	group := r.PostFormValue("group")
	again := func(problems ...string) {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_move", map[string]any{"Title": st.Spec.Name + " grubunu değiştir",
			"S": st, "Groups": sites.Groups, "Group": group, "Problems": problems})
	}
	if r.PostFormValue("confirm") != st.Spec.Name {
		again("Onay için site adını aynen yaz.")
		return
	}
	id, err := s.agent.MoveSite(r.Context(), st.Spec.Name, group, st.Spec.Name, s.actor(r))
	if err != nil {
		again(problemsOf(err)...)
		return
	}
	http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
}

// siteDNS checks the site's domains when asked; nothing runs in the background.
func (s *Server) siteDNS(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	checks, err := s.agent.SiteDNS(r.Context(), st.Spec.Name)
	certs, _ := s.agent.Certs(r.Context())
	srv, _ := s.agent.Server(r.Context())
	s.render(w, r, "site_dns", map[string]any{"Title": st.Spec.Name + " DNS", "S": st, "Checks": checks, "Err": errString(err),
		"Now": s.now(), "DNS": dnsRecords(st.Spec.Domains, certs, firstOf(srv.IPv4))})
}

func (s *Server) siteConfirm(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.findSite(w, r)
		if !ok {
			return
		}
		s.render(w, r, "site_confirm", map[string]any{"S": st, "Verb": verb})
	}
}

func (s *Server) siteAction(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, ok := s.findSite(w, r)
		if !ok {
			return
		}
		// Removal is confirmed by typing the site's name, not by a single click.
		if verb == "remove" && r.PostFormValue("confirm") != st.Spec.Name {
			s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_confirm",
				map[string]any{"S": st, "Verb": verb, "Problem": "Onay için site adını aynen yaz."})
			return
		}
		id, err := s.agent.SiteJob(r.Context(), st.Spec.Name, verb, s.actor(r))
		if err != nil {
			s.sessions.setFlash(current(r).ID, err.Error())
			http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
	}
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reJobID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	j, err := s.agent.Job(r.Context(), id)
	data := map[string]any{"Title": "İşlem", "J": j, "Err": errString(err), "Tpl": templateByID(r.URL.Query().Get("tpl")).ID}
	if err == nil && !j.Done {
		data["Refresh"], data["Live"] = 2, 2
	}
	s.render(w, r, "job", data)
}

// ---- certificates ----

func (s *Server) certs(w http.ResponseWriter, r *http.Request) {
	list, err := s.agent.Certs(r.Context())
	s.render(w, r, "certs", map[string]any{"Title": "Sertifikalar", "Certs": list, "Err": errString(err), "Now": time.Now()})
}

func (s *Server) certInstall(w http.ResponseWriter, r *http.Request) {
	c, err := s.agent.InstallCert(r.Context(), r.PostFormValue("cert"), r.PostFormValue("key"), s.actor(r))
	msg := "Sertifika yüklendi: " + strings.Join(c.Names, ", ")
	if err != nil {
		msg = "Sertifika yüklenemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/certs", http.StatusSeeOther)
}

func (s *Server) certRemoveConfirm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reCertID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "cert_remove", map[string]any{"ID": id})
}

func (s *Server) certRemove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !reCertID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	msg := "Sertifika silindi."
	if err := s.agent.RemoveCert(r.Context(), id, s.actor(r)); err != nil {
		msg = "Silinemedi: " + err.Error()
	}
	s.sessions.setFlash(current(r).ID, msg)
	http.Redirect(w, r, "/certs", http.StatusSeeOther)
}
