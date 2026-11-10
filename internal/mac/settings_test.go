package mac

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// footops-mac ayar as the Settings window uses it: what is known, written, read back by
// durum; the file 0600 in a folder 0700.
func TestSettingsCommand(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	ctx := context.Background()
	e, out, errb := testEnv(t, r2, testNow)
	e.settings = filepath.Join(t.TempDir(), "FootOps", "ayarlar.json")

	// No file yet: the Keychain's servers, h1's bucket from its kova item.
	if code := e.run(ctx, []string{"ayar", "--json"}); code != 0 {
		t.Fatalf("no file: %d %s %s", code, out, errb)
	}
	var v SettingsView
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v.Path != e.settings || v.Error != "" || len(v.Servers) != 2 ||
		v.Servers[0] != (ServerSetting{Code: "a", DefaultBucket: "footops-a", Keychain: true}) ||
		v.Servers[1] != (ServerSetting{Code: "h1", DefaultBucket: "footops-h1/kasa", Keychain: true}) {
		t.Fatalf("no file: %v %s", err, out)
	}

	// Written: trimmed, a server with nothing set left out, one only in the file kept.
	e.in = strings.NewReader(`{"format":1,"servers":{
		"h1":{"panel":" https://panel.example ","ssh":"kisi@sunucu.example","bucket":"/footops-h1/kasa/"},
		"a":{"panel":"","ssh":"","bucket":""},
		"b":{"ssh":"kisi@192.0.2.10:2222","panel":"http://localhost:7701"}}}`)
	out.Reset()
	if code := e.run(ctx, []string{"ayar", "yaz", "--json"}); code != 0 {
		t.Fatalf("write: %d %s %s", code, out, errb)
	}
	v = SettingsView{}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || len(v.Servers) != 3 ||
		v.Servers[0] != (ServerSetting{Code: "a", DefaultBucket: "footops-a", Keychain: true}) ||
		v.Servers[1] != (ServerSetting{Code: "b", Panel: "http://localhost:7701", SSH: "kisi@192.0.2.10:2222", DefaultBucket: "footops-b"}) ||
		v.Servers[2] != (ServerSetting{Code: "h1", Panel: "https://panel.example", SSH: "kisi@sunucu.example", Bucket: "footops-h1/kasa",
			DefaultBucket: "footops-h1/kasa", Keychain: true}) {
		t.Fatalf("written: %v %s", err, out)
	}
	data, err := os.ReadFile(e.settings)
	if err != nil {
		t.Fatal(err)
	}
	var set Settings
	if err := json.Unmarshal(data, &set); err != nil || set.Format != 1 || len(set.Servers) != 2 || set.Servers["a"] != (ServerSettings{}) {
		t.Errorf("file: %v %s", err, data)
	}
	if fi, err := os.Stat(e.settings); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode: %v %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Dir(e.settings)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder mode: %v %v", fi.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(e.settings), ".ayarlar-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}

	// durum reads it: h1's panel and SSH in its answer.
	out.Reset()
	e.run(ctx, []string{"durum", "--json"})
	var st Status
	if json.Unmarshal(out.Bytes(), &st) != nil || len(st.Servers) != 3 || st.Servers[2].Panel != "https://panel.example" ||
		st.Servers[2].SSH != "kisi@sunucu.example" || st.Servers[1].Bucket != "footops-b" {
		t.Errorf("durum after: %s", out)
	}

	// The Terminal's view.
	out.Reset()
	if code := e.run(ctx, []string{"ayar"}); code != 0 || !strings.Contains(out.String(), e.settings) ||
		!strings.Contains(out.String(), "footops-a (varsayılan)") || !strings.Contains(out.String(), "b (şifre yok)") ||
		!strings.Contains(out.String(), "kisi@sunucu.example") {
		t.Errorf("text: %d %s", code, out)
	}
	errb.Reset()
	if code := e.run(ctx, []string{"ayar", "sil"}); code != 2 || !strings.Contains(errb.String(), "Kullanım:") {
		t.Errorf("usage: %d %s", code, errb)
	}
	e.in = strings.NewReader(`{"format":1}`)
	out.Reset()
	if code := e.run(ctx, []string{"ayar", "yaz"}); code != 0 || !strings.Contains(out.String(), "✓ Ayarlar yazıldı") {
		t.Errorf("write, text: %d %s", code, out)
	}
	if data, _ := os.ReadFile(e.settings); strings.TrimSpace(string(data)) != `{
  "format": 1
}` {
		t.Errorf("emptied: %s", data)
	}
}

