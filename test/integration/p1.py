#!/usr/bin/env python3
"""P1 scenarios (end dates and parking, Faz P1b) against a real install, after f0.py
(its *.it.example.com certificate): a small site gets an end date and is parked from
the panel — its domain answers the maintenance page from Caddy itself (503,
Retry-After), its container is stopped but kept, there is no "down" alert, the status
file says "parked", what would start the container is refused, an agent restart leaves
it parked — and is brought back. From the stand-in Telegram (tgfake.py): /siteler lists
it with its end date; [+1 ay] asks once more and extends; [Parka al] asks, parks it and
tells in the question's place how it ended, with [+1 yıl ile aç], which brings it back.
What came from Telegram is in the audit log under the person who linked the chat.

Usage: p1.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import json
import os
import re
import sys
import time

from itlib import ACTOR, api, fail, fresh_totp, ok, rss, sh, wait, wait_idle, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
TOKEN = "123456789:AAH-it_secret-part-0123456789abcdefgh"
TG_PORT = 9811
PARK, PARK_DOMAIN = "itpark", "park.it.example.com"  # covered by f0's *.it.example.com certificate
JAR = "/tmp/p1-jar"
measure = {}


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def web(path, form=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar (a POST
    when there is a form). Returns (status, location, body)."""
    args = "".join(" --data-urlencode %s" % quote("%s=%s" % kv) for kv in (form or {}).items())
    out = sh("curl -sS -b %s -c %s -o /tmp/p1-page -w '%%{http_code} %%{redirect_url}' %s 'http://localhost:7701%s'; "
             "echo; cat /tmp/p1-page" % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, loc = head.split(" ", 1)
    return int(code), loc.replace("http://localhost:7701", ""), body


def csrf(page):
    m = re.search(r'name="csrf" value="([^"]+)"', page)
    if not m:
        fail("no csrf token in page")
    return m.group(1)


def visit():
    """The site's domain through this server's Caddy, as Cloudflare would ask it:
    (status, headers, body, seconds)."""
    out = sh("curl -sk --max-time 10 --resolve %s:443:127.0.0.1 -D /tmp/p1-head -o /tmp/p1-body -w '%%{http_code} %%{time_total}' "
             "https://%s/; echo; cat /tmp/p1-head; echo '----'; cat /tmp/p1-body" % (PARK_DOMAIN, PARK_DOMAIN), check=False)
    first, _, rest = out.partition("\n")
    head, _, body = rest.partition("----\n")
    code, took = first.split()
    return int(code), head.lower(), body, float(took)


def meta():
    return json.loads(sh("cat /srv/musteri/%s/footops.json" % PARK))


def running():
    return sh("docker inspect -f '{{.State.Running}}' %s 2>/dev/null || echo gone" % PARK).strip()


def agent_up():
    wait(lambda: sh("test -S /run/footops/agent.sock && curl -s --unix-socket /run/footops/agent.sock http://agent/v1/notify "
                    ">/dev/null && echo y", check=False).strip() == "y", "agent socket", timeout=60)


def alert_keys():
    _, al = api("GET", "/v1/alerts")
    return {a["key"] for a in al}


def sent():
    out = sh("cat /tmp/tg-sent.jsonl 2>/dev/null || true")
    return [json.loads(l) for l in out.splitlines() if l.strip()]


def calls(method, since):
    return [m for m in sent()[since:] if m.get("_method") == method]


def tg_add(*ups):
    """Appends updates to the stand-in's queue, numbered after the last one."""
    cur = json.loads(sh("cat /tmp/tg-updates.json"))
    last = max([u["update_id"] for u in cur] or [0])
    for i, u in enumerate(ups):
        u["update_id"] = last + 1 + i
    sh("cat > /tmp/tg-updates.new && mv /tmp/tg-updates.new /tmp/tg-updates.json", stdin=json.dumps(cur + list(ups)).encode())


def tg_msg(text):
    return {"message": {"message_id": 50, "date": int(time.time()), "text": text,
                        "chat": {"id": 555, "type": "private", "first_name": "Deneme"},
                        "from": {"id": 555, "is_bot": False, "first_name": "Deneme", "username": "deneme"}}}


def tg_press(msg_id, data):
    tg_add({"callback_query": {"id": "cb-%d" % time.time_ns(), "data": data, "from": {"id": 555, "is_bot": False, "first_name": "Deneme"},
                               "message": {"message_id": int(msg_id), "date": 1, "chat": {"id": 555, "type": "private"}}}})


def buttons(m):
    return [b for row in json.loads(m.get("reply_markup") or '{"inline_keyboard": []}')["inline_keyboard"] for b in row]


def button(m, label):
    for b in buttons(m):
        if b["text"] == label:
            return b["callback_data"]
    fail("no button %r under %r" % (label, m.get("text")))


def await_sent(contains, since, what, timeout=60):
    wait(lambda: any(contains in m.get("text", "") for m in calls("sendMessage", since)), what, timeout=timeout)
    return [m for m in calls("sendMessage", since) if contains in m.get("text", "")][-1]


def await_edit(contains, since, what, timeout=120):
    wait(lambda: any(contains in m.get("text", "") for m in calls("editMessageText", since)), what, timeout=timeout)
    return [m for m in calls("editMessageText", since) if contains in m.get("text", "")][-1]


def set_expiry(day):
    api("POST", "/v1/sites/%s/expiry" % PARK, {"expires": day, "actor": ACTOR})


print("P1 senaryoları")
agent_up()
# y2 / y4's S3 server, if there is one: the status file, and the definition's backup after
# a park — restic runs in a tool container, which the firewall keeps from the server's
# own ports (y2 opens 7070 the same way; the rule does not outlive a restart).
sh("docker start it-s3 >/dev/null 2>&1 || true", check=False)
sh("iptables -C INPUT -p tcp --dport 7070 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 7070 -j ACCEPT")
code_ = json.loads(sh("cat /etc/footops/footops.json")).get("server_code", "")
PREFIX = "[%s]" % code_.upper()
today = sh("date +%Y-%m-%d").strip()
yesterday = sh("date -d yesterday +%Y-%m-%d").strip()
hour = int(sh("date +%H").strip())

# ---- the stand-in Telegram (as y4), linked to chat 555 by user 555 ----
sh("cat > /root/tgfake.py", stdin=open(os.path.join(os.path.dirname(__file__), "tgfake.py"), "rb").read())
sh("systemctl stop it-tgfake 2>/dev/null; rm -f /tmp/tg-sent.jsonl /tmp/tg-updates.json; echo '[]' > /tmp/tg-updates.json", check=False)
sh("systemd-run --unit it-tgfake --property PrivateTmp=no python3 /root/tgfake.py %d %s >/dev/null" % (TG_PORT, TOKEN))
wait(lambda: sh("curl -s -o /dev/null -w %%{http_code} -X POST http://127.0.0.1:%d/bot%s/getMe" % (TG_PORT, TOKEN), check=False).strip() == "200",
     "the stand-in Telegram", timeout=30)
sh("mkdir -p /etc/systemd/system/footops-agent.service.d && printf '[Service]\\nEnvironment=FOOTOPS_TELEGRAM_API=http://127.0.0.1:%d\\n' "
   "> /etc/systemd/system/footops-agent.service.d/it-telegram.conf && systemctl daemon-reload && systemctl restart footops-agent" % TG_PORT)
agent_up()
api("POST", "/v1/notify/telegram/remove", {"actor": ACTOR})
_, link = api("POST", "/v1/notify/telegram", {"token": TOKEN, "actor": ACTOR})
sh("cat > /tmp/tg-updates.json", stdin=json.dumps([{"update_id": 1, "message": {
    "message_id": 1, "date": int(time.time()), "text": "/start " + link["code"],
    "chat": {"id": 555, "type": "private", "first_name": "Deneme", "username": "deneme"},
    "from": {"id": 555, "is_bot": False, "first_name": "Deneme", "username": "deneme"}}}]).encode())
api("POST", "/v1/notify/telegram/link", {"actor": ACTOR})
wait(lambda: (api("GET", "/v1/notify")[1].get("commands") or {}).get("reading"), "the reader", timeout=90)
ok("sahte Telegram bağlı, okuyucu dinliyor")

# ---- a small site to park ----
wait_idle()
if sh("test -e /srv/musteri/%s && echo y || true" % PARK).strip():
    _, r = api("POST", "/v1/sites/%s/remove" % PARK, {"actor": ACTOR})  # left by a run cut short
    wait_job(r["job_id"])
_, r = api("POST", "/v1/sites", {"spec": {"name": PARK, "group": "musteri", "domains": [PARK_DOMAIN],
                                          "image": "nginxinc/nginx-unprivileged:1.27-alpine", "port": 8080, "memory_mb": 64},
                                 "env": {}, "actor": ACTOR})
if wait_job(r["job_id"]).get("error"):
    fail("site create")
code, head, body, _ = visit()
if code != 200 or "nginx" not in body.lower():
    fail("the site before parking: %d" % code)
ok("site %s yayında (%s → 200)" % (PARK, PARK_DOMAIN))

# ---- the panel: an end date ----
user, pw, totp_secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(totp_secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
code, _, page = web("/sites/" + PARK)
tok = csrf(page)
if code != 200 or 'id="sure"' not in page or "Süresiz" not in page or 'href="/sites/%s/park"' % PARK not in page:
    fail("site page: %d" % code)
mark = len(sent())
code, loc, _ = web("/sites/%s/expiry" % PARK, {"csrf": tok, "act": "set", "expires": yesterday})
if code != 303 or meta().get("expires") != yesterday:
    fail("end date: %d %s %s" % (code, loc, meta().get("expires")))
_, _, page = web("/sites")
if 'badge bad">1 gün önce doldu' not in page:
    fail("the list does not show the end date")
wait(lambda: "expiry:" + PARK in alert_keys(), "the end date's alert", timeout=60)
if hour >= 10:
    m = await_sent("%s sitesinin süresi 1 gün önce doldu" % PARK, mark, "the day's question", timeout=30)
    if [b["text"] for b in buttons(m)] != ["⏸ Parka al", "+1 ay", "+1 yıl"]:
        fail("question's buttons: %s" % buttons(m))
    ok("bitiş tarihi panelden verildi; listede '1 gün önce doldu', uyarı açıldı, Telegram'a parka alma sorusu geldi (düğmeli)")
else:
    ok("bitiş tarihi panelden verildi; listede '1 gün önce doldu', uyarı açıldı (saat %02d: hatırlatma 10:00'da, burada denenmedi)" % hour)
expiry_alert = [a for a in api("GET", "/v1/alerts")[1] if a["key"] == "expiry:" + PARK]
if expiry_alert[0]["level"] != "warn" or any("süresi" in m.get("text", "") and m.get("text", "").startswith("🟡") for m in sent()[mark:]):
    fail("the end date's alert went to Telegram as an alert too")

# ---- the panel: park ----
code, _, page = web("/sites/%s/park" % PARK)
if code != 200 or "parka alınsın mı?" not in page:
    fail("park confirm: %d" % code)
wait_idle()
t0 = time.time()
code, loc, _ = web("/sites/%s/park" % PARK, {"csrf": tok})
m2 = re.match(r"^/jobs/([A-Za-z0-9]+)$", loc)
if code != 303 or not m2:
    fail("park: %d %s" % (code, loc))
j = wait_job(m2.group(1))
measure["park_s"] = round(time.time() - t0, 1)
steps = "\n".join(j.get("steps") or [])
if j.get("error") or "bakım sayfasını veriyor" not in steps or "Container durduruldu" not in steps:
    fail("park job: %s\n%s" % (j.get("error"), steps))
code, head, body, took = visit()
measure["park_page_ms"] = round(took * 1000)
measure["park_page_bytes"] = len(body.encode())
if code != 503 or "<title>Bakım çalışması</title>" not in body or "Bu site şu anda bakımda. Kısa süre içinde yeniden yayında olacak." not in body or \
        "retry-after: 3600" not in head or "cache-control: no-store" not in head or "content-type: text/html; charset=utf-8" not in head:
    fail("park page: %d\n%s\n%s" % (code, head, body[:300]))
if "footops" in body.lower() or "nginx" in body.lower():
    fail("the park page tells what serves it")
if running() != "false":
    fail("container: %s" % running())
if not meta().get("parked", {}).get("at") or meta()["parked"].get("by") != user:  # the panel's user
    fail("record: %s" % meta())
ok("panelden parka alındı (%.1f sn): %s → 503 bakım sayfası (Retry-After 3600, %d bayt, %d ms), container durdu ama silinmedi"
   % (measure["park_s"], PARK_DOMAIN, measure["park_page_bytes"], measure["park_page_ms"]))
time.sleep(35)  # the alerts' hold (30 s) and a sample
keys = alert_keys()
if "down:" + PARK in keys or "expiry:" + PARK in keys:
    fail("alerts for a parked site: %s" % keys)
_, _, page = web("/")
if 'badge warn">parkta' not in page or 'href="/containers/%s/restart"' % PARK in page:
    fail("dashboard")
_, _, page = web("/sites/" + PARK)
if "Bu site parkta" not in page or 'href="/sites/%s/unpark"' % PARK not in page or "/redeploy\"" in page:
    fail("parked site page")
ok("parktayken uyarı yok ('çalışmıyor' de 'süresi doldu' da), panoda ve site sayfasında 'parkta'")


def status_site():
    sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json 2>/dev/null || echo '{}'"))
    if not sec.get("repository"):
        return None
    path = sec["repository"].split("://", 1)[1].split("/", 1)[1].strip("/")
    out = sh("docker run --rm -v it-s3:/veri mongo:7.0.45 cat /veri/%s/izleme/durum.json 2>/dev/null || true" % path, check=False)
    if not out.strip():
        return None
    return {s["name"]: s for s in json.loads(out)["sites"]}.get(PARK)


if sh("docker inspect -f '{{.State.Running}}' it-s3 2>/dev/null || true").strip() == "true" and \
        sh("test -e /etc/footops/secrets/backup/repository.json && echo y || true").strip():
    # A site job writes the status file at once (and the park's end did).
    wait(lambda: (status_site() or {}).get("state") == "parked", "'parked' in the status file", timeout=60)
    # The park is followed by a backup of the site's definition alone.
    wait_idle()
    defs = [json.loads(l) for l in sh("tail -n 30 /var/lib/footops/audit.jsonl").splitlines() if l.strip()]
    if not any(a["action"] == "backup.run" and a.get("target") == PARK and a["actor"].get("via") == "park" and a.get("ok") for a in defs):
        fail("no backup of the definition after the park")
    ok("durum dosyasında state: parked (nöbetçi atlıyor); parktan sonra site tanımı yedeklendi")
else:
    ok("durum dosyası ve tanım yedeği denenmedi: yedek deposu yok (y2 / y4 kurar)")

# What would start the container is refused; a restart leaves the site parked.
wait_idle()
code, out = api("POST", "/v1/containers/%s/restart" % PARK, {"actor": ACTOR}, check=False)
if code != 409 or "parkta" not in out.get("error", ""):
    fail("restart of a parked site: %d %s" % (code, out))
_, r = api("POST", "/v1/sites/%s/redeploy" % PARK, {"actor": ACTOR})
if "parkta" not in (wait_job(r["job_id"]).get("error") or ""):
    fail("redeploy of a parked site")
sh("systemctl restart footops-agent")
agent_up()
time.sleep(5)  # Caddy's blocks are brought up to date once Caddy runs
if visit()[0] != 503 or running() != "false":
    fail("after an agent restart: %d %s" % (visit()[0], running()))
ok("parktayken yeniden başlatma ve yeniden dağıtma reddedildi; agent yeniden başlayınca site parkta kaldı")

# Back from the panel, with no end date.
wait(lambda: (api("GET", "/v1/notify")[1].get("commands") or {}).get("reading"), "the reader", timeout=90)
code, _, page = web("/sites/%s/unpark" % PARK)
if code != 200 or "yeniden yayına alınsın mı?" not in page or 'value="year" checked' not in page:
    fail("unpark form: %d" % code)
wait_idle()
t0 = time.time()
code, loc, _ = web("/sites/%s/unpark" % PARK, {"csrf": tok, "choice": "none"})
m2 = re.match(r"^/jobs/([A-Za-z0-9]+)$", loc)
if code != 303 or not m2 or wait_job(m2.group(1)).get("error"):
    fail("unpark: %d %s" % (code, loc))
measure["unpark_s"] = round(time.time() - t0, 1)
if visit()[0] != 200 or running() != "true" or "parked" in meta() or meta().get("expires"):
    fail("after unpark: %d %s %s" % (visit()[0], running(), meta()))
ok("panelden süresiz yayına alındı (%.1f sn): 200, container çalışıyor" % measure["unpark_s"])

# ---- from Telegram: /siteler, [+1 ay], [Parka al], [+1 yıl ile aç] ----
rss_before = rss("footops agent")
set_expiry(yesterday)
mark = len(sent())
tg_add(tg_msg("/siteler"))
lst = await_sent("Süreli ve parktaki siteler", mark, "the /siteler answer")
if "⌛ %s — süresi 1 gün önce doldu" % PARK not in lst["text"]:
    fail("/siteler: %s" % lst["text"])
mark = len(sent())
tg_press(lst["_message_id"], button(lst, "%s: +1 ay" % PARK))
q = await_sent("%s sitesinin süresi uzatılsın mı?" % PARK, mark, "the extension's question")
new = re.search(r"yeni bitiş: (\d\d)\.(\d\d)\.(\d{4})", q["text"])
mark = len(sent())
tg_press(q["_message_id"], button(q, "✅ Evet, uzat"))
await_edit("✅ %s %s: yeni bitiş" % (PREFIX, PARK), mark, "the extension's result")
if meta().get("expires") != "%s-%s-%s" % (new.group(3), new.group(2), new.group(1)):
    fail("extended to %s, the question said %s" % (meta().get("expires"), new.group(0)))
ok("Telegram /siteler: süresi dolan site listede; [+1 ay] bir kez daha sordu, [Evet] sorudaki tarihe uzattı (%s)" % meta()["expires"])

set_expiry(yesterday)
mark = len(sent())
tg_add(tg_msg("/siteler"))
lst = await_sent("Süreli ve parktaki siteler", mark, "the /siteler answer")
mark = len(sent())
tg_press(lst["_message_id"], button(lst, "⏸ %s: parka al" % PARK))
q = await_sent("%s parka alınsın mı?" % PARK, mark, "the park question")
wait_idle()
mark = len(sent())
t0 = time.time()
tg_press(q["_message_id"], button(q, "⏸ Evet, parka al"))
await_edit("%s parka alınıyor…" % PARK, mark, "the park under way")
done = await_edit("%s %s parka alındı" % (PREFIX, PARK), mark, "the park's result")
measure["telegram_park_s"] = round(time.time() - t0, 1)
if visit()[0] != 503 or running() != "false" or done["message_id"] != str(q["_message_id"]):
    fail("parked from Telegram: %d %s %s" % (visit()[0], running(), done))
mark = len(sent())
tg_press(q["_message_id"], button(done, "▶ +1 yıl ile aç"))
q2 = await_sent("%s yeniden yayına alınsın mı?" % PARK, mark, "the unpark question")
new = re.search(r"yeni bitiş (\d\d)\.(\d\d)\.(\d{4})", q2["text"])
wait_idle()
mark = len(sent())
tg_press(q2["_message_id"], button(q2, "▶ Evet, yayına al"))
await_edit("%s %s yeniden yayında" % (PREFIX, PARK), mark, "the unpark's result")
if visit()[0] != 200 or running() != "true" or meta().get("expires") != "%s-%s-%s" % (new.group(3), new.group(2), new.group(1)):
    fail("back from Telegram: %d %s %s" % (visit()[0], running(), meta()))
measure["agent_rss_delta_kb"] = round((rss("footops agent") - rss_before) / 1024)
audit = sh("tail -n 40 /var/lib/footops/audit.jsonl")
for act in ("site.park", "site.unpark", "site.expiry"):
    if not any(json.loads(l)["action"] == act and json.loads(l)["actor"].get("via") == "telegram" and
               json.loads(l)["actor"].get("user") == "Deneme (@deneme)" for l in audit.splitlines() if l.strip()):
        fail("%s from Telegram not audited" % act)
ok("Telegram'dan parka alındı (%.1f sn; sonuç sorunun yerinde) ve [+1 yıl ile aç] ile geri açıldı; hepsi denetimde 'Deneme (@deneme)' adıyla"
   % measure["telegram_park_s"])

# ---- leave things as they were ----
wait_idle()
_, r = api("POST", "/v1/sites/%s/remove" % PARK, {"actor": ACTOR})
wait_job(r["job_id"])
for rs in api("GET", "/v1/removed")[1] or []:
    if rs["site"] == PARK:
        _, r = api("POST", "/v1/removed/%s/purge" % rs["id"], {"confirm": PARK, "actor": ACTOR})
        wait_job(r["job_id"])
api("POST", "/v1/notify/telegram/remove", {"actor": ACTOR})
sh("systemctl stop it-tgfake; rm -f /etc/systemd/system/footops-agent.service.d/it-telegram.conf; systemctl daemon-reload; "
   "systemctl restart footops-agent", check=False)
agent_up()
measure["agent_rss_mb"] = round(rss("footops agent") / 2**20, 1)
ok("site kaldırıldı, sahte Telegram ve drop-in kaldırıldı")
print(json.dumps(measure))
