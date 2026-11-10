package mac

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
)

// testPassword is a repository password as FootOps makes them: 40 letters and digits.
const testPassword = "Abcdefghij0123456789Klmnopqrst0123456789"

// serverEnv is testEnv's: two servers in the Keychain (a, h1 with a kova item), no
// restic.
func serverEnv(t *testing.T, r2 *fakeR2) (*env, *strings.Builder) {
	t.Helper()
	e, out, _ := testEnv(t, r2, testNow)
	e.look = func(string) (string, error) { return "", errors.New("yok") }
	var asked strings.Builder
	e.errw = &asked
	_ = out
	return e, &asked
}

func run(t *testing.T, e *env, in string, args ...string) (int, string) {
	t.Helper()
	out := e.out.(interface {
		Reset()
		String() string
	})
	out.Reset()
	e.in = strings.NewReader(in)
	code := e.run(context.Background(), args)
	return code, out.String()
}

func settingsOf(t *testing.T, out string) SettingsView {
	t.Helper()
	var v SettingsView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return v
}

func setting(v SettingsView, code string) (ServerSetting, bool) {
	for _, s := range v.Servers {
		if s.Code == code {
			return s, true
		}
	}
	return ServerSetting{}, false
}

// sunucu ekle as the Settings window adds one: the password into the Keychain only,
// the rest into the settings; what the app gets back is the list as it is now.
func TestServerAdd(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)

	code, out := run(t, e, `{"code":" H2 ","name":" Yedek – sunucu ","panel":"HTTPS://Panel-h2.example","ssh":"kisi@192.0.2.20",
		"bucket":"/footops-h2/","password":" `+testPassword+` "}`, "sunucu", "ekle", "--json")
	s, ok := setting(settingsOf(t, out), "h2")
	if code != 0 || !ok || s != (ServerSetting{Code: "h2", Name: "Yedek – sunucu", Panel: "https://Panel-h2.example", SSH: "kisi@192.0.2.20",
		Bucket: "footops-h2", DefaultBucket: "footops-h2", Keychain: true}) {
		t.Fatalf("added: %d %s", code, out)
	}
	if kc.items["restic-h2"] != testPassword {
		t.Errorf("Keychain: %q", kc.items["restic-h2"])
	}
	data, _ := os.ReadFile(e.settings)
	if strings.Contains(string(data), testPassword) || !strings.Contains(string(data), `"name": "Yedek – sunucu"`) {
		t.Errorf("settings file: %s", data)
	}

	// Its name everywhere the Mac shows it: the menu (before the status file's), the
	// backups' list.
	code, out = run(t, e, "", "durum", "--json")
	var st Status
	if json.Unmarshal([]byte(out), &st) != nil || len(st.Servers) != 3 || st.Servers[2].Code != "h2" || st.Servers[2].Name != "Yedek – sunucu" ||
		st.Servers[1].Name != "H1" {
		t.Errorf("durum: %d %s", code, out)
	}
	code, out = run(t, e, "", "yedek", "sunucular", "--json")
	if code != 0 || !strings.Contains(out, `{"code":"h2","name":"Yedek – sunucu","bucket":"footops-h2","password":true}`) ||
		!strings.Contains(out, `{"code":"h1","name":"H1","bucket":"footops-h1/kasa","password":true}`) {
		t.Errorf("yedek sunucular: %d %s", code, out)
	}

	// Only the password: known by its Keychain item, nothing in the file.
	if code, out = run(t, e, `{"code":"h3","password":"`+testPassword+`"}`, "sunucu", "ekle", "--json"); code != 0 {
		t.Fatalf("password only: %d %s", code, out)
	}
	if s, ok := setting(settingsOf(t, out), "h3"); !ok || !s.Keychain || s.Name != "" {
		t.Errorf("password only: %s", out)
	}
	if data, _ := os.ReadFile(e.settings); strings.Contains(string(data), `"h3"`) {
		t.Errorf("password only, file: %s", data)
	}

	// Refused: the file and the Keychain as they were.
	before, _ := os.ReadFile(e.settings)
	items := len(kc.items)
	for _, c := range []struct{ in, want string }{
		{`{"code":"h1","name":"İkinci"}`, "bu kodla bir sunucu zaten var: h1"},
		{`{"code":"H2","name":"Yine"}`, "zaten var: h2"},
		{`{"code":"-x","name":"Seçenek"}`, "sunucu kodu geçersiz"},
		{`{"code":"h 4","name":"Boşluk"}`, "sunucu kodu geçersiz"},
		{`{"code":"","name":"Kodsuz"}`, "sunucu kodu geçersiz"},
		{`{"code":"h4"}`, "en az bir bilgi gerekli"},
		{`{"code":"h4","name":"Satır\nkırık"}`, "ad geçersiz"},
		{`{"code":"h4","name":"Ters\u202eçevrik"}`, "ad geçersiz"},
		{`{"code":"h4","name":"İki\u2028satır"}`, "ad geçersiz"},
		{`{"code":"h4","name":"` + strings.Repeat("ç", nameMax+1) + `"}`, "ad geçersiz"},
		{`{"code":"h4","panel":"javascript:alert(1)"}`, "panel adresi geçersiz"},
		{`{"code":"h4","ssh":"-oProxyCommand=sh"}`, "SSH hedefi geçersiz"},
		{`{"code":"h4","bucket":"../kova"}`, "kova adı geçersiz"},
		{`{"code":"h4","password":"kısa"}`, "şifresi beklenen biçimde değil (4 karakter)"},
		{`{"code":"h4","password":"` + testPassword + `\nadd-generic-password"}`, "beklenen biçimde değil"},
		{`{"code":"h4","sifre":"x"}`, `unknown field "sifre"`},
		{`{"code":"h4"}{"code":"h5"}`, "tek bir JSON nesnesi"},
	} {
		code, out := run(t, e, c.in, "sunucu", "ekle", "--json")
		var ev map[string]string
		if code != 1 || json.Unmarshal([]byte(out), &ev) != nil || ev["event"] != "error" || !strings.Contains(ev["message"], c.want) {
			t.Errorf("%.60q: %d %s", c.in, code, out)
		}
		if strings.Contains(out, testPassword) {
			t.Errorf("%.60q: the password in the answer", c.in)
		}
	}
	if after, _ := os.ReadFile(e.settings); string(after) != string(before) || len(kc.items) != items {
		t.Errorf("changed by what was refused: %s %d", after, len(kc.items))
	}

	// The Keychain refusing: nothing written.
	kc.setErr = map[string]error{"restic-h5": errors.New("Anahtar Zinciri'ne yazılamadı: kilitli")}
	e.keychain = kc
	if code, out := run(t, e, `{"code":"h5","name":"Beş","password":"`+testPassword+`"}`, "sunucu", "ekle", "--json"); code != 1 ||
		!strings.Contains(out, "kilitli") {
		t.Errorf("keychain refused: %d %s", code, out)
	}
	if after, _ := os.ReadFile(e.settings); string(after) != string(before) {
		t.Errorf("keychain refused, file: %s", after)
	}

	// In the Terminal: asked, a line each, the password without echo.
	e2, asked := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	e2.stdinTerminal = func() bool { return true }
	e2.readHidden = func() (string, error) { return testPassword + "\n", nil }
	if code, out := run(t, e2, "h6\nAltıncı\n\nkisi@192.0.2.60\n\n", "sunucu", "ekle"); code != 0 ||
		!strings.Contains(out, "✓ Eklendi: Altıncı (h6) — dene: footops-mac sunucu dene h6") ||
		!strings.Contains(asked.String(), "Kod (sunucuya FootOps kurulurken verilen") || strings.Contains(asked.String(), testPassword) {
		t.Errorf("terminal: %d %s / %s", code, out, asked)
	}
	if e2.keychain.(fakeKeychain).items["restic-h6"] != testPassword {
		t.Errorf("terminal: password not written")
	}
}

