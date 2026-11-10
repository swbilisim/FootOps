package doctor

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/docker"
)

const (
	rebootFile = "/var/run/reboot-required"
	rebootPkgs = "/var/run/reboot-required.pkgs"
	// rebootGrace: unattended-upgrades sees the flag at its morning run and reboots at
	// the set time the night after, so a flag a day and a half old is still on time.
	rebootGrace = 48 * time.Hour

	updateStamp = "/var/lib/apt/periodic/update-success-stamp"
	uuStamp     = "/var/lib/apt/periodic/unattended-upgrades-stamp"
	// listsOld: package lists older than this may miss updates (apt refreshes them daily).
	listsOld = 48 * time.Hour
	// uuLate: unattended-upgrades runs every morning; a security update waiting while it
	// has not run for this long is a problem, not a matter of time.
	uuLate  = 50 * time.Hour
	uuStale = 72 * time.Hour

	// supportWarn: how long before cloudflared's support ends it becomes a warning.
	supportWarn = 60 * 24 * time.Hour
)

func reboot(env Env, now time.Time) Finding {
	f := Finding{ID: "reboot", Section: "system"}
	st, err := env.stat(rebootFile)
	if err != nil {
		f.Level, f.Title = OK, "Yeniden başlatma gerekmiyor"
		return f
	}
	age := now.Sub(st.ModTime())
	var pkgs []string
	seen := map[string]bool{}
	for _, l := range strings.Split(env.read(rebootPkgs), "\n") {
		if l = strings.TrimSpace(l); l != "" && !seen[l] {
			seen[l] = true
			pkgs = append(pkgs, l)
		}
	}
	sort.Strings(pkgs)
	asked := "Yeniden başlatma " + span(age) + " önce istendi"
	if len(pkgs) > 0 {
		asked += " (" + list(pkgs, 4) + ")"
	}
	if env.AutoReboot && age < rebootGrace {
		f.Level, f.Title = Info, "Yeniden başlatma bekliyor; sunucu kendiliğinden yeniden başlayacak (saat "+env.RebootTime+")"
		f.Detail = asked + "."
		return f
	}
	f.Level, f.Title = Warn, "Yeniden başlatma bekliyor"
	f.Detail = asked + "; güncellemeler (çoğunlukla çekirdek) ancak yeniden başlatınca devreye girer."
	if env.AutoReboot {
		f.Detail += " Otomatik yeniden başlatma açık ama gerçekleşmemiş: /var/log/unattended-upgrades/ kayıtlarına bak."
	} else {
		f.Detail += " Otomatik yeniden başlatma kapalı."
	}
	f.Fix = "Uygun bir saatte: sudo reboot (siteler 1-2 dk erişilemez)"
	return f
}

// pending is an update apt would install.
type pending struct {
	Name, To string
	Security bool
}

// parseInst reads `apt-get -s upgrade`: "Inst name [old] (new origin, origin [arch])".
func parseInst(out string) []pending {
	var ps []pending
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(l, "Inst ") {
			continue
		}
		f := strings.Fields(l)
		open, end := strings.IndexByte(l, '('), strings.LastIndexByte(l, ')')
		if len(f) < 2 || open < 0 || end < open {
			continue
		}
		inner := l[open+1 : end]
		p := pending{Name: f[1], Security: strings.Contains(inner, "-security")}
		if fs := strings.Fields(inner); len(fs) > 0 {
			p.To = fs[0]
		}
		ps = append(ps, p)
	}
	return ps
}

