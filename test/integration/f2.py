#!/usr/bin/env python3
"""F2a scenarios against a real install (run after f0.py, which seeds the itsite
database): export a FootOps archive, change the database, upload the archive through the
panel in chunks, check it (trial restore), apply it, undo it, add-only mode, a broken
archive, and an agent restart in the middle of an import.

Usage: f2.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time

from itlib import ACTOR, SITE, api, fail, fresh_totp, ok, rss, sh, tools, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
measure = {}
WORK = tempfile.mkdtemp(prefix="footops-f2-")
CHUNK = 8 << 20

uri = re.search(r'MONGODB_URI: "([^"]+)"', sh("cat /srv/musteri/%s/compose.yaml" % SITE)).group(1)
local = uri.replace("@mongo:27017", "@127.0.0.1:27017")
root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)


def site_js(js):
    return sh("docker exec -i footops-mongo mongosh --quiet '%s' --file /dev/stdin" % local, stdin=js.encode())


def counts():
    out = site_js('const o={};db.getCollectionNames().filter(n=>!n.startsWith("system.")).forEach(n=>o[n]=db.getCollection(n).countDocuments({}));print("C "+JSON.stringify(o))')
    return json.loads(out.split("C ", 1)[1])


def temp_dbs():
    out = sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
             stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);'
                    'print("T "+JSON.stringify(c.getDBNames().filter(x=>x.startsWith("footops_tmp_"))));'
                    'print("U "+c.getDB("admin").getCollection("system.users").countDocuments({db:/^footops_tmp_/}))' % json.dumps(root_pw)).encode())
    return json.loads(out.split("T ", 1)[1].split("\n")[0]), int(out.split("U ", 1)[1].split()[0])


def site_running():
    return sh("docker inspect -f '{{.State.Running}}' %s" % SITE).strip() == "true"


def clean_after(what):
    if tools():
        fail("%s: tool container left: %s" % (what, tools()))
    t, u = temp_dbs()
    if t or u:
        fail("%s: temporary databases/users left: %s %d" % (what, t, u))
    if sh("ls -A /var/lib/footops/recovery 2>/dev/null || true").strip():
        fail("%s: recovery record left" % what)
    jobs = [d for d in sh("ls -A /var/lib/footops/staging").split() if not d.startswith(("exp-", "upl-"))]
    if jobs:
        fail("%s: job staging left: %s" % (what, jobs))


ORIGINAL = {"small": 10, "big": 200000, "media": 5}
print("F2 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
if counts() != ORIGINAL:
    fail("unexpected start state (run f0.py first): %s" % counts())
measure["agent_rss_idle"] = rss("footops agent")

# ---- export ----
t0 = time.time()
_, r = api("POST", "/v1/sites/%s/exports" % SITE, {"actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("export: %s\n%s" % (j["error"], "\n".join(j["steps"])))
_, exports = api("GET", "/v1/sites/%s/exports" % SITE)
ex = next(x for x in exports if x["id"] == j["result"])
sh("curl -sS -D /tmp/exp.h --unix-socket /run/footops/agent.sock -o /tmp/exp.tar http://agent/v1/sites/%s/exports/%s/download" % (SITE, ex["id"]))
if sh("sha256sum /tmp/exp.tar").split()[0] != ex["sha256"] or ex["sha256"] not in sh("cat /tmp/exp.h"):
    fail("export checksum")
listing = sh("tar -tvf /tmp/exp.tar")
names = [l.split()[-1] for l in listing.splitlines()]
man = json.loads(sh("tar -xOf /tmp/exp.tar manifest.json"))
if names != ["db.archive.gz", "manifest.json"] or man["collections"] != ORIGINAL or man["documents"] != 200015:
    fail("export contents: %s %s" % (names, man))
if sh("tar -xOf /tmp/exp.tar db.archive.gz | sha256sum").split()[0] != man["members"]["db.archive.gz"]["sha256"]:
    fail("manifest checksum of the dump")
u = j["usage"]
measure.update(export_wall_s=round(time.time() - t0, 2), export_peak_mem=u.get("peak_mem"), export_bytes=ex["bytes"])
ok("dışa aktarma: %s, GNU tar okuyor, manifest 200.015 belge, sha256'lar tutuyor (en çok %.0f MB bellek)" %
   (ex["file"], u.get("peak_mem", 0) / 2**20))
subprocess.run(["docker", "cp", "%s:/tmp/exp.tar" % C, WORK + "/exp.tar"], check=True)
archive = open(WORK + "/exp.tar", "rb").read()

# ---- change the database ----
CHANGED = {"small": 15, "big": 200000, "extra": 2}
site_js('db.media.drop();for(let i=0;i<5;i++)db.small.insertOne({yeni:i});db.extra.insertMany([{a:1},{a:2}]);print("ok")')
if counts() != CHANGED:
    fail("change: %s" % counts())
ok("veritabanı değiştirildi: %s" % CHANGED)

# ---- upload through the panel, in chunks ----
user, pw, secret = open(LOGIN).read().split()
jar = "/tmp/jar2"
sh("rm -f %s" % jar)
page = sh("curl -sS -c %s -b %s http://localhost:7701/login" % (jar, jar))
csrf = re.search(r'name="csrf" value="([^"]+)"', page).group(1)
sh("curl -sS -o /dev/null -c %s -b %s --data-urlencode csrf=%s --data-urlencode username=%s "
   "--data-urlencode password@- --data-urlencode code=%s http://localhost:7701/login" % (jar, jar, csrf, user, fresh_totp(secret)),
   stdin=pw.encode())
page = sh("curl -sS -b %s http://localhost:7701/sites/%s/import" % (jar, SITE))
if "upload.js" not in page:
    fail("import page:\n" + page[:1500])
tok = re.search(r'name="csrf" value="([^"]+)"', page).group(1)


def panel(method, path, body=b"", headers=()):
    hs = " ".join("-H '%s'" % h for h in ("X-CSRF-Token: " + tok, "Origin: http://localhost:7701") + tuple(headers))
    out = sh("curl -sS -w '\\n%%{http_code}' -b %s -X %s %s --data-binary @- http://localhost:7701%s" % (jar, method, hs, path), stdin=body)
    text, code = out.rsplit("\n", 1)
    return int(code), text


web_before = rss("footops web")
t0 = time.time()
code, text = panel("POST", "/sites/%s/uploads" % SITE, json.dumps({"name": ex["file"], "size": len(archive)}).encode(),
                   ("Content-Type: application/json",))
if code != 200:
    fail("create upload: %d %s" % (code, text))
up = json.loads(text)
if up["chunk"] != CHUNK or up["missing"] != list(range((len(archive) + CHUNK - 1) // CHUNK)):
    fail("upload: %s" % up)
for n in up["missing"]:
    part = archive[n * CHUNK:(n + 1) * CHUNK]
    if n == 1:  # one damaged chunk first: refused, then sent again
        code, _ = panel("PUT", "/uploads/%s/%d" % (up["id"], n), part, ("X-Chunk-Sha256: " + "0" * 64,))
        if code != 422:
            fail("damaged chunk: %d" % code)
    code, text = panel("PUT", "/uploads/%s/%d" % (up["id"], n), part, ("X-Chunk-Sha256: " + hashlib.sha256(part).hexdigest(),))
    if code != 200:
        fail("chunk %d: %d %s" % (n, code, text))
code, text = panel("POST", "/uploads/%s/finish" % up["id"], b"{}", ("Content-Type: application/json",))
if code != 200:
    fail("finish: %d %s" % (code, text))
check_job = json.loads(text)["next"].rsplit("/", 1)[1]
measure.update(upload_wall_s=round(time.time() - t0, 2), web_rss_before_upload=web_before, web_rss_after_upload=rss("footops web"))
ok("panelden parçalı yükleme: %d parça, bozuk parça 422 ile reddedildi ve yeniden gönderildi" % len(up["missing"]))

t_check = time.time()
j = wait_job(check_job)
if j.get("error"):
    fail("check: %s\n%s" % (j["error"], "\n".join(j["steps"])))
_, ic = api("GET", "/v1/sites/%s/imports/%s" % (SITE, up["id"]))
if ic["format"] != "footops" or ic["archive"] != ORIGINAL or ic["current"] != CHANGED or ic["section"]["offset"] != 512:
    fail("check result: %s" % {k: ic[k] for k in ("format", "archive", "current", "section")})
if counts() != CHANGED:
    fail("the check changed the site's database")
clean_after("check")
measure.update(check_peak_mem=j["usage"].get("peak_mem"), check_wall_s=round(time.time() - t_check, 2))
ok("kontrol: deneme yüklemesi 200.015 belge saydı; site veritabanına dokunulmadı; geçici DB/kullanıcı ve araç container'ı silindi")
page = sh("curl -sS -b %s http://localhost:7701/sites/%s/import/%s" % (jar, SITE, up["id"]))
if "İçe aktarma önizlemesi" not in page or "200000" not in page or "yalnız sitede" not in page:
    fail("preview page:\n" + page[:3000])
ok("önizleme sayfası: sayımlar ve 'yalnız sitede' notu görünüyor")

# ---- apply (replace) ----
code, text = panel("POST", "/sites/%s/import/%s/apply" % (SITE, up["id"]), b"mode=replace&confirm=yanlis",
                   ("Content-Type: application/x-www-form-urlencoded",))
if code != 422:
    fail("wrong confirmation: %d" % code)
_, r = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, up["id"]), {"options": {"db": True, "db_mode": "replace"}, "confirm": SITE, "actor": ACTOR})
code, _ = api("POST", "/v1/sites/%s/exports" % SITE, {"actor": ACTOR}, check=False)
if code != 409:
    fail("a second job started during the import: %d" % code)
t0 = time.time()
j = wait_job(r["job_id"])
if j.get("error"):
    fail("apply: %s\n%s" % (j["error"], "\n".join(j["steps"])))
measure.update(apply_wall_s=round(time.time() - t0, 2), apply_peak_mem=j["usage"].get("peak_mem"), apply_cpu_ms=j["usage"].get("cpu", 0) // 1_000_000)
print("    " + "\n    ".join(j["steps"]))
_, audit = api("GET", "/v1/audit?limit=10")
a = next(a for a in audit if a["action"] == "import.apply")
if counts() != ORIGINAL or not site_running():
    fail("after apply: %s running=%s" % (counts(), site_running()))
snap_before = re.search(r"geri alma: (\d{8}-\d{6})", a["detail"]).group(1)
clean_after("apply")
code, _ = api("GET", "/v1/sites/%s/imports/%s" % (SITE, up["id"]), check=False)
if code != 404:
    fail("applied upload kept")
ok("uygula (tamamen değiştir): 200.015 belge geri geldi, 'extra' silindi, site çalışıyor; güvenlik kopyası %s" % snap_before)

# ---- undo: restore the safety copy ----
_, r = api("POST", "/v1/sites/%s/snapshots/%s/restore" % (SITE, snap_before), {"confirm": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error") or counts() != CHANGED or not site_running():
    fail("undo: %s %s\n%s" % (j.get("error"), counts(), "\n".join(j["steps"])))
measure.update(restore_peak_mem=j["usage"].get("peak_mem"))
clean_after("undo")
ok("geri al: kopya geri yüklendi, veritabanı içe aktarmadan önceki halinde (%s)" % CHANGED)

# ---- add only, via the single-request upload ----
out = sh("curl -sS --unix-socket /run/footops/agent.sock -H 'Content-Type: application/octet-stream' --data-binary @/tmp/exp.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=tek.footops.tar'" % SITE)
single = json.loads(out)
_, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": single["id"], "actor": ACTOR})
j = wait_job(r["job_id"])
if j.get("error"):
    fail("check (add): %s" % j["error"])
_, r = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, single["id"]), {"options": {"db": True, "db_mode": "add"}, "confirm": SITE, "actor": ACTOR})
j = wait_job(r["job_id"])
want = {"small": 15, "big": 200000, "extra": 2, "media": 5}
if j.get("error") or counts() != want:
    fail("add: %s %s\n%s" % (j.get("error"), counts(), "\n".join(j["steps"])))
clean_after("add")
ok("yalnız ekle: eksik 'media' eklendi, var olan koleksiyonlara dokunulmadı (%s)" % want)

# ---- a broken archive is refused before anything is written ----
sh("head -c %d /tmp/exp.tar > /tmp/broken.tar" % (len(archive) // 2))
out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @/tmp/broken.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=bozuk.footops.tar'" % SITE)
broken = json.loads(out)
_, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": broken["id"], "actor": ACTOR})
j = wait_job(r["job_id"])
if not j.get("error") or counts() != want:
    fail("broken archive accepted: %s" % j)
code, _ = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, broken["id"]), {"options": {"db": True, "db_mode": "replace"}, "confirm": SITE, "actor": ACTOR}, check=False)
if code != 404:
    fail("apply of an unchecked upload: %d" % code)
clean_after("broken")
ok("yarım arşiv kontrolde reddedildi (%s); uygulanamaz, veri değişmedi" % j["error"][:70])

# ---- agent restart in the middle of an import ----
out = sh("curl -sS --unix-socket /run/footops/agent.sock --data-binary @/tmp/exp.tar "
         "'http://agent/v1/sites/%s/uploads/single?name=yeniden.footops.tar'" % SITE)
again = json.loads(out)
_, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": again["id"], "actor": ACTOR})
wait_job(r["job_id"])
before_restart = counts()
_, audit = api("GET", "/v1/audit?limit=1000")
recovers_before = len([a for a in audit if a["action"] == "db.recover"])
_, r = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, again["id"]), {"options": {"db": True, "db_mode": "replace"}, "confirm": SITE, "actor": ACTOR})
jid = r["job_id"]


# The restore takes under two seconds here, so the watch runs inside the container: as soon
# as mongorestore is the job's last step, wait a moment and restart the agent.
out = sh("for i in $(seq 1 1200); do curl -s --unix-socket /run/footops/agent.sock http://agent/v1/jobs/%s "
         "| grep -qF 'mongorestore çalışıyor (sınır: 512 MB bellek, %%50 CPU)\"]' "
         "&& { sleep 0.4; systemctl restart footops-agent; echo restarted; break; }; sleep 0.05; done" % jid)
if "restarted" not in out:
    fail("never saw the restore running")
wait(lambda: sh("curl -s -o /dev/null -w %{http_code} --unix-socket /run/footops/agent.sock http://agent/v1/overview",
                check=False).strip() == "200", "agent after restart")
_, j = api("GET", "/v1/jobs/" + jid)
if not j.get("interrupted"):
    fail("import not interrupted: %s" % j)


def recovered():
    _, audit = api("GET", "/v1/audit?limit=1000")
    return [a for a in audit if a["action"] == "db.recover"]


wait(lambda: len(recovered()) > recovers_before, "the recovery job", 300)
rec = recovered()[0]  # newest first
after = counts()
if not rec["ok"] or after not in (before_restart, ORIGINAL) or not site_running():
    fail("recovery: %s; before %s, after %s, running %s" % (rec, before_restart, after, site_running()))
clean_after("recovery")
ok("içe aktarma ortasında agent yeniden başladı: kurtarma işi veriyi %s haline getirdi (%s), site çalışıyor" %
   ("eski" if after == before_restart else "yeni", after))

measure["agent_rss_after"] = rss("footops agent")
sh("rm -f /tmp/exp.tar /tmp/exp.h /tmp/broken.tar %s" % jar)
os.remove(WORK + "/exp.tar")
os.rmdir(WORK)
print("ÖLÇÜMLER " + json.dumps(measure))
print("F2: tüm senaryolar geçti")
