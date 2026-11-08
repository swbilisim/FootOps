#!/usr/bin/env python3
"""Y4 scenarios (notifications, the status file, removed sites) against a real install,
after f0.py (y2.py's S3 server and repository are set up here when missing): a stand-in
Telegram (tgfake.py, the agent pointed at it with a systemd drop-in) is linked through
the real panel — the code is looked for, another chat's message does not count —, a
test message, an alert that goes out once and not again after an agent restart, a
Telegram that cannot be reached, /durum answered to the person who linked the chat only
(with a button that brings it up to date in place); the status file appears next to the repository in the
S3 server, without secrets, and follows a site's removal; a removed site is listed and
removed for good (last copy of its database first, the database and its user gone, the
image of a site still running kept). The bot token never reaches a page, a log or the
audit log.

Usage: y4.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import os
import re
import sys
import time

from itlib import ACTOR, api, fail, fresh_totp, ok, rss, sh, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
TOKEN = "123456789:AAH-it_secret-part-0123456789abcdefgh"
TG_PORT = 9811
VERSITY = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"
KEY, SECRET, BUCKET = "itdeneme01", "it-deneme-gizli-0123456789", "footops-it"
PURGE, PURGE_DOMAIN = "itsil", "sil.it.example.com"  # covered by f0's *.it.example.com certificate
JAR = "/tmp/y4-jar"
measure = {}


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def web(path, form=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar (a POST
    when there is a form). Returns (status, location, body)."""
    args = "".join(" --data-urlencode %s" % quote("%s=%s" % kv) for kv in (form or {}).items())
    out = sh("curl -sS -b %s -c %s -o /tmp/y4-page -w '%%{http_code} %%{redirect_url} %%{time_total}' %s 'http://localhost:7701%s'; "
             "echo; cat /tmp/y4-page" % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, loc, took = head.split(" ", 2)
    web.took = float(took)
    return int(code), loc.replace("http://localhost:7701", ""), body


def csrf(page):
    m = re.search(r'name="csrf" value="([^"]+)"', page)
    if not m:
        fail("no csrf token in page")
    return m.group(1)


def sent():
    out = sh("cat /tmp/tg-sent.jsonl 2>/dev/null || true")
    return [json.loads(l) for l in out.splitlines() if l.strip()]


def agent_up():
    wait(lambda: sh("test -S /run/footops/agent.sock && curl -s --unix-socket /run/footops/agent.sock http://agent/v1/notify "
                    ">/dev/null && echo y", check=False).strip() == "y", "agent socket", timeout=60)


def token_nowhere():
    hay = sh("journalctl -u footops-web -u footops-agent --no-pager -q 2>/dev/null; cat /var/lib/footops/audit.jsonl; "
             "cat /var/lib/footops/jobs/*.json /var/lib/footops/notify.json /var/lib/footops/telegram-buttons.json 2>/dev/null || true")
    if TOKEN in hay or TOKEN.split(":")[1] in hay:
        fail("the bot token reached a log, a job or the audit log")


def mongo_eval(js):
    """Runs js in MongoDB as root (the password from the secrets file, not on a command line)."""
    return sh("set -a; . /etc/footops/secrets/mongo.env; set +a; docker exec -e MONGO_INITDB_ROOT_USERNAME -e MONGO_INITDB_ROOT_PASSWORD "
              "footops-mongo mongosh --quiet --norc mongodb://127.0.0.1:27017/admin --eval "
              "%s" % quote("db.auth(process.env.MONGO_INITDB_ROOT_USERNAME, process.env.MONGO_INITDB_ROOT_PASSWORD); " + js)).strip()


print("Y4 senaryoları")
agent_up()
code_ = json.loads(sh("cat /etc/footops/footops.json")).get("server_code", "")
PREFIX = "[%s]" % code_.upper()

# ---- the install: the notifications' directory, writable for the agent alone ----
if sh("stat -c '%a %U' /etc/footops/secrets/notify").strip() != "700 root":
    fail("notify directory: %s" % sh("stat -c '%a %U' /etc/footops/secrets/notify", check=False))
if "ReadWritePaths=-/etc/footops/secrets/backup -/etc/footops/secrets/notify" not in sh("cat /etc/systemd/system/footops-agent.service"):
    fail("agent unit lacks the notify directory")
ok("kurulum: /etc/footops/secrets/notify (700 root), agent birimi oraya yazabiliyor")

# ---- the stand-in Telegram, the agent pointed at it ----
sh("cat > /root/tgfake.py", stdin=open(os.path.join(os.path.dirname(__file__), "tgfake.py"), "rb").read())
sh("systemctl stop it-tgfake 2>/dev/null; rm -f /tmp/tg-sent.jsonl /tmp/tg-updates.json; echo '[]' > /tmp/tg-updates.json", check=False)
sh("systemd-run --unit it-tgfake --property PrivateTmp=no python3 /root/tgfake.py %d %s >/dev/null" % (TG_PORT, TOKEN))
wait(lambda: sh("curl -s -o /dev/null -w %%{http_code} -X POST http://127.0.0.1:%d/bot%s/getMe" % (TG_PORT, TOKEN), check=False).strip() == "200",
     "the stand-in Telegram", timeout=30)
sh("mkdir -p /etc/systemd/system/footops-agent.service.d && printf '[Service]\\nEnvironment=FOOTOPS_TELEGRAM_API=http://127.0.0.1:%d\\n' "
   "> /etc/systemd/system/footops-agent.service.d/it-telegram.conf && systemctl daemon-reload && systemctl restart footops-agent" % TG_PORT)
restarted = time.time()
agent_up()
api("POST", "/v1/notify/telegram/remove", {"actor": ACTOR})
# A link left by a run cut short was read until removed just now (its command menu set):
# that is not this run's.
sh(": > /tmp/tg-sent.jsonl")
ok("sahte Telegram 127.0.0.1:%d, agent ona yönlendirildi (systemd drop-in)" % TG_PORT)

# ---- a repository (y2's S3 server), if y2 / y3 did not leave one ----
if not sh("test -e /etc/footops/secrets/backup/repository.json && echo y || true").strip():
    if sh("docker inspect -f '{{.State.Running}}' it-s3 2>/dev/null || true").strip() != "true":
        sh("docker rm -f it-s3 >/dev/null 2>&1; docker volume create it-s3 >/dev/null", check=False)
        sh("docker run --rm -v it-s3:/veri --entrypoint mkdir mongo:7.0.45 -p /veri/%s" % BUCKET)
        sh("docker run -d --name it-s3 --network host -v it-s3:/veri -e ROOT_ACCESS_KEY_ID=%s -e ROOT_SECRET_ACCESS_KEY=%s "
           "-e VGW_REGION=auto %s posix /veri >/dev/null" % (KEY, SECRET, VERSITY))
    host_ip = sh("hostname -I").split()[0]
    _, r = api("POST", "/v1/backup/setup", {"repository": "s3:http://%s:7070/%s/y4-%d" % (host_ip, BUCKET, time.time()),
                                            "key_id": KEY, "secret_key": SECRET, "actor": ACTOR})
    if wait_job(r["job_id"]).get("error"):
        fail("repository setup")
sh("docker start it-s3 >/dev/null 2>&1 || true", check=False)
sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))
repo_path = sec["repository"].split("://", 1)[1].split("/", 1)[1].strip("/")  # bucket/folder
ok("yedek deposu: %s" % repo_path)

