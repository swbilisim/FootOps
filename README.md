# FootOps

Lightweight, security-first self-hosted server panel written in Go.

One command turns a fresh Ubuntu server into a hardened Docker host (swap, SSH hardening,
firewall, Docker, Caddy, MongoDB) and installs a small web panel for adding, deploying and
monitoring sites.

> **Status:** early development. The installer and panel work end to end in tests and
> run on a first production server; expect changes.

## Design in short

- **Single binary.** `footops` is both the installer and the panel. No runtime dependencies on the host besides Docker.
- **Per-server.** Each server runs its own FootOps; there is no central control plane.
- **Never exposed to the internet.** The panel listens on `127.0.0.1` only and is reached through Cloudflare Tunnel + Cloudflare Access, then its own password + TOTP login. An SSH tunnel is the break-glass path.
- **Privilege separation.** `footops-web` (the UI) runs unprivileged and can only ask `footops-agent` to perform a fixed set of validated actions. A bug in the UI cannot be turned into arbitrary root commands.
- **No ports per app.** Only Caddy listens, on 443 (sites are https only behind Cloudflare). Apps join per-project Docker networks and are reached by container name, so there is no port table to maintain. Caddy passes the visitor's address in `X-Real-IP` (from Cloudflare's `CF-Connecting-IP`, never from what the visitor sends) and caps request bodies at 100 MiB, Cloudflare's own limit on the Free and Pro plans.
- **Files on disk are the source of truth.** Each site lives in `/srv/<group>/<project>/` as a normal `docker-compose.yml` + `.env`. If FootOps is removed, everything keeps running and can be managed by hand.
- **Pull-based deploys.** GitHub Actions builds images and pushes them to GHCR; FootOps notices new images and deploys them. CI never holds credentials to the server.

## Install

Requires Ubuntu 22.04 or 24.04, root access over SSH with a key.

```bash
VERSION=v0.1.0   # pick the latest release
ARCH=amd64       # or arm64
BASE=https://github.com/swbilisim/FootOps/releases/download/$VERSION

curl -fsSLO "$BASE/footops_linux_$ARCH"
curl -fsSLO "$BASE/SHA256SUMS"
sha256sum --ignore-missing -c SHA256SUMS   # must print: footops_linux_<arch>: OK

chmod +x "footops_linux_$ARCH"
sudo "./footops_linux_$ARCH" install
```

Do not pipe a script from the internet into a shell; download, verify, then run.

The installer asks its questions in the terminal (never in a browser, so there is no
unauthenticated setup page) and stops before disabling root/password SSH login until you
confirm that key-based login for the new admin user works from a second terminal.

At the end it prints the panel URL, the generated panel password, recovery codes and the
MongoDB root password **once**. Store them in your password manager; they are not kept in
plain text on the server.

### What the installer does (so far)

| Step | Change |
|---|---|
| packages | `ca-certificates curl gnupg ufw fail2ban python3-systemd unattended-upgrades` |
| upgrade | Installs pending updates, including ones that need new packages (kernel security fixes); never removes packages |
| hostname | Sets the hostname and tells cloud-init to keep it across reboots |
| timezone | e.g. `Europe/Istanbul` |
| swap | Sized from RAM (≤6 GB RAM → 2 GB, more → 4 GB), `vm.swappiness=10` |
| unattended-upgrades | Automatic security updates, optional reboot at a fixed time |
| admin-user | Creates the admin user in `sudo`, installs your SSH keys; sudo with password (recommended) or without |
| firewall | UFW: deny incoming, allow 22/443 (removes the 80 rule earlier versions added) |
| fail2ban | sshd jail in aggressive mode (counts failed key logins), 5 tries in 10 min → 1 h ban |
| ssh-hardening | No root login, no passwords, only the admin user — **applied only after you confirm** key login works from a second terminal; the config is validated with `sshd -t`/`sshd -T` and rolled back if invalid or overridden |
| docker-repo | Docker's official apt repository; the signing key is installed only if its fingerprint is Docker's |
| docker | Docker Engine + Compose plugin (refuses to replace distribution `docker.io` packages) |
| docker-daemon | `daemon.json`: rotated `local` logs, `live-restore`, `no-new-privileges`, room for 256 networks; validated with `dockerd --validate` before restart |
| core-files | `/srv/core`: Caddyfile, `compose.yaml`, MongoDB root password (generated once, never regenerated) |
| core-stack | Starts Caddy and MongoDB and waits until both are healthy; re-runs only when their config changed |

