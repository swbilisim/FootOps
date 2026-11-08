#!/usr/bin/env python3
"""Y1 scenarios (the backup core) against a real install, after f0.py: a local S3 server
(versitygw) in the test container, the repository connected with `footops backup setup`
(a key the bucket refuses, a new repository, an existing one), a full backup (database,
files, definition, server) read back with restic itself and compared, deduplication on
the next run, the night run and an hourly run from the real timetable, a frozen site with
the agent restarted while it is paused, a cancelled run, and the secrets never on a
command line.

Usage: y1.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import hashlib
import json
import re
import subprocess
import sys
import threading
import time

from itlib import ACTOR, api, fail, ok, rss, sh, tools, wait, wait_idle, wait_job

C = sys.argv[1]
SITE, DOMAIN = "itbak", "bak.it.example.com"  # covered by f0's *.it.example.com certificate
DATA = "/srv/musteri/%s/data" % SITE
VERSITY = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"
KEY, SECRET = "itdeneme01", "it-deneme-gizli-0123456789"
BUCKET = "footops-it"
measure = {}


def cli(args, stdin=None, check=True):
    """footops backup as root over SSH would run it; returns (exit status, output)."""
    p = subprocess.run(["docker", "exec", "-i", C, "/usr/local/bin/footops", "backup"] + args,
                       input=stdin.encode() if stdin is not None else b"", capture_output=True)
    out = p.stdout.decode(errors="replace") + p.stderr.decode(errors="replace")
    if check and p.returncode != 0:
        fail("footops backup %s: %d\n%s" % (" ".join(args), p.returncode, out))
    return p.returncode, out


def rr(args, extra=""):
    """restic itself on the repository (the test's own eyes): the agent's binary in the
    MongoDB image, the host's network, a copy of the repository settings."""
    return sh("docker run --rm --network host %s -v /root/y1cfg:/run/footops:ro "
              "-v /var/lib/footops/tools/restic-0.19.1:/usr/local/bin/restic:ro --tmpfs /tmp "
              "-e RESTIC_REPOSITORY_FILE=/run/footops/repository -e RESTIC_PASSWORD_FILE=/run/footops/password "
              "-e AWS_SHARED_CREDENTIALS_FILE=/run/footops/s3 --entrypoint restic mongo:7.0.45 --no-cache %s" % (extra, args))


def snapshots():
    return json.loads(rr("snapshots --json"))


def site_js(js):
    uri = re.search(r'MONGODB_URI: "([^"]+)"', sh("cat /srv/musteri/%s/compose.yaml" % SITE)).group(1)
    local = uri.replace("@mongo:27017", "@127.0.0.1:27017")
    return sh("docker exec -i footops-mongo mongosh --quiet '%s' --file /dev/stdin" % local, stdin=js.encode())


def counts_js(db):
    return ('const d=c.getDB("%s");const o={};d.getCollectionNames().filter(n=>!n.startsWith("system."))'
            '.forEach(n=>o[n]=d.getCollection(n).countDocuments({}));print("C "+JSON.stringify(o));' % db)


root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)


def root_js(js):
    return sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
              stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);' % json.dumps(root_pw) + js).encode())


def tree(path):
    """Type, owner, mode, size and SHA-256 of everything below path."""
    out = sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%s %%P\\n' | sort" % path)
    sums = sh("cd %s && find . -type f -print0 | sort -z | xargs -0 -r sha256sum" % path)
    return out, sums


def runs(n=20):
    return api("GET", "/v1/backup/runs?limit=%d" % n)[1]


def latest_job():
    for e in api("GET", "/v1/audit?limit=50")[1]:
        if e["action"] == "backup.run":
            return e
    return None


