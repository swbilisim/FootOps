#!/usr/bin/env python3
"""Y3 scenarios (restoring from backups) against a real install, after y2.py (whose
repository it uses; one is set up here if there is none): a site with a database (a view
among its collections) and files of the app's own user (a 0700 folder with 0600 files, a
hidden file, a link) is backed up, then changed; through the real panel its backup is
listed, opened for a look (a temporary database the browser reads), restored — database
and files — and compared to what was backed up (counts, owners, modes, checksums), the
safety copies undo it, a backup is made into an archive that an import accepts, single
files are listed and downloaded; a snapshot gone from the repository fails the restore
cleanly, an opened backup survives an agent restart, and the secrets stay off the process
list.

Usage: y3.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import html
import json
import re
import subprocess
import sys
import threading
import time

from itlib import ACTOR, api, fail, fresh_totp, ok, rss, sh, tools, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itbak", "bak.it.example.com"
DATA = "/srv/musteri/%s/data" % SITE
VERSITY = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"
KEY, SECRET = "itdeneme01", "it-deneme-gizli-0123456789"
BUCKET = "footops-it"
JAR = "/tmp/y3-jar"
measure = {}


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def web(path, form=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar (a POST
    when there is a form). Returns (status, location, body)."""
    args = "".join(" --data-urlencode %s" % quote("%s=%s" % kv) for kv in (form or {}).items())
    out = sh("curl -sS -b %s -c %s -o /tmp/y3-page -w '%%{http_code} %%{redirect_url} %%{time_total}' %s %s; echo; cat /tmp/y3-page"
             % (JAR, JAR, args, quote("http://localhost:7701" + path)))
    head, _, body = out.partition("\n")
    code, loc, took = head.split(" ", 2)
    web.took = float(took)
    return int(code), loc.replace("http://localhost:7701", ""), body


def download(path, to):
    """A download through the panel into a file in the container; its status."""
    return int(sh("curl -sS -b %s -o %s -w '%%{http_code}' %s" % (JAR, to, quote("http://localhost:7701" + path))).strip())


def csrf(page):
    m = re.search(r'name="csrf" value="([^"]+)"', page)
    if not m:
        fail("no csrf token in page")
    return m.group(1)


def job_of(loc):
    m = re.match(r"^/jobs/([A-Za-z0-9]+)$", loc)
    if not m:
        fail("not a job: %s" % loc)
    return m.group(1), wait_job(m.group(1))


def steps(j):
    return "\n".join(j.get("steps") or [])


root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)


def root_js(js):
    return sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
              stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);' % json.dumps(root_pw) + js).encode())


def counts(db=SITE):
    out = root_js('const d=c.getDB("%s");const o={};d.getCollectionInfos().filter(i=>!i.name.startsWith("system."))'
                  '.forEach(i=>o[i.name]=i.type=="view"?"view":d.getCollection(i.name).countDocuments({}));print("C "+JSON.stringify(o));' % db)
    return json.loads(out.split("C ", 1)[1])


def temp_dbs():
    out = root_js('print("D "+JSON.stringify(c.getDB("admin").adminCommand({listDatabases:1,nameOnly:true}).databases.map(d=>d.name)));')
    return [d for d in json.loads(out.split("D ", 1)[1]) if d.startswith(("footops_tmp_", "footops_view_"))]


def tree(path):
    """Type, owner, mode, size and SHA-256 of everything below path."""
    out = sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%s %%P\\n' | sort" % path)
    sums = sh("cd %s && find . -type f -print0 | sort -z | xargs -0 -r sha256sum" % path)
    return out, sums


def site_running():
    return sh("docker inspect -f '{{.State.Running}}' %s" % SITE).strip() == "true"


def site_answers():
    code = sh("curl -sk -o /dev/null -w '%%{http_code}' --resolve %s:443:127.0.0.1 https://%s/" % (DOMAIN, DOMAIN), check=False).strip()
    return code not in ("", "000") and int(code) < 500


