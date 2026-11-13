package mac

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The servers' backups on the Mac (panel-tasarim §29.5): what the bash helper footops-yedek
// did, in Go (it is gone since, M3). The
// restic repositories are only read — with the Mac's read-only key, every command
// --no-lock, never unlock — so nothing done here can change or remove a backup. The key
// reaches restic in its environment alone and the repository's password through restic's
// password command, from the Keychain: neither sits in a file, an argument or the output.

const backupUsage = `Kullanım: footops-mac yedek <komut> [--json]

  sunucular                    Anahtar Zinciri'ndeki sunucular ve kovaları
  dene <sunucu>                depo bu Mac'teki anahtar ve şifreyle açılıyor mu (yalnız okur)
  liste <sunucu> [site]        siteler ve son yedekleri; site verilirse o sitenin yedekleri
  ac <sunucu> <site> [zaman | yedek] [--hedef KLASÖR] [--db] [--dosyalar] [--ayarlar]
                               [--mongo [--ad VERİTABANI] [--uzerine]]
                               sitenin yedeğini klasöre açar (varsayılan ~/Downloads/<site>-<zaman>):
                               db.archive, files/, env, footops.json; yalnız bazıları: --db / --dosyalar / --ayarlar
                               zaman: "2026-11-07 04:00" — o ana kadarki en yeni yedek (boş: en son);
                               yedek: liste'deki kimlik (ilk 6+ harfi yeter)
                               --mongo: veritabanını Mac'teki MongoDB'ye de yükler (varsayılan ad
                               <site>_yedek_<tarih>_<saat>, başkası --ad ile; o adla veritabanı varsa
                               --uzerine ister; MONGO_URI)
  mongo <sunucu> <site> [zaman | yedek] [--ad VERİTABANI] [--uzerine]
                               yalnız veritabanını Mac'teki MongoDB'ye yükler (klasör bırakmaz)
  acilanlar                    bu Mac'te açılan yedekler: klasörler ve MongoDB'deki *_yedek_* veritabanları
  sil <klasör | veritabanı>    açılan bir yedeği siler: footops-mac'in açtığı klasör ya da *_yedek_* veritabanı
  kurtarma <sunucu>            "yedekten kur" için tek satırlık kurtarma bilgisi (SIR içerir; ekrana yazılmaz):
                               footops-mac yedek kurtarma h1 | pbcopy

Yedeklere yalnız okuyarak erişir (salt okunur anahtar, kilitsiz): hiçbir yedek değişmez ya da silinmez.
`

const (
	// defaultBackupRoot is where the parts sit in a repository (the agent's tool
	// container): <root>/<site>/db.archive, …/files, …/def, <root>/_server.
	defaultBackupRoot = "/footops"
	serverPart        = "_server"
	// The parts, as the agent tags them (part=…).
	partDB     = "db"
	partFiles  = "files"
	partDef    = "def"
	partServer = "server"
	// listTimeout bounds opening a repository and reading its list: restic retries a
	// network that is gone for longer than anyone waits.
	listTimeout = 3 * time.Minute
	// snapshotsMax is the most read of restic's list of snapshots.
	snapshotsMax = 64 << 20
)