// sunucu sifre writes a server's password; kaldir takes a server off this Mac: its
// settings and its Keychain items, nothing else.
func TestServerPasswordRemove(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"name":"Ana","panel":"https://panel.example"},"b":{"ssh":"kisi@sunucu.example"}}}`)

	if code, out := run(t, e, "  "+testPassword+"\n", "sunucu", "sifre", "H1", "--json"); code != 0 || kc.items["restic-h1"] != testPassword ||
		strings.Contains(out, testPassword) {
		t.Errorf("sifre: %d %s", code, out)
	}
	// A server only in the settings gets its first.
	code, out := run(t, e, testPassword, "sunucu", "sifre", "b", "--json")
	if s, ok := setting(settingsOf(t, out), "b"); code != 0 || !ok || !s.Keychain {
		t.Errorf("sifre, first: %d %s", code, out)
	}
	for _, c := range []struct {
		args []string
		in   string
		want string
	}{
		{[]string{"sifre", "h1"}, "kısa", "beklenen biçimde değil (4 karakter)"},
		{[]string{"sifre", "h1"}, "", "şifre boş"},
		{[]string{"sifre", "x9"}, testPassword, "bilinmeyen sunucu: x9"},
		{[]string{"kaldir", "x9"}, "", "bilinmeyen sunucu: x9"},
		{[]string{"kaldir", "../h1"}, "", "geçersiz sunucu kodu"},
	} {
		if code, out := run(t, e, c.in, append(append([]string{"sunucu"}, c.args...), "--json")...); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%v: %d %s", c.args, code, out)
		}
	}
	if kc.items["restic-h1"] != testPassword {
		t.Errorf("refused, yet written: %q", kc.items["restic-h1"])
	}
	// In the Terminal: without echo.
	e.stdinTerminal = func() bool { return true }
	e.readHidden = func() (string, error) { return "Zyxwvutsrq9876543210" + "Zyxwvutsrq9876543210", nil }
	if code, out := run(t, e, "", "sunucu", "sifre", "a"); code != 0 || !strings.Contains(out, "restic-a") ||
		kc.items["restic-a"] != "Zyxwvutsrq9876543210Zyxwvutsrq9876543210" {
		t.Errorf("terminal: %d %s", code, out)
	}
	e.stdinTerminal = nil

	// kaldir: h1's settings, password and kova item gone; a and b as they were.
	code, out = run(t, e, "", "sunucu", "kaldir", "h1", "--json")
	v := settingsOf(t, out)
	if _, ok := setting(v, "h1"); code != 0 || ok || len(v.Servers) != 2 {
		t.Errorf("kaldir: %d %s", code, out)
	}
	for _, n := range []string{"restic-h1", "kova-h1"} {
		if _, ok := kc.items[n]; ok {
			t.Errorf("kaldir: %s left", n)
		}
	}
	if kc.items["restic-a"] == "" || kc.items["r2-hesap"] == "" {
		t.Errorf("kaldir took too much: %v", kc.items)
	}
	data, _ := os.ReadFile(e.settings)
	if strings.Contains(string(data), `"h1"`) || !strings.Contains(string(data), `"b"`) {
		t.Errorf("kaldir, file: %s", data)
	}
	// In the Terminal, by its name.
	if code, out := run(t, e, "", "sunucu", "kaldir", "b"); code != 0 ||
		!strings.Contains(out, "✓ B (b) bu Mac'ten kaldırıldı") {
		t.Errorf("kaldir, text: %d %s", code, out)
	}
}

// sunucu dene: the status file as the Mac's key reads it, the repository when its
// password is here — a line each, with what to do when one fails.
func TestServerTry(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, _ := serverEnv(t, r2)
	file := goodFile(testNow)
	file.Server = "A"
	file.Sites = append(file.Sites, monitor.StatusSite{Name: "eski", State: "parked"})
	r2.put("/footops-a/izleme/durum.json", file, testNow.Add(-time.Minute))

	try := func(code string) (int, ServerTry, string) {
		t.Helper()
		c, out := run(t, e, "", "sunucu", "dene", code, "--json")
		var st ServerTry
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		var lines []string
		for _, ch := range st.Checks {
			lines = append(lines, ch.Part+" "+ch.State+": "+ch.Text)
		}
		return c, st, strings.Join(lines, "\n")
	}

	// The file read, its server the one asked; no restic here, so the backups fail.
	code, st, lines := try("a")
	if code != 1 || st.OK || st.Code != "a" || st.Name != "A" || st.Bucket != "footops-a" || len(st.Checks) != 2 ||
		!strings.Contains(lines, "status ok: Durum dosyası okundu: FootOps abc1234 · 2 / 3 site çalışıyor · son yedek 08:00 (1 dk önce yazılmış)") ||
		!strings.Contains(lines, "backups fail: Yedek deposu açılamadı: restic bulunamadı: brew install restic") {
		t.Errorf("a: %d %+v\n%s", code, st, lines)
	}

	// Another server's file in its bucket: most likely the wrong bucket.
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"name":"Ana","bucket":"footops-a"}}}`)
	if _, st, lines = try("h1"); st.Name != "Ana" || !strings.Contains(lines, `status warn: Durum dosyası okundu`) ||
		!strings.Contains(lines, `dosya "A" adlı sunucunun, bu Mac'teki kod h1: kova doğru mu?`) {
		t.Errorf("another's: %+v\n%s", st, lines)
	}
	writeSettings(t, e, `{"format":1}`)

	// Silent for long.
	file.Time = testNow.Add(-3 * time.Hour)
	r2.put("/footops-a/izleme/durum.json", file, testNow.Add(-3*time.Hour))
	if _, _, lines = try("a"); !strings.Contains(lines, "status warn:") || !strings.Contains(lines, "sunucudan 3 sa haber yok") {
		t.Errorf("silent: %s", lines)
	}
	file.Time = testNow.Add(-2 * time.Minute)
	r2.put("/footops-a/izleme/durum.json", file, testNow.Add(-time.Minute))

	// No file there: no backup set up, or the bucket wrong.
	if code, st, lines = try("h1"); code != 1 || st.OK ||
		!strings.Contains(lines, "status fail: Kovada durum dosyası yok (footops-h1/kasa/izleme/durum.json)") {
		t.Errorf("no file: %d %s", code, lines)
	}

	// The bucket refusing the Mac's key: Cloudflare's step, the backups not tried.
	r2.status = 403
	if code, st, lines = try("a"); code != 1 || st.OK ||
		!strings.Contains(lines, "status fail: Kova bu Mac'in salt okunur anahtarıyla okunamıyor (R2: 403 AccessDenied). Cloudflare → R2 → API token'ları → bu Mac'in salt okunur anahtarı → footops-a kovasını ekle.") ||
		!strings.Contains(lines, "backups skip: Yedekler denenmedi: kova okunamadı") {
		t.Errorf("403: %d %s", code, lines)
	}
	r2.status = 0

	// No password: the status still read, the backups not.
	kc := e.keychain.(fakeKeychain)
	delete(kc.items, "restic-a")
	writeSettings(t, e, `{"format":1,"servers":{"a":{"name":"Contabo"}}}`)
	if _, st, lines = try("a"); st.Name != "Contabo" ||
		!strings.Contains(lines, "backups skip: Yedek deposu şifresi Anahtar Zinciri'nde yok") {
		t.Errorf("no password: %s", lines)
	}

	// No R2 access: said, nothing else tried.
	delete(kc.items, "r2-mac-gizli")
	if code, st, lines = try("a"); code != 1 || len(st.Checks) != 1 ||
		lines != "access fail: Anahtar Zinciri'nde footops-yedek / r2-mac-gizli yok (salt okunur anahtarın gizlisi): Ayarlar → R2 erişimi" {
		t.Errorf("no access: %d %s", code, lines)
	}
	kc.items["r2-mac-gizli"] = testSecret

	// The Terminal's (a skipped part fails nothing), and an unknown server.
	if code, out := run(t, e, "", "sunucu", "dene", "a"); code != 0 || !strings.Contains(out, "Contabo (a) · kova footops-a\n  ✓ Durum dosyası okundu") ||
		!strings.Contains(out, "  – Yedek deposu şifresi") {
		t.Errorf("text: %d %s", code, out)
	}
	if code, out := run(t, e, "", "sunucu", "dene", "x9", "--json"); code != 1 || !strings.Contains(out, "bilinmeyen sunucu: x9") {
		t.Errorf("unknown: %d %s", code, out)
	}
}