def staging_left():
    return [d for d in sh("ls -A /var/lib/footops/staging 2>/dev/null || true").split() if not d.startswith(("exp-", "upl-"))]


def views():
    return sh("ls /var/lib/footops/backup/views 2>/dev/null || true").split()


class Watch:
    """Watches the process list for secrets and the site's container for its downtime
    while a job runs."""

    def __init__(self, secrets):
        self.secrets, self.seen, self.down, self.stop = secrets, [], 0.0, threading.Event()
        self.t = threading.Thread(target=self.run)
        self.t.start()

    def run(self):
        while not self.stop.is_set():
            t0 = time.time()
            ps = subprocess.run(["docker", "exec", C, "sh", "-c", "ps -eo args; docker inspect -f '{{.State.Running}}' " + SITE],
                                capture_output=True).stdout.decode(errors="replace")
            self.seen += [s for s in self.secrets if s and s in ps]
            if ps.strip().endswith("false"):
                self.down += time.time() - t0 + 0.1
            time.sleep(0.1)

    def end(self):
        self.stop.set()
        self.t.join()
        if self.seen:
            fail("a secret was visible in the process list")
        return self


print("Y3 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
measure["agent_rss_before_mb"] = round(rss("footops agent") / 2**20, 1)

# ---- the repository: what y2 left, or a new one ----
if sh("docker inspect -f '{{.State.Running}}' it-s3 2>/dev/null || true").strip() != "true":
    sh("docker rm -f it-s3 >/dev/null 2>&1; docker volume create it-s3 >/dev/null", check=False)
    sh("docker run --rm -v it-s3:/veri --entrypoint mkdir mongo:7.0.45 -p /veri/%s" % BUCKET)
    sh("docker run -d --name it-s3 --network host -v it-s3:/veri -e ROOT_ACCESS_KEY_ID=%s -e ROOT_SECRET_ACCESS_KEY=%s "
       "-e VGW_REGION=auto %s posix /veri >/dev/null" % (KEY, SECRET, VERSITY))
sh("iptables -C INPUT -p tcp --dport 7070 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 7070 -j ACCEPT")
host_ip = sh("hostname -I").split()[0]
wait(lambda: sh("curl -s -o /dev/null -w %%{http_code} http://%s:7070/" % host_ip, check=False).strip() not in ("", "000"), "S3 server", timeout=60)
_, st = api("GET", "/v1/backup")
if not st.get("configured") or (st["settings"].get("confirmed") or "0001").startswith("0001"):
    _, r = api("POST", "/v1/backup/setup", {"repository": "s3:http://%s:7070/%s/y3-%d" % (host_ip, BUCKET, time.time()),
                                            "key_id": KEY, "secret_key": SECRET, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("setup: %s" % j["error"])
    api("POST", "/v1/backup/confirm", {"actor": ACTOR})
    ok("yedek deposu kuruldu (y2'den kalan yoktu)")
sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))
repo_pw = sec["password"]
sh("mkdir -p /root/y3cfg && cd /root/y3cfg && python3 -c %s" % quote(
    'import json; s=json.load(open("/etc/footops/secrets/backup/repository.json")); open("repository","w").write(s["repository"]); '
    'open("password","w").write(s["password"]); open("s3","w").write("[default]\\naws_access_key_id = %s\\naws_secret_access_key = %s\\n" % (s["key_id"], s["secret_key"]))'))


def rr(args):
    """restic itself on the repository (the test's own eyes)."""
    return sh("docker run --rm --network host -v /root/y3cfg:/run/footops:ro -v /var/lib/footops/tools/restic-0.19.1:/usr/local/bin/restic:ro "
              "--tmpfs /tmp -e RESTIC_REPOSITORY_FILE=/run/footops/repository -e RESTIC_PASSWORD_FILE=/run/footops/password "
              "-e AWS_SHARED_CREDENTIALS_FILE=/run/footops/s3 --entrypoint restic mongo:7.0.45 --no-cache %s" % args)


# No night run owed while the scenarios run.
h, mi = map(int, sh("date +%H:%M").strip().split(":"))
sh("/usr/local/bin/footops backup set night=%02d:%02d" % ((h + 3) % 24, mi))

# ---- the site: a database with a view, files of the app's user ----
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                              "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                              "data_mount": "/usr/share/nginx/html", "database": True}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s" % j["error"])
sh("docker start %s >/dev/null; docker unpause %s >/dev/null 2>&1 || true" % (SITE, SITE), check=False)
root_js("""const d=c.getDB("%s");
d.getCollectionInfos().filter(i=>!i.name.startsWith("system.")).forEach(i=>d.getCollection(i.name).drop());
const u=[];for(let i=0;i<300;i++)u.push({ad:"ürün çğıöşü "+i,fiyat:i%%25});d.urunler.insertMany(u);
const s=[];for(let i=0;i<50;i++)s.push({no:i,durum:"yeni"});d.siparisler.insertMany(s);
d.ayarlar.insertOne({ad:"site",tema:"koyu"});
d.createView("ucuz","urunler",[{$match:{fiyat:{$lt:5}}}]);print("ok");""" % SITE)
ORIG = {"urunler": 300, "siparisler": 50, "ayarlar": 1, "ucuz": "view"}
if counts() != ORIG:
    fail("seed: %s" % counts())