# ---- linking through the panel ----
user, pw, totp_secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(totp_secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
code, _, page = web("/notify")
tok = csrf(page)
if code != 200 or "Telegram'ı bağla" not in page or 'href="/notify" aria-current="page"' not in page:
    fail("notify page: %d" % code)
measure["page_notify_ms"] = round(web.took * 1000)
code, _, page = web("/notify/telegram", {"csrf": tok, "token": "123456789:AAH-wrong_secret-part-0123456789abcdefgh"})
if code != 422 or "geçersiz" not in page or "wrong_secret" in page:
    fail("wrong token: %d" % code)
code, loc, _ = web("/notify/telegram", {"csrf": tok, "token": TOKEN})
if code != 303 or loc != "/notify":
    fail("start: %d %s" % (code, loc))
_, _, page = web("/notify")
m = re.search(r"https://t\.me/OrnekBot\?start=([A-Z0-9]{10})", page)
if not m or TOKEN in page:
    fail("the code is not shown (or the token is)")
link_code = m.group(1)
code, _, page = web("/notify/link", {"csrf": tok})
if code != 409 or "henüz bota ulaşmadı" not in page:
    fail("before the message: %d" % code)
# Someone else writes to the bot first: not linked to them.
updates = [{"update_id": 1, "message": {"message_id": 1, "date": 1, "text": "/start", "chat": {"id": 999, "type": "private", "first_name": "Yabanci"}}},
           {"update_id": 2, "message": {"message_id": 2, "date": 2, "text": "/start " + link_code,
                                        "chat": {"id": 555, "type": "private", "first_name": "Deneme", "username": "deneme"},
                                        "from": {"id": 555, "is_bot": False, "first_name": "Deneme", "username": "deneme"}}}]
sh("cat > /tmp/tg-updates.json", stdin=json.dumps(updates[:1]).encode())
if web("/notify/link", {"csrf": tok})[0] != 409:
    fail("linked to another chat")
sh("cat > /tmp/tg-updates.json", stdin=json.dumps(updates).encode())
t0 = time.time()
code, loc, _ = web("/notify/link", {"csrf": tok})
measure["link_ms"] = round((time.time() - t0) * 1000)
if code != 303:
    fail("link: %d" % code)
_, _, page = web("/notify")
if "Telegram bağlandı: @OrnekBot → Deneme (@deneme)" not in page or "Deneme mesajı gönder" not in page:
    fail("linked page")
msgs = [m for m in sent() if m.get("_method") == "sendMessage"]
if len(msgs) < 1 or msgs[0].get("chat_id") != "555" or "%s FootOps bu sohbete bağlandı" % PREFIX not in msgs[0].get("text", "") or \
        "/durum" not in msgs[0].get("text", ""):
    fail("welcome: %s" % msgs[:1])
cfg = json.loads(sh("cat /etc/footops/secrets/notify/telegram.json"))
if cfg["token"] != TOKEN or cfg["chat_id"] != 555 or cfg.get("user_id") != 555 or \
        sh("stat -c '%a %U' /etc/footops/secrets/notify/telegram.json").strip() != "600 root":
    fail("saved settings")
ok("panelden bağlandı (%d ms): kod gösterildi, önce yazan başka sohbet sayılmadı, karşılama 555'e gitti; ayar 600 root" % measure["link_ms"])

web("/notify/test", {"csrf": tok})
if not any("Deneme mesajı" in m.get("text", "") for m in sent()):
    fail("test message")
ok("deneme mesajı gitti")

# ---- an alert: once, not again after an agent restart ----
_, s = api("GET", "/v1/backup/settings")
api("POST", "/v1/backup/settings", {"settings": dict(s, paused=True), "actor": ACTOR})
t0 = time.time()
wait(lambda: any("Zamanlanmış yedekleme duraklatıldı" in m.get("text", "") for m in sent()), "the paused alert on Telegram", timeout=60)
measure["alert_to_telegram_s"] = round(time.time() - t0, 1)
first = [m for m in sent() if "duraklatıldı" in m.get("text", "")]
if not first[0]["text"].startswith("🟡 %s Zamanlanmış yedekleme duraklatıldı" % PREFIX):
    fail("alert text: %s" % first[0]["text"])
sh("systemctl restart footops-agent")
agent_up()
time.sleep(40)  # the alerts are worked out again (10 s), the notifier looks (15 s)
if len([m for m in sent() if "duraklatıldı" in m.get("text", "")]) != 1:
    fail("the alert was sent again after a restart")
ok("uyarı %.1f sn'de Telegram'a gitti; agent yeniden başlayınca tekrar gönderilmedi" % measure["alert_to_telegram_s"])
api("POST", "/v1/backup/settings", {"settings": dict(s, paused=False), "actor": ACTOR})

# ---- Telegram out of reach ----
sh("systemctl stop it-tgfake")
web("/notify/test", {"csrf": tok})
_, _, page = web("/notify")
if "Deneme mesajı gönderilemedi: Telegram&#39;a ulaşılamadı" not in page or TOKEN in page or "123456789" in page:
    fail("unreachable Telegram not said (or the token shown)")
sh("systemd-run --unit it-tgfake --property PrivateTmp=no python3 /root/tgfake.py %d %s >/dev/null" % (TG_PORT, TOKEN))
ok("Telegram'a ulaşılamayınca nedeni token'sız gösterildi")


# ---- commands from the chat (P1a): only the person who linked it, answered with a button ----
def tg_add(*ups):
    """Appends updates to the stand-in's queue, numbered after the last one."""
    cur = json.loads(sh("cat /tmp/tg-updates.json"))
    last = max([u["update_id"] for u in cur] or [0])
    for i, u in enumerate(ups):
        u["update_id"] = last + 1 + i
    sh("cat > /tmp/tg-updates.new && mv /tmp/tg-updates.new /tmp/tg-updates.json", stdin=json.dumps(cur + list(ups)).encode())


def tg_msg(chat, sender, text):
    return {"message": {"message_id": 50, "date": int(time.time()), "text": text,
                        "chat": {"id": chat, "type": "private", "first_name": "Deneme"},
                        "from": {"id": sender, "is_bot": False, "first_name": "Deneme"}}}


def calls(method, since):
    return [m for m in sent()[since:] if m.get("_method") == method]


rss_before = rss("footops agent")
mark = len(sent())
tg_add(tg_msg(999, 999, "/durum"), tg_msg(555, 777, "/durum"), tg_msg(555, 555, "/durum"))
t0 = time.time()
wait(lambda: any("Sunucu durumu" in m.get("text", "") for m in calls("sendMessage", mark)), "the /durum answer", timeout=60)
measure["durum_answer_s"] = round(time.time() - t0, 1)
time.sleep(2)  # anything the strangers' messages would have brought
answers = calls("sendMessage", mark)
report = answers[0]["text"]
if len(answers) != 1 or answers[0]["chat_id"] != "555" or not report.startswith("🖥 %s Sunucu durumu · " % PREFIX) or \
        "Siteler: " not in report or "Disk: " not in report or "Yedek: " not in report:
    fail("/durum answers: %s" % answers)
button = json.loads(answers[0]["reply_markup"])["inline_keyboard"][0][0]["callback_data"]
mark = len(sent())
tg_add({"callback_query": {"id": "cb-it", "data": button, "from": {"id": 555, "is_bot": False, "first_name": "Deneme"},
                           "message": {"message_id": answers[0]["_message_id"], "date": 1, "chat": {"id": 555, "type": "private"}}}})
wait(lambda: calls("answerCallbackQuery", mark), "the button's answer", timeout=30)
edits, pressed = calls("editMessageText", mark), calls("answerCallbackQuery", mark)
if len(edits) != 1 or edits[0]["message_id"] != str(answers[0]["_message_id"]) or "Sunucu durumu" not in edits[0]["text"] or \
        pressed[0].get("text") != "Güncellendi":
    fail("refresh: %s %s" % (edits, pressed))
if not calls("setMyCommands", 0) or '"telegram.command"' not in sh("tail -n 5 /var/lib/footops/audit.jsonl"):
    fail("no command menu, or the command not audited")
if sh("stat -c '%a %U' /var/lib/footops/telegram-buttons.json").strip() != "600 root":
    fail("buttons file")
st = api("GET", "/v1/notify")[1]
cmds = st.get("commands") or {}
if not cmds.get("reading") or cmds.get("user") != "Deneme (@deneme)" or cmds.get("last_command") != "Yenile":
    fail("commands status: %s" % cmds)
measure["agent_rss_reading_kb"] = round((rss("footops agent") - rss_before) / 1024)
ok("komutlar: /durum yalnız bağlayana cevaplandı (%.1f sn; iki yabancıya cevap yok), Yenile mesajı yerinde güncelledi, "
   "komut denetimde, menü ayarlandı" % measure["durum_answer_s"])

# ---- the status file next to the repository ----
def status_file():
    out = sh("docker run --rm -v it-s3:/veri mongo:7.0.45 cat /veri/%s/izleme/durum.json 2>/dev/null || true" % repo_path)
    return json.loads(out) if out.strip() else None


wait(lambda: status_file() is not None, "the status file", timeout=max(10, 75 - (time.time() - restarted)) + 60)
st = status_file()
raw = json.dumps(st)
for secret in (sec["password"], sec["secret_key"], sec["key_id"], TOKEN):
    if secret in raw:
        fail("a secret in the status file")
names = {x["name"]: x for x in st["sites"]}
if st["format"] != 1 or st["server"].lower() != code_.lower() or st["every"] != 300 or "itsite" not in names or \
        names["itsite"]["state"] != "running" or not names["itsite"]["domains"] or not st["backup"]["ready"]:
    fail("status file: %s" % raw[:600])
_, ns = api("GET", "/v1/notify")
if not ns.get("status_file", {}).get("last_ok") or ns["status_file"]["key"] != repo_path + "/izleme/durum.json":
    fail("status file state: %s" % ns.get("status_file"))
measure["status_file_bytes"] = len(sh("docker run --rm -v it-s3:/veri mongo:7.0.45 cat /veri/%s/izleme/durum.json" % repo_path))
ok("durum dosyası kovada: %d site, %d bayt, sırsız; panel son yazmayı gösteriyor" % (len(st["sites"]), measure["status_file_bytes"]))

# ---- the outside watcher's heartbeat, read with the agent's own signed GET ----
BEAT_URL = sec["repository"][len("s3:"):].rstrip("/") + "/izleme/nobetci.json"


def beat(minutes_ago, down=False):
    """Puts a heartbeat as the watcher would, with curl's own SigV4 (not the agent's)."""
    t = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - minutes_ago * 60))
    doms = [{"name": "it.example.com", "site": "itsite", "ok": not down}]
    if down:
        doms[0].update(down=True, code="502", reason="502 — site yanıt vermiyor", since=t)
    sh("curl -sS -f --aws-sigv4 aws:amz:auto:s3 --user %s -X PUT -H 'Content-Type: application/json' --data-binary @- %s"
       % (quote(sec["key_id"] + ":" + sec["secret_key"]), BEAT_URL),
       stdin=json.dumps({"format": 1, "time": t, "every": 300, "interval": 120, "domains": doms}).encode())


