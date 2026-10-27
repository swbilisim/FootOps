#!/usr/bin/env python3
"""F3 scenarios (file manager) against a real install, after f2b.py: the nginx site whose
data directory is its web root (uid 101). Folders, chunked upload put into a folder (a
link, no copy), a taken name, overwrite (the old file to the trash), rename, move, trash
and restore (nginx stops and starts serving), download with ranges (the helper hands
the agent an open file), zip of a folder, opening a zip that is in the folder, links
never followed, the trash out of the app's sight, emptying it.

Usage: f3.py <container> <login-file>
"""
import base64
import hashlib
import io
import json
import random
import sys
import time
import zipfile

from itlib import ACTOR, api, fail, ok, rss, sh, tools, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itfiles", "files.it.example.com"
BASE = "/srv/musteri/%s" % SITE
DATA, TRASH = BASE + "/data", BASE + "/trash"
measure = {}


def serve(path):
    return sh("curl -sk -o /dev/null -w '%%{http_code}' --resolve %s:443:127.0.0.1 'https://%s%s'" % (DOMAIN, DOMAIN, path), check=False).strip()


def body(path):
    return sh("curl -sk --resolve %s:443:127.0.0.1 'https://%s%s'" % (DOMAIN, DOMAIN, path), check=False)


def q(s):
    return s.replace("'", "'\\''")


def files(d=""):
    _, l = api("GET", "/v1/sites/%s/files?dir=%s" % (SITE, d))
    return l


def change(op, **kw):
    kw.update(op=op, actor=ACTOR)
    return api("POST", "/v1/sites/%s/files/change" % SITE, kw, check=False)