sh("""find %(d)s -mindepth 1 -delete
mkdir -p %(d)s/img %(d)s/belge %(d)s/ozel
i=0; while [ $i -lt 8 ]; do head -c $(( (i + 1) * 30000 )) /dev/urandom > %(d)s/img/foto$i.jpg; i=$((i + 1)); done
printf 'merhaba dünya\\n' > '%(d)s/belge/çay ve şeker.txt'
printf 'RewriteEngine On\\n' > %(d)s/.htaccess
head -c 50000 /dev/urandom > %(d)s/ozel/kasa.sqlite3; echo gizli > %(d)s/ozel/anahtar.pem
printf '<h1>itbak</h1>' > %(d)s/index.html
ln -s img/foto0.jpg %(d)s/kisayol
chown -hR 101:101 %(d)s; chmod 700 %(d)s/ozel; chmod 600 %(d)s/ozel/*""" % {"d": DATA})
orig_tree = tree(DATA)
nfiles = orig_tree[1].count("\n")
ok("site hazır: 3 koleksiyon + 1 görünüm, %d dosya (gizli dosya, bağlantı, 0700 klasörde 0600 dosyalar; sahibi 101)" % nfiles)

# ---- a backup of the site, and the list it leaves ----
_, r = api("POST", "/v1/backup/run", {"site": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("backup: %s\n%s" % (j["error"], steps(j)))
R1 = j["id"]
if "Yedek listesi yenilendi" not in steps(j):
    fail("the run did not refresh the list:\n" + steps(j))
_, lst = api("GET", "/v1/sites/%s/backups" % SITE)
pt = lst["points"][0]
if pt["run"] != R1 or set(pt["parts"]) != {"db", "files", "def"} or pt["parts"]["files"]["files"] != nfiles:
    fail("list: %s" % pt)
ok("yedek alındı (%s) ve liste kendiliğinden yenilendi: veritabanı, dosyalar (%d), site tanımı" % (R1, nfiles))

# ---- the site changes ----
root_js("""const d=c.getDB("%s");d.siparisler.drop();d.urunler.deleteMany({fiyat:{$gte:20}});
const y=[];for(let i=0;i<7;i++)y.push({i});d.yeni.insertMany(y);print("ok");""" % SITE)
CHANGED = counts()
sh("rm %(d)s/img/foto0.jpg && head -c 1000 /dev/urandom > %(d)s/img/yeni.jpg && printf 'değişti' > '%(d)s/belge/çay ve şeker.txt' && "
   "chown -R 101:101 %(d)s" % {"d": DATA})
changed_tree = tree(DATA)
ok("site değişti: veritabanı %s, dosyalar (biri silindi, biri eklendi, biri değişti)" % CHANGED)

# ---- the panel: the list, a backup's page ----
user, pw, totp_secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(totp_secret)})
if code != 303:
    fail("login: %d %s" % (code, loc))
