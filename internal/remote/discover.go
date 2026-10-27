package remote

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Discovery is what the source told about itself (read-only, F4 step 1).
type Discovery struct {
	Taken      time.Time           `json:"taken"`
	System     System              `json:"system"`
	Tools      []Tool              `json:"tools"`
	Mongo      []MongoSource       `json:"mongo"`
	Web        []WebServer         `json:"web"`
	Upstreams  map[string][]string `json:"upstreams,omitempty"`
	Procs      []Proc              `json:"procs"`
	Listen     []Listen            `json:"listen"`
	Containers []Container         `json:"containers"`
	Apps       []AppDir            `json:"apps"`
	Candidates []Candidate         `json:"candidates"`
	// Complete is false when the script did not run to its end (output cut short).
	Complete bool `json:"complete"`
}

type System struct {
	OS       string `json:"os"`
	Kernel   string `json:"kernel"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname"`
	User     string `json:"user"`
	CPUs     int    `json:"cpus"`
	MemTotal int64  `json:"mem_total"` // bytes
	MemAvail int64  `json:"mem_avail"`
	Disks    []Disk `json:"disks"`
}

type Disk struct {
	Mount string `json:"mount"`
	Size  int64  `json:"size"` // bytes
	Used  int64  `json:"used"`
	Avail int64  `json:"avail"`
}

type Tool struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

// MongoSource is a MongoDB the source runs: on the host or in a container.
type MongoSource struct {
	Via       string    `json:"via"` // "host" or "docker"
	Container string    `json:"container,omitempty"`
	Shell     string    `json:"shell"`
	Version   string    `json:"version,omitempty"`
	Err       string    `json:"error,omitempty"`
	DBs       []MongoDB `json:"dbs"`
}

type MongoDB struct {
	Name        string `json:"name"`
	Collections int64  `json:"collections"`
	Objects     int64  `json:"objects"`
	DataSize    int64  `json:"data_size"`
	DiskSize    int64  `json:"disk_size"`
}

// Proc is a running node process. Only a few environment names are read on the source
// (PM2's name and id, PORT, NODE_ENV); no other environment value leaves it.
type Proc struct {
	PID     int    `json:"pid"`
	PPID    int    `json:"ppid"`
	User    string `json:"user"`
	RSS     int64  `json:"rss"` // bytes
	Exe     string `json:"exe"`
	Cwd     string `json:"cwd"`
	Cmd     string `json:"cmd"`
	PM2Name string `json:"pm2_name,omitempty"`
	PM2ID   string `json:"pm2_id,omitempty"`
	PM2Home string `json:"pm2_home,omitempty"`
	Port    string `json:"port,omitempty"`
	NodeEnv string `json:"node_env,omitempty"`
	// Daemon is PM2's own process ("PM2 vX: God Daemon (home)").
	Daemon bool  `json:"daemon,omitempty"`
	Ports  []int `json:"ports,omitempty"` // listening, from ss
}

