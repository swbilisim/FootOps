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
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/swbilisim/FootOps/internal/version"
)

const usage = `footops-mac — FootOps'un Mac tarafı (Terminal ve FootOps.app)

Kullanım: footops-mac <komut> [--json]

  durum              sunucuların durumu: R2'deki durum dosyası (sunucunun FootOps'u yazar)
                     ve dış nöbetçinin gördüğü — sunucu başına iki küçük okuma, salt okunur anahtarla
  yokla [sunucu…]    sitelere bu Mac'ten şimdi bakar, nöbetçinin gördüğüyle yan yana gösterir
  surum              sürüm

  --json             FootOps.app için: tek JSON nesnesi (durum) ya da satır satır olaylar (yokla)

Sırlar yalnız Anahtar Zinciri'nde (hizmet footops-yedek): r2-hesap, r2-mac-erisim, r2-mac-gizli.
Sunucular restic-<kod> kayıtlarından ve ayarlar dosyasından gelir:
  ~/Library/Application Support/FootOps/ayarlar.json
`

// env is everything a command reaches outside itself; tests put stand-ins in its place.
type env struct {
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
	return &env{out: stdout, errw: stderr, keychain: securityKeychain{Tool: "/usr/bin/security", Service: service},
		settings: filepath.Join(dir, "FootOps", "ayarlar.json"),
		r2:       func(account string) string { return "https://" + account + ".r2.cloudflarestorage.com" },
		client:   client, probeTimeout: probeTimeout, now: time.Now}
}

// Run runs the command in args[0] and returns the process's exit code. FootOps.app
// stops a command with SIGTERM: its context ends and it cleans up after itself.
func Run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A second signal ends the process at once, whatever it is waiting on.
	go func() {
		<-ctx.Done()
		stop()
	}()
	return defaultEnv(stdout, stderr).run(ctx, args)
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
		case strings.HasPrefix(a, "-"):
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
		fmt.Fprint(e.errw, usage)
		return 2
	case errors.Is(err, errReported):
		return 1
	}
	if jsonOut {
		e.event(map[string]any{"event": "error", "message": err.Error()})
	} else {
		fmt.Fprintf(e.errw, "✗ %v\n", err)
	}
	return 1
}

// errReported: the command has said what went wrong in its own output (its JSON
// object); only the exit code is left to give.
var errReported = errors.New("bildirildi")

// errStopped: SIGTERM (or ^C) came; the command cleaned up and gave up.
var errStopped = errors.New("durduruldu")

// event writes one JSON line: a whole answer (durum) or one step of a longer command.
func (e *env) event(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(map[string]any{"event": "error", "message": err.Error()})
	}
	e.out.Write(append(data, '\n'))
}