// sunucu dene with restic: the repository opened as yedek dene opens it.
func TestServerTryRepository(t *testing.T) {
	f := newFixture(t)
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, _, _ := testEnv(t, r2, testNow)
	f.e.r2, f.e.client, f.e.now = e.r2, e.client, e.now
	file := goodFile(testNow)
	file.Server = "T1"
	r2.put("/footops-t1/izleme/durum.json", file, testNow.Add(-time.Minute))
	f.out.Reset()
	f.e.in = strings.NewReader("")
	code := f.e.run(context.Background(), []string{"sunucu", "dene", "t1", "--json"})
	var st ServerTry
	if code != 0 || json.Unmarshal(f.out.Bytes(), &st) != nil || !st.OK || len(st.Checks) != 2 ||
		st.Checks[1] != (TryCheck{Part: "backups", State: "ok", Text: "Yedek deposu açıldı (salt okunur): 6 anlık görüntü"}) {
		t.Errorf("%d %s %s", code, f.out, f.errb)
	}
}

// sunucu r2 shows the Mac's R2 access without its secrets; r2 yaz writes it.
func TestServerR2(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)
	code, out := run(t, e, "", "sunucu", "r2", "--json")
	if code != 0 || out != `{"account":"0123…cdef","account_set":true,"key_set":true,"secret_set":true}`+"\n" {
		t.Errorf("r2: %d %s", code, out)
	}
	const key, secret = "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if code, out := run(t, e, `{"key_id":" `+key+` ","secret":"`+secret+`"}`, "sunucu", "r2", "yaz", "--json"); code != 0 ||
		kc.items["r2-mac-erisim"] != key || kc.items["r2-mac-gizli"] != secret || kc.items["r2-hesap"] != testAccount ||
		strings.Contains(out, secret) {
		t.Errorf("yaz: %d %s %v", code, out, kc.items)
	}
	for _, c := range []struct{ in, want string }{
		{`{"account":"0123"}`, "32 onaltılık rakam"},
		{`{"key_id":"` + key + `"}`, "birlikte verilir"},
		{`{"secret":"` + secret + `"}`, "birlikte verilir"},
		{`{"key_id":"ab cd","secret":"` + secret + `"}`, "anahtar kimliği beklenen biçimde değil"},
		{`{"key_id":"` + key + `","secret":"x\ny"}`, "gizli anahtar beklenen biçimde değil (3 karakter)"},
		{`{}`, "yazılacak bir şey yok"},
		{`{"hesap":"x"}`, `unknown field \"hesap\"`},
	} {
		if code, out := run(t, e, c.in, "sunucu", "r2", "yaz", "--json"); code != 1 || !strings.Contains(out, c.want) {
			t.Errorf("%s: %d %s", c.in, code, out)
		}
	}
	// A Mac with none: all three needed.
	for _, n := range []string{"r2-hesap", "r2-mac-erisim", "r2-mac-gizli"} {
		delete(kc.items, n)
	}
	if code, out := run(t, e, "", "sunucu", "r2", "--json"); code != 0 || out != `{"account_set":false,"key_set":false,"secret_set":false}`+"\n" {
		t.Errorf("none: %d %s", code, out)
	}
	if code, out := run(t, e, `{"key_id":"`+key+`","secret":"`+secret+`"}`, "sunucu", "r2", "yaz", "--json"); code != 1 ||
		!strings.Contains(out, "hesap kimliği gerekli") {
		t.Errorf("none, partly: %d %s", code, out)
	}
	if code, out := run(t, e, `{"account":"`+strings.ToUpper(testAccount)+`","key_id":"`+key+`","secret":"`+secret+`"}`,
		"sunucu", "r2", "yaz"); code != 0 || kc.items["r2-hesap"] != testAccount || !strings.Contains(out, "✓ R2 erişimi") {
		t.Errorf("none, whole: %d %s", code, out)
	}
	if code, out := run(t, e, "", "sunucu", "r2"); code != 0 || !strings.Contains(out, "hesap kimliği    0123…cdef") {
		t.Errorf("text: %d %s", code, out)
	}
}

