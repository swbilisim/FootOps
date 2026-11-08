package install

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/config"
	"github.com/swbilisim/FootOps/internal/core"
)

// Recovery mode (Faz Y5, panel-tasarim §27.5): the server is installed as usual, without
// the tunnel and the panel account, and then brought back from the backups of the
// server whose code it takes (agent.Recovery). It ends as a rehearsal: `footops go-live`
// puts the tunnel, the backups and Telegram back once the old server is surely off.

// RecoverAnswers make the install a recovery. The repository's secret key and password
// are only held in memory for the run that uses them.
type RecoverAnswers struct {
	Repository string `json:"repository"`
	KeyID      string `json:"key_id"`
	// At goes back to the newest backups before it; zero: the newest.
	At time.Time `json:"at,omitzero"`

	SecretKey string `json:"-"`
	Password  string `json:"-"`
}

// Validate checks what is kept (no secrets: a resumed run asks for them again).
func (r RecoverAnswers) Validate() error {
	if err := agent.CheckRepository(r.Repository); err != nil {
		return fmt.Errorf("recover.repository: %w", err)
	}
	if r.KeyID == "" {
		return errors.New("recover.key_id: boş")
	}
	return nil
}

func (r RecoverAnswers) request() agent.RecoverRequest {
	return agent.RecoverRequest{Repository: r.Repository, KeyID: r.KeyID, SecretKey: r.SecretKey, Password: r.Password, At: r.At,
		Actor: installActor()}
}

func installActor() agent.Actor {
	who := os.Getenv("SUDO_USER")
	if who == "" {
		who = "root"
	}
	return agent.Actor{User: who, IP: "sunucu", Via: "ssh"}
}

// recoveryPrefix starts a recovery line: everything a recovery needs in one paste,
// made on the Mac (footops-yedek kurtarma <sunucu>).
const recoveryPrefix = "FOOTOPS-KURTARMA-1:"

type recoveryInfo struct {
	Repository string `json:"repository"`
	KeyID      string `json:"key_id"`
	SecretKey  string `json:"secret_key"`
	Password   string `json:"password"`
}

// ParseRecoveryInfo reads a recovery line.
func ParseRecoveryInfo(s string) (RecoverAnswers, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(s), recoveryPrefix)
	if !ok {
		return RecoverAnswers{}, errors.New("kurtarma bilgisi " + recoveryPrefix + " ile başlamalı (Mac'te: footops-yedek kurtarma <sunucu>)")
	}
	raw, err := base64.StdEncoding.DecodeString(rest)
	var in recoveryInfo
	if err != nil || json.Unmarshal(raw, &in) != nil {
		return RecoverAnswers{}, errors.New("kurtarma bilgisi bozuk; satırın tamamını kopyala")
	}
	r := RecoverAnswers{Repository: in.Repository, KeyID: in.KeyID, SecretKey: in.SecretKey, Password: in.Password}
	req := r.request()
	if err := req.Check(); err != nil {
		return RecoverAnswers{}, err
	}
	return r, nil
}

// FormatRecoveryInfo makes a recovery line (what the Mac's helper prints).
func FormatRecoveryInfo(r RecoverAnswers) string {
	b, _ := json.Marshal(recoveryInfo{Repository: r.Repository, KeyID: r.KeyID, SecretKey: r.SecretKey, Password: r.Password})
	return recoveryPrefix + base64.StdEncoding.EncodeToString(b)
}

var reAccount = regexp.MustCompile(`^[0-9a-f]{32}$`)

// r2Endpoint takes an R2 account ID (the endpoint is made from it) or an https address.
func r2Endpoint(s string) (string, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if reAccount.MatchString(strings.ToLower(s)) {
		return "https://" + strings.ToLower(s) + ".r2.cloudflarestorage.com", nil
	}
	if strings.HasPrefix(s, "https://") && !strings.Contains(strings.TrimPrefix(s, "https://"), "/") {
		return s, nil
	}
	return "", errors.New("32 karakterlik hesap kimliği ya da https://<sunucu> biçiminde uç nokta gir")
}

