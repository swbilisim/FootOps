package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/swbilisim/FootOps/internal/docker"
	"github.com/swbilisim/FootOps/internal/sites"
	"github.com/swbilisim/FootOps/internal/staging"
)

// ---- the container's own layer (Faz L) ----

// LayerReport is what a site's container wrote outside its persistent folder: what goes
// when the container is made anew (a redeploy, an update, a move, a removal).
type LayerReport struct {
	Files int      `json:"files"`           // files added or changed
	Paths []string `json:"paths,omitempty"` // where, up to layerShown: a folder with its count, or a file
	More  int      `json:"more,omitempty"`  // places not listed
	Err   string   `json:"error,omitempty"` // the layer could not be read: nothing is known
}

// Risky reports whether making the container anew may lose something.
func (r LayerReport) Risky() bool { return r.Files > 0 || r.Err != "" }

// Summary is the report in a line.
func (r LayerReport) Summary() string {
	if r.Err != "" {
		return "container'ın katmanı okunamadı: " + r.Err
	}
	s := fmt.Sprintf("kalıcı klasör dışında %d dosya (%s", r.Files, strings.Join(r.Paths, ", "))
	if r.More > 0 {
		s += fmt.Sprintf(" ve %d yer daha", r.More)
	}
	return s + ")"
}

// layerShown is how many places a report lists; layerStats how many single changed paths
// it asks dockerd about (a folder is no file: one changed by a file made and deleted
// again, an empty one made at start).
const (
	layerShown = 10
	layerStats = 50
)

// layerIgnored are paths made again or of no worth: temporary and run-time files, caches,
// logs, what the system mounts, the configuration entrypoints write at each start (nginx's
// conf.d, users). layerIgnoredNames are such files and folders wherever they are: build
// and package caches, compiled Python, PM2's logs, shell histories.
var (
	layerIgnored = []string{"/tmp", "/var/tmp", "/run", "/var/run", "/var/cache", "/var/log", "/var/lib/nginx",
		"/proc", "/sys", "/dev", "/etc"}
	layerIgnoredNames = []string{"/.next/", "/__pycache__/", "/.npm/", "/.cache/", "/.pm2/",
		"/.ash_history/", "/.bash_history/", "/.wget-hsts/"}
)

// layerFiles reports the files (leaves: nothing changed below them) a container's layer
// added or changed, leaving out the ignored ones, what is under trees (the container's
// mounts, a new persistent folder about to be filled from it) and point (the persistent
// folder's mountpoint). Deleted paths come back with the image. A folder the container
// made stands for what is in it, as does a folder of the image several files went into;
// a path isDir says is a folder is no file.
func layerFiles(changes []docker.Change, isDir func(string) bool, point string, trees ...string) LayerReport {
	kind := make(map[string]int, len(changes))
	for _, c := range changes {
		kind[path.Clean(c.Path)] = c.Kind
	}
	above := map[string]bool{} // has a change below it
	for p := range kind {
		for d := path.Dir(p); d != "/" && d != "." && !above[d]; d = path.Dir(d) {
			above[d] = true
		}
	}
	if point != "" {
		point = path.Clean(point)
	}
	clean := make([]string, len(trees))
	for i, t := range trees {
		clean[i] = path.Clean(t)
	}
	type place struct {
		n    int
		made bool   // a folder the container made
		leaf string // the path, when it is the only one
	}
	var leaves []string
	for p, k := range kind {
		if k == docker.ChangeDeleted || above[p] || p == point || layerSkip(p, clean) {
			continue
		}
		leaves = append(leaves, p)
	}
	sort.Strings(leaves) // the same ones asked about each time
	places := map[string]*place{}
	for _, p := range leaves {
		if isDir != nil && isDir(p) {
			continue // a folder emptied again, or made empty
		}
		top := p
		for d := path.Dir(p); d != "/" && d != "."; d = path.Dir(d) {
			if dk, ok := kind[d]; !ok || dk != docker.ChangeAdded {
				break
			}
			top = d
		}
		key := top
		if top == p {
			key = path.Dir(p) // the image's folder it is in
		}
		pl := places[key]
		if pl == nil {
			pl = &place{}
			places[key] = pl
		}
		pl.n++
		pl.made = pl.made || top != p
		pl.leaf = p
	}
	var r LayerReport
	keys := make([]string, 0, len(places))
	for k, pl := range places {
		keys = append(keys, k)
		r.Files += pl.n
	}
	// The biggest first, then by path.
	sort.Slice(keys, func(i, j int) bool {
		if a, b := places[keys[i]].n, places[keys[j]].n; a != b {
			return a > b
		}
		return keys[i] < keys[j]
	})
	for i, k := range keys {
		if i == layerShown {
			r.More = len(keys) - layerShown
			break
		}
		pl := places[k]
		if pl.n == 1 && !pl.made {
			r.Paths = append(r.Paths, pl.leaf)
		} else {
			r.Paths = append(r.Paths, fmt.Sprintf("%s — %d dosya", k, pl.n))
		}
	}
	return r
}

