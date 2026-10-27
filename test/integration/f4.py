#!/usr/bin/env python3
"""F4a scenarios (moving a site from another server) against a real install. A "source
server" runs as a container inside the test server's Docker: sshd, MongoDB without
authentication (like the old servers), nginx, an app folder with uploads, a hidden file,
a link and a FIFO. The panel registers it, its key is added there with the panel's own
command, the first connection pins the host key; discovery and the app scan run
read-only (no secret leaves the source); the pre-copy brings the database and the
uploads over ssh into a site; again after the source changed; a copy whose connection
drops half-way changes nothing; a changed host key is refused; removing the source takes
the key off it.

Usage: f4.py <container> <login-file>
"""
import json
import shlex
import sys
import time

from itlib import ACTOR, api, fail, ok, rss, sh, tools, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itmig", "mig.it.example.com"  # covered by f0's *.it.example.com certificate
DATA = "/srv/musteri/%s/data" % SITE
SRC, NET = "it-source", "it-src"
APP = "/home/eski/www"
UP = APP + "/public/uploads"
SRCDB = "itmig_eski"
SECRETS = ("SuperGizliSifre99", "cok-gizli-deger-123", "ortamdaki-gizli-deger")
measure = {}

DOCKERFILE = """FROM mongo:7.0.45
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server nginx procps \\
 && rm -rf /var/lib/apt/lists/* /etc/nginx/sites-enabled/default && mkdir -p /run/sshd /root/.ssh && chmod 700 /root/.ssh
ENTRYPOINT []
CMD ["/bin/sh", "-c", "mongod --bind_ip 127.0.0.1 --fork --logpath /var/log/mongod.log --dbpath /data/db && nginx && exec /usr/sbin/sshd -D -e"]
"""

SEED = r"""set -e
rm -rf /home/eski && mkdir -p /home/eski/www/public/uploads/blog/2026 /home/eski/www/lib
cd /home/eski/www/public/uploads
for i in $(seq 1 120); do head -c $((20000 + i * 997)) /dev/urandom > "foto$i.jpg"; done
for i in 1 2 3; do head -c 300000 /dev/urandom > "blog/2026/kapak $i.png"; done
printf 'çay' > "blog/çay ve şeker.txt"
mkdir bos
echo gizli > .htaccess
mkdir .cache && echo x > .cache/a
ln -s /etc/passwd parola-baglantisi
mkfifo boru
cd /home/eski/www
cat > package.json <<'EOF'
{
  "name": "eski-magaza",
  "main": "app.js",
  "engines": {
    "node": ">=14"
  },
  "dependencies": {
    "express": "^4.17.1",
    "mongoose": "^5.9.6"
  }
}
EOF
printf 'DB_URI=mongodb://localhost:27017/itmig_eski\nSECRET_KEY=cok-gizli-deger-123\n' > .env
printf "const pass = 'SuperGizliSifre99';\nfetch('http://203.0.113.20:8196/graphql')\n" > lib/mail.js
cat > /etc/nginx/conf.d/mig.conf <<'EOF'
server {
    listen 80;
    server_name mig.it.example.com www.mig.it.example.com;
    root /home/eski/www/public;
    location /uploads/ { alias /home/eski/www/public/uploads/; }
    location /api/ { proxy_pass http://127.0.0.1:3000; }
}
EOF
nginx -s reload
"""

SEED_DB = """const d = db.getSiblingDB("itmig_eski"); d.dropDatabase();
d.urunler.insertMany(Array.from({length: 1500}, (_, i) => ({ad: "ürün " + i, fiyat: i})));
d.yorumlar.insertMany(Array.from({length: 40}, (_, i) => ({metin: "yorum " + i})));
d.createView("ucuzlar", "urunler", [{$match: {fiyat: {$lt: 10}}}]);
print("OK");
"""

# A tar that stops after 2 MB for a minute, so the connection can be cut half-way.
SLOW_TAR = """#!/bin/sh
case " $* " in *" -cf "*) /usr/bin/tar "$@" | { head -c 2000000; sleep 60; cat; }; exit 0 ;; esac
exec /usr/bin/tar "$@"
"""


