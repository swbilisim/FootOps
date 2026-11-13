# FootOps dış nöbetçisi (`footops-nobetci`)

[English](README.md) · **Türkçe**

FootOps sunucularını dışarıdan, bir ziyaretçinin gördüğü gibi izleyen bir Cloudflare
Worker. FootOps bir sunucunun içinde ters gidenleri (yedekler, disk, sertifikalar,
MongoDB…) kendi Telegram sohbetine söyler; sunucunun kendisinin gittiğini ya da bir sitenin
internetten artık açılmadığını söyleyemez. Nöbetçi söyler; kendi Telegram sohbetinde (ya da
aynı sohbette):

| Mesaj | Ne zaman |
|---|---|
| `❌ [H1] example.com çalışmıyor: 502 — site yanıt vermiyor` | bir alan adı üst üste üç ziyarette başarısız (5xx, Cloudflare 52x/530, 10 sn'de cevap yok, bağlantı yok) |
| `✅ [H1] example.com yeniden açılıyor (kesinti 6 dk)` | üst üste iki kez cevap verince |
| `🔴 [H1] FootOps'tan 16 dk haber yok` | sunucunun durum dosyası 15 dakikadan eski; ikinci satır sitelerinin hâlâ açılıp açılmadığını söyler |
| `🟡 [H1] Son başarılı yedek 1 gün 3 sa önce` | en yeni yedek 26 saatten eski (ikinci bir göz; FootOps kendi yedek hatalarını zaten bildirir) |
| `⚠️ [H1] example.com izlenemiyor` | Cloudflare ziyarete bir doğrulamayla (challenge) cevap veriyor (Bot Fight Mode, bir güvenlik kuralı): site izlenemez |
| `🟢 FootOps nöbetçisi çalışıyor.` | ilk çalışma |

Bir sunucunun aynı nedenli alan adları tek satırda gider; bütün bir sunucunun düşmesi tek
mesajdır. Yönlendirme, giriş sayfası, 404 ya da Cloudflare doğrulaması bir cevap sayılır.

## Nasıl çalışır

- Her sunucunun FootOps'u 5 dakikada bir yedek kovasına `izleme/durum.json` yazar:
  siteleri, alan adları, container durumları ve son yedek (sır yok). Nöbetçi onu bir R2
  bağlamasıyla okur — anahtar yok.
- Bir cron Worker'ı dakikada bir çalıştırır. Çalışan bir alan adı `CHECK_EVERY_MIN`
  dakikada bir ziyaret edilir (dakikalara yayılarak); biri başarısız olunca o sunucunun her
  alan adı her şey düzelene kadar dakikada bir ziyaret edilir. Sunucular bir çalışmanın
  altı bağlantısını sırayla kullanır; ziyaretleri asılı kalan bir sunucu ötekileri
  sıkıştıramaz. Dışarıda kalan bir alan adı (zaman ya da alt istek kalmadıysa) sonraki
  çalışmada önce gelir. Sessizliğe sunucunun saatiyle değil, R2'nin saatiyle (dosyanın
  geldiği an) karar verilir.
- Bir çalışma kendi dakikasının içinde rahatça biter: 25 sn'den sonra hiçbir ziyaret
  başlamaz, bir ziyaret en çok `TIMEOUT_SEC` (≤ 15 sn) sürer, en çok 3 mesaj gider,
  sonuncusu en geç 45. saniyede. Gerisi sonraki çalışmayı bekler; aynı anda iki çalışma aynı
  haberi iki kez gönderebilirdi.