The security baseline runs first, so root login is closed before the long Docker
downloads start.

### Core services

| Service | Image | Exposed |
|---|---|---|
| Caddy | `caddy:2.11.7-alpine`, all capabilities dropped except binding 443 | 443 |
| MongoDB | `mongo:7.0.45`, auth on, WiredTiger cache sized from RAM, 60 s to shut down cleanly | `127.0.0.1:27017` only |

Caddy never requests certificates (`auto_https off`): sites use Cloudflare Origin CA
certificates, so the origin only needs to be trusted by Cloudflare.

MongoDB is reachable from your machine through an SSH tunnel (Robo 3T, Compass):
SSH to the admin user, then connect to `localhost:27017` as `root` with auth database
`admin`. The password is shown once at the end of the install and stored in
`/etc/footops/secrets/mongo.env` (root only).

Every step checks the current state first, so the installer can be re-run at any time:
completed steps are skipped and an interrupted run continues where it stopped.

- `sudo footops install --dry-run` shows what would change without changing anything.
- **Ctrl+C** while questions are asked quits immediately. Once the server is being
  changed, the first Ctrl+C lets the running command (e.g. apt) finish and then stops; a
  second Ctrl+C quits at once.
- If the SSH connection drops, the installer keeps going and stops safely at the next
  question it cannot ask. Details are always in `/var/log/footops-install.log`.
- Answers (no secrets) are kept in `/etc/footops/install.json` for re-runs.

## The panel

After the core stack, the installer creates the `footops` system user (uid 800-899, so
it never matches the uids container images write files as), installs the binary to
`/usr/local/bin/footops`, enrols the admin's authenticator app (QR code in the terminal,
confirmed with a live code) and starts two services:

| Service | Runs as | Can do |
|---|---|---|
| `footops-agent` | root | A fixed, validated API on `/run/footops/agent.sock`: overview, metric history, alerts, container logs, restart, sites, audit log. Accepts connections only from root and the `footops` uid, checked with `SO_PEERCRED`. |
| `footops-web` | `footops`, no capabilities, seccomp, `ProtectSystem=strict` | Login and pages. Cannot see `/srv`, secrets, Docker's socket or data, or the audit log; asks the agent for everything. |

Access:

- **SSH tunnel (always):** `ssh -L 7701:127.0.0.1:7701 admin@server`, then
  `http://localhost:7701`. Only `localhost` host names are answered (DNS-rebinding
  protection).
- **Cloudflare Tunnel (optional):** `cloudflared` runs in the core stack and forwards the
  panel host name to `127.0.0.1:7700`. That listener accepts only requests carrying a
  valid Cloudflare Access JWT for the configured team and application (RS256, issuer,
  audience, expiry checked; keys fetched from Cloudflare), so a deleted or misconfigured
  Access application does not expose the login page.

Login is username + argon2id password + TOTP (RFC 6238, codes cannot be replayed) or a
single-use recovery code. Five failed logins lock the account (5 min, doubling); logins
are also rate-limited per IP and overall. Sessions are server-side, bound to the
listener and Cloudflare identity, idle-expire after 30 minutes and end after 12 hours.
Every form has a CSRF token and cross-site requests are refused. The only script is the
panel's own `live.js` (no inline script or style, no third parties): the
Content-Security-Policy is `default-src 'none'; script-src 'self'; connect-src 'self';
style-src 'self'`. Pages work without it and fall back to a plain refresh.

What the panel shows:

- **Server and containers:** CPU, RAM, swap, disk, load and per-container CPU, memory
  (against its limit), restarts and health, updating themselves every 10 seconds. A
  container's CPU and memory are read from its cgroup files, as `docker stats` computes
  them (CPU: share of one core over the 10 seconds); dockerd's stats request, which cost
  it a few milliseconds and a second or two of waiting per container, is left for where
  those files cannot be read.
  Background refreshes do not count as activity, so an open tab still logs out after 30
  idle minutes.