func layerSkip(p string, trees []string) bool {
	under := func(roots []string) bool {
		for _, k := range roots {
			if k = strings.TrimSuffix(k, "/"); k != "" && (p == k || strings.HasPrefix(p, k+"/")) {
				return true
			}
		}
		return false
	}
	if under(layerIgnored) || under(trees) {
		return true
	}
	for _, d := range layerIgnoredNames {
		if strings.Contains(p+"/", d) {
			return true
		}
	}
	return false
}

// LayerDocker reads a container's own layer; *docker.Client is one.
type LayerDocker interface {
	Changes(ctx context.Context, name string) ([]docker.Change, error)
	Inspect(ctx context.Context, name string) (docker.Inspect, error)
	PathStat(ctx context.Context, name, p string) (os.FileMode, error)
}

// LayerOf checks what a site's container holds in its own layer, what its mounts hold
// and keep aside. The mounts are the container's own, not the site record's: a record
// written ahead of its container (an update cut short) hides nothing. No container:
// nothing to lose.
func LayerOf(ctx context.Context, d LayerDocker, spec sites.Spec, keep ...string) LayerReport {
	name := spec.Container()
	changes, err := d.Changes(ctx, name)
	if docker.IsNotFound(err) {
		return LayerReport{}
	}
	if err != nil {
		return LayerReport{Err: err.Error()}
	}
	trees := append([]string(nil), keep...)
	if in, err := d.Inspect(ctx, name); err == nil {
		for _, m := range in.Mounts {
			trees = append(trees, m.Destination)
		}
	}
	asked := 0
	isDir := func(p string) bool {
		if asked == layerStats {
			return false
		}
		asked++
		m, err := d.PathStat(ctx, name, p)
		return err == nil && m.IsDir()
	}
	return layerFiles(changes, isDir, spec.DataMount, trees...)
}

// Layer is LayerOf for a site of these operations.
func (o *SiteOps) Layer(ctx context.Context, spec sites.Spec, keep ...string) LayerReport {
	return LayerOf(ctx, o.Docker, spec, keep...)
}

// layerError stops an operation inside its job: the layer holds files the check before
// it did not see (written meanwhile, a pull can take minutes) and nobody agreed to lose.
func layerError(r LayerReport) error {
	return fmt.Errorf("container yeniden oluşturulmadı: %s — yeniden onaylamak için işlemi baştan başlatın", r.Summary())
}

// startAsIs starts a container whose layer holds files as it is: compose would make it
// anew when its file changed (a newer FootOps writes them differently), and nobody is
// asked here — a restore, an unpark from Telegram. What changed waits for a deploy,
// which asks. A container off its site's network (a move cut short) is put back on it.
// false: nothing at stake (or no container), compose may start it.
func (o *SiteOps) startAsIs(ctx context.Context, spec sites.Spec, progress Progress) (bool, error) {
	if !o.Layer(ctx, spec).Risky() {
		return false, nil
	}
	if o.offNetwork(ctx, spec) {
		if err := o.Docker.Connect(ctx, spec.Network(), spec.Container()); err != nil {
			return true, err
		}
	}
	if err := o.Docker.Start(ctx, spec.Container()); err != nil {
		if docker.IsNotFound(err) {
			return false, nil
		}
		return true, err
	}
	if progress != nil {
		msg := "ℹ Container'ın içinde kalıcı klasör dışında dosyalar var: yeniden oluşturulmadan başlatıldı"
		if o.composeStale(spec) {
			msg += "; yeni ayarları bir sonraki dağıtım (sorarak) uygular"
		}
		progress(msg)
	}
	return true, nil
}