print("Y1 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
measure["agent_rss_before"] = rss("footops agent")

# ---- a clean start: the S3 server, no repository, no backup state ----
sh("docker rm -f it-s3 >/dev/null 2>&1; docker volume rm it-s3 >/dev/null 2>&1; docker volume create it-s3 >/dev/null", check=False)
sh("docker run --rm -v it-s3:/veri --entrypoint mkdir mongo:7.0.45 -p /veri/%s" % BUCKET)
sh("docker run -d --name it-s3 --network host -v it-s3:/veri -e ROOT_ACCESS_KEY_ID=%s -e ROOT_SECRET_ACCESS_KEY=%s "
   "-e VGW_REGION=auto %s posix /veri >/dev/null" % (KEY, SECRET, VERSITY))
# Tool containers reach the S3 server on the host's address; ufw would drop it.
sh("iptables -C INPUT -p tcp --dport 7070 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 7070 -j ACCEPT")
host_ip = sh("hostname -I").split()[0]
REPO = "s3:http://%s:7070/%s" % (host_ip, BUCKET)
wait(lambda: sh("curl -s -o /dev/null -w %%{http_code} http://%s:7070/" % host_ip, check=False).strip() not in ("", "000"),
     "S3 server", timeout=60)
sh("rm -rf /etc/footops/secrets/backup/repository.json /var/lib/footops/backup /var/cache/footops/restic /root/y1cfg /root/y1restore")
sh("mkdir -p /var/lib/footops/backup && chmod 700 /var/lib/footops/backup")
ok("S3 sunucusu (versitygw) hazır: %s" % REPO.replace(host_ip, "<ip>"))

# ---- the site: a database and a data directory owned by the app's user ----
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                              "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                              "data_mount": "/usr/share/nginx/html", "database": True}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    ok("yedek deneme sitesi oluşturuldu (veritabanı + nginx uid 101)")
sh("docker start %s >/dev/null; docker unpause %s >/dev/null 2>&1 || true" % (SITE, SITE), check=False)
seed = """
const d = db.getSiblingDB("%s");
d.getCollectionNames().filter(n => !n.startsWith("system.")).forEach(n => d.getCollection(n).drop());
const rnd = () => Array.from({length: 6}, () => Math.random().toString(36).slice(2)).join("");
for (let b = 0; b < 30; b++) {
  const docs = [];
  for (let i = 0; i < 2000; i++) docs.push({b, i, ad: "ürün çğıöşü " + i, aciklama: rnd(), t: new Date()});
  d.urunler.insertMany(docs, {ordered: false});
}
for (let i = 0; i < 500; i++) d.siparisler.insertOne({no: i, kalemler: [{u: i %% 7, adet: 2}], durum: "yeni"});
d.ayarlar.insertOne({ad: "site", tema: "koyu"});
print("SEEDED " + (d.urunler.countDocuments() + d.siparisler.countDocuments() + d.ayarlar.countDocuments()));
""" % SITE
if "SEEDED 60501" not in site_js(seed):
    fail("seed")
sh("""find %(d)s -mindepth 1 -delete
mkdir -p %(d)s/img %(d)s/belge %(d)s/ozel
i=0; while [ $i -lt 60 ]; do head -c $(( (i %% 7 + 1) * 40000 )) /dev/urandom > %(d)s/img/foto$i.jpg; i=$((i + 1)); done
printf 'merhaba dünya\\n' > '%(d)s/belge/çay ve şeker.txt'
head -c 200000 /dev/urandom > %(d)s/ozel/kasa.sqlite3; echo gizli > %(d)s/ozel/anahtar.pem
chown -R 101:101 %(d)s; chmod 700 %(d)s/ozel; chmod 600 %(d)s/ozel/*""" % {"d": DATA})
orig_tree = tree(DATA)
ok("veri hazır: 3 koleksiyon / 60.501 belge, %s dosya (bir klasör 0700 + 0600)" % orig_tree[1].count("\n"))

# No night run is owed while the scenarios run (a fresh repository would otherwise get its
# first night run at once): the night is three hours away.
h, mi = map(int, sh("date +%H:%M").strip().split(":"))
cli(["set", "night=%02d:%02d" % ((h + 3) % 24, mi)])

# ---- setup: a refused key fails at once and saves nothing ----
t0 = time.time()
code, out = cli(["setup"], "%s\n%s\nyanlis-gizli-anahtar-0000\n" % (REPO, KEY), check=False)
if code == 0 or "erişimi reddetti" not in out or time.time() - t0 > 30:
    fail("refused key: %d %.1fs\n%s" % (code, time.time() - t0, out))
if sh("test -e /etc/footops/secrets/backup/repository.json && echo y || true").strip():
    fail("a failed setup saved settings")
measure["refused_key_s"] = round(time.time() - t0, 1)
ok("yanlış anahtar %.1f sn'de reddedildi, ayar kaydedilmedi" % measure["refused_key_s"])

# ---- setup: a new repository, its password shown once and confirmed ----
code, out = cli(["setup"], "%s\n%s\n%s\nhayır\nkaydettim\n" % (REPO, KEY, SECRET))
m = re.search(r"│  ([A-Za-z0-9]{40})  │", out)
if not m or "Yeni depo oluşturuldu" not in out or "Onaylandı" not in out:
    fail("setup:\n" + out)
pw = m.group(1)
sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))
if sec["password"] != pw or sh("stat -c '%a %U' /etc/footops/secrets/backup/repository.json").strip() != "600 root" or \
        sh("stat -c '%a %U' /etc/footops/secrets/backup").strip() != "700 root":
    fail("secret file")