var (
	// As the agent takes them (internal/agent, snapshotOf).
	reResticID = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reSiteName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}[a-z0-9]$`)
	reRun      = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	reKind     = regexp.MustCompile(`^[a-z]{1,16}$`)
)

// The parts a site's backup has, in the order they are opened.
var siteParts = []string{partDB, partFiles, partDef}

var partTR = map[string]string{partDB: "veritabanı", partFiles: "dosyalar", partDef: "ayarlar", partServer: "sunucu ayarları"}

var kindTR = map[string]string{"daily": "gece", "hourly": "saatlik", "manual": "elle"}

// snapshot is one of FootOps' snapshots in a repository.
type snapshot struct {
	ID         string
	Time       time.Time
	Site, Part string
	Kind, Run  string
	// Files and Bytes are what the backup read (restic's summary): for a database, its
	// dump.
	Files, Bytes int64
}

func partPath(root, site, part string) string {
	switch part {
	case partDB:
		return root + "/" + site + "/db.archive"
	case partServer:
		return root + "/" + serverPart
	}
	return root + "/" + site + "/" + part
}

// parseSnapshots reads restic snapshots --json, FootOps' own only, newest first; other
// counts those left out. A repository is the server's word: a snapshot not as the agent
// makes them is not read (the agent's own rule, snapshotOf).
func parseSnapshots(data []byte, root string) ([]snapshot, int, error) {
	var raw []struct {
		ID      string    `json:"id"`
		Time    time.Time `json:"time"`
		Paths   []string  `json:"paths"`
		Tags    []string  `json:"tags"`
		Summary *struct {
			Files int64 `json:"total_files_processed"`
			Bytes int64 `json:"total_bytes_processed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, 0, fmt.Errorf("anlık görüntü listesi anlaşılamadı: %v", err)
	}
	out := []snapshot{}
	other := 0
	for _, r := range raw {
		s := snapshot{ID: r.ID, Time: r.Time.UTC()}
		ours := false
		for _, tag := range r.Tags {
			k, v, _ := strings.Cut(tag, "=")
			switch k {
			case "footops":
				ours = true
			case "site":
				s.Site = v
			case "part":
				s.Part = v
			case "kind":
				s.Kind = v
			case "run":
				s.Run = v
			}
		}
		ok := ours && reResticID.MatchString(s.ID) && !s.Time.IsZero() && len(r.Paths) == 1 &&
			(s.Run == "" || reRun.MatchString(s.Run)) && (s.Kind == "" || reKind.MatchString(s.Kind))
		if ok {
			switch {
			case s.Site == serverPart && s.Part == partServer:
			case reSiteName.MatchString(s.Site) && (s.Part == partDB || s.Part == partFiles || s.Part == partDef):
			default:
				ok = false
			}
		}
		if !ok || r.Paths[0] != partPath(root, s.Site, s.Part) {
			other++
			continue
		}
		if r.Summary != nil {
			s.Files, s.Bytes = max(r.Summary.Files, 0), max(r.Summary.Bytes, 0)
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, other, nil
}

// BackupPart is one part of a backup: its snapshot.
type BackupPart struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// Files and Bytes: what was backed up (for a database, its dump's size).
	Files int64 `json:"files,omitempty"`
	Bytes int64 `json:"bytes,omitempty"`
}

// BackupPoint is a site's backup as the panel lists it: the parts one run took that the
// repository still keeps.
type BackupPoint struct {
	Run   string                `json:"run"`
	Time  time.Time             `json:"time"` // the earliest part's
	Kind  string                `json:"kind,omitempty"`
	Parts map[string]BackupPart `json:"parts"`
}

// SiteBackups is a site's backups, newest first.
type SiteBackups struct {
	Name   string        `json:"name"`
	Points []BackupPoint `json:"points"`
}

// BackupList is footops-mac yedek liste's answer: every site's backups (or one site's).
type BackupList struct {
	Server string    `json:"server"`
	Bucket string    `json:"bucket"`
	Time   time.Time `json:"time"`
	TookMS int64     `json:"took_ms"`
	// Snapshots are FootOps' in the repository (of the site asked for); Other those left
	// out, not as the agent makes them.
	Snapshots int `json:"snapshots"`
	Other     int `json:"other,omitempty"`
	// ServerPart is the newest backup of the server's own settings.
	ServerPart time.Time     `json:"server_part,omitzero"`
	Sites      []SiteBackups `json:"sites"`
}

