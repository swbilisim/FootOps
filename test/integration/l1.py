#!/usr/bin/env python3
"""Faz L (panel-tasarim §32) against a real install: what a site's container wrote
outside its persistent folder, lost when the container is made anew. A real nginx
image's own run-time writes are not counted; a redeploy and a move are refused until
someone says go on; an unpark after a FootOps update (the container made from a compose
file FootOps no longer writes) starts it as it is; an update that makes a persistent
folder takes what the container held there; the server check warns.

Usage: l1.py <container> <login-file>   (after f0: it uses f0's certificate)
Exits non-zero on the first failed check.
"""
import json
import sys

from itlib import ACTOR, DOMAIN, api, fail, ok, sh, wait, wait_idle, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
NAME, HOST, GROUP = "itkatman", "katman." + DOMAIN, "katman"  # f0's certificate covers *.it.example.com
IMAGE = "nginxinc/nginx-unprivileged:1.27-alpine"  # uid 101, port 8080: the copy's owner is tested too
HTML = "/usr/share/nginx/html"
COMPOSE = "/srv/%s/%s/compose.yaml" % (GROUP, NAME)


def job(method, path, body):
    code, r = api(method, path, body, check=False)
    if code == 409 and not (r or {}).get("layer"):  # one job at a time: a backup the test server runs on its own
        wait_idle()
        code, r = api(method, path, body, check=False)
    if code != 202:
        fail("%s %s -> %d %s" % (method, path, code, r))
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("%s: %s\n%s" % (path, j["error"], "\n".join(j["steps"])))
    return "\n".join(j["steps"])


def refused(method, path, body, files):
    """The agent refuses, with the layer's report, what would make the container anew."""
    code, r = api(method, path, body, check=False)
    layer = (r or {}).get("layer") or {}
    if code != 409 or layer.get("files") != files:
        fail("%s %s: %d %s" % (method, path, code, json.dumps(r, ensure_ascii=False)))
    return layer


def layer():
    return api("GET", "/v1/sites/%s/layer" % NAME)[1]


def get(path):
    return sh("curl -sk --max-time 20 --resolve %s:443:127.0.0.1 -w ' %%{http_code}' https://%s%s" % (HOST, HOST, path), check=False).strip()


def container_id():
    return sh("docker inspect -f '{{.Id}}' %s" % NAME).strip()


def interval():
    return sh("docker inspect -f '{{.Config.Healthcheck.Interval}}' %s" % NAME).strip()


def check_finding():
    r = json.loads(sh("footops doctor --json || true"))  # warnings set its exit code
    return {f["id"]: f for f in r["findings"] if f["id"].startswith("site-layer")}


def remove_and_purge():
    _, sites = api("GET", "/v1/sites")
    if any(s["spec"]["name"] == NAME for s in sites):
        job("POST", "/v1/sites/%s/remove" % NAME, {"actor": ACTOR})
    _, removed = api("GET", "/v1/removed")
    for r in removed or []:
        if r["site"] == NAME:
            job("POST", "/v1/removed/%s/purge" % r["id"], {"confirm": NAME, "actor": ACTOR})


print("L1 senaryoları (Faz L: container katmanı koruması)")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
remove_and_purge()  # an earlier run cut short

spec = {"name": NAME, "group": GROUP, "domains": [HOST], "image": IMAGE, "port": 8080, "memory_mb": 64, "database": False}
job("POST", "/v1/sites", {"spec": spec, "env": {}, "actor": ACTOR})
wait(lambda: get("/").endswith(" 200"), "the site to answer", timeout=90)

# A fresh nginx container writes at start (its pid, cache, conf.d): none of it counts.
r = layer()
if r.get("files") or r.get("error"):
    fail("a fresh container: %s\n%s" % (r, sh("docker diff %s" % NAME)))
ok("yeni nginx container'ı: katmanda sayılan dosya yok (önbellek, pid, conf.d yok sayıldı: %d değişiklik)" %
   len(sh("docker diff %s" % NAME).split("\n")))

# An empty folder made at start, and one whose file was made and deleted again: folders,
# no files.
sh("docker exec -u 0 %s sh -c 'mkdir -p /srv/bos/ic && touch %s/gecici && rm %s/gecici'" % (NAME, HTML, HTML))
r = layer()
if r.get("files") or r.get("error"):
    fail("empty folders: %s\n%s" % (r, sh("docker diff %s" % NAME)))
ok("boş klasör ve yalnız içi değişmiş klasör dosya sayılmadı")

# The container as a FootOps update finds it: made from a compose file FootOps no longer
# writes (an older healthcheck interval). Then the app writes outside any persistent folder.
sh("sed -i 's/interval: 2m/interval: 30s/' %s && docker compose --file %s up --detach --force-recreate --quiet-pull 2>&1" % (COMPOSE, COMPOSE))
wait(lambda: get("/").endswith(" 200"), "the site after its old compose", timeout=90)
if interval() != "30s":
    fail("old compose: %s" % interval())