def watcher():
    _, ns = api("GET", "/v1/notify")
    return ns.get("status_file", {})


def alert_keys():
    _, al = api("GET", "/v1/alerts")
    return {a["key"] for a in (al if isinstance(al, list) else al.get("alerts", []))}


def kick():
    """A site job makes the agent write the status file (and read the heartbeat) at once."""
    _, r = api("POST", "/v1/sites/itsite/redeploy", {"actor": ACTOR})
    if wait_job(r["job_id"]).get("error"):
        fail("redeploy for the status file")


if not watcher().get("watcher_none"):
    kick()
    wait(lambda: watcher().get("watcher_none"), "no heartbeat said", timeout=30)
beat(1, down=True)
kick()
wait(lambda: (watcher().get("watcher") or {}).get("domains"), "the heartbeat read", timeout=30)
w = watcher()["watcher"]
if w["domains"][0]["reason"] != "502 — site yanıt vermiyor" or w["interval"] != 120 or watcher().get("watcher_error"):
    fail("heartbeat: %s" % watcher())
_, _, page = web("/notify")
if "Son yoklama" not in page or "1 tanesi açılmıyor" not in page or "it.example.com" not in page:
    fail("the notify page does not show the watcher")
if "monitor:watcher" in alert_keys():
    fail("an alert for a fresh heartbeat")
