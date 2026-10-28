package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swbilisim/FootOps/internal/sites"
)

// ImageInfo is what a new site's form can learn from its image before the site exists:
// the image is pulled (creating the site would pull it anyway) and inspected. It never
// holds the image's environment values, which may be secrets, except PORT and HOSTNAME.
type ImageInfo struct {
	Image       string   `json:"image"`
	ID          string   `json:"id"`
	Size        int64    `json:"size"`
	Platform    string   `json:"platform"`        // the image's, e.g. linux/amd64
	Server      string   `json:"server_platform"` // this server's
	User        string   `json:"user,omitempty"`  // USER as written in the image
	UID         int      `json:"uid"`             // -1: unknown
	GID         int      `json:"gid"`
	Ports       []int    `json:"ports,omitempty"` // EXPOSEd TCP ports
	Volumes     []string `json:"volumes,omitempty"`
	EnvPort     string   `json:"env_port,omitempty"`
	EnvHostname string   `json:"env_hostname,omitempty"`
}

// Bounds on what an image's metadata can put into a job's result.
const (
	maxInfoList = 20
	maxInfoText = 200
)

// SuggestPort is the port the app most likely listens on: an EXPOSEd port (80/3000/8080
// first if there are several), then the image's PORT variable; 0 if neither says.
func (i ImageInfo) SuggestPort() int {
	for _, p := range i.Ports {
		if len(i.Ports) == 1 || p == 3000 || p == 8080 || p == 80 {
			return p
		}
	}
	if p, err := strconv.Atoi(i.EnvPort); err == nil && p > 0 && p < 65536 {
		return p
	}
	if len(i.Ports) > 0 {
		return i.Ports[0]
	}
	return 0
}

// SuggestMount is the image's only VOLUME, the natural place for uploads.
func (i ImageInfo) SuggestMount() string {
	if len(i.Volumes) == 1 {
		return i.Volumes[0]
	}
	return ""
}

// PlatformOK reports whether the image runs on this server.
func (i ImageInfo) PlatformOK() bool { return i.Platform == "" || i.Platform == i.Server }

