#!/usr/bin/env python3
"""M4 scenarios (archives between the Mac and a panel), after y3.py (whose site, itbak — a
database and files of the app's own user — it uses and leaves for y3 to make anew): on
this Mac, footops-mac makes a FootOps archive of a database of the Mac's MongoDB and a
folder; the panel's import checks it (a trial restore) and applies it, and the site's
database and files are then the archive's, owned by the app's user. The other way, the
panel's export of the site is checked, opened into a folder and loaded into the Mac's
MongoDB by footops-mac, counted as the site holds it.

Needs on this Mac: MongoDB (MONGO_URI, default mongodb://127.0.0.1:27017) and its
database tools (mongodump, mongorestore). footops-mac runs with a home of its own (its
list of what was opened, its Downloads); the Mac's MongoDB gets two databases named
m4test_<random>_arsiv_…, removed at the end (it says so when one cannot be). Not in
run.sh's default list: run.sh m4.

Usage: m4.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time

from itlib import ACTOR, api, fail, ok, sh, wait_job

C = sys.argv[1]
SITE = "itbak"
DATA = "/srv/musteri/%s/data" % SITE
ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
URI = os.environ.get("MONGO_URI", "mongodb://127.0.0.1:27017")
WORK = tempfile.mkdtemp(prefix="footops-m4-")
HOME = os.path.join(WORK, "home")
ID = os.urandom(3).hex()
SOURCE = "m4test_%s_arsiv_20000101_0000" % ID  # of a default name: footops-mac deletes it at the end
BACK = "m4test_%s_arsiv_geri" % ID
measure = {}

root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)


def root_js(js):
    return sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
              stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);' % json.dumps(root_pw) + js).encode())


def counts(db=SITE):
    out = root_js('const d=c.getDB("%s");const o={};d.getCollectionInfos().filter(i=>!i.name.startsWith("system."))'
                  '.forEach(i=>o[i.name]=i.type=="view"?"view":d.getCollection(i.name).countDocuments({}));print("C "+JSON.stringify(o));' % db)
    return json.loads(out.split("C ", 1)[1])


def mac(*args):
    """footops-mac on this Mac, in a home of its own."""
    p = subprocess.run([os.path.join(WORK, "footops-mac"), *args], capture_output=True,
                       env=dict(os.environ, HOME=HOME, MONGO_URI=URI))
    return p.returncode, p.stdout.decode(), p.stderr.decode()


def result(out):
    """A long command's last JSON line."""
    return json.loads(out.strip().splitlines()[-1])


def local_tree(path):
    """Relative path → SHA-256 of the regular files below path that an import takes."""
    out = {}
    for d, dirs, files in os.walk(path):
        dirs[:] = [x for x in dirs if not x.startswith(".")]
        for f in files:
            p = os.path.join(d, f)
            if f.startswith(".") or os.path.islink(p):
                continue
            out[os.path.relpath(p, path)] = hashlib.sha256(open(p, "rb").read()).hexdigest()
    return out


def cleanup():
    for name in (SOURCE, BACK):
        code, out, err = mac("arsiv", "sil", name)
        if code != 0:
            print("  ⚠ %s silinemedi: %s" % (name, (out + err).strip()))
    sh("rm -f /tmp/m4-mac.tar /tmp/m4-exp.tar", check=False)
    shutil.rmtree(WORK, ignore_errors=True)


