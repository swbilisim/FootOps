package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/swbilisim/FootOps/internal/agent"
	"github.com/swbilisim/FootOps/internal/install"
)

// Hidden `footops backup …` commands, until the panel's backup pages (Faz Y2). They ask
// the agent over its socket (as root, through sudo), so a backup is a job like any
// other: one at a time, in the panel's job list and in the audit log.
const backupUsage = `Kullanım: sudo footops backup <komut>

  status                     depo, ayarlar, son yedekler
  setup [--existing]         yedek deposunu bağla (yeni depo: şifre bir kez gösterilir;
                             --existing: var olan deponun şifresiyle bağlan)
  confirm                    şifre başka yere kaydedildi: zamanlanmış yedekler başlar
  run [site]                 şimdi yedekle (bütün siteler + sunucu ayarları ya da tek site)
  runs [site]                son yedek çalıştırmaları
  set night=SS:DD | paused=on|off
  set defaults|<site> db=daily|6h|hourly files=daily|weekly|off keep=48h,30d,6m
                      [freeze=on|off] [off=on|off] | <site> reset
  show-key                   depo şifresini göster (denetim kaydına yazılır)
`

func runBackup(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, backupUsage)
		return flag.ErrHelp
	}
	c := agent.NewClient(agent.DefaultSocket)
	ctx := context.Background()
	who := os.Getenv("SUDO_USER")
	if who == "" {
		who = "root"
	}
	actor := agent.Actor{User: who, IP: "sunucu", Via: "ssh"}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "status":
		err = backupStatus(ctx, c, stdout)
	case "setup":
		err = backupSetup(ctx, c, actor, rest, stdin, stdout)
	case "confirm":
		if err = c.BackupConfirm(ctx, actor); err == nil {
			fmt.Fprintln(stdout, "✓ Onaylandı: zamanlanmış yedekler başlıyor.")
		}
	case "run":
		site := ""
		if len(rest) > 0 {
			site = rest[0]
		}
		var id string
		if id, err = c.BackupRun(ctx, site, actor); err == nil {
			err = followJob(ctx, c, id, stdout)
		}
	case "runs":
		site := ""
		if len(rest) > 0 {
			site = rest[0]
		}
		err = backupRuns(ctx, c, site, stdout)
	case "set":
		err = backupSet(ctx, c, actor, rest, stdout)
	case "show-key":
		var pw string
		if pw, err = c.BackupKey(ctx, actor); err == nil {
			fmt.Fprintln(stdout, pw)
		}
	case "help", "-h", "--help":
		fmt.Fprint(stdout, backupUsage)
	default:
		fmt.Fprint(stderr, backupUsage)
		return fmt.Errorf("bilinmeyen komut %q", cmd)
	}
	var ae *agent.Error
	if errors.As(err, &ae) && len(ae.Problems) > 0 {
		return fmt.Errorf("%s:\n  - %s", ae.Message, strings.Join(ae.Problems, "\n  - "))
	}
	if err != nil && strings.Contains(err.Error(), "agent'a ulaşılamadı") {
		return fmt.Errorf("%w (sudo ile çalıştır; footops-agent çalışıyor mu?)", err)
	}
	return err
}

// errDetached: the operator stopped following; the job goes on in the agent.
var errDetached = errors.New("izleme durdu; işlem agent'ta sürüyor (sonucu: sudo footops backup runs / status)")

// followJob prints a job's steps as they come and returns its error. Ctrl+C stops the
// following, not the job (errDetached).
func followJob(ctx context.Context, c *agent.Client, id string, out io.Writer) error {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	shown := 0
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		j, err := c.Job(ctx, id)
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
			fmt.Fprintf(out, "✓ Bitti (%s)\n", j.Finished.Sub(j.Started).Round(100*time.Millisecond))
			return nil
		}
		select {
		case <-sigs:
			fmt.Fprintln(out)
			return errDetached
		case <-tick.C:
		}
	}
}