def src(cmd, stdin=None, check=True):
    """A command on the source server (a container in the test server's Docker)."""
    # HOME as in root's login (the mongo image sets it to /data/db).
    return sh("docker exec -i -e HOME=/root %s sh -c %s" % (SRC, shlex.quote(cmd)), stdin=stdin, check=check)


def site_counts():
    out = sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin", stdin=(
        'const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);const d=c.getDB(%s);const o={};'
        'd.getCollectionInfos().forEach(i=>{if(!i.name.startsWith("system."))o[i.name]=i.type==="view"?"view":d.getCollection(i.name).countDocuments({})});'
        'print("C "+JSON.stringify(o))' % (json.dumps(root_pw), json.dumps(SITE))).encode())
    return json.loads(out.split("C ", 1)[1].split("\n")[0])


def tree(where, cmd):
    """The folders and regular files of a folder with their checksums, hidden entries
    left out: what the copy takes (links and FIFOs are neither folders nor files)."""
    return cmd("cd %s && find . -mindepth 1 -name '.*' -prune -o -type d -printf 'd %%P\\n' -o -type f -printf 'f %%P\\n' | sort && "
               "find . -mindepth 1 -name '.*' -prune -o -type f -exec sha256sum {} + | sort -k2" % where)


def site_tree():
    return tree(DATA, sh)


def source_tree():
    return tree(UP, src)


expected_tree = source_tree


def job_text(j):
    return "\n".join(j["steps"]) + "\n" + (j.get("error") or "")


def clean(what):
    if tools():
        fail("%s: tool container left: %s" % (what, tools()))
    for unit in ("footops-ssh-*", "footops-files-*"):
        if sh("systemctl list-units --all --no-legend '%s' 2>/dev/null" % unit).strip():
            fail("%s: scope left: %s" % (what, sh("systemctl list-units --all --no-legend '%s'" % unit)))
    if sh("pgrep -f '[s]sh -F /dev/null' || true").strip():  # [s]: not the shell running pgrep
        fail("%s: ssh process left" % what)
    left = [d for d in sh("ls -A /var/lib/footops/staging 2>/dev/null || true").split() if d.startswith("mig-")]
    if left:
        fail("%s: staging left: %s" % (what, left))
    if sh("ls -A /var/lib/footops/recovery 2>/dev/null || true").strip() not in ("", "temp-databases"):
        fail("%s: recovery record left" % what)
    if sh("test -e /srv/musteri/%s/data.new && echo y || true" % SITE).strip():
        fail("%s: data.new left" % what)


def copy_job(expect_error=False):
    _, r = api("POST", "/v1/sites/%s/migration/copy" % SITE, {"confirm": SITE, "actor": ACTOR})
    t0 = time.time()
    j = wait_job(r["job_id"], timeout=900)
    if bool(j.get("error")) != expect_error:
        fail("copy: %s\n%s" % (j.get("error"), "\n".join(j["steps"])))
    return j, time.time() - t0


def leaked():
    """Places on the test server where a secret of the source turned up."""
    found = sh("grep -rl %s /var/lib/footops 2>/dev/null || true" % " ".join("-e " + s for s in SECRETS)).split()
    if sh("journalctl -u footops-agent -u footops-web --no-pager 2>/dev/null | grep -c %s || true"
          % " ".join("-e " + x for x in SECRETS)).strip() not in ("", "0"):
        found.append("journal")
    return found


print("F4a senaryoları (sunucudan taşıma)")
root_pw = sh("sed -n 's/^MONGO_INITDB_ROOT_PASSWORD=//p' /etc/footops/secrets/mongo.env").strip()

# ---- the source server ----
if sh("docker image inspect %s >/dev/null 2>&1 && echo y || true" % SRC).strip() != "y":
    t0 = time.time()
    sh("docker build -q -t %s - >/dev/null" % SRC, stdin=DOCKERFILE.encode())
    ok("kaynak sunucu imajı hazırlandı (mongo 7.0.45 + sshd + nginx, %.0f sn)" % (time.time() - t0))