// pointsOf groups the snapshots by site and by the run that took them (the agent's
// pointsOf), the sites by name, their backups newest first.
func pointsOf(list []snapshot) []SiteBackups {
	bySite := map[string][]BackupPoint{}
	index := map[[2]string]int{}
	for _, s := range list {
		if s.Site == serverPart {
			continue
		}
		key := s.Run
		if key == "" {
			key = s.ID
		}
		i, ok := index[[2]string{s.Site, key}]
		if !ok {
			i = len(bySite[s.Site])
			index[[2]string{s.Site, key}] = i
			bySite[s.Site] = append(bySite[s.Site], BackupPoint{Run: key, Time: s.Time, Kind: s.Kind, Parts: map[string]BackupPart{}})
		}
		p := &bySite[s.Site][i]
		if _, dup := p.Parts[s.Part]; dup {
			continue // the newest kept (the list is newest first)
		}
		p.Parts[s.Part] = BackupPart{ID: s.ID, Time: s.Time, Files: s.Files, Bytes: s.Bytes}
		if s.Time.Before(p.Time) {
			p.Time = s.Time
		}
	}
	out := make([]SiteBackups, 0, len(bySite))
	for name, points := range bySite {
		sort.SliceStable(points, func(i, j int) bool { return points[i].Time.After(points[j].Time) })
		out = append(out, SiteBackups{Name: name, Points: points})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// choose picks what of a site's backups is opened: each part's newest at or before a
// moment — the newest (when ""), a backup's (its run, or the first 6+ letters of it), or a
// minute ("2026-11-07 04:00", in loc) with the backup that began in it whole — so a time
// copied from the list opens that backup, and parts it did not take come from the
// backups before (the recovery's rule: the site as it was then).
func choose(list []snapshot, site, when string, loc *time.Location) (map[string]snapshot, error) {
	var own []snapshot
	for _, s := range list {
		if s.Site == site {
			own = append(own, s)
		}
	}
	var at time.Time // zero: the newest
	switch {
	case when == "":
	case reRun.MatchString(when):
		runs := map[string]bool{}
		for _, s := range own {
			key := s.Run
			if key == "" {
				key = s.ID // a snapshot of no run is a backup of its own (pointsOf)
			}
			if key == when || (len(when) >= 6 && strings.HasPrefix(key, when)) {
				runs[key] = true
				if s.Time.After(at) {
					at = s.Time
				}
			}
		}
		switch {
		case len(runs) == 0:
			return nil, fmt.Errorf("bu yedek depoda yok: %s (saklama kuralıyla silinmiş olabilir; liste'ye bak)", when)
		case len(runs) > 1:
			return nil, fmt.Errorf("%q birden çok yedeğe uyuyor: kimliğin daha çok harfini yaz", when)
		}
	default:
		t, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(when), loc)
		if err != nil {
			return nil, fmt.Errorf("%w: zaman YYYY-AA-GG SS:DD biçiminde olmalı, örnek: 2026-11-07 04:00 (ya da liste'deki yedek kimliği)", errUsage)
		}
		end := t.Add(time.Minute - time.Nanosecond)
		at = end
		// A backup that began in that minute is taken whole, however long its parts took.
		first := map[string]time.Time{}
		last := map[string]time.Time{}
		for _, s := range own {
			key := s.Run
			if key == "" {
				key = s.ID
			}
			if f, ok := first[key]; !ok || s.Time.Before(f) {
				first[key] = s.Time
			}
			if s.Time.After(last[key]) {
				last[key] = s.Time
			}
		}
		for key, f := range first {
			if !f.Before(t) && !f.After(end) && last[key].After(at) {
				at = last[key]
			}
		}
	}
	chosen := map[string]snapshot{}
	for _, s := range own {
		if s.Part == partServer || (!at.IsZero() && s.Time.After(at)) {
			continue
		}
		if _, ok := chosen[s.Part]; !ok {
			chosen[s.Part] = s // the list is newest first
		}
	}
	if len(chosen) == 0 {
		if when != "" {
			return nil, fmt.Errorf("bu sitenin %s için yedeği yok: %s", whenTR(when), site)
		}
		return nil, fmt.Errorf("bu sitenin yedeği yok: %s", site)
	}
	return chosen, nil
}

func whenTR(when string) string {
	if reRun.MatchString(when) {
		return when + " yedeği"
	}
	return when + " ve öncesi"
}

// ---- the repository ----

// repo is one server's restic repository, ready to be read.
type repo struct {
	code, bucket string
	restic       string
	env          []string
	account      string // masked in what restic says
	root         string
}

// openRepo readies a server's repository: its bucket, the Mac's read-only key and the
// repository's password from the Keychain (only its name: restic asks for it).
func (e *env) openRepo(ctx context.Context, asked string) (*repo, error) {
	return e.openRepoWith(ctx, asked, "")
}

// openRepoWith is openRepo with the password from another Keychain item ("": the
// server's restic-<code>): sunucu sifre tries a password before it takes the place of
// the one there.
func (e *env) openRepoWith(ctx context.Context, asked, item string) (*repo, error) {
	code := strings.ToLower(strings.TrimSpace(asked))
	if !reCode.MatchString(code) {
		return nil, fmt.Errorf("geçersiz sunucu kodu: %q", clip(asked, 40))
	}
	restic, err := e.look("restic")
	if err != nil {
		return nil, errors.New("restic bulunamadı: brew install restic")
	}
	list, acc, err := e.servers(ctx)
	if err != nil {
		return nil, err
	}
	var srv *server
	for i := range list {
		if list[i].Code == code {
			srv = &list[i]
		}
	}
	if srv == nil {
		return nil, unknownServer(asked, list)
	}
	if item == "" {
		if !srv.Password {
			return nil, fmt.Errorf("Anahtar Zinciri'nde footops-yedek / restic-%s yok (bu sunucunun depo şifresi)", code)
		}
		item = "restic-" + code
	}
	pw := e.keychain.Command(item)
	for _, a := range pw {
		// restic splits the command at spaces itself.
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			return nil, fmt.Errorf("depo şifresini okuyacak komut kurulamadı: %q", a)
		}
	}
	env := []string{}
	for _, kv := range os.Environ() {
		// Nothing of another repository or account the shell may hold.
		if !strings.HasPrefix(kv, "RESTIC_") && !strings.HasPrefix(kv, "AWS_") {
			env = append(env, kv)
		}
	}
	env = append(env, "RESTIC_REPOSITORY="+e.repository(acc, srv.Bucket), "RESTIC_PASSWORD_COMMAND="+strings.Join(pw, " "),
		"AWS_ACCESS_KEY_ID="+acc.KeyID, "AWS_SECRET_ACCESS_KEY="+acc.Secret, "RESTIC_CACHE_DIR="+e.resticCache,
		"RESTIC_PROGRESS_FPS=2")
	return &repo{code: code, bucket: srv.Bucket, restic: restic, env: env, account: acc.Account, root: e.backupRoot}, nil
}

