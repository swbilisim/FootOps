# Site yayınlama

[English](NEW-SITE.md) · **Türkçe**

Bilgisayarındaki koddan bir FootOps sunucusundaki `https://ornek.com`'a. İşin çoğunu panel
yapar: **Siteler** → **Projeni hazırla** projene gereken dosyaları yazar, **Yeni site** onu
çalıştırır. İlk seferde yaklaşık 45 dakika; sonraki her değişiklik birkaç dakika.

```
bilgisayardaki kod ──git push──▶ GitHub ──Actions derler──▶ GHCR (imaj) ──panel çeker──▶ sunucu
yereldeki veritabanı ──arşiv──▶ panel → İçe aktar
Cloudflare: sertifika (alan adı başına bir kez) ve DNS (en son)
```

**Başlamadan önce:**

- FootOps kurulu bir sunucu: [FootOps kurulumu](INSTALL-TR.md).
- `git` ve GitHub CLI, yani `gh` ([cli.github.com](https://cli.github.com)); bir kez
  `gh auth login` ile giriş yapılmış. Bilgisayarında Docker gerekmez: imajı GitHub derler.
- Proje bilgisayarında derlenebiliyor (`npm run build`).
- Bir ad: küçük harf, rakam ve tire, ör. `kahve-dukkani`. GitHub reposu, imaj ve paneldeki
  site bu adı alır.

FootOps **Next.js**, **Node.js** (Express ve benzerleri) ve **statik** siteleri (düz HTML ya
da Vite, Astro, Next.js statik dışa aktarma… ile derlenen) çalıştırır. Veritabanı isteyen
site panelden kendine ait bir MongoDB veritabanı ve kullanıcısı alır.

## 1. Projeyi hazırla: Projeni hazırla

Panel → **Siteler** → **Projeni hazırla**:

1. Tür: **Next.js**, **Node.js / Express** ya da **Statik site**.
2. Form:
   - **GitHub hesabı:** kendi hesabın ya da bir kuruluş; repo orada açılır.
   - **Proje adı.**
   - **Paket yöneticisi:** lock dosyan hangisiyse npm ya da pnpm.
   - **Port:** uygulamanın dinlediği port, genelde 3000 (Next.js ve Node.js).
   - **Yükleme klasörü** (isteğe bağlı; Next.js ve Node.js): site yüklenen dosyaları
     (görsel, belge) saklıyorsa onları yazdığı klasör, ör. `/app/uploads` (container'da
     proje `/app`'tedir). Boş kalırsa uygulama hiçbir yere yazamaz.
   - **Başlatma dosyası** (Node.js): node'un çalıştıracağı dosya, ör. `server.js`.
   - **Derleme çıktısı** (isteğe bağlı; statik site): `dist` (Vite, Astro), `out`
     (Next.js statik dışa aktarma), `build` (Create React App). Boşsa proje klasörü olduğu
     gibi yayınlanır.
3. **Dosyaları üret** üç dosya verir: `Dockerfile`, `.dockerignore` ve
   `.github/workflows/docker.yml`. **Zip indir**, sonra bir terminalde, projenin
   klasöründe (`package.json`'ın yanında):
   ```bash
   unzip ~/Downloads/<ad>-footops.zip
   ```
   (Çift tıklamak zip'i yeni bir klasöre açar; macOS ayrıca adı noktayla başlayan dosyaları
   gizler.) Ya da her dosyayı **Kopyala** ile al. Aynı adlı dosyaların yerine geçerler: önce
   kendi dosyalarına bak.

Dosyalar ne yapar (şablonlar [internal/scaffold/files](../internal/scaffold/files)'ta;
panel doldurur):

- Next.js ve Node.js imajları yetkisiz bir kullanıcıyla (`app`, uid 1001), statik sitenin
  nginx'i kendi yetkisiz kullanıcısıyla çalışır. Kod root'undur, uygulama onu değiştiremez; yalnız yükleme klasörüne (Next.js ayrıca görsel önbelleğine) yazabilir.
  Yükleme klasörü imaja bir etiketle yazılır; panel kalıcı klasörü kendisi doldurur.
- `.dockerignore` `.env*` dosyalarını imajın dışında tutar (`.env.production` Next.js'te ve
  derlenen statik sitede imaja girer); `.git`, `node_modules` ve anahtar dosyaları da
  dışarıda kalır.
- Workflow `main`'e her gönderimde derler ve iki etiket basar: sunucunun çektiği `:main`
  ve geri dönmek için `:sha-<kısa commit>` (ör. `:sha-abc1234`). İmaj sunucunun mimarisi için derlenir: amd64 ya da
  (GitHub'ın arm64 derleyicisinde) arm64.

Sonra sayfanın türüne göre gösterdiği kontrol listesinden geç. Önemli noktalar:

| | |
|---|---|
| Next.js | `next.config`'de `output: 'standalone'`. Bir `public/` klasörü (içinde `.gitkeep` olan boş bir klasör yeter). İmaj derlenirken veritabanı yoktur: derlenirken veritabanını okuyan sayfaya `export const dynamic = 'force-dynamic'`. |
| Node.js | Bütün adreslerde `process.env.PORT`'u dinle: `app.listen(process.env.PORT)`, `'localhost'` değil. Çalışırken gereken paketler `dependencies`'te olmalı. Sağlık kontrolü `/`'a bakar: `/`'u olmayan bir API'de Dockerfile'daki `HEALTHCHECK` satırına başka bir yol yaz. |
| Statik | Kökte (ya da derleme çıktısında) bir `index.html`. Tek sayfalı uygulamada `/` dışındaki adresler doğrudan açılınca 404 verir: böyle bir site için Node.js türünü seç ya da nginx ayarı ekle. |
| Veritabanı | Adres bir ortam değişkeninden gelmeli (`process.env.MONGODB_URI`); kodda asla `mongodb://localhost…` olmasın. |
| Yüklemeler (Next.js) | Next.js çalışmaya başladıktan sonra `public/`'e yazılan dosyayı göstermez (yeniden başlatana kadar 404): yüklemeleri `/app/uploads`'a yaz ve diskten okuyup döndüren bir route'la sun, ör. `app/uploads/[...yol]/route.ts`. `..` içeren yolları reddet, yalnız izin verdiğin uzantıları sun. |
| Lock dosyası | `package-lock.json` ya da `pnpm-lock.yaml` commit'li ve `package.json`'la uyumlu. `npm run build` bilgisayarında hata veriyorsa GitHub'da da verir. |
| `.gitignore` | İçinde `node_modules` ve `.env*` (Next.js'te `.next` de) olmalı. `.env.production` repoya girecekse `.env*`'ın altına `!.env.production` ekle; yoksa `git add .` onu atlar ve derleme herkese açık değerleri almaz. |

**Ortam değişkenleri üç türlüdür:**

| Tür | Örnek | Nereye |
|---|---|---|
| Sırlar ve sunucu ayarları | `JWT_SECRET`, API anahtarları, SMTP | `.env.local`'de kalır; repoya ve imaja girmez. Site oluşturulurken panele verilir (**.env dosyasından ekle**). |
| Derlemede koda gömülen herkese açık değerler | `NEXT_PUBLIC_*`, `VITE_*` | `.env.production`'a yazılır, repoya girer. Yalnız tarayıcının zaten göreceği değerler. Panelden değiştirilemez: commit, push ve yeniden dağıt. |
| Veritabanı adresi | `MONGODB_URI` | Hiçbir yere: veritabanını panel oluşturur ve adresi kendisi koyar. |

## 2. GitHub'a gönder

Projenin klasöründe:

```bash
git init -b main
git add .
git status            # .env.local, node_modules ve .next listede olmamalı
git commit -m "İlk sürüm"
gh repo create <hesap>/<ad> --private --source . --push
gh run watch
```

Projenin reposu zaten varsa üç dosyayı commit'le:

```bash
git add Dockerfile .dockerignore .github/workflows/docker.yml
git commit -m "FootOps: Docker imajı"
git push
gh run watch
```

Son bir bakış: `git ls-files | grep -i env` yalnız `.env.production`'ı (varsa) göstermeli.
Reponun varsayılan dalı `main` olmalı.

GitHub Actions imajı 3-5 dakikada derler. Yeşil: imaj `ghcr.io/<hesap>/<ad>:main`.
Kırmızı: `gh run view --log-failed` ve [Sorun giderme](#sorun-giderme).

Özel (private) reponun imajı da özeldir: sunucunun imaj girişi olmalı (**İmaj girişleri**;
[FootOps kurulumu](INSTALL-TR.md), 5.2).

## 3. Alan adı ve sertifika (alan adı başına bir kez)

**Sertifikalar** sayfasında alan adının sertifikası zaten varsa bu bölümü atla.

1. Alan adı Cloudflare'de değilse: Cloudflare → **Add a domain** → Free plan → alan adını
   aldığın yerde nameserver'ları Cloudflare'in verdiği iki adresle değiştir → alan
   **Active** olana kadar bekle (dakikalar ile birkaç saat arası). DNS kaydını henüz ekleme;
   o en sona kalır.
2. Alan adı için bir Origin sertifikası, **Sertifikalar**'a yapıştırılmış, SSL/TLS kipi
   **Full (strict)**: [FootOps kurulumu](INSTALL-TR.md), 5.1.

## 4. Siteyi kur: Yeni site

**Projeni hazırla**'nın en altındaki **Bu imajla yeni site** formu imaj adı dolu açar. Ya da:
**Siteler** → **Yeni site**.

1. **Uygulama türü** ve imaj: `ghcr.io/<hesap>/<ad>:main` → **İmajı kontrol et**. Sunucu
   imajı indirir ve içine bakar: sunucunun mimarisine uyuyor mu, hangi portu ve kullanıcıyı
   bildiriyor, hangi yükleme klasörünü. Birkaç saniye ile birkaç dakika sürer.
2. Form; sağdaki sütun her alanı anlatır:

| Alan | Ne yazılır |
|---|---|
| **Proje adı** | `<ad>` |
| **Grup** | Birlikte duran siteler için bir klasör (`/srv/<grup>/<ad>`): listeden seç ya da yeni bir ad yaz, ör. müşterinin adı |
| **Alan adları** | Her satıra bir tane: `ornek.com`, `www.ornek.com` |
| **Uygulama portu** | İmajdan dolar, genelde `3000` |
| **Bellek sınırı (MB)** | ör. Next.js sitesi için `512`; aşarsa container yeniden başlar. Sonra değiştirilebilir |
| **En büyük istek (MB)** | Dosya yüklenmeyen sitede birkaç MB; yükleme varsa en çok 100 |
| **Kalıcı klasör** | İmajın yükleme klasöründen dolar; site dosya yazmıyorsa boş |
| **Veritabanı** | **MongoDB veritabanı ve kullanıcısı oluştur** Next.js ve Node.js'te işaretli gelir; sitenin veritabanı yoksa kaldır. Değişken, kodunun okuduğu ad (`MONGODB_URI`); sonradan değiştirilemez |
| **Ortam değişkenleri** | **.env dosyasından ekle** → `.env.local`'in. Tarayıcı okur ve listeyi doldurur; dosyanın kendisi yüklenmez. İçindeki yerel veritabanı adresi atlanır: panel sunucudakini koyar |

3. **Özet:** değerler gösterilmez, yalnız adlar ve uyarılar. Oku → **Siteyi oluştur**.
4. Panel adımları çalıştırır (klasör, ağ, veritabanı, imaj, container, Caddy, HTTPS
   denemesi); biri başarısız olursa hepsini geri alır. Sitenin sayfası bir hafta
   **Sıradaki adımlar** ile açılır. Site dışarıdan henüz açılmaz: DNS kaydı yok.

## 5. Veriyi getir

Sitenin veritabanı ya da yüklenmiş dosyaları varsa bir arşiv hazırla:

- **Herhangi bir bilgisayarda** MongoDB'nin
  [Database Tools](https://www.mongodb.com/try/download/database-tools)'u ile:
  `mongodump --archive=veri.gz --gzip --db <yerel-veritabanı>`. Yerel veritabanının adı
  sitenin adından farklı olabilir; panel eşler. Yüklenmiş dosyalar: klasörün zip ya da
  tar.gz arşivi.
- **FootOps.app'li bir Mac'te:** **Arşivler** → **Arşiv oluştur**: veritabanı ve yükleme
  klasörü tek bir `.footops.tar`'da.

Panel → site → **Veri aktarımı** → **İçe aktar…** → dosya → **Yükle ve kontrol et**. Arşiv
kontrol edilirken site çalışmaya devam eder: veritabanı geçici bir veritabanına yüklenip
sayılır. Önizleme koleksiyonları, belgeleri ve dosyaları gösterir; onayla. Site kısa bir
süre durur: önce değişecek olanın kopyası alınır, veri yüklenip sayılır, site yeniden
başlar. Bir şey ters giderse kopyalar kendiliğinden geri konur. Veritabanı ve dosya
arşivleri art arda içe aktarılır.

## 6. DNS: siteyi aç (en son)

Sitenin sayfası → **Cloudflare'de yapılacaklar** tam kayıt listesini verir. Genelde:

1. Cloudflare → alan adı → **DNS** → **Records** → **Add record**: tür `A`, ad `@`, IPv4
   adresi = sunucunun adresi, **Proxied** (turuncu bulut) açık. Sonra tür `CNAME`, ad
   `www`, hedef `ornek.com`, Proxied açık.
2. **SSL/TLS** → **Overview**: **Full (strict)**. **SSL/TLS** → **Edge Certificates** →
   **Always Use HTTPS** açık.
3. Panel → site → **DNS'i kontrol et**: her alan adı ✓, yani Cloudflare üzerinden HTTPS
   cevabı. Sonra siteyi aç ve temel işleri bir kez dene: giriş yap, bir form gönder, bir
   görsel yükle.

Bundan sonra site gece yedeğine girer; dış nöbetçi (kurduysan) onu ziyaret eder ve cevap
vermeyi bırakınca Telegram'a yazar.

## Günlük işler

| İş | Nasıl |
|---|---|
| Yeni kodu yayınla | `git push` → Actions yeşil (`gh run watch`) → panel → site → **Yeniden dağıt**: `:main`'i yeniden çeker ve container'ı yeniler; birkaç saniye kesinti |
| Ortam değişkeni değiştir | Panel → site → **Düzenle** → ortam değişkenleri → kaydet. Container yeniden oluşturulur; değerler bir daha gösterilmez |
| `NEXT_PUBLIC_*` değiştir | `.env.production`'ı düzelt → commit, push → **Yeniden dağıt** |
| Eski bir sürüme dön | **Düzenle** → imaj `ghcr.io/<hesap>/<ad>:sha-<kısa commit>` (ör. `:sha-abc1234`) → kaydet. Etiketler GitHub'da, reponun Packages bölümünde. En yeniye dönmek için yine `:main` |
| Site çalışmıyor | Panel → site → **Loglar**; gerekirse **Yeniden başlat** |
| Veriye bak ya da düzelt | Panel → site → **Veritabanı** |
| Mac'teki veritabanı ile sitedeki | FootOps.app → **Veritabanları**: bir kez siteyle eşleştir, sonra **Karşılaştır** (sitenin son yedeğiyle, belge belge), **Sunucuya gönder** (**İçe aktar** için arşiv) ya da **Yerele çek** (yedek onun yerine, önce kopyası alınır) |
| Yüklenen dosyalar | Panel → site → **Dosyalar** |
| Yedekten geri yükle | Panel → site → **Yedekler** → bir yedek → önce **Aç ve incele**, sonra **Geri yükle…** |
| Bir süre yayından kaldır | **Parka al…**: yerine bir bakım sayfası gelir, veri ve yedekler kalır; geri açmak için **Yayına al…** |
| Kaldır | **Tehlikeli bölge** → **Siteyi kaldır…**: dosyaları `_removed`'a taşınır, veritabanı kalır. Sonra DNS kayıtlarını sil |

## Sorun giderme

| Belirti | Neden ve çözüm |
|---|---|
| Actions kırmızı: `"/package-lock.json": not found` ya da `npm ci` hatası | Lock dosyası commit'lenmemiş ya da `package.json`'la uyuşmuyor: yerelde `npm install`, `package-lock.json`'ı commit'le |
| Actions kırmızı: derlemede `MongoNetworkError` ya da `ECONNREFUSED` | Bir sayfa imaj derlenirken veritabanını okuyor, oysa veritabanı yok: o sayfaya `export const dynamic = 'force-dynamic'` |
| Actions kırmızı: `"/app/public": not found` | `public/` klasörü yok: içinde `.gitkeep` olan boş bir klasör ekle |
| İmaj kontrolü: giriş yok ya da erişim yok | İmaj adı yanlış (küçük harf mi?), Actions henüz bitmedi ya da imaj girişindeki token'ın süresi doldu (**İmaj girişleri**) |
| İmaj kontrolü: mimari uyuşmuyor | İmaj başka bir mimari için derlenmiş, ör. Apple işlemcili bir Mac'ten (arm64) amd64 sunucuya gönderilmiş: imajı workflow derlesin (**Projeni hazırla** onu sunucunun mimarisine göre yazar) |
| Site oluşturma uygulamayı beklerken duruyor (*… portunu dinlemeye başlamadı* ya da *container çalışmıyor*, son log satırlarıyla) ve her şey geri alınıyor | Uygulama başka bir portu ya da yalnız localhost'u dinliyor (Next.js'e `HOSTNAME=0.0.0.0` gerekir; üretilen Dockerfile koyar) ya da açılırken duruyor, çoğu zaman eksik bir ortam değişkeni yüzünden: düzelt, push et, siteyi yeniden oluştur |
| Yüklenen görsel yeniden başlatana kadar 404 | Next.js `public/`'e sonradan yazılanı göstermez: yüklemeleri `public/` dışında tut, bir route'la sun (1. bölüm) |
| Loglarda `EACCES` ya da `read-only` | Uygulama izin olmayan bir yere yazıyor: o klasörü yükleme klasörü (ve kalıcı klasör) yap |
| Yüklenen dosyalar yeniden dağıtımdan sonra yok | Kalıcı klasör yok ya da yolu yanlış: **Düzenle** → **Kalıcı klasör** |
| Veritabanı boş görünüyor ya da uygulama bağlanamıyor | Kod, panelin koyduğundan başka bir değişkeni okuyor (sitenin sayfası onu gösterir, ör. `MONGODB_URI`): kodu o değişkeni okuyacak şekilde düzelt, push et, **Yeniden dağıt** |
| Cloudflare **520** ya da **525** | Sunucu o adı sunmuyor: sitenin alan adları arasında yok (**Düzenle**) ya da A kaydı başka bir sunucuyu gösteriyor |
| Cloudflare **521** ya da **522** | Cloudflare sunucuya 443'ten ulaşamıyor: A kaydı başka yeri gösteriyor, sağlayıcının güvenlik duvarı 443'ü kapatıyor ya da SSL/TLS Full (strict) değil (Flexible 80. porta gider, o kapalı): 6. bölüm |
| Tarayıcı sertifikaya güvenilmediğini söylüyor | Proxied kapalı: Origin sertifikasına yalnız Cloudflare güvenir. Turuncu bulutu aç |
| `NEXT_PUBLIC_*` değeri eski | Derlemede gömülür: `.env.production`, push, **Yeniden dağıt** |
