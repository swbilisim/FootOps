// Package mac is footops-mac, the Mac's side of FootOps (panel-tasarim §29): a command
// for the Terminal and for FootOps.app, which runs it for each thing it shows. It reads
// what the servers leave in their R2 buckets with the Mac's read-only key and visits the
// sites from the Mac. It never talks to the panels and has no way to change a server.
package mac

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/swbilisim/FootOps/internal/version"
)

const usage = `footops-mac — FootOps'un Mac tarafı (Terminal ve FootOps.app)

Kullanım: footops-mac <komut> [--json]

  durum              sunucuların durumu: R2'deki durum dosyası (sunucunun FootOps'u yazar)
                     ve dış nöbetçinin gördüğü — sunucu başına iki küçük okuma, salt okunur anahtarla
  yokla [sunucu…]    sitelere bu Mac'ten şimdi bakar, nöbetçinin gördüğüyle yan yana gösterir
  yedek <komut>      sunucuların yedekleri: liste, klasöre / Mac'teki MongoDB'ye açma, kurtarma satırı
                     (yalnız okur; ayrıntı: footops-mac yedek yardim)
  ayar               bu Mac'in ayarları: sunucu başına panel adresi, SSH hedefi, kova
  ayar yaz           ayarları stdin'den JSON olarak alır, denetler ve yazar (FootOps.app'in Ayarlar'ı)
  surum              sürüm

  --json             FootOps.app için: tek JSON nesnesi (durum, ayar, yedek liste…) ya da satır satır
                     olaylar (yokla, yedek ac)

Sırlar yalnız Anahtar Zinciri'nde (hizmet footops-yedek): r2-hesap, r2-mac-erisim, r2-mac-gizli.
Sunucular restic-<kod> kayıtlarından ve ayarlar dosyasından gelir:
  ~/Library/Application Support/FootOps/ayarlar.json
`

// env is everything a command reaches outside itself; tests put stand-ins in its place.
type env struct {
	in        io.Reader // ayar yaz's settings
	out, errw io.Writer
	keychain  Keychain
	// settings is the settings file's path.
	settings string
	// r2 is the address of an R2 account's S3 endpoint.
	r2 func(account string) string
	// client reads from R2; probe visits the sites (nil: a fresh one per run).
	client       *http.Client
	probe        *http.Client
	probeTimeout time.Duration
	now          func() time.Time

	// The backups (yedek …). look finds a tool (restic, mongorestore); repository is a
	// server's restic repository; backupRoot where the parts sit in it (/footops).
	look        func(name string) (string, error)
	repository  func(acc access, bucket string) string
	backupRoot  string
	resticCache string
	// downloads is where a backup is opened (~/Downloads); opened the list of what was
	// (acilanlar.json); tmp a temporary folder's place ("": the system's).
	downloads string
	opened    string
	tmp       string
	mongoURI  string
	// terminal: the output is a terminal (a secret is not written there); free the free
	// room of a folder's disk.
	terminal func() bool
	free     func(dir string) (uint64, error)
	// lost stops the command when its output can no longer be written; exclude keeps a
	// folder out of Time Machine.
	lost    func()
	exclude func(dir string)
}

func defaultEnv(stdout, stderr io.Writer) *env {
	service := os.Getenv("FOOTOPS_YEDEK_SERVICE")
	if service == "" {
		service = "footops-yedek"
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), "Library", "Application Support")
	}
	client := &http.Client{Timeout: 15 * time.Second,
		// A redirect is an answer, not a way: R2 sends none.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	home, _ := os.UserHomeDir()
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = filepath.Join(home, "Library", "Caches")
	}
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		mongoURI = defaultMongoURI
	}
	e := &env{in: os.Stdin, out: stdout, errw: stderr, keychain: securityKeychain{Tool: "/usr/bin/security", Service: service},
		settings: filepath.Join(dir, "FootOps", "ayarlar.json"),
		r2:       func(account string) string { return "https://" + account + ".r2.cloudflarestorage.com" },
		client:   client, probeTimeout: probeTimeout, now: time.Now,
		look: lookTool, backupRoot: defaultBackupRoot, resticCache: filepath.Join(cache, "FootOps", "restic"),
		downloads: filepath.Join(home, "Downloads"), opened: filepath.Join(dir, "FootOps", "acilanlar.json"), mongoURI: mongoURI,
		terminal: func() bool {
			f, ok := stdout.(*os.File)
			return ok && term.IsTerminal(int(f.Fd()))
		},
		free: freeRoom, exclude: timeMachineExclude}
	e.repository = func(acc access, bucket string) string { return "s3:" + e.r2(acc.Account) + "/" + bucket }
	return e
}