def upload(name, data):
    """A chunked upload through the agent, as the panel sends it."""
    _, st = api("POST", "/v1/sites/%s/uploads" % SITE, {"name": name, "size": len(data), "actor": ACTOR})
    chunk = st["chunk"]
    for n in range((len(data) + chunk - 1) // chunk):
        part = data[n * chunk:(n + 1) * chunk]
        sh("cat > /tmp/part", stdin=part)
        out = sh("curl -sS -o /dev/null -w '%%{http_code}' -X PUT -H 'X-Chunk-Sha256: %s' --data-binary @/tmp/part "
                 "--unix-socket /run/footops/agent.sock http://agent/v1/uploads/%s/chunks/%d" % (hashlib.sha256(part).hexdigest(), st["id"], n))
        if out.strip() != "200":
            fail("chunk %d: %s" % (n, out))
    sh("rm -f /tmp/part")
    api("POST", "/v1/uploads/%s/finish" % st["id"], {"actor": ACTOR})
    return st["id"]


def place(upload_id, d, overwrite=False):
    return api("POST", "/v1/sites/%s/files/place" % SITE, {"upload": upload_id, "dir": d, "overwrite": overwrite, "actor": ACTOR}, check=False)


def blob(path):
    """A binary file of the container (sh decodes text)."""
    return base64.b64decode(sh("base64 -w0 '%s'" % q(path)))


def status(path):
    """The agent's status code for a GET, without reading the body."""
    return int(sh("curl -sS -o /dev/null -w '%%{http_code}' --unix-socket /run/footops/agent.sock 'http://agent%s'" % path))


def stat(p):
    return sh("stat -c '%%u:%%g %%a %%h %%s' '%s'" % q(p)).split()


def clean(what):
    if tools():
        fail("%s: tool container left" % what)
    if sh("systemctl list-units --all --no-legend 'footops-files-*' 2>/dev/null").strip():
        fail("%s: helper scope left" % what)
    left = sh("find %s %s -name '.footops-*' -o -name '.unpack-*' -o -name '.upload-*' 2>/dev/null || true" % (DATA, TRASH)).strip()
    if left:
        fail("%s: work files left: %s" % (what, left))


print("F3 senaryoları")
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    fail("f2b's site %s is missing (run f2b.py first)" % SITE)
sh("docker start %s >/dev/null; find %s -mindepth 1 -delete; rm -rf %s" % (SITE, DATA, TRASH))

# ---- folders, an upload put into a folder (linked, the site user's) ----
for name, d in (("fotolar", ""), ("2026", "fotolar")):
    code, r = change("mkdir", dir=d, name=name)
    if code != 200:
        fail("mkdir %s: %s" % (name, r))
if change("mkdir", dir="", name="fotolar")[0] != 409:
    fail("mkdir twice accepted")
rnd = random.Random(3)
photo = bytes(rnd.getrandbits(8) for _ in range(5_000_000))
t0 = time.time()
up = upload("çiçek bahçesi.jpg", photo)
measure["upload_5mb_s"] = round(time.time() - t0, 2)
t0 = time.time()
code, p = place(up, "fotolar")
measure["place_ms"] = round((time.time() - t0) * 1000)
if code != 200 or p["name"] != "çiçek bahçesi.jpg" or not p["linked"]:
    fail("place: %s %s" % (code, p))
own, mode, links, size = stat(DATA + "/fotolar/çiçek bahçesi.jpg")
if own != "101:101" or mode != "644" or links != "1" or int(size) != len(photo):
    fail("placed file: %s %s %s %s" % (own, mode, links, size))
if serve("/fotolar/%C3%A7i%C3%A7ek%20bah%C3%A7esi.jpg") != "200":
    fail("nginx does not serve the upload")
if sh("test -e /var/lib/footops/staging/upl-%s && echo y || true" % up).strip():
    fail("the placed upload is still in staging")
ok("klasörler oluşturuldu; 5 MB parçalı yükleme klasöre bağlantıyla kondu (kopyasız, %d ms), sahibi 101, 644, nginx sunuyor" % measure["place_ms"])

# ---- a taken name, overwrite ----
code, p = place(upload("çiçek bahçesi.jpg", b"ikinci"), "fotolar")
if code != 200 or p["name"] != "çiçek bahçesi (1).jpg":
    fail("taken name: %s" % p)
code, p = place(upload("çiçek bahçesi.jpg", b"yeni hali"), "fotolar", overwrite=True)
if code != 200 or p["name"] != "çiçek bahçesi.jpg" or not p.get("trashed"):
    fail("overwrite: %s" % p)
if body("/fotolar/%C3%A7i%C3%A7ek%20bah%C3%A7esi.jpg") != "yeni hali":
    fail("overwritten file not served")
_, trash = api("GET", "/v1/sites/%s/trash" % SITE)
if len(trash) != 1 or trash[0]["reason"] != "overwrite" or trash[0]["bytes"] != len(photo):
    fail("trash after overwrite: %s" % trash)
ok("aynı ad: yenisi “çiçek bahçesi (1).jpg”; üzerine yazma: yenisi yerinde, eskisi (5 MB) çöp kutusunda")

# ---- the trash is outside the app's view ----
own, mode, _, _ = stat(TRASH)
if own != "101:101" or mode != "700":
    fail("trash folder: %s %s" % (own, mode))
mounts = sh("docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' %s" % SITE).split()
if mounts != [DATA]:
    fail("site mounts: %s" % mounts)
ok("çöp kutusu %s (101, 700), site container'ı yalnız data/'yı görüyor" % TRASH)

# ---- rename, move, trash a folder, restore ----
if change("rename", dir="fotolar", name="çiçek bahçesi (1).jpg", to="ikinci.jpg")[0] != 200:
    fail("rename")
if change("rename", dir="fotolar", name="ikinci.jpg", to="çiçek bahçesi.jpg")[0] != 409:
    fail("rename onto a taken name accepted")
if change("move", dir="fotolar", names=["ikinci.jpg"], to="fotolar/2026")[0] != 200:
    fail("move")
if change("move", dir="", names=["fotolar"], to="fotolar/2026")[0] != 422:
    fail("move into itself accepted")
if serve("/fotolar/2026/ikinci.jpg") != "200":
    fail("moved file not served")
code, r = change("trash", dir="", names=["fotolar"])
if code != 200 or r["entry"]["files"] != 2:
    fail("trash: %s" % r)
if serve("/fotolar/2026/ikinci.jpg") != "404" or files()["total"] != 0:
    fail("trashed folder still served")
code, res = api("POST", "/v1/sites/%s/trash/%s/restore" % (SITE, r["entry"]["id"]), {"actor": ACTOR})
if code != 200 or serve("/fotolar/2026/ikinci.jpg") != "200":
    fail("restore: %s" % res)
ok("ad değiştir, taşı (çakışma ve kendi içine taşıma reddedildi), klasörü çöpe at (nginx 404) → geri al (nginx 200)")

# ---- download with ranges, zip ----
sh("head -c 3000 /dev/zero | tr '\\0' x > %s/fotolar/big.txt && chown 101:101 %s/fotolar/big.txt" % (DATA, DATA))
dl = sh("curl -sS -o /tmp/dl -w '%%{http_code}' --unix-socket /run/footops/agent.sock "
        "'http://agent/v1/sites/%s/files/download?path=fotolar/big.txt'" % SITE)
if dl.strip() != "200" or sh("wc -c < /tmp/dl").strip() != "3000":
    fail("download: %s" % dl)
dl = sh("curl -sS -r 1000-1999 -o /tmp/dl -w '%%{http_code}' --unix-socket /run/footops/agent.sock "
        "'http://agent/v1/sites/%s/files/download?path=fotolar/big.txt'" % SITE)
if dl.strip() != "206" or sh("wc -c < /tmp/dl").strip() != "1000":
    fail("range download: %s" % dl)
t0 = time.time()
sh("curl -sS -o /tmp/f.zip -X POST -H 'Content-Type: application/json' --data-binary '%s' --unix-socket /run/footops/agent.sock "
   "http://agent/v1/sites/%s/files/zip" % (json.dumps({"dir": "", "names": ["fotolar"], "actor": ACTOR}), SITE))
measure["zip_ms"] = round((time.time() - t0) * 1000)
z = zipfile.ZipFile(io.BytesIO(blob("/tmp/f.zip")))
names = sorted(z.namelist())
if names != ["fotolar/", "fotolar/2026/", "fotolar/2026/ikinci.jpg", "fotolar/big.txt", "fotolar/çiçek bahçesi.jpg"] or z.testzip():
    fail("zip: %s" % names)
# A file the site's user may not read (root's, 0600) is left out and listed in the zip.
sh("echo gizli > %s/fotolar/kok.txt && chmod 600 %s/fotolar/kok.txt" % (DATA, DATA))
sh("curl -sS -o /tmp/f.zip -X POST -H 'Content-Type: application/json' --data-binary '%s' --unix-socket /run/footops/agent.sock "
   "http://agent/v1/sites/%s/files/zip" % (json.dumps({"dir": "fotolar", "actor": ACTOR}), SITE))
z = zipfile.ZipFile(io.BytesIO(blob("/tmp/f.zip")))
if "FOOTOPS-OKUNAMAYANLAR.txt" not in z.namelist() or "kok.txt" not in z.read("FOOTOPS-OKUNAMAYANLAR.txt").decode() or "kok.txt" in z.namelist():
    fail("unreadable file in zip: %s" % z.namelist())
sh("rm %s/fotolar/kok.txt" % DATA)
ok("indirme kaldığı yerden (Range 206, dosyayı yardımcı açıp agent'a veriyor); klasör zip'i (%d ms) bütün ve doğru; okunamayan dosya zip'te listelendi" % measure["zip_ms"])

# ---- open a zip that is in the folder ----
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w") as zf:
    for i in range(30):
        zf.writestr("galeri/resim%02d.jpg" % i, bytes(rnd.getrandbits(8) for _ in range(100_000)))
    zf.writestr("galeri/.DS_Store", b"x")
    zf.writestr("oku.txt", "merhaba")
pkg = buf.getvalue()
code, p = place(upload("paket.zip", pkg), "fotolar")
_, r = api("POST", "/v1/sites/%s/files/unpack" % SITE, {"path": "fotolar/paket.zip", "trash_archive": True, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("unpack: %s\n%s" % (j["error"], "\n".join(j["steps"])))
print("    " + "\n    ".join(j["steps"]))
measure.update(unpack_peak=j["usage"].get("peak_mem"), unpack_s=round(j["usage"].get("wall", 0) / 1e9, 2))
own, mode, _, _ = stat(DATA + "/fotolar/galeri/resim07.jpg")
if own != "101:101" or mode != "644" or serve("/fotolar/galeri/resim07.jpg") != "200" or serve("/fotolar/oku.txt") != "200":
    fail("unpacked files: %s %s" % (own, mode))
if sh("test -e %s/fotolar/paket.zip && echo y || true" % DATA).strip() or sh("test -e %s/fotolar/galeri/.DS_Store && echo y || true" % DATA).strip():
    fail("archive not trashed or hidden file extracted")
# Again: everything is taken now, nothing moves.
code, p = place(upload("paket.zip", pkg), "fotolar")
_, r = api("POST", "/v1/sites/%s/files/unpack" % SITE, {"path": "fotolar/paket.zip", "actor": ACTOR})
j = wait_job(r["job_id"])
if "zaten var" not in (j.get("error") or ""):
    fail("conflicting unpack: %s" % j)
clean("unpack")
ok("klasördeki zip açıldı (30 dosya, sahibi 101, .DS_Store alınmadı, arşiv çöpe); aynı zip ikinci kez: çakışma, hiçbir şey açılmadı")

# ---- links are never followed ----
sh("ln -s /etc %s/dis && docker exec %s ln -s /etc/passwd /usr/share/nginx/html/pw" % (DATA, SITE))
for path, what in (("/v1/sites/%s/files?dir=dis" % SITE, "list through a link"),
                   ("/v1/sites/%s/files/download?path=dis/passwd" % SITE, "download through a link"),
                   ("/v1/sites/%s/files/download?path=pw" % SITE, "download a link")):
    if status(path) // 100 == 2:
        fail(what + " worked")
if change("move", dir="fotolar", names=["big.txt"], to="dis")[0] // 100 == 2 or sh("test -e /etc/big.txt && echo y || true").strip():
    fail("move into a link worked")
l = files()
kinds = {e["name"]: e["type"] for e in l["entries"]}
if kinds.get("dis") != "link" or kinds.get("pw") != "link":
    fail("links listed as %s" % kinds)
sh("rm %s/dis %s/pw" % (DATA, DATA))
ok("bağlantılar izlenmiyor: dis → /etc ve uygulamanın koyduğu pw → /etc/passwd listelendi ama açılamadı, taşınamadı")

# ---- empty the trash ----
if api("POST", "/v1/sites/%s/trash/empty" % SITE, {"confirm": "yanlis", "actor": ACTOR}, check=False)[0] != 422:
    fail("empty without the name")
_, before = api("GET", "/v1/sites/%s/trash" % SITE)
code, res = api("POST", "/v1/sites/%s/trash/empty" % SITE, {"confirm": SITE, "actor": ACTOR})
if res["entries"] != len(before) or sh("ls -A %s" % TRASH).strip():
    fail("empty: %s, left %s" % (res, sh("ls -A %s" % TRASH)))
ok("çöp kutusu site adıyla boşaltıldı (%d kayıt)" % res["entries"])

# ---- cost: a listing is one short helper run, no unit ----
times = []
for _ in range(20):
    t = sh("curl -sS -o /dev/null -w '%%{time_total}' --unix-socket /run/footops/agent.sock 'http://agent/v1/sites/%s/files?dir=fotolar/galeri'" % SITE)
    times.append(float(t))
times.sort()
measure.update(list_ms_median=round(times[10] * 1000, 1), list_ms_max=round(times[-1] * 1000, 1))
audit = sh("tail -n 300 /var/lib/footops/audit.jsonl")
for action in ("files.mkdir", "files.upload", "files.rename", "files.move", "files.trash", "files.untrash", "files.download", "files.zip",
               "files.unpack", "files.empty-trash"):
    if '"action":"%s"' % action not in audit:
        fail("audit lacks %s" % action)
measure["agent_rss_after"] = rss("footops agent")
clean("end")
sh("rm -f /tmp/dl /tmp/f.zip")
print("ÖLÇÜMLER " + json.dumps(measure))
print("F3: tüm senaryolar geçti")