print("M4 senaryoları (Mac ↔ panel arşivleri)")
for tool in ("mongodump", "mongorestore", "go"):
    if not shutil.which(tool):
        fail("bu Mac'te %s yok" % tool)
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    fail("%s yok: önce y3.py" % SITE)
os.makedirs(HOME, mode=0o700)
subprocess.run(["go", "build", "-o", os.path.join(WORK, "footops-mac"), "./cmd/footops-mac"], cwd=ROOT, check=True)
try:
    # ---- on the Mac: a database and a folder ----
    subprocess.run(["mongorestore", "--uri=" + URI, "--archive=" + os.path.join(ROOT, "internal/archive/testdata/magaza.archive"),
                    "--nsInclude=magaza.*", "--nsFrom=magaza.*", "--nsTo=%s.*" % SOURCE, "--quiet"], check=True)
    folder = os.path.join(WORK, "site")
    os.makedirs(os.path.join(folder, "img"))
    os.makedirs(os.path.join(folder, "belge"))
    with open(os.path.join(folder, "img", "foto1.jpg"), "wb") as f:
        f.write(os.urandom(40000))
    with open(os.path.join(folder, "belge", "çay ve şeker.txt"), "w") as f:
        f.write("merhaba dünya\n")
    with open(os.path.join(folder, "index.html"), "w") as f:
        f.write("<h1>Mac'ten</h1>")
    with open(os.path.join(folder, ".env.local"), "w") as f:
        f.write("GIZLI=1\n")
    os.symlink("index.html", os.path.join(folder, "kisayol"))
    want_files = local_tree(folder)

    t0 = time.time()
    code, out, err = mac("arsiv", "olustur", "--db", SOURCE, "--dosyalar", folder, "--site", SITE,
                         "--hedef", os.path.join(WORK, "mac.footops.tar"), "--json")
    if code != 0:
        fail("olustur: %d %s %s" % (code, out, err))
    made = result(out)
    measure["create_s"] = round(time.time() - t0, 2)
    db = made["archive"]["database"]
    want_db = {c["name"]: ("view" if c.get("view") else c["documents"]) for c in db["collections"]}
    if made["event"] != "result" or made["skipped"] != 2 or made["archive"]["files"]["files"] != 3 or db["documents"] <= 0:
        fail("olustur: %s" % made)
    ok("Mac'te arşiv üretildi (%.1f sn): %s %s, dosyalar 3 (gizli dosya ve bağlantı alınmadı), %d bayt" %
       (measure["create_s"], SOURCE, want_db, made["archive"]["size"]))

    # ---- the panel takes it ----
    subprocess.run(["docker", "cp", made["path"], "%s:/tmp/m4-mac.tar" % C], check=True)
    out = sh("curl -sS --unix-socket /run/footops/agent.sock -H 'Content-Type: application/octet-stream' --data-binary @/tmp/m4-mac.tar "
             "'http://agent/v1/sites/%s/uploads/single?name=mac.footops.tar'" % SITE)
    up = json.loads(out)
    t0 = time.time()
    _, r = api("POST", "/v1/sites/%s/imports" % SITE, {"upload": up["id"], "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("check: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    measure["check_s"] = round(time.time() - t0, 2)
    _, ic = api("GET", "/v1/sites/%s/imports/%s" % (SITE, up["id"]))
    trial = dict(ic["archive"], **{v: "view" for v in ic.get("views") or []})
    if ic["format"] != "footops" or ic["manifest"]["site"] != SITE or ic["source_db"] != SOURCE or trial != want_db or \
            ic["sha256"] != made["archive"]["sha256"] or ic["files"]["archive"]["files"] != 3:
        fail("check: %s" % json.dumps(ic)[:2000])
    ok("panelin kontrolü kabul etti (%.1f sn): FootOps arşivi, deneme yüklemesi %s = Mac'teki sayım, sha256 aynı" % (measure["check_s"], trial))
    t0 = time.time()
    _, r = api("POST", "/v1/sites/%s/imports/%s/apply" % (SITE, up["id"]),
               {"options": {"db": True, "db_mode": "replace", "files": True, "files_mode": "replace"}, "confirm": SITE, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("apply: %s\n%s" % (j["error"], "\n".join(j["steps"])))
    measure["apply_s"] = round(time.time() - t0, 2)
    if counts() != want_db:
        fail("site's database: %s, want %s" % (counts(), want_db))
    listing = sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%P\\n' | sort" % DATA)
    sums = sh("cd %s && find . -type f -print0 | sort -z | xargs -0 -r sha256sum" % DATA)
    got_files = {line.split("  ", 1)[1].removeprefix("./"): line.split()[0] for line in sums.strip().splitlines()}
    if got_files != want_files or any(not l.startswith(("f 101:101 644 ", "d 101:101 755 ")) for l in listing.splitlines()):
        fail("site's files:\n%s\n%s\nwant %s" % (listing, got_files, want_files))
    ok("uygulandı (%.1f sn): sitenin veritabanı ve dosyaları Mac'tekinin aynısı (sha256), sahibi uygulamanın kullanıcısı (101), 644/755" %
       measure["apply_s"])

    # ---- the other way: the panel's export, on the Mac ----
    _, r = api("POST", "/v1/sites/%s/exports" % SITE, {"actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("export: %s" % j["error"])
    sh("curl -sS --unix-socket /run/footops/agent.sock -o /tmp/m4-exp.tar http://agent/v1/sites/%s/exports/%s/download" % (SITE, j["result"]))
    exp = os.path.join(WORK, "itbak.footops.tar")
    subprocess.run(["docker", "cp", "%s:/tmp/m4-exp.tar" % C, exp], check=True)
    code, out, err = mac("arsiv", "dogrula", exp, "--json")
    if code != 0:
        fail("dogrula: %d %s %s" % (code, out, err))
    info = result(out)
    if info["kind"] != "footops" or info["site"] != SITE or info["database"]["name"] != SITE or info["files"]["files"] != 3 or \
            info["database"]["documents"] != db["documents"]:
        fail("dogrula: %s" % info)
    ok("panelin dışa aktardığı arşiv Mac'te doğrulandı: %s, %d belge (manifest), 3 dosya" % (info["name"], info["database"]["documents"]))
    t0 = time.time()
    opened = os.path.join(WORK, "acilan")
    code, out, err = mac("arsiv", "ac", exp, "--hedef", opened, "--mongo", "--ad", BACK, "--json")
    if code != 0:
        fail("ac: %d %s %s" % (code, out, err))
    res = result(out)
    measure["open_s"] = round(time.time() - t0, 2)
    loaded = res["database"]
    if loaded["name"] != BACK or loaded["documents"] != db["documents"] or loaded.get("expected") != db["documents"] or res.get("warnings"):
        fail("ac: %s" % res)
    if local_tree(os.path.join(opened, "files")) != want_files or oct(os.stat(opened).st_mode & 0o777) != "0o700":
        fail("opened files: %s" % local_tree(os.path.join(opened, "files")))
    ok("Mac'te açıldı (%.1f sn): files/ Mac'teki klasörün aynısı, veritabanı %s olarak yüklendi — %d belge, manifestle aynı" %
       (measure["open_s"], BACK, loaded["documents"]))
    code, out, err = mac("arsiv", "acilanlar", "--json")
    v = json.loads(out)
    if code != 0 or [f["archive"] for f in v["folders"]] != ["itbak.footops.tar"] or BACK not in [d["name"] for d in v["databases"]]:
        fail("acilanlar: %s %s" % (out, err))
    for what in (opened, BACK):
        code, out, err = mac("arsiv", "sil", what)
        if code != 0:
            fail("sil %s: %s %s" % (what, out, err))
    if os.path.exists(opened):
        fail("the opened folder is left")
    ok("açılanlar listesinde arşivden geldiği yazıyor; klasör ve veritabanı silindi")
finally:
    cleanup()
print(json.dumps(measure, indent=2))
print("M4: hepsi geçti")