sh("docker exec -u 0 %s sh -c 'mkdir -p %s/yuklemeler/2024 && echo bir > %s/yuklemeler/a.txt && echo iki > %s/yuklemeler/2024/b.txt && echo uc > %s/not.txt'"
   % (NAME, HTML, HTML, HTML, HTML))
r = layer()
want = [HTML + "/yuklemeler — 2 dosya", HTML + "/not.txt"]
if r.get("files") != 3 or r.get("paths") != want:
    fail("layer: %s" % r)
ok("katman: %d dosya — %s" % (r["files"], ", ".join(r["paths"])))

# Redeploy and move: refused until said go on.
refused("POST", "/v1/sites/%s/redeploy" % NAME, {"actor": ACTOR}, 3)
refused("POST", "/v1/sites/%s/move" % NAME, {"group": "katman2", "confirm": NAME, "actor": ACTOR}, 3)
ok("yeniden dağıtım ve grup değiştirme 409 ile reddedildi (rapor yanıtta)")

# Caddy's part only (a second domain): no question, the container and its old compose
# file stay.
before = container_id()
two = {"domains": [HOST, "katman2." + DOMAIN], "image": IMAGE, "port": 8080, "memory_mb": 64, "max_body_mb": 0, "data_mount": ""}
steps = job("POST", "/v1/sites/%s/update" % NAME, {"update": two, "actor": ACTOR})
if container_id() != before or interval() != "30s" or "container ve compose.yaml olduğu gibi" not in steps:
    fail("domains only: %s %s\n%s" % (container_id() == before, interval(), steps))
wait(lambda: sh("curl -sk --max-time 20 --resolve katman2.%s:443:127.0.0.1 -o /dev/null -w '%%{http_code}' https://katman2.%s/not.txt" % (DOMAIN, DOMAIN), check=False).strip() == "200",
     "the second domain", timeout=60)
ok("yalnız alan adı değişikliği: sorulmadı, aynı container, compose olduğu gibi; ikinci alan adı yayında")

found = check_finding()
f = found.get("site-layer:" + NAME)
if not f or f["level"] != "warn" or "3 dosya" not in f["title"]:
    fail("server check: %s" % found)
ok("sunucu kontrolü: %s" % f["title"])

# Park, then unpark: the container is started as it is, not made anew from the compose
# file FootOps writes now; the files stay.
before = container_id()
job("POST", "/v1/sites/%s/park" % NAME, {"actor": ACTOR})
steps = job("POST", "/v1/sites/%s/unpark" % NAME, {"expires": "", "actor": ACTOR})
if container_id() != before or interval() != "30s" or "yeniden oluşturulmadan başlatıldı" not in steps or "bir sonraki dağıtım" not in steps:
    fail("unpark: %s %s\n%s" % (container_id() == before, interval(), steps))
if sh("docker exec %s cat %s/yuklemeler/2024/b.txt %s/not.txt" % (NAME, HTML, HTML)).split() != ["iki", "uc"]:
    fail("files after unpark")
wait(lambda: get("/not.txt").endswith(" 200"), "the site after unpark", timeout=60)
ok("parktan yayına alma: aynı container, eski compose ile; dosyalar yerinde")

# An update that makes a persistent folder at yuklemeler: those are not at stake (they
# are taken along), not.txt is.
upd = {"domains": [HOST], "image": IMAGE, "port": 8080, "memory_mb": 64, "max_body_mb": 0, "data_mount": HTML + "/yuklemeler"}
refused("POST", "/v1/sites/%s/update" % NAME, {"update": upd, "actor": ACTOR}, 1)
steps = job("POST", "/v1/sites/%s/update" % NAME, {"update": upd, "actor": ACTOR, "force": True})
if "kalıcı klasöre taşındı: 2 dosya" not in steps or "kopyalanırken durduruldu" not in steps:
    fail("update steps:\n%s" % steps)
data = "/srv/%s/%s/data" % (GROUP, NAME)
if sh("cat %s/a.txt %s/2024/b.txt" % (data, data)).split() != ["bir", "iki"] or sh("test -e %s.copy && echo var || true" % data).strip() or \
        sh("stat -c %%u:%%g %s/2024/b.txt" % data).strip() != "101:101":
    fail("data folder: %s" % sh("ls -laR %s*" % data))
wait(lambda: get("/yuklemeler/2024/b.txt").startswith("iki"), "the moved file through Caddy", timeout=60)
if not get("/not.txt").endswith(" 404") or interval() != "2m0s" or layer().get("files"):
    fail("after the update: %s %s %s" % (get("/not.txt"), interval(), layer()))
ok("kalıcı klasör eklendi: 2 dosya taşındı ve yayında, not.txt (onaylanan) gitti, compose yenilendi")

if "site-layer:" + NAME in check_finding():
    fail("server check after: %s" % check_finding())
steps = job("POST", "/v1/sites/%s/redeploy" % NAME, {"actor": ACTOR})
ok("katmanı temiz site: kontrolde bulgu yok, yeniden dağıtım sormadan geçti")

remove_and_purge()
print("l1: OK")