// askRecover asks where the backups are and with what to open them.
func askRecover(p *Prompter, code, tz string) (*RecoverAnswers, error) {
	p.Say("")
	p.Say("Yedek deposu: eski sunucunun R2 kovası. Okuyabilen bir anahtar yeter; önerilen Mac'teki salt okunur")
	p.Say("anahtar (yazamaz, silemez: canlı sunucunun yedeklerine hiçbir şey olmaz).")
	p.Say("Mac'te hepsi tek satırda panoya alınır:  footops-yedek kurtarma %s | pbcopy", strings.ToLower(code))
	for {
		line, err := p.Secret("Kurtarma bilgisi (ekranda görünmez; yoksa boş bırak, tek tek sorulur)", func(s string) error {
			if s == "" {
				return nil
			}
			_, err := ParseRecoveryInfo(s)
			return err
		})
		if err != nil {
			return nil, err
		}
		var r RecoverAnswers
		if line != "" {
			r, _ = ParseRecoveryInfo(line)
			p.Say("  ✓ Depo: %s, anahtar %s…", agent.RepoLabel(r.Repository), tail(r.KeyID, 4))
		} else {
			acct, err := p.Ask("R2 hesap kimliği (Cloudflare → R2 → Account ID) ya da S3 uç noktası", "", func(s string) error {
				_, err := r2Endpoint(s)
				return err
			})
			if err != nil {
				return nil, err
			}
			endpoint, _ := r2Endpoint(acct)
			bucket, err := p.Ask("Kova (kova/klasör olabilir)", "footops-"+strings.ToLower(code), func(s string) error {
				return agent.CheckRepository("s3:" + endpoint + "/" + strings.Trim(s, "/"))
			})
			if err != nil {
				return nil, err
			}
			r.Repository = "s3:" + endpoint + "/" + strings.Trim(bucket, "/")
			if r.KeyID, err = p.Ask("Erişim anahtarı kimliği (Access Key ID)", "", nonEmpty); err != nil {
				return nil, err
			}
			if r.SecretKey, err = p.Secret("Gizli erişim anahtarı (Secret Access Key, ekranda görünmez)", nonEmpty); err != nil {
				return nil, err
			}
			if r.Password, err = p.Secret("Depo şifresi (ekranda görünmez)", nonEmpty); err != nil {
				return nil, err
			}
		}
		at, err := askAt(p, tz)
		if err != nil {
			return nil, err
		}
		r.At = at
		req := r.request()
		if err := req.Check(); err != nil {
			p.Say("  ✗ %v — yeniden gir", err)
			continue
		}
		r.Repository, r.KeyID, r.SecretKey = req.Repository, req.KeyID, req.SecretKey
		return &r, nil
	}
}

// atLayout is how a point in time is typed (in the server's time zone).
const atLayout = "2006-01-02 15:04"

// askAt asks which backups to go back to: empty is the newest.
func askAt(p *Prompter, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.Local
	}
	var at time.Time
	_, err = p.Ask("Hangi zamana dönülsün? (boş: en son yedek; örnek 2026-11-07 04:00, saat "+tz+")", "", func(s string) error {
		if s == "" {
			at = time.Time{}
			return nil
		}
		t, err := time.ParseInLocation(atLayout, s, loc)
		if err != nil {
			return errors.New("YYYY-AA-GG SS:DD biçiminde gir, örnek 2026-11-07 04:00")
		}
		if t.After(time.Now()) {
			return errors.New("gelecekte bir zaman olamaz")
		}
		at = t.UTC()
		return nil
	})
	return at, err
}

// askRecoverSecrets asks a resumed recovery for what is never kept.
func askRecoverSecrets(p *Prompter, r *RecoverAnswers) error {
	p.Say("")
	p.Say("Kurtarma sürüyor: depo %s, anahtar %s…", agent.RepoLabel(r.Repository), tail(r.KeyID, 4))
	line, err := p.Secret("Kurtarma bilgisi (ekranda görünmez; yoksa boş bırak)", func(s string) error {
		if s == "" {
			return nil
		}
		in, err := ParseRecoveryInfo(s)
		if err == nil && (in.Repository != r.Repository || in.KeyID != r.KeyID) {
			err = errors.New("bu bilgi başka bir depo ya da anahtar için")
		}
		return err
	})
	if err != nil {
		return err
	}
	if line != "" {
		in, _ := ParseRecoveryInfo(line)
		r.SecretKey, r.Password = in.SecretKey, in.Password
		return nil
	}
	if r.SecretKey, err = p.Secret("Gizli erişim anahtarı (Secret Access Key, ekranda görünmez)", nonEmpty); err != nil {
		return err
	}
	r.Password, err = p.Secret("Depo şifresi (ekranda görünmez)", nonEmpty)
	return err
}