beat(40)
kick()
wait(lambda: "monitor:watcher" in alert_keys(), "the watcher's alert", timeout=40)
sh("curl -sS -f --aws-sigv4 aws:amz:auto:s3 --user %s -X DELETE %s" % (quote(sec["key_id"] + ":" + sec["secret_key"]), BEAT_URL))
kick()
wait(lambda: "monitor:watcher" not in alert_keys() and watcher().get("watcher_none"), "the alert gone with the heartbeat", timeout=40)
ok("dış nöbetçinin kalp atışı agent'ın imzalı GET'iyle okundu: panelde görünüyor, 40 dk eskisi uyarı, silinince uyarı yok")

# ---- a removed site, removed for good ----
_, r = api("POST", "/v1/sites", {"spec": {"name": PURGE, "group": "musteri", "domains": [PURGE_DOMAIN],
                                          "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64,
                                          "data_mount": "/usr/share/nginx/html", "database": True}, "env": {}, "actor": ACTOR})
if wait_job(r["job_id"]).get("error"):
    fail("site create")
mongo_eval("db.getSiblingDB('%s').notlar.insertMany([{a: 1}, {a: 2}, {a: 3}])" % PURGE)
_, r = api("POST", "/v1/sites/%s/remove" % PURGE, {"actor": ACTOR})
if wait_job(r["job_id"]).get("error"):
    fail("site remove")
