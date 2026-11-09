package mac

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swbilisim/FootOps/internal/monitor"
	"github.com/swbilisim/FootOps/internal/s3obj"
)

func goodFile(now time.Time) monitor.StatusFile {
	return monitor.StatusFile{Format: 1, Server: "H1", FootOps: "abc1234", Time: now.Add(-2 * time.Minute), Every: 300,
		Sites: []monitor.StatusSite{
			{Name: "magaza", Domains: []string{"magaza.example", "www.magaza.example"}, State: "running"},
			{Name: "blog", Domains: []string{"blog.example"}, State: "running"},
		},
		Backup: monitor.StatusBackup{Ready: true, LastOK: now.Add(-time.Hour), NightDone: "2026-11-09"},
		Sys: &monitor.StatusSys{Time: now.Add(-2 * time.Minute), CPUPercent: 3.2, Load1: 0.1, MemUsed: 2 << 30, MemTotal: 4 << 30,
			DiskUsed: 10 << 30, DiskTotal: 40 << 30, Uptime: 3600}}
}

func goodBeat(now time.Time) monitor.WatcherBeat {
	return monitor.WatcherBeat{Format: 1, Time: now.Add(-time.Minute), Every: 300, Visits: 120, Domains: []monitor.WatcherDomain{
		{Name: "blog.example", Site: "blog", OK: true}, {Name: "magaza.example", Site: "magaza", OK: true},
		{Name: "www.magaza.example", Site: "magaza", OK: true, Note: "cloudflare-challenge"}}}
}

func asFetched(v any, at time.Time) fetched {
	data, _ := json.Marshal(v)
	return fetched{data: data, at: at}
}

func texts(s ServerStatus) string {
	var out []string
	for _, p := range s.Problems {
		out = append(out, p.Color+": "+p.Text)
	}
	return strings.Join(out, "\n")
}

// A status file is a server's word: its names reach the menu clean of control and
// format characters (U+202E turns a name around) and no longer than a line.
func TestJudgeCleansNames(t *testing.T) {
	now := testNow
	f := goodFile(now)
	f.Server = "H1\u202ekot"
	f.FootOps = "abc1234\x1b[31m"
	f.Sites[0].Name = strings.Repeat("uzun", 30)
	f.Sites[1].Domains = []string{"blog.example\u200f"}
	s := judge(server{Code: "h1", Bucket: "footops-h1"}, asFetched(f, now.Add(-time.Minute)), asFetched(goodBeat(now), now), now)
	if s.Name != "H1?kot" || s.File.FootOps != "abc1234?[31m" || len(s.Sites[0].Name) != 64+len("…") ||
		s.Sites[1].Domains[0].Name != "blog.example?" {
		t.Errorf("%q %q %q %q", s.Name, s.File.FootOps, s.Sites[0].Name, s.Sites[1].Domains[0].Name)
	}
}

