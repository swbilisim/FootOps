#!/usr/bin/env python3
"""F2b scenarios (files) against a real install, after f0.py: a site whose data directory
is nginx's web root (an image that runs as uid 101), so imported files are served for
real. Import a zip (top folder stripped, hidden files dropped), check ownership and modes
(the helper ran as the site's user), export, add-only import, replace and undo.

Usage: f2b.py <container> <login-file>
"""
import io
import json
import os
import random
import sys
import time
import zipfile

from itlib import ACTOR, api, fail, ok, rss, sh, tools, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itfiles", "files.it.example.com"  # covered by f0's *.it.example.com certificate
DATA = "/srv/musteri/%s/data" % SITE
measure = {}


def serve(path):
    return sh("curl -sk --resolve %s:443:127.0.0.1 https://%s%s" % (DOMAIN, DOMAIN, path), check=False)


def tree():
    out = sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%s %%P\\n' | sort" % DATA)
    return [l.split(" ", 4) for l in out.splitlines()]


def upload_single(name, data):
    path = "/tmp/" + name
    sh("cat > %s" % path, stdin=data)
    out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @%s 'http://agent/v1/sites/%s/uploads/single?name=%s'"
             % (path, SITE, name))
    sh("rm -f %s" % path)
    return json.loads(out)["id"]


def check(upload):
    _, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": upload, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("check: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    _, ic = api("GET", "/v1/sites/%s/imports/%s" % (SITE, upload))
    return ic, j