wait(lambda: PURGE not in {x["name"] for x in (status_file() or {"sites": []})["sites"]}, "the status file without the removed site", timeout=30)
ok("site kaldırılınca durum dosyası hemen güncellendi")
code, _, page = web("/sites")
if 'href="/removed"' not in page:
    fail("no link to the removed sites")
code, _, page = web("/removed")
m = re.search(r'href="/removed/(%s-\d{8}-\d{6})"' % PURGE, page)
if code != 200 or not m:
    fail("removed list: %d" % code)
rid = m.group(1)
measure["page_removed_ms"] = round(web.took * 1000)
code, _, page = web("/removed/" + rid, {"csrf": tok, "confirm": "yanlis"})
if code != 422 or not sh("test -d /srv/musteri/_removed/%s && echo y || true" % rid).strip():
    fail("wrong confirm: %d" % code)
t0 = time.time()
code, loc, _ = web("/removed/" + rid, {"csrf": tok, "confirm": PURGE})
m2 = re.match(r"^/jobs/([A-Za-z0-9]+)$", loc)
if code != 303 or not m2:
    fail("purge: %d %s" % (code, loc))
j = wait_job(m2.group(1))
measure["purge_s"] = round(time.time() - t0, 1)
steps = "\n".join(j.get("steps") or [])
if j.get("error") or "Kopya hazır" not in steps or "veritabanı ve kullanıcıları silindi" not in steps or \
        "İmaj bırakıldı (çalışan bir site de kullanıyor)" not in steps:
    fail("purge job: %s\n%s" % (j.get("error"), steps))