- **History:** the agent keeps one point per minute for a day and one per hour for a
  week (`/var/lib/footops/metrics.json`, saved every 5 minutes and on shutdown). Charts
  for the server and for each container answer "which container made memory grow".
  They are drawn on the server as SVG; hovering shows the values, and a table under
  each chart lists now, average and peak.
- **Alerts:** disk ≥ 80%, RAM ≥ 90%, swap ≥ 50%, CPU ≥ 90% for 5 minutes, a FootOps
  container that is not running, unhealthy, restarting, OOM-killed or near its memory
  limit, restarts within 15 minutes, certificates expiring within 30 days. Short spikes
  (under 30 seconds) are ignored. Alerts show as a count in the menu and a list on the
  dashboard.
- **Telegram (optional, Bildirimler):** alerts go to a linked chat when they open, get
  worse and clear. In that chat the person who linked it can ask **`/durum`** — CPU,
  load, memory, swap, disk, uptime, the sites, the last backup and the open alerts,
  with a **Yenile** button that brings the message up to date — and **`/siteler`**: the
  sites with an end date and the parked ones, with buttons to give more time, park or
  bring back (see [End date and parking](#end-date-and-parking)). Nobody else gets an
  answer, commands are rate-limited and audited, and nothing can be deleted from
  Telegram. **Give every server a bot of its own:** Telegram lets one reader at a time
  read a bot's messages; a second one is refused, and the panel says so after 10
  minutes. Reading costs one long-poll request about once a minute when idle.
- **Outside watcher (optional):** a Cloudflare Worker ([`worker/`](worker/README.md))
  visits the sites as a visitor would and reads the status file each server keeps next
  to its backups; it tells a Telegram chat (with a bot of its own, which only writes)
  when a site stops answering or a server falls silent. The panel shows what it sees,
  and alerts when it goes quiet.
- **Logs:** last 200-2000 lines, search (highlighted), warning/error filter, live follow.

Every login, logout, restart and recovery command is written to
`/var/lib/footops/audit.jsonl` and shown in the panel.

## Sites

A site is a container image served under one or more domains. From the panel
(**Sites → New site**):

1. **Certificates** first: paste a Cloudflare Origin CA certificate and key for the
   zone (`*.example.com` + `example.com`, 15 years). The panel checks that the key
   matches and shows which names it covers.
2. **New site**: name, group (`/srv/<group>/<name>`), domains, image (e.g. from GHCR),
   app port, memory limit, largest request body, optional persistent directory, optional
   MongoDB database, environment variables.

**Groups** are the folders under `/srv` (`core` is FootOps's own): the form offers the
ones there and takes a new name too, which makes the group's folder (root-only, `0700`).
A group goes with its last site: moving that site elsewhere, or removing it for good,
removes the empty folder.

**Largest request body** (MB, 1-100; 100 by default, what Cloudflare's Free and Pro
plans let through): Caddy answers a larger request `413` before the app has read more,
so an app that buffers bodies cannot be made to hold a large one. A site that takes no
uploads needs a few MB at most. Changing it touches only Caddy, not the container.

The agent then runs the steps as a background job (the page follows its progress) and
undoes everything if any step fails:

| Step | What happens |
|---|---|
| directory | `/srv/<group>/<name>/` with `footops.json` (no secrets) and `compose.yaml` (0600) |
| network | a Docker network of its own; Caddy (and MongoDB, if used) are attached to it — sites cannot reach each other |
| database | a MongoDB database and user of the site's name, `readWrite` on that database only; the connection string goes into the chosen variable (`MONGODB_URI` by default). The root password reaches `mongosh` via stdin, never a command line |
| image | pulled before anything starts |
| data | `data/` owned by the user the image runs as, mounted at the given path |
| container | no published ports, all capabilities dropped, `no-new-privileges`, memory and PID limits |
| ready | waits until the app accepts connections on its port; if it crashes, the error shows its last log lines |
| Caddy | site block with the matching certificate, `caddy validate` before `caddy reload` |
| check | an HTTPS request through Caddy must reach the app |

**Edit** changes domains, image, port, memory limit, the largest request body, the
persistent directory and environment variables. Current values are never sent to the browser: variables are added,
replaced or removed by name, and the audit log records only names. The database variable
stays managed by FootOps. If the site does not come back with the new settings, the old
files are restored and the old container is started again.

**Redeploy** pulls the image again and recreates the container if it changed.
**Remove** (confirmed by typing the name) takes the site offline and moves its files to
`/srv/<group>/_removed/`; the database is kept.

### End date and parking

A site may have an **end date** (site page → **Süre**; none by default). The linked
Telegram chat hears of it 7 and 3 days before and on the day (at 10:00 server time),
with **+1 ay** / **+1 yıl** buttons; on the day it is asked whether to **park** the site.
Without an answer the site stays up and the question comes again every morning: no
site goes offline without a yes. Every button asks once more, with its exact effect;
only that answer's **Evet** acts, once, within 5 minutes, and a button pressed after the
site changed does nothing.

**Parking** (from the chat or the site page) puts a neutral maintenance page in the
site's place — Caddy answers it itself, `503` with `Retry-After`; it says nothing about
why — and then stops the container (kept, with its image). Data and backups stay;
scheduled backups take only the site's definition while it is parked (its data do not
change; its last backups are kept). Redeploy, edit, group change and restart are refused
until the site is brought back (**Yayına al**, with a new end date or none), which
starts the container, waits for the app and restores the site's own Caddy block — or
stops it again and keeps the maintenance page if the app does not come up. The status
file marks a parked site, so the outside watcher leaves it alone.

Host names no site claims get their connection dropped. Caddy trusts Cloudflare's ranges
for the visitor's address (`CF-Connecting-IP`). In Cloudflare, point the domains at the
server (A record, proxied) and use SSL mode **Full (strict)**.

The agent re-attaches Caddy and MongoDB to every site network every 10 seconds, so
recreating the core containers heals itself.

## Recovering a server from its backups

Backups go to the server's own S3 bucket with restic (encrypted on the server). When a
server is lost, a new one is installed from them:

1. `sudo ./footops install` and answer **yes** to "Bu sunucu bir yedekten mi kurulacak?".
   Give the old server's code (backups are taken under it), the usual answers, and the
   repository: a key that can *read* the bucket is enough. On the Mac it is all one line
   for a single paste: FootOps.app's **Yedekler → Kurtarma satırını kopyala** (onto the
   pasteboard only, cleared a minute later), or `footops-mac yedek kurtarma h1 | pbcopy`.
2. After the usual steps (no tunnel, no new panel account: the old one comes back) the
   agent brings back the server's own part (certificates, the panel's login and 2FA,
   registry logins, backup settings) and then every site of the newest full backup:
   definition, a new database user, the database and the files *before* the container
   first starts, then Caddy and an HTTPS check. Every read is `--no-lock`; nothing is
   written to the repository. A site that fails is undone and the others go on; running
   the installer again brings back only what is missing.