func backupSetup(ctx context.Context, c *agent.Client, actor agent.Actor, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(out)
	existing := fs.Bool("existing", false, "connect with the existing repository's password")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var readSecret func() (string, error)
	if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fd := int(f.Fd())
		readSecret = func() (string, error) {
			b, err := term.ReadPassword(fd)
			return string(b), err
		}
	}
	p := install.NewPrompter(stdin, out, readSecret)
	nonEmpty := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("boş olamaz")
		}
		return nil
	}
	var req agent.BackupSetupRequest
	var err error
	if req.Repository, err = p.Ask("Depo adresi (s3:https://<hesap>.r2.cloudflarestorage.com/<kova>)", "", func(s string) error {
		if !strings.HasPrefix(s, "s3:") {
			return errors.New("s3: ile başlamalı")
		}
		return nil
	}); err != nil {
		return err
	}
	if req.KeyID, err = p.Ask("Erişim anahtarı kimliği (Access Key ID)", "", nonEmpty); err != nil {
		return err
	}
	if req.SecretKey, err = p.Secret("Gizli erişim anahtarı (Secret Access Key)", nonEmpty); err != nil {
		return err
	}
	if *existing {
		if req.Password, err = p.Secret("Var olan deponun şifresi", nonEmpty); err != nil {
			return err
		}
	}
	req.Actor = actor
	id, err := c.BackupSetup(ctx, req)
	if err != nil {
		return err
	}
	if err := followJob(ctx, c, id, out); err != nil {
		return err
	}
	j, err := c.Job(ctx, id)
	if err != nil {
		return err
	}
	if j.Result != "created" {
		fmt.Fprintln(out, "✓ Var olan depoya bağlandı; şifre zaten biliniyor (onaylandı).")
		return nil
	}
	pw, err := c.BackupKey(ctx, actor)
	if err != nil {
		return err
	}
	line := strings.Repeat("─", len(pw)+4)
	fmt.Fprintf(out, "\nYedek deposunun şifresi (yalnız şimdi gösteriliyor):\n\n  ┌%s┐\n  │  %s  │\n  └%s┘\n\n", line, pw, line)
	fmt.Fprintln(out, "Bu şifre olmadan yedekler açılamaz; sunucu kaybolursa yalnız bu şifreyle geri gelir.")
	fmt.Fprintln(out, "İki ayrı yere kaydet (ör. parola yöneticisi + Mac Anahtar Zinciri), sonra onayla.")
	for {
		ans, err := p.Ask("Kaydettiysen 'kaydettim' yaz", "", nil)
		if errors.Is(err, install.ErrInputClosed) {
			fmt.Fprintln(out, "Onay verilmedi; yedekler başlamaz. Kaydedince: sudo footops backup confirm")
			return nil
		}
		if err != nil {
			return err
		}
		if strings.EqualFold(strings.TrimSpace(ans), "kaydettim") {
			break
		}
	}
	if err := c.BackupConfirm(ctx, actor); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Onaylandı: zamanlanmış yedekler başlıyor.")
	return nil
}

func dbName(v string) string {
	return map[string]string{agent.DBDaily: "günlük", agent.DB6h: "6 saatte bir", agent.DBHourly: "saatlik"}[v]
}

func filesName(v string) string {
	return map[string]string{agent.FilesDaily: "günlük", agent.FilesWeekly: "haftalık", agent.FilesOff: "kapalı"}[v]
}

func policyText(p agent.BackupPolicy) string {
	var parts []string
	if p.Off {
		parts = append(parts, "YEDEKLENMİYOR")
	}
	if p.DB != "" {
		parts = append(parts, "veritabanı "+dbName(p.DB))
	}
	if p.Files != "" {
		parts = append(parts, "dosyalar "+filesName(p.Files))
	}
	if p.Freeze {
		parts = append(parts, "dondurarak")
	}
	if p.KeepHours != 0 || p.KeepDays != 0 || p.KeepMonths != 0 {
		parts = append(parts, fmt.Sprintf("saklama %s sa / %s gün / %s ay", orDefault(p.KeepHours), orDefault(p.KeepDays), orDefault(p.KeepMonths)))
	}
	return strings.Join(parts, ", ")
}

func orDefault(n int) string {
	if n == 0 {
		return "genel"
	}
	return strconv.Itoa(n)
}

func backupStatus(ctx context.Context, c *agent.Client, out io.Writer) error {
	st, err := c.BackupStatus(ctx)
	if err != nil {
		return err
	}
	if !st.Configured {
		fmt.Fprintln(out, "Yedek deposu bağlı değil: sudo footops backup setup")
		return nil
	}
	s := st.Settings
	conf := "ONAYLANMADI (yedek alınmaz: sudo footops backup confirm)"
	if !s.Confirmed.IsZero() {
		conf = "onaylandı " + s.Confirmed.Local().Format("02.01.2006 15:04")
	}
	fmt.Fprintf(out, "Yedek deposu: %s · şifre %s\n", st.Repository, conf)
	sched := "gece " + s.Night
	if s.Paused {
		sched += " · DURAKLATILDI"
	} else if !st.NextNight.IsZero() {
		sched += " · sonraki " + st.NextNight.Local().Format("02.01 15:04")
	}
	fmt.Fprintf(out, "Zamanlama: %s\nGenel: %s\n", sched, policyText(s.Defaults))
	names := make([]string, 0, len(s.Sites))
	for n := range s.Sites {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(out, "  %s: %s\n", n, policyText(s.Sites[n]))
	}
	if len(st.Last) > 0 {
		fmt.Fprintln(out, "Son başarılı yedekler:")
		keys := make([]string, 0, len(st.Last))
		for k := range st.Last {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			l := st.Last[k]
			short := l.Snapshot
			if len(short) > 8 {
				short = short[:8]
			}
			fmt.Fprintf(out, "  %-28s %s  %s\n", k, l.Time.Local().Format("02.01 15:04"), short)
		}
	}
	if st.RepoSize > 0 {
		fmt.Fprintf(out, "Depoda: %s (%s ölçüldü)\n", human(st.RepoSize), st.RepoSized.Local().Format("02.01 15:04"))
	}
	if !st.LastPrune.IsZero() || !st.LastCheck.IsZero() {
		fmt.Fprintf(out, "Son temizlik: %s · son denetim: %s\n", dateOr(st.LastPrune), dateOr(st.LastCheck))
	}
	if st.Error != "" {
		fmt.Fprintln(out, "⚠ "+st.Error)
	}
	return nil
}