- Nöbetçi 5 dakikada bir (ve bir karar değişince hemen) her durum dosyasının yanına
  `izleme/nobetci.json` yazar. FootOps onu okur: paneli nöbetçinin gördüğünü gösterir;
  nöbetçi 30 dakika sessiz kalırsa ya da 15 dakika mesajlarını gönderemez ya da durumunu
  saklayamazsa bir uyarı (Telegram'a da) verir. Süper gruba dönüşen bir sohbet yeni
  kimliğiyle izlenir. Nöbetçiyi tamamen kaldırırsan kovalardan `izleme/nobetci.json`'ı sil;
  yoksa FootOps onun sessiz olduğunu söylemeye devam eder.
- Bildiği şeyler (hangi alan adı düştü, sohbete ne söylendi) kendine ait bir kovada,
  `nobetci.json`'da durur. Adresi yoktur: fetch işleyicisi yok, workers.dev ve önizleme
  adresleri kapalı.

## Kurulum

Bir Cloudflare hesabı (ücretsiz Workers planı yeter) ve yedekleri o hesaptaki R2'de duran
FootOps sunucuları gerekir.

1. Nöbetçinin kovasını aç: `npx wrangler@4.152.0 r2 bucket create footops-izleme`
2. `cp wrangler.example.toml wrangler.toml` (git onu yok sayar) ve her sunucu için bir
   `SERVER_<kod>` bağlaması yaz; `bucket_name` o sunucunun yedek kovası.
3. Sırlar: `npx wrangler@4.152.0 secret put TELEGRAM_TOKEN` (nöbetçiye ait bir bot: yalnız
   yazar; her sunucu komutlar için kendi botunu okur — Telegram bir botu aynı anda yalnız
   bir okuyucuya okutur) ve `… secret put TELEGRAM_CHAT` (sohbetin kimliği).
4. `npx wrangler@4.152.0 deploy`. Önce denemek için `DRY_RUN = "1"` ile dağıt: mesajlar
   Telegram yerine loga gider (`npx wrangler@4.152.0 tail`). Sonra kapatınca baştan,
   selamlamayla başlar.

## Ayarlar (`[vars]`)

| Değişken | Varsayılan | |
|---|---|---|
| `CHECK_EVERY_MIN` | `2` | çalışan bir alan adının ziyaretleri arasındaki dakika (1-60) |
| `FAIL_AFTER` | `3` | ❌'dan önce üst üste başarısızlık |
| `RECOVER_AFTER` | `2` | ✅'dan önce üst üste cevap |
| `SILENT_AFTER_MIN` | `15` | 🔴'dan önce durum dosyasının yaşı (FootOps'un en az üç aralığı) |
| `BACKUP_AFTER_H` | `26` | 🟡'dan önce en yeni yedeğin yaşı (yedekler duraklatılmışken değil) |
| `TIMEOUT_SEC` | `10` | o süreye kadar cevap gelmeyen ziyaret başarısız (2-15) |
| `MAX_CHECKS` | `40` | bir çalışmada en çok ziyaret; alan adı fazlaysa sırayla |
| `SUBREQUESTS` | `50` | planın bir çalışmadaki alt istekleri (Workers Free); R2 çağrıları ve en çok 3 mesaj ziyaretlerden önce düşülür |
| `UTC_OFFSET_MIN` | `180` | mesajlardaki saatler, UTC'nin kaç dakika doğusu (Türkiye +3 sa); sabit bir fark, çünkü `Intl`'in ilk kullanımı bir çalışmanın 10 ms'lik CPU'sunun çoğunu yer |
| `DRY_RUN`, `LOG_CHECKS` | kapalı | mesajları göndermek yerine logla; her ziyareti logla |

## Maliyet

Ücretsiz plan: dakikada bir çalışma (günde 100.000'in 1.440'ı). CPU: bir çalışma
yaklaşık 18 ms ölçüldü (p99 50 ms), çoğu soğuk başlangıç; ücretsiz planın sınırı 10 ms,
"ara sıra aşan bir Worker için biraz esneklikle". Cloudflare şimdiye kadar her çalışmayı
geçirdi. Çalışmaları kesmeye başlarsa (sonuç `exceededCpu`) nabızlar durur ve FootOps
nöbetçinin sessiz olduğunu söyler; ücretli plan 30 sn'ye izin verir. R2: dakikada yaklaşık
3 okuma ve 5 dakikada birkaç yazma (ayda ücretsiz 10 M okuma ve 1 M yazmanın çok içinde).
Her ziyaret sunucuya aşağı yukarı bir ziyaretçinin sayfa görüntülemesi kadar yük getirir;
16 alan adı 2 dakikada bir, 2 çekirdekli bir sunucuda ölçüldü: CPU'sunun yaklaşık %0,07'si
ve ayda 2-3 GB trafik.

## Geliştirme

Bağımlılık yok: `npm test` (Node 22+, `node:test`). Yerelde, gerçek çalışma ortamında:
`npx wrangler@4.152.0 dev --test-scheduled`, `CHECK_URL` ve `TELEGRAM_API` yerine
koyulanları göstersin; sonra `curl "http://localhost:8787/__scheduled?cron=*+*+*+*+*"`.
