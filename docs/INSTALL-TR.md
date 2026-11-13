# FootOps kurulumu

[English](INSTALL.md) · **Türkçe**

Bu belge, yeni kurulmuş bir Ubuntu sunucuyu ilk sitesine hazır, çalışan bir FootOps
paneline götürür. Yaklaşık bir saat sürer; çoğu indirmeleri beklemekle geçer. Kurulum ve
panel Türkçedir; aşağıda her ad panelde göründüğü gibi yazılı.

Sonra: [Site yayınlama](NEW-SITE-TR.md).

## Gerekenler

| | |
|---|---|
| **Bir sunucu** | Yeni kurulmuş Ubuntu 22.04 ya da 24.04, amd64 ya da arm64, en az 1 GB RAM ve 8 GB boş disk. 2 çekirdek + 4 GB RAM on kadar küçük siteyi taşır. Sunucuya SSH ile root olarak girebilmelisin (anahtarla ya da sağlayıcının verdiği şifreyle). |
| **Cloudflare'de bir alan adı** | Ücretsiz plan yeter. Panelin ve sitelerin adresleri orada olur, ziyaretçiler Cloudflare'in arkasından gelir. Alan adının nameserver'ları Cloudflare'i göstermeli, alan **Active** olmalı. |
| **Bir SSH anahtarı** | Bilgisayarında: `ssh-keygen -t ed25519` (macOS, Linux, Windows 10 ve sonrası). Root girişi kapanır; sunucuya bundan sonra bu anahtarla girersin. |
| **Bir doğrulama uygulaması** | Telefonunda (Google Authenticator, Aegis, 2FAS, Bitwarden, 1Password…): panel her girişte kodunu sorar. |
| **Bir parola yöneticisi** | Kurulum panel şifresini, kurtarma kodlarını ve MongoDB root şifresini bir kez gösterir. |
| İsteğe bağlı | Yedek için bir Cloudflare R2 kovası (ya da S3 uyumlu başka bir depo), uyarılar için Telegram, sitelerin kodu ve imajları için bir GitHub hesabı. |

Sağlayıcının kendi güvenlik duvarı varsa (ör. Hetzner Cloud Firewall) 22 ve 443
portlarına gelen bağlantıya izin ver. Gerisini FootOps sunucunun kendisinde kapatır.

## 1. Cloudflare'i hazırla

Kurulum bunları soracak; önceden hazırla (yaklaşık 15 dakika). Cloudflare menülerin adını
ara sıra değiştirir; parçalar aynı kalır: e-posta adresinle bir Access uygulaması, bir
tünel ve panelin adresinden `http://127.0.0.1:7700`'e bir yönlendirme.

**Panelin adresini seç:** alan adının tek seviye bir alt alanı, ör. `panel.ornek.com`.
`panel.sunucu1.ornek.com` olmaz: Cloudflare'in ücretsiz sertifikası tek seviyeyi kapsar.
Bunun için DNS kaydı ekleme; tünel kendi kaydını ekler.

### 1.1 Zero Trust

[one.dash.cloudflare.com](https://one.dash.cloudflare.com)'u aç (Cloudflare One, eski adıyla
Zero Trust). İlk seferde bir **takım adı** (team name) ve **Free** planı seç (Cloudflare
ücretsiz plan için de ödeme yöntemi isteyebilir). Takım alanın
`<takım>.cloudflareaccess.com` olur; sonra **Settings** altında da görünür. Kurulum takım
adını sorar.

### 1.2 Access uygulaması: panele kim girebilir

**Access** → **Applications** → **Add an application** → **Self-hosted**:

1. Ad: ör. `FootOps sunucu1`. Oturum süresi istediğin gibi (ör. 24 saat).
2. Public hostname: alt alan `panel`, alan adı `ornek.com` (panelin adresi).
3. Policy: eylem **Allow**, Include → **Emails** → e-posta adresin. Yalnız bu sunucuyu
   yönetecek kişileri ekle.
4. Giriş yöntemi: **One-time PIN** (e-postayla gelen kod) zaten açıktır.
5. Kaydet, uygulamayı yeniden aç ve **Application Audience (AUD) Tag** değerini kopyala:
   64 karakterlik onaltılık bir değer; uygulamanın genel bilgilerinde ya da ek ayarlarında.