func nonEmpty(s string) error {
	if s == "" {
		return errors.New("boş olamaz")
	}
	return nil
}

// ---- the step ----

// RecoverFunc starts the recovery in the agent and follows it to the end, printing its
// steps to out; StatusFunc reads where it stands.
type (
	RecoverFunc func(ctx context.Context, req agent.RecoverRequest, out io.Writer) error
	StatusFunc  func(ctx context.Context) (agent.RecoverStatus, error)
)

type recoverStep struct {
	rec    *RecoverAnswers
	run    RecoverFunc
	status StatusFunc
	out    io.Writer
}

func (s recoverStep) Name() string { return "recover" }
func (s recoverStep) Describe() string {
	return "Yedekten kurtarma (" + agent.RepoLabel(s.rec.Repository) + "): sunucu ayarları ve siteler, prova kipinde"
}

func (s recoverStep) Check(ctx context.Context) (bool, error) {
	st, err := s.status(ctx)
	if err != nil {
		return false, nil // the agent is not there yet
	}
	return st.Running == "" && st.State != nil && st.State.Complete(), nil
}

func (s recoverStep) Apply(ctx context.Context) error {
	// A recovery under way (an earlier run's, its following stopped) is followed: that
	// needs no secret.
	running := false
	if st, err := s.status(ctx); err == nil {
		running = st.Running != ""
	}
	if !running && (s.rec.SecretKey == "" || s.rec.Password == "") {
		return errors.New("kurtarma bilgisi yok; kurulumu yeniden başlatıp gir")
	}
	return s.run(ctx, s.rec.request(), s.out)
}

// AgentRecover is RecoverFunc against the server's agent. A recovery already under way
// (an earlier run's, its following stopped) is followed instead of started again.
func AgentRecover(ctx context.Context, req agent.RecoverRequest, out io.Writer) error {
	c := agent.NewClient(agent.DefaultSocket)
	var id string
	if st, err := c.RecoverStatus(ctx); err == nil && st.Running != "" {
		id = st.Running
		fmt.Fprintln(out, "Önceki kurtarma sürüyor; izleniyor.")
	} else {
		var err error
		if id, err = waitAgent(ctx, func() (string, error) { return c.Recover(ctx, req) }); err != nil {
			var ae *agent.Error
			if errors.As(err, &ae) && len(ae.Problems) > 0 {
				return fmt.Errorf("%s: %s", ae.Message, strings.Join(ae.Problems, "; "))
			}
			return err
		}
	}
	return followJob(ctx, c, id, out)
}

// AgentRecoverStatus is StatusFunc against the server's agent.
func AgentRecoverStatus(ctx context.Context) (agent.RecoverStatus, error) {
	return agent.NewClient(agent.DefaultSocket).RecoverStatus(ctx)
}

// waitAgent retries a call while the agent, just (re)started, is not listening yet.
func waitAgent(ctx context.Context, call func() (string, error)) (string, error) {
	var last error
	for i := 0; i < 30; i++ {
		id, err := call()
		if err == nil || !strings.Contains(err.Error(), "agent'a ulaşılamadı") {
			return id, err
		}
		last = err
		if err := sleep(ctx, time.Second); err != nil {
			return "", err
		}
	}
	return "", last
}

// followJob prints a job's steps as they come and returns its error. When ctx ends the
// following stops; the job goes on in the agent.
func followJob(ctx context.Context, c *agent.Client, id string, out io.Writer) error {
	shown := 0
	for {
		j, err := c.Job(context.WithoutCancel(ctx), id)
		if err != nil {
			return err
		}
		for ; shown < len(j.Steps); shown++ {
			fmt.Fprintln(out, "  "+j.Steps[shown])
		}
		if j.Done {
			if j.Err != "" {
				return errors.New(j.Err)
			}
			return nil
		}
		if err := sleep(ctx, time.Second); err != nil {
			fmt.Fprintln(out, "  İzleme durdu; kurtarma agent'ta sürüyor. Kurulumu yeniden çalıştırınca izlemeye devam eder.")
			return err
		}
	}
}