// The usage, and commands used wrongly.
func TestServerUsage(t *testing.T) {
	e, asked := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	for _, args := range [][]string{{"sunucu"}, {"sunucu", "sil", "a"}, {"sunucu", "dene"}, {"sunucu", "ekle", "a"}, {"sunucu", "r2", "sil"}} {
		asked.Reset()
		if code, _ := run(t, e, "", args...); code != 2 || !strings.Contains(asked.String(), "footops-mac sunucu —") {
			t.Errorf("%v: %d %s", args, code, asked)
		}
	}
	if code, out := run(t, e, "", "sunucu", "yardim"); code != 0 || !strings.Contains(out, "kaldir <kod>") {
		t.Errorf("yardim: %d %s", code, out)
	}
}

// What the Mac opened of a server's backups is named as the Mac calls the server now.
func TestOpenedServerName(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	e.opened = filepath.Join(t.TempDir(), "acilanlar.json")
	e.mongoURI = "mongodb://127.0.0.1:1"
	dir := filepath.Join(t.TempDir(), "magaza-20261109-0300")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := e.withOpened(func(l *openedList) bool {
		l.Items = append(l.Items, openedEntry{ID: "id1", Kind: "folder", Path: dir, Server: "h1", Site: "magaza", Time: testNow, At: testNow})
		return true
	}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, openedMark), []byte("id1"), 0o600)
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"name":"Ana sunucu"}}}`)
	v, err := e.openedView(context.Background())
	if err != nil || len(v.Folders) != 1 || v.Folders[0].ServerName != "Ana sunucu" {
		t.Errorf("%v %+v", err, v.Folders)
	}
}

// What would break security's command line is never sent: refused before security runs
// (the tool here is none at all).
func TestSecurityKeychainRefuses(t *testing.T) {
	k := securityKeychain{Tool: filepath.Join(t.TempDir(), "yok"), Service: "footops-yedek"}
	for _, c := range []struct{ name, secret string }{{"restic-t1", "iki kelime"}, {"restic-t1", "satır\nkırık"},
		{"restic-t1", `tırnak"`}, {"restic-t1", "tek'tırnak"}, {"restic-t1", `ters\bölü`}, {"restic-t1", "sekme\tile"},
		{"restic-t1", "#yorum"}, {"restic-t1", strings.Repeat("a", 257)}, {"restic-t1", "-w"}, {"restic-t1", ""},
		{"restic t1", testPassword}, {"-a", testPassword}} {
		if err := k.Set(context.Background(), c.name, c.secret); err == nil || !strings.Contains(err.Error(), "beklenmeyen bir karakter") {
			t.Errorf("%q %q: %v", c.name, c.secret, err)
		}
	}
}