// imageInspect is the part of `docker image inspect` the check reads.
type imageInspect struct {
	ID           string `json:"Id"`
	Size         int64  `json:"Size"`
	Os           string `json:"Os"`
	Architecture string `json:"Architecture"`
	Config       struct {
		User         string              `json:"User"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		Env          []string            `json:"Env"`
		Volumes      map[string]struct{} `json:"Volumes"`
	} `json:"Config"`
}

// serverPlatform is what this server's containers run as: Linux on the agent's own
// architecture (FootOps runs where Docker runs).
func serverPlatform() string { return "linux/" + runtime.GOARCH }

// parseImageInspect reads the JSON array `docker image inspect` prints. Lists and texts
// are bounded: the result is kept with the job.
func parseImageInspect(out, image string) (ImageInfo, error) {
	var list []imageInspect
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list) != 1 {
		return ImageInfo{}, errors.New("docker image inspect anlaşılamadı")
	}
	in := list[0]
	info := ImageInfo{Image: image, ID: strings.TrimPrefix(in.ID, "sha256:"), Size: in.Size,
		Server: serverPlatform(), User: clipMsg(in.Config.User, 64), UID: -1, GID: -1}
	if len(info.ID) > 12 {
		info.ID = info.ID[:12]
	}
	if in.Os != "" && in.Architecture != "" {
		info.Platform = clipMsg(in.Os+"/"+in.Architecture, 64)
	}
	for p := range in.Config.ExposedPorts {
		n, proto, _ := strings.Cut(p, "/")
		if v, err := strconv.Atoi(n); err == nil && (proto == "" || proto == "tcp") && v > 0 && v < 65536 {
			info.Ports = append(info.Ports, v)
		}
	}
	sort.Ints(info.Ports)
	for v := range in.Config.Volumes {
		info.Volumes = append(info.Volumes, clipMsg(v, maxInfoText))
	}
	sort.Strings(info.Volumes)
	if len(info.Ports) > maxInfoList {
		info.Ports = info.Ports[:maxInfoList]
	}
	if len(info.Volumes) > maxInfoList {
		info.Volumes = info.Volumes[:maxInfoList]
	}
	for _, kv := range in.Config.Env {
		switch k, v, _ := strings.Cut(kv, "="); k {
		case "PORT":
			info.EnvPort = clipMsg(v, 16)
		case "HOSTNAME":
			info.EnvHostname = clipMsg(v, 64)
		}
	}
	return info, nil
}

// Pull errors in words an operator can act on. docker's own last line stays at the end.
var (
	// Registries answer "denied" both for a private image without a login and for one that
	// does not exist (so as not to reveal which), hence one message for both.
	errImageAuth     = errors.New("imaja erişilemedi: özel bir imajsa bu sunucuda o kayıt defterine giriş yok ya da token'ın bu pakete okuma izni yok (İmaj girişleri sayfası); adını ve etiketini de kontrol et")
	errImageNotFound = errors.New("imaj ya da etiketi bulunamadı: adı ve etiketi doğru mu (ör. …:main)?")
	errImageNetwork  = errors.New("kayıt defterine ulaşılamadı (ağ ya da kayıt defteri sorunu); biraz sonra tekrar dene")
)

func explainPull(err error) error {
	if err == nil {
		return nil
	}
	m := strings.ToLower(err.Error())
	var why error
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("no matching manifest"):
		why = fmt.Errorf("imaj bu sunucunun mimarisi (%s) için derlenmemiş", serverPlatform())
	case has("unauthorized", "denied", "authentication required", "authorization failed", "no basic auth credentials",
		"repository does not exist"):
		why = errImageAuth
	case has("manifest unknown", "not found"):
		why = errImageNotFound
	case has("timeout", "dial tcp", "no such host", "connection refused", "connection reset"):
		why = errImageNetwork
	default:
		return err
	}
	return fmt.Errorf("%w (%s)", why, clipMsg(lastLine(err.Error()), 300))
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// inspectImage reads an image already present on the server.
func (o *SiteOps) inspectImage(ctx context.Context, image string) (ImageInfo, error) {
	out, err := o.Run.Run(ctx, "docker", "image", "inspect", image)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("imaj okunamadı: %w", err)
	}
	return parseImageInspect(out, image)
}

// errPlatform refuses an image built for another machine (e.g. arm64 from an Apple
// Silicon Mac): it would start and crash with "exec format error".
func errPlatform(i ImageInfo) error {
	return fmt.Errorf("imaj %s için derlenmiş, bu sunucu %s: GitHub Actions'ta platform %s olarak derle", i.Platform, i.Server, i.Server)
}

// CheckImage pulls an image and reports what a site needs to know about it.
func (o *SiteOps) CheckImage(ctx context.Context, image string, t *Task) (ImageInfo, error) {
	image = strings.TrimSpace(image)
	if !sites.ValidImage(image) {
		return ImageInfo{}, errors.New("imaj adı geçersiz (örn. ghcr.io/kullanici/site:main)")
	}
	t.Step("İmaj indiriliyor (ya da güncel mi diye bakılıyor): " + image)
	start := time.Now()
	if _, err := o.killer().Run(ctx, "docker", "image", "pull", "--quiet", image); err != nil {
		if ctx.Err() != nil {
			return ImageInfo{}, ctx.Err()
		}
		return ImageInfo{}, explainPull(err)
	}
	info, err := o.inspectImage(ctx, image)
	if err != nil {
		return info, err
	}
	t.Step(fmt.Sprintf("✓ İmaj hazır: %s, %s, %s (%s)", humanBytes(info.Size), info.Platform, info.ID, took(time.Since(start))))
	if !info.PlatformOK() {
		return info, errPlatform(info)
	}
	if uid, gid, err := imageIDs(ctx, o.killer(), image); err == nil {
		info.UID, info.GID = uid, gid
	}
	switch {
	case info.UID == 0:
		t.Step("ℹ Uygulama container'da root olarak çalışıyor (yetkileri yine kısıtlı; kalıcı klasör root'a ait olur)")
	case info.UID > 0:
		t.Step(fmt.Sprintf("✓ Kullanıcı: %s (uid %d, gid %d)", orDash(info.User), info.UID, info.GID))
	default:
		t.Step("ℹ İmajın kullanıcısı okunamadı (içinde id komutu yok); kalıcı klasör imajın USER satırına göre hazırlanır")
	}
	if p := info.SuggestPort(); p > 0 {
		from := "EXPOSE"
		if len(info.Ports) == 0 {
			from = "imajdaki PORT değişkeni"
		}
		t.Step(fmt.Sprintf("✓ Port önerisi: %d (%s)", p, from))
	} else {
		t.Step("ℹ İmaj bir port bildirmiyor (EXPOSE yok); uygulamanın dinlediği portu formda yaz")
	}
	if len(info.Volumes) > 0 {
		t.Step("ℹ İmajın veri klasörleri (VOLUME): " + strings.Join(info.Volumes, ", "))
	}
	if h := info.EnvHostname; h == "localhost" || h == "127.0.0.1" {
		t.Step("⚠ İmajda HOSTNAME=" + h + ": uygulama yalnız container içinden erişilebilir dinler; ortam değişkeninde HOSTNAME=0.0.0.0 ver")
	}
	return info, nil
}