func dateOr(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("02.01 15:04")
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func backupRuns(ctx context.Context, c *agent.Client, site string, out io.Writer) error {
	list, err := c.BackupRuns(ctx, site, 20)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(out, "Henüz yedek çalıştırması yok.")
		return nil
	}
	kinds := map[string]string{agent.RunDaily: "gece", agent.RunHourly: "saatlik", agent.RunManual: "elle"}
	for _, r := range list {
		var added int64
		for _, p := range r.Parts {
			added += p.AddedPacked
		}
		mark := "✓"
		if r.Err != "" {
			mark = "✗"
		}
		fmt.Fprintf(out, "%s %s %-7s %d parça, depoya +%s, %s\n", mark, r.Started.Local().Format("02.01 15:04"), kinds[r.Kind],
			len(r.Parts), human(added), r.Finished.Sub(r.Started).Round(time.Second))
		for _, p := range r.Parts {
			switch {
			case p.Err != "":
				fmt.Fprintf(out, "    ✗ %s/%s: %s\n", p.Site, p.Part, p.Err)
			case p.Warning != "":
				fmt.Fprintf(out, "    ⚠ %s/%s: %s\n", p.Site, p.Part, p.Warning)
			}
		}
		if r.Err != "" {
			fmt.Fprintf(out, "    %s\n", r.Err)
		}
	}
	return nil
}

// backupSet changes settings: global keys, the defaults or one site's override.
func backupSet(ctx context.Context, c *agent.Client, actor agent.Actor, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(backupUsage)
	}
	s, err := c.BackupSettings(ctx)
	if err != nil {
		return err
	}
	onOff := func(v string) (bool, error) {
		switch v {
		case "on", "açık", "evet":
			return true, nil
		case "off", "kapalı", "hayır":
			return false, nil
		}
		return false, fmt.Errorf("%q: on ya da off", v)
	}
	target := args[0]
	if strings.Contains(target, "=") {
		for _, kv := range args {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "night":
				s.Night = v
			case "paused":
				if s.Paused, err = onOff(v); err != nil {
					return err
				}
			default:
				return fmt.Errorf("bilinmeyen ayar %q (night, paused)", k)
			}
		}
	} else {
		var pol agent.BackupPolicy
		if target == "defaults" {
			pol = s.Defaults
		} else if s.Sites != nil {
			pol = s.Sites[target]
		}
		for _, kv := range args[1:] {
			if kv == "reset" && target != "defaults" {
				pol = agent.BackupPolicy{}
				continue
			}
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("%q: ad=değer biçiminde olmalı", kv)
			}
			switch k {
			case "db":
				pol.DB = v
			case "files":
				pol.Files = v
			case "freeze":
				if pol.Freeze, err = onOff(v); err != nil {
					return err
				}
			case "off":
				if pol.Off, err = onOff(v); err != nil {
					return err
				}
			case "keep":
				for _, part := range strings.Split(v, ",") {
					n, err := strconv.Atoi(strings.TrimRight(part, "hdm"))
					if err != nil || part == "" {
						return fmt.Errorf("keep=%s: örn. 48h,30d,6m", v)
					}
					switch part[len(part)-1] {
					case 'h':
						pol.KeepHours = n
					case 'd':
						pol.KeepDays = n
					case 'm':
						pol.KeepMonths = n
					default:
						return fmt.Errorf("keep=%s: örn. 48h,30d,6m", v)
					}
				}
			default:
				return fmt.Errorf("bilinmeyen ayar %q (db, files, keep, freeze, off)", k)
			}
		}
		if target == "defaults" {
			s.Defaults = pol
		} else {
			if s.Sites == nil {
				s.Sites = map[string]agent.BackupPolicy{}
			}
			if pol == (agent.BackupPolicy{}) {
				delete(s.Sites, target)
			} else {
				s.Sites[target] = pol
			}
		}
	}
	if err := c.SetBackupSettings(ctx, s, actor); err != nil {
		return err
	}
	fmt.Fprintln(out, "✓ Kaydedildi")
	return backupStatus(ctx, c, out)
}