// What a server's colour comes of (panel-tasarim §29.3): each rule alone, from a server
// where all is well.
func TestJudge(t *testing.T) {
	now := testNow
	srv := server{Code: "h1", Bucket: "footops-h1"}
	type change struct {
		file  func(*monitor.StatusFile)
		beat  func(*monitor.WatcherBeat)
		sf    *fetched // instead of the file
		wf    *fetched
		color string
		want  []string // in the problems' texts, in order
	}
	refused := fetched{err: &s3obj.Error{Status: 403, Code: "AccessDenied"}}
	unreachable := fetched{err: netErr{errors.New("depoya ulaşılamadı: dial tcp: lookup <hesap>.r2.cloudflarestorage.com: no such host")}}
	missing := fetched{err: s3obj.ErrNoObject}
	garbage := fetched{data: []byte(`{"format":0}`)}
	for name, c := range map[string]change{
		"all well": {color: green},
		"server quiet": {file: func(f *monitor.StatusFile) { f.Time = now.Add(-16 * time.Minute) }, color: red,
			want: []string{"Sunucudan 16 dk haber yok (son durum dosyası 11:44)"}},
		// R2's own clock decides when it says (the server's may be off).
		"server clock off": {sf: ptr(asFetched(func() monitor.StatusFile {
			f := goodFile(now)
			f.Time = now.Add(-time.Hour)
			return f
		}(), now.Add(-time.Minute))), color: green},
		"long interval":  {file: func(f *monitor.StatusFile) { f.Time, f.Every = now.Add(-20*time.Minute), 600 }, color: green},
		"no status file": {sf: &missing, color: red, want: []string{"Durum dosyası yok (footops-h1/izleme/durum.json)"}},
		"status garbage": {sf: &garbage, color: red, want: []string{"Durum dosyası okunamadı: biçimi tanınmadı"}},
		"key refused": {sf: &refused, wf: &refused, color: gray,
			want: []string{"Durum dosyası okunamadı: R2 bu Mac'in anahtarını reddetti"}},
		"r2 unreachable": {sf: &unreachable, wf: &unreachable, color: gray,
			want: []string{"Durum dosyası okunamadı: R2'ye ulaşılamadı: dial tcp"}},
		"missing container": {file: func(f *monitor.StatusFile) { f.Sites[1].State = "missing" }, color: red,
			want: []string{"Container'ı yok: blog"}},
		"unhealthy": {file: func(f *monitor.StatusFile) { f.Sites[0].State, f.Sites[1].State = "unhealthy", "restarting" }, color: yellow,
			want: []string{"Sağlıksız: magaza", "Yeniden başlıyor: blog"}},
		"off on purpose": {file: func(f *monitor.StatusFile) { f.Sites[0].State, f.Sites[1].State = "parked", "paused" }, color: green},
		// Stopped is not on purpose (parking is): the agent says it of a crashed container.
		"stopped": {file: func(f *monitor.StatusFile) { f.Sites[1].State = "stopped" }, color: red,
			want: []string{"Çalışmıyor (container durmuş): blog"}},
		"backup problems": {file: func(f *monitor.StatusFile) { f.Backup.Problems = []string{"Yedek gecikti"} }, color: yellow,
			want: []string{"Yedek: Yedek gecikti"}},
		"disk full": {file: func(f *monitor.StatusFile) { f.Sys.DiskUsed = 34 << 30 }, color: yellow,
			want: []string{"Disk %85 dolu (34,0 GB / 40,0 GB)"}},
		"memory full": {file: func(f *monitor.StatusFile) { f.Sys.MemUsed = 3800 << 20 }, color: yellow,
			want: []string{"Bellek %93 dolu"}},
		"older footops": {file: func(f *monitor.StatusFile) { f.Sys = nil }, color: green},
		"site list unreadable": {file: func(f *monitor.StatusFile) { f.Sites, f.SitesError = nil, "footops.json okunamadı" }, color: yellow,
			want: []string{"Site listesi okunamadı: footops.json okunamadı"}},
		"domain down": {beat: func(b *monitor.WatcherBeat) {
			b.Domains[0] = monitor.WatcherDomain{Name: "blog.example", Site: "blog", Down: true, Code: "502",
				Reason: "502 — site yanıt vermiyor", Since: now.Add(-20 * time.Minute)}
		}, color: red, want: []string{"Nöbetçiye göre açılmıyor: blog.example (502 — site yanıt vermiyor, 11:40'dan beri)"}},
		"domain failing": {beat: func(b *monitor.WatcherBeat) {
			b.Domains[1] = monitor.WatcherDomain{Name: "magaza.example", Site: "magaza", Code: "timeout", Reason: "zaman aşımı"}
		}, color: yellow, want: []string{"Nöbetçi son yoklamada ulaşamadı, henüz kesinti sayılmadı: magaza.example (zaman aşımı)"}},
		// An old heartbeat's word is not gone by: the watcher's silence is the news.
		"watcher quiet": {beat: func(b *monitor.WatcherBeat) {
			b.Time = now.Add(-31 * time.Minute)
			b.Domains[0].OK, b.Domains[0].Down = false, true
		}, color: yellow, want: []string{"Dış nöbetçiden 31 dk haber yok"}},
		// The watcher's messages refused a while (15 min, the agent's), not for a moment;
		// its state not saved is the agent's alert to give.
		"watcher trouble": {beat: func(b *monitor.WatcherBeat) {
			b.SendError, b.SendFailing = "Telegram reddetti (HTTP 403)", now.Add(-20*time.Minute)
		}, color: yellow, want: []string{"Dış nöbetçi 20 dk Telegram'a mesaj gönderemiyor (Telegram reddetti (HTTP 403))"}},
		"watcher trouble for a moment": {beat: func(b *monitor.WatcherBeat) {
			b.SendError, b.SendFailing = "Telegram reddetti (HTTP 502)", now.Add(-4*time.Minute)
		}, color: green},
		"watcher state":   {beat: func(b *monitor.WatcherBeat) { b.StateError = "KV yazılamadı" }, color: green},
		"no watcher":      {wf: &missing, color: yellow, want: []string{"Dış nöbetçinin dosyası yok"}},
		"watcher garbage": {wf: &garbage, color: yellow, want: []string{"Dış nöbetçinin dosyası okunamadı: biçimi tanınmadı"}},
		// A quiet server's file says nothing of now: its sites and backups are not judged.
		"quiet and down": {file: func(f *monitor.StatusFile) {
			f.Time = now.Add(-40 * time.Minute)
			f.Sites[1].State, f.Backup.Problems = "missing", []string{"Yedek gecikti"}
		}, beat: func(b *monitor.WatcherBeat) {
			b.Domains[0] = monitor.WatcherDomain{Name: "blog.example", Site: "blog", Down: true, Code: "522", Reason: "522 — sunucuya bağlanılamadı"}
		}, color: red, want: []string{"Sunucudan 40 dk haber yok", "Nöbetçiye göre açılmıyor: blog.example (522"}},
	} {
		f, b := goodFile(now), goodBeat(now)
		if c.file != nil {
			c.file(&f)
		}
		if c.beat != nil {
			c.beat(&b)
		}
		sf, wf := asFetched(f, time.Time{}), asFetched(b, time.Time{})
		if c.sf != nil {
			sf = *c.sf
		}
		if c.wf != nil {
			wf = *c.wf
		}
		got := judge(srv, sf, wf, now.In(time.FixedZone("TR", 3*3600)))
		all := texts(got)
		if got.Color != c.color || len(got.Problems) != len(c.want) {
			t.Errorf("%s: %s\n%s", name, got.Color, all)
			continue
		}
		for i, w := range c.want {
			if !strings.Contains(got.Problems[i].Text, w) {
				t.Errorf("%s: problem %d %q, want %q", name, i, got.Problems[i].Text, w)
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }

// netErr is an error of the way to R2, as s3obj marks one.
type netErr struct{ error }

func (netErr) Is(target error) bool { return target == s3obj.ErrNetwork }

// The sites one by one: their colour, their domains as the watcher sees them, the count.
func TestJudgeSites(t *testing.T) {
	now := testNow
	f, b := goodFile(now), goodBeat(now)
	f.Sites = append(f.Sites, monitor.StatusSite{Name: "eski", Domains: []string{"eski.example"}, State: "parked"},
		monitor.StatusSite{Name: "yeni\x1b[31m", Domains: []string{"yeni.example"}, State: "running"})
	b.Domains[0] = monitor.WatcherDomain{Name: "blog.example", Site: "blog", Code: "502", Reason: "502 — site\nyanıt vermiyor"}
	got := judge(server{Code: "h1", Bucket: "footops-h1"}, asFetched(f, now.Add(-time.Minute)), asFetched(b, time.Time{}), now)
	if got.Name != "H1" || got.Running != 3 || got.Total != 4 || got.Color != yellow || !got.FileAt.Equal(now.Add(-time.Minute).Truncate(time.Second)) {
		t.Fatalf("server: %+v\n%s", got, texts(got))
	}
	want := map[string]string{"magaza": green, "blog": yellow, "eski": gray, "yeni?[31m": green}
	for _, s := range got.Sites {
		if want[s.Name] != s.Color {
			t.Errorf("site %s: %s", s.Name, s.Color)
		}
	}
	magaza := got.Sites[0].Domains
	if len(magaza) != 2 || magaza[0].Watcher != "ok" || magaza[1].Watcher != "blind" {
		t.Errorf("magaza: %+v", magaza)
	}
	if d := got.Sites[1].Domains[0]; d.Watcher != "failing" || d.Code != "502" || strings.ContainsAny(d.Reason, "\n\x1b") {
		t.Errorf("blog: %+v", d)
	}
	if d := got.Sites[3].Domains[0]; d.Watcher != "unknown" {
		t.Errorf("yeni: %+v", d)
	}
	if got.Sites[2].Label != "parkta" {
		t.Errorf("label: %+v", got.Sites[2])
	}
}

// durum end to end: the Keychain's servers (a folder below h1's bucket), the settings'
// panel address, four GETs signed with the Mac's key, the text and the JSON; no secret
// or account anywhere in what is said.
func TestStatusCommand(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	e, out, errb := testEnv(t, r2, testNow)
	writeSettings(t, e, `{"format":1,"servers":{"h1":{"panel":"https://panel.example","ssh":"kisi@sunucu.example"}}}`)
	r2.put("/footops-h1/kasa/izleme/durum.json", goodFile(testNow), testNow.Add(-2*time.Minute))
	r2.put("/footops-h1/kasa/izleme/nobetci.json", goodBeat(testNow), testNow.Add(-time.Minute))
	fa := goodFile(testNow)
	fa.Server, fa.Sites = "A", fa.Sites[:1]
	fa.Sites[0].State = "unhealthy"
	r2.put("/footops-a/izleme/durum.json", fa, testNow.Add(-3*time.Minute))
	ctx := context.Background()

	if code := e.run(ctx, []string{"durum"}); code != 0 {
		t.Fatalf("durum: %d\n%s%s", code, out, errb)
	}
	text := out.String()
	for _, w := range []string{"FootOps · 09.11.2026", "🟡 bakılmalı", "🟢 H1 · 2 / 2 site çalışıyor · son yedek", "FootOps abc1234",
		"İşlemci %3 · yük 0,10 / 0,00 / 0,00 · bellek 2,0 GB / 4,0 GB (%50) · disk 10,0 GB / 40,0 GB (%25) · açık 1 sa",
		"Durum dosyası 2 dk önce · nöbetçi 1 dk önce: 3 / 3 alan adı açık",
		"🟡 A · 0 / 1 site çalışıyor", "🟡 Sağlıksız: magaza", "🟡 Dış nöbetçinin dosyası yok"} {
		if !strings.Contains(text, w) {
			t.Errorf("text lacks %q:\n%s", w, text)
		}
	}
	if len(r2.gets) != 4 {
		t.Errorf("GETs: %v", r2.gets)
	}

	out.Reset()
	if code := e.run(ctx, []string{"durum", "--json"}); code != 0 {
		t.Fatalf("durum --json: %d %s", code, errb)
	}
	var st Status
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || strings.Count(out.String(), "\n") != 1 {
		t.Fatalf("one JSON object: %v\n%s", err, out)
	}
	if st.Color != yellow || len(st.Servers) != 2 || st.Servers[0].Code != "a" || st.Servers[1].Panel != "https://panel.example" ||
		st.Servers[1].SSH != "kisi@sunucu.example" || st.Servers[1].Bucket != "footops-h1/kasa" || st.Servers[1].File.Sys == nil {
		t.Errorf("status: %+v", st)
	}
	for _, secret := range []string{testSecret, testKeyID, testAccount, "RepoPassword"} {
		if strings.Contains(out.String()+text+errb.String(), secret) {
			t.Errorf("%q in what durum says", secret)
		}
	}
}

// Nothing to go by: the Keychain without the key or the account, a settings file not
// understood — said, gray, exit code 1 (the JSON still one object for the app).
func TestStatusCommandSetup(t *testing.T) {
	r2 := &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}
	ctx := context.Background()
	e, out, errb := testEnv(t, r2, testNow)
	k := e.keychain.(fakeKeychain)
	delete(k.items, "r2-mac-gizli")
	if code := e.run(ctx, []string{"durum", "--json"}); code != 1 {
		t.Fatalf("no key: %d", code)
	}
	var st Status
	if json.Unmarshal(out.Bytes(), &st) != nil || st.Color != gray || !strings.Contains(st.Error, "footops-yedek / r2-mac-gizli yok") {
		t.Errorf("no key: %s", out)
	}
	out.Reset()
	if code := e.run(ctx, []string{"durum"}); code != 1 || !strings.Contains(errb.String(), "✗ Anahtar Zinciri'nde footops-yedek / r2-mac-gizli yok") {
		t.Errorf("no key, text: %d %s", code, errb)
	}
	k.items["r2-mac-gizli"] = testSecret
	k.items["r2-hesap"] = "https://baska.example"
	errb.Reset()
	if code := e.run(ctx, []string{"durum"}); code != 1 || !strings.Contains(errb.String(), "R2 hesap kimliğine benzemiyor") {
		t.Errorf("bad account: %d %s", code, errb)
	}
	k.items["r2-hesap"] = testAccount
	for _, bad := range []string{`{"format":2}`, `{"format":1,"servers":{"H 1":{}}}`, `{"format":1,"servers":{"b":{"bucket":"../kova"}}}`, `{`} {
		writeSettings(t, e, bad)
		errb.Reset()
		if code := e.run(ctx, []string{"durum"}); code != 1 || !strings.Contains(errb.String(), "ayarlar dosyası") {
			t.Errorf("settings %s: %d %s", bad, code, errb)
		}
	}
	// No server known: neither a repository password nor the settings name one.
	writeSettings(t, e, `{"format":1}`)
	delete(k.items, "restic-h1")
	delete(k.items, "restic-a")
	errb.Reset()
	if code := e.run(ctx, []string{"durum"}); code != 1 || !strings.Contains(errb.String(), "sunucu yok") {
		t.Errorf("no servers: %d %s", code, errb)
	}
	// One in the settings only is enough.
	writeSettings(t, e, `{"format":1,"servers":{"b":{"bucket":"kova-b"}}}`)
	out.Reset()
	if code := e.run(ctx, []string{"durum", "--json"}); code != 0 || json.Unmarshal(out.Bytes(), &st) != nil ||
		len(st.Servers) != 1 || st.Servers[0].Bucket != "kova-b" || st.Servers[0].Color != red {
		t.Errorf("settings only: %s", out)
	}
	// R2's answer cut short: the way's trouble (gray), not the server's.
	r2.cut = true
	out.Reset()
	e.run(ctx, []string{"durum", "--json"})
	if json.Unmarshal(out.Bytes(), &st) != nil || st.Color != gray || len(st.Servers[0].Problems) != 1 ||
		!strings.Contains(st.Servers[0].Problems[0].Text, "R2'den okunurken bağlantı koptu") {
		t.Errorf("cut short: %s", out)
	}
	r2.cut = false
	// R2 refusing the key: gray, said once for both files.
	r2.status = 403
	out.Reset()
	e.run(ctx, []string{"durum", "--json"})
	if json.Unmarshal(out.Bytes(), &st) != nil || st.Color != gray || len(st.Servers[0].Problems) != 1 {
		t.Errorf("refused: %s", out)
	}
}

// Stopped (the app's SIGTERM): no status from what was read by then, exit 1.
func TestStatusStopped(t *testing.T) {
	e, out, _ := testEnv(t, &fakeR2{objects: map[string][]byte{}, modified: map[string]time.Time{}}, testNow)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := e.run(ctx, []string{"durum", "--json"}); code != 1 || strings.TrimSpace(out.String()) != `{"event":"error","message":"durduruldu"}` {
		t.Errorf("stopped: %d %s", code, out)
	}
}
