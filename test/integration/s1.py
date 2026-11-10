#!/usr/bin/env python3
"""Faz S against a real install: groups are the folders under /srv (a site in a group not
there yet makes it, private like the others; a move makes the next one and takes the
emptied one away; purging a group's last removed site takes the group), a site's own
request body cap (Caddy answers 413 before the app has read more; changing it leaves the
container alone), and container metrics read from cgroups instead of dockerd's stats.

Usage: s1.py <container> <login-file>   (after f0: it uses f0's certificate)
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import re
import sys
import time

from itlib import ACTOR, DOMAIN, api, fail, fresh_totp, ok, rss, sh, wait, wait_idle, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
NAME, HOST = "itgrup", "grup." + DOMAIN  # f0's certificate covers *.it.example.com
G1, G2 = "yenigrup", "ikincigrup"
IMAGE = "traefik/whoami:v1.10.1"
JAR = "/tmp/s1-jar"
measure = {}


def groups():
    return api("GET", "/v1/groups")[1]


def mode(path):
    return sh("stat -c '%%a %%U:%%G' %s" % path).strip()


def exists(path):
    return sh("test -e %s && echo y || true" % path).strip() == "y"


def post(path, data, chunked=False):
    """Status of a POST of a file through Caddy to the test site."""
    extra = " -H 'Transfer-Encoding: chunked'" if chunked else ""
    return sh("curl -sk -o /dev/null -w '%%{http_code}' --max-time 60 --resolve %s:443:127.0.0.1 -H 'Content-Type: application/json'%s "
              "--data-binary @%s https://%s%s" % (HOST, extra, data, HOST, path), check=False).strip()


def get(path="/"):
    return sh("curl -sk -o /dev/null -w '%%{http_code}' --max-time 20 --resolve %s:443:127.0.0.1 https://%s%s" % (HOST, HOST, path), check=False).strip()


def job(method, path, body):
    code, r = api(method, path, body, check=False)
    if code == 409:  # one job at a time: a backup the test server runs on its own, say
        wait_idle()
        code, r = api(method, path, body, check=False)
    if code != 202:
        fail("%s %s -> %d %s" % (method, path, code, r))
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("%s: %s\n%s" % (path, j["error"], "\n".join(j["steps"])))
    return "\n".join(j["steps"])


def remove_and_purge():
    """Removes the test site, then its removed folders for good."""
    _, sites = api("GET", "/v1/sites")
    if any(s["spec"]["name"] == NAME for s in sites):
        job("POST", "/v1/sites/%s/remove" % NAME, {"actor": ACTOR})
    steps = ""
    _, removed = api("GET", "/v1/removed")
    for r in removed or []:
        if r["site"] == NAME:
            steps += job("POST", "/v1/removed/%s/purge" % r["id"], {"confirm": NAME, "actor": ACTOR})
    return steps


def sh_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def web(path, form=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar."""
    args = "".join(" --data-urlencode " + sh_quote("%s=%s" % (k, v)) for k, v in (form or {}).items())
    out = sh("curl -sS -b %s -c %s -o /tmp/s1-page -w '%%{http_code} %%{redirect_url}' %s 'http://localhost:7701%s'; echo; cat /tmp/s1-page"
             % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, _, loc = head.partition(" ")
    return int(code), loc.replace("http://localhost:7701", ""), body


print("S1 senaryoları (Faz S: serbest gruplar, gövde sınırı, cgroup metrikleri)")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
remove_and_purge()  # an earlier run cut short
measure["agent_rss_before"] = rss("footops agent")

# ---- S1: the groups are the folders under /srv ----
sh("mkdir -p /srv/_yedek-s1 /srv/.gizli-s1 /srv/Buyuk && touch /srv/dosya-s1 && ln -sfn /srv/musteri /srv/baglanti-s1")
gs = groups()
sh("rm -rf /srv/_yedek-s1 /srv/.gizli-s1 /srv/Buyuk /srv/dosya-s1 /srv/baglanti-s1")
if "musteri" not in gs or any(g in gs for g in ("core", "_yedek-s1", ".gizli-s1", "Buyuk", "dosya-s1", "baglanti-s1", G1, G2)):
    fail("groups: %s" % gs)
ok("gruplar /srv'deki klasörler: %s (core, _ / . ile başlayan, büyük harfli, dosya, bağlantı yok)" % ", ".join(gs))

spec = {"name": NAME, "group": G1, "domains": [HOST], "image": IMAGE, "port": 80, "memory_mb": 64, "database": False}
for bad, want in (("core", "ayrılmış"), ("_gizli", "grup adı"), ("", "grup adı")):
    code, r = api("POST", "/v1/sites/check", {"spec": dict(spec, group=bad), "env": {}, "actor": ACTOR}, check=False)
    if code != 422 or want not in json.dumps(r, ensure_ascii=False):
        fail("group %r: %d %s" % (bad, code, r))
ok("grup adı kuralı: core ayrılmış, _ ile başlayan ve boş ad reddedildi")

t0 = time.time()
steps = job("POST", "/v1/sites", {"spec": spec, "env": {}, "actor": ACTOR})
measure["create_s"] = round(time.time() - t0, 1)
if "Yeni grup: /srv/" + G1 not in steps or mode("/srv/" + G1) != "700 root:footops" or G1 not in groups():
    fail("new group: %s %s\n%s" % (mode("/srv/" + G1), groups(), steps))
wait(lambda: get() == "200", "the site in the new group")
ok("yeni grupta site: /srv/%s açıldı (700 root:footops, bugünkü gruplar gibi), site 200" % G1)

steps = job("POST", "/v1/sites/%s/move" % NAME, {"group": G2, "confirm": NAME, "actor": ACTOR})
if not exists("/srv/%s/%s/footops.json" % (G2, NAME)) or exists("/srv/" + G1) or mode("/srv/" + G2) != "700 root:footops":
    fail("move:\n" + steps)
gs = groups()
if G2 not in gs or G1 in gs or "Yeni grup: /srv/" + G2 not in steps or G1 + " grubunda site kalmadı" not in steps:
    fail("groups after the move: %s\n%s" % (gs, steps))
wait(lambda: get() == "200", "the site after the move")
nets = sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' %s" % NAME).split()
if nets != ["prj_%s_%s" % (G2, NAME)]:
    fail("networks after the move: %s" % nets)
ok("yeni gruba taşındı (kalıcı klasörsüz site: container yeni ağda yeniden oluştu): /srv/%s açıldı, boşalan /srv/%s kalktı, site 200" % (G2, G1))

# A move that fails once the container was made again (Caddy down) is put back: the old
# container is made again on its own network, the group the move made goes. (Caddy's own
# reload cannot run while it is down: its file is put back and loaded when it starts.)
wait_idle()  # a job of the server's own (a backup) would refuse the move with 409
sh("docker stop footops-caddy >/dev/null")
try:
    code, r = api("POST", "/v1/sites/%s/move" % NAME, {"group": G1, "confirm": NAME, "actor": ACTOR}, check=False)
    j = wait_job(r["job_id"]) if code == 202 else {"error": "%d %s" % (code, r)}
finally:
    sh("docker start footops-caddy >/dev/null")
steps = "\n".join(j.get("steps", []))
nets = sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' %s" % NAME).split()
if not j.get("error") or "Eski hâline dönülüyor" not in steps or nets != ["prj_%s_%s" % (G2, NAME)] or exists("/srv/" + G1) \
        or not exists("/srv/%s/%s/footops.json" % (G2, NAME)):
    fail("failed move: %s %s\n%s" % (j.get("error"), nets, steps))
wait(lambda: get() == "200", "the site after Caddy is back", timeout=60)
ok("yarıda kalan taşıma (Caddy kapalı) geri alındı: container eski ağında yeniden oluştu, açılan /srv/%s kalktı, site 200" % G1)

# ---- S2: the site's own request body cap ----
cid = sh("docker inspect -f '{{.Id}}' %s" % NAME).strip()
upd = {"domains": [HOST], "image": IMAGE, "port": 80, "memory_mb": 64, "data_mount": "", "max_body_mb": 1}
steps = job("POST", "/v1/sites/%s/update" % NAME, {"update": upd, "actor": ACTOR})
block = sh("cat /srv/core/caddy/etc/sites/%s.caddy" % NAME)
if "max_size 1MiB" not in block or "en büyük istek 100 → 1 MB" not in steps:
    fail("cap 1 MB: %s\n%s" % (block, steps))
if sh("docker inspect -f '{{.Id}}' %s" % NAME).strip() != cid:
    fail("container recreated for a cap change")
if '"max_body_mb": 1' not in sh("cat /srv/%s/%s/footops.json" % (G2, NAME)):
    fail("footops.json: " + sh("cat /srv/%s/%s/footops.json" % (G2, NAME)))
# whoami's /health decodes a JSON body before it answers (400 for a string): the app reads
# all of it, as an app parsing a form or JSON would.
sh(r"""for n in 1048576 1048577 3145728; do { printf '"'; head -c $((n-2)) /dev/zero | tr '\0' a; printf '"'; } > /tmp/s1-$n; done""")
if sh("stat -c %s /tmp/s1-1048576 /tmp/s1-1048577 /tmp/s1-3145728").split() != ["1048576", "1048577", "3145728"]:
    fail("body files")
codes = [post("/health", "/tmp/s1-1048576"), post("/health", "/tmp/s1-1048577"), post("/health", "/tmp/s1-3145728", chunked=True)]
if codes != ["400", "413", "413"]:
    fail("1 MB cap: exactly 1 MiB / +1 byte / 3 MiB chunked -> %s (want 400 413 413)" % codes)
ok("en büyük istek 1 MB: tam 1 MiB uygulamaya ulaştı (400 = uygulamanın cevabı), +1 bayt 413, chunked 3 MiB 413; container aynı")

upd["max_body_mb"] = 100
steps = job("POST", "/v1/sites/%s/update" % NAME, {"update": upd, "actor": ACTOR})
if "max_size 100MiB" not in sh("cat /srv/core/caddy/etc/sites/%s.caddy" % NAME) or "max_body_mb" in sh("cat /srv/%s/%s/footops.json" % (G2, NAME)):
    fail("back to the default:\n" + steps)
if post("/health", "/tmp/s1-3145728", chunked=True) != "400":
    fail("3 MiB with the default cap")
sh("rm -f /tmp/s1-1048576 /tmp/s1-1048577 /tmp/s1-3145728")
ok("varsayılana dönüş: 100MiB, footops.json'da alan yok, 3 MiB yine uygulamaya ulaşıyor")

# ---- the panel: groups offered, the cap shown ----
user, pw, secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
tok = re.search(r'name="csrf" value="([^"]+)"', page).group(1)
code, loc, _ = web("/login", {"csrf": tok, "username": user, "password": pw, "code": fresh_totp(secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
_, _, page = web("/sites/new/form?tpl=node")
if '<datalist id="groups">' not in page or '<option value="%s">' % G2 not in page or '<option value="musteri">' not in page or 'name="max_body_mb" value="100"' not in page:
    fail("new site form:\n" + page)
_, _, page = web("/sites/%s/move" % NAME)
if '<option value="musteri">' not in page or '<option value="%s">' % G2 in page:
    fail("move form:\n" + page)
_, _, page = web("/sites/%s" % NAME)
if "<th>En büyük istek</th><td>100 MB</td>" not in page:
    fail("site page:\n" + page)
sh("rm -f %s /tmp/s1-page" % JAR)
ok("panel: yeni site formu grupları öneriyor (%s dahil), taşıma formu bulunduğu grubu önermiyor, site sayfasında en büyük istek" % G2)

# ---- S3: container metrics from cgroups ----
_, o = api("GET", "/v1/overview")
c = next((x for x in o["containers"] if x["name"] == NAME), None)
scope = "/sys/fs/cgroup/system.slice/docker-%s.scope" % cid
cur = int(sh("cat %s/memory.current" % scope))
inactive = int(re.search(r"(?m)^inactive_file (\d+)", sh("cat %s/memory.stat" % scope)).group(1))
if not c or c["mem_limit"] != 64 << 20 or not 0 < c["mem_usage"] <= 64 << 20 or abs(c["mem_usage"] - (cur - inactive)) > 4 << 20 or c["cpu_percent"] < 0:
    fail("overview: %s (cgroup now %d - %d)" % (c, cur, inactive))
mongo = next((x for x in o["containers"] if x["name"] == "footops-mongo"), None)
if not mongo or mongo["mem_usage"] <= 0 or max(x["cpu_percent"] for x in o["containers"]) <= 0 or o.get("errors"):
    fail("mongo / errors: %s %s\n%s" % (mongo, o.get("errors"), o["containers"]))
measure.update(site_mem=c["mem_usage"], site_cpu=round(c["cpu_percent"], 2), mongo_mem=mongo["mem_usage"], mongo_cpu=round(mongo["cpu_percent"], 2))
ok("overview cgroup'tan: %s %.1f MiB / 64 MiB (cgroup: %.1f MiB), MongoDB %%%.2f CPU, %.0f MiB"
   % (NAME, c["mem_usage"] / 2**20, (cur - inactive) / 2**20, mongo["cpu_percent"], mongo["mem_usage"] / 2**20))

# dockerd's own CPU over 30 s with the agent sampling: no stats requests left (each cost
# it ~3 ms and the agent's sample a second or two of waiting).
ticks = "for p in $(pgrep -x dockerd) $(pgrep -x containerd); do awk '{s+=$14+$15} END {print s}' /proc/$p/stat; done | awk '{s+=$1} END {print s}'"
t0 = int(sh(ticks))
time.sleep(30)
measure["dockerd_containerd_ticks_30s"] = int(sh(ticks)) - t0
measure["containers"] = len(o["containers"])
ok("dockerd + containerd 30 sn'de %d tick (10 ms), %d container örneklenirken" % (measure["dockerd_containerd_ticks_30s"], measure["containers"]))

# The share against docker stats: one core kept busy in MongoDB's container for 35 s
# (mongosh runs in its cgroup). The agent's share is the last 10 s sample's, docker
# stats' the last second: read once the last sample lies wholly inside the busy time.
sh("docker exec -d footops-mongo mongosh --quiet --nodb --eval 'const e = Date.now() + 35000; while (Date.now() < e) {}'")
time.sleep(24)
ours = next(x for x in api("GET", "/v1/overview")[1]["containers"] if x["name"] == "footops-mongo")["cpu_percent"]
theirs = float(sh("docker stats --no-stream --format '{{.CPUPerc}}' footops-mongo").strip().rstrip("%"))
time.sleep(12)  # the loop ends
measure.update(busy_cpu_ours=round(ours, 1), busy_cpu_docker_stats=round(theirs, 1))
if ours < 75 or theirs < 75 or abs(ours - theirs) > 25:
    fail("one busy core: agent %.1f%%, docker stats %.1f%%" % (ours, theirs))
ok("bir çekirdek meşgulken: agent %%%.0f, docker stats %%%.0f (aynı ölçek: 100 = bir çekirdek)" % (ours, theirs))

# ---- Caddy made again (an upgrade of the core stack) loses its site networks: the
# agent finds that from Caddy's own networks and attaches it again within its 10 s ----
before = set(sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' footops-caddy").split())
if "prj_%s_%s" % (G2, NAME) not in before:
    fail("Caddy not on the site's network: %s" % before)
caddy_id = sh("docker inspect -f '{{.Id}}' footops-caddy").strip()
t0 = time.time()
sh("docker compose -f /srv/core/compose.yaml up -d --force-recreate --no-deps caddy >/dev/null 2>&1")
after = sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' footops-caddy").split()
if sh("docker inspect -f '{{.Id}}' footops-caddy").strip() == caddy_id:
    fail("Caddy was not made again")
wait(lambda: set(sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' footops-caddy").split()) >= before,
     "Caddy attached to the site networks again", timeout=60)
wait(lambda: get() == "200", "the site through the new Caddy", timeout=60)
measure["caddy_reattach_s"] = round(time.time() - t0, 1)
ok("Caddy yeniden oluşturuldu (%d ağla başladı): agent %d site ağına %.0f sn içinde yeniden bağladı, site 200"
   % (len(after), len(before) - 1, measure["caddy_reattach_s"]))

# ---- the last site of a group removed for good: the group goes ----
steps = remove_and_purge()
if exists("/srv/" + G2) or G2 in groups() or "grubun klasörü de silindi" not in steps or not exists("/srv/musteri"):
    fail("group after purge: %s\n%s" % (groups(), steps))
ok("son site kalıcı silinince grup da kalktı (/srv/%s yok); whoami imajı itsite kullandığı için duruyor" % G2)
measure["agent_rss_after"] = rss("footops agent")
print("ÖLÇÜMLER " + json.dumps(measure))
print("S1: tüm senaryolar geçti")