type Listen struct {
	Addr    string `json:"addr"`
	Port    int    `json:"port"`
	Process string `json:"process,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

type Container struct {
	Name   string  `json:"name"`
	Image  string  `json:"image"`
	Status string  `json:"status"`
	Ports  string  `json:"ports,omitempty"`
	Mounts []Mount `json:"mounts,omitempty"`
}

type Mount struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
}

// AppDir is the folder a node process runs in.
type AppDir struct {
	Dir string `json:"dir"`
	KB  int64  `json:"kb"`
}

// Candidate is a folder that may hold an app's uploads.
type Candidate struct {
	Dir string `json:"dir"`
	Counts
}

// Counts are a folder counted like the copy takes it.
type Counts struct {
	Files  int64 `json:"files"`
	Bytes  int64 `json:"bytes"`
	Dirs   int64 `json:"dirs"`
	Hidden int64 `json:"hidden"` // not copied
	Links  int64 `json:"links"`  // not copied
	Other  int64 `json:"other"`  // sockets, devices…: not copied
	// Err is find's exit status: not 0 when a folder could not be read or the count ran
	// out of time, so the numbers are short.
	Err int64 `json:"err,omitempty"`
}

// Skipped is what the copy leaves out.
func (c Counts) Skipped() int64 { return c.Hidden + c.Links + c.Other }

func parseCounts(f []string) (Counts, bool) {
	if len(f) < 7 {
		return Counts{}, false
	}
	var n [7]int64
	for i := range n {
		v, err := strconv.ParseInt(strings.TrimSpace(f[i]), 10, 64)
		if err != nil || v < 0 {
			return Counts{}, false
		}
		n[i] = v
	}
	return Counts{Files: n[0], Bytes: n[1], Dirs: n[2], Hidden: n[3], Links: n[4], Other: n[5], Err: n[6]}, true
}

// Limits on what is kept from the output.
const (
	maxProcs      = 300
	maxContainers = 150
	maxMongoDBs   = 300
	maxListen     = 300
	maxCands      = 200
	maxField      = 500
)

// cleanField keeps a field printable and short (the source cleaned it, but is not
// trusted to).
func cleanField(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	for len(s) > maxField {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return max(n, 0)
}

var (
	rePM2Daemon = regexp.MustCompile(`^PM2 v[0-9.]+: God Daemon \((.+)\)`)
	// users:(("node",pid=1234,fd=20)) — older ss prints users:(("node",1234,20)).
	reSSUser = regexp.MustCompile(`\("([^"]*)",(?:pid=)?([0-9]+),`)
)

// ParseDiscovery reads the discovery script's output.
func ParseDiscovery(out string, taken time.Time) Discovery {
	d := Discovery{Taken: taken.UTC(), Tools: []Tool{}, Mongo: []MongoSource{}, Procs: []Proc{}, Listen: []Listen{},
		Containers: []Container{}, Apps: []AppDir{}, Candidates: []Candidate{}}
	var nginx strings.Builder
	mounts := map[string][]Mount{}
	var cur *MongoSource
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		tag, rest, _ := strings.Cut(line, "\t")
		if tag == "ngx" {
			if nginx.Len() < 1<<20 {
				nginx.WriteString(rest)
				nginx.WriteByte('\n')
			}
			continue
		}
		f := strings.Split(rest, "\t")
		for i := range f {
			f[i] = cleanField(f[i])
		}
		get := func(i int) string {
			if i < len(f) {
				return f[i]
			}
			return ""
		}
		switch tag {
		case "os":
			d.System.OS, d.System.Kernel, d.System.Arch, d.System.Hostname, d.System.User = get(0), get(1), get(2), get(3), get(4)
		case "cpu":
			d.System.CPUs = int(atoi(get(0)))
		case "mem":
			d.System.MemTotal, d.System.MemAvail = atoi(get(0))<<10, atoi(get(1))<<10
		case "disk":
			if len(d.System.Disks) < 10 {
				d.System.Disks = append(d.System.Disks, Disk{Mount: get(0), Size: atoi(get(1)) << 10, Used: atoi(get(2)) << 10, Avail: atoi(get(3)) << 10})
			}
		case "tool":
			if len(d.Tools) < 30 {
				d.Tools = append(d.Tools, Tool{Name: get(0), Path: get(1), Version: get(2)})
			}
		case "msrc":
			if len(d.Mongo) < 20 {
				d.Mongo = append(d.Mongo, MongoSource{Via: get(0), Container: strings.TrimPrefix(get(1), "-"), Shell: get(2), DBs: []MongoDB{}})
				cur = &d.Mongo[len(d.Mongo)-1]
			} else {
				cur = nil
			}
		case "FOOTOPS_MVER":
			if cur != nil {
				cur.Version = get(0)
			}
		case "FOOTOPS_MERR", "merr":
			if cur != nil && cur.Err == "" {
				cur.Err = get(0)
			}
		case "FOOTOPS_MDB":
			if cur != nil && len(cur.DBs) < maxMongoDBs {
				cur.DBs = append(cur.DBs, MongoDB{Name: get(0), Collections: atoi(get(1)), Objects: atoi(get(2)),
					DataSize: atoi(get(3)), DiskSize: atoi(get(4))})
			}
		case "proc":
			if len(d.Procs) < maxProcs {
				d.Procs = append(d.Procs, parseProc(f))
			}
		case "listen":
			if l, ok := parseListen(rest); ok && len(d.Listen) < maxListen {
				d.Listen = append(d.Listen, l)
			}
		case "ctr":
			if len(d.Containers) < maxContainers {
				d.Containers = append(d.Containers, Container{Name: get(0), Image: get(1), Status: get(2), Ports: get(3)})
			}
		case "mnt":
			name := get(0)
			for _, m := range f[1:] {
				src, dst, ok := strings.Cut(m, ":")
				if ok && len(mounts[name]) < 20 {
					mounts[name] = append(mounts[name], Mount{Source: src, Dest: dst})
				}
			}
		case "app":
			if len(d.Apps) < 50 {
				d.Apps = append(d.Apps, AppDir{Dir: get(0), KB: atoi(get(1))})
			}
		case "cand":
			if c, ok := parseCounts(f[1:]); ok && len(d.Candidates) < maxCands && strings.HasPrefix(get(0), "/") {
				d.Candidates = append(d.Candidates, Candidate{Dir: get(0), Counts: c})
			}
		case "end":
			d.Complete = true
		}
	}
	for i := range d.Containers {
		d.Containers[i].Mounts = mounts[d.Containers[i].Name]
	}
	ports := map[int][]int{} // pid -> listening ports
	for _, l := range d.Listen {
		if l.PID > 0 {
			ports[l.PID] = append(ports[l.PID], l.Port)
		}
	}
	for i := range d.Procs {
		d.Procs[i].Ports = uniqInts(ports[d.Procs[i].PID])
	}
	d.Web, d.Upstreams = ParseNginx(nginx.String())
	return d
}

