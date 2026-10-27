package remote

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// WebServer is one nginx server block: which names it answers and where it sends them.
type WebServer struct {
	File      string     `json:"file,omitempty"`
	Names     []string   `json:"names"`
	Listen    []string   `json:"listen,omitempty"`
	Root      string     `json:"root,omitempty"`
	Locations []Location `json:"locations,omitempty"`
}

// Location is a location block's target.
type Location struct {
	Path  string `json:"path"`
	Proxy string `json:"proxy,omitempty"`
	Root  string `json:"root,omitempty"`
	Alias string `json:"alias,omitempty"`
}

// Limits of what is read from `nginx -T` (it is capped at 1 MB on the source as well).
const (
	maxDirectives = 50_000
	maxDepth      = 16
	maxServers    = 300
	maxLocations  = 60
	maxArgLen     = 300
)

type ngxTok struct {
	s    string
	kind byte // 'w' word, ';', '{', '}'
	file string
}

// ngxTokens splits nginx configuration text into words and punctuation. `nginx -T`
// marks each file with "# configuration file <path>:". A # starts a comment only at
// the start of a word.
func ngxTokens(text string) []ngxTok {
	var out []ngxTok
	file := ""
	for line := range strings.Lines(text) {
		line = strings.TrimSuffix(line, "\n")
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "# configuration file ") && strings.HasSuffix(t, ":") {
			file = clip(strings.TrimSuffix(strings.TrimPrefix(t, "# configuration file "), ":"), maxArgLen)
			continue
		}
		for i := 0; i < len(line); {
			c := line[i]
			switch {
			case c == ' ' || c == '\t' || c == '\r':
				i++
			case c == '#':
				i = len(line)
			case c == ';' || c == '{' || c == '}':
				out = append(out, ngxTok{kind: c, file: file})
				i++
			case c == '"' || c == '\'':
				var b strings.Builder
				j := i + 1
				for ; j < len(line) && line[j] != c; j++ {
					if line[j] == '\\' && j+1 < len(line) {
						j++
					}
					b.WriteByte(line[j])
				}
				out = append(out, ngxTok{s: b.String(), kind: 'w', file: file})
				i = j + 1
			default:
				j := i
				for j < len(line) && !strings.ContainsRune(" \t\r;{}", rune(line[j])) {
					j++
				}
				out = append(out, ngxTok{s: line[i:j], kind: 'w', file: file})
				i = j
			}
			if len(out) > 4*maxDirectives {
				return out
			}
		}
	}
	return out
}

type ngxDirective struct {
	name  string
	args  []string
	block []ngxDirective
	isBlk bool
	file  string
}

type ngxParser struct {
	toks []ngxTok
	i    int
	n    int // directives so far
}

// block reads directives until a closing brace (or the end).
func (p *ngxParser) block(depth int) []ngxDirective {
	var out []ngxDirective
	for p.i < len(p.toks) && p.n < maxDirectives {
		t := p.toks[p.i]
		switch t.kind {
		case '}':
			p.i++
			return out
		case ';', '{':
			p.i++ // stray punctuation
			if t.kind == '{' && depth < maxDepth {
				p.block(depth + 1)
			}
			continue
		}
		d := ngxDirective{name: t.s, file: t.file}
		p.i++
		for p.i < len(p.toks) && p.toks[p.i].kind == 'w' {
			if len(d.args) < 16 {
				d.args = append(d.args, clip(p.toks[p.i].s, maxArgLen))
			}
			p.i++
		}
		p.n++
		if p.i < len(p.toks) {
			switch p.toks[p.i].kind {
			case ';':
				p.i++
			case '{':
				p.i++
				d.isBlk = true
				if depth < maxDepth {
					d.block = p.block(depth + 1)
				} else {
					p.skip()
				}
			}
		}
		out = append(out, d)
	}
	return out
}

// skip passes over a block nested too deep.
func (p *ngxParser) skip() {
	for depth := 1; p.i < len(p.toks) && depth > 0; p.i++ {
		switch p.toks[p.i].kind {
		case '{':
			depth++
		case '}':
			depth--
		}
	}
}