func unknownServer(asked string, list []server) error {
	var known []string
	for _, s := range list {
		known = append(known, s.Code)
	}
	return fmt.Errorf("bilinmeyen sunucu: %s (bilinenler: %s)", clip(asked, 40), strings.Join(known, ", "))
}

// run runs a restic command that only reads (--no-lock: no lock is taken, none is
// needed), its output into out. Stopped, restic gets SIGINT and a moment to end.
func (r *repo) run(ctx context.Context, out io.Writer, args ...string) error {
	full := append([]string{args[0], "--no-lock"}, args[1:]...)
	cmd := exec.CommandContext(ctx, r.restic, full...)
	cmd.Env = r.env
	cmd.Stdout = out
	errs := &tail{max: 8 << 10}
	cmd.Stderr = errs
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("restic %s içinde bitmedi: R2'ye ulaşılamıyor olabilir", limitTR(listTimeout))
	case ctx.Err() != nil:
		return errStopped
	case err != nil:
		return r.failure(err, errs.String())
	}
	return nil
}

// resticFailure is a restic command that failed: its exit code and what it said.
type resticFailure struct {
	code   int
	stderr string
	text   string
}

func (f *resticFailure) Error() string { return f.text }

// failure is what restic's exit says, in Turkish where it is known (restic 0.17+: 10 no
// repository, 12 wrong password), the account masked.
func (r *repo) failure(err error, stderr string) error {
	f := &resticFailure{code: -1, stderr: stderr}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		f.code = ee.ExitCode()
	}
	switch f.code {
	case 10:
		f.text = fmt.Sprintf("%s kovasında restic deposu yok: kova ya da klasör yanlış olabilir", r.bucket)
	case 12:
		f.text = fmt.Sprintf("depo şifresi yanlış: Anahtar Zinciri'ndeki footops-yedek / restic-%s bu deponun şifresi değil", r.code)
	default:
		// Masked before toolWords clips it: the account cut in half would show.
		f.text = "restic: " + toolWords(r.mask(stderr), r.mask(err.Error()))
	}
	return f
}

func (r *repo) mask(s string) string {
	if r.account == "" {
		return s
	}
	return strings.ReplaceAll(s, r.account, "<hesap>")
}

