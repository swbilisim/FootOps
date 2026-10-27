package remote

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/run"
)

// Scan is a look into an app folder on the source (F4 step 1, read-only).
type Scan struct {
	Taken time.Time `json:"taken"`
	Dir   string    `json:"dir"` // as resolved on the source
	Err   string    `json:"error,omitempty"`
	Total Counts    `json:"total"`
	Subs  []SubDir  `json:"subs"`
	Files []string  `json:"files"` // package.json, Dockerfile, … present
	// From package.json: name, main, engines.node, dependency names.
	Package *Package `json:"package,omitempty"`
	// Env lists the variable names (never values) of each .env file.
	Env []EnvFile `json:"env"`
	// Secrets are places in the code that look like a secret: file and line only.
	Secrets []Finding `json:"secrets"`
	// DBRefs are database names in mongodb:// addresses in the code (names only).
	DBRefs []Ref `json:"db_refs"`
	// URLs are addresses with an IP or localhost built into the code (they break when
	// the site moves).
	URLs     []Ref `json:"urls"`
	Complete bool  `json:"complete"`
}

type SubDir struct {
	Name string `json:"name"`
	Counts
}

type Package struct {
	Name string   `json:"name,omitempty"`
	Main string   `json:"main,omitempty"`
	Node string   `json:"node,omitempty"`
	Deps []string `json:"deps,omitempty"`
}

// Known reports the dependencies that tell what kind of app this is.
func (p Package) Known() []string {
	var out []string
	for _, d := range p.Deps {
		switch d {
		case "next", "express", "react", "vue", "nuxt", "mongoose", "mongodb", "multer", "apollo-server", "apollo-server-express",
			"graphql", "nodemailer", "sharp", "socket.io", "koa", "fastify", "@nestjs/core", "pm2":
			out = append(out, d)
		}
	}
	return out
}

type EnvFile struct {
	File string   `json:"file"`
	Keys []string `json:"keys"`
}

type Finding struct {
	Kind  string `json:"kind"`  // mongo-uri, password, api-key, private-key, smtp
	Where string `json:"where"` // file:line
}

type Ref struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

var (
	reEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	reDBName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,63}$`)
	reURL    = regexp.MustCompile(`^https?://(([0-9]{1,3}\.){3}[0-9]{1,3}|localhost)(:[0-9]{1,5})?$`)
	reDep    = regexp.MustCompile(`^[@A-Za-z0-9._/-]{1,100}$`)
)

// ParseScan reads the scan script's output.
func ParseScan(out string, taken time.Time) Scan {
	s := Scan{Taken: taken.UTC(), Subs: []SubDir{}, Files: []string{}, Env: []EnvFile{}, Secrets: []Finding{}, DBRefs: []Ref{}, URLs: []Ref{}}
	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		tag, rest, _ := strings.Cut(line, "\t")
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
		pkg := func() *Package {
			if s.Package == nil {
				s.Package = &Package{}
			}
			return s.Package
		}
		switch tag {
		case "err":
			s.Err = get(0)
		case "dir":
			s.Dir = get(0)
		case "total":
			s.Total, _ = parseCounts(f)
		case "sub":
			if c, ok := parseCounts(f[1:]); ok && len(s.Subs) < 60 {
				s.Subs = append(s.Subs, SubDir{Name: get(0), Counts: c})
			}
		case "file":
			if len(s.Files) < 30 {
				s.Files = append(s.Files, get(0))
			}
		case "pkgname":
			pkg().Name = get(0)
		case "pkgmain":
			pkg().Main = get(0)
		case "pkgnode":
			pkg().Node = get(0)
		case "dep":
			if p := pkg(); len(p.Deps) < 300 && reDep.MatchString(get(0)) {
				p.Deps = append(p.Deps, get(0))
			}
		case "env":
			e := EnvFile{File: get(0), Keys: []string{}}
			for _, k := range strings.Fields(get(1)) {
				if reEnvKey.MatchString(k) && len(e.Keys) < 100 {
					e.Keys = append(e.Keys, k)
				}
			}
			if len(s.Env) < 10 {
				s.Env = append(s.Env, e)
			}
		case "secret":
			if len(s.Secrets) < 200 {
				s.Secrets = append(s.Secrets, Finding{Kind: get(0), Where: get(1)})
			}
		case "dbref":
			if reDBName.MatchString(get(0)) && len(s.DBRefs) < 20 {
				s.DBRefs = append(s.DBRefs, Ref{Value: get(0), Count: atoi(get(1))})
			}
		case "url":
			if reURL.MatchString(get(0)) && len(s.URLs) < 30 {
				s.URLs = append(s.URLs, Ref{Value: get(0), Count: atoi(get(1))})
			}
		case "end":
			s.Complete = true
		}
	}
	return s
}