if pw in sh("cat /var/lib/footops/jobs/*.json") or pw in sh("cat /var/lib/footops/audit.jsonl"):
    fail("password in a job record or the audit log")
ok("yeni depo: şifre bir kez gösterildi (40 karakter), 0600 root dosyada; iş kaydında / denetimde yok")
_, k = api("GET", "/v1/backup/key")
panel = sh("sudo -u footops curl -s -o /dev/null -w %{http_code} --unix-socket /run/footops/agent.sock http://agent/v1/backup/key")
if k["password"] != pw or panel.strip() != "403":
    fail("key endpoint: root %s, panel %s" % (bool(k), panel))
# The panel's user cannot point the backups somewhere else without the current password.
evil = json.dumps({"repository": "s3:https://evil.example.com/bucket", "key_id": "EVILKEY123", "secret_key": "evil-secret-123",
                   "password": "their-own-password", "actor": ACTOR})
code = sh("sudo -u footops curl -s -o /dev/null -w %{http_code} -X POST -H 'Content-Type: application/json' --data-binary @- "
          "--unix-socket /run/footops/agent.sock http://agent/v1/backup/setup", stdin=evil.encode()).strip()
if code != "403" or json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))["repository"] != REPO:
    fail("the panel replaced the repository: %s" % code)
ok("şifre: root'a gösteriliyor, onaydan sonra panele değil (403); panel depoyu şifresiz değiştiremiyor (403)")
_, show = cli(["show-key"])
if show.strip() != pw:
    fail("show-key")
# The test's own restic gets a copy of the settings.
sh("mkdir -p /root/y1cfg && cd /root/y1cfg && python3 -c '%s'" %
   "import json; s=json.load(open(\"/etc/footops/secrets/backup/repository.json\")); open(\"repository\",\"w\").write(s[\"repository\"]); "
   "open(\"password\",\"w\").write(s[\"password\"]); open(\"s3\",\"w\").write(\"[default]\\naws_access_key_id = %s\\naws_secret_access_key = %s\\n\" % (s[\"key_id\"], s[\"secret_key\"]))")
code, out = cli(["setup"], "%s\n%s\n%s\n" % (REPO, KEY, SECRET), check=False)
if code == 0 or "başka bir şifreyle" not in out:
    fail("setup over an existing repository with a new password:\n" + out)
code, out = cli(["setup", "--existing"], "%s\n%s\n%s\n%s\n" % (REPO, KEY, SECRET, pw))
if "Var olan depoya bağlandı" not in out:
    fail("setup --existing:\n" + out)
ok("var olan depo: yeni şifreyle reddedildi, kendi şifresiyle bağlandı")

# ---- a full backup by hand, with the secrets watched for in the process list ----
seen = []
stop = threading.Event()


def watch_ps():
    while not stop.is_set():
        out = subprocess.run(["docker", "exec", C, "ps", "-eo", "args"], capture_output=True).stdout.decode(errors="replace")
        for s in (pw, SECRET):
            if s in out:
                seen.append(s)
        time.sleep(0.2)


watcher = threading.Thread(target=watch_ps)
watcher.start()
t0 = time.time()
code, out = cli(["run"])
stop.set()
watcher.join()
measure["full_wall_s"] = round(time.time() - t0, 1)
if seen:
    fail("a secret was visible in the process list")
print("    " + "\n    ".join(out.strip().splitlines()))
r = runs(1)[0]
parts = {(p["site"], p["part"]): p for p in r["parts"]}
for want in [(SITE, "db"), (SITE, "files"), (SITE, "def"), ("itsite", "db"), ("_server", "server")]:
    if want not in parts or not parts[want].get("snapshot") or parts[want].get("error"):
        fail("part %s: %s" % (want, parts.get(want)))
db = parts[(SITE, "db")]
if db["documents"] != 60501 or db["collections"] != {"urunler": 60000, "siparisler": 500, "ayarlar": 1}:
    fail("db counts: %s" % db)
if parts[(SITE, "files")]["files"] != orig_tree[1].count("\n"):
    fail("file count: %s" % parts[(SITE, "files")])