// What ayar yaz refuses — said in one error event, the file as it was.
func TestSettingsWriteRefused(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	ctx := context.Background()
	e, out, _ := testEnv(t, r2, testNow)
	const kept = `{"format":1,"servers":{"h1":{"panel":"https://panel.example"}}}`
	writeSettings(t, e, kept)
	for _, c := range []struct{ in, want string }{
		{`{"format":1,"servers":{"h1":{"panle":"https://panel.example"}}}`, `unknown field "panle"`},
		{`{"format":2}`, "biçimi 1 olmalı"},
		{`{"format":1}{"format":1}`, "tek bir JSON nesnesi"},
		{`{"format":1,"servers":{"H1":{"ssh":"kisi@sunucu.example"}}}`, "geçersiz sunucu kodu"},
		{`{"format":1,"servers":{"h1":{"bucket":"../kova"}}}`, "kova adı geçersiz"},
		{`{"format":1,"servers":{"h1":{"bucket":"kova/a/b/c/d"}}}`, "en çok üç klasör"},
		{`{"format":1,"servers":{"h1":{"panel":"javascript:alert(1)"}}}`, "panel adresi geçersiz"},
		{`{"format":1,"servers":{"h1":{"panel":"file:///etc/passwd"}}}`, "panel adresi geçersiz"},
		{`{"format":1,"servers":{"h1":{"panel":"https://kisi:sifre@panel.example"}}}`, "panel adresi geçersiz"},
		{`{"format":1,"servers":{"h1":{"panel":"https://panel.example/a b"}}}`, "panel adresi geçersiz"},
		{`{"format":1,"servers":{"h1":{"panel":"panel.example"}}}`, "panel adresi geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"-oProxyCommand=sh"}}}`, "SSH hedefi geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"kisi@-oProxyCommand=sh"}}}`, "SSH hedefi geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"kisi @sunucu.example"}}}`, "SSH hedefi geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"ssh://kisi@sunucu.example"}}}`, "SSH hedefi geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"kisi@sunucu.example:70000"}}}`, "portu geçersiz"},
		{`{"format":1,"servers":{"h1":{"ssh":"` + strings.Repeat("a", settingsMax) + `"}}}`, "çok büyük"},
		{`{`, "anlaşılamadı"},
	} {
		e.in = strings.NewReader(c.in)
		out.Reset()
		code := e.run(ctx, []string{"ayar", "yaz", "--json"})
		var ev map[string]string
		if code != 1 || json.Unmarshal(out.Bytes(), &ev) != nil || ev["event"] != "error" || !strings.Contains(ev["message"], c.want) {
			t.Errorf("%.80s: %d %s", c.in, code, out)
		}
		if data, _ := os.ReadFile(e.settings); string(data) != kept {
			t.Fatalf("%.80s: the file changed: %s", c.in, data)
		}
	}
	// A file written by hand: a panel or SSH target not to the rules is left out of what
	// the app opens — the servers still read — and named where the settings are shown,
	// as written, to be put right.
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"ssh":"-oProxyCommand=sh","panel":"https://panel.example"}}}`)
	out.Reset()
	var st Status
	if code := e.run(ctx, []string{"durum", "--json"}); code != 0 || json.Unmarshal(out.Bytes(), &st) != nil || len(st.Servers) != 2 ||
		st.Servers[1].SSH != "" || st.Servers[1].Panel != "https://panel.example" {
		t.Errorf("durum, bad SSH: %d %s", code, out)
	}
	out.Reset()
	var v SettingsView
	if code := e.run(ctx, []string{"ayar", "--json"}); code != 1 || json.Unmarshal(out.Bytes(), &v) != nil ||
		!strings.Contains(v.Error, "ayarlar dosyası: h1: SSH hedefi geçersiz") || v.Servers[1].SSH != "-oProxyCommand=sh" {
		t.Errorf("ayar, bad SSH: %d %s", code, out)
	}
	// A bucket not to the rules still stops it: it would read the wrong place.
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"bucket":"Kova"}}}`)
	out.Reset()
	if code := e.run(ctx, []string{"durum", "--json"}); code != 1 || !strings.Contains(out.String(), "kova adı geçersiz") {
		t.Errorf("durum, bad bucket: %d %s", code, out)
	}
}

// A panel's scheme is written as the app reads it: HTTPS:// is https://.
func TestSettingsWriteScheme(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, out, _ := testEnv(t, r2, testNow)
	e.in = strings.NewReader(`{"format":1,"servers":{"h1":{"panel":"HTTPS://Panel.Example:7701/giris"}}}`)
	if code := e.run(context.Background(), []string{"ayar", "yaz", "--json"}); code != 0 {
		t.Fatalf("write: %d %s", code, out)
	}
	var v SettingsView
	if json.Unmarshal(out.Bytes(), &v) != nil || v.Servers[1].Panel != "https://Panel.Example:7701/giris" {
		t.Errorf("scheme: %s", out)
	}
}

// What can be read is listed though something cannot: the file not understood (the
// Keychain's servers, to write it anew), the Keychain locked (the file's).
func TestSettingsViewTrouble(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	ctx := context.Background()
	e, out, _ := testEnv(t, r2, testNow)
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"panel":"https://panel.example"},"x":{"bucket":"../kova"}}}`)
	var v SettingsView
	if code := e.run(ctx, []string{"ayar", "--json"}); code != 1 || json.Unmarshal(out.Bytes(), &v) != nil ||
		!strings.Contains(v.Error, "kova adı geçersiz") || len(v.Servers) != 2 || v.Servers[1].Panel != "" {
		t.Errorf("bad file: %d %s", code, out)
	}
	// Written anew over it.
	e.in = strings.NewReader(`{"format":1,"servers":{"h1":{"panel":"https://panel.example"}}}`)
	out.Reset()
	v = SettingsView{}
	if code := e.run(ctx, []string{"ayar", "yaz", "--json"}); code != 0 || json.Unmarshal(out.Bytes(), &v) != nil || v.Error != "" ||
		v.Servers[1].Panel != "https://panel.example" {
		t.Errorf("written over: %d %s", code, out)
	}

	writeSettings(t, e, `{"format":1,"servers":{"b":{"ssh":"kisi@sunucu.example"}}}`)
	e.keychain = fakeKeychain{err: errors.New("Anahtar Zinciri okunamadı: kilitli")}
	out.Reset()
	v = SettingsView{}
	if code := e.run(ctx, []string{"ayar", "--json"}); code != 1 || json.Unmarshal(out.Bytes(), &v) != nil ||
		!strings.Contains(v.Error, "kilitli") || len(v.Servers) != 1 ||
		v.Servers[0] != (ServerSetting{Code: "b", SSH: "kisi@sunucu.example", DefaultBucket: "footops-b"}) {
		t.Errorf("keychain locked: %d %s", code, out)
	}
}
