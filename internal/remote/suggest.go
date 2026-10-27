package remote

import (
	"path"
	"sort"
	"strconv"
	"strings"
)

// Suggestion is the wizard's best guess of what to move for a site, from the source's
// web server configuration, its processes and its databases. The operator confirms or
// changes it.
type Suggestion struct {
	Web        []WebServer `json:"web"`        // server blocks answering the site's domains
	Ports      []int       `json:"ports"`      // local ports they proxy to
	Procs      []Proc      `json:"procs"`      // processes listening on those ports
	Containers []Container `json:"containers"` // containers publishing those ports
	AppDir     string      `json:"app_dir,omitempty"`
	FilesDir   string      `json:"files_dir,omitempty"`
	DB         *DBChoice   `json:"db,omitempty"`
}

// DBChoice is a database on the source: on the host (Container "") or in a container.
type DBChoice struct {
	Container string `json:"container,omitempty"`
	Name      string `json:"name"`
	Why       string `json:"why,omitempty"`
}

// Suggest matches a site (its name and domains) against what the source runs.
func Suggest(d Discovery, sc *Scan, site string, domains []string) Suggestion {
	var s Suggestion
	for _, w := range d.Web {
		for _, dom := range domains {
			if w.Matches(dom) || w.Matches("www."+dom) {
				s.Web = append(s.Web, w)
				break
			}
		}
	}
	ports := map[int]bool{}
	for _, w := range s.Web {
		for _, l := range w.Locations {
			if l.Proxy != "" {
				for _, p := range ProxyPorts(l.Proxy, d.Upstreams) {
					ports[p] = true
				}
			}
		}
	}
	for p := range ports {
		s.Ports = append(s.Ports, p)
	}
	sort.Ints(s.Ports)
	for _, p := range d.Procs {
		if p.Daemon {
			continue
		}
		hit := false
		for _, port := range p.Ports {
			hit = hit || ports[port]
		}
		if n, err := strconv.Atoi(p.Port); err == nil && ports[n] {
			hit = true
		}
		if hit {
			s.Procs = append(s.Procs, p)
		}
	}
	for _, c := range d.Containers {
		for p := range ports {
			if strings.Contains(c.Ports, ":"+strconv.Itoa(p)+"->") {
				s.Containers = append(s.Containers, c)
				break
			}
		}
	}

	switch {
	case len(s.Procs) > 0:
		s.AppDir = s.Procs[0].Cwd
	case len(s.Containers) > 0:
		for _, m := range s.Containers[0].Mounts {
			if strings.HasPrefix(m.Source, "/") && s.AppDir == "" {
				s.AppDir = m.Source
			}
		}
	case len(s.Web) > 0 && s.Web[0].Root != "":
		s.AppDir = s.Web[0].Root
	}
	if sc != nil && sc.Dir != "" && s.AppDir == "" {
		s.AppDir = sc.Dir
	}
	s.FilesDir = filesDir(d, s)
	s.DB = suggestDB(d, sc, site, domains)
	return s
}

// filesDir picks the uploads folder: a candidate inside the app folder (the one with
// the most files), else a location serving /uploads from disk.
func filesDir(d Discovery, s Suggestion) string {
	best, bestN := "", int64(-1)
	if s.AppDir != "" {
		for _, c := range d.Candidates {
			if (c.Dir == s.AppDir || strings.HasPrefix(c.Dir, strings.TrimSuffix(s.AppDir, "/")+"/")) && c.Files > bestN {
				best, bestN = c.Dir, c.Files
			}
		}
	}
	if best != "" {
		return best
	}
	for _, w := range s.Web {
		for _, l := range w.Locations {
			p := strings.Fields(l.Path)
			if len(p) == 0 || !strings.Contains(p[len(p)-1], "upload") {
				continue
			}
			if l.Alias != "" {
				return strings.TrimSuffix(l.Alias, "/")
			}
			if l.Root != "" {
				return path.Join(l.Root, strings.Trim(p[len(p)-1], "/^~*$"))
			}
		}
	}
	return ""
}

// suggestDB prefers a database the app's code names; else the one whose name is most
// like the site's name or domain.
func suggestDB(d Discovery, sc *Scan, site string, domains []string) *DBChoice {
	type cand struct {
		container, name string
	}
	var all []cand
	for _, m := range d.Mongo {
		for _, db := range m.DBs {
			if ValidDB(db.Name) {
				all = append(all, cand{m.Container, db.Name})
			}
		}
	}
	if sc != nil {
		for _, r := range sc.DBRefs {
			for _, c := range all {
				if c.name == r.Value {
					return &DBChoice{Container: c.container, Name: c.name, Why: "uygulamanın kodunda geçiyor"}
				}
			}
		}
	}
	keys := []string{strings.ToLower(site)}
	for _, dom := range domains {
		label := strings.ToLower(strings.Split(strings.TrimPrefix(dom, "www."), ".")[0])
		keys = append(keys, label)
	}
	var best *DBChoice
	bestScore, tie := 0, false
	for _, c := range all {
		score := 0
		for _, k := range keys {
			score = max(score, commonRun(strings.ToLower(c.name), k))
		}
		switch {
		case score > bestScore:
			best, bestScore, tie = &DBChoice{Container: c.container, Name: c.name, Why: "adı siteye benziyor"}, score, false
		case score == bestScore:
			tie = true
		}
	}
	if best == nil || bestScore < 5 || tie {
		return nil
	}
	return best
}

// commonRun is the length of the longest common substring of a and b.
func commonRun(a, b string) int {
	if len(a) > 64 || len(b) > 64 {
		a, b = a[:min(len(a), 64)], b[:min(len(b), 64)]
	}
	best := 0
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				best = max(best, cur[j])
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
	}
	return best
}