code, _, page = web("/sites/%s/backup" % SITE)
tok = csrf(page)
if code != 200 or 'href="/sites/%s/backups/%s"' % (SITE, R1) not in page or "Yedekler" not in page:
    fail("site backups page: %d" % code)
measure["page_list_ms"] = round(web.took * 1000)
code, _, page = web("/sites/%s/backups/%s" % (SITE, R1))
if code != 200 or "Aç ve incele" not in page or "%d dosya" % nfiles not in page or "351 belge · döküm" not in page:
    fail("backup page: %d\n%s" % (code, page[page.find("<main>"):][:4000]))
ok("panel: site yedeklerinde liste, yedek sayfasında parçalar (kayıttan 351 belge; %d dosya)" % nfiles)

# ---- open it for a look ----
w = Watch([repo_pw, SECRET])
t0 = time.time()
code, loc, _ = web("/sites/%s/backups/%s/open" % (SITE, R1), {"csrf": tok})
vid, j = job_of(loc)
w.end()
measure["open_s"] = round(time.time() - t0, 1)
measure["open_peak_mem_mb"] = round((j["usage"].get("peak_mem") or 0) / 2**20)
if j.get("error") or j.get("result") != R1 or w.down:
    fail("open: %s %s down %.1f\n%s" % (j.get("error"), j.get("result"), w.down, steps(j)))
if "Sayımlar yedek alınırken sayılanla aynı" not in steps(j):
    fail("the view was not compared with the run's counts:\n" + steps(j))
tmp = "footops_view_" + vid.lower()
if temp_dbs() != [tmp] or counts(tmp) != ORIG:
    fail("temporary database: %s %s" % (temp_dbs(), counts(tmp) if tmp in temp_dbs() else "-"))
if counts() != CHANGED:
    fail("the site's database was touched by opening")
_, _, page = web("/jobs/" + vid)
if 'href="/sites/%s/backups/%s/db"' % (SITE, R1) not in page:
    fail("the job page does not lead to the collections")
code, _, page = web("/sites/%s/backups/%s/db" % (SITE, R1))
if code != 200 or "Yedekteki veritabanı" not in page or "siparisler" not in page or "Son değişiklikler" in page:
    fail("view collections: %d" % code)
code, _, page = web("/sites/%s/backups/%s/db/coll?c=siparisler" % (SITE, R1))
m = re.search(r'href="(/sites/%s/backups/%s/db/doc\?[^"]+)"' % (SITE, R1), page)
if code != 200 or "50 belge" not in page or not m:
    fail("view collection: %d" % code)
code, _, page = web(html.unescape(m.group(1)))
if code != 200 or "Düzenle" in page or "salt okunur" not in page:
    fail("view document: %d" % code)
code = int(sh("curl -s -o /dev/null -w '%%{http_code}' -X POST --data-binary '{}' --unix-socket /run/footops/agent.sock "
              "'http://agent/v1/sites/%s/views/%s/db/replace'" % (SITE, vid)).strip())
if code not in (404, 405):
    fail("a write to a view: %d" % code)
ok("açıldı (%.1f sn, en çok %d MB): geçici veritabanında 300/50/1 + görünüm, kayıttaki sayımlarla aynı; tarayıcıda salt okunur; "
   "site etkilenmedi, şifreler süreç listesinde yok" % (measure["open_s"], measure["open_peak_mem_mb"]))