// ParseNginx reads the output of `nginx -T`: the virtual servers and the upstreams
// they may proxy to.
func ParseNginx(text string) ([]WebServer, map[string][]string) {
	p := &ngxParser{toks: ngxTokens(text)}
	var top []ngxDirective
	// A stray closing brace ends block(0) early; the files after it still count.
	for p.i < len(p.toks) && p.n < maxDirectives {
		top = append(top, p.block(0)...)
	}
	var servers []WebServer
	upstreams := map[string][]string{}
	var walk func(ds []ngxDirective)
	walk = func(ds []ngxDirective) {
		for _, d := range ds {
			switch {
			case d.name == "stream" || d.name == "mail" || d.name == "events":
				continue
			case d.name == "upstream" && d.isBlk && len(d.args) > 0:
				if len(upstreams) < 200 {
					var addrs []string
					for _, s := range d.block {
						if s.name == "server" && !s.isBlk && len(s.args) > 0 && len(addrs) < 20 {
							addrs = append(addrs, s.args[0])
						}
					}
					upstreams[d.args[0]] = addrs
				}
			case d.name == "server" && d.isBlk:
				if len(servers) < maxServers {
					servers = append(servers, serverOf(d))
				}
			case d.isBlk:
				walk(d.block)
			}
		}
	}
	walk(top)
	return servers, upstreams
}

func serverOf(d ngxDirective) WebServer {
	ws := WebServer{File: d.file, Names: []string{}}
	for _, s := range d.block {
		switch s.name {
		case "server_name":
			for _, n := range s.args {
				if len(ws.Names) < 30 {
					ws.Names = append(ws.Names, strings.ToLower(n))
				}
			}
		case "listen":
			if len(s.args) > 0 && len(ws.Listen) < 10 {
				ws.Listen = append(ws.Listen, strings.Join(s.args, " "))
			}
		case "root":
			if len(s.args) > 0 {
				ws.Root = s.args[0]
			}
		case "location":
			ws.Locations = locations(ws.Locations, s)
		}
	}
	return ws
}

// locations adds a location block and the ones nested in it.
func locations(out []Location, d ngxDirective) []Location {
	if !d.isBlk || len(out) >= maxLocations {
		return out
	}
	l := Location{Path: strings.Join(d.args, " ")}
	var nested []ngxDirective
	for _, s := range d.block {
		switch {
		case s.name == "proxy_pass" && len(s.args) > 0:
			l.Proxy = hideUserinfo(s.args[0])
		case s.name == "root" && len(s.args) > 0:
			l.Root = s.args[0]
		case s.name == "alias" && len(s.args) > 0:
			l.Alias = s.args[0]
		case s.name == "location":
			nested = append(nested, s)
		}
	}
	out = append(out, l)
	for _, n := range nested {
		out = locations(out, n)
	}
	return out
}

// hideUserinfo removes a user and password, and the query (it may carry a token),
// from a proxy address.
func hideUserinfo(s string) string {
	s, _, _ = strings.Cut(s, "?")
	scheme, rest, ok := strings.Cut(s, "://")
	if !ok {
		return s
	}
	host, path, _ := strings.Cut(rest, "/")
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		host = "***@" + host[i+1:]
	}
	if path != "" || strings.HasSuffix(rest, "/") {
		return scheme + "://" + host + "/" + path
	}
	return scheme + "://" + host
}

// ProxyPorts are the local ports a proxy target reaches: its own port, or those of the
// upstream it names. Only loopback and wildcard addresses count (an app on this host).
func ProxyPorts(target string, upstreams map[string][]string) []int {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return nil
	}
	var addrs []string
	if ups, ok := upstreams[u.Hostname()]; ok {
		addrs = ups
	} else {
		addrs = []string{u.Host}
	}
	var out []int
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			host, port = a, "80"
		}
		if !localHost(host) {
			continue
		}
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n < 65536 {
			out = append(out, n)
		}
	}
	return out
}

func localHost(h string) bool {
	switch strings.Trim(h, "[]") {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0", "::", "*", "":
		return true
	}
	return false
}

// Matches reports whether a server block answers for domain (exact names, a leading
// wildcard, or a ".example.com" name).
func (w WebServer) Matches(domain string) bool {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	for _, n := range w.Names {
		n = strings.TrimSuffix(n, ".")
		switch {
		case n == domain:
			return true
		case strings.HasPrefix(n, "*.") && strings.HasSuffix(domain, n[1:]):
			return true
		case strings.HasPrefix(n, ".") && (domain == n[1:] || strings.HasSuffix(domain, n)):
			return true
		}
	}
	return false
}
