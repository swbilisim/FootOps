#!/usr/bin/env python3
"""Y2 scenarios (the backup pages) against a real install, after f0.py (the S3 server and
the site y1.py uses are made here when missing): through the real panel a site's backup
settings are saved before there is a repository; the repository is set up — a refused
key, then a new repository in a folder of the bucket —; its password is shown only when
asked and only until it is confirmed, and the first backup then starts by itself; the
general settings; "back up now" for the server and for a site; a site whose database
user is gone shows up as an alert until a backup puts it right; pausing; replacing the
repository takes its password; the secrets never reach a page they should not, a log or
the audit log.

Usage: y2.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import re
import sys
import time

from itlib import ACTOR, api, fail, fresh_totp, ok, rss, sh, wait, wait_idle, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itbak", "bak.it.example.com"  # covered by f0's *.it.example.com certificate
VERSITY = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"
KEY, SECRET = "itdeneme01", "it-deneme-gizli-0123456789"
BUCKET = "footops-it"
JAR = "/tmp/y2-jar"
measure = {}


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def web(path, form=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar (a POST
    when there is a form). Returns (status, location, body); the time it took goes to
    web.took."""
    args = "".join(" --data-urlencode %s" % quote("%s=%s" % kv) for kv in (form or {}).items())
    out = sh("curl -sS -b %s -c %s -o /tmp/y2-page -w '%%{http_code} %%{redirect_url} %%{time_total}' %s 'http://localhost:7701%s'; "
             "echo; cat /tmp/y2-page" % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, loc, took = head.split(" ", 2)
    web.took = float(took)
    return int(code), loc.replace("http://localhost:7701", ""), body


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


def runs(n=20):
    return api("GET", "/v1/backup/runs?limit=%d" % n)[1]


def alerts():
    return {a["key"]: a for a in api("GET", "/v1/alerts")[1]}


def clock():
    """The container's time of day as (hour, minute)."""
    return tuple(map(int, sh("date +%H:%M").strip().split(":")))


def confirmed():
    """When the password was confirmed ("" if never: Go writes the zero time)."""
    c = api("GET", "/v1/backup/settings")[1].get("confirmed") or ""
    return "" if c.startswith("0001-") else c


def cli(args):
    out = sh("/usr/local/bin/footops backup %s" % args)
    return out


def secrets_nowhere(*secrets):
    """The secrets are in no log, job record or audit entry."""
    hay = sh("journalctl -u footops-web -u footops-agent --no-pager -q 2>/dev/null; cat /var/lib/footops/audit.jsonl; "
             "cat /var/lib/footops/jobs/*.json 2>/dev/null; cat /var/lib/footops/backup/runs/*.json 2>/dev/null || true")
    for s in secrets:
        if s and s in hay:
            fail("a secret reached a log, a job or the audit log")


print("Y2 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
measure["agent_rss_before_mb"] = round(rss("footops agent") / 2**20, 1)

# ---- the S3 server, the site, and no repository ----
if sh("docker inspect -f '{{.State.Running}}' it-s3 2>/dev/null || true").strip() != "true":
    sh("docker rm -f it-s3 >/dev/null 2>&1; docker volume create it-s3 >/dev/null", check=False)
    sh("docker run --rm -v it-s3:/veri --entrypoint mkdir mongo:7.0.45 -p /veri/%s" % BUCKET)
    sh("docker run -d --name it-s3 --network host -v it-s3:/veri -e ROOT_ACCESS_KEY_ID=%s -e ROOT_SECRET_ACCESS_KEY=%s "
       "-e VGW_REGION=auto %s posix /veri >/dev/null" % (KEY, SECRET, VERSITY))
sh("iptables -C INPUT -p tcp --dport 7070 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 7070 -j ACCEPT")
host_ip = sh("hostname -I").split()[0]
ENDPOINT = "http://%s:7070" % host_ip
FOLDER = "y2-%d" % time.time()  # a new, empty repository below the bucket
wait(lambda: sh("curl -s -o /dev/null -w %%{http_code} %s/" % ENDPOINT, check=False).strip() not in ("", "000"), "S3 server", timeout=60)
_, sites = api("GET", "/v1/sites")
if not any(s["spec"]["name"] == SITE for s in sites):
    _, r = api("POST", "/v1/sites", {"spec": {"name": SITE, "group": "musteri", "domains": [DOMAIN],
                                              "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                              "data_mount": "/usr/share/nginx/html", "database": True}, "env": {}, "actor": ACTOR})
    j = wait_job(r["job_id"])
    if j.get("error"):
        fail("site create: %s" % j["error"])
sh("docker start %s >/dev/null; docker unpause %s >/dev/null 2>&1 || true" % (SITE, SITE), check=False)
sh("mkdir -p /srv/musteri/%s/data/img && head -c 300000 /dev/urandom > /srv/musteri/%s/data/img/y2.jpg && chown -R 101:101 /srv/musteri/%s/data"
   % (SITE, SITE, SITE))
sh("rm -rf /etc/footops/secrets/backup/repository.json /var/lib/footops/backup/settings.json /var/lib/footops/backup/state.json "
   "/var/lib/footops/backup/runs")
ok("S3 sunucusu, %s sitesi hazır; yedek deposu yok" % SITE)

# ---- the panel: not set up ----
user, pw, totp_secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(totp_secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
code, _, page = web("/backup")
tok = csrf(page)
if code != 200 or "Depoyu bağla" not in page or 'href="/backup" aria-current="page"' not in page:
    fail("not set up: %d" % code)
measure["page_setup_ms"] = round(web.took * 1000)
wait(lambda: "backup:setup" in alerts(), "the setup alert", timeout=30)
if "Uzak yedekleme kurulmadı" not in web("/")[2]:
    fail("the dashboard does not show the setup alert")
ok("kurulmamış: Yedekleme sayfası kurulum formunu, pano 'Uzak yedekleme kurulmadı' uyarısını gösteriyor")

# A site's settings before there is a repository (a SQLite app is frozen from the first backup).
code, loc, _ = web("/sites/%s/backup" % SITE, {"csrf": tok, "on": "on", "db": "hourly", "files": "", "freeze": "on", "keep_days": "14"})
_, s = api("GET", "/v1/backup/settings")
if code != 303 or s.get("sites", {}).get(SITE) != {"db": "hourly", "freeze": True, "keep_days": 14}:
    fail("site settings: %d %s" % (code, s.get("sites")))
ok("depo yokken site ayarı kaydedildi (saatlik veritabanı, dondurma, 14 gün)")

# ---- setup: a refused key, an endpoint with a path, then a new repository ----
form = {"csrf": tok, "endpoint": ENDPOINT, "bucket": BUCKET + "/" + FOLDER, "key_id": KEY, "secret_key": "yanlis-gizli-anahtar-0000"}
t0 = time.time()
code, loc, _ = web("/backup/setup", form)
jid, j = job_of(loc)
if "erişimi reddetti" not in (j.get("error") or "") or time.time() - t0 > 30:
    fail("refused key: %.1fs %s" % (time.time() - t0, j.get("error")))
if sh("test -e /etc/footops/secrets/backup/repository.json && echo y || true").strip():
    fail("a refused setup saved settings")
_, _, page = web("/jobs/" + jid)
if 'class="crumb" href="/backup"' not in page or "Yedekleme sayfasına dön" not in page or "Siteye git" in page:
    fail("setup job page")
ok("yanlış anahtar %.1f sn'de reddedildi; iş sayfası Yedekleme'ye dönüyor, ayar kaydedilmedi" % (time.time() - t0))
code, _, page = web("/backup/setup", dict(form, endpoint=ENDPOINT + "/" + BUCKET))
if code != 422 or "kovayı kendi alanına" not in page or "yanlis-gizli" in page:
    fail("endpoint with a path: %d" % code)
form["secret_key"] = SECRET
t0 = time.time()
code, loc, _ = web("/backup/setup", form)
jid, j = job_of(loc)
measure["setup_s"] = round(time.time() - t0, 1)
if j.get("error") or j.get("result") != "created":
    fail("setup: %s %s" % (j.get("error"), j.get("result")))
_, _, page = web("/jobs/" + jid)
if "Devam: şifreyi kaydet" not in page:
    fail("the job does not lead to the password")
sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))
repo_pw = sec["password"]
if sec["repository"] != "s3:%s/%s/%s" % (ENDPOINT, BUCKET, FOLDER) or len(repo_pw) != 40:
    fail("saved repository: %s" % sec["repository"].replace(host_ip, "<ip>"))
ok("yeni depo panelden kuruldu (%.1f sn): %s/%s" % (measure["setup_s"], BUCKET, FOLDER))

# ---- the password: only when asked, only until confirmed ----
code, _, page = web("/backup")
if "Şifreyi göster" not in page or repo_pw in page:
    fail("the password on the page before it was asked for")
wait(lambda: "onaylanmadı" in alerts().get("backup:setup", {}).get("title", ""), "the not-confirmed alert", timeout=30)
code, _, page = web("/backup/key", {"csrf": tok})
if code != 200 or repo_pw not in page or "data-live=" in page:
    fail("password page: %d" % code)
if '"backup.key"' not in sh("tail -n 5 /var/lib/footops/audit.jsonl"):
    fail("showing the password is not in the audit log")
ok("şifre yalnız 'Şifreyi göster' ile, kendini yenilemeyen sayfada; her gösterim denetimde")

# A new key for the same repository before the password was confirmed: connected with
# the server's own password, and still not confirmed (nobody saw the password).
code, loc, _ = web("/backup/setup", {"csrf": tok, "mode": "same", "endpoint": ENDPOINT, "bucket": BUCKET + "/" + FOLDER,
                                     "key_id": KEY, "secret_key": SECRET})
_, j = job_of(loc)
if j.get("error") or j.get("result") != "connected" or confirmed():
    fail("same repository before confirm: %s %s %s" % (j.get("error"), j.get("result"), confirmed()))
if json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))["password"] != repo_pw:
    fail("the password changed")
code, _, page = web("/backup")
if "Şifreyi göster" not in page:
    fail("not in the password step any more")
ok("onaydan önce 'aynı depo, yeni anahtar': sunucudaki şifreyle bağlandı, onaylanmış sayılmadı")

# The night time 30 minutes ago: confirming owes the night run at once (as on a server set
# up in the afternoon); the hourly slot is then half an hour away.
h, mi = clock()
night = (h * 60 + mi - 30) % 1440
cli("set night=%02d:%02d" % (night // 60, night % 60))
code, _, _ = web("/backup/confirm", {"csrf": tok})
if code != 422:
    fail("confirmed without the box: %d" % code)
before = {r["id"] for r in runs(50)}
t0 = time.time()
code, loc, _ = web("/backup/confirm", {"csrf": tok, "saved": "on"})
if code != 303 or loc != "/backup":
    fail("confirm: %d %s" % (code, loc))
code, _, page = web("/backup")
if "Onaylandı" not in page or repo_pw in page:
    fail("after confirm")
code, loc, _ = web("/backup/key", {"csrf": tok})
code, _, page = web(loc)
if repo_pw in page or "Şifre gösterilemedi" not in page:
    fail("the panel still shows the password after confirm")
ok("onaylandı: panel şifreyi artık göstermiyor (sunucuda show-key)")

# ---- the first backup starts by itself ----
live = False
while time.time() - t0 < 300:
    new = [r for r in runs(50) if r["id"] not in before and r["kind"] == "daily"]
    if new:
        break
    if not live:
        _, _, page = web("/backup")
        live = "Şu an yedek alınıyor" in page and 'data-live="5"' in page
    time.sleep(0.3)
else:
    fail("no backup started after confirm")
first = new[0]
measure["first_backup_after_confirm_s"] = round(time.time() - t0, 1)
if first.get("error"):
    fail("first backup: %s" % first["error"])
parts = {(p["site"], p["part"]) for p in first["parts"]}
if not {(SITE, "db"), (SITE, "files"), (SITE, "def"), ("_server", "server")} <= parts:
    fail("first backup parts: %s" % parts)
steps = api("GET", "/v1/jobs/" + first["id"])[1]["steps"]
if not any("donuk kaldı" in s for s in steps):
    fail("the site was not frozen for its files:\n" + "\n".join(steps))
measure["first_backup_parts"], measure["first_backup_took_s"] = len(first["parts"]), round(
    sum(p.get("took", 0) for p in first["parts"]) / 1e9, 1)
ok("ilk yedek onaydan sonra kendiliğinden başladı (%.0f sn içinde biten gece işi, %d parça, %s dondurularak)%s" %
   (measure["first_backup_after_confirm_s"], len(first["parts"]), SITE, "; sayfa iş sürerken canlı" if live else ""))
# A clean night run is followed by the monthly trial restore: never made in this new
# repository, so now.
wait(lambda: (api("GET", "/v1/backup")[1].get("trial") or {}).get("job"), "the trial after the first night run", timeout=300)
trial = api("GET", "/v1/backup")[1]["trial"]
if not trial.get("ok"):
    fail("trial after the first night run: %s" % trial)
wait_idle()
ok("ardından aylık deneme geri yüklemesi kendiliğinden: %s — %s" % (trial["site"], trial["detail"]))
wait(lambda: not any(k.startswith("backup:") for k in alerts()), "backup alerts to clear", timeout=30)

code, _, page = web("/backup")
measure["page_ready_ms"] = round(web.took * 1000)
for want in ["Son yedekler", "tamam", "gece", ">%s<" % SITE, "Depoda", "kendi ayarı · dondurarak"]:
    if want not in page:
        fail("backup page lacks %r" % want)
code, _, page = web("/sites/%s/backup" % SITE)
if code != 200 or "son " not in page or "Şimdi yedekle" not in page:
    fail("site backup page: %d" % code)
code, _, page = web("/sites/%s" % SITE)
if "Uzak yedek" not in page or "veritabanı saatlik" not in page:
    fail("site page lacks the backup line")
ok("Yedekleme sayfası (%d ms), site yedek sayfası ve site sayfasındaki özet son yedeği gösteriyor; uyarı kalmadı" % measure["page_ready_ms"])

# ---- general settings; the site's own stay ----
gen = {"csrf": tok, "night": "%02d:%02d" % (night // 60, night % 60), "db": "daily", "files": "weekly", "keep_hours": "24",
       "keep_days": "30", "keep_months": "6", "warn_gb": "3"}
code, _, page = web("/backup/settings", dict(gen, keep_days="otuz"))
if code != 422 or "Günlük saklama sayı olmalı" not in page:
    fail("bad number: %d" % code)
code, loc, _ = web("/backup/settings", gen)
_, s = api("GET", "/v1/backup/settings")
if code != 303 or s["defaults"] != {"db": "daily", "files": "weekly", "keep_hours": 24, "keep_days": 30, "keep_months": 6} or \
        s.get("warn_gb") != 3 or s["sites"].get(SITE, {}).get("freeze") is not True or not confirmed():
    fail("general settings: %d %s" % (code, s))
ok("genel ayarlar formdan kaydedildi; site ayarı ve onay korundu")

# ---- back up now: the server, one site ----
code, loc, _ = web("/backup/run", {"csrf": tok})
jid, j = job_of(loc)
if j.get("error"):
    fail("run all: %s" % j["error"])
code, loc, _ = web("/sites/%s/backup/run" % SITE, {"csrf": tok})
jid, j = job_of(loc)
_, _, page = web("/jobs/" + jid)
if j.get("error") or 'class="crumb" href="/sites/%s/backup"' % SITE not in page:
    fail("run site: %s" % j.get("error"))
measure["site_run_s"] = round(sum(p.get("took", 0) for p in runs(1)[0]["parts"]) / 1e9, 1)
ok("Şimdi yedekle: bütün sunucu ve tek site (%.1f sn); iş sayfası site yedeklerine dönüyor" % measure["site_run_s"])

# ---- a failure is an alert until a backup puts it right ----
uri = re.search(r'MONGODB_URI: "([^"]+)"', sh("cat /srv/musteri/%s/compose.yaml" % SITE)).group(1)
site_pw = re.match(r"mongodb://[^:]+:([^@]+)@", uri).group(1)
root_pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env")).group(1)


def root_js(js):
    return sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin",
              stdin=('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);' % json.dumps(root_pw) + js).encode())


root_js('c.getDB("%s").dropUser("%s");' % (SITE, SITE))
code, loc, _ = web("/sites/%s/backup/run" % SITE, {"csrf": tok})
_, j = job_of(loc)
if "veritabanı" not in (j.get("error") or ""):
    fail("a backup without the database user: %s" % j.get("error"))
wait(lambda: alerts().get("backup:failed", {}).get("title") == "Yedek alınamadı: %s (veritabanı)" % SITE, "the failure alert", timeout=30)
_, _, page = web("/")
_, _, bpage = web("/backup")
if "Yedek alınamadı: %s (veritabanı)" % SITE not in page or "✗ başarısız" not in bpage:
    fail("the failure is not on the pages")
root_js('c.getDB("%s").createUser({user: "%s", pwd: %s, roles: [{role: "readWrite", db: "%s"}]});' % (SITE, SITE, json.dumps(site_pw), SITE))
code, loc, _ = web("/sites/%s/backup/run" % SITE, {"csrf": tok})
_, j = job_of(loc)
if j.get("error"):
    fail("after the user is back: %s" % j["error"])
wait(lambda: "backup:failed" not in alerts(), "the failure alert to clear", timeout=30)
ok("veritabanı kullanıcısı yokken yedek başarısız → panoda ve Yedekleme'de 'Yedek alınamadı: %s (veritabanı)'; düzelince uyarı kalktı" % SITE)

# ---- pause and resume ----
code, _, _ = web("/backup/pause", {"csrf": tok, "paused": "on"})
_, s = api("GET", "/v1/backup/settings")
_, _, page = web("/backup")
if code != 303 or not s.get("paused") or "Zamanlanmış yedekleme duraklatıldı" not in page:
    fail("pause")
wait(lambda: "backup:paused" in alerts(), "the paused alert", timeout=30)
web("/backup/pause", {"csrf": tok, "paused": "off"})
if api("GET", "/v1/backup/settings")[1].get("paused"):
    fail("resume")
ok("duraklat / sürdür tek tıkla; duraklatılınca uyarı")

# ---- replacing: a new key for the same repository freely, another repository with the password ----
code, _, page = web("/backup/repo")
if code != 200 or 'name="current"' not in page:
    fail("replace page")
was = confirmed()
rep = {"csrf": tok, "mode": "same", "endpoint": ENDPOINT, "bucket": BUCKET + "/" + FOLDER, "key_id": KEY, "secret_key": SECRET}
code, loc, _ = web("/backup/setup", rep)
_, j = job_of(loc)
if j.get("error") or j.get("result") != "connected" or not was or confirmed() != was:
    fail("same repository: %s %s" % (j.get("error"), j.get("result")))
new = dict(rep, mode="new", bucket=BUCKET + "/" + FOLDER + "-yeni")
code, _, page = web("/backup/setup", new)
if code != 422 or "şu anki deponun şifresi gerekli" not in page:
    fail("new repository without the password: %d" % code)
code, _, page = web("/backup/setup", dict(new, current="yanlis-sifre-0123456789"))
if code != 422 or "şu anki deponun şifresi gerekir" not in page:
    fail("new repository with a wrong password: %d" % code)
if json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))["repository"] != "s3:%s/%s/%s" % (ENDPOINT, BUCKET, FOLDER):
    fail("the repository was replaced")
ok("aynı depo + yeni anahtar şifresiz bağlandı (onay korundu); başka depoya geçmek şu anki şifreyi istiyor (yoksa / yanlışsa reddedildi)")

# ---- the secrets stayed where they belong ----
secrets_nowhere(repo_pw, SECRET, site_pw)
ok("depo şifresi, gizli anahtar ve sitenin veritabanı şifresi hiçbir günlükte, iş kaydında, denetimde yok")

measure["web_rss_mb"] = round(rss("footops web") / 2**20, 1)
measure["agent_rss_after_mb"] = round(rss("footops agent") / 2**20, 1)
web("/sites/%s/backup" % SITE, {"csrf": tok, "reset": "1"})
cli("set night=04:00")
print(json.dumps(measure, indent=2, ensure_ascii=False))
print("Y2: hepsi geçti")