# ---- the preview, then the restore ----
code, _, page = web("/sites/%s/backups/%s/restore" % (SITE, R1))
for want in ["yeni: yedekten gelir", "yalnız sitede: silinir", "farklı: yedekteki hali gelir", "aynı sayıda", 'name="db" checked']:
    if want not in page:
        fail("preview lacks %r" % want)
code, _, page = web("/sites/%s/backups/%s/restore" % (SITE, R1), {"csrf": tok, "db": "on", "files": "on", "confirm": "yanlis"})
if code != 422 or counts() != CHANGED:
    fail("a wrong confirmation: %d" % code)
w = Watch([repo_pw, SECRET])
t0 = time.time()
code, loc, _ = web("/sites/%s/backups/%s/restore" % (SITE, R1), {"csrf": tok, "db": "on", "files": "on", "confirm": SITE})
rid, j = job_of(loc)
w.end()
measure["restore_s"] = round(time.time() - t0, 1)
measure["restore_down_s"] = round(w.down, 1)
measure["restore_peak_mem_mb"] = round((j["usage"].get("peak_mem") or 0) / 2**20)
if j.get("error"):
    fail("restore: %s\n%s" % (j["error"], steps(j)))
if counts() != ORIG:
    fail("restored database: %s" % counts())
if tree(DATA) != orig_tree:
    fail("restored files differ:\n%s\nwant\n%s" % (tree(DATA)[0], orig_tree[0]))
k1, old = j["result"].split()
old = old.replace("files-", "")
if tree("/srv/musteri/%s/data.old-%s" % (SITE, old)) != changed_tree:
    fail("the previous directory is not what was there")
_, snaps = api("GET", "/v1/sites/%s/snapshots" % SITE)
k1s = [s for s in snaps if s["id"] == k1]
# mongodump counts a view as an empty collection; the copy's record keeps what it said.
if not k1s or k1s[0]["reason"] != "before-restore" or \
        {k: v for k, v in k1s[0]["collections"].items() if CHANGED.get(k) != "view"} != {k: v for k, v in CHANGED.items() if v != "view"}:
    fail("safety copy: %s" % k1s)
if temp_dbs() or views() or staging_left() or tools():
    fail("left after the restore: %s %s %s %s" % (temp_dbs(), views(), staging_left(), tools()))
if not site_running() or not site_answers():
    fail("the site is not back")
_, _, page = web("/jobs/" + rid)
if 'href="/sites/%s/snapshots/%s/restore"' % (SITE, k1) not in page:
    fail("no undo link on the job page")
ok("geri yüklendi (%.1f sn, site ~%.1f sn kapalı, en çok %d MB): sayımlar ve dosyalar yedekle birebir (sahip 101, 0700/0600, gizli dosya, "
   "bağlantı, sha256); önceki hal data.old-%s, veritabanı kopyası %s (geri yüklemeden önce); geçici veritabanı, kayıt, staging temiz" %
   (measure["restore_s"], measure["restore_down_s"], measure["restore_peak_mem_mb"], old, k1))

