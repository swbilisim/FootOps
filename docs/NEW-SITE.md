# Publishing a site

**English** · [Türkçe](NEW-SITE-TR.md)

From code on your computer to `https://example.com` on a FootOps server. The panel does
most of the work: **Siteler** → **Projeni hazırla** (Prepare your project) writes the
files your project needs, and **Yeni site** (New site) runs it. About 45 minutes the first
time, a few minutes for each change after that.

```
code on your computer ──git push──▶ GitHub ──Actions builds──▶ GHCR (image) ──panel pulls──▶ server
your local database ──archive──▶ panel → İçe aktar (Import)
Cloudflare: certificate (once per domain) and DNS (last)
```

**Before you start:**

- A server with FootOps: [Installing FootOps](INSTALL.md).
- `git` and the GitHub CLI, `gh` ([cli.github.com](https://cli.github.com)), logged in
  once with `gh auth login`. Docker is not needed on your computer: GitHub builds the image.
- The project builds on your computer (`npm run build`).
- A name: lower-case letters, digits and dashes, e.g. `coffee-shop`. The GitHub repository,
  the image and the site in the panel all take it.

FootOps runs **Next.js**, **Node.js** (Express and the like) and **static** sites (plain
HTML, or built with Vite, Astro, a static Next.js export…). A site that needs a database
gets a MongoDB database and user of its own from the panel.

## 1. Prepare the project: Projeni hazırla

Panel → **Siteler** (Sites) → **Projeni hazırla** (Prepare your project):

1. The kind: **Next.js**, **Node.js / Express** or **Statik site** (static site).
2. The form:
   - **GitHub hesabı** (GitHub account): yours or an organisation's; the repository goes
     there.
   - **Proje adı** (Project name).
   - **Paket yöneticisi** (Package manager): npm or pnpm, as your lock file says.
   - **Port**: what the app listens on, usually 3000 (Next.js and Node.js).
   - **Yükleme klasörü** (Upload folder, optional; Next.js and Node.js): if the site stores
     uploaded files (images, documents), the folder it writes them to, e.g. `/app/uploads`
     (the project is in `/app` in the container). Left empty, the app can write nowhere.
   - **Başlatma dosyası** (Start file, Node.js): the file node runs, e.g. `server.js`.
   - **Derleme çıktısı** (Build output, optional; static sites): `dist` (Vite, Astro),
     `out` (a static Next.js export), `build` (Create React App). Empty publishes the
     project folder as it is.
3. **Dosyaları üret** (Generate files) gives three files: `Dockerfile`, `.dockerignore`
   and `.github/workflows/docker.yml`. **Zip indir** (Download zip), then in a terminal,
   in the project's folder (next to `package.json`):
   ```bash
   unzip ~/Downloads/<name>-footops.zip
   ```
   (A double-click unpacks it into a new folder instead, and macOS hides the files whose
   names start with a dot.) Or copy each file (**Kopyala**, Copy). They replace files of the
   same name: look at yours first.

What the files do (the templates are in
[internal/scaffold/files](../internal/scaffold/files); the panel fills them in):

- Next.js and Node.js images run as an unprivileged user (`app`, uid 1001), a static
  site's nginx as its own unprivileged user. The code belongs to root and the app cannot
  change it; it can write only to its upload folder (and Next.js to its image
  cache). The upload folder is written into the image as a label, so the panel fills in
  the persistent folder by itself.
- `.dockerignore` keeps `.env*` files out of the image (`.env.production` goes in for
  Next.js and built static sites), and `.git`, `node_modules` and key files with them.
- The workflow builds on every push to `main` and pushes two tags: `:main`, which the
  server pulls, and `:sha-<short commit>` (e.g. `:sha-abc1234`), to go back to. The image is built for the server's
  architecture: amd64, or arm64 on GitHub's arm64 runner.

Then go through the checklist the page shows for your kind. The main points:

| | |
|---|---|
| Next.js | `output: 'standalone'` in `next.config`. A `public/` folder (an empty one with a `.gitkeep` will do). There is no database while the image is built: a page that reads it while building needs `export const dynamic = 'force-dynamic'`. |
| Node.js | Listen on `process.env.PORT` on every address: `app.listen(process.env.PORT)`, not `'localhost'`. Packages needed at run time go in `dependencies`. The health check visits `/`: an API without `/` names another path in the Dockerfile's `HEALTHCHECK`. |
| Static | An `index.html` in the root (or in the build output). In a single-page app, addresses other than `/` give 404 when opened directly: pick the Node.js kind for it, or add an nginx setting. |
| Database | The address comes from an environment variable (`process.env.MONGODB_URI`), never `mongodb://localhost…` in the code. |
| Uploads (Next.js) | Next.js doesn't serve files written into `public/` after it started (404 until a restart): write uploads to `/app/uploads` and serve them from a route, e.g. `app/uploads/[...path]/route.ts`, that reads the file from disk. Refuse paths with `..` and serve only the extensions you allow. |
| Lock file | `package-lock.json` or `pnpm-lock.yaml` is committed and matches `package.json`. If `npm run build` fails on your computer, it fails on GitHub too. |
| `.gitignore` | Lists `node_modules` and `.env*` (and `.next` for Next.js). If `.env.production` is to be committed, add `!.env.production` below `.env*`, or `git add .` leaves it out and the build gets no public values. |

**Environment variables come in three kinds:**

| Kind | Example | Where it goes |
|---|---|---|
| Secrets and server settings | `JWT_SECRET`, API keys, SMTP | They stay in `.env.local`, out of the repository and the image. The panel takes them when the site is created (**.env dosyasından ekle**, Add from a .env file). |
| Public values baked in at build time | `NEXT_PUBLIC_*`, `VITE_*` | In `.env.production`, committed. Only what the browser may see anyway. The panel cannot change them: commit, push and redeploy. |
| The database address | `MONGODB_URI` | Nowhere: the panel makes the database and sets it. |

## 2. Push to GitHub

In the project's folder:

```bash
git init -b main
git add .
git status            # .env.local, node_modules and .next must not be listed
git commit -m "First version"
gh repo create <account>/<name> --private --source . --push
gh run watch
```

If the project already has a repository, commit the three files:

```bash
git add Dockerfile .dockerignore .github/workflows/docker.yml
git commit -m "FootOps: Docker image"
git push
gh run watch
```

A last look at what went in: `git ls-files | grep -i env` lists nothing but
`.env.production` (if you have one). The repository's default branch must be `main`.

GitHub Actions builds the image in 3-5 minutes. Green: the image is
`ghcr.io/<account>/<name>:main`. Red: `gh run view --log-failed`, and see
[Troubleshooting](#troubleshooting).

The image of a private repository is private: the server needs an image login
(**İmaj girişleri**; [Installing FootOps](INSTALL.md), section 5.2).

## 3. Domain and certificate (once per domain)

If **Sertifikalar** (Certificates) already lists a certificate for the domain, skip this.

1. If the domain is not on Cloudflare yet: Cloudflare → **Add a domain** → the Free plan →
   at your registrar, replace the nameservers with the two Cloudflare gives → wait until
   the zone is **Active** (minutes to a few hours). Don't add DNS records yet; they come
   last.
2. An Origin certificate for it, pasted into **Sertifikalar**, and SSL/TLS mode **Full
   (strict)**: [Installing FootOps](INSTALL.md), section 5.1.

## 4. Create the site: Yeni site

At the bottom of **Projeni hazırla**, **Bu imajla yeni site** (New site with this image)
opens the form with the image filled in. Or: **Siteler** → **Yeni site**.

1. **Uygulama türü** (Application type), and the image `ghcr.io/<account>/<name>:main` →
   **İmajı kontrol et** (Check the image). The server pulls it and looks inside: does it
   match the server's architecture, which port and user does it declare, which upload
   folder. Seconds to a few minutes.
2. The form; the column on the right explains each field:

| Field | What to write |
|---|---|
| **Proje adı** (Project name) | `<name>` |
| **Grup** (Group) | A folder for sites that belong together (`/srv/<group>/<name>`): pick one or type a new name, e.g. a customer's |
| **Alan adları** (Domains) | One per line: `example.com`, `www.example.com` |
| **Uygulama portu** (App port) | Filled in from the image, usually `3000` |
| **Bellek sınırı (MB)** (Memory limit) | e.g. `512` for a Next.js site; past it the container restarts. It can be changed later |
| **En büyük istek (MB)** (Largest request) | A few MB for a site without uploads; up to 100 with them |
| **Kalıcı klasör** (Persistent folder) | Filled in from the image's upload folder; empty if the site writes no files |
| **Veritabanı** (Database) | **MongoDB veritabanı ve kullanıcısı oluştur** (Create a MongoDB database and user) is ticked for Next.js and Node.js; untick it if the site has no database. The variable is the name your code reads (`MONGODB_URI`); it cannot be changed later |
| **Ortam değişkenleri** (Environment variables) | **.env dosyasından ekle** → your `.env.local`. The browser reads it and fills the list; the file itself is not uploaded. The local database address in it is skipped: the panel sets the server's |

3. **Özet** (Summary): values are not shown, only names and warnings. Read them →
   **Siteyi oluştur** (Create the site).
4. The panel runs the steps (folder, network, database, image, container, Caddy, an HTTPS
   check) and undoes them all if one fails. For a week the site's page then opens with
   **Sıradaki adımlar** (Next steps). The site doesn't open from outside yet: there is no
   DNS record.

## 5. Bring the data

If the site has a database or uploaded files, make an archive:

- **On any computer** with MongoDB's
  [Database Tools](https://www.mongodb.com/try/download/database-tools):
  `mongodump --archive=data.gz --gzip --db <local-database>`. The local database's name
  may differ from the site's; the panel maps it. Uploaded files: a zip or tar.gz of the
  folder.
- **On a Mac with FootOps.app:** **Arşivler** (Archives) → **Arşiv oluştur** (Create an
  archive): the database and the upload folder in one `.footops.tar`.

Panel → the site → **Veri aktarımı** (Data transfer) → **İçe aktar…** (Import) → the file
→ **Yükle ve kontrol et** (Upload and check). The site keeps running while the archive is
checked: its database is loaded into a temporary one and counted. The preview shows the
collections, documents and files; confirm. The site stops for a moment: what will change
is copied first, the data are loaded and counted, and the site starts again. If anything
goes wrong, the copies go back by themselves. A database and a files archive are imported
one after the other.

## 6. DNS: open the site (last)

The site's page → **Cloudflare'de yapılacaklar** (To do in Cloudflare) lists the exact
records. Usually:

1. Cloudflare → the domain → **DNS** → **Records** → **Add record**: type `A`, name `@`,
   IPv4 address = the server's address, **Proxied** (the orange cloud) on. Then type
   `CNAME`, name `www`, target `example.com`, Proxied on.
2. **SSL/TLS** → **Overview**: **Full (strict)**. **SSL/TLS** → **Edge Certificates** →
   **Always Use HTTPS** on.
3. Panel → the site → **DNS'i kontrol et** (Check DNS): every domain ✓, an HTTPS answer
   through Cloudflare. Then open the site and try the basics once: log in, send a form,
   upload an image.

From now on the site is in the night backup, and the outside watcher (if you set it up)
visits it and tells Telegram when it stops answering.

## Day to day

| Task | How |
|---|---|
| Ship new code | `git push` → Actions green (`gh run watch`) → panel → the site → **Yeniden dağıt** (Redeploy): pulls `:main` again and replaces the container; a few seconds' interruption |
| Change environment variables | Panel → the site → **Düzenle** (Edit) → environment variables → save. The container is recreated; values are never shown back |
| Change a `NEXT_PUBLIC_*` value | Edit `.env.production` → commit, push → **Yeniden dağıt** |
| Go back to an earlier version | **Düzenle** → image `ghcr.io/<account>/<name>:sha-<short commit>` (e.g. `:sha-abc1234`) → save. The tags are on GitHub, under the repository's Packages. Back to the newest: `:main` again |
| The site is down | Panel → the site → **Loglar** (Logs); **Yeniden başlat** (Restart) if needed |
| Look at or fix data | Panel → the site → **Veritabanı** (Database) |
| Uploaded files | Panel → the site → **Dosyalar** (Files) |
| Restore a backup | Panel → the site → **Yedekler** (Backups) → a backup → **Aç ve incele** (Open and inspect) first, then **Geri yükle…** (Restore) |
| Take it offline for a while | **Parka al…** (Park): a maintenance page takes its place, data and backups stay; **Yayına al…** (Bring back) |
| Remove it | **Tehlikeli bölge** (Danger zone) → **Siteyi kaldır…** (Remove the site): its files move to `_removed`, the database stays. Then delete its DNS records |

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Actions red: `"/package-lock.json": not found`, or `npm ci` fails | The lock file is not committed, or is out of step with `package.json`: run `npm install` locally and commit `package-lock.json` |
| Actions red: `MongoNetworkError` or `ECONNREFUSED` while building | A page reads the database while the image is built, and there is none: `export const dynamic = 'force-dynamic'` in that page |
| Actions red: `"/app/public": not found` | No `public/` folder: add an empty one with a `.gitkeep` |
| Image check: no login, or access denied | Wrong image name (lower case?), Actions not finished yet, or the image login's token expired (**İmaj girişleri**) |
| Image check: the architecture does not match | The image was built for another architecture, e.g. pushed from an Apple silicon Mac (arm64) to an amd64 server: let the workflow build it (**Projeni hazırla** writes it for the server's architecture) |
| Creating the site stops while waiting for the app (*… portunu dinlemeye başlamadı* or *container çalışmıyor*, with its last log lines) and everything is undone | The app listens on another port or only on localhost (Next.js needs `HOSTNAME=0.0.0.0`, which the generated Dockerfile sets), or it stops at start, often for a missing environment variable: fix it, push, and create the site again |
| An uploaded image gives 404 until a restart | Next.js doesn't serve what is written into `public/` later: keep uploads out of `public/` and serve them from a route (section 1) |
| `EACCES` or `read-only` in the logs | The app writes where it may not: make that folder the upload folder (and the persistent folder) |
| Uploaded files are gone after a redeploy | No persistent folder, or the wrong path: **Düzenle** → **Kalıcı klasör** |
| The database looks empty or the app cannot connect | The code reads another variable than the one the panel set (the site's page shows it, e.g. `MONGODB_URI`): make the code read that one, push, **Yeniden dağıt** |
| Cloudflare **520** or **525** | The server does not serve that host name: it is not among the site's domains (**Düzenle**), or the A record points at another server |
| Cloudflare **521** or **522** | Cloudflare cannot reach the server on port 443: the A record points elsewhere, the provider's firewall blocks 443, or SSL/TLS is not Full (strict) (Flexible goes to port 80, which is closed): section 6 |
| The browser says the certificate is not trusted | Proxied is off: only Cloudflare trusts the Origin certificate. Turn the orange cloud on |
| A `NEXT_PUBLIC_*` value is old | It is baked in at build time: `.env.production`, push, **Yeniden dağıt** |
