package mac

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/swbilisim/FootOps/internal/monitor"
	"github.com/swbilisim/FootOps/internal/s3obj"
)

const serverUsage = `footops-mac sunucu — bu Mac'in bildiği sunucular (yalnız bu Mac; sunuculara dokunmaz)

Kullanım: footops-mac sunucu <komut> [--json]

  ekle           yeni sunucu: kod, ad, panel, SSH, kova ve yedek deposu şifresi (Terminal'de
                 sorar; FootOps.app stdin'den JSON verir); şifre yalnız Anahtar Zinciri'ne yazılır
  dene <kod>     sunucuyu bu Mac'ten dener: kovadaki durum dosyası (salt okunur anahtarla) ve
                 şifre varsa yedek deposu; ikisini de yalnız okur
  sifre <kod>    yedek deposunun şifresini yazar ya da değiştirir (Anahtar Zinciri, restic-<kod>;
                 Terminal'de görünmeden sorar, yoksa stdin'den okur)
  kaldir <kod>   sunucuyu bu Mac'ten kaldırır: ayarları ve Anahtar Zinciri kayıtları (restic-<kod>,
                 kova-<kod>); sunucuya ve yedeklerine bir şey olmaz
  r2             R2 erişimi: hesap kimliği ve bu Mac'in salt okunur anahtarı Anahtar Zinciri'nde mi
  r2 yaz         R2 erişimini yazar (Terminal'de sorar; FootOps.app stdin'den JSON verir)

Kod, sunucuya FootOps kurulurken verilen koddur (ör. h1; küçük harfle tutulur). Kova boşsa
footops-<kod> okunur. Yeni bir kova, Cloudflare'de bu Mac'in salt okunur anahtarına
eklenince okunur.
`

var (
	// reNewCode is a server's code as one is added: the code it was given at its install
	// (letters, digits and dashes) in lower case, a letter or digit first — the commands
	// that take it would read a dash first as an option.
	reNewCode = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// rePassword is a repository's password as FootOps makes one (40 letters and digits)
	// or one like it: what goes to the Keychain as a word of security's (reItemWord).
	rePassword = regexp.MustCompile(`^[A-Za-z0-9._@+/=][A-Za-z0-9._@+/=-]{11,127}$`)
	// An access key of R2's (32 hex digits, its secret 64) or of S3's like it.
	reKeyID     = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
	reKeySecret = regexp.MustCompile(`^[A-Za-z0-9/+=]{20,128}$`)
)

// serverCmd is footops-mac sunucu: the servers this Mac knows, added and taken off on
// this Mac only — nothing of it reaches a server.
func (e *env) serverCmd(ctx context.Context, args []string, jsonOut bool) error {
	if len(args) == 0 {
		return errUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ekle":
		if len(rest) > 0 {
			return errUsage
		}
		return e.addServer(ctx, jsonOut)
	case "dene":
		if len(rest) != 1 {
			return errUsage
		}
		return e.tryServer(ctx, rest[0], jsonOut)
	case "sifre":
		if len(rest) != 1 {
			return errUsage
		}
		return e.setPassword(ctx, rest[0], jsonOut)
	case "kaldir":
		if len(rest) != 1 {
			return errUsage
		}
		return e.removeServer(ctx, rest[0], jsonOut)
	case "r2":
		switch {
		case len(rest) == 0:
			return e.r2Cmd(ctx, jsonOut)
		case len(rest) == 1 && rest[0] == "yaz":
			return e.writeR2(ctx, jsonOut)
		}
		return errUsage
	case "yardim", "help":
		fmt.Fprint(e.out, serverUsage)
		return nil
	}
	return fmt.Errorf("%w: bilinmeyen komut: sunucu %s", errUsage, clip(sub, 40))
}

// NewServer is a server to add (sunucu ekle's input). Password, when given, goes to the
// Keychain only.
type NewServer struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Panel    string `json:"panel"`
	SSH      string `json:"ssh"`
	Bucket   string `json:"bucket"`
	Password string `json:"password"`
}