if sh("test -e /srv/musteri/_removed/%s && echo y || true" % rid).strip():
    fail("folder still there")
if mongo_eval("print(db.getMongo().getDBNames().includes('%s') + ' ' + db.getSiblingDB('%s').getUsers().users.length)" % (PURGE, PURGE)) != "false 0":
    fail("database or user still there")
if not sh("ls /var/lib/footops/snapshots/%s/*/manifest.json 2>/dev/null || true" % PURGE).strip():
    fail("no last copy")
if '"site.purge"' not in sh("tail -n 3 /var/lib/footops/audit.jsonl"):
    fail("purge not audited")
ok("kaldırılan site kalıcı silindi (%.1f sn): son kopya alındı, veritabanı + kullanıcı + klasör gitti, çalışan sitenin imajı kaldı" % measure["purge_s"])

token_nowhere()
ok("bot token'ı hiçbir günlükte, işte, denetimde yok")

# ---- leave Telegram as it was ----
api("POST", "/v1/notify/telegram/remove", {"actor": ACTOR})
sh("systemctl stop it-tgfake; rm -f /etc/systemd/system/footops-agent.service.d/it-telegram.conf; systemctl daemon-reload; "
   "systemctl restart footops-agent", check=False)
agent_up()
measure["agent_rss_mb"] = round(rss("footops agent") / 2**20, 1)
measure["web_rss_mb"] = round(rss("footops web") / 2**20, 1)
ok("sahte Telegram ve drop-in kaldırıldı")
print(json.dumps(measure))