// composeStale reports whether the site's compose file is not what FootOps writes now.
func (o *SiteOps) composeStale(s sites.Spec) bool {
	env, err := o.Store.Env(s)
	if err != nil {
		return false
	}
	have, err := os.ReadFile(o.path(filepath.Join(s.Dir(), sites.ComposeFile)))
	return err == nil && string(have) != sites.Compose(s, env)
}

// fillDataDir copies what the container holds at mount into dir, which a persistent
// folder there would hide. A new dir is filled beside and renamed, so it is there whole
// or not at all; one already there (left unmounted) takes the container's files over its
// own. No such container or path: nothing to copy. It returns the files and bytes dir
// then holds.
func (o *SiteOps) fillDataDir(ctx context.Context, from sites.Spec, mount, dir string) (int, int64, error) {
	dest := dir
	_, err := os.Stat(dir)
	fresh := errors.Is(err, fs.ErrNotExist)
	if fresh {
		dest = dir + ".copy"
		_ = os.RemoveAll(dest)
		if err := os.MkdirAll(dest, 0o750); err != nil {
			return 0, 0, err
		}
		defer os.RemoveAll(dest) // renamed by then, unless nothing came
	}
	src := from.Container() + ":" + strings.TrimSuffix(path.Clean(mount), "/") + "/."
	if _, err := o.Run.Run(ctx, "docker", "cp", src, dest); err != nil {
		if s := strings.ToLower(err.Error()); strings.Contains(s, "could not find the file") || strings.Contains(s, "no such container") {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("container'daki %s kopyalanamadı: %w", mount, err)
	}
	var n int
	var size int64
	_ = filepath.WalkDir(dest, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n, size = n+1, size+fi.Size()
			}
		}
		return nil
	})
	if fresh && n > 0 {
		return n, size, os.Rename(dest, dir)
	}
	return n, size, nil
}

// roomFor checks the disk holding dir has room for what the site's container wrote to its
// layer (an upper bound of what a fill copies).
func (o *SiteOps) roomFor(ctx context.Context, spec sites.Spec, dir string) error {
	in, err := o.Docker.InspectSize(ctx, spec.Container())
	if err != nil {
		if docker.IsNotFound(err) {
			return nil
		}
		return err
	}
	if in.SizeRw == 0 {
		return nil
	}
	if err := staging.CheckFree(dir, in.SizeRw); err != nil {
		return fmt.Errorf("container'ın dosyaları (%s) için diskte yer yok: %w", humanBytes(in.SizeRw), err)
	}
	return nil
}

// copyBack puts what a failed update took into the persistent folder back into the
// container it came from, made anew without the folder; ownership as it is in the folder.
func (o *SiteOps) copyBack(ctx context.Context, spec sites.Spec, mount, dir string) error {
	_, err := o.Run.Run(ctx, "docker", "cp", "--archive", dir+"/.", spec.Container()+":"+strings.TrimSuffix(path.Clean(mount), "/"))
	return err
}

// chownTree gives what is under dir the owner dir has.
func chownTree(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, int(st.Uid), int(st.Gid))
	})
}

// UpdateLayer is what an update would lose: nothing when the container stays, else what
// its layer holds outside its mounts and outside a first persistent folder, which the
// update fills from the container.
func (o *SiteOps) UpdateLayer(ctx context.Context, name string, u SiteUpdate) (LayerReport, error) {
	p, err := o.planUpdate(name, u)
	if err != nil {
		return LayerReport{}, err
	}
	if !o.recreates(ctx, p) {
		return LayerReport{}, nil
	}
	return o.Layer(ctx, p.cur, p.fills()...), nil
}

// refuseLayer answers 409 with the report when making the container anew would lose
// something: the request comes again with force once someone has seen it.
func refuseLayer(w http.ResponseWriter, r LayerReport) bool {
	if !r.Risky() {
		return false
	}
	writeJSON(w, http.StatusConflict, errorResponse{Error: "container yeniden oluşturulunca kaybolacaklar var: " + r.Summary(), Layer: &r})
	return true
}

// forcedNote is what the audit log adds when someone agreed to lose the layer's files.
func forcedNote(force bool) string {
	if !force {
		return ""
	}
	return " (container'ın içindeki dosyaların kaybı onaylandı)"
}

// siteLayer answers GET /v1/sites/{name}/layer.
func (s *Server) siteLayer(w http.ResponseWriter, r *http.Request) {
	spec, err := s.Sites.Store.Get(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Sites.Layer(r.Context(), spec))
}
