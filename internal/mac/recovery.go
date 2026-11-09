package mac

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// recoveryPrefix starts a recovery line: what "yedekten kur" asks for in one paste
// (internal/install reads it, ParseRecoveryInfo).
const recoveryPrefix = "FOOTOPS-KURTARMA-1:"

// recoveryCmd is yedek kurtarma <server> [--goster]: the repository, the Mac's read-only
// key and the repository's password in one line. It is a secret: never written to a
// terminal unless asked (--goster), never as JSON — only into a pipe: pbcopy, or the
// app, which puts it on the pasteboard alone.
func (e *env) recoveryCmd(ctx context.Context, args []string, jsonOut bool) error {
	show := false
	var pos []string
	for _, a := range args {
		switch {
		case a == "--goster":
			show = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("%w: bilinmeyen seçenek: %s", errUsage, clip(a, 40))
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 1 {
		return errUsage
	}
	code := strings.ToLower(strings.TrimSpace(pos[0]))
	switch {
	case !reCode.MatchString(code):
		return fmt.Errorf("geçersiz sunucu kodu: %q", clip(pos[0], 40))
	case jsonOut:
		return fmt.Errorf("kurtarma satırı JSON'a yazılmaz (sır içerir): footops-mac yedek kurtarma %s | pbcopy", code)
	case e.terminal() && !show:
		return fmt.Errorf("bu satır sır içerir; ekrana yazılmaz. Panoya al: footops-mac yedek kurtarma %s | pbcopy", code)
	}
	list, acc, err := e.servers(ctx)
	if err != nil {
		return err
	}
	var srv *server
	for i := range list {
		if list[i].Code == code {
			srv = &list[i]
		}
	}
	switch {
	case srv == nil:
		return unknownServer(code, list)
	case !srv.Password:
		return fmt.Errorf("Anahtar Zinciri'nde footops-yedek / restic-%s yok (bu sunucunun depo şifresi)", code)
	}
	pw, err := e.keychain.Get(ctx, "restic-"+code)
	if errors.Is(err, errNoItem) {
		return fmt.Errorf("Anahtar Zinciri'nde footops-yedek / restic-%s yok (bu sunucunun depo şifresi)", code)
	}
	if err != nil {
		return err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Repository string `json:"repository"`
		KeyID      string `json:"key_id"`
		SecretKey  string `json:"secret_key"`
		Password   string `json:"password"`
	}{e.repository(acc, srv.Bucket), acc.KeyID, acc.Secret, pw}); err != nil {
		return err
	}
	line := recoveryPrefix + base64.StdEncoding.EncodeToString(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	if f, ok := e.out.(*os.File); ok {
		// Sent into a file (> dosya), the file is the owner's only before the secret is in
		// it (the shell made it with its own umask).
		if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
			if err := f.Chmod(0o600); err != nil {
				return fmt.Errorf("kurtarma satırının yazılacağı dosya yalnız sana açılamadı: %v", err)
			}
		}
	}
	fmt.Fprintln(e.out, line)
	fmt.Fprintf(e.errw, "Kurtarma bilgisi yazıldı (%s, salt okunur anahtar). Sunucuda \"Kurtarma bilgisi\" sorusuna yapıştır;\n"+
		"panoya aldıysan sonra temizle: pbcopy </dev/null\n", srv.Bucket)
	return nil
}
