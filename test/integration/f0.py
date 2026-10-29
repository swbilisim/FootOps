#!/usr/bin/env python3
"""F0 scenarios against a real install in the test container: a database site, a copy
taken by a limited tool container, download with resume, a trial restore of the copy,
cancellation, an agent restart in the middle of a job, and the panel's download path.

Usage: f0.py <container> <login-file>   (install.py writes the login file)
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import re
import subprocess
import sys
import threading
import time

from itlib import ACTOR, DOMAIN, SITE, api, fail, fresh_totp, ok, rss, sh, tools, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
measure = {}


def job_staging():
    """Job directories in the staging area (they hold connection files); exports and
    uploads (exp-*, upl-*) stay there for a day by design."""
    return [d for d in sh("ls -A /var/lib/footops/staging").split() if not d.startswith(("exp-", "upl-"))]


print("F0 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
api("GET", "/v1/overview")
ok("agent cevap veriyor")
measure["agent_rss_idle"] = rss("footops agent")

# ---- MongoDB's healthcheck: a ping sent by bash, no mongosh (~260 MB every 30 s) ----
hc = sh("docker inspect footops-mongo --format '{{json .Config.Healthcheck.Test}}'")
if "mongosh" in hc or '"bash"' not in hc:
    fail("mongo healthcheck: %s" % hc)
wait(lambda: sh("docker inspect footops-mongo --format '{{.State.Health.Status}}'").strip() == "healthy",
     "MongoDB healthy", timeout=120)
runs = [l.split() for l in sh("docker inspect footops-mongo --format "
                              "'{{range .State.Health.Log}}{{.Start.UnixNano}} {{.End.UnixNano}} {{.ExitCode}}\n{{end}}'").splitlines() if l.strip()]
# Probes during a fresh data directory's first start may fail (start_period): the last
# one counts.
if not runs or runs[-1][2] != "0":
    fail("mongo healthcheck failed: %s" % runs)
measure["mongo_healthcheck_ms"] = [round((int(e) - int(s)) / 1e6, 1) for s, e, code in runs if code == "0"]
ok("MongoDB sağlık kontrolü bash ile, sağlıklı (%s ms)" % measure["mongo_healthcheck_ms"])

# ---- site with a database ----
code, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    sh("cd /tmp && openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 "
       "-subj /CN=%s -addext subjectAltName=DNS:%s,DNS:*.%s -keyout k.pem -out c.pem 2>/dev/null" % (DOMAIN, DOMAIN, DOMAIN))
    api("POST", "/v1/certs", {"cert": sh("cat /tmp/c.pem"), "key": sh("cat /tmp/k.pem"), "actor": ACTOR})
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                             "image": "traefik/whoami:v1.10.1", "port": 80, "memory_mb": 64,
                                             "database": True}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    ok("veritabanlı deneme sitesi oluşturuldu")

uri = re.search(r'MONGODB_URI: "([^"]+)"', sh("cat /srv/musteri/%s/compose.yaml" % SITE)).group(1)
password = re.match(r"mongodb://[^:]+:([^@]+)@", uri).group(1)
local = uri.replace("@mongo:27017", "@127.0.0.1:27017")
seed = """
const d = db.getSiblingDB("%s");
d.getCollectionNames().filter(n => !n.startsWith("system.")).forEach(n => d.getCollection(n).drop());
for (let i = 0; i < 10; i++) d.small.insertOne({i, ad: "çğıöşü " + i});
// Random text compresses poorly, so the copy takes long enough to be cancelled.
const rnd = () => Array.from({length: 16}, () => Math.random().toString(36).slice(2)).join("");
for (let b = 0; b < 100; b++) {
  const docs = [];
  for (let i = 0; i < 2000; i++) docs.push({b, i, pad: rnd(), t: new Date()});
  d.big.insertMany(docs, {ordered: false});
}
for (let i = 0; i < 5; i++) d.media.insertOne({i, yol: "/uploads/" + i + ".jpg"});
print("SEEDED " + (d.small.countDocuments() + d.big.countDocuments() + d.media.countDocuments()));
""" % SITE
out = sh("docker exec -i footops-mongo mongosh --quiet '%s' --file /dev/stdin" % local, stdin=seed.encode())
if "SEEDED 200015" not in out:
    fail("seed: " + out)
ok("200.015 belge yazıldı (site kullanıcısıyla)")

# ---- copy ----
seen_secret = []
stop = threading.Event()


def watch_ps():
    while not stop.is_set():
        out = subprocess.run(["docker", "exec", C, "ps", "-eo", "args"], capture_output=True).stdout.decode(errors="replace")
        if password in out:
            seen_secret.append(out)
        time.sleep(0.2)


watcher = threading.Thread(target=watch_ps)
watcher.start()
t0 = time.time()
_, r = api("POST", "/v1/sites/%s/snapshots" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
stop.set()
watcher.join()
if j.get("error"):
    fail("snapshot: %s\n%s" % (j["error"], "\n".join(j["steps"])))
if seen_secret:
    fail("password visible in the process list")
ok("kopya alındı; şifre süreç listesinde hiç görünmedi")
print("    " + "\n    ".join(j["steps"]))
u = j.get("usage", {})
if not u.get("peak_mem") or not u.get("cpu"):
    fail("usage not measured: %s" % u)
measure.update(copy_wall_s=round(time.time() - t0, 2), copy_peak_mem=u["peak_mem"], copy_cpu_ms=u["cpu"] // 1_000_000,
               copy_bytes=j["bytes"])
ok("ölçüm: en çok %.1f MB bellek, %d ms CPU, %.1f MB" % (u["peak_mem"] / 2**20, u["cpu"] // 1_000_000, j["bytes"] / 2**20))

_, snaps = api("GET", "/v1/sites/%s/snapshots" % SITE)
s = next(x for x in snaps if x["id"] == j["result"])
if s["documents"] != 200015 or s["collections"] != {"small": 10, "big": 200000, "media": 5}:
    fail("manifest counts: %s" % s)
ok("manifest: 3 koleksiyon, 200.015 belge")
if tools():
    fail("tool container left: %s" % tools())
if job_staging():
    fail("staging not empty")
if sh("stat -c %a /var/lib/footops/snapshots/" + SITE).strip() != "700":
    fail("snapshot dir mode")
ok("araç container'ı silindi, staging boş, kopya klasörü 0700")

# ---- download and resume through the agent, then a trial restore ----
dl = "curl -sS --unix-socket /run/footops/agent.sock -o /tmp/dl.gz http://agent/v1/sites/%s/snapshots/%s/download" % (SITE, s["id"])
sh(dl)
if sh("sha256sum /tmp/dl.gz").split()[0] != s["sha256"]:
    fail("download checksum")
sh(dl.replace("-o /tmp/dl.gz", "-H 'Range: bytes=100-' -o /tmp/dl.part"))
if sh("tail -c +101 /tmp/dl.gz | sha256sum").split()[0] != sh("sha256sum /tmp/dl.part").split()[0]:
    fail("range download")
ok("indirme sha256 tutuyor; Range ile devam doğru")

root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)
restore = ("docker cp /tmp/dl.gz footops-mongo:/tmp/dl.gz && docker exec -i footops-mongo sh -c "
           "'umask 077; cat > /tmp/r.yaml; mongorestore --quiet --config /tmp/r.yaml --authenticationDatabase admin -u root "
           "--archive=/tmp/dl.gz --gzip --nsFrom \"%s.*\" --nsTo \"itcheck.*\"; rm -f /tmp/r.yaml /tmp/dl.gz'" % SITE)
sh(restore, stdin=("password: %s\n" % root_pw).encode())
count = sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
           stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);'
                  'const d=c.getDB("itcheck");print("N "+(d.small.countDocuments()+d.big.countDocuments()+d.media.countDocuments()));'
                  'print("TR "+d.small.findOne({i:3}).ad);d.dropDatabase()' % json.dumps(root_pw)).encode())
if "N 200015" not in count or "TR çğıöşü 3" not in count:
    fail("trial restore: " + count)
ok("kopya geçici bir veritabanına geri yüklendi: 200.015 belge, Türkçe karakterler sağlam")

# ---- cancel ----
before = len(snaps)
time.sleep(1.1)  # copies are named by the second
_, r = api("POST", "/v1/sites/%s/snapshots" % SITE, {"actor": ACTOR})
wait(lambda: tools(), "tool container to start")
api("POST", "/v1/jobs/%s/cancel" % r["job_id"], {"actor": ACTOR})
j = wait_job(r["job_id"])
if not j.get("cancelled") or j.get("error") != "iptal edildi":
    fail("cancel: %s" % j)
wait(lambda: not tools(), "tool container to go after cancel", 30)
_, snaps = api("GET", "/v1/sites/%s/snapshots" % SITE)
if len(snaps) != before or sh("ls /var/lib/footops/snapshots/%s | wc -l" % SITE).strip() != str(before):
    fail("cancelled copy left files")
ok("iptal: container durdu, yarım dosya kalmadı")

# ---- agent restart in the middle of a job ----
time.sleep(1.1)
_, r = api("POST", "/v1/sites/%s/snapshots" % SITE, {"actor": ACTOR})
wait(lambda: tools(), "tool container to start")
sh("systemctl restart footops-agent")
wait(lambda: sh("curl -s -o /dev/null -w %{http_code} --unix-socket /run/footops/agent.sock http://agent/v1/overview",
                check=False).strip() == "200", "agent after restart")
_, j = api("GET", "/v1/jobs/" + r["job_id"])
if not (j["done"] and j.get("interrupted") and "yarıda" in j.get("error", "")):
    fail("interrupted job: %s" % j)
wait(lambda: not tools(), "leftover tool container to be removed", 30)
_, audit = api("GET", "/v1/audit?limit=20")
if not any(a["action"] == "db.snapshot" and "yarıda" in a.get("detail", "") for a in audit):
    fail("interrupted job not in the audit log")
_, snaps = api("GET", "/v1/sites/%s/snapshots" % SITE)
if len(snaps) != before or sh("ls /var/lib/footops/snapshots/%s | wc -l" % SITE).strip() != str(before):
    fail("half-written copy left")
if job_staging():
    fail("staging (connection file) left after the restart")
ok("agent yeniden başladı: iş 'yarıda kaldı' oldu, denetime yazıldı; artık container, yarım kopya ve geçici dosyalar silindi")

# ---- the panel's own download path (web -> agent), with a real login ----
user, pw, secret = open(LOGIN).read().split()
jar = "/tmp/jar"
sh("rm -f %s" % jar)
page = sh("curl -sS -c %s -b %s http://localhost:7701/login" % (jar, jar))
csrf = re.search(r'name="csrf" value="([^"]+)"', page).group(1)
sh("curl -sS -o /dev/null -c %s -b %s --data-urlencode csrf=%s --data-urlencode username=%s "
   "--data-urlencode password@- --data-urlencode code=%s http://localhost:7701/login" % (jar, jar, csrf, user, fresh_totp(secret)),
   stdin=pw.encode())
page = sh("curl -sS -b %s http://localhost:7701/sites/%s" % (jar, SITE))
if "Veritabanı kopyaları" not in page or s["id"] + "/download" not in page:
    fail("site page lacks copies:\n" + page[:2000])
web_rss_before = rss("footops web")
hdr = sh("curl -sS -D - -b %s -o /tmp/web.gz http://localhost:7701/sites/%s/snapshots/%s/download" % (jar, SITE, s["id"]))
if sh("sha256sum /tmp/web.gz").split()[0] != s["sha256"] or "attachment" not in hdr:
    fail("panel download:\n" + hdr)
hdr = sh("curl -sS -D - -b %s -H 'Range: bytes=1000-' -o /dev/null http://localhost:7701/sites/%s/snapshots/%s/download" % (jar, SITE, s["id"]))
if " 206" not in hdr.splitlines()[0]:
    fail("panel range:\n" + hdr)
measure.update(web_rss_before_download=web_rss_before, web_rss_after_download=rss("footops web"),
               agent_rss_after=rss("footops agent"))
ok("panelden giriş + indirme (sha256 tutuyor) + Range 206")

sh("rm -f /tmp/dl.gz /tmp/dl.part /tmp/web.gz %s /tmp/k.pem /tmp/c.pem" % jar)
print("ÖLÇÜMLER " + json.dumps(measure))
print("F0: tüm senaryolar geçti")