measure["full"] = {"%s/%s" % k: {"added_packed": p.get("added_packed", 0), "bytes": p.get("bytes", 0),
                                 "took_ms": p.get("took", 0) // 1_000_000, "peak_mem": p["usage"].get("peak_mem")}
                   for k, p in parts.items()}
measure["full_peak_mem"] = r["usage"].get("peak_mem")
ok("tam yedek: %d parça, %.1f sn, en çok %.0f MB bellek; şifre ve gizli anahtar süreç listesinde hiç görünmedi" %
   (len(r["parts"]), measure["full_wall_s"], (r["usage"].get("peak_mem") or 0) / 2**20))
# Exports and uploads (exp-*, upl-*) stay in staging for a day by design.
left = [d for d in sh("ls -A /var/lib/footops/staging").split() if not d.startswith(("exp-", "upl-"))]
if tools() or left:
    fail("tool container or staging left: %s %s" % (tools(), left))
ok("araç container'ı kalmadı, staging boş (kimlik bilgileri silindi)")

# ---- read back with restic itself ----
snaps = snapshots()
paths = {tuple(s["paths"]) for s in snaps}
for p in ["/footops/%s/db.archive" % SITE, "/footops/%s/files" % SITE, "/footops/%s/def" % SITE, "/footops/_server"]:
    if (p,) not in paths:
        fail("no snapshot of %s: %s" % (p, paths))
bad = [s for s in snaps if s["hostname"] != "IT" or "footops" not in s["tags"] or not any(t.startswith("run=") for t in s["tags"])]
if bad:
    fail("host / tags: %s" % bad[0])
# The database: streamed out of the repository into a temporary database and counted.
sh("docker run --rm --network host -v /root/y1cfg:/run/footops:ro -v /var/lib/footops/tools/restic-0.19.1:/usr/local/bin/restic:ro "
   "--tmpfs /tmp -e RESTIC_REPOSITORY_FILE=/run/footops/repository -e RESTIC_PASSWORD_FILE=/run/footops/password "
   "-e AWS_SHARED_CREDENTIALS_FILE=/run/footops/s3 --entrypoint restic mongo:7.0.45 --no-cache dump latest "
   "/footops/%s/db.archive --path /footops/%s/db.archive | docker exec -i footops-mongo mongorestore --quiet --archive "
   "-u root -p '%s' --authenticationDatabase admin --nsFrom '%s.*' --nsTo 'y1chk.*'" % (SITE, SITE, root_pw, SITE))
got = root_js(counts_js("y1chk") + 'print("TR "+c.getDB("y1chk").urunler.findOne({i: 3}).ad);c.getDB("y1chk").dropDatabase();')
if json.loads(got.split("C ", 1)[1].split("\n")[0]) != {"urunler": 60000, "siparisler": 500, "ayarlar": 1}:
    fail("restored counts: " + got)
if "TR ürün çğıöşü 3" not in got:
    fail("Turkish text: " + got)
ok("veritabanı yedeği geri okundu: 60.501 belge birebir, Türkçe karakterler sağlam")
# The files: restored with ownership (the restore container may chown) and compared.
sh("mkdir -p /root/y1restore")
rr("restore latest --path /footops/%s/files --target /r" % SITE,
   extra="-v /root/y1restore:/r --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE")
if tree("/root/y1restore/footops/%s/files" % SITE) != orig_tree:
    fail("restored files differ")
ok("dosyalar geri yüklendi: içerik (sha256), sahiplik (101:101) ve izinler (0700 / 0600) birebir")
if rr("dump latest /footops/%s/def/compose.yaml --path /footops/%s/def" % (SITE, SITE)) != sh("cat /srv/musteri/%s/compose.yaml" % SITE):
    fail("compose.yaml differs")
listing = rr("ls latest --path /footops/_server")
for want in ["/footops/_server/etc/secrets/mongo.env", "/footops/_server/etc/footops.json", "/footops/_server/web/auth.json",
             "/footops/_server/caddy/etc/Caddyfile", "/footops/_server/backup/settings.json"]:
    if want not in listing:
        fail("server snapshot lacks %s" % want)
if not re.search(r"/footops/_server/caddy/etc/certs/\S+\.key", listing):
    fail("server snapshot lacks the certificates")
ok("site tanımı (compose.yaml) birebir; sunucu ayarları: /etc/footops (sırlar), panel girişi, Caddy + sertifikalar")

# ---- the next run adds next to nothing ----
t0 = time.time()
cli(["run", SITE])
measure["second_wall_s"] = round(time.time() - t0, 1)
p2 = {p["part"]: p for p in runs(1)[0]["parts"]}
if p2["files"].get("added_packed", 0) > 64 << 10 or p2["db"].get("added_packed", 0) > 2 << 20:
    fail("not deduplicated: %s" % p2)
measure["second"] = {k: {"added_packed": v.get("added_packed", 0), "took_ms": v.get("took", 0) // 1_000_000} for k, v in p2.items()}
ok("ikinci yedek (%.1f sn): dosyalar +%d B, veritabanı +%.1f KB — değişmeyen yeniden yazılmadı" %
   (measure["second_wall_s"], p2["files"].get("added_packed", 0), p2["db"].get("added_packed", 0) / 1024))

# ---- the night run from the real timetable ----
now = sh("date +%H:%M").strip()
h, mi = map(int, now.split(":"))
night = "%02d:%02d" % ((h + (mi + 1) // 60) % 24, (mi + 1) % 60)
cli(["set", "night=" + night])
before = {x["id"] for x in runs(50)}
t0 = time.time()
wait(lambda: any(x["kind"] == "daily" and x["id"] not in before and x.get("finished", "").startswith("2")
                 for x in runs(50)), "the night run", timeout=300)
nr = next(x for x in runs(50) if x["kind"] == "daily" and x["id"] not in before)
if nr.get("error") or not nr.get("pruned") or not nr.get("checked") or not nr.get("repo_size"):
    fail("night run: %s" % json.dumps(nr)[:2000])
_, st = api("GET", "/v1/backup")
if st["night_done"] != sh("date +%F").strip():
    fail("night not recorded: %s" % st.get("night_done"))
measure.update(night_run_s=round(sum(p.get("took", 0) for p in nr["parts"]) / 1e9, 1), repo_size=nr["repo_size"],
               night_peak_mem=nr["usage"].get("peak_mem"))
ok("gece yedeği zamanlayıcıdan (%s): %d parça + saklama + prune + check; depo %.1f MB" %
   (night, len(nr["parts"]), nr["repo_size"] / 2**20))
# The trial restore that follows the night run would make the hourly slot skip.
wait_idle()

# ---- an hourly run: the site's database only ----
cli(["set", SITE, "db=hourly"])
now = sh("date +%H:%M").strip()
h, mi = map(int, now.split(":"))
cli(["set", "night=%02d:%02d" % ((h + (mi + 1) // 60 - 2) % 24, (mi + 1) % 60)])
before = {x["id"] for x in runs(50)}
wait(lambda: any(x["kind"] == "hourly" and x["id"] not in before and x.get("finished", "").startswith("2")
                 for x in runs(50)), "the hourly run", timeout=300)
hr = next(x for x in runs(50) if x["kind"] == "hourly" and x["id"] not in before)
if [(p["site"], p["part"]) for p in hr["parts"]] != [(SITE, "db")] or hr.get("error"):
    fail("hourly run: %s" % hr)
ok("saatlik yedek zamanlayıcıdan: yalnız %s veritabanı" % SITE)
cli(["set", SITE, "reset"])
h, mi = map(int, sh("date +%H:%M").strip().split(":"))
cli(["set", "night=%02d:%02d" % ((h + 3) % 24, mi)])

# ---- a frozen site, the agent restarted while it is paused ----
cli(["set", SITE, "freeze=on"])
# Many files make the copy (the only paused part) last long enough to restart the agent in it.
sh("mkdir -p %s/cok && cd %s/cok && seq 1 20000 | xargs -P4 -n500 sh -c 'for i; do head -c 2000 /dev/urandom > f$i; done' sh "
   "&& head -c 300000000 /dev/urandom > %s/buyuk.bin && chown -R 101:101 %s/cok %s/buyuk.bin" % (DATA, DATA, DATA, DATA, DATA))
paused_seen = []
bg = subprocess.Popen(["docker", "exec", C, "/usr/local/bin/footops", "backup", "run", SITE], stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
deadline = time.time() + 120
while sh("docker inspect -f '{{.State.Paused}}' %s" % SITE).strip() != "true":
    if time.time() > deadline:
        fail("the site was never seen paused")
    time.sleep(0.02)
paused_seen.append(time.time())
sh("systemctl restart footops-agent")
bg.wait()
wait(lambda: sh("curl -s -o /dev/null -w %{http_code} --unix-socket /run/footops/agent.sock http://agent/v1/overview",
                check=False).strip() == "200", "agent back")
wait(lambda: sh("docker inspect -f '{{.State.Paused}}' %s" % SITE).strip() == "false", "the site unpaused", timeout=60)
if sh("test -e /var/lib/footops/backup/paused.json && echo y || true").strip():
    fail("record of paused containers left")
wait(lambda: not tools(), "tool containers removed", timeout=60)
cut = latest_job()
if not cut or "yarıda kaldı" not in cut.get("detail", ""):
    fail("the cut-off run is not recorded as interrupted: %s" % cut)
ok("dondurulmuş site agent yeniden başlayınca çözüldü (%.1f sn sonra), araç container'ı kalmadı, iş 'yarıda kaldı'" % (time.time() - paused_seen[0]))
t0 = time.time()
code, out = cli(["run", SITE])
measure["frozen_files_s"] = round(time.time() - t0, 1)
m = re.search(r"✓ %s (.+?) donuk kaldı: dosyaları kopyalandı \((\d+) dosya" % SITE, out)
if not m:
    fail("freeze steps:\n" + out)
measure["paused_for"], measure["frozen_files"] = m.group(1), int(m.group(2))
if sh("docker inspect -f '{{.State.Paused}}' %s" % SITE).strip() != "false":
    fail("site left paused")
sh("rm -rf /root/y1restore/*")
rr("restore latest --path /footops/%s/files --target /r --include /footops/%s/files/buyuk.bin" % (SITE, SITE),
   extra="-v /root/y1restore:/r --cap-drop ALL --cap-add CHOWN --cap-add FOWNER --cap-add DAC_OVERRIDE")
if sh("sha256sum /root/y1restore/footops/%s/files/buyuk.bin" % SITE).split()[0] != sh("sha256sum %s/buyuk.bin" % DATA).split()[0]:
    fail("big file differs")
ok("yarıda kalan yedekten sonra yeni yedek tamam (kilit temizlendi); site yalnız kopya için %s donuk kaldı (%d dosya, 300 MB); büyük dosya birebir" %
   (measure["paused_for"], measure["frozen_files"]))
sh("rm -rf %s/buyuk.bin %s/cok" % (DATA, DATA))
cli(["set", SITE, "reset"])
cli(["set", "night=04:00"])

# ---- cancel ----
_, r = api("POST", "/v1/backup/run", {"actor": ACTOR})
wait(lambda: tools(), "a tool container", timeout=60)
api("POST", "/v1/jobs/%s/cancel" % r["job_id"], {"actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error") != "iptal edildi":
    fail("cancel: %s" % j.get("error"))
wait(lambda: not tools(), "tool container to go after cancel", timeout=60)
code, out = cli(["run", SITE])
# What the cancelled run left (its lock) must not stop the night's exclusive commands.
now = sh("date +%H:%M").strip()
h, mi = map(int, now.split(":"))
before = {x["id"] for x in runs(50)}
sh("python3 -c 'import json; p=\"/var/lib/footops/backup/state.json\"; s=json.load(open(p)); s.pop(\"night_done\", None); json.dump(s, open(p, \"w\"))'")
cli(["set", "night=%02d:%02d" % ((h + (mi + 1) // 60) % 24, (mi + 1) % 60)])
wait(lambda: any(x["kind"] == "daily" and x["id"] not in before and x.get("finished", "").startswith("2") for x in runs(50)),
     "the night run after a cancel", timeout=300)
nr2 = next(x for x in runs(50) if x["kind"] == "daily" and x["id"] not in before)
if nr2.get("error"):
    fail("night after a cancel: %s" % nr2["error"])
cli(["set", "night=04:00"])
ok("iptal: container durdu; sonraki yedek ve gece işi (forget / prune, kilit isteyen) sorunsuz")

# ---- what it costs ----
measure["agent_rss_after"] = rss("footops agent")
measure["repo_bytes_on_s3"] = int(sh("du -sb /var/lib/docker/volumes/it-s3/_data/%s | cut -f1" % BUCKET).strip())
measure["cache_bytes"] = int(sh("du -sb /var/cache/footops/restic | cut -f1").strip())
_, st = api("GET", "/v1/backup")
if len(st.get("last", {})) < 5:
    fail("status: %s" % st)
code, out = cli(["status"])
print("    " + "\n    ".join(out.strip().splitlines()))
sh("rm -rf /root/y1restore /root/y1cfg")
print(json.dumps(measure, indent=2, ensure_ascii=False))
print("Y1: hepsi geçti")