// The Keychain itself (macOS only, and only when asked: it writes the login keychain):
// an item written by security is read back without a prompt, written again in place,
// deleted.
func TestSecurityKeychainReal(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("FOOTOPS_TEST_KEYCHAIN") == "" {
		t.Skip("FOOTOPS_TEST_KEYCHAIN=1 ile, macOS'ta")
	}
	ctx := context.Background()
	k := securityKeychain{Tool: "/usr/bin/security", Service: "footops-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	t.Cleanup(func() { k.Delete(context.Background(), "restic-t1") })
	if err := k.Set(ctx, "restic-t1", testPassword); err != nil {
		t.Fatal(err)
	}
	if v, err := k.Get(ctx, "restic-t1"); err != nil || v != testPassword {
		t.Fatalf("%q %v", v, err)
	}
	if err := k.Set(ctx, "restic-t1", "Ikinci0123456789"); err != nil {
		t.Fatal(err)
	}
	if names, err := k.Names(ctx); err != nil || len(names) != 1 || names[0] != "restic-t1" {
		t.Errorf("names: %v %v", names, err)
	}
	if out, err := exec.Command(k.Tool, "find-generic-password", "-s", k.Service, "-a", "restic-t1").CombinedOutput(); err != nil ||
		!strings.Contains(string(out), `="`+k.Service+` restic-t1"`) {
		t.Errorf("label: %v %s", err, out)
	}
	if err := k.Delete(ctx, "restic-t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(ctx, "restic-t1"); !errors.Is(err, errNoItem) {
		t.Errorf("deleted: %v", err)
	}
	if err := k.Delete(ctx, "restic-t1"); err != nil {
		t.Errorf("none there: %v", err)
	}
}

// The settings file not written: the password written before it is taken back (macOS:
// the folder made immutable for the owner too).
func TestServerAddRollback(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("chflags yalnız macOS'ta")
	}
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)
	writeSettings(t, e, `{"format":1}`)
	dir := filepath.Dir(e.settings)
	if err := exec.Command("chflags", "uchg", dir).Run(); err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { exec.Command("chflags", "nouchg", dir).Run() })
	if code, out := run(t, e, `{"code":"h2","name":"İki","password":"`+testPassword+`"}`, "sunucu", "ekle", "--json"); code != 1 ||
		!strings.Contains(out, "ayarlar yazılamadı") {
		t.Errorf("%d %s", code, out)
	}
	if _, ok := kc.items["restic-h2"]; ok {
		t.Error("the password left in the Keychain")
	}
}