3. The server is now a **rehearsal**: the tunnel's token, the repository's settings and
   the Telegram bot are kept aside (root only), no repository or bot can be set up, and
   one firewall rule (`DOCKER-USER`, tagged `footops-prova`) rejects every new
   connection from the sites' networks to the outside, so an app's mail, payments or
   marketplace updates do not happen twice while the old server still runs (at boot,
   `footops-prova.service` puts it in place before Docker starts the sites). Look around
   through the SSH tunnel and try the sites with `curl -k --resolve <domain>:443:<ip>`.
4. Once the old server is surely off: `sudo footops go-live` connects the repository
   again with the old server's write key (it asks for a new one if that key was revoked,
   and stops to ask if a backup newer than the recovery shows the old server still
   runs), puts Telegram, the tunnel and Access back, opens the sites to the outside and
   starts a first backup. Then point the domains' A records at the new address.

Once a month, after a clean night run, one site's newest backup (the sites take turns) is
restored where it harms nothing and checked: its database into a temporary database,
compared with what mongodump counted, and a sample of its files against the live ones.
A failure is an alert (and a Telegram message). **Yedekleme → Şimdi dene** or
`sudo footops backup trial [site]` runs it at once.

### On a Mac: FootOps.app and `footops-mac`

A menu bar app that shows how the servers are without reaching them: each server's
status file and the outside watcher's heartbeat, read from the buckets with the same
read-only key (four small reads every 5 minutes while the Mac is awake, and when the
menu opens on an older read). Its icon is green, yellow (worth a look), red (visitors
are hit, or a server went quiet) or gray (nothing known). The menu lists each server's
sites with what the watcher sees of them, opens the server's panel in the browser and
SSH in the Terminal, and on request visits every site from the Mac, side by side with
the watcher. It never talks to a panel and has no way to change a server.