Artık panele ulaşmadan önce Cloudflare izinli bir e-posta adresi sorar. FootOps da her
istekte Cloudflare'in imzasına bakar: Cloudflare'i atlayıp gelen istek reddedilir.

### 1.3 Tünel: Cloudflare panele nasıl ulaşır

**Networks** → **Tunnels** → **Create a tunnel** → **Cloudflared**:

1. Ad: ör. sunucunun kodu (`sunucu1`). Kaydet.
2. Sonraki sayfa bağlayıcıyı kuran komutları gösterir (Debian, Docker…); hepsinde tünelin
   token'ı var. Komutlardan birini tamamen kopyala. **Çalıştırma:** `cloudflared`'i FootOps
   kendisi çalıştırır. Token gizlidir; elinde olan senin tünelini çalıştırabilir.
3. Sonra bir **public hostname** (ya da *published application route*) ekle: alt alan
   `panel`, alan adı `ornek.com`, yol boş, servis türü **HTTP**, adres `127.0.0.1:7700`.
   Kaydet. DNS kaydını Cloudflare kendisi ekler.

Tünel bağlantıyı sunucudan Cloudflare'e doğru açar; panel için sunucuda port açmak
gerekmez.

> **Tünelsiz** panel yalnız SSH tüneliyle açılır ([4. bölüm](#4-panele-ilk-giriş)).
> Tüneli sonra ekleyebilirsin: kurulumu yeniden çalıştır (`sudo footops install`),
> **Bu cevaplarla devam edilsin mi?** sorusuna `h`, tünel sorusuna evet de.

## 2. FootOps'u indir ve doğrula

Sunucuya root olarak gir (`ssh root@<sunucu-ip>`) ve son sürümü indir:

```bash
ARCH=$(dpkg --print-architecture)    # amd64 ya da arm64
BASE=https://github.com/swbilisim/FootOps/releases/latest/download
curl -fsSLO "$BASE/footops_linux_$ARCH"
curl -fsSLO "$BASE/footops-release.json"
curl -fsSLO "$BASE/footops-release.json.sig"
```

Her sürümün manifesti (`footops-release.json`) dosyalarını SHA-256'larıyla listeler ve
FootOps'un sürüm anahtarıyla imzalıdır; anahtarın gizli yarısı geliştiricinin
bilgisayarından hiç çıkmaz. Önce imzayı denetle:

```bash
cat > footops-release.pem <<'EOF'
-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAyBME5onXmZME6cI+mM/t967k2m85RP0XzNzeeQTWV9k=
-----END PUBLIC KEY-----
EOF
cut -d' ' -f3 footops-release.json.sig | base64 -d > footops-release.sig
openssl pkeyutl -verify -pubin -inkey footops-release.pem -rawin \
  -in footops-release.json -sigfile footops-release.sig
```

`Signature Verified Successfully` yazmalı. Sonra ikiliyi manifestle karşılaştır:

```bash
python3 -c "import json; print(json.load(open('footops-release.json'))['files']['footops_linux_$ARCH']['sha256'] + '  footops_linux_$ARCH')" | sha256sum -c
```

`footops_linux_amd64: OK` (ya da `arm64`) yazmalı. İkisinden biri tutmazsa dur: indirilen
dosya yayımlanan değil.

Aynı anahtar FootOps'un içinde de var (`internal/release/keys.go`); her sunucu her
güncellemeyi onunla denetler. Bu belgeyi
[github.com/swbilisim/FootOps](https://github.com/swbilisim/FootOps) dışında bir yerde
okuyorsan anahtarı oradaki dosyayla karşılaştır. Ayrıca her sürüm, reponun **Release
check** iş akışında kendi etiketinden yeniden derlenir ve baytlar imzalı manifestle
karşılaştırılır.

İnternetten gelen bir betiği doğrudan kabuğa verme: indir, doğrula, sonra çalıştır.

## 3. Kurulumu çalıştır

```bash
chmod +x footops_linux_$ARCH
./footops_linux_$ARCH install
```

(Root olarak. Başka bir hesaptan: `sudo ./footops_linux_$ARCH install`. Sonuna
`--dry-run` eklersen hiçbir şeyi değiştirmeden neyin değişeceğini gösterir.)

Kurulum sorularını terminalde sorar; bu yüzden senden önce birinin ulaşabileceği bir
kurulum sayfası yoktur. Köşeli parantezdeki değer varsayılandır, Enter onu kabul eder.
Evet / hayır sorularına `e` ya da `h` yazılır; büyük harf olan varsayılandır.

| Soru | Ne yazılır |
|---|---|
| Bu sunucu bir yedekten mi kurulacak? | Enter (hayır). Evet yalnız kaybolan bir sunucuyu geri getirmek için: [7. bölüm](#7-kaybolan-sunucuyu-geri-getirmek). |
| Sunucu kodu | Kısa bir ad, 1-16 harf, rakam ya da tire, ör. `S1`. Panelin başlığında, Telegram mesajlarında (`[S1] …`) ve yedeklerde görünür; sonradan değiştirme. |
| Hostname | Enter (kodun küçük harflisi). |
| Saat dilimi | ör. `Europe/Istanbul`. Gece yedeği (03:00), yeniden başlatma saati ve hatırlatmalar buna göredir. |
| Kullanıcı adı | Sunucudaki hesabın, ör. `ali`: küçük harf; `root`, `admin`, `ubuntu` olmaz. Sunucuya ve panele bununla girersin. |
| root kullanıcısında N SSH anahtarı bulundu … eklensin mi? | Anahtarlar seninse `e`; sonra başka bir public key varsa yapıştır, yoksa Enter. |
| SSH public key(ler)ini yapıştır | root'ta anahtar yoksa ya da bir önceki soruya `h` dediysen sorulur: bilgisayarındaki `~/.ssh/id_ed25519.pub` dosyasının içeriği, sonra boş bir satır. |
| sudo şifre istesin mi? | Enter (evet, önerilen: SSH anahtarın çalınsa bile root için ayrıca bu şifre gerekir). Sonra sudo şifresi iki kez (en az 12 karakter); parola yöneticine kaydet. |
| Swap boyutu (GB) (önerilen) | Enter. |
| MongoDB önbelleği (GB) (önerilen) | Enter. |
| Panel adresi | `panel.ornek.com` (1. bölüm). |
| Cloudflare Tunnel şimdi kurulsun mu? | Enter (evet), sonra sıradaki üç soru. |
| Tunnel token ya da komutun tamamı | 1.3'te kopyaladığın komutu yapıştır; ekranda görünmez. |
| Zero Trust takım adı | ör. `takimim` (ya da `takimim.cloudflareaccess.com`). |
| Access AUD etiketi | 1.2'deki etiket. |
| Gerekirse sunucu otomatik yeniden başlatılsın mı? | Enter (evet), sonra saat: Enter (04:00, gece yedeğinden bir saat sonra). |
| Bu ayarlarla kuruluma başlansın mı? | Özeti oku, sonra `e`. Burada varsayılan hayır. |

Cevaplar (sırlar hariç) `/etc/footops/install.json`'da saklanır.

### Kurulum sürerken (10-20 dakika)

- **SSH denemesi.** Root ve şifreyle giriş kapatılmadan önce kurulum durur ve yeni hesabı
  **ikinci** bir terminalden denemeni ister (ilk terminali kapatma):
  ```bash
  ssh <kullanıcı>@<sunucu-ip>
  sudo -v
  ```
  İkisi de çalışıyorsa **Yeni kullanıcıyla giriş ve sudo çalıştı mı?** sorusuna `e` yaz.
  Çalışmıyorsa `h`: SSH olduğu gibi kalır ve kurulum durur; anahtarla giriş çalışınca
  yeniden çalıştır (aşağıda). Üst üste deneme: 10 dakikada 5 hatalı giriş adresini bir
  saat engeller (fail2ban).
- **İki adımlı giriş.** Bir QR kodu çıkar: doğrulama uygulamanla okut (ya da altındaki
  anahtarı elle gir), sonra **Uygulamanın gösterdiği 6 haneli kod**'u yaz. Kod tutmazsa
  telefonunun saatinin otomatik ayarlı olduğuna bak.
- **Sonda** 🔑 ile başlayan satırlar **bir kez** gösterilir: panel kullanıcısı ve şifresi,
  on kurtarma kodu (her biri bir kez çalışır, telefon kaybolursa diye) ve MongoDB root
  şifresi. Hemen parola yöneticine kaydet.
- Onların üstünde: panelin adresi, MongoDB'ye nasıl bağlanılacağı ve **Sıradaki adımlar**.
- Varsa **Güvenlik notları**: kurulumun senin yerine değiştirmediği şeyler, düzelten
  komutuyla.
- Güncellemeler yeniden başlatma istiyorsa söyler: her şeyi kaydettikten sonra
  `sudo reboot`.

Her şey `/var/log/footops-install.log`'a yazılır. Kurulum her zaman yeniden
çalıştırılabilir: biten adımlar atlanır, yarıda kalan çalıştırma kaldığı yerden sürer.
Kurulum bitene kadar indirdiğin dosyayı yeniden çalıştır: root olarak
`./footops_linux_amd64 install`, root girişi kapandıktan sonra yönetici kullanıcınla
`sudo /root/footops_linux_amd64 install` (ARM sunucuda `arm64`). Bittikten sonra
`sudo footops install`. Sorular sorulurken Ctrl+C hemen çıkar; sunucu değiştirilmeye başlandıktan sonra ilk
Ctrl+C çalışan komutun (ör. apt) bitmesini bekleyip durur, ikincisi hemen çıkar. SSH
bağlantısı koparsa kurulum devam eder ve soramadığı ilk soruda güvenle durur.

Kurulum bitince FootOps `/usr/local/bin/footops` olur; indirdiğin dosyalar silinebilir.

## 4. Panele ilk giriş

1. `https://panel.ornek.com`'u aç. Cloudflare e-posta adresini sorar ve bir kod gönderir.
2. Sonra FootOps'un kendi girişi: kullanıcı adı (yönetici kullanıcın), kurulumun sonunda
   gösterilen panel şifresi ve doğrulama uygulamasındaki kod. Uygulamanın kodu yerine bir
   kurtarma kodu da olur.

**Tünel yoksa** ya da Cloudflare çalışmıyorsa SSH tüneli her zaman çalışır:

```bash
ssh -L 7701:127.0.0.1:7701 <kullanıcı>@<sunucu-ip>
```

sonra tarayıcıda http://localhost:7701.

Oturum 30 dakika hareketsizlikte, en geç 12 saatte kapanır. 5 hatalı giriş hesabı 5
dakika kilitler, her seferinde iki katı.

Ana sayfa (**Sunucu**) **Başlarken** listesiyle açılır: aşağıdaki adımlar, yapıldıkça
işaretlenir. Sertifika, bir site ve yedekleme tamamlanınca liste kaybolur.

## 5. Sunucuyu panelde hazırla

### 5.1 Sertifika: Sertifikalar

Cloudflare sitelerine HTTPS ile bir Cloudflare Origin sertifikası üzerinden ulaşır; 15 yıl
geçerlidir, sunucu Let's Encrypt'ten hiç sertifika istemez. Alan adı başına tek sertifika
alan adını ve bütün alt alan adlarını kapsar:

1. Cloudflare → alan adın → **SSL/TLS** → **Origin Server** → **Create Certificate**.
2. "Generate private key and CSR with Cloudflare", anahtar türü RSA, adlar `*.ornek.com`
   ve `ornek.com`, süre 15 yıl → **Create**.
3. Panel → **Sertifikalar** → **Origin Certificate** ve **Private Key**'i yapıştır →
   **Yükle**. Cloudflare anahtarı bir daha göstermez; istersen parola yöneticine de kaydet
   (gerekirse her zaman yeni sertifika alınır).
4. Cloudflare → **SSL/TLS** → **Overview**: şifreleme kipi **Full (strict)**.

### 5.2 İmaj girişi: İmaj girişleri (özel imajlar için)

Sunucu sitelerin imajlarını bir kayıt defterinden, genelde GitHub'ınkinden (`ghcr.io`)
indirir. Özel (private) repoların imajları için giriş gerekir; herkese açık olanlar için
gerekmez.

1. Panel → **Siteler** → **İmaj girişleri** → **GitHub'da token oluştur**. GitHub'ın
   sayfası **read:packages** seçili açılır.
2. **Expiration**'ı seç (ör. 1 yıl) → **Generate token** → kopyala.
3. Panelde: kayıt defteri `ghcr.io`, kullanıcı adı = token'ı oluşturan GitHub hesabı,
   token → **Giriş yap ve kaydet**.

Token'ın sahibi imajları okuyabilmeli: imajlar kendi hesabındadır ya da onların sahibi olan
kuruluşun üyesidir. Token'ın bitmesine 14 gün kala panel uyarır.

### 5.3 Yedek: Yedekleme (önerilir)

Her gece 03:00'te her sitenin veritabanı, kalıcı klasörü ve tanımı (ortam değişkenleri
dahil) ile sunucunun kendi ayarları restic'le sunucuya ayrılmış bir kovaya yedeklenir;
sunucudan çıkmadan şifrelenir. Her seferinde yalnız değişen gönderilir.

Cloudflare R2 ile:

1. Cloudflare → **R2** → **Create bucket**: ör. `footops-s1`, her sunucuya ayrı bir kova.
2. R2 → **Manage API Tokens** → **Create Account API token**: izin **Object Read &
   Write**, yalnız o kova → oluştur, sonra **Access Key ID** ve **Secret Access Key**'i
   kopyala (bir kez gösterilir).
3. Uç nokta: R2 → **Overview** → **Account Details** → S3 API:
   `https://<hesap-kimliği>.r2.cloudflarestorage.com`, kova adı olmadan.
4. Panel → **Yedekleme** → **Depoyu bağla**: uç nokta, kova, iki anahtar → **Bağlan ve
   dene**.
5. **Şifreyi göster** deponun şifresini gösterir. **Şifre olmadan yedekler açılamaz**;
   kaybolan bir sunucu yalnız bu şifreyle geri gelir. Bu sunucuya bağlı olmayan iki yere
   kaydet (parola yöneticisi ve bir ikincisi), **Şifreyi iki ayrı yere kaydettim**'i
   işaretle → **Onayla, yedeklemeyi başlat**.

S3 uyumlu başka depolar da (Backblaze B2, AWS S3…) aynı şekilde çalışır. Her sitenin yedek
sıklığı ve saklama süresi kendi satırında (**Ayarlar**). Ayda bir, temiz bir geceden sonra,
bir sitenin en yeni yedeği zarar vermeyeceği bir yere geri yüklenip denetlenir.

### 5.4 Telegram: Bildirimler (isteğe bağlı)

Uyarılar (alınamayan yedek, dolan disk, bitmek üzere olan sertifika, sürekli yeniden
başlayan ya da belleği yetmeyen bir site…) bir Telegram sohbetine gelir; orada `/durum` ve
`/siteler` komutları da cevap verir. Cevap vermeyi bırakan bir siteyi, ziyaretçinin
gördüğü gibi, dış nöbetçi (5.5) bildirir.

1. Telegram'da **@BotFather** → `/newbot` → bir ad ver → token'ı kopyala. **Her sunucuya
   ayrı bot:** Telegram bir botun mesajlarını aynı anda yalnız bir sunucuya okutur.
2. Panel → **Bildirimler** → token'ı yapıştır → **Devam**.
3. Gösterilen `t.me/…` bağlantısını aç ve **Başlat**'a bas (ya da kodu bota gönder) →
   **Gönderdim, bağla**.
4. **Deneme mesajı gönder**.

Komutu yalnız sohbeti bağlayan kişi verebilir; Telegram'dan hiçbir şey silinemez.

### 5.5 Dış nöbetçi (isteğe bağlı)

Sitelerini dışarıdan ziyaret eden ve biri cevap vermeyi bırakınca ya da sunucudan haber
kesilince bir Telegram sohbetine yazan bir Cloudflare Worker:
[worker/README-TR.md](../worker/README-TR.md). Her sunucunun yedek kovasına yazdığı
durum dosyasını okur; bu yüzden önce 5.3 gerekir.

## 6. İlk site

→ [Site yayınlama](NEW-SITE-TR.md): **Siteler** → **Projeni hazırla** projene gereken üç
dosyayı verir, GitHub imajı derler, panel çalıştırır.

## 7. Kaybolan sunucuyu geri getirmek

Yedekler bir sunucuyu yeniden kurmaya yeter: yeni bir sunucu onlardan kurulur.

1. Yeni sunucuda FootOps'u indir ve doğrula (2. bölüm), kurulumu çalıştır ve **Bu sunucu
   bir yedekten mi kurulacak?** sorusuna **evet** de. Eski sunucunun kodunu (yedekler bu
   kodla alındı), her zamanki cevapları ve depoyu ver: uç nokta, kova, kovayı *okuyabilen*
   bir anahtar, deponun şifresi ve hangi yedeğe dönüleceği (varsayılan en yenisi). Mac'te
   FootOps.app depo bilgisini tek seferde yapıştırılacak bir satıra koyar (**Yedekler** →
   **Kurtarma satırını kopyala**).
2. Her zamanki adımlardan sonra (tünel yok, yeni panel hesabı yok: eskisi geri gelir) önce
   sunucunun kendi parçası gelir (sertifikalar, panel girişi ve 2FA'sı, imaj girişleri,
   yedek ayarları), sonra en yeni tam yedekteki her site: tanımı, yeni bir veritabanı
   kullanıcısı, veritabanı ve dosyaları, ardından Caddy ve HTTPS denemesi. Depoya hiçbir
   şey yazılmaz. Geri gelemeyen site geri alınır, diğerleri sürer; kurulumu yeniden
   çalıştırmak yalnız eksik kalanı getirir.
3. Sunucu artık **prova kipinde**: tünel, yedekleme ve Telegram kenarda tutulur, siteler
   dışarıya bağlanamaz; böylece eski sunucu hâlâ çalışıyorsa bir uygulamanın e-postaları ya
   da ödemeleri iki kez olmaz. SSH tüneliyle panele bak, siteleri
   `curl -k --resolve <alan-adı>:443:<yeni-ip> https://<alan-adı>/` ile dene.
4. Eski sunucunun kapalı olduğundan emin olunca: `sudo footops go-live` depoyu yeniden
   bağlar, Telegram'ı, tüneli ve Access'i yerine koyar, siteleri dışarıya açar ve ilk
   yedeği alır. Sonra alan adlarının A kayıtlarını yeni adrese çevir.

## Güncel tutmak

**FootOps.** Sunucu günde bir yeni sürüme bakar ve Telegram sohbetine haber verir. Panel →
**Sunucu** → **FootOps** kartı → **Güncelleme** → **Şimdi denetle** → neyin değişeceğini
ve sitelerin bir an durup durmayacağını oku → **… sürümüne güncelle**. Hiçbir şey
değişmeden önce sürüm, sürüm anahtarıyla denetlenir; yeni sürüm açılmazsa önceki sürüm
kendiliğinden geri gelir, **… sürümüne dön** düğmesi de var. SSH'den:
`sudo footops self-update` (`--check` yalnız bakar).

**İşletim sistemi.** Ubuntu'nun güvenlik güncellemeleri her gün kendiliğinden kurulur;
yeniden başlatma gerekirse sunucu seçilen saatte yeniden başlar. Docker'ın kendi deposundan
gelen paketler bunun dışında: panel → **Sunucu** → **Kontrol** bekleyen güncelleme
gösterdiğinde sakin bir saatte `sudo apt update && sudo apt upgrade`.

## Bir şey ters giderse

| Sorun | Ne yapılır |
|---|---|
| Panel şifresi kayboldu | SSH'den: `sudo footops reset-password` (yeni şifre; açık panel oturumları kapanır) |
| Telefon kayboldu | Bir kurtarma koduyla gir ya da `sudo footops reset-2fa` (yeni QR kodu ve yeni kurtarma kodları) |
| Panel açılmıyor | SSH tüneli (4. bölüm); `sudo systemctl status footops-web footops-agent` |
| SSH ile girilemiyor | Sağlayıcının web konsolu |
| MongoDB root şifresi sızdı | `sudo footops rotate-mongo-root` (sitelerin kendi veritabanı kullanıcıları etkilenmez) |
| Bir site çalışmıyor | Panel → site → **Loglar**; gerekirse **Yeniden başlat** |
| Veri silindi ya da bozuldu | Panel → **Yedekleme** → site → bir yedek → önce **Aç ve incele**, sonra **Geri yükle…** |
| Disk doluyor | Panel → **Sunucu** → **Kontrol** → **Kullanılmayan imajları sil** |
| Sunucu kayboldu | [7. bölüm](#7-kaybolan-sunucuyu-geri-getirmek) |

**Bilgisayarından MongoDB'ye** (Compass, Robo 3T): yönetici kullanıcınla SSH üzerinden
bağlan, sonra `localhost:27017`'ye kullanıcı `root`, kimlik doğrulama veritabanı `admin`
ile. Şifre kurulumun sonunda gösterildi; sunucuda `/etc/footops/secrets/mongo.env`'de
(`sudo grep PASSWORD /etc/footops/secrets/mongo.env`). Port internete hiç açılmaz.

## Kurulum neleri değiştirir

Önce güvenlik adımları çalışır; uzun Docker indirmeleri başlamadan root girişi kapanmış
olur. Her adım bir şeyi değiştirmeden önce şimdiki durumu denetler.

| Adım | Değişiklik |
|---|---|
| packages | `ca-certificates curl gnupg ufw fail2ban python3-systemd unattended-upgrades` |
| upgrade | Bekleyen güncellemeler, yeni paket isteyenler dahil (çekirdek güvenlik düzeltmeleri); hiçbir paketi kaldırmaz |
| hostname | Hostname, yeniden başlatmada cloud-init geri almasın diye ayarıyla |
| timezone | Cevaptaki saat dilimi, ör. `Europe/Istanbul` |
| swap | RAM'e göre (6 GB'a kadar → 2 GB, fazlası → 4 GB), `vm.swappiness=10` |
| unattended-upgrades | Otomatik güvenlik güncellemeleri, istenirse sabit saatte yeniden başlatma |
| admin-user | `sudo` grubunda yönetici kullanıcı (uid 2000 ve üstü, container imajlarının kullandığı numaralardan uzak), SSH anahtarların; sudo şifreli (önerilen) ya da şifresiz |
| firewall | UFW: 22 ve 443 dışında gelen bağlantı yok |
| fail2ban | SSH için sıkı kip (hatalı anahtar girişleri de sayılır): 10 dakikada 5 deneme → bir saat engel |
| ssh-hardening | Root girişi yok, şifreyle giriş yok, yalnız yönetici kullanıcı — **ancak** ikinci terminalden anahtarla girişin çalıştığını **onayladıktan sonra**; `sshd -t` / `sshd -T` ile denetlenir, geçersizse ya da başka bir dosya ezerse geri alınır |
| docker-repo | Docker'ın resmî apt deposu; imza anahtarı yalnız parmak izi Docker'ınkiyse kurulur |
| docker | Docker Engine ve Compose eklentisi (dağıtımın `docker.io` paketlerinin yerine geçmez) |
| docker-daemon | `daemon.json`: döndürülen loglar, `live-restore`, `no-new-privileges`, 256 ağa yer; yeniden başlatmadan önce `dockerd --validate` ile denetlenir |
| core-files | `/srv/core`: Caddyfile, `compose.yaml`, MongoDB root şifresi (bir kez üretilir, hiç değiştirilmez) |
| core-stack | Caddy, MongoDB ve (tünelle) cloudflared'i başlatır, sağlıklı olana kadar bekler |
| panel | `footops` sistem kullanıcısı, `/usr/local/bin`'deki ikili, 2FA'sıyla panel hesabı, `footops-agent` ve `footops-web` servisleri |

Çekirdek servisler:

| Servis | İmaj | Ulaşılan yer |
|---|---|---|
| Caddy | `caddy:2.11.7-alpine`, 443'e bağlanma dışında bütün yetkiler kaldırılmış | 443 |
| MongoDB | `mongo:7.0.45`, kimlik doğrulama açık, önbellek RAM'e göre | yalnız `127.0.0.1:27017` |
| cloudflared | `cloudflare/cloudflared` (tünelle) | — (dışarıya bağlanır) |

Caddy hiç sertifika istemez: siteler, yalnız Cloudflare'in güvenmesi gereken Cloudflare
Origin sertifikalarını kullanır. Siteler dışarıya port açmayan container'larda, her biri
kendi Docker ağında çalışır; bu ağlara yalnız Caddy ve MongoDB katılır.