func updates(ctx context.Context, env Env, now time.Time) ([]pending, Finding) {
	f := Finding{ID: "updates", Section: "system"}
	// Phased updates (Ubuntu hands them out to a share of machines at a time) are left
	// out: in the agent's read-only service apt takes its own mount namespace for a
	// chroot and would count them all, at a terminal it counts only this machine's.
	out, err := env.Run.Run(ctx, "apt-get", "-s", "-o", "APT::Get::Never-Include-Phased-Updates=true", "--with-new-pkgs", "upgrade")
	if err != nil {
		f.Level, f.Title, f.Detail = Info, "Bekleyen paket güncellemeleri denetlenemedi", err.Error()
		return nil, f
	}
	ps := parseInst(out)
	var notes []string
	if st, err := env.stat(updateStamp); err == nil && now.Sub(st.ModTime()) > listsOld {
		notes = append(notes, "Paket listeleri "+span(now.Sub(st.ModTime()))+" önce yenilendi; liste eksik olabilir.")
	}
	if len(ps) == 0 {
		f.Level, f.Title, f.Detail = OK, "Bekleyen paket güncellemesi yok", strings.Join(notes, " ")
		return ps, f
	}
	var sec, other []string
	docker := false
	for _, p := range ps {
		if p.Security {
			sec = append(sec, p.Name)
		} else {
			other = append(other, p.Name)
		}
		docker = docker || strings.HasPrefix(p.Name, "docker-") || p.Name == "containerd.io"
	}
	f.Level, f.Title = Info, fmt.Sprintf("%d paket güncellemesi bekliyor", len(ps))
	var parts []string
	if len(sec) > 0 {
		f.Title += fmt.Sprintf(" (%d güvenlik)", len(sec))
		parts = append(parts, "Güvenlik: "+list(sec, 6)+" — otomatik güvenlik güncellemeleri bunları kurar.")
		if st, err := env.stat(uuStamp); err != nil || now.Sub(st.ModTime()) > uuLate {
			f.Level = Warn
			parts = append(parts, "Otomatik güncellemeler 2 günden uzun süredir çalışmadı.")
		}
	}
	if len(other) > 0 {
		parts = append(parts, "Diğer: "+list(other, 6)+" — kendiliğinden kurulmaz.")
		f.Fix = "SSH'den: sudo apt upgrade"
		if docker {
			f.Fix += " (Docker güncellenirken container'lar çalışmaya devam eder)"
		}
	}
	f.Detail = strings.Join(append(parts, notes...), " ")
	return ps, f
}

func unattended(ctx context.Context, env Env, now time.Time) Finding {
	f := Finding{ID: "unattended", Section: "system"}
	// is-active exits non-zero for anything but active; its word still says what it is.
	out, _ := env.Run.Run(ctx, "systemctl", "is-active", "apt-daily-upgrade.timer")
	if state := strings.TrimSpace(out); state != "active" {
		f.Level, f.Title = Warn, "Otomatik güvenlik güncellemeleri çalışmıyor"
		f.Detail = "apt-daily-upgrade.timer etkin değil"
		if state != "" && !strings.Contains(state, "\n") {
			f.Detail += " (" + state + ")"
		}
		f.Detail += "."
		f.Fix = "sudo systemctl enable --now apt-daily-upgrade.timer"
		return f
	}
	st, err := env.stat(uuStamp)
	if err != nil {
		f.Level, f.Title = Warn, "Otomatik güvenlik güncellemeleri hiç çalışmamış görünüyor"
		f.Detail, f.Fix = uuStamp+" yok.", "Kayıtlar: /var/log/unattended-upgrades/"
		return f
	}
	age := now.Sub(st.ModTime())
	if age > uuStale {
		f.Level, f.Title = Warn, "Otomatik güvenlik güncellemeleri "+span(age)+" önce çalıştı"
		f.Detail, f.Fix = "Her sabah çalışmalı.", "Kayıtlar: /var/log/unattended-upgrades/unattended-upgrades.log"
		return f
	}
	f.Level, f.Title = OK, "Otomatik güvenlik güncellemeleri çalışıyor (son: "+span(age)+" önce)"
	return f
}

func timeSync(ctx context.Context, env Env) Finding {
	f := Finding{ID: "time", Section: "network"}
	out, err := env.Run.Run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value")
	switch v := strings.TrimSpace(out); {
	case err == nil && v == "yes":
		f.Level, f.Title = OK, "Saat senkron (NTP)"
	case err == nil && v == "no":
		f.Level, f.Title = Warn, "Saat senkron değil"
		f.Detail = "Panelin 2FA kodları ve sertifika denetimleri doğru saate bağlı."
		f.Fix = "timedatectl status ile bak: systemd-timesyncd çalışıyor olmalı"
	default:
		f.Level, f.Title = Info, "Saat senkronu denetlenemedi"
		if err != nil {
			f.Detail = err.Error()
		}
	}
	return f
}

// listener is a socket of `ss -H -tulnp`.
type listener struct {
	proto, host string
	port        int
	proc        string
}

// parseSS reads lines like
// `tcp LISTEN 0 4096 0.0.0.0:443 0.0.0.0:* users:(("docker-proxy",pid=7,fd=4))`.
func parseSS(out string) []listener {
	var ls []listener
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 5 {
			continue
		}
		i := strings.LastIndexByte(f[4], ':')
		if i < 0 {
			continue
		}
		port, err := strconv.Atoi(f[4][i+1:])
		if err != nil {
			continue
		}
		li := listener{proto: f[0], host: f[4][:i], port: port}
		if len(f) > 6 {
			if _, rest, ok := strings.Cut(f[6], `(("`); ok {
				li.proc, _, _ = strings.Cut(rest, `"`)
			}
		}
		ls = append(ls, li)
	}
	return ls
}