// sifre tries the password first: one that does not open the repository leaves the
// one there; one that opens it, or one that cannot be tried (no repository reached),
// is written.
func TestServerPasswordTried(t *testing.T) {
	f := newFixture(t)
	kc := f.e.keychain.(fakeKeychain)
	// The fixture's keychain command prints restic-t1's only: the item tried comes from
	// a file of the test's.
	tried := filepath.Join(f.tmp, "tried")
	script := filepath.Join(f.tmp, "kc2.sh")
	body := "#!/bin/sh\ncase \"$1\" in\nrestic-t1) echo '" + fixturePassword + "' ;;\ndeneme-restic-t1) cat '" + tried + "' ;;\n*) exit 44 ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	kc.cmd = []string{script}
	f.e.keychain = kc
	try := func(pw string) (int, string) {
		t.Helper()
		os.WriteFile(tried, []byte(pw), 0o600)
		f.out.Reset()
		f.errb.Reset()
		f.e.in = strings.NewReader(pw)
		code := f.e.run(context.Background(), []string{"sunucu", "sifre", "t1"})
		return code, f.out.String() + f.errb.String()
	}
	if code, out := try("Yanlis0123456789Yanlis0123456789Yanlis01"); code != 1 || !strings.Contains(out, "bu şifre") ||
		!strings.Contains(out, "dokunulmadı") || kc.items["restic-t1"] != fixturePassword {
		t.Errorf("wrong: %d %s / %q", code, out, kc.items["restic-t1"])
	}
	if _, ok := kc.items["deneme-restic-t1"]; ok {
		t.Error("the item tried left behind")
	}
	if code, out := try(fixturePassword); code != 0 || !strings.Contains(out, "depoyu açtığı denendi") {
		t.Errorf("right: %d %s", code, out)
	}
	// No repository there: written, and said so.
	f.e.repository = func(access, string) string { return filepath.Join(f.tmp, "yok") }
	if code, out := try("Baska0123456789Baska0123456789Baska012345"); code != 0 || !strings.Contains(out, "denenemedi") ||
		kc.items["restic-t1"] != "Baska0123456789Baska0123456789Baska012345" {
		t.Errorf("not reached: %d %s", code, out)
	}
}

