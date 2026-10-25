# FootOps

Lightweight, security-first self-hosted server panel written in Go.

One command turns a fresh Ubuntu server into a hardened Docker host (swap, SSH hardening,
firewall, Docker, Caddy, MongoDB) and installs a small web panel for adding, deploying and
monitoring sites.

> **Status:** early development. The installer and panel are not usable yet.

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

## Commands

| Command | Purpose |
|---|---|
| `footops install` | Prepare the server and install FootOps (interactive, root) |
| `footops doctor` | Check the server against the FootOps standard |
| `footops reset-password` / `reset-2fa` | Recover panel access (root over SSH) |
| `footops self-update` | Download, verify and install the latest release |
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