Its **Yedekler** window opens the servers' backups: a server, a site, one of its
backups, then its parts into a folder of the owner's (`~/Downloads/<site>-<time>`:
`db.archive`, `files/`, `footops.json` and the site's `env` as the panel's download gives
it; kept out of Time Machine) or its database into the Mac's own MongoDB. It only reads
(a read-only key, every restic command `--no-lock`), so nothing it does can change or
remove a backup. What was opened is listed with its age — a week old, the menu says so
— and deleted from there; and it puts a server's recovery line on the pasteboard.

Its **Arşivler** window takes a FootOps archive (a panel's export, a backup's download),
a mongodump archive or an archive of files dropped on it, and checks it as a panel's
import would (the same code: type from the content, every checksum, paths, links,
decompression bombs) before anything is written; then opens it into a folder of the
owner's or loads its database into the Mac's MongoDB, its count compared with the
manifest's. The other way, it makes an archive for a panel's import of a database of the
Mac's MongoDB and a folder that is to be the site's persistent one (a new site: its
database from the Mac), and opens the chosen site's import page in the browser.

`make mac-install` builds `FootOps.app` (SwiftUI, the Go core `footops-mac` inside, signed
ad hoc for this Mac), puts it in Applications, links `footops-mac` into `~/.local/bin`
and opens it; from then on it starts with the Mac (its Settings turn that off). The
core works in the Terminal too (needs `brew install restic`; loading into MongoDB also
`mongodb-database-tools`):

```bash
footops-mac durum                          # the servers, as the menu shows them
footops-mac yokla                          # visit every site from this Mac now
footops-mac ayar                           # each server's panel, SSH target and bucket
footops-mac yedek liste h1                 # its sites and their newest backups
footops-mac yedek liste h1 magaza          # one site's backups
footops-mac yedek ac h1 magaza "2026-11-07 04:00"   # the backup begun that minute → ~/Downloads
footops-mac yedek mongo h1 magaza          # its database into the local MongoDB (magaza_yedek_<time>)
footops-mac yedek acilanlar                # what was opened on this Mac; delete with yedek sil
footops-mac yedek kurtarma h1 | pbcopy     # the recovery line for the installer
footops-mac arsiv dogrula x.footops.tar    # check an archive as a panel's import would
footops-mac arsiv ac x.footops.tar --mongo # into ~/Downloads/<site>-<time>, its database into MongoDB too
footops-mac arsiv olustur --db magaza --dosyalar ./uploads   # → ~/Downloads/magaza-<time>.footops.tar
```

The settings (no secrets) are in `~/Library/Application Support/FootOps/ayarlar.json`,
written by the app's Settings window through `footops-mac ayar yaz`. The secrets are in
the Keychain, service `footops-yedek`: `r2-hesap` (the R2 account), `r2-mac-erisim` and
`r2-mac-gizli` (the read-only key), `restic-<server>` (each repository's password),
optionally `kova-<server>`.

## Commands

| Command | Purpose |
|---|---|
| `footops install` | Prepare the server and install FootOps (interactive, root); also recovers a server from its backups |
| `footops go-live` | Make a server recovered from backups the live one (root) |
| `footops reset-password` | New panel password, lifts a lockout (root over SSH) |
| `footops reset-2fa` | Enrol a new authenticator, new recovery codes (root over SSH) |
| `footops rotate-mongo-root` | New MongoDB root password; verified before the secret file is replaced (root) |
| `footops agent` / `footops web` | The services (started by systemd) |
| `footops doctor` | Basic checks (more to come) |
| `footops self-update` | Not implemented yet |
| `footops version` | Print version |

## Development

```bash
make check   # go vet + tests
make build   # local binary
make linux   # dist/footops_linux_{amd64,arm64} + SHA256SUMS
make mac     # dist/FootOps.app (on a Mac: Swift and Go); swift test --package-path mac/app
```

Releases: push a `vX.Y.Z` tag; the Release workflow builds and publishes binaries.

**Never commit server-specific data** (IPs, domains, customer names, tokens). All of that
lives in the server's own configuration under `/etc/footops`.

## License

MIT