// printRecovery tells what came back, how to look at the rehearsal and what going live
// takes. The DNS list is the sites' domains, which Cloudflare must point here then.
func printRecovery(ctx context.Context, o Options, a Answers, status StatusFunc) {
	p := o.Prompt
	st, err := status(ctx)
	if err != nil || st.State == nil {
		return
	}
	ip := serverIP(ctx, o.Host)
	p.Say("")
	p.Say("Yedekten kurtarma (%s, sunucu ayarları %s):", st.State.Repository, localTime(st.State.ServerTaken))
	var domains []string
	for _, s := range st.State.Sites {
		mark := map[string]string{"ok": "✓", "warn": "⚠", "failed": "✗", "started": "…"}[s.State]
		line := fmt.Sprintf("  %s %s", mark, s.Name)
		if s.Detail != "" {
			line += " — " + s.Detail
		}
		p.Say("%s", line)
		for _, w := range s.Warnings {
			p.Say("      ⚠ %s", w)
		}
		if s.Error != "" {
			p.Say("      ✗ %s", s.Error)
		}
		if s.State == "ok" || s.State == "warn" {
			domains = append(domains, s.Domains...)
		}
	}
	for _, s := range st.State.Skipped {
		p.Say("  – %s: getirilmedi (son tam yedekte yok)", s)
	}
	sort.Strings(domains)
	p.Say("")
	p.Say("Sunucu PROVA KİPİNDE: tünel, yedekleme ve Telegram kapalı; siteler dışarıya bağlantı açamaz.")
	p.Say("  Panel (eski sunucunun şifresi ve 2FA'sı): ssh -L 7701:127.0.0.1:7701 %s@%s → http://localhost:7701", a.AdminUser, ip)
	if len(domains) > 0 {
		p.Say("  Siteleri DNS'e dokunmadan dene (Mac'te; Origin sertifikasını tarayıcılar tanımaz, -k ondan):")
		p.Say("    curl -skI --resolve %s:443:%s https://%s/", domains[0], ip, domains[0])
	}
	p.Say("")
	p.Say("Gerçek bir felaketse (eski sunucu kapalı) canlıya al: sudo footops go-live")
	p.Say("  sonra Cloudflare DNS'te şu A kayıtlarını %s'e çevir:", ip)
	for _, d := range domains {
		p.Say("    %s", d)
	}
	p.Say("  (Panelin adresi tünelin; DNS'i değişmez.)")
}

func localTime(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	return t.Local().Format("02.01.2006 15:04")
}

// ---- going live ----

// GoLiveDeps are go-live's ways to the agent; tests stand in fakes.
type GoLiveDeps struct {
	Status StatusFunc
	// Setup connects the repository again (as root, for go-live) and follows the job.
	Setup func(ctx context.Context, req agent.BackupSetupRequest, out io.Writer) error
	End   func(ctx context.Context) error
	// Backup starts a backup of everything and returns its job.
	Backup func(ctx context.Context) (string, error)
	// HeldDir is where the recovery kept what goes back in place.
	HeldDir string
}

// AgentGoLive are GoLiveDeps against the server's agent.
func AgentGoLive() GoLiveDeps {
	c := agent.NewClient(agent.DefaultSocket)
	return GoLiveDeps{
		Status: AgentRecoverStatus,
		Setup: func(ctx context.Context, req agent.BackupSetupRequest, out io.Writer) error {
			id, err := waitAgent(ctx, func() (string, error) { return c.BackupSetup(ctx, req) })
			if err != nil {
				return err
			}
			return followJob(ctx, c, id, out)
		},
		End: func(ctx context.Context) error { return c.EndRehearsal(ctx, installActor()) },
		Backup: func(ctx context.Context) (string, error) {
			return waitAgent(ctx, func() (string, error) { return c.BackupRun(ctx, "", installActor()) })
		},
		HeldDir: filepath.Join(agent.DefaultRecoverDir, "held"),
	}
}