// r2 yaz writing one item and failing at the next gives the first back what it had.
func TestServerR2Rollback(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)
	kc.setErr = map[string]error{"r2-mac-gizli": errors.New("Anahtar Zinciri'ne yazılamadı: kilitli")}
	e.keychain = kc
	const key, secret = "fedcba9876543210fedcba9876543210", "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	if code, out := run(t, e, `{"key_id":"`+key+`","secret":"`+secret+`"}`, "sunucu", "r2", "yaz", "--json"); code != 1 ||
		!strings.Contains(out, "kilitli") {
		t.Errorf("%d %s", code, out)
	}
	if kc.items["r2-mac-erisim"] != testKeyID || kc.items["r2-mac-gizli"] != testSecret {
		t.Errorf("not given back: %q %q", kc.items["r2-mac-erisim"], kc.items["r2-mac-gizli"])
	}
	// One there was none of goes again.
	delete(kc.items, "r2-hesap")
	kc.setErr = map[string]error{"r2-mac-erisim": errors.New("kilitli")}
	e.keychain = kc
	if code, _ := run(t, e, `{"account":"`+testAccount+`","key_id":"`+key+`","secret":"`+secret+`"}`, "sunucu", "r2", "yaz", "--json"); code != 1 {
		t.Errorf("second: %d", code)
	}
	if _, ok := kc.items["r2-hesap"]; ok {
		t.Error("r2-hesap written, though the rest was not")
	}
}