// normalize trims what was typed: the code in lower case, the bucket without slashes at
// its ends, the panel as written again (HTTPS:// is https://).
func (n *NewServer) normalize() {
	n.Code = strings.ToLower(strings.TrimSpace(n.Code))
	n.Name = strings.TrimSpace(n.Name)
	n.Panel = strings.TrimSpace(n.Panel)
	n.SSH = strings.TrimSpace(n.SSH)
	n.Bucket = strings.Trim(strings.TrimSpace(n.Bucket), "/")
	n.Password = strings.TrimSpace(n.Password)
	if u, err := url.Parse(n.Panel); n.Panel != "" && err == nil && checkPanel(n.Panel) == nil {
		n.Panel = u.String()
	}
}

func (n NewServer) check() error {
	if !reNewCode.MatchString(n.Code) {
		return fmt.Errorf("sunucu kodu geçersiz: %q (harf, rakam ve tire, en çok 32 karakter: sunucuya FootOps kurulurken verilen kod)",
			clip(n.Code, 40))
	}
	checks := []struct {
		value string
		check func(string) error
	}{{n.Name, checkName}, {n.Panel, checkPanel}, {n.SSH, checkSSH}, {n.Bucket, checkBucket}, {n.Password, checkPassword}}
	for _, c := range checks {
		if c.value != "" {
			if err := c.check(c.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPassword says nothing of the password but its length.
func checkPassword(p string) error {
	if !rePassword.MatchString(p) {
		return fmt.Errorf("yedek deposu şifresi beklenen biçimde değil (%d karakter): FootOps'un verdiği şifre 40 harf ve rakamdır",
			utf8.RuneCountInString(p))
	}
	return nil
}

// addServer is sunucu ekle: the password into the Keychain, the rest into the settings.
func (e *env) addServer(ctx context.Context, jsonOut bool) error {
	var ns NewServer
	if !jsonOut && e.terminalIn() {
		a := e.asker()
		fmt.Fprintln(e.errw, "Yeni sunucu — yalnız bu Mac'e eklenir, sunucuya dokunulmaz.")
		for _, q := range []struct {
			text string
			to   *string
		}{{"Kod (sunucuya FootOps kurulurken verilen, ör. h2)", &ns.Code}, {"Ad (menüde görünen; boş: kodun büyük harfi)", &ns.Name},
			{"Panel adresi (https://…; boş geçilebilir)", &ns.Panel}, {"SSH hedefi (kullanıcı@sunucu; boş geçilebilir)", &ns.SSH},
			{"Kova (boş: footops-<kod>)", &ns.Bucket}} {
			v, err := a.line(q.text)
			if err != nil {
				return err
			}
			*q.to = v
		}
		pw, err := a.secret("Yedek deposu şifresi (panelin Yedekleme sayfasının bir kez gösterdiği; görünmez; boş: sonra)")
		if err != nil {
			return err
		}
		ns.Password = pw
	} else if err := e.readInput(&ns); err != nil {
		return err
	}
	ns.normalize()
	if err := ns.check(); err != nil {
		return err
	}
	set, _, codes, err := e.known(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(codes, ns.Code) {
		return fmt.Errorf("bu kodla bir sunucu zaten var: %s (Ayarlar'da düzenlenir)", ns.Code)
	}
	entry := ServerSettings{Name: ns.Name, Panel: ns.Panel, SSH: ns.SSH, Bucket: ns.Bucket}
	if entry == (ServerSettings{}) && ns.Password == "" {
		return errors.New("sunucu için en az bir bilgi gerekli: ad, panel, SSH, kova ya da yedek deposu şifresi")
	}
	// The password first: a server is known by its Keychain item too, so a file that
	// cannot be written leaves nothing behind but that item, taken back.
	if ns.Password != "" {
		if err := e.keychain.Set(ctx, "restic-"+ns.Code, ns.Password); err != nil {
			return err
		}
	}
	if entry != (ServerSettings{}) {
		if set.Servers == nil {
			set.Servers = map[string]ServerSettings{}
		}
		set.Format, set.Servers[ns.Code] = settingsFormat, entry
		if err := saveSettings(e.settings, set); err != nil {
			if ns.Password != "" {
				_ = e.keychain.Delete(context.WithoutCancel(ctx), "restic-"+ns.Code)
			}
			return err
		}
	}
	return e.answerSettings(ctx, jsonOut, fmt.Sprintf("✓ Eklendi: %s (%s) — dene: footops-mac sunucu dene %s",
		displayName(ns.Code, map[string]string{ns.Code: ns.Name}), ns.Code, ns.Code))
}

// setPassword is sunucu sifre: a server's repository password, into the Keychain.
func (e *env) setPassword(ctx context.Context, asked string, jsonOut bool) error {
	code, err := e.knownCode(ctx, asked)
	if err != nil {
		return err
	}
	var pw string
	if !jsonOut && e.terminalIn() {
		if pw, err = e.asker().secret(fmt.Sprintf("%s yedek deposu şifresi (görünmez)", strings.ToUpper(code))); err != nil {
			return err
		}
	} else {
		data, err := io.ReadAll(io.LimitReader(e.in, 4<<10+1))
		if err != nil {
			return fmt.Errorf("şifre okunamadı: %v", err)
		}
		if len(data) > 4<<10 {
			return errors.New("şifre çok uzun")
		}
		pw = string(data)
	}
	if pw = strings.TrimSpace(pw); pw == "" {
		return errors.New("şifre boş")
	}
	if err := checkPassword(pw); err != nil {
		return err
	}
	tried, err := e.passwordOpens(ctx, code, pw)
	if err != nil {
		return err
	}
	if err := e.keychain.Set(ctx, "restic-"+code, pw); err != nil {
		return err
	}
	return e.answerSettings(ctx, jsonOut, fmt.Sprintf("✓ Yedek deposu şifresi Anahtar Zinciri'ne yazıldı (footops-yedek / restic-%s; %s)", code, tried))
}

// passwordOpens tries a password on the server's repository before it takes the place
// of the one there, from a Keychain item of its own (deneme-restic-<code>: no server is
// known by it), deleted after. Wrong (restic 12): an error, nothing written. Not to be
// told — no restic, no R2 access, R2 or the repository not reached —: written all the
// same (a server not yet readable gets its password first), and said so.
func (e *env) passwordOpens(ctx context.Context, code, pw string) (string, error) {
	item := "deneme-restic-" + code
	if err := e.keychain.Set(ctx, item, pw); err != nil {
		return "", err
	}
	defer e.keychain.Delete(context.WithoutCancel(ctx), item)
	r, err := e.openRepoWith(ctx, code, item)
	if err == nil {
		lctx, cancel := context.WithTimeout(ctx, listTimeout)
		defer cancel()
		err = r.run(lctx, io.Discard, "cat", "config")
	}
	var rf *resticFailure
	switch {
	case err == nil:
		return "depoyu açtığı denendi", nil
	case errors.Is(err, errStopped):
		return "", err
	case errors.As(err, &rf) && rf.code == 12:
		bucket := ""
		if r != nil {
			bucket = r.bucket + " "
		}
		return "", fmt.Errorf("bu şifre %sdeposunu açmadı: Anahtar Zinciri'ndeki şifreye dokunulmadı (yanlış yazılmış olabilir; Vaultwarden'daki kayıtla karşılaştır)", bucket)
	}
	return "depoyu açtığı denenemedi: " + err.Error(), nil
}

// removeServer is sunucu kaldir: the server's settings and Keychain items off this Mac.
// What was opened of its backups here stays (yedek acilanlar).
func (e *env) removeServer(ctx context.Context, asked string, jsonOut bool) error {
	code, err := e.knownCode(ctx, asked)
	if err != nil {
		return err
	}
	set, err := readSettings(e.settings)
	if err != nil {
		return err
	}
	name := displayName(code, map[string]string{code: set.Servers[code].usable().Name})
	// The Keychain's items first, its password last: a server is known by that and by
	// its settings, so one cut short is still on the list, to be taken off again — and
	// leaves no bucket behind for one added later under the same code.
	for _, item := range []string{"kova-" + code, "restic-" + code} {
		if err := e.keychain.Delete(ctx, item); err != nil {
			return err
		}
	}
	if _, ok := set.Servers[code]; ok {
		delete(set.Servers, code)
		if err := saveSettings(e.settings, set); err != nil {
			return err
		}
	}
	return e.answerSettings(ctx, jsonOut, fmt.Sprintf("✓ %s (%s) bu Mac'ten kaldırıldı: ayarları ve Anahtar Zinciri kayıtları silindi; sunucuya ve yedeklerine dokunulmadı",
		name, code))
}

// knownCode is a server of this Mac's, by its code as asked.
func (e *env) knownCode(ctx context.Context, asked string) (string, error) {
	code := strings.ToLower(strings.TrimSpace(asked))
	if !reCode.MatchString(code) {
		return "", fmt.Errorf("geçersiz sunucu kodu: %q", clip(asked, 40))
	}
	_, _, codes, err := e.known(ctx)
	if err != nil {
		return "", err
	}
	if !slices.Contains(codes, code) {
		return "", fmt.Errorf("bilinmeyen sunucu: %s (bilinenler: %s)", clip(asked, 40), strings.Join(codes, ", "))
	}
	return code, nil
}

// answerSettings answers the app with the settings as they are now (its list), the
// Terminal with what was done.
func (e *env) answerSettings(ctx context.Context, jsonOut bool, done string) error {
	if !jsonOut {
		fmt.Fprintln(e.out, done)
		return nil
	}
	e.event(e.settingsView(ctx))
	return nil
}

// ServerTry is sunucu dene's answer: what was tried, a line each; OK: nothing failed.
type ServerTry struct {
	Code   string     `json:"code"`
	Name   string     `json:"name"`
	Bucket string     `json:"bucket"`
	Checks []TryCheck `json:"checks"`
	OK     bool       `json:"ok"`
	TookMS int64      `json:"took_ms"`
}

// TryCheck is one thing tried (Part: access, status or backups): State ok, warn (it
// works, with a word of care), fail, or skip (not tried; why).
type TryCheck struct {
	Part  string `json:"part"`
	State string `json:"state"`
	Text  string `json:"text"`
}

// tryServer is sunucu dene: the server as this Mac reaches it — its status file with
// the Mac's read-only key, its repository with its password. Both only read.
func (e *env) tryServer(ctx context.Context, asked string, jsonOut bool) error {
	start := time.Now()
	code := strings.ToLower(strings.TrimSpace(asked))
	if !reCode.MatchString(code) {
		return fmt.Errorf("geçersiz sunucu kodu: %q", clip(asked, 40))
	}
	list, err := e.serverList(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(list, func(s server) bool { return s.Code == code })
	if i < 0 {
		return unknownServer(asked, list)
	}
	srv := list[i]
	t := ServerTry{Code: code, Name: displayName(code, map[string]string{code: srv.Name}), Bucket: srv.Bucket, Checks: []TryCheck{}}
	add := func(part, state, format string, args ...any) {
		t.Checks = append(t.Checks, TryCheck{Part: part, State: state, Text: fmt.Sprintf(format, args...)})
	}
	acc, err := e.readAccess(ctx)
	var na *noAccessError
	switch {
	case errors.As(err, &na):
		add("access", "fail", "%s: Ayarlar → R2 erişimi", na.text)
	case err != nil:
		return err
	default:
		worth := e.tryStatus(ctx, acc, srv, add)
		e.tryBackups(ctx, srv, worth, add)
	}
	t.OK = !slices.ContainsFunc(t.Checks, func(c TryCheck) bool { return c.State == "fail" })
	t.TookMS = time.Since(start).Milliseconds()
	if jsonOut {
		e.event(t)
	} else {
		printTry(e.out, t)
	}
	if !t.OK {
		return errReported
	}
	return nil
}

// tryStatus reads the server's status file as the menu does. Worth: its backups are
// worth trying (the bucket did not refuse the Mac's key, R2 was reached).
func (e *env) tryStatus(ctx context.Context, acc access, srv server, add func(part, state, format string, args ...any)) (worth bool) {
	f := e.fetch(ctx, acc, srv.Bucket, monitor.StatusKey)
	file, err := decode(f, func(f *monitor.StatusFile) bool { return f.Format >= 1 && !f.Time.IsZero() })
	var se *s3obj.Error
	switch {
	case errors.As(err, &se) && (se.Status == 401 || se.Status == 403):
		bucket, _, _ := strings.Cut(srv.Bucket, "/")
		switch se.Code {
		case "SignatureDoesNotMatch", "InvalidAccessKeyId", "InvalidToken", "ExpiredToken":
			// The key itself refused, whatever the bucket.
			add("status", "fail", "R2 bu Mac'in anahtarını tanımadı (%s): anahtar kimliği ya da gizlisi yanlış ya da anahtar silinmiş — Ayarlar → R2 erişimi.",
				se.Code)
		default:
			add("status", "fail", "Kova bu Mac'in salt okunur anahtarıyla okunamıyor (R2: %d %s). Cloudflare → R2 → API token'ları → bu Mac'in salt okunur anahtarı → %s kovasını ekle.",
				se.Status, orDash(se.Code), bucket)
		}
		return false
	case errors.Is(err, s3obj.ErrNoObject):
		add("status", "fail", "Kovada durum dosyası yok (%s/%s): kova adı doğru mu, sunucuda yedekleme ayarlı mı (panel → Yedekleme)?",
			srv.Bucket, monitor.StatusKey)
		return true
	case err != nil:
		_, text := readTrouble(err, "")
		add("status", "fail", "Durum dosyası okunamadı: %s", text)
		// R2 not reached, or not well: restic would wait minutes on it for the same.
		return !errors.As(err, &se) && !errors.Is(err, s3obj.ErrNetwork)
	}
	now := e.now()
	at := f.at
	if at.IsZero() {
		at = file.Time
	}
	running := 0
	for _, s := range file.Sites {
		if s.State == "running" || s.State == "paused" {
			running++
		}
	}
	parts := []string{"FootOps " + orDash(clip(clean(file.FootOps), 40)), fmt.Sprintf("%d / %d site çalışıyor", running, len(file.Sites))}
	if !file.Backup.LastOK.IsZero() {
		parts = append(parts, "son yedek "+clock(file.Backup.LastOK, now))
	} else {
		parts = append(parts, "henüz yedek yok")
	}
	text := "Durum dosyası okundu: " + strings.Join(parts, " · ") + " (" + ago(at, now) + " yazılmış)"
	who := clip(clean(file.Server), 64)
	switch {
	case who != "" && !strings.EqualFold(who, srv.Code):
		// The Mac's code is the Mac's own; a status file of another name is most likely
		// another server's bucket.
		add("status", "warn", "%s — dosya %q adlı sunucunun, bu Mac'teki kod %s: kova doğru mu?", text, who, srv.Code)
	case now.Sub(at) > max(statusSilent, 3*time.Duration(file.Every)*time.Second):
		add("status", "warn", "%s — sunucudan %s haber yok: sunucu ya da FootOps durmuş olabilir", text, spanTR(now.Sub(at)))
	default:
		add("status", "ok", "%s", text)
	}
	return true
}

// tryBackups opens the repository as yedek dene does, when its password is here.
func (e *env) tryBackups(ctx context.Context, srv server, worth bool, add func(part, state, format string, args ...any)) {
	switch {
	case !srv.Password:
		add("backups", "skip", "Yedek deposu şifresi Anahtar Zinciri'nde yok: yedekler bu Mac'te açılamaz (Ayarlar → şifre)")
		return
	case !worth:
		add("backups", "skip", "Yedekler denenmedi: kova okunamadı")
		return
	}
	_, n, err := e.countSnapshots(ctx, srv.Code)
	if errors.Is(err, errStopped) {
		add("backups", "skip", "Yedekler denenmedi: durduruldu")
		return
	}
	if err != nil {
		add("backups", "fail", "Yedek deposu açılamadı: %v", err)
		return
	}
	add("backups", "ok", "Yedek deposu açıldı (salt okunur): %d anlık görüntü", n)
}

func printTry(w io.Writer, t ServerTry) {
	fmt.Fprintf(w, "%s (%s) · kova %s\n", t.Name, t.Code, t.Bucket)
	marks := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "–"}
	for _, c := range t.Checks {
		fmt.Fprintf(w, "  %s %s\n", marks[c.State], c.Text)
	}
}

// R2Access is sunucu r2's answer: which items of the Mac's R2 access the Keychain has;
// the account masked (no secret, but no business of what shows the window either).
type R2Access struct {
	Account    string `json:"account,omitempty"`
	AccountSet bool   `json:"account_set"`
	KeySet     bool   `json:"key_set"`
	SecretSet  bool   `json:"secret_set"`
}

func (e *env) r2View(ctx context.Context) (R2Access, error) {
	names, err := e.keychain.Names(ctx)
	if err != nil {
		return R2Access{}, err
	}
	v := R2Access{AccountSet: slices.Contains(names, "r2-hesap"), KeySet: slices.Contains(names, "r2-mac-erisim"),
		SecretSet: slices.Contains(names, "r2-mac-gizli")}
	if v.AccountSet {
		a, err := e.keychain.Get(ctx, "r2-hesap")
		if err != nil {
			return R2Access{}, err
		}
		if a = strings.TrimSpace(a); reAccount.MatchString(a) {
			v.Account = a[:4] + "…" + a[len(a)-4:]
		} else {
			v.Account = "?"
		}
	}
	return v, nil
}

func (e *env) r2Cmd(ctx context.Context, jsonOut bool) error {
	v, err := e.r2View(ctx)
	if err != nil {
		return err
	}
	if jsonOut {
		e.event(v)
		return nil
	}
	printR2(e.out, v)
	return nil
}

func printR2(w io.Writer, v R2Access) {
	have := func(ok bool) string {
		if ok {
			return "var"
		}
		return "yok"
	}
	fmt.Fprintln(w, "R2 erişimi (Anahtar Zinciri, footops-yedek):")
	fmt.Fprintf(w, "  hesap kimliği    %s\n", orDash(v.Account))
	fmt.Fprintf(w, "  erişim anahtarı  %s\n", have(v.KeySet))
	fmt.Fprintf(w, "  gizli anahtar    %s\n", have(v.SecretSet))
}

// NewR2 is the Mac's R2 access to write (sunucu r2 yaz's input): a field left empty
// keeps what is there; the key and its secret go together.
type NewR2 struct {
	Account string `json:"account"`
	KeyID   string `json:"key_id"`
	Secret  string `json:"secret"`
}

func (e *env) writeR2(ctx context.Context, jsonOut bool) error {
	var n NewR2
	if !jsonOut && e.terminalIn() {
		a := e.asker()
		fmt.Fprintln(e.errw, "R2 erişimi — boş bırakılan alan olduğu gibi kalır.")
		var err error
		if n.Account, err = a.line("Hesap kimliği (Cloudflare → R2; 32 onaltılık rakam)"); err != nil {
			return err
		}
		if n.KeyID, err = a.line("Salt okunur anahtarın kimliği (Access Key ID)"); err != nil {
			return err
		}
		if n.Secret, err = a.secret("Gizli anahtar (Secret Access Key; görünmez)"); err != nil {
			return err
		}
	} else if err := e.readInput(&n); err != nil {
		return err
	}
	n.Account, n.KeyID, n.Secret = strings.ToLower(strings.TrimSpace(n.Account)), strings.TrimSpace(n.KeyID), strings.TrimSpace(n.Secret)
	switch {
	case n.Account != "" && !reAccount.MatchString(n.Account):
		return errors.New("R2 hesap kimliği 32 onaltılık rakam olmalı (Cloudflare → R2 → hesap kimliği)")
	case n.KeyID != "" && !reKeyID.MatchString(n.KeyID):
		return fmt.Errorf("anahtar kimliği beklenen biçimde değil (%d karakter; harf ve rakam)", len(n.KeyID))
	case n.Secret != "" && !reKeySecret.MatchString(n.Secret):
		return fmt.Errorf("gizli anahtar beklenen biçimde değil (%d karakter)", len(n.Secret))
	case (n.KeyID == "") != (n.Secret == ""):
		return errors.New("anahtar kimliği ve gizli anahtar birlikte verilir (ikisi aynı anahtarın)")
	case n == (NewR2{}):
		return errors.New("yazılacak bir şey yok")
	}
	cur, err := e.r2View(ctx)
	if err != nil {
		return err
	}
	if n.Account == "" && !cur.AccountSet {
		return errors.New("hesap kimliği gerekli (Anahtar Zinciri'nde yok)")
	}
	if n.KeyID == "" && !(cur.KeySet && cur.SecretSet) {
		return errors.New("salt okunur anahtar gerekli (Anahtar Zinciri'nde yok)")
	}
	// Written one at a time: should one fail, those written before it get back what
	// they had (a key with another key's secret would read nothing).
	type item struct{ name, value, was string }
	var items []item
	for _, it := range []item{{name: "r2-hesap", value: n.Account}, {name: "r2-mac-erisim", value: n.KeyID}, {name: "r2-mac-gizli", value: n.Secret}} {
		if it.value == "" {
			continue
		}
		v, err := e.keychain.Get(ctx, it.name)
		if err != nil && !errors.Is(err, errNoItem) {
			return err
		}
		it.was = v
		items = append(items, it)
	}
	for i, it := range items {
		if err := e.keychain.Set(ctx, it.name, it.value); err != nil {
			back := context.WithoutCancel(ctx)
			for _, done := range items[:i] {
				if done.was == "" {
					_ = e.keychain.Delete(back, done.name)
				} else {
					_ = e.keychain.Set(back, done.name, done.was)
				}
			}
			return err
		}
	}
	if !jsonOut {
		fmt.Fprintln(e.out, "✓ R2 erişimi Anahtar Zinciri'ne yazıldı — dene: footops-mac durum")
		return nil
	}
	return e.r2Cmd(ctx, true)
}

// readInput reads one JSON object of the app's from stdin into v; unknown fields are
// refused (a misspelt one would be lost silently).
func (e *env) readInput(v any) error {
	if e.in == nil {
		return errors.New("girdi yok")
	}
	data, err := io.ReadAll(io.LimitReader(e.in, settingsMax+1))
	if err != nil {
		return fmt.Errorf("girdi okunamadı: %v", err)
	}
	if len(data) > settingsMax {
		return errors.New("girdi çok büyük")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("girdi anlaşılamadı: %v", err)
	}
	if dec.More() {
		return errors.New("girdi anlaşılamadı: tek bir JSON nesnesi olmalı")
	}
	return nil
}

// terminalIn: the input is a terminal — the Terminal's user answers questions.
func (e *env) terminalIn() bool { return e.stdinTerminal != nil && e.stdinTerminal() }

// questions asks the Terminal's questions on stderr, an answer a line from stdin, a
// secret without echo.
type questions struct {
	e  *env
	in *bufio.Reader
}

func (e *env) asker() *questions { return &questions{e: e, in: bufio.NewReader(e.in)} }

func (q *questions) line(text string) (string, error) {
	fmt.Fprintf(q.e.errw, "  %s: ", text)
	s, err := q.in.ReadString('\n')
	if err != nil && (err != io.EOF || s == "") {
		return "", errors.New("yanıt okunamadı")
	}
	return strings.TrimSpace(s), nil
}

func (q *questions) secret(text string) (string, error) {
	fmt.Fprintf(q.e.errw, "  %s: ", text)
	s, err := q.e.readHidden()
	fmt.Fprintln(q.e.errw)
	if err != nil {
		return "", errors.New("yanıt okunamadı")
	}
	return strings.TrimSpace(s), nil
}