// toolWords is the line of a tool's errors that says why: restic's "Fatal:" (in JSON
// with --json: its exit_error, else the first error it names), mongorestore's
// "Failed:", else the last line.
func toolWords(stderr, fallback string) string {
	var fatal, first, last string
	for _, line := range strings.Split(stderr, "\n") {
		line = reToolTime.ReplaceAllString(strings.TrimSpace(line), "")
		if line == "" {
			continue
		}
		var m resticMessage
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &m) == nil && m.Type != "" {
			switch m.Type {
			case "exit_error":
				fatal = strings.TrimPrefix(m.Message, "Fatal: ")
			case "error":
				if first == "" {
					first = strings.TrimSpace(strings.TrimPrefix(m.Item+": ", ": ") + m.Error.Message)
				}
			}
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Fatal: "); ok && fatal == "" {
			fatal = rest
			continue
		}
		if rest, ok := strings.CutPrefix(line, "Failed: "); ok && fatal == "" {
			fatal = rest
			continue
		}
		last = line
	}
	text := fatal
	switch {
	case first != "" && (text == "" || strings.HasPrefix(text, "There were ")):
		text = first
		if fatal != "" {
			text += " (" + fatal + ")"
		}
	case text == "":
		text = last
	}
	if text == "" {
		text = fallback
	}
	return clip(clean(text), 300)
}

