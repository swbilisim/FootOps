package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
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
	removed, _ := s.agent.Removed(r.Context(), false) // a directory listing: cheap
	s.render(w, r, "sites", map[string]any{"Title": "Siteler", "Sites": list, "Err": errString(err), "Removed": len(removed)})
}

// siteEditForm is the edit form, kept as strings like siteForm. Env holds only new or
// changed variables: current values are never sent to the browser.
type siteEditForm struct {
	Domains, Image, Port, MemoryMB, MaxBodyMB, DataMount, Env string
	Unset                                                     map[string]bool
	EnvAck                                                    bool // the notes on Env were seen
}

func editFormFor(sp sites.Spec) siteEditForm {
	return siteEditForm{Domains: strings.Join(sp.Domains, "\n"), Image: sp.Image, Port: strconv.Itoa(sp.Port),
		MemoryMB: strconv.Itoa(sp.MemoryMB), MaxBodyMB: strconv.Itoa(sp.BodyMB()), DataMount: sp.DataMount, Unset: map[string]bool{}}
}

// bodyMB reads the largest request field: empty is the default (0 to the agent); a
// number must be 1-100 — 0 typed for "no uploads" is not taken for the default 100.
func bodyMB(v string) (int, error) {
	if v = strings.TrimSpace(v); v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > sites.DefaultBodyMB {
		return 0, errBodyMB
	}
	return n, nil
}

var errBodyMB = fmt.Errorf("En büyük istek 1-%d MB arasında bir sayı olmalı (boş bırakılırsa %d)", sites.DefaultBodyMB, sites.DefaultBodyMB)

func (s *Server) siteEdit(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	s.render(w, r, "site_edit", map[string]any{"Title": st.Spec.Name + " düzenle", "S": st.Spec, "F": editFormFor(st.Spec), "EnvFile": true,
		"Layer": s.layerOf(r, st.Spec, false)})
}

func (s *Server) siteUpdate(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	f := siteEditForm{
		Domains: r.PostFormValue("domains"), Image: r.PostFormValue("image"), Port: r.PostFormValue("port"),
		MemoryMB: r.PostFormValue("memory_mb"), MaxBodyMB: r.PostFormValue("max_body_mb"), DataMount: r.PostFormValue("data_mount"),
		Env: r.PostFormValue("env"), Unset: map[string]bool{}, EnvAck: r.PostFormValue("env_ack") == "on",
	}
	for _, k := range r.PostForm["unset"] {
		f.Unset[k] = true
	}
	var notes []sites.EnvRow
	var layer *layerView
	again := func(problems ...string) {
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_edit",
			map[string]any{"Title": st.Spec.Name + " düzenle", "S": st.Spec, "F": f, "Problems": problems, "EnvFile": true, "EnvNotes": notes,
				"Layer": layer})
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
	body, err := bodyMB(f.MaxBodyMB)
	if err != nil {
		again(err.Error())
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
	u := agent.SiteUpdate{Domains: strings.Fields(f.Domains), Image: f.Image, Port: port, MemoryMB: mem, MaxBodyMB: body,
		DataMount: f.DataMount, Set: set, Unset: sites.SortedKeys(boolKeys(f.Unset))}
	// The box counts for the update it was shown with: a form changed since is checked
	// again.
	force := forced(r) && r.PostFormValue("consent") == consentOf(u)
	id, err := s.agent.UpdateSite(r.Context(), st.Spec.Name, agent.UpdateSiteRequest{Update: u, Actor: s.actor(r), Force: force})
	if err != nil {
		// The change makes the container anew and its layer holds files: the same form,
		// with them and the box.
		if layer = layerRefused(err, st.Spec); layer != nil {
			layer.Required, layer.Consent = false, consentOf(u)
			again()
			return
		}
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
		if bst, err := s.agent.BackupStatus(r.Context()); err == nil {
			data["Backup"] = siteBackupSummary{Configured: bst.Configured, Ready: bst.Configured && !bst.Settings.Confirmed.IsZero(),
				Row: backupRowOf(bst, st)}
		}
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
	s.render(w, r, "site_move", map[string]any{"Title": st.Spec.Name + " grubunu değiştir", "S": st, "Groups": s.otherGroups(r, st.Spec.Group),
		"Layer": s.layerOf(r, st.Spec, true)})
}

// otherGroups are the groups a site in group g can move to.
func (s *Server) otherGroups(r *http.Request, g string) []string {
	groups, _ := s.agent.Groups(r.Context()) // without them the form still takes a group typed in
	return slices.DeleteFunc(groups, func(x string) bool { return x == g })
}

func (s *Server) siteMove(w http.ResponseWriter, r *http.Request) {
	st, ok := s.findSite(w, r)
	if !ok {
		return
	}
	group := r.PostFormValue("group")
	var layer *layerView
	again := func(problems ...string) {
		if layer == nil {
			layer = s.layerOf(r, st.Spec, true)
		}
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "site_move", map[string]any{"Title": st.Spec.Name + " grubunu değiştir",
			"S": st, "Groups": s.otherGroups(r, st.Spec.Group), "Group": group, "Problems": problems, "Layer": layer})
	}
	if r.PostFormValue("confirm") != st.Spec.Name {
		again("Onay için site adını aynen yaz.")
		return
	}
	id, err := s.agent.MoveSite(r.Context(), st.Spec.Name, group, st.Spec.Name, forced(r), s.actor(r))
	if layer = layerRefused(err, st.Spec); layer != nil {
		again()
		return
	}
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
		data := map[string]any{"S": st, "Verb": verb}
		if verb == "redeploy" || verb == "remove" {
			data["Layer"] = s.layerOf(r, st.Spec, verb == "redeploy")
		}
		s.render(w, r, "site_confirm", data)
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
				map[string]any{"S": st, "Verb": verb, "Problem": "Onay için site adını aynen yaz.", "Layer": s.layerOf(r, st.Spec, false)})
			return
		}
		id, err := s.agent.SiteJob(r.Context(), st.Spec.Name, verb, verb == "redeploy" && forced(r), s.actor(r))
		if layer := layerRefused(err, st.Spec); layer != nil {
			s.renderStatus(w, r, http.StatusConflict, "site_confirm", map[string]any{"S": st, "Verb": verb, "Layer": layer})
			return
		}
		if err != nil {
			s.sessions.setFlash(current(r).ID, err.Error())
			http.Redirect(w, r, "/sites/"+st.Spec.Name, http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/jobs/"+id, http.StatusSeeOther)
	}
}