sh("docker network inspect %s >/dev/null 2>&1 || docker network create %s >/dev/null" % (NET, NET))
sh("docker rm -f %s >/dev/null 2>&1 || true" % SRC)
sh("docker run -d --name %s --hostname eski --network %s %s >/dev/null" % (SRC, NET, SRC))
wait(lambda: "OK" in src("mongosh --quiet --eval 'print(\"OK\")'", check=False), "mongod on the source", timeout=60)
wait(lambda: src("cat /proc/1/comm", check=False).strip() == "sshd", "sshd on the source", timeout=30)
src(SEED)
if "OK" not in src("mongosh --quiet --file /dev/stdin", stdin=SEED_DB.encode()):
    fail("source database not seeded")
# The app, as PM2 runs it: a "node" (a copy of sleep) in the app folder, port 3000 (nginx
# proxies /api/ there), and a secret in its environment that must not leave the source.
src("mkdir -p /opt/fake && cp /bin/sleep /opt/fake/node")
sh("docker exec -d -w %s -e name=eski -e pm_id=0 -e PORT=3000 -e PM2_HOME=/root/.pm2 -e SECRET=ortamdaki-gizli-deger %s /opt/fake/node 100000"
   % (APP, SRC))
ip = sh("docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' %s" % SRC).strip()
ok("kaynak sunucu çalışıyor (%s): MongoDB'de %s (1500 + 40 belge, bir görünüm), %s altında 124 dosya + gizli dosya, bağlantı, FIFO" % (ip, SRCDB, UP))

# ---- the target site: a database and a data directory (nginx's web root, uid 101) ----
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                             "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                             "data_mount": "/usr/share/nginx/html", "database": True}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    ok("hedef site oluşturuldu (veritabanı + kalıcı klasör, nginx uid 101)")
# A fresh start: no move, no sources, no copies from earlier runs.
sh("rm -rf /var/lib/footops/migrations/%s /srv/musteri/%s/data.old-* /srv/musteri/%s/data.failed-*; docker start %s >/dev/null"
   % (SITE, SITE, SITE, SITE))
_, sources = api("GET", "/v1/sources")
for s in sources:
    api("POST", "/v1/sources/%s/remove" % s["id"], {"cleanup": False, "actor": ACTOR})
sh("find %s -mindepth 1 -delete" % DATA)
measure["agent_rss_before"] = rss("footops agent")

# ---- register the source, add its key there, connect ----
code, bad = api("POST", "/v1/sources", {"label": "x", "host": "-oProxyCommand=id", "port": 22, "user": "root", "actor": ACTOR}, check=False)
if code != 422 or not bad.get("problems"):
    fail("a bad host was accepted: %d %s" % (code, bad))
_, s = api("POST", "/v1/sources", {"label": "eski sunucu", "host": ip, "port": 22, "user": "root", "actor": ACTOR})
sid = s["id"]
if not s["authorized"].startswith("restrict ") or not s["public_key"].startswith("ssh-ed25519 ") or s.get("host_keys"):
    fail("new source: %s" % s)
perms = sh("stat -c '%%a %%U' /var/lib/footops/sources/%s /var/lib/footops/sources/%s/id_ed25519" % (sid, sid)).split()
if perms != ["700", "root", "600", "root"]:
    fail("key permissions: %s" % perms)
code, _ = api("POST", "/v1/sources/%s/test" % sid, {"actor": ACTOR}, check=False)
if code != 502:
    fail("connected before the key was added: %d" % code)
src(s["install"])  # what the operator pastes on the source
if s["authorized"] not in src("cat /root/.ssh/authorized_keys"):
    fail("install command did not add the line")
_, s = api("POST", "/v1/sources/%s/test" % sid, {"actor": ACTOR})
want_fp = src("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub").split()[1]
if s.get("host_keys") != ["ED25519 " + want_fp] or "Ubuntu" not in s.get("system", ""):
    fail("after test: %s (source key %s)" % (s, want_fp))
