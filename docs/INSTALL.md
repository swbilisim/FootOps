# Installing FootOps

**English** · [Türkçe](INSTALL-TR.md)

This guide takes a freshly installed Ubuntu server to a running FootOps panel, ready for
its first site. Plan on about an hour, most of it waiting for downloads. The installer and
the panel speak Turkish; this guide gives each Turkish name with its meaning, e.g.
**Sertifikalar** (Certificates).

Next: [Publishing a site](NEW-SITE.md).

## What you need

| | |
|---|---|
| **A server** | Ubuntu 22.04 or 24.04, freshly installed, amd64 or arm64, at least 1 GB of RAM and 8 GB of free disk. 2 vCPUs and 4 GB of RAM run about ten small sites. You need to log in as root over SSH (with a key, or the password the provider gives you). |
| **A domain on Cloudflare** | The Free plan is enough. The panel and your sites get their addresses there, and Cloudflare stands in front of them. The domain's nameservers must point at Cloudflare and the zone must be **Active**. |
| **An SSH key** | On your computer: `ssh-keygen -t ed25519` (macOS, Linux, Windows 10 and later). Root login gets closed; from then on you sign in with this key. |
| **An authenticator app** | On your phone (Google Authenticator, Aegis, 2FAS, Bitwarden, 1Password…): the panel asks for its code at every login. |
| **A password manager** | The installer shows the panel password, the recovery codes and the MongoDB root password once. |
| Optional | A Cloudflare R2 bucket (or other S3-compatible storage) for backups, Telegram for alerts, a GitHub account for your sites' code and images. |

