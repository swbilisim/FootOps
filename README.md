# FootOps

Lightweight, security-first self-hosted server panel written in Go.

One command turns a fresh Ubuntu server into a hardened Docker host (swap, SSH hardening,
firewall, Docker, Caddy, MongoDB) and installs a small web panel for adding, deploying and
monitoring sites.

> **Status:** early development. The installer and panel work end to end in tests;
> they have not run on a production server yet.

## Design in short

- **Single binary.** `footops` is both the installer and the panel. No runtime dependencies on the host besides Docker.
- **Per-server.** Each server runs its own FootOps; there is no central control plane.
- **Never exposed to the internet.** The panel listens on `127.0.0.1` only and is reached through Cloudflare Tunnel + Cloudflare Access, then its own password + TOTP login. An SSH tunnel is the break-glass path.
- **Privilege separation.** `footops-web` (the UI) runs unprivileged and can only ask `footops-agent` to perform a fixed set of validated actions. A bug in the UI cannot be turned into arbitrary root commands.
- **No ports per app.** Only Caddy listens on 80/443. Apps join per-project Docker networks and are reached by container name, so there is no port table to maintain.
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
| firewall | UFW: deny incoming, allow 22/80/443 |
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
| Caddy | `caddy:2.11.7-alpine`, all capabilities dropped except binding 80/443 | 80, 443 |
| MongoDB | `mongo:7.0.45`, auth on, WiredTiger cache sized from RAM | `127.0.0.1:27017` only |

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
| `footops-agent` | root | A fixed, validated API on `/run/footops/agent.sock`: overview, container logs, restart, audit log. Accepts connections only from root and the `footops` uid, checked with `SO_PEERCRED`. |
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
Every form has a CSRF token and cross-site requests are refused. The UI ships no
JavaScript, so the Content-Security-Policy is `default-src 'none'`.

Every login, logout, restart and recovery command is written to
`/var/lib/footops/audit.jsonl` and shown in the panel.

## Sites

A site is a container image served under one or more domains. From the panel
(**Sites → New site**):

1. **Certificates** first: paste a Cloudflare Origin CA certificate and key for the
   zone (`*.example.com` + `example.com`, 15 years). The panel checks that the key
   matches and shows which names it covers.
2. **New site**: name, group (`/srv/<group>/<name>`), domains, image (e.g. from GHCR),
   app port, memory limit, optional persistent directory, optional MongoDB database,
   environment variables.

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

**Redeploy** pulls the image again and recreates the container if it changed.
**Remove** (confirmed by typing the name) takes the site offline and moves its files to
`/srv/<group>/_removed/`; the database is kept.

Host names no site claims get their connection dropped. Caddy trusts Cloudflare's ranges
for the visitor's address (`CF-Connecting-IP`). In Cloudflare, point the domains at the
server (A record, proxied) and use SSL mode **Full (strict)**.

The agent re-attaches Caddy and MongoDB to every site network every 10 seconds, so
recreating the core containers heals itself.

## Commands

| Command | Purpose |
|---|---|
| `footops install` | Prepare the server and install FootOps (interactive, root) |
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
```

Releases: push a `vX.Y.Z` tag; the Release workflow builds and publishes binaries.

**Never commit server-specific data** (IPs, domains, customer names, tokens). All of that
lives in the server's own configuration under `/etc/footops`.

## License

MIT