// reToolTime is the time mongodump and mongorestore put before each line they write
// ("2026-11-13T13:34:39.869+0300<TAB>").
var reToolTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?([+-]\d{4}|Z)\s+`)

// resticMessage is a line of restic's --json output.
type resticMessage struct {
	Type          string `json:"message_type"`
	Message       string `json:"message"`
	Item          string `json:"item"`
	TotalBytes    int64  `json:"total_bytes"`
	BytesRestored int64  `json:"bytes_restored"`
	Error         struct {
		Message string `json:"message"`
	} `json:"error"`
}

// snapshots lists FootOps' snapshots (of one site, if given), newest first.
func (r *repo) snapshots(ctx context.Context, site string) ([]snapshot, int, error) {
	buf := &capped{max: snapshotsMax}
	tag := "footops"
	if site != "" {
		tag += ",site=" + site
	}
	if err := r.run(ctx, buf, "snapshots", "--json", "--tag", tag); err != nil {
		return nil, 0, err
	}
	if buf.over {
		return nil, 0, errors.New("anlık görüntü listesi beklenenden büyük")
	}
	return parseSnapshots(buf.Bytes(), r.root)
}

// tail keeps the end of what is written (a tool's errors).
type tail struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// capped keeps up to max bytes and drops the rest (noting it), so the writer is never
// stopped half-way.
type capped struct {
	bytes.Buffer
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room < len(p) {
		c.over = true
		if room > 0 {
			c.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return c.Buffer.Write(p)
}

// ---- the commands ----

// backupCmd is footops-mac yedek …
func (e *env) backupCmd(ctx context.Context, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return errUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "sunucular":
		if len(rest) > 0 {
			return errUsage
		}
		return e.backupServers(ctx, jsonOut)
	case "dene":
		if len(rest) != 1 || anyFlag(rest) {
			return errUsage
		}
		return e.backupCheck(ctx, rest[0], jsonOut)
	case "liste":
		if len(rest) < 1 || len(rest) > 2 || anyFlag(rest) {
			return errUsage
		}
		site := ""
		if len(rest) == 2 {
			site = rest[1]
		}
		return e.backupList(ctx, rest[0], site, jsonOut)
	case "ac":
		req, err := openArgs(rest, true)
		if err != nil {
			return err
		}
		return e.openBackup(ctx, req, jsonOut)
	case "mongo":
		req, err := openArgs(rest, false)
		if err != nil {
			return err
		}
		return e.openBackup(ctx, req, jsonOut)
	case "acilanlar":
		if len(rest) > 0 {
			return errUsage
		}
		return e.openedCmd(ctx, jsonOut)
	case "sil":
		if len(rest) != 1 || anyFlag(rest) {
			return errUsage
		}
		return e.deleteOpened(ctx, rest[0], jsonOut)
	case "kurtarma":
		return e.recoveryCmd(ctx, rest, jsonOut)
	case "yardim", "help", "-h", "--help":
		fmt.Fprint(e.out, backupUsage)
		return nil
	}
	return fmt.Errorf("%w: bilinmeyen komut: yedek %s", errUsage, clip(sub, 40))
}

// anyFlag: an option among arguments that take none.
func anyFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return true
		}
	}
	return false
}

// BackupServer is a server whose backups the Mac can read; Name as the Mac calls it.
type BackupServer struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	// Password: its repository's password is in the Keychain (without it, its backups
	// cannot be opened).
	Password bool `json:"password"`
}

// backupServers is yedek sunucular: no secret is read but the bucket items.
func (e *env) backupServers(ctx context.Context, jsonOut bool) error {
	set, has, codes, err := e.known(ctx)
	if err != nil {
		return err
	}
	list := []BackupServer{}
	for _, c := range codes {
		s := BackupServer{Code: c, Name: strings.ToUpper(c), Bucket: "footops-" + c, Password: has["restic-"+c]}
		if n := set.Servers[c].usable().Name; n != "" {
			s.Name = n
		}
		if has["kova-"+c] {
			b, err := e.keychain.Get(ctx, "kova-"+c)
			if err != nil {
				return err
			}
			s.Bucket = strings.Trim(strings.TrimSpace(b), "/")
		}
		if st, ok := set.Servers[c]; ok && st.Bucket != "" {
			s.Bucket = strings.Trim(st.Bucket, "/")
		}
		list = append(list, s)
	}
	if jsonOut {
		e.event(map[string]any{"servers": list})
		return nil
	}
	if len(list) == 0 {
		return errors.New("Anahtar Zinciri'nde footops-yedek / restic-<sunucu> kaydı yok")
	}
	printBackupServers(e.out, list)
	return nil
}

// BackupCheck is yedek dene's answer.
type BackupCheck struct {
	Server    string `json:"server"`
	Bucket    string `json:"bucket"`
	Snapshots int    `json:"snapshots"`
	TookMS    int64  `json:"took_ms"`
}

// backupCheck opens a repository as a recovery would (its config, no lock) and counts
// its snapshots.
func (e *env) backupCheck(ctx context.Context, server string, jsonOut bool) error {
	start := time.Now()
	r, n, err := e.countSnapshots(ctx, server)
	if err != nil {
		return err
	}
	c := BackupCheck{Server: r.code, Bucket: r.bucket, Snapshots: n, TookMS: time.Since(start).Milliseconds()}
	if jsonOut {
		e.event(c)
		return nil
	}
	fmt.Fprintf(e.out, "✓ %s açıldı (salt okunur): %d anlık görüntü\n", c.Bucket, c.Snapshots)
	return nil
}

// countSnapshots opens a repository as a recovery would (its config, no lock) and
// counts its snapshots (yedek dene, sunucu dene).
func (e *env) countSnapshots(ctx context.Context, server string) (*repo, int, error) {
	r, err := e.openRepo(ctx, server)
	if err != nil {
		return nil, 0, err
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	if err := r.run(lctx, io.Discard, "cat", "config"); err != nil {
		return nil, 0, err
	}
	list, _, err := r.snapshots(lctx, "")
	if err != nil {
		return nil, 0, err
	}
	return r, len(list), nil
}

// backupList is yedek liste: every site's backups, or one site's.
func (e *env) backupList(ctx context.Context, server, site string, jsonOut bool) error {
	if site != "" && !reSiteName.MatchString(site) {
		return fmt.Errorf("geçersiz site adı: %q", clip(site, 40))
	}
	start := time.Now()
	r, err := e.openRepo(ctx, server)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	list, other, err := r.snapshots(lctx, site)
	if err != nil {
		return err
	}
	out := BackupList{Server: r.code, Bucket: r.bucket, Time: e.now().Truncate(time.Second), Snapshots: len(list), Other: other,
		Sites: pointsOf(list)}
	for _, s := range list {
		if s.Site == serverPart {
			out.ServerPart = s.Time
			break
		}
	}
	out.TookMS = time.Since(start).Milliseconds()
	if jsonOut {
		e.event(out)
		return nil
	}
	if site != "" {
		if len(out.Sites) == 0 {
			return fmt.Errorf("bu sitenin yedeği yok: %s", site)
		}
		printSiteBackups(e.out, out.Sites[0], e.now())
		return nil
	}
	printBackupList(e.out, out, e.now())
	return nil
}