// lookTool finds a tool in PATH, then where Homebrew puts it: an app started by macOS
// has a PATH without Homebrew's folders.
func lookTool(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s bulunamadı", name)
}

// timeMachineExclude keeps a folder out of Time Machine (tmutil's sticky exclusion: it
// goes with the folder, no root needed), so an opened backup's secrets deleted from the
// Mac are not kept in its backups. Elsewhere (Linux), or failing, nothing.
func timeMachineExclude(dir string) {
	const tmutil = "/usr/bin/tmutil"
	if _, err := os.Stat(tmutil); err != nil {
		return
	}
	cmd := exec.Command(tmutil, "addexclusion", dir)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

// freeRoom is the room left for the owner on a folder's disk.
func freeRoom(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// Run runs the command in args[0] and returns the process's exit code. FootOps.app
// stops a command with SIGTERM: its context ends and it cleans up after itself.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx, lost, done := commandContext()
	defer done()
	e := defaultEnv(stdout, stderr)
	e.lost = lost
	return e.run(ctx, args)
}

// commandContext is a command's context, ended by SIGINT, SIGTERM or SIGHUP (the
// Terminal window closed) — a second one ends the process at once, whatever it waits on
// — or by lost: its output gone. A closed pipe is an error to the writer, not the end of
// the process (Go's default for fd 1 and 2): the app that quit or crashed under a yedek
// ac must not leave half a folder behind. Not ignored either: restic and mongorestore
// would inherit that.
func commandContext() (ctx context.Context, lost context.CancelFunc, done func()) {
	sigs, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, lost = context.WithCancel(sigs)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, lost, func() {
		lost()
		stop()
	}
}

// errUsage is a command used wrongly: the usage is shown, exit code 2.
var errUsage = errors.New("kullanım hatası")

func (e *env) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.errw, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	var jsonOut bool
	var pos []string
	for _, a := range rest {
		switch {
		case a == "--json":
			jsonOut = true
		case strings.HasPrefix(a, "-") && cmd != "yedek": // yedek's own are its own
			fmt.Fprintf(e.errw, "bilinmeyen seçenek: %s\n\n%s", a, usage)
			return 2
		default:
			pos = append(pos, a)
		}
	}
	var err error
	switch cmd {
	case "durum":
		if len(pos) > 0 {
			err = errUsage
			break
		}
		err = e.status(ctx, jsonOut)
	case "yokla":
		err = e.probeCmd(ctx, pos, jsonOut)
	case "yedek":
		err = e.backupCmd(ctx, pos, jsonOut)
	case "ayar":
		err = e.settingsCmd(ctx, pos, jsonOut)
	case "surum", "version", "--version", "-v":
		fmt.Fprintf(e.out, "footops-mac %s %s/%s\n", version.String(), runtime.GOOS, runtime.GOARCH)
	case "yardim", "help", "--help", "-h":
		fmt.Fprint(e.out, usage)
	default:
		fmt.Fprintf(e.errw, "bilinmeyen komut: %s\n\n%s", cmd, usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errUsage):
		if err != errUsage { // said why
			fmt.Fprintf(e.errw, "%s\n\n", strings.TrimPrefix(err.Error(), errUsage.Error()+": "))
		}
		if cmd == "yedek" {
			fmt.Fprint(e.errw, backupUsage)
		} else {
			fmt.Fprint(e.errw, usage)
		}
		return 2
	case errors.Is(err, errReported):
		return 1
	}
	if errors.Is(err, errStopped) {
		err = errStopped // what it stopped is no news: the app knows the word
	}
	if jsonOut {
		ev := map[string]any{"event": "error", "message": err.Error()}
		var ce *codedError
		if errors.As(err, &ce) {
			ev["code"] = ce.code
			for k, v := range ce.fields {
				ev[k] = v
			}
		}
		e.event(ev)
	} else {
		fmt.Fprintf(e.errw, "✗ %v\n", err)
	}
	return 1
}

// codedError is an error the app answers in a way of its own, by its code (db_exists:
// it asks before writing over a database); fields go into the error's JSON beside it.
type codedError struct {
	code   string
	text   string
	fields map[string]any
}

func (c *codedError) Error() string { return c.text }

// errReported: the command has said what went wrong in its own output (its JSON
// object); only the exit code is left to give.
var errReported = errors.New("bildirildi")

// errStopped: SIGTERM (or ^C) came; the command cleaned up and gave up.
var errStopped = errors.New("durduruldu")

// event writes one JSON line: a whole answer (durum) or one step of a longer command.
// A line that cannot be written (nobody reads any more: the app is gone) stops the
// command as SIGTERM would.
func (e *env) event(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(map[string]any{"event": "error", "message": err.Error()})
	}
	if _, err := e.out.Write(append(data, '\n')); err != nil && e.lost != nil {
		e.lost()
	}
}