// ValidDir accepts an absolute folder on the source; it is quoted on the way there, but
// "/" and the like are refused (a whole system is never copied).
func ValidDir(d string) error {
	switch {
	case !strings.HasPrefix(d, "/"):
		return errors.New("klasör / ile başlayan tam yol olmalı (ör. /home/site/www/public/uploads)")
	case len(d) > 1024 || strings.ContainsAny(d, "\x00\n\r\t"):
		return errors.New("klasör yolu geçersiz")
	}
	switch path.Clean(d) {
	case "/", "/bin", "/boot", "/dev", "/etc", "/lib", "/lib64", "/proc", "/root", "/run", "/sbin", "/sys", "/usr", "/var", "/home", "/srv", "/opt", "/tmp":
		return fmt.Errorf("%s bir sistem klasörü; uygulamanın kendi klasörünü yaz", path.Clean(d))
	}
	return nil
}

// ScanDir runs the scan script on a folder of the source.
func (s *Store) ScanDir(ctx context.Context, src Source, dir string) (Scan, error) {
	if err := ValidDir(dir); err != nil {
		return Scan{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, err := s.Output(ctx, src, 1<<20, ScanScript, dir)
	if err != nil {
		return Scan{}, err
	}
	sc := ParseScan(out, s.clock())
	if sc.Err != "" {
		return sc, errors.New(sc.Err)
	}
	if !sc.Complete {
		return sc, errors.New("inceleme betiği sonuna kadar çalışmadı (çıktı yarım)")
	}
	return sc, nil
}

// Count counts a folder of the source the way the copy takes it.
func (s *Store) Count(ctx context.Context, src Source, dir string) (Counts, error) {
	if err := ValidDir(dir); err != nil {
		return Counts{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := s.Output(ctx, src, 4096, CountScript, dir)
	if ExitCode(err) == 3 {
		return Counts{}, fmt.Errorf("kaynakta %s klasörü yok ya da girilemiyor", dir)
	}
	if err != nil {
		return Counts{}, err
	}
	_, line, ok := strings.Cut(out, "FOOTOPS_COUNT ")
	if !ok {
		return Counts{}, errors.New("kaynak klasörü sayamadı")
	}
	c, ok := parseCounts(strings.Fields(firstLine(line)))
	if !ok {
		return Counts{}, errors.New("kaynağın sayımı anlaşılamadı")
	}
	switch c.Err {
	case 0:
		return c, nil
	case 124:
		return c, fmt.Errorf("kaynakta %s 2 dakikada sayılamadı (çok büyük ya da disk çok yavaş)", dir)
	}
	return c, fmt.Errorf("kaynakta %s eksiksiz okunamadı (find çıkış kodu %d): bazı klasörlere girilemiyor olabilir", dir, c.Err)
}

// ValidDB accepts a database name on the source (it goes into a command line).
func ValidDB(name string) bool {
	return reDBName.MatchString(name) && name != "admin" && name != "config" && name != "local"
}

// ValidContainer accepts a Docker container name.
var reContainer = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ValidContainer(name string) bool { return reContainer.MatchString(name) }

// DumpCmd streams the source's database as `mongodump --archive --gzip`: on the host
// (container "") or in a container. The caller sets Stdout and Stderr.
func (s *Store) DumpCmd(src Source, job, container, db string) (run.Cmd, error) {
	if !ValidDB(db) {
		return run.Cmd{}, fmt.Errorf("veritabanı adı %q kullanılamaz", db)
	}
	via, c := "host", "-"
	if container != "" {
		if !ValidContainer(container) {
			return run.Cmd{}, fmt.Errorf("container adı %q geçersiz", container)
		}
		via, c = "docker", container
	}
	return s.StreamCmd(src, job, DumpScript, via, c, db), nil
}

// TarCmd streams a folder of the source as a tar of its files and folders. The caller
// sets Stdout and Stderr.
func (s *Store) TarCmd(src Source, job, dir string) (run.Cmd, error) {
	if err := ValidDir(dir); err != nil {
		return run.Cmd{}, err
	}
	return s.StreamCmd(src, job, TarScript, dir), nil
}

// Exit codes of DumpScript and TarScript besides the tools' own.
const (
	ExitNoDir   = 3  // the folder is missing or cannot be entered
	ExitBadArgs = 64 // the script was called wrongly
	ExitNotGNU  = 66 // find or tar is not GNU's
	ExitNoMongo = 69 // no mongodump on the source
)

// tarLive are tar's complaints a site that still runs explains: a file changed, grew,
// shrank or vanished while it was read.
var tarLive = []string{"file changed as we read it", "File removed before we read it", "File shrank by",
	"Cannot stat: No such file or directory"}

// StreamLog keeps what a dump or tar on the source said on stderr: the last lines for
// messages, find's first complaint, and tar's complaints sorted into those a live site
// explains and the others. Lines are clipped; memory stays small whatever is sent.
type StreamLog struct {
	lines    []string
	Find     string // the first "find:" line
	TarLive  int    // tar complaints a live site explains
	TarOther string // the first other "tar:" line
}

func (s *StreamLog) Add(line string) {
	line = clip(line, 300)
	switch {
	case strings.HasPrefix(line, "find:") && s.Find == "":
		s.Find = line
	case strings.HasPrefix(line, "tar:"):
		live := false
		for _, p := range tarLive {
			live = live || strings.Contains(line, p)
		}
		if live {
			s.TarLive++
		} else if s.TarOther == "" {
			s.TarOther = line
		}
	}
	s.lines = append(s.lines, line)
	if len(s.lines) > 8 {
		s.lines = s.lines[1:]
	}
}

func (s *StreamLog) String() string { return strings.Join(s.lines, "\n") }

// Last is the last line, usually the one that says what went wrong.
func (s *StreamLog) Last() string {
	for i := len(s.lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(s.lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// LiveOnly reports whether tar complained only of things a live site explains.
func (s *StreamLog) LiveOnly() bool { return s.TarLive > 0 && s.TarOther == "" && s.Find == "" }

// StreamError explains how a dump (tar false) or a tar on the source ended. With live
// (the site still runs there), tar ending 1 or 2 only because files changed or vanished
// while being read is accepted; any other failure is an error, and so is a find that
// could not read a folder even when tar ended well.
func StreamError(err error, log *StreamLog, tar, live bool) error {
	if tar && log.Find != "" {
		return fmt.Errorf("kaynakta bazı klasörler okunamadı: %s", log.Find)
	}
	if err == nil {
		return nil
	}
	switch code := ExitCode(err); {
	case code == ExitSSH:
		return Explain(err, log.String())
	case code == ExitNoDir:
		return errors.New("kaynakta klasör yok ya da girilemiyor")
	case code == ExitNotGNU:
		return errors.New("kaynakta GNU find/tar yok (taşıma için gerekli)")
	case code == ExitNoMongo:
		return errors.New("kaynakta mongodump yok (veritabanı container'daysa keşifte container'ı seç)")
	case tar && (code == 1 || code == 2) && log.LiveOnly():
		if live {
			return nil
		}
		return errors.New("kaynakta bazı dosyalar okunurken değişti; site orada hâlâ yazıyor olabilir")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Explain(err, "")
	}
	if msg := log.Last(); msg != "" && !strings.Contains(err.Error(), msg) {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}