def apply(upload, opts):
    _, r = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, upload), {"options": opts, "confirm": SITE, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("apply: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    return j


def clean(what):
    if tools():
        fail("%s: tool container left" % what)
    if sh("systemctl list-units --all --no-legend 'footops-files-*' 2>/dev/null").strip():
        fail("%s: helper scope left: %s" % (what, sh("systemctl list-units --all --no-legend 'footops-files-*'")))
    if sh("ls -A /var/lib/footops/recovery 2>/dev/null || true").strip() not in ("", "temp-databases"):
        fail("%s: recovery record left" % what)
    if sh("test -e /srv/musteri/%s/data.new && echo y || true" % SITE).strip():
        fail("%s: data.new left" % what)


print("F2b senaryoları")
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                              "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                              "data_mount": "/usr/share/nginx/html"}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    ok("dosyalı deneme sitesi (nginx, uid 101) oluşturuldu")
# A fresh start: the site runs, no copies from earlier runs.
sh("rm -rf /srv/musteri/%s/data.old-* /srv/musteri/%s/data.failed-*; docker start %s >/dev/null" % (SITE, SITE, SITE))
owner = sh("stat -c %%u:%%g %s" % DATA).strip()
if owner != "101:101":
    fail("data owner %s" % owner)
sh("find %s -mindepth 1 -delete" % DATA)

# ---- import a zip: top folder stripped, hidden files dropped ----
rnd = random.Random(7)
photos = {"site/img/foto%02d.jpg" % i: bytes(rnd.getrandbits(8) for _ in range(200_000)) for i in range(40)}
files = {"site/index.html": "<h1>Merhaba dünya — içe aktarıldı</h1>".encode(), "site/ürünler/çay.html": "çay".encode()}
files.update(photos)
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w") as z:
    for n, b in files.items():
        z.writestr(n, b)
    z.writestr("__MACOSX/site/._index.html", b"x")
    z.writestr("site/.DS_Store", b"x")
    z.writestr("site/.htaccess", b"deny from all")
up = upload_single("site.zip", buf.getvalue())
ic, _ = check(up)
fc = ic["files"]
if ic["has_db"] or fc["format"] != "zip" or fc["archive"]["prefix"] != "site" or not fc["strip_default"] or fc["archive"]["hidden"] != 3:
    fail("zip check: %s" % fc)
t0 = time.time()
j = apply(up, {"files": True, "files_mode": "replace", "strip": True})
measure.update(import_zip_wall_s=round(time.time() - t0, 2), import_zip_peak=j["usage"].get("peak_mem"))
print("    " + "\n    ".join(j["steps"]))
if "Site sağlıklı" not in "\n".join(j["steps"]) or sh("docker inspect -f '{{.State.Running}}' %s" % SITE).strip() != "true":
    fail("site not started and checked")
if "Merhaba dünya" not in serve("/"):
    fail("imported index.html not served: %s" % serve("/")[:200])
entries = tree()
names = sorted(e[4] for e in entries)
want = sorted(["img", "index.html", "ürünler", "ürünler/çay.html"] + ["img/foto%02d.jpg" % i for i in range(40)])
if names != want:
    fail("tree: %s" % names)
for typ, own, mode, size, name in entries:
    if own != "101:101" or (typ == "f" and mode != "644") or (typ == "d" and mode != "755"):
        fail("ownership/mode of %s: %s %s" % (name, own, mode))
clean("zip import")
ok("zip içe aktarıldı: site/ klasörü atıldı, 3 gizli dosya alınmadı, %d dosya sitenin kullanıcısına (101) ait, 644/755; nginx yeni index.html'i sunuyor" % (len(names) - 3))

# ---- export: files only ----
t0 = time.time()
_, r = api("POST", "/v1/sites/%s/exports" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("export: %s\n%s" % (j["error"], "\n".join(j["steps"])))
measure.update(export_wall_s=round(time.time() - t0, 2), export_peak=j["usage"].get("peak_mem"))
_, exports = api("GET", "/v1/sites/%s/exports" % SITE)
ex = next(x for x in exports if x["id"] == j["result"])
sh("curl -sS --unix-socket /run/footops/agent.sock -o /tmp/f.tar http://agent/v1/sites/%s/exports/%s/download" % (SITE, ex["id"]))
members = sh("tar -tf /tmp/f.tar").split()
inner = sh("tar -xOf /tmp/f.tar files.tar.gz | tar -tz").split()
man = json.loads(sh("tar -xOf /tmp/f.tar manifest.json"))
if members != ["files.tar.gz", "manifest.json"] or "index.html" not in inner or man["files"]["files"] != 42 or ex["files"] != 42:
    fail("export: %s %s %s" % (members, inner[:5], man.get("files")))
ok("dışa aktarma (yalnız dosyalar): files.tar.gz + manifest, 42 dosya, GNU tar okuyor")

# ---- add only: the archive's files over the current ones ----
sh("echo yerel > %s/yerel.txt && chown 101:101 %s/yerel.txt && echo değişti > %s/index.html" % (DATA, DATA, DATA))
sh("cp /tmp/f.tar /tmp/geri.footops.tar")
out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @/tmp/geri.footops.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=geri.footops.tar'" % SITE)
up = json.loads(out)["id"]
ic, _ = check(up)
if ic["format"] != "footops" or ic["files"]["strip_default"] or ic["files"]["current"]["files"] != 43:
    fail("footops check: %s" % ic["files"])
apply(up, {"files": True, "files_mode": "add"})
if "Merhaba dünya" not in serve("/") or sh("cat %s/yerel.txt" % DATA).strip() != "yerel":
    fail("add: index %s, yerel %s" % (serve("/")[:80], sh("cat %s/yerel.txt" % DATA, check=False)))
# The kept file is a hard link shared with the previous directory (data.old-*): no copy.
if sh("stat -c %%h %s/yerel.txt" % DATA).strip() != "2":
    fail("yerel.txt was copied, not linked")
clean("add")
ok("ekle ve üzerine yaz: arşivdeki index.html geri geldi, yerel.txt kaldı (kopyalanmadan, önceki klasörle ortak bağlantı)")

# ---- replace, then undo from the files backup ----
out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @/tmp/geri.footops.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=geri2.footops.tar'" % SITE)
up = json.loads(out)["id"]
check(up)
j = apply(up, {"files": True, "files_mode": "replace"})
if sh("test -e %s/yerel.txt && echo y || true" % DATA).strip():
    fail("replace kept yerel.txt")
old = next(p for p in j["result"].split() if p.startswith("files-"))[len("files-"):]
_, backups = api("GET", "/v1/sites/%s/files/backups" % SITE)
if not any(b["id"] == old for b in backups):
    fail("backup %s not listed: %s" % (old, backups))
_, r = api("POST", "/v1/sites/%s/files/backups/%s/restore" % (SITE, old), {"confirm": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error") or sh("cat %s/yerel.txt" % DATA, check=False).strip() != "yerel":
    fail("files restore: %s\n%s" % (j.get("error"), "\n".join(j["steps"])))
clean("restore")
ok("tamamen değiştir → yerel.txt gitti; dosya kopyası %s geri yüklendi → yerel.txt geri geldi" % old)

# ---- a hostile archive: refused at the check, nothing written ----
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w") as z:
    z.writestr("../../etc/kaçış", b"x")
up = upload_single("kotu.zip", buf.getvalue())
_, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": up, "actor": ACTOR})
j = wait_job(r["job_id"])
if not j.get("error") or sh("test -e /etc/kaçış && echo y || true").strip():
    fail("hostile zip: %s" % j)
ok("'../' içeren zip kontrolde reddedildi (%s)" % j["error"][:60])

measure["agent_rss_after"] = rss("footops agent")
sh("rm -f /tmp/f.tar /tmp/geri.footops.tar")
print("ÖLÇÜMLER " + json.dumps(measure))
print("F2b: tüm senaryolar geçti")