// GoLive makes a recovered server the live one: the tunnel and the panel's Cloudflare
// Access, the backup repository and the Telegram bot go back in place, the sites may
// reach out again, a first backup starts. It can be run again if it stops half-way: what
// is done is checked and kept, and the kept files are removed only at the end.
func GoLive(ctx context.Context, h Host, p *Prompter, out io.Writer, d GoLiveDeps) error {
	if os.Geteuid() != 0 && h.Root == "/" {
		return errors.New("root olarak çalıştırılmalı: sudo footops go-live")
	}
	st, err := d.Status(ctx)
	if err != nil {
		return fmt.Errorf("agent'a ulaşılamadı: %w", err)
	}
	if st.Running != "" {
		return errors.New("kurtarma hâlâ sürüyor; bitince yeniden dene")
	}
	heldDir := h.Path(d.HeldDir)
	if !h.Exists(d.HeldDir) {
		if st.Rehearsal == nil {
			return errors.New("bu sunucu prova kipinde değil (yedekten kurtarılmamış ya da zaten canlı)")
		}
		return errors.New("prova kipinde ama canlıya alma için saklananlar yok (" + d.HeldDir + "); kurtarma tamamlanmamış olabilir: sudo footops install")
	}
	saved, err := h.ReadFile(AnswersPath)
	if err != nil || saved == "" {
		return errors.New("kurulum cevapları okunamadı (" + AnswersPath + ")")
	}
	a, err := UnmarshalAnswers(saved)
	if err != nil {
		return err
	}
	held := func(name string) string {
		b, err := os.ReadFile(filepath.Join(heldDir, name))
		if err != nil {
			return ""
		}
		return string(b)
	}
	var oldPanel config.Panel
	if s := held("footops.json"); s != "" {
		_ = json.Unmarshal([]byte(s), &oldPanel)
	}
	token := core.TunnelTokenFromEnv(held("cloudflared.env"))
	tunnel := token != "" && oldPanel.TunnelEnabled()
	var repo agent.BackupSetup
	_ = json.Unmarshal([]byte(held("repository.json")), &repo)
	telegram := held("telegram.json")

	p.Say("")
	p.Say("Canlıya alma (%s):", a.ServerCode)
	if tunnel {
		p.Say("  - Cloudflare Tunnel + Access geri gelir (%s): eski sunucu da açıksa tünele ikinci bağlantı olur", oldPanel.PanelHost)
	}
	if repo.Repository != "" {
		p.Say("  - yedekler eski sunucunun deposuna (%s) bu sunucudan devam eder", agent.RepoLabel(repo.Repository))
	}
	if telegram != "" {
		p.Say("  - Telegram bildirimleri açılır")
	}
	p.Say("  - siteler dışarıya bağlanabilir (e-posta, ödeme, pazaryeri işleri çalışır)")
	p.Say("")
	p.Say("⚠ Eski sunucu KAPALI olmalı (sağlayıcının panelinden kapat ya da sil). İkisi birlikte çalışırsa")
	p.Say("  siteler aynı dış işleri iki kez yapar ve iki sunucu aynı depoya yazar.")
	if _, err := p.Ask("Eski sunucu kapalıysa onaylamak için sunucu kodunu yaz ("+a.ServerCode+")", "", func(s string) error {
		if s != a.ServerCode {
			return errors.New("sunucu kodu tutmuyor; vazgeçmek için Ctrl+C")
		}
		return nil
	}); err != nil {
		return err
	}

	// The repository first: it is what is most likely to need a hand (a key revoked
	// after the incident), and nothing is connected anywhere before it opens.
	if repo.Repository != "" {
		fmt.Fprintln(out, "\n… Yedek deposu: "+agent.RepoLabel(repo.Repository))
		repo.GoLive = true
		err := d.Setup(ctx, agent.BackupSetupRequest{BackupSetup: repo, Actor: installActor()}, out)
		if err != nil && strings.Contains(err.Error(), "erişimi reddetti") {
			p.Say("")
			p.Say("Yedekteki yazma anahtarı reddedildi (iptal edilmiş ya da değiştirilmiş olabilir). Cloudflare → R2 →")
			p.Say("API token: yalnız bu kovaya Object Read & Write bir anahtar oluştur ve gir (boş bırakırsan go-live durur).")
			if repo.KeyID, err = p.Ask("Erişim anahtarı kimliği (Access Key ID)", "", nil); err != nil {
				return err
			}
			if repo.KeyID == "" {
				return errors.New("yeni anahtar girilmedi; sunucu prova kipinde kaldı")
			}
			if repo.SecretKey, err = p.Secret("Gizli erişim anahtarı (Secret Access Key, ekranda görünmez)", nonEmpty); err != nil {
				return err
			}
			err = d.Setup(ctx, agent.BackupSetupRequest{BackupSetup: repo, Actor: installActor()}, out)
		}
		if err != nil {
			return fmt.Errorf("yedek deposu bağlanamadı (sunucu prova kipinde kaldı; düzeltip yeniden çalıştır): %w", err)
		}
		// A backup taken after this recovery started is not this server's: the old one
		// still runs.
		if now, err := d.Status(ctx); err == nil && st.State != nil && now.Newest.After(st.State.Started) {
			p.Say("")
			p.Say("⚠ Depoda bu kurtarmadan sonra alınmış bir yedek var (%s): eski sunucu hâlâ çalışıyor olabilir.", localTime(now.Newest))
			goOn, err := p.YesNo("Eski sunucunun kapalı olduğundan eminsen devam edilsin mi?", false)
			if err != nil {
				return err
			}
			if !goOn {
				return errors.New("durduruldu: sunucu prova kipinde kaldı (depo bağlı, yedek alınmıyor)")
			}
		}
	} else {
		fmt.Fprintln(out, "⚠ Yedekte depo ayarı yoktu: canlıya aldıktan sonra panelden Yedekleme → Depoyu bağla.")
	}

	if telegram != "" {
		if err := os.MkdirAll(h.Path(core.NotifySecretDir), 0o700); err != nil {
			return err
		}
		if err := h.WriteFile(filepath.Join(core.NotifySecretDir, "telegram.json"), telegram, 0o600); err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ Telegram bağlantısı yerine kondu")
	}

	if tunnel {
		fmt.Fprintln(out, "\n… Tünel ve Access")
		typed := a.PanelHost
		a.TunnelEnabled, a.AccessTeam, a.AccessAUD, a.PanelHost = true, oldPanel.AccessTeam, oldPanel.AccessAUD, oldPanel.PanelHost
		if err := a.Validate(); err != nil {
			return fmt.Errorf("eski panel ayarları geçersiz: %w", err)
		}
		data, err := a.Marshal()
		if err != nil {
			return err
		}
		if err := h.WriteFile(AnswersPath, data, 0o600); err != nil {
			return err
		}
		coreCfg := core.Config{MongoCacheGB: a.MongoCacheGB, Tunnel: true}
		panelCfg := config.Panel{ServerCode: a.ServerCode, PanelHost: a.PanelHost, AccessTeam: a.AccessTeam, AccessAUD: a.AccessAUD,
			ListenTunnel: config.DefaultTunnel, ListenLocal: config.DefaultLocal}
		// panel-config restarts footops-web when footops.json changes: it opens its tunnel
		// listener only when it starts.
		steps := []Step{tunnelSecretStep{h, token}, coreFilesStep{h, coreCfg, &Notes{}}, coreStackStep{h, coreCfg},
			panelConfigStep{h, panelCfg}, panelServicesStep{h}}
		if _, err := Run(ctx, steps, out); err != nil {
			return err
		}
		if typed != a.PanelHost {
			// The agent reads the panel's address when it starts (its messages' links).
			if _, err := h.Run.Run(ctx, "systemctl", "restart", "footops-agent"); err != nil {
				return err
			}
		}
	} else {
		fmt.Fprintln(out, "ℹ Eski sunucuda tünel yoktu: panel SSH tüneliyle açılmaya devam eder.")
	}

	if err := d.End(ctx); err != nil {
		return fmt.Errorf("prova kipi bitirilemedi: %w", err)
	}
	fmt.Fprintln(out, "✓ Prova kipi bitti: siteler dışarıya bağlanabilir")
	if repo.Repository != "" {
		if id, err := d.Backup(ctx); err != nil {
			fmt.Fprintln(out, "⚠ İlk yedek başlatılamadı (gece kendiliğinden alınır): "+err.Error())
		} else {
			fmt.Fprintln(out, "✓ İlk yedek başladı (panelde Yedekleme'den izle; iş "+id+")")
		}
	}
	// The install is no recovery any more: a later `footops install` is an update.
	a.Recover = nil
	data, err := a.Marshal()
	if err != nil {
		return err
	}
	if err := h.WriteFile(AnswersPath, data, 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(heldDir); err != nil {
		return err
	}

	p.Say("")
	p.Say("Sunucu canlı. Şimdi Cloudflare DNS'te şu A kayıtlarını %s'e çevir:", serverIP(ctx, h))
	if st.State != nil {
		var domains []string
		for _, s := range st.State.Sites {
			if s.State == "ok" || s.State == "warn" {
				domains = append(domains, s.Domains...)
			}
		}
		sort.Strings(domains)
		for _, dm := range domains {
			p.Say("  %s", dm)
		}
	}
	if tunnel {
		p.Say("Panel: https://%s (tünel; DNS'i değişmez)", a.PanelHost)
	}
	return nil
}