// loopback reports whether an address only this machine reaches ("127.0.0.53%lo",
// "[::1]"); "*", "0.0.0.0" and "[::]" are every interface.
func loopback(host string) bool {
	if h, iface, ok := strings.Cut(host, "%"); ok {
		if iface == "lo" {
			return true
		}
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// expected: SSH and HTTPS; DHCP clients; UDP sockets in the ephemeral range are
// outgoing ones (cloudflared's QUIC connections, resolvers), which UFW keeps closed to
// anything but their replies.
func expected(l listener) bool {
	switch l.proto {
	case "tcp":
		return l.port == 22 || l.port == 443
	case "udp":
		return l.port == 443 || l.port == 68 || l.port == 546 || l.port >= 32768
	}
	return false
}

func ports(ctx context.Context, env Env, containers []docker.Container, dockerErr error) Finding {
	f := Finding{ID: "ports", Section: "network"}
	out, err := env.Run.Run(ctx, "ss", "-H", "-tulnp")
	if err != nil {
		f.Level, f.Title, f.Detail = Info, "Açık portlar denetlenemedi", err.Error()
		return f
	}
	// Docker publishes ports past UFW: those are open to everyone.
	var published []string
	pub := map[string]bool{}
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		for _, p := range c.Ports {
			key := fmt.Sprintf("%s %d", p.Type, p.PublicPort)
			if p.PublicPort == 0 || loopback(p.IP) || pub[key] {
				continue
			}
			pub[key] = true // IPv4 and IPv6 list it twice
			if c.Name() == "footops-caddy" && p.PublicPort == 443 {
				continue
			}
			published = append(published, key+" → "+c.Name())
		}
	}
	var odd []string
	seen := map[string]bool{}
	for _, l := range parseSS(out) {
		key := fmt.Sprintf("%s %d", l.proto, l.port)
		if loopback(l.host) || expected(l) || pub[key] || seen[key] {
			continue
		}
		seen[key] = true
		if l.proc != "" {
			key += " (" + l.proc + ")"
		}
		odd = append(odd, key)
	}
	switch {
	case len(published) > 0:
		f.Level, f.Title = Bad, "Docker dışarıya port açmış: "+list(published, 4)
		f.Detail = "Docker'ın açtığı port güvenlik duvarına (UFW) takılmaz, herkese açıktır. FootOps'ta siteler port açmaz; Caddy üzerinden yayınlanır."
		if len(odd) > 0 {
			f.Detail += " Ayrıca dinleyen: " + list(odd, 4) + "."
		}
		f.Fix = "Bu container'ı port yayınlamadan yeniden oluştur; FootOps dışında başlatıldıysa kaldır"
	case len(odd) > 0:
		f.Level, f.Title = Warn, "Beklenmeyen dinleyen port: "+list(odd, 4)
		f.Detail = "Güvenlik duvarı (UFW) içeri girişi kapalı tuttuğu sürece dışarıdan ulaşılamaz; yine de ne olduğuna bak."
		f.Fix = "sudo ss -tulnp ile süreci gör"
	default:
		f.Level, f.Title = OK, "Dışarıya açık yalnız 22 (SSH) ve 443 (HTTPS)"
	}
	if dockerErr != nil {
		f.Detail = strings.TrimSpace(f.Detail + " Docker'ın yayımladığı portlar denetlenemedi: " + dockerErr.Error())
	}
	return f
}

func images(ctx context.Context, env Env) Finding {
	f := Finding{ID: "images", Section: "disk"}
	if env.Docker == nil {
		f.Level, f.Title = Info, "İmajlar denetlenemedi"
		return f
	}
	u, err := env.Docker.ImageUsage(ctx)
	if err != nil {
		f.Level, f.Title, f.Detail = Info, "İmajlar denetlenemedi", err.Error()
		return f
	}
	var names []string
	var low, high int64
	for _, im := range Unused(u, env.KeepImages) {
		high += im.Size
		low += im.Size - max(im.SharedSize, 0)
		names = append(names, ImageName(im))
	}
	if len(names) == 0 {
		f.Level, f.Title = OK, "Kullanılmayan imaj yok"
		return f
	}
	sort.Strings(names)
	f.Level = Info
	f.Title = fmt.Sprintf("%d imajı hiçbir container kullanmıyor (%s)", len(names), sizeRange(low, high))
	f.Detail = list(names, 5) + "."
	return f
}

// Unused are the images no container uses, stopped ones included, but those keep names
// (FootOps' own; references as pulled, a digest too).
func Unused(u docker.ImageUsage, keep []string) []docker.ImageEntry {
	refs := map[string]bool{}
	for _, ref := range keep {
		name, digest, _ := strings.Cut(ref, "@")
		refs[name] = true
		if digest != "" {
			if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
				name = name[:i]
			}
			refs[name+"@"+digest] = true
		}
	}
	var out []docker.ImageEntry
	for _, im := range u.Images {
		// -1: not counted (an older engine); such an image is left alone.
		if im.Containers == 0 && !kept(im, refs) {
			out = append(out, im)
		}
	}
	return out
}