// ---- what a site's container holds in its own layer (Faz L) ----

// layerView is the layer notice: the report, the site and its persistent folder (what
// to advise depends on it), and whether the form asks to go on anyway: its box sends
// force=1, required on a form that does nothing else, with the update it was shown for
// on the edit form (Consent).
type layerView struct {
	L             agent.LayerReport
	Site, Mount   string
	Ask, Required bool
	Consent       string
}

// layerOf is the notice for a site's container, nil when nothing is at stake. Nor when
// the agent cannot say: it checks again, and refuses, when the form is sent.
func (s *Server) layerOf(r *http.Request, spec sites.Spec, ask bool) *layerView {
	l, err := s.agent.SiteLayer(r.Context(), spec.Name)
	if err != nil || !l.Risky() {
		return nil
	}
	return &layerView{L: l, Site: spec.Name, Mount: spec.DataMount, Ask: ask, Required: ask}
}

// layerRefused is the agent's refusal to make a container anew, as a notice with the box.
func layerRefused(err error, spec sites.Spec) *layerView {
	var ae *agent.Error
	if errors.As(err, &ae) && ae.Layer != nil {
		return &layerView{L: *ae.Layer, Site: spec.Name, Mount: spec.DataMount, Ask: true, Required: true}
	}
	return nil
}

// consentOf names an update, so the box ticked for it counts for it alone.
func consentOf(u agent.SiteUpdate) string {
	b, _ := json.Marshal(u)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// forced reports whether the form's box said go on though files are lost.
func forced(r *http.Request) bool { return r.PostFormValue("force") == "1" }

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
	// Backup jobs belong to the backup pages: the server's, or a site's for its own run
	// (other targets — "gece yedeği", a bucket — are no site names).
	if strings.HasPrefix(j.Kind, "backup.") {
		data["Back"], data["BackLabel"], data["Nav"] = "/backup", "Yedekleme", "backup"
		if j.Kind != "backup.setup" && docker.ValidName(j.Target) {
			data["Back"], data["BackLabel"], data["Nav"] = "/sites/"+j.Target+"/backup", j.Target+" yedekleri", "sites"
		}
	}
	// The server's jobs belong to its pages; their targets — the images, a release, a
	// bucket — are no site names either.
	switch j.Kind {
	case "images.prune":
		data["Back"], data["BackLabel"], data["Nav"] = "/server/check", "Sunucu kontrolü", "server"
	case "server.update":
		data["Back"], data["BackLabel"], data["Nav"] = "/server/update", "Güncelleme", "server"
	case "server.recover":
		data["Back"], data["BackLabel"], data["Nav"] = "/backup", "Yedekleme", "backup"
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