// dene tells the key refused from the bucket out of its reach.
func TestServerTryKeyRefused(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, _ := serverEnv(t, r2)
	kc := e.keychain.(fakeKeychain)
	kc.items["r2-mac-gizli"] = "Baska/Gizli+Anahtar="
	code, out := run(t, e, "", "sunucu", "dene", "a", "--json")
	if code != 1 || !strings.Contains(out, "R2 bu Mac'in anahtarını tanımadı (SignatureDoesNotMatch)") || !strings.Contains(out, "R2 erişimi") ||
		strings.Contains(out, "kovasını ekle") {
		t.Errorf("%d %s", code, out)
	}
}

// kaldir cut short at the password: the server still listed, its bucket item gone.
func TestServerRemoveCutShort(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	kc := e.keychain.(fakeKeychain)
	kc.delErr = map[string]error{"restic-h1": errors.New("Anahtar Zinciri'nden silinemedi: kilitli")}
	e.keychain = kc
	if code, out := run(t, e, "", "sunucu", "kaldir", "h1", "--json"); code != 1 || !strings.Contains(out, "kilitli") {
		t.Errorf("%d %s", code, out)
	}
	if _, ok := kc.items["kova-h1"]; ok {
		t.Error("kova-h1 left")
	}
	code, out := run(t, e, "", "ayar", "--json")
	if _, ok := setting(settingsOf(t, out), "h1"); code != 0 || !ok {
		t.Errorf("h1 gone from the list: %s", out)
	}
}

// ayar yaz with the revision it read writes over that file only.
func TestSettingsRevision(t *testing.T) {
	e, _ := serverEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}})
	_, out := run(t, e, "", "ayar", "--json")
	if rev := settingsOf(t, out).Revision; rev != "yok" {
		t.Fatalf("no file: %q", rev)
	}
	code, out := run(t, e, `{"format":1,"revision":"yok","servers":{"h1":{"name":"Ana"}}}`, "ayar", "yaz", "--json")
	rev := settingsOf(t, out).Revision
	if code != 0 || len(rev) != 16 {
		t.Fatalf("written: %d %s", code, out)
	}
	// Changed elsewhere since (a server added from the Terminal).
	if code, out := run(t, e, `{"code":"h2","name":"Terminalden"}`, "sunucu", "ekle", "--json"); code != 0 {
		t.Fatalf("ekle: %d %s", code, out)
	}
	before, _ := os.ReadFile(e.settings)
	code, out = run(t, e, `{"format":1,"revision":"`+rev+`","servers":{"h1":{"name":"Eski pencere"}}}`, "ayar", "yaz", "--json")
	var ev map[string]string
	if code != 1 || json.Unmarshal([]byte(out), &ev) != nil || ev["code"] != "settings_changed" {
		t.Errorf("stale: %d %s", code, out)
	}
	if after, _ := os.ReadFile(e.settings); string(after) != string(before) {
		t.Errorf("stale, yet written: %s", after)
	}
	// Without one, as before.
	if code, out := run(t, e, `{"format":1,"servers":{"h1":{"name":"Zorla"}}}`, "ayar", "yaz", "--json"); code != 0 {
		t.Errorf("no revision: %d %s", code, out)
	}
}