func parseProc(f []string) Proc {
	get := func(i int) string {
		if i < len(f) {
			return f[i]
		}
		return ""
	}
	p := Proc{PID: int(atoi(get(0))), PPID: int(atoi(get(1))), User: get(2), RSS: atoi(get(3)) << 10, Exe: get(4), Cwd: get(5), Cmd: get(6)}
	if m := rePM2Daemon.FindStringSubmatch(p.Cmd); m != nil {
		p.Daemon, p.PM2Home = true, m[1]
	}
	for _, kv := range strings.Split(get(7), "|") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "name":
			p.PM2Name = v
		case "pm_id":
			p.PM2ID = v
		case "PM2_HOME":
			p.PM2Home = v
		case "PORT":
			p.Port = v
		case "NODE_ENV":
			p.NodeEnv = v
		}
	}
	if p.PM2ID == "" && !p.Daemon {
		p.PM2Name = "" // a "name" variable without PM2's id is something else
	}
	return p
}

// parseListen reads one line of `ss -ltnp`: State Recv-Q Send-Q Local Peer Process.
func parseListen(line string) (Listen, bool) {
	f := strings.Fields(line)
	if len(f) < 5 {
		return Listen{}, false
	}
	local := f[3]
	i := strings.LastIndexByte(local, ':')
	if i < 0 {
		return Listen{}, false
	}
	port, err := strconv.Atoi(local[i+1:])
	if err != nil || port <= 0 || port > 65535 {
		return Listen{}, false
	}
	l := Listen{Addr: cleanField(local[:i]), Port: port}
	if m := reSSUser.FindStringSubmatch(line); m != nil {
		l.Process = cleanField(m[1])
		l.PID = int(atoi(m[2]))
	}
	return l, true
}

func uniqInts(in []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range in {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// Discover runs the discovery script on the source (read-only, about a minute at most).
func (s *Store) Discover(ctx context.Context, src Source) (Discovery, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	out, err := s.Output(ctx, src, 3<<20, DiscoverScript)
	if err != nil {
		return Discovery{}, err
	}
	d := ParseDiscovery(out, s.clock())
	if !d.Complete {
		return d, errors.New("keşif betiği sonuna kadar çalışmadı (çıktı yarım)")
	}
	return d, nil
}