ok("kaynak eklendi, anahtar panelin komutuyla kaynağa kondu; ilk bağlantı sunucu anahtarını kaydetti (%s)" % want_fp)

# ---- discovery and the app scan (read-only) ----
api("POST", "/v1/sites/%s/migration" % SITE, {"choice": {"source": sid, "source_only": True}, "actor": ACTOR})
before = source_tree()
_, r = api("POST", "/v1/sites/%s/migration/discover" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("discover: %s" % job_text(j))
_, v = api("GET", "/v1/sites/%s/migration" % SITE)
d = v["discovery"]
dbs = {db["name"]: db for m in d["mongo"] for db in m["dbs"]}
# dbStats counts the view's definition (system.views) as an object too.
if SRCDB not in dbs or dbs[SRCDB]["objects"] != 1541 or not d["complete"]:
    fail("discovery databases: %s" % d["mongo"])
if not any(DOMAIN in w["names"] for w in d["web"]) or {"mongodump", "nginx", "tar", "find"} - {t["name"] for t in d["tools"]}:
    fail("discovery web/tools: %s %s" % (d["web"], d["tools"]))
sg = v["suggest"]
if sg.get("files_dir") != UP or (sg.get("db") or {}).get("name") != SRCDB or sg.get("app_dir") != APP:
    fail("suggestion: %s" % sg)
app = [p for p in d["procs"] if p.get("cwd") == APP]
if len(app) != 1 or app[0].get("pm2_name") != "eski" or app[0].get("port") != "3000":
    fail("node process not found: %s" % d["procs"])
cand = [c for c in d["candidates"] if c["dir"] == UP]
if len(cand) != 1 or cand[0]["files"] != 124 or cand[0]["links"] != 1 or cand[0].get("err"):
    fail("uploads candidate: %s" % d["candidates"])
ok("keşif: MongoDB %s (%d belge), nginx'te bu sitenin alan adı → port 3000 → PM2 uygulaması “eski” (%s); öneri: veritabanı %s, klasör %s (124 dosya)"
   % (d["mongo"][0].get("version"), dbs[SRCDB]["objects"], APP, SRCDB, UP))
api("POST", "/v1/sites/%s/migration" % SITE, {"choice": {"app_dir": APP}, "actor": ACTOR})
_, r = api("POST", "/v1/sites/%s/migration/scan" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("scan: %s" % job_text(j))
_, v = api("GET", "/v1/sites/%s/migration" % SITE)
sc = v["scan"]
kinds = {f["kind"]: f["where"] for f in sc["secrets"]}
if sc["package"]["name"] != "eski-magaza" or sc["env"][0]["keys"] != ["DB_URI", "SECRET_KEY"] or kinds.get("password") != "lib/mail.js:1" \
        or [r["value"] for r in sc["db_refs"]] != [SRCDB] or [u["value"] for u in sc["urls"]] != ["http://203.0.113.20:8196"]:
    fail("scan: %s" % json.dumps(sc)[:800])
if leaked():
    fail("a secret of the source reached the test server: %s" % leaked())
if source_tree() != before:
    fail("discovery or scan changed the source")
ok("uygulama incelemesi: package.json, .env değişken adları, sır olabilecek yer (dosya:satır), IP'li adres, koddaki veritabanı; değerler sunucuya hiç gelmedi, kaynak değişmedi")

# ---- the pre-copy ----
api("POST", "/v1/sites/%s/migration" % SITE, {"choice": {"db": SRCDB, "files_dir": UP, "app_dir": APP}, "actor": ACTOR})
code, _ = api("POST", "/v1/sites/%s/migration/copy" % SITE, {"confirm": "yanlis", "actor": ACTOR}, check=False)
if code != 422:
    fail("copy without confirmation: %d" % code)
j, wall = copy_job()
print("    " + "\n    ".join(j["steps"]))
measure.update(copy_wall_s=round(wall, 1), copy_peak=j["usage"].get("peak_mem"), copy_cpu_ns=j["usage"].get("cpu"))
got = site_counts()
if got != {"urunler": 1500, "yorumlar": 40, "ucuzlar": "view"}:
    fail("site database: %s" % got)
if site_tree() != expected_tree():
    fail("site files differ:\n%s\n---\n%s" % (site_tree()[:1500], expected_tree()[:1500]))
for line in sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%P\\n'" % DATA).splitlines():
    typ, own, mode, name = line.split(" ", 3)
    if own != "101:101" or (typ == "f" and mode != "644") or (typ == "d" and mode != "755"):
        fail("ownership/mode of %s: %s %s" % (name, own, mode))
served = sh("curl -sk --resolve %s:443:127.0.0.1 https://%s/foto7.jpg | sha256sum" % (DOMAIN, DOMAIN)).split()[0]
if served != src("sha256sum %s/foto7.jpg" % UP).split()[0]:
    fail("copied file not served as on the source")
_, v = api("GET", "/v1/sites/%s/migration" % SITE)
last = v["migration"]["copies"][-1]
if last.get("error") or sum(last["source_docs"].values()) != 1540 or sum(last["site_docs"].values()) != 1540 \
        or last["source_files"]["files"] != 124 or last["site_files"]["files"] != 124 or last["source_files"]["links"] != 1:
    fail("copy record: %s" % last)
_, snaps = api("GET", "/v1/sites/%s/snapshots" % SITE)
if not snaps or snaps[0]["reason"] != "before-migrate":
    fail("no safety copy: %s" % snaps)
clean("copy")
if leaked():
    fail("a secret of the source reached the test server: %s" % leaked())
ok("ön kopya: veritabanı 1540/1540 belge + görünüm, 124 dosya birebir (sha256), sahibi 101, 644/755; gizli dosya, bağlantı ve FIFO alınmadı; "
   "Caddy dosyayı sunuyor; %.1f sn, tepe %d MB" % (wall, (j["usage"].get("peak_mem") or 0) >> 20))

# ---- the source changes, copy again ----
src("rm %s/foto1.jpg && head -c 50000 /dev/urandom > %s/yeni.jpg" % (UP, UP))
src("mongosh --quiet --eval 'db.getSiblingDB(\"%s\").yorumlar.insertMany(Array.from({length: 10}, (_, i) => ({metin: \"yeni \" + i})))' >/dev/null" % SRCDB)
j, wall = copy_job()
if site_counts().get("yorumlar") != 50 or site_tree() != expected_tree():
    fail("second copy: %s" % site_counts())
measure["recopy_wall_s"] = round(wall, 1)
clean("second copy")
ok("kaynak değişti (bir dosya silindi, biri eklendi, 10 belge eklendi), ön kopya tekrarlandı: site kaynağın aynısı (%.1f sn)" % wall)

# ---- the connection drops half-way: nothing changes ----
before_tree, before_counts = site_tree(), site_counts()
src("cat > /usr/local/bin/tar && chmod 755 /usr/local/bin/tar", stdin=SLOW_TAR.encode())
_, r = api("POST", "/v1/sites/%s/migration/copy" % SITE, {"confirm": SITE, "actor": ACTOR})
jid = r["job_id"]
wait(lambda: "Dosyalar açılıyor" in "\n".join(api("GET", "/v1/jobs/" + jid)[1]["steps"]), "the tar to start", timeout=180)
time.sleep(3)
src("pkill -f 'sshd: root@notty'", check=False)  # the session's sshd: the connection is gone
j = wait_job(jid)
src("rm -f /usr/local/bin/tar")
if not j.get("error") or ("koptu" not in j["error"] and "SSH" not in j["error"]):
    fail("cut connection: %s" % job_text(j))
if site_tree() != before_tree or site_counts() != before_counts:
    fail("a cut-off copy changed the site")
if sh("docker inspect -f '{{.State.Running}}' %s" % SITE).strip() != "true":
    fail("site not running after the cut-off copy")
clean("cut connection")
ok("bağlantı aktarımın ortasında koparıldı: iş “%s” hatasıyla bitti, site ve verisi değişmedi" % j["error"][:90])

# ---- the agent restarts in the middle of a copy: recovery, nothing left running ----
src("cat > /usr/local/bin/tar && chmod 755 /usr/local/bin/tar", stdin=SLOW_TAR.encode())
_, r = api("POST", "/v1/sites/%s/migration/copy" % SITE, {"confirm": SITE, "actor": ACTOR})
jid = r["job_id"]
wait(lambda: "Dosyalar açılıyor" in "\n".join(api("GET", "/v1/jobs/" + jid)[1]["steps"]), "the tar to start", timeout=180)
time.sleep(2)
if not sh("systemctl list-units --no-legend 'footops-ssh-%s-*' 'footops-files-%s.scope'" % (jid, jid)).strip():
    fail("no scopes running during the copy")
sh("systemctl restart footops-agent")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
wait(lambda: api("GET", "/v1/jobs/" + jid, check=False)[0] == 200, "the job record")
j = api("GET", "/v1/jobs/" + jid)[1]
if not j.get("interrupted"):
    fail("job not marked interrupted: %s" % j)
wait(lambda: not sh("systemctl list-units --no-legend 'footops-ssh-*' 'footops-files-*' 2>/dev/null").strip(), "scopes to stop", timeout=30)
wait(lambda: sh("ls -A /var/lib/footops/recovery 2>/dev/null || true").strip() in ("", "temp-databases"), "recovery", timeout=120)
src("rm -f /usr/local/bin/tar")
wait(lambda: sh("docker inspect -f '{{.State.Running}}' %s" % SITE).strip() == "true", "the site to run", timeout=60)
if site_tree() != before_tree or site_counts() != before_counts:
    fail("an interrupted copy changed the site")
clean("agent restart")
ok("agent kopyanın ortasında yeniden başladı: iş “yarıda kaldı”, ssh ve yardımcı durduruldu, kurtarma siteyi değişmemiş haliyle bıraktı")

# ---- the source's host key changes: refused until reset ----
src("rm -f /etc/ssh/ssh_host_* && ssh-keygen -A >/dev/null && kill -HUP 1")
time.sleep(1)
code, e = api("POST", "/v1/sources/%s/test" % sid, {"actor": ACTOR}, check=False)
if code != 502 or "farklı" not in e.get("error", ""):
    fail("changed host key: %d %s" % (code, e))
_, r = api("POST", "/v1/sites/%s/migration/discover" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
if not j.get("error") or "farklı" not in j["error"]:
    fail("discover with a changed key: %s" % job_text(j))
api("POST", "/v1/sources/%s/reset-hostkey" % sid, {"actor": ACTOR})
_, s = api("POST", "/v1/sources/%s/test" % sid, {"actor": ACTOR})
if s.get("host_keys") != ["ED25519 " + src("ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub").split()[1]]:
    fail("new key not pinned: %s" % s)
ok("kaynağın sunucu anahtarı değişti: bağlantı ve keşif reddedildi; “parmak izini sıfırla”dan sonra yeni anahtar kaydedildi")

# ---- remove the source: the key leaves the source too ----
api("POST", "/v1/sources/%s/remove" % sid, {"cleanup": True, "actor": ACTOR})
if "footops-" in src("cat /root/.ssh/authorized_keys"):
    fail("key left on the source")
if sh("test -e /var/lib/footops/sources/%s && echo y || true" % sid).strip():
    fail("key left on the server")
api("POST", "/v1/sites/%s/migration/forget" % SITE, {"actor": ACTOR})
audit = [e["action"] for e in api("GET", "/v1/audit?limit=200")[1]]
for want in ("source.create", "source.test", "source.reset-hostkey", "source.remove", "migrate.set", "migrate.discover",
             "migrate.scan", "migrate.copy"):
    if want not in audit:
        fail("audit lacks %s" % want)
measure["agent_rss_after"] = rss("footops agent")
sh("docker rm -f %s >/dev/null" % SRC)
ok("kaynak kaldırıldı: anahtar satırı kaynaktan, anahtar dosyası sunucudan silindi; denetim kaydında her adım var")
print("  ölçümler:", json.dumps(measure))
