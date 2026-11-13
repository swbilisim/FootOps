# FootOps

[English](README.md) · **Türkçe**

Go ile yazılmış, hafif ve güvenliği önde tutan, kendi sunucuna kurduğun bir sunucu paneli.

Tek komut yeni bir Ubuntu sunucuyu sağlamlaştırılmış bir Docker sunucusuna çevirir (swap,
SSH sertleştirme, güvenlik duvarı, Docker, Caddy, MongoDB) ve siteleri eklemek, dağıtmak,
yedeklemek ve izlemek için küçük bir web paneli kurar.

> **Durum:** Ekim 2026'dan beri geliştiricinin canlı sunucularında çalışıyor; değişiklikler
> sürüyor. Kurulum ve panel Türkçe.

## Başlarken

1. **[FootOps'u kur](docs/INSTALL-TR.md)**: yeni bir Ubuntu 22.04 ya da 24.04 sunucuya.
   Cloudflare Tunnel ve Access, imzalı sürümü indirip doğrulamak, kurulumun soruları,
   paneldeki ilk adımlar.
2. **[Site yayınla](docs/NEW-SITE-TR.md):** panelin **Projeni hazırla** sayfası projene bir
   Dockerfile ve bir GitHub Actions workflow'u verir; imajı GitHub derler, panel çalıştırır.

## Kısaca tasarım

- **Tek dosya.** `footops` hem kurulum hem panel. Sunucuda Docker dışında hiçbir şeye ihtiyaç duymaz.
- **Sunucu başına.** Her sunucu kendi FootOps'unu çalıştırır; merkezî bir yönetim noktası yoktur.
- **İnternete hiç açılmaz.** Panel yalnız `127.0.0.1`'i dinler; ona Cloudflare Tunnel + Cloudflare Access üzerinden, sonra kendi şifre + TOTP girişiyle ulaşılır. Acil durum yolu SSH tüneli.
- **Yetki ayrımı.** `footops-web` (arayüz) yetkisiz çalışır ve `footops-agent`'tan yalnız belli, doğrulanmış işleri isteyebilir. Arayüzdeki bir hata keyfî root komutlarına dönüşemez.
- **Uygulama başına port yok.** Yalnız Caddy dinler, 443'te (siteler Cloudflare arkasında yalnız https). Uygulamalar proje başına Docker ağlarına katılır ve container adıyla bulunur; tutulacak bir port tablosu yoktur. Caddy ziyaretçinin adresini `X-Real-IP`'de verir (Cloudflare'in `CF-Connecting-IP`'sinden, ziyaretçinin gönderdiğinden asla değil) ve istek gövdesini 100 MiB'la sınırlar: Cloudflare'in Free ve Pro planlarındaki sınır.
- **Diskteki dosyalar esas.** Her site `/srv/<grup>/<proje>/` altında sıradan bir Compose dosyası (`compose.yaml`, ortam değişkenleri içinde, 0600) ve `footops.json` olarak durur. FootOps kaldırılsa bile her şey çalışmaya devam eder ve elle yönetilebilir.
- **Çekerek dağıtım.** GitHub Actions imajı derleyip GHCR'ye gönderir; panelin **Yeniden dağıt**'ı onu çeker. CI sunucuya erişecek hiçbir bilgi tutmaz.
- **İmzalı sürümler.** Sunucu yalnız manifesti güvendiği bir anahtarla imzalanmış ve kendisinden yeni bir sürümü kurar; CI her sürümü kendi etiketinden yeniden derleyip baytları karşılaştırır.

## Panel

Kurulum `footops` sistem kullanıcısını açar (uid 800-899; container imajlarının dosya
yazdığı numaralarla hiç çakışmaz), ikiliyi `/usr/local/bin/footops`'a koyar, yöneticinin
doğrulama uygulamasını kaydeder (terminalde QR kodu, canlı bir kodla onaylanır) ve iki servis
başlatır:

| Servis | Kimle çalışır | Ne yapabilir |
|---|---|---|
| `footops-agent` | root | `/run/footops/agent.sock` üzerinde sabit, doğrulanmış bir API: genel durum, ölçüm geçmişi, uyarılar, container logları, yeniden başlatma, siteler, yedekler, denetim kaydı. Yalnız root'tan ve `footops` uid'inden bağlantı kabul eder (`SO_PEERCRED` ile denetlenir). |
| `footops-web` | `footops`; yetkisiz, seccomp, `ProtectSystem=strict` | Giriş ve sayfalar. `/srv`'yi, sırları, Docker'ın soketini ve verisini, denetim kaydını göremez; her şeyi agent'a sorar. |

Erişim:

- **SSH tüneli (her zaman):** `ssh -L 7701:127.0.0.1:7701 <kullanıcı>@<sunucu-ip>`, sonra
  `http://localhost:7701`. Yalnız `localhost` adlarına cevap verilir (DNS rebinding'e karşı).
- **Cloudflare Tunnel (isteğe bağlı):** `cloudflared` çekirdek yığında çalışır ve panelin
  adresini `127.0.0.1:7700`'e iletir. O dinleyici yalnız yapılandırılan takım ve uygulama
  için geçerli bir Cloudflare Access JWT'si taşıyan istekleri kabul eder (RS256; yayıncı,
  hedef kitle ve süre denetlenir; anahtarlar Cloudflare'den alınır). Böylece silinmiş ya da
  yanlış yapılandırılmış bir Access uygulaması giriş sayfasını açığa çıkarmaz.

Giriş: kullanıcı adı + argon2id şifre + TOTP (RFC 6238, kodlar yeniden kullanılamaz) ya da
tek kullanımlık bir kurtarma kodu. 5 hatalı giriş hesabı kilitler (5 dk, her seferinde iki
katı); girişler IP başına ve toplamda da sınırlıdır. Oturumlar sunucuda tutulur, dinleyiciye
ve Cloudflare kimliğine bağlıdır, 30 dakika hareketsizlikte ve en geç 12 saatte biter. Her
formda CSRF anahtarı var, siteler arası istekler reddedilir. Çalışan betikler yalnız panelin
kendi küçük dosyaları (canlı güncelleme, kaldığı yerden süren yükleme, `.env` dosyasını
tarayıcıda okuma, kopyala düğmeleri): satır içi betik ya da stil yok, üçüncü taraf yok.
Content-Security-Policy: `default-src 'none'; script-src 'self'; connect-src 'self';
style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri
'none'`. Sayfalar betikler olmadan da çalışır, düz yenilemeye döner.

Panelin gösterdikleri:

- **Başlarken:** yeni bir sunucunun ana sayfası hâlâ neyin eksik olduğunu listeler
  (sertifika, imaj girişi, ilk site, yedekleme, Telegram); durumdan okunur, her biri
  bağlantılı. Sertifika, bir site ve yedekleme tamamlanınca liste kaybolur.
- **Sunucu ve container'lar:** CPU, RAM, swap, disk, yük ve container başına CPU, bellek
  (sınırına göre), yeniden başlamalar ve sağlık; 10 saniyede bir kendiliğinden güncellenir.
  Bir container'ın CPU'su ve belleği, `docker stats`'in hesapladığı gibi cgroup
  dosyalarından okunur (CPU: 10 saniyede bir çekirdeğin payı); dockerd'nin stats isteği
  (container başına birkaç milisaniye ve bir iki saniye bekleme) yalnız bu dosyaların
  okunamadığı yerde kullanılır. Arka plan yenilemeleri hareket sayılmaz; açık bir sekme de
  30 dakika hareketsizlikte çıkış yapar.
- **Geçmiş:** agent bir gün için dakikada bir, bir hafta için saatte bir nokta tutar
  (`/var/lib/footops/metrics.json`; 5 dakikada bir ve kapanırken kaydedilir). Sunucunun ve
  her container'ın grafikleri "belleği hangi container büyüttü" sorusunu cevaplar. Grafikler
  sunucuda SVG olarak çizilir; üstüne gelince değerler görünür, her grafiğin altındaki
  tablo şimdiki, ortalama ve en yüksek değeri verir.
- **Uyarılar:** disk ≥ %80, RAM ≥ %90, swap ≥ %50, 5 dakika boyunca CPU ≥ %90; çalışmayan,
  sağlıksız, sürekli yeniden başlayan, belleği yetmeyip öldürülen ya da bellek sınırına
  yaklaşan bir FootOps container'ı; 15 dakika içinde yeniden başlamalar; 30 gün içinde
  bitecek sertifikalar; başarısız ya da gecikmiş yedekler. Kısa sıçramalar (30 saniyenin
  altı) sayılmaz. Uyarılar menüde sayı, ana sayfada liste olarak görünür.
- **Kontrol:** sunucunun FootOps standardına göre denetimi — bekleyen güncellemeler ve
  yeniden başlatmalar, otomatik güvenlik güncellemeleri, saat, açık portlar, kullanılmayan
  imajlar, sürümler —; her gün ve istenince (`footops doctor`).
- **Güncelleme:** FootOps günde bir yeni imzalı sürüme bakar, neyin değişeceğini ve
  sitelerin bir an durup durmayacağını söyler, istenince kurar; açılmayan bir sürüm
  kendiliğinden geri alınır.
- **Telegram (isteğe bağlı, Bildirimler):** uyarılar bağlı bir sohbete açılınca,
  kötüleşince ve düzelince gider. O sohbette onu bağlayan kişi **`/durum`** (CPU, yük,
  bellek, swap, disk, açık kalma süresi, siteler, son yedek ve açık uyarılar; mesajı yerinde
  güncelleyen bir **Yenile** düğmesiyle) ve **`/siteler`** (süresi olan ve parktaki siteler;
  süre verme, parka alma ve geri açma düğmeleriyle, bkz.
  [Süre ve park](#süre-ve-park)) komutlarını kullanabilir. Başka kimseye cevap verilmez,
  komutlar sınırlı ve denetim kaydına yazılır, Telegram'dan hiçbir şey silinemez. **Her
  sunucuya ayrı bot aç:** Telegram bir botun mesajlarını aynı anda yalnız bir okuyucuya
  okutur; ikincisi reddedilir ve panel bunu 10 dakika sonra söyler. Okumanın boşta maliyeti
  dakikada yaklaşık bir uzun bekleyen istek.
- **Dış nöbetçi (isteğe bağlı):** sitelere bir ziyaretçi gibi giden ve her sunucunun
  yedeklerinin yanında tuttuğu durum dosyasını okuyan bir Cloudflare Worker
  ([`worker/`](worker/README.md)); bir site cevap vermeyi bırakınca ya da bir sunucudan
  haber kesilince bir Telegram sohbetine yazar (yalnız yazan, kendine ait bir botla). Panel
  onun gördüğünü gösterir, sessiz kalınca uyarır.
- **Loglar:** son 200-2000 satır, arama (vurgulu), uyarı / hata süzgeci, canlı izleme.

Her giriş, çıkış, yeniden başlatma ve kurtarma komutu `/var/lib/footops/audit.jsonl`'a
yazılır ve panelde görünür.

## Siteler

Site, bir ya da birkaç alan adında sunulan bir container imajıdır. Adım adım nasıl
yayınlanacağı: [docs/NEW-SITE-TR.md](docs/NEW-SITE-TR.md). Kısaca, panelden (**Siteler →
Yeni site**):

1. Önce **Sertifikalar**: alan adının Cloudflare Origin CA sertifikasını ve anahtarını
   yapıştır (`*.ornek.com` + `ornek.com`, 15 yıl). Panel anahtarın tuttuğunu denetler ve
   hangi adları kapsadığını gösterir.
2. **Yeni site**: ad, grup (`/srv/<grup>/<ad>`), alan adları, imaj (ör. GHCR'den),
   uygulama portu, bellek sınırı, en büyük istek, isteğe bağlı kalıcı klasör, isteğe bağlı
   MongoDB veritabanı, ortam değişkenleri. İmaj önce kontrol edilir (indirilebiliyor mu,
   bu sunucu için mi derlenmiş, portu, kullanıcısı ve bildirdiği yükleme klasörü).

**Projeni hazırla**, bir projenin böyle bir imaj olması için gerekenleri yazar: bir
Dockerfile (Next.js, Node.js ya da statik site; yetkisiz kullanıcı, salt okunur kod,
`footops.data-dir` etiketinde bildirilen yükleme klasörü), `.env*`'i dışarıda tutan bir
`.dockerignore` ve imajı sunucunun mimarisi için derleyip GHCR'ye gönderen bir GitHub
Actions workflow'u. Şablonlar [`internal/scaffold/files`](internal/scaffold/files)'ta.

**Gruplar** `/srv` altındaki klasörlerdir (`core` FootOps'un kendisinin): form var
olanları önerir, yeni bir ad da alır ve grubun klasörünü açar (yalnız root, `0700`). Grup
son sitesiyle gider: o site başka yere taşınınca ya da tamamen silinince boş klasör de
silinir.

**En büyük istek** (MB, 1-100; varsayılan 100, Cloudflare'in Free ve Pro planlarının
geçirdiği): Caddy daha büyük bir isteğe, uygulama fazlasını okumadan `413` ile cevap verir;
gövdeyi belleğe alan bir uygulamaya büyük bir gövde tutturulamaz. Dosya yüklenmeyen bir
site için birkaç MB yeter. Değiştirmek container'a değil yalnız Caddy'ye dokunur.

Agent sonra adımları bir arka plan işi olarak çalıştırır (sayfa ilerlemesini izler); bir
adım başarısız olursa her şeyi geri alır:

| Adım | Ne olur |
|---|---|
| directory | `/srv/<grup>/<ad>/`, içinde `footops.json` (sır yok) ve `compose.yaml` (0600) |
| network | kendine ait bir Docker ağı; Caddy (ve kullanılıyorsa MongoDB) ona bağlanır — siteler birbirine ulaşamaz |
| database | sitenin adında bir MongoDB veritabanı ve yalnız o veritabanında `readWrite` yetkili bir kullanıcı; bağlantı adresi seçilen değişkene gider (varsayılan `MONGODB_URI`). Root şifresi `mongosh`'a stdin'den gider, asla komut satırından değil |
| image | hiçbir şey başlamadan indirilir |
| data | imajın çalıştığı kullanıcıya ait `data/`, verilen yola bağlanır |
| container | yayınlanan port yok, bütün yetkiler kaldırılmış, `no-new-privileges`, bellek ve PID sınırı |
| ready | uygulama portunda bağlantı kabul edene kadar bekler; çökerse hata son log satırlarını gösterir |
| Caddy | eşleşen sertifikayla site bloğu; `caddy reload`'dan önce `caddy validate` |
| check | Caddy üzerinden bir HTTPS isteği uygulamaya ulaşmalı |

**Düzenle**; alan adlarını, imajı, portu, bellek sınırını, en büyük isteği, kalıcı klasörü
ve ortam değişkenlerini değiştirir. Şimdiki değerler tarayıcıya hiç gönderilmez:
değişkenler adıyla eklenir, değiştirilir ya da silinir, denetim kaydı yalnız adları
tutar. Veritabanı değişkenini FootOps yönetir. Site yeni ayarlarla açılmazsa eski dosyalar
geri konur ve eski container yeniden başlatılır.

**Yeniden dağıt** imajı yeniden çeker ve değiştiyse container'ı yeniden oluşturur.
**Siteyi kaldır** (adı yazılarak onaylanır) siteyi yayından kaldırır ve dosyalarını
`/srv/<grup>/_removed/`'a taşır; veritabanı kalır.

Sitenin verisi SSH'siz taşınır: **İçe aktar** bir FootOps arşivini, bir `mongodump`
arşivini ya da dosyaların zip / tar.gz arşivini alır, denetler (tür, sağlamalar, yollar,
bağlantılar, sıkıştırma bombaları; veritabanı geçici bir veritabanına yüklenip sayılır)
ve önizleme gösterir; ancak onaydan sonra değişecek olanın kopyasını alıp uygular.
**Dışa aktar** sitenin veritabanını ve dosyalarını bir FootOps arşivine yazar. Sitenin
veritabanı tarayıcıda gezilip düzenlenebilir (geri alınabilir), kalıcı klasörü bir dosya
yöneticisiyle yönetilir.

### Süre ve park

Bir sitenin **bitiş tarihi** olabilir (site sayfası → **Süre**; varsayılan yok). Bağlı
Telegram sohbeti 7 ve 3 gün önce ve o gün (sunucu saatiyle 10:00) **+1 ay** / **+1 yıl**
düğmeleriyle haber alır; o gün sitenin **parka** alınıp alınmayacağı sorulur. Cevap
gelmezse site açık kalır ve soru her sabah yinelenir: hiçbir site "evet" olmadan yayından
kalkmaz. Her düğme etkisini tam söyleyerek bir kez daha sorar; yalnız o cevabın **Evet**'i,
bir kez ve 5 dakika içinde çalışır; site değiştikten sonra basılan düğme hiçbir şey yapmaz.

**Park** (sohbetten ya da site sayfasından) sitenin yerine tarafsız bir bakım sayfası
koyar — Caddy kendisi cevap verir, `503` ve `Retry-After` ile; nedenini söylemez — sonra
container'ı durdurur (imajıyla birlikte kalır). Veri ve yedekler kalır; site parktayken
zamanlanmış yedekler yalnız sitenin tanımını alır (verisi değişmez; son yedekleri
saklanır). Yeniden dağıtma, düzenleme, grup değiştirme ve yeniden başlatma, site geri
açılana kadar (**Yayına al**, yeni bir bitiş tarihiyle ya da tarihsiz) reddedilir. Yayına
al container'ı başlatır, uygulamayı bekler ve sitenin kendi Caddy bloğunu geri koyar;
uygulama açılmazsa container'ı yeniden durdurur ve bakım sayfasını bırakır. Durum dosyası
parktaki siteyi işaretler; dış nöbetçi ona dokunmaz.

Hiçbir sitenin sahiplenmediği adlara gelen bağlantı kesilir. Caddy ziyaretçinin adresi
için Cloudflare'in adres aralıklarına güvenir (`CF-Connecting-IP`). Cloudflare'de alan
adlarını sunucuya yönlendir (A kaydı, proxy açık) ve SSL kipini **Full (strict)** yap.

Agent her 10 saniyede Caddy'yi ve MongoDB'yi bütün site ağlarına yeniden bağlar; çekirdek
container'ları yeniden oluşturmak kendiliğinden toparlanır.

## Yedekler

Her gece (varsayılan 03:00; yoğun veritabanları ayrıca saatlik) her sitenin veritabanı,
kalıcı klasörü ve tanımı (ortam değişkenleri dahil) ile sunucunun kendi ayarları, sunucuya
ayrılmış bir S3 kovasına (ör. Cloudflare R2) restic'le, sunucuda şifrelenerek gider;
yalnız değişen gönderilir. Panel her sitenin yedeklerini listeler, birini açıp içine
baktırır (veritabanı gezilebilir, dosyaları listelenir), arşiv olarak indirir, veritabanını
ya da dosyalarını yerine geçeceği şeyin kopyasını alarak geri yükler.

Ayda bir, temiz bir gece yedeğinden sonra, bir sitenin en yeni yedeği (siteler sırayla)
zarar vermeyeceği bir yere geri yüklenip denetlenir: veritabanı geçici bir veritabanına
yüklenip mongodump'ın saydığıyla karşılaştırılır, dosyalarından bir örnek canlıdakilerle
karşılaştırılır. Başarısızlık bir uyarıdır (ve bir Telegram mesajı). **Yedekleme → Şimdi
dene** ya da `sudo footops backup trial [site]` hemen çalıştırır.

Kaybolan bir sunucu yedeklerinden geri gelir: kurulum sunucunun ayarlarını ve bütün
siteleri bir **prova**ya geri getirir (eski sunucu hâlâ çalışıyor olabilir diye siteler
dışarıya kapalı; hiçbir şey iki kez olmaz), `sudo footops go-live` onu canlı sunucu
yapar: [docs/INSTALL-TR.md, 7. bölüm](docs/INSTALL-TR.md#7-kaybolan-sunucuyu-geri-getirmek).

## Mac'te: FootOps.app ve `footops-mac`

Sunuculara bağlanmadan nasıl olduklarını gösteren bir menü çubuğu uygulaması: her
sunucunun durum dosyası ve dış nöbetçinin nabzı, aynı salt okunur anahtarla kovalardan
okunur (Mac uyanıkken 5 dakikada bir dört küçük okuma ve menü eski bir okumayla
açılınca). Simgesi yeşil, sarı (bakmaya değer), kırmızı (ziyaretçiler etkileniyor ya da
bir sunucudan haber yok) ya da gridir (bilgi yok). Menü her sunucunun sitelerini nöbetçinin
gördüğüyle listeler, sunucunun panelini tarayıcıda ve SSH'yi Terminal'de açar, istenince
her siteyi Mac'ten ziyaret eder ve nöbetçinin gördüğüyle yan yana koyar. Hiçbir panelle
konuşmaz; bir sunucuyu değiştirecek yolu yoktur.

**Yedekler** penceresi sunucuların yedeklerini açar: bir sunucu, bir site, onun bir yedeği;
sonra parçaları sahibin bir klasörüne (`~/Downloads/<site>-<zaman>`: `db.archive`,
`files/`, `footops.json` ve panelin indirmesindeki gibi sitenin `env`'i; Time Machine
dışında) ya da veritabanı Mac'in kendi MongoDB'sine. Yalnız okur (salt okunur anahtar,
her restic komutu `--no-lock`); yaptığı hiçbir şey bir yedeği değiştiremez ya da
silemez. Açılanlar yaşlarıyla listelenir — bir haftalık olunca menü söyler — ve oradan
silinir; bir sunucunun kurtarma satırını da panoya koyar.

**Arşivler** penceresi üzerine bırakılan bir FootOps arşivini (panelin dışa aktarması, bir
yedeğin indirmesi), bir mongodump arşivini ya da dosya arşivini alır ve bir şey yazmadan
önce panelin içe aktarması gibi denetler (aynı kod: içerikten tür, her sağlama, yollar,
bağlantılar, sıkıştırma bombaları); sonra sahibin bir klasörüne açar ya da veritabanını
Mac'in MongoDB'sine yükleyip sayısını manifestle karşılaştırır. Öbür yönde, Mac'in
MongoDB'sindeki bir veritabanından ve sitenin kalıcı klasörü olacak bir klasörden panelin
içe aktarması için arşiv hazırlar (yeni bir site: veritabanı Mac'ten) ve seçilen sitenin
içe aktarma sayfasını tarayıcıda açar.

`make mac-install` `FootOps.app`'i derler (SwiftUI, içinde Go çekirdeği `footops-mac`, bu
Mac için ad hoc imzalı), Uygulamalar'a koyar, `footops-mac`'i `~/.local/bin`'e bağlar ve
açar; bundan sonra Mac'le birlikte açılır (Ayarlar'dan kapatılır). Çekirdek Terminal'de de
çalışır (`brew install restic` gerekir; MongoDB'ye yüklemek için ayrıca
`mongodb-database-tools`):

```bash
footops-mac durum                          # sunucular, menüde göründüğü gibi
footops-mac yokla                          # her siteyi şimdi bu Mac'ten ziyaret et
footops-mac ayar                           # her sunucunun paneli, SSH hedefi ve kovası
footops-mac yedek liste h1                 # siteleri ve en yeni yedekleri
footops-mac yedek liste h1 magaza          # bir sitenin yedekleri
footops-mac yedek ac h1 magaza "2026-11-07 04:00"   # o dakika başlayan yedek → ~/Downloads
footops-mac yedek mongo h1 magaza          # veritabanı yerel MongoDB'ye (magaza_yedek_<zaman>)
footops-mac yedek acilanlar                # bu Mac'te açılanlar; yedek sil ile silinir
footops-mac yedek kurtarma h1 | pbcopy     # kurulum için kurtarma satırı
footops-mac arsiv dogrula x.footops.tar    # arşivi panelin içe aktarması gibi denetle
footops-mac arsiv ac x.footops.tar --mongo # ~/Downloads/<site>-<zaman>'a, veritabanı MongoDB'ye de
footops-mac arsiv olustur --db magaza --dosyalar ./uploads   # → ~/Downloads/magaza-<zaman>.footops.tar
```

Ayarlar (sır yok) `~/Library/Application Support/FootOps/ayarlar.json`'da; onları
uygulamanın Ayarlar penceresi `footops-mac ayar yaz` ile yazar. Sırlar Anahtar
Zinciri'nde, servis `footops-yedek`: `r2-hesap` (R2 hesabı), `r2-mac-erisim` ve
`r2-mac-gizli` (salt okunur anahtar), `restic-<sunucu>` (her deponun şifresi), isteğe bağlı
`kova-<sunucu>`.

## Komutlar

| Komut | Ne yapar |
|---|---|
| `footops install` | Sunucuyu hazırlar ve FootOps'u kurar (etkileşimli, root); bir sunucuyu yedeklerinden de geri getirir. Bkz. [docs/INSTALL-TR.md](docs/INSTALL-TR.md) |
| `footops go-live` | Yedekten geri getirilen sunucuyu canlı sunucu yapar (root) |
| `footops self-update` | Son sürümü indirir, doğrular ve kurar (`--check` yalnız bakar); panelin **Güncelleme** sayfası da aynısını yapar |
| `footops doctor` | Sunucuyu FootOps standardına göre denetler (panelin **Kontrol** sayfası) |
| `footops reset-password` | Yeni panel şifresi; kilidi kaldırır, açık panel oturumlarını kapatır (SSH'den root) |
| `footops reset-2fa` | Yeni doğrulama uygulaması kaydı, yeni kurtarma kodları; açık panel oturumlarını kapatır (SSH'den root) |
| `footops rotate-mongo-root` | Yeni MongoDB root şifresi; sır dosyası değişmeden önce denenir (root) |
| `footops agent` / `footops web` | Servisler (systemd başlatır) |
| `footops version` | Sürümü yazar |

## Geliştirme

```bash
make check                  # go vet + testler (yarış denetimiyle)
make build                  # yerel ikili
make linux                  # dist/footops_linux_{amd64,arm64} + SHA256SUMS
make mac                    # dist/FootOps.app (Mac'te: Swift ve Go); swift test --package-path mac/app
test/integration/run.sh     # içinde Docker olan systemd'li bir container'da gerçek kurulum, sonra senaryolar
```

**Sürümler** geliştiricinin Mac'inde derlenir ve imzalanır (`footops-mac yayin yap vX.Y.Z`;
gizli anahtar Anahtar Zinciri'nden hiç çıkmaz): bir etiket ve iki ikili,
`footops-release.json` (her dosyanın SHA-256'sı ve boyutu, sürüm, kısa notlar) ve onun
ed25519 imzasıyla bir GitHub sürümü. **Release check** iş akışı etiketi
`make release-build` ile yeniden derler ve baytları imzalı manifestle karşılaştırır;
böylece herkes bir sürümün bu reponun kodu olduğunu görebilir. Sunucular bir sürümü yalnız
imzası [`internal/release/keys.go`](internal/release/keys.go)'daki anahtarlardan biriyse
ve kendilerinden yeniyse kurar.

**Sunucuya özgü veri asla commit'lenmez** (IP adresleri, alan adları, sırlar). Hepsi
sunucunun `/etc/footops` altındaki kendi yapılandırmasında durur.

## Lisans

MIT