If the provider has a firewall of its own (e.g. Hetzner's Cloud Firewall), let ports 22
and 443 in. FootOps closes everything else on the server itself.

## 1. Prepare Cloudflare

Have these ready before the installer asks for them (about 15 minutes). Cloudflare
renames its menus now and then; the parts stay the same: an Access application with your
e-mail address, a tunnel, and a route from the panel's address to `http://127.0.0.1:7700`.

**Pick the panel's address:** a subdomain of your domain one level deep, such as
`panel.example.com`. Not `panel.server1.example.com`: Cloudflare's free certificate covers
one level only. Don't add a DNS record for it; the tunnel adds its own.

### 1.1 Zero Trust

Open [one.dash.cloudflare.com](https://one.dash.cloudflare.com) (Cloudflare One, also
called Zero Trust). The first time, pick a **team name** and the **Free** plan (Cloudflare
may ask for a payment method even for the free plan). Your team domain is
`<team>.cloudflareaccess.com`; you can see it later under **Settings**. The installer asks
for the team name.

### 1.2 An Access application: who may reach the panel

**Access** → **Applications** → **Add an application** → **Self-hosted**:

1. Name: e.g. `FootOps server1`. Session duration as you like (e.g. 24 hours).
2. Public hostname: subdomain `panel`, domain `example.com` (the panel's address).
3. Policy: action **Allow**, include **Emails** → your e-mail address. Add only the people
   who will manage this server.
4. Login method: **One-time PIN** (a code by e-mail) is on by default.
5. Save, open the application again and copy its **Application Audience (AUD) Tag**:
   64 hexadecimal characters, in the application's overview or its additional settings.

Cloudflare now asks for an allowed e-mail address before anyone reaches the panel, and
FootOps checks Cloudflare's signature on every request: a request that goes around
Cloudflare is refused.

### 1.3 A tunnel: how Cloudflare reaches the panel

**Networks** → **Tunnels** → **Create a tunnel** → **Cloudflared**:

1. Name: e.g. the server's code (`server1`). Save.
2. The next page shows commands that install the connector (Debian, Docker…), each with
   the tunnel's token in it. Copy one whole command. **Don't run it:** FootOps runs
   `cloudflared` itself. The token is a secret; whoever holds it can run your tunnel.
3. Next, add a **public hostname** (also called a *published application route*):
   subdomain `panel`, domain `example.com`, path empty, service type **HTTP**, URL
   `127.0.0.1:7700`. Save. Cloudflare adds the DNS record itself.

The tunnel connects from the server out to Cloudflare, so the panel needs no open port.

> **Without a tunnel** the panel opens only through an SSH tunnel ([section 4](#4-first-login)).
> You can add one later: run the installer again (`sudo footops install`), answer `h` to
> **Bu cevaplarla devam edilsin mi?** (Continue with these answers?) and yes to the tunnel.

## 2. Download and verify FootOps

Log in to the server as root (`ssh root@<server-ip>`) and download the latest release:

```bash
ARCH=$(dpkg --print-architecture)    # amd64 or arm64
BASE=https://github.com/swbilisim/FootOps/releases/latest/download
curl -fsSLO "$BASE/footops_linux_$ARCH"
curl -fsSLO "$BASE/footops-release.json"
curl -fsSLO "$BASE/footops-release.json.sig"
```

A release's manifest (`footops-release.json`) lists its files with their SHA-256, and it
is signed with FootOps' release key, whose private half never leaves the maintainer's
computer. Check the signature:

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

It must print `Signature Verified Successfully`. Then check the binary against the
manifest:

```bash
python3 -c "import json; print(json.load(open('footops-release.json'))['files']['footops_linux_$ARCH']['sha256'] + '  footops_linux_$ARCH')" | sha256sum -c
```

It must print `footops_linux_amd64: OK` (or `arm64`). If either check fails, stop: the
download is not what was released.

The same key is built into FootOps (`internal/release/keys.go`), and every server checks
each update with it. If you are reading this guide somewhere other than
[github.com/swbilisim/FootOps](https://github.com/swbilisim/FootOps), compare the key with
that file there. Each release is also rebuilt from its tag by the repository's
**Release check** workflow, which compares the bytes with the signed manifest.

Don't pipe a script from the internet into a shell: download, verify, then run.

## 3. Run the installer

```bash
chmod +x footops_linux_$ARCH
./footops_linux_$ARCH install
```

(As root. From another account: `sudo ./footops_linux_$ARCH install`. Adding `--dry-run`
shows what would change without changing anything.)

The installer asks its questions in the terminal, so there is no setup page anyone could
reach before you. The value in brackets is the default, which Enter accepts. Yes/no
questions take `e` (*evet*, yes) or `h` (*hayır*, no); the capital letter is the default.

| Question (as shown) | Meaning | Answer |
|---|---|---|
| Bu sunucu bir yedekten mi kurulacak? | Is this server being installed from a backup? | Enter (no). Yes only to bring back a lost server: [section 7](#7-bringing-back-a-lost-server). |
| Sunucu kodu | Server code | A short name, 1-16 letters, digits or dashes, e.g. `S1`. It appears in the panel's header, in Telegram messages (`[S1] …`) and in the backups; keep it for good. |
| Hostname | | Enter (the code in lower case). |
| Saat dilimi | Time zone | e.g. `Europe/Istanbul`, `Europe/Berlin`. The night backup (03:00), the reboot time and reminders follow it. |
| Kullanıcı adı | User name (the admin user) | Your account on the server, e.g. `ali`: lower case, not `root`, `admin` or `ubuntu`. You will log in to the server and the panel with it. |
| root kullanıcısında N SSH anahtarı bulundu … eklensin mi? | Root has N SSH keys; give them to the admin user too? | `e` if they are yours; then paste any other public key, or just press Enter. |
| SSH public key(ler)ini yapıştır | Paste your SSH public key(s) | Asked when root has no key, or when you answered `h` above: the contents of `~/.ssh/id_ed25519.pub` on your computer, then an empty line. |
| sudo şifre istesin mi? | Should sudo ask for a password? | Enter (yes, recommended: then a stolen SSH key alone does not give root). Then the sudo password twice (at least 12 characters); save it in your password manager. |
| Swap boyutu (GB) (önerilen) | Swap size (recommended) | Enter. |
| MongoDB önbelleği (GB) (önerilen) | MongoDB cache (recommended) | Enter. |
| Panel adresi | Panel address | `panel.example.com` (section 1). |
| Cloudflare Tunnel şimdi kurulsun mu? | Set up Cloudflare Tunnel now? | Enter (yes), then the next three. |
| Tunnel token ya da komutun tamamı | The tunnel token or the whole command | Paste the command from 1.3; it is not shown on screen. |
| Zero Trust takım adı | Zero Trust team name | e.g. `myteam` (or `myteam.cloudflareaccess.com`). |
| Access AUD etiketi | Access AUD tag | The tag from 1.2. |
| Gerekirse sunucu otomatik yeniden başlatılsın mı? | Reboot automatically when an update needs it? | Enter (yes), then the time: Enter (04:00, an hour after the night backup). |
| Bu ayarlarla kuruluma başlansın mı? | Start installing with these settings? | Read the summary, then `e`. Here the default is no. |

The answers (no secrets) are kept in `/etc/footops/install.json`.

### While it runs (10-20 minutes)

- **The SSH check.** Before root and password logins are closed, the installer stops and
  asks you to try the new account from a **second** terminal (keep the first one open):
  ```bash
  ssh <user>@<server-ip>
  sudo -v
  ```
  If both work, answer `e` to **Yeni kullanıcıyla giriş ve sudo çalıştı mı?** (Did login
  and sudo work with the new user?). If not, answer `h`: SSH stays as it was and the
  installer stops; once key login works, run it again (below). Don't retry too fast: five
  failed logins within 10 minutes ban your address for an hour (fail2ban).
- **Two-factor login.** A QR code appears: scan it with your authenticator app (or type
  the key printed under it), then enter the code the app shows (**Uygulamanın gösterdiği
  6 haneli kod**). If it is refused, make sure your phone sets its clock automatically.
- **At the end** the lines that start with 🔑 are shown **once**: the panel user and
  password, ten recovery codes (each works once, for when the phone is lost) and the
  MongoDB root password. Put them in your password manager now.
- Above them: the panel's address, how to reach MongoDB, and **Sıradaki adımlar** (next
  steps).
- **Güvenlik notları** (security notes), if any: what the installer did not change for
  you, with the command that does.
- If updates need a reboot, it says so: run `sudo reboot` once you have everything.

Everything is logged in `/var/log/footops-install.log`. The installer can be run again at
any time: finished steps are skipped, and an interrupted run carries on where it stopped.
Until it has finished, run the downloaded file again: as root `./footops_linux_amd64
install`, or, once root login is closed, as your admin user
`sudo /root/footops_linux_amd64 install` (`arm64` on an ARM server). After that it is
`sudo footops install`. Ctrl+C while questions are asked quits at once; once the
server is being changed, the first Ctrl+C lets the running command (e.g. apt) finish and
then stops, and a second one quits immediately. If the SSH connection drops, the
installer keeps going and stops safely at the next question it cannot ask.

Once the install has finished, FootOps is `/usr/local/bin/footops` and the downloaded files
can be deleted.

## 4. First login

1. Open `https://panel.example.com`. Cloudflare asks for your e-mail address and mails you
   a code.
2. Then FootOps' own login: the user name (your admin user), the panel password from the
   end of the install, and the code from your authenticator app. A recovery code works in
   place of the app's code.

**Without the tunnel**, or while Cloudflare is down, an SSH tunnel always works:

```bash
ssh -L 7701:127.0.0.1:7701 <user>@<server-ip>
```

and then open http://localhost:7701.

Sessions end after 30 idle minutes, and after 12 hours at most. Five wrong logins lock the
account for 5 minutes, then for twice as long each time.

The home page, **Sunucu** (Server), opens with **Başlarken** (Getting started): the steps
below, each ticked off once done. The list goes away once there are a certificate, a site
and backups.

## 5. Set up the server in the panel

### 5.1 Certificate: Sertifikalar

Cloudflare reaches your sites over HTTPS with a Cloudflare Origin certificate, valid for
15 years; the server never asks Let's Encrypt. One certificate per domain covers the domain
and all its subdomains:

1. Cloudflare → your domain → **SSL/TLS** → **Origin Server** → **Create Certificate**.
2. "Generate private key and CSR with Cloudflare", key type RSA, host names
   `*.example.com` and `example.com`, validity 15 years → **Create**.
3. Panel → **Sertifikalar** (Certificates) → paste the **Origin Certificate** and the
   **Private Key** → **Yükle** (Upload). Cloudflare won't show the key again; keep a copy in
   your password manager if you like (a new certificate can always be made).
4. Cloudflare → **SSL/TLS** → **Overview**: encryption mode **Full (strict)**.

### 5.2 Image login: İmaj girişleri (for private images)

The server pulls your sites' images from a registry, usually GitHub's (`ghcr.io`). Images
of private repositories need a login; public ones don't.

1. Panel → **Siteler** (Sites) → **İmaj girişleri** (Image logins) → **GitHub'da token
   oluştur** (Create a token on GitHub). GitHub's page opens with **read:packages** ticked.
2. Choose an **Expiration** (e.g. one year) → **Generate token** → copy it.
3. Back in the panel: registry `ghcr.io`, user name = the GitHub account that made the
   token, the token → **Giriş yap ve kaydet** (Log in and save).

The token's owner must be able to read the images: they are its own, or it is a member of
the organisation that owns them. The panel warns 14 days before the token expires.

### 5.3 Backups: Yedekleme (recommended)

Every night at 03:00, each site's database, persistent folder and definition (environment
variables included) and the server's own settings are backed up with restic to a bucket of
the server's own, encrypted before they leave the server. Only what changed is uploaded.

With Cloudflare R2:

1. Cloudflare → **R2** → **Create bucket**: e.g. `footops-s1`, one bucket per server.
2. R2 → **Manage API Tokens** → **Create Account API token**: permission **Object Read &
   Write**, for that bucket only → create, then copy the **Access Key ID** and the
   **Secret Access Key** (shown once).
3. The endpoint: R2 → **Overview** → **Account Details** → S3 API:
   `https://<account-id>.r2.cloudflarestorage.com`, without the bucket's name.
4. Panel → **Yedekleme** (Backups) → **Depoyu bağla** (Connect the repository): endpoint,
   bucket, the two keys → **Bağlan ve dene** (Connect and test).
5. **Şifreyi göster** (Show the password) shows the repository's password. **Without it
   the backups cannot be opened**, and a lost server comes back only with it. Save it in
   two places that don't depend on this server (a password manager and a second one), tick
   **Şifreyi iki ayrı yere kaydettim** (I saved the password in two places) →
   **Onayla, yedeklemeyi başlat** (Confirm, start backing up).

Other S3-compatible storage (Backblaze B2, AWS S3…) works the same way. Each site's
schedule and how long its backups are kept are on its row (**Ayarlar**, Settings). Once a
month, after a clean night, one site's newest backup is restored where it harms nothing
and checked.

### 5.4 Telegram: Bildirimler (optional)

Alerts (a failed backup, a disk filling up, a certificate about to expire, a site that
keeps restarting or runs out of memory…) go to a Telegram chat, where `/durum` (status) and
`/siteler` (sites) answer too. A site that stops answering is reported by the outside
watcher (5.5), as visitors see it.

1. In Telegram, **@BotFather** → `/newbot` → give it a name → copy the token. **One bot per
   server:** Telegram lets only one server at a time read a bot's messages.
2. Panel → **Bildirimler** (Notifications) → paste the token → **Devam** (Continue).
3. Open the `t.me/…` link it shows and press **Start** (or send the bot the code) →
   **Gönderdim, bağla** (Sent it, link).
4. **Deneme mesajı gönder** (Send a test message).

Only the person who linked the chat can give commands, and nothing can be deleted from
Telegram.

### 5.5 Outside watcher (optional)

A Cloudflare Worker that visits your sites from outside and tells a Telegram chat when one
stops answering or a server falls silent: [worker/README.md](../worker/README.md). It reads
the status file each server keeps in its backup bucket, so it needs 5.3 first.

## 6. Your first site

→ [Publishing a site](NEW-SITE.md): **Siteler** → **Projeni hazırla** (Prepare your
project) gives your project the three files it needs, GitHub builds its image, and the
panel runs it.

## 7. Bringing back a lost server

Backups are enough to rebuild a server: a new one is installed from them.

1. Download and verify FootOps on a new server (section 2), run the installer and answer
   **yes** to **Bu sunucu bir yedekten mi kurulacak?**. Give the old server's code (the
   backups are kept under it), the usual answers and the repository: endpoint, bucket, a
   key that can *read* it, the repository's password, and which backup to go back to (the
   newest by default). On a Mac, FootOps.app puts the repository on one line for a single
   paste (**Yedekler** → **Kurtarma satırını kopyala**).
2. After the usual steps (no tunnel, no new panel account: the old one comes back), the
   server's own part comes back (certificates, the panel login and its 2FA, image logins,
   backup settings), then every site of the newest full backup: its definition, a new
   database user, its database and files, then Caddy and an HTTPS check. Nothing is
   written to the repository. A site that fails is undone and the others go on; running
   the installer again brings back only what is missing.
3. The server is now a **rehearsal** (*prova*): the tunnel, the backups and Telegram are
   kept aside, and the sites cannot connect to the outside, so an app's e-mails or
   payments don't happen twice while the old server may still run. Look around through
   the SSH tunnel and try the sites with `curl -k --resolve <domain>:443:<new-ip> https://<domain>/`.
4. Once the old server is surely off: `sudo footops go-live` reconnects the repository,
   puts Telegram, the tunnel and Access back, opens the sites to the outside and takes a
   first backup. Then point the domains' A records at the new address.

## Keeping it up to date

**FootOps.** The server looks for a new release once a day and tells the Telegram chat.
Panel → **Sunucu** → the **FootOps** card → **Güncelleme** (Update) → **Şimdi denetle**
(Check now) → read what will change and whether sites will stop for a moment →
**… sürümüne güncelle** (Update to …). The release is checked against the release key
before anything changes; if the new version does not come up, the previous one comes back
by itself, and **… sürümüne dön** (Back to …) is there too. Over SSH:
`sudo footops self-update` (`--check` only looks).

**The system.** Ubuntu's security updates install themselves every day, and the server
reboots at the chosen time when one needs it. Packages from Docker's own repository are
not among them: when panel → **Sunucu** → **Kontrol** (Check) lists waiting updates, run
`sudo apt update && sudo apt upgrade` at a quiet hour.

## When something goes wrong

| Problem | What to do |
|---|---|
| Panel password lost | Over SSH: `sudo footops reset-password` (a new password; open panel sessions end) |
| Phone lost | Log in with a recovery code, or `sudo footops reset-2fa` (a new QR code and new recovery codes) |
| The panel doesn't open | The SSH tunnel (section 4); `sudo systemctl status footops-web footops-agent` |
| SSH login doesn't work | The provider's web console |
| MongoDB root password leaked | `sudo footops rotate-mongo-root` (the sites' own database users are not affected) |
| A site is down | Panel → the site → **Loglar** (Logs); **Yeniden başlat** (Restart) if needed |
| Data deleted or broken | Panel → **Yedekleme** → the site → a backup → **Aç ve incele** (Open and inspect) first, then **Geri yükle…** (Restore) |
| Disk filling up | Panel → **Sunucu** → **Kontrol** → **Kullanılmayan imajları sil** (Delete unused images) |
| The server is lost | [Section 7](#7-bringing-back-a-lost-server) |

**MongoDB from your computer** (Compass, Robo 3T): connect over SSH as your admin user,
then to `localhost:27017` as `root` with authentication database `admin`. The password was
shown at the end of the install; on the server it is in `/etc/footops/secrets/mongo.env`
(`sudo grep PASSWORD /etc/footops/secrets/mongo.env`). The port is never open to the
internet.

## What the installer changes

The security baseline runs first, so root login is closed before the long Docker downloads
start. Every step checks the current state before it changes anything.

| Step | Change |
|---|---|
| packages | `ca-certificates curl gnupg ufw fail2ban python3-systemd unattended-upgrades` |
| upgrade | Installs pending updates, including those that need new packages (kernel security fixes); never removes packages |
| hostname | Sets the hostname and tells cloud-init to keep it across reboots |
| timezone | The answer, e.g. `Europe/Istanbul` |
| swap | Sized from RAM (up to 6 GB of RAM → 2 GB, more → 4 GB), `vm.swappiness=10` |
| unattended-upgrades | Automatic security updates, optional reboot at a fixed time |
| admin-user | The admin user in `sudo` (uid 2000 and up, away from the uids container images use), your SSH keys; sudo with a password (recommended) or without |
| firewall | UFW: incoming denied except 22 and 443 |
| fail2ban | SSH jail in aggressive mode (failed key logins count too): 5 tries in 10 minutes → banned for an hour |
| ssh-hardening | No root login, no passwords, only the admin user — **applied only after you confirm** that key login works from a second terminal; checked with `sshd -t` / `sshd -T` and rolled back if invalid or overridden |
| docker-repo | Docker's official apt repository; its signing key is installed only if the fingerprint is Docker's |
| docker | Docker Engine and the Compose plugin (refuses to replace the distribution's `docker.io` packages) |
| docker-daemon | `daemon.json`: rotated logs, `live-restore`, `no-new-privileges`, room for 256 networks; checked with `dockerd --validate` before the restart |
| core-files | `/srv/core`: Caddyfile, `compose.yaml`, the MongoDB root password (made once, never replaced) |
| core-stack | Starts Caddy, MongoDB and (with the tunnel) cloudflared, and waits until they are healthy |
| panel | The `footops` system user, the binary in `/usr/local/bin`, the panel account with its 2FA, and the `footops-agent` and `footops-web` services |

The core services:

| Service | Image | Reachable on |
|---|---|---|
| Caddy | `caddy:2.11.7-alpine`, every capability dropped except binding 443 | 443 |
| MongoDB | `mongo:7.0.45`, authentication on, cache sized from RAM | `127.0.0.1:27017` only |
| cloudflared | `cloudflare/cloudflared` (with the tunnel) | — (connects out) |

Caddy never asks for certificates: the sites use Cloudflare Origin certificates, which
only Cloudflare has to trust. Sites run in containers without published ports, each on a
Docker network of its own; only Caddy and MongoDB join them.