func kept(im docker.ImageEntry, keep map[string]bool) bool {
	for _, r := range append(append([]string{}, im.RepoTags...), im.RepoDigests...) {
		if keep[r] {
			return true
		}
	}
	return false
}

// ImageName is an image as people know it: its first tag, else its ID.
func ImageName(im docker.ImageEntry) string {
	for _, t := range im.RepoTags {
		if t != "<none>:<none>" {
			return t
		}
	}
	id := strings.TrimPrefix(im.ID, "sha256:")
	return "etiketsiz " + id[:min(12, len(id))]
}

// sizeRange: images share layers, so what deleting them frees lies between the parts
// only they hold (low) and their whole size (high).
func sizeRange(low, high int64) string {
	if high-low <= high/20 {
		return "~" + bytesTR(high)
	}
	return bytesTR(low) + " ile " + bytesTR(high) + " arası"
}

func versions(ctx context.Context, env Env, containers []docker.Container, dockerErr error, ps []pending, now time.Time) []Finding {
	out := []Finding{{ID: "footops", Section: "versions", Level: OK, Title: "FootOps " + env.FootOps}}
	d := Finding{ID: "docker", Section: "versions", Level: Info, Title: "Docker sürümü denetlenemedi"}
	if env.Docker != nil {
		if v, err := env.Docker.Version(ctx); err != nil {
			d.Detail = err.Error()
		} else {
			d.Level, d.Title = OK, "Docker "+v.Version
			for _, p := range ps {
				if p.Name == "docker-ce" {
					d.Level, d.Title = Info, d.Title+" — "+aptVersion(p.To)+" çıkmış"
					d.Fix = "SSH'den: sudo apt upgrade (container'lar çalışmaya devam eder)"
				}
			}
		}
	}
	out = append(out, d)
	if dockerErr != nil {
		return out
	}
	byName := map[string]string{}
	for _, c := range containers {
		byName[c.Name()] = c.Image
	}
	if img := byName["footops-caddy"]; img != "" {
		out = append(out, Finding{ID: "caddy", Section: "versions", Level: OK, Title: "Caddy " + tagOf(img)})
	}
	if img := byName["footops-mongo"]; img != "" {
		out = append(out, Finding{ID: "mongo", Section: "versions", Level: OK, Title: "MongoDB " + tagOf(img)})
	}
	if img := byName["footops-cloudflared"]; img != "" {
		out = append(out, cloudflared(tagOf(img), now))
	}
	return out
}

// cloudflared: Cloudflare supports a release for a year; its version is the year and
// month it came out (2026.10.3).
func cloudflared(tag string, now time.Time) Finding {
	f := Finding{ID: "cloudflared", Section: "versions", Level: Info, Title: "cloudflared " + tag}
	parts := strings.Split(tag, ".")
	if len(parts) < 2 {
		return f
	}
	y, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || m < 1 || m > 12 {
		return f
	}
	end := time.Date(y+1, time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	left := end.Sub(now)
	switch {
	case left <= 0:
		f.Level, f.Title = Bad, "cloudflared "+tag+": Cloudflare'in desteği bitti"
		f.Detail = "Bir yıldan eski sürümler bağlanamayabilir: panelin tüneli kopar (siteler etkilenmez)."
		f.Fix = "FootOps'un yeni sürümüyle güncellenir (panel: Sunucu → Güncelleme)"
	case left < supportWarn:
		f.Level, f.Title = Warn, "cloudflared "+tag+": Cloudflare desteğinin bitmesine "+span(left)+" kaldı"
		f.Detail = "Destek bitince panelin tüneli kopabilir (siteler etkilenmez)."
		f.Fix = "FootOps'un yeni sürümüyle güncellenir (panel: Sunucu → Güncelleme)"
	default:
		f.Level = OK
		f.Title += " (Cloudflare desteği bitişi: " + end.Format("2006-01") + ")"
	}
	return f
}

// tagOf is an image reference's tag ("caddy:2.11.7-alpine" → "2.11.7-alpine").
func tagOf(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[i+1:]
	}
	return ref
}

// aptVersion strips the epoch and the packaging part: "5:29.9.5-1~ubuntu.24.04~noble"
// → "29.9.5".
func aptVersion(v string) string {
	if _, rest, ok := strings.Cut(v, ":"); ok {
		v = rest
	}
	v, _, _ = strings.Cut(v, "-")
	return v
}