# ---- undo: the safety copies put the site back as it was ----
_, r = api("POST", "/v1/sites/%s/snapshots/%s/restore" % (SITE, k1), {"confirm": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error") or counts() != CHANGED:
    fail("undo database: %s %s" % (j.get("error"), counts()))
_, r = api("POST", "/v1/sites/%s/files/backups/%s/restore" % (SITE, old), {"confirm": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error") or tree(DATA) != changed_tree:
    fail("undo files: %s" % j.get("error"))
ok("geri al: veritabanı kopyası ve dosya kopyası siteyi geri yüklemeden önceki haline getirdi")

# ---- files alone, no view needed ----
code, loc, _ = web("/sites/%s/backups/%s/restore" % (SITE, R1), {"csrf": tok, "files": "on", "confirm": SITE})
_, j = job_of(loc)
if j.get("error") or tree(DATA) != orig_tree or counts() != CHANGED:
    fail("files alone: %s\n%s" % (j.get("error"), steps(j)))
code, _, page = web("/sites/%s/backups/%s/restore" % (SITE, R1), {"csrf": tok, "db": "on", "confirm": SITE})
if code != 422 or "önce yedeği aç" not in page:
    fail("the database without an opened view: %d" % code)
ok("yalnız dosyalar geri yüklendi (veritabanına dokunulmadı); açılmamış yedekten veritabanı istenince önce açılması isteniyor")

# ---- an archive of the backup, imported again ----
w = Watch([repo_pw, SECRET])
t0 = time.time()
code, loc, _ = web("/sites/%s/backups/%s/export" % (SITE, R1), {"csrf": tok, "db": "on", "files": "on", "env": "on"})
xid, j = job_of(loc)
w.end()
measure["export_s"] = round(time.time() - t0, 1)
if j.get("error"):
    fail("export: %s\n%s" % (j["error"], steps(j)))
if "1 öğe alınmadı" not in steps(j):
    fail("the link was not left out:\n" + steps(j))
if download("/sites/%s/exports/%s/download" % (SITE, j["result"]), "/tmp/y3.footops.tar") != 200:
    fail("download of the archive")
members = sh("tar tf /tmp/y3.footops.tar").split()
if members != ["db.archive.gz", "files.tar.gz", "env", "manifest.json"]:
    fail("members: %s" % members)
man = json.loads(sh("tar xOf /tmp/y3.footops.tar manifest.json"))
if man["backup"]["run"] != R1 or man["collections"] != {"urunler": 300, "siparisler": 50, "ayarlar": 1} or man["files"]["files"] != nfiles:
    fail("manifest: %s" % man)
env = sh("tar xOf /tmp/y3.footops.tar env")
# The site's own database connection (FootOps makes it on every server) stays out.
if "MONGODB_URI=" in env or "MONGODB_URI alınmadı" not in env or "SIRLAR İÇERİR" not in env:
    fail("env: %s" % env[:300])
files_list = sh("tar xOf /tmp/y3.footops.tar files.tar.gz | tar tzvf -")
if ".htaccess" not in files_list or "kisayol" in files_list or "ozel/anahtar.pem" not in files_list:
    fail("files member:\n" + files_list)
measure["export_mb"] = round(int(sh("stat -c %s /tmp/y3.footops.tar")) / 2**20, 2)
out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @/tmp/y3.footops.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=y3.footops.tar'" % SITE)
up = json.loads(out)
_, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": up["id"], "actor": ACTOR})
j = wait_job(r["job_id"])
_, ic = api("GET", "/v1/sites/%s/imports/%s" % (SITE, up["id"]))
if j.get("error") or ic["archive"] != {"urunler": 300, "siparisler": 50, "ayarlar": 1} or not any("ortam değişkenleri" in n for n in ic.get("notes", [])):
    fail("import check of the archive: %s %s" % (j.get("error"), ic.get("notes")))
api("DELETE", "/v1/uploads/" + up["id"])
ok("İndir (%.1f sn, %.2f MB): db.archive.gz + files.tar.gz (gizli dosya var, bağlantı yok) + env (sırlar) + manifest (yedek %s); "
   "İçe aktar arşivi kabul etti, sayımlar yedekteki gibi, env yalnız not" % (measure["export_s"], measure["export_mb"], R1))

# ---- single files ----
code, _, page = web("/sites/%s/backups/%s/files" % (SITE, R1))
if code != 200 or "ozel" not in page or ".htaccess" not in page or "bağlantı" not in page:
    fail("files page: %d" % code)
measure["page_files_ms"] = round(web.took * 1000)
code, _, page = web("/sites/%s/backups/%s/files?path=belge" % (SITE, R1))
if code != 200 or "çay ve şeker.txt" not in page:
    fail("subfolder: %d" % code)
if download("/sites/%s/backups/%s/files/download?path=belge/%%C3%%A7ay%%20ve%%20%%C5%%9Feker.txt" % (SITE, R1), "/tmp/y3-tek") != 200 or \
        sh("cat /tmp/y3-tek") != "merhaba dünya\n":
    fail("single file download")
if download("/sites/%s/backups/%s/files/download?path=ozel/anahtar.pem" % (SITE, R1), "/tmp/y3-tek") != 200 or sh("cat /tmp/y3-tek") != "gizli\n":
    fail("a 0600 file")
ok("yedekteki dosyalar: klasör klasör (%d ms), tek dosya indirme (Türkçe ad, 0600 dosya)" % measure["page_files_ms"])

# ---- a snapshot gone from the repository: the restore fails before anything changes ----
before = tree(DATA)
fid = [s for s in json.loads(rr("snapshots --json")) if "run=" + R1 in s["tags"] and "part=files" in s["tags"]][0]["id"]
# Forgotten and pruned, as retention does it: restic's cache may still know the snapshot,
# but its data is gone.
rr("forget --prune %s" % fid)
code, loc, _ = web("/sites/%s/backups/%s/restore" % (SITE, R1), {"csrf": tok, "files": "on", "confirm": SITE})
_, j = job_of(loc)
if "depoda artık yok" not in (j.get("error") or "") or tree(DATA) != before or not site_running() or \
        sh("ls -d /srv/musteri/%s/data.new 2>/dev/null || true" % SITE).strip():
    fail("a missing snapshot: %s" % j.get("error"))
measure["missing_snapshot_error"] = j["error"][:160]
code, loc, _ = web("/sites/%s/backups/list" % SITE, {"csrf": tok})
_, j = job_of(loc)
_, d = api("GET", "/v1/sites/%s/backups/%s" % (SITE, R1))
if j.get("error") or "files" in d["parts"]:
    fail("refreshed list: %s %s" % (j.get("error"), list(d["parts"])))
ok("depoda silinmiş anlık görüntü: geri yükleme hiçbir şeyi değiştirmeden düştü; liste yenilenince o parça listeden çıktı")

# ---- an opened backup survives an agent restart, then closes ----
code, loc, _ = web("/sites/%s/backups/%s/open" % (SITE, R1), {"csrf": tok})
vid, j = job_of(loc)
if j.get("error"):
    fail("open again: %s" % j["error"])
sh("systemctl restart footops-agent")
wait(lambda: api("GET", "/v1/overview", check=False)[0] == 200, "agent after restart", timeout=60)
code, ov = api("GET", "/v1/sites/%s/views/%s/db" % (SITE, vid), check=False)
if code != 200 or not any(c["name"] == "siparisler" for c in ov["collections"]):
    fail("view after a restart: %d" % code)
code, _, _ = web("/sites/%s/backups/%s/close" % (SITE, R1), {"csrf": tok})
if code != 303 or temp_dbs() or views() or staging_left():
    fail("close: %d %s %s %s" % (code, temp_dbs(), views(), staging_left()))
ok("açık yedek agent yeniden başlayınca da açık; Kapat geçici veritabanını ve dökümü sildi")

# ---- the audit log ----
audit = sh("tail -n 60 /var/lib/footops/audit.jsonl")
for action in ["backup.open", "backup.restore", "backup.export", "backup.download", "backup.close", "backup.list"]:
    if '"%s"' % action not in audit:
        fail("%s not in the audit log" % action)
if repo_pw in audit or SECRET in audit:
    fail("a secret in the audit log")
ok("denetimde: aç, geri yükle, arşiv, tek dosya, kapat, liste — sırsız")

measure["web_rss_mb"] = round(rss("footops web") / 2**20, 1)
measure["agent_rss_after_mb"] = round(rss("footops agent") / 2**20, 1)
sh("/usr/local/bin/footops backup set night=04:00")
print(json.dumps(measure, indent=2, ensure_ascii=False))
print("Y3: hepsi geçti")
