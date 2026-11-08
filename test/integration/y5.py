#!/usr/bin/env python3
"""Y5 scenarios against a real install, after y1-y4 (whose repository it uses; one is set
up here if there is none): the monthly trial restore on the test container, then a second
container installed in recovery mode from the test container's backups — the server's
part and every site come back (database counts, files with owners and checksums, the
panel's old login), it is a rehearsal (its sites cannot reach out, no repository or bot
can be set up, nothing is written to the repository) — and go-live with the source's
agent stopped (the old server off): the repository and the sites' outside reach come
back and the copy takes the next backup.

Usage: y5.py <container> <login-file>
Prints measurements as JSON at the end; exits non-zero on the first failed check.
"""
import base64
import json
import os
import re
import subprocess
import sys
import time

from itlib import ACTOR, api, drive, fail, fresh_totp, ok, rss, sh, wait, wait_idle, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
WORK = os.path.dirname(os.path.abspath(LOGIN))
C2 = C + "-dr"
IMAGE = "footops-it:ubuntu24.04"
VERSITY = "versity/versitygw:v1.10.0@sha256:89cad13cc981986ee284cbafea4613f9533ce6fd5f37a2c96c4c4fb8adb3a316"
KEY, SECRET, BUCKET = "itdeneme01", "it-deneme-gizli-0123456789", "footops-it"
TRIAL_SITE = "itbak"
measure = {}


def cli(args, c=None, stdin="", check=True):
    p = subprocess.run(["docker", "exec", "-i", c or C, "/usr/local/bin/footops"] + args, input=stdin.encode(), capture_output=True)
    out = p.stdout.decode(errors="replace") + p.stderr.decode(errors="replace")
    if check and p.returncode != 0:
        fail("footops %s (%s): %d\n%s" % (" ".join(args), c or C, p.returncode, out))
    return p.returncode, out


def root_counts(db, c=None):
    pw = re.search(r"MONGO_INITDB_ROOT_PASSWORD=(\S+)", sh("cat /etc/footops/secrets/mongo.env", c=c)).group(1)
    js = ('const c=new Mongo("mongodb://127.0.0.1:27017");c.getDB("admin").auth("root",%s);const d=c.getDB("%s");const o={};'
          'd.getCollectionNames().filter(n=>!n.startsWith("system.")).forEach(n=>o[n]=d.getCollection(n).countDocuments({}));'
          'print("C "+JSON.stringify(o));' % (json.dumps(pw), db))
    out = sh("docker exec -i footops-mongo mongosh --quiet --file /dev/stdin", stdin=js.encode(), c=c)
    return json.loads(re.search(r"C (\{.*\})", out).group(1))


def tree(path, c=None):
    """Type, owner, mode, size and SHA-256 of everything below path."""
    out = sh("cd %s && find . -mindepth 1 -printf '%%y %%U:%%G %%m %%s %%P\\n' | sort" % path, c=c)
    sums = sh("cd %s && find . -type f -print0 | sort -z | xargs -0 -r sha256sum" % path, c=c)
    return out, sums


def restic(args):
    """restic itself on the repository, from the source container (the test's own eyes)."""
    return sh("docker run --rm --network host -v /root/y5cfg:/run/footops:ro "
              "-v /var/lib/footops/tools/restic-0.19.1:/usr/local/bin/restic:ro --tmpfs /tmp "
              "-e RESTIC_REPOSITORY_FILE=/run/footops/repository -e RESTIC_PASSWORD_FILE=/run/footops/password "
              "-e AWS_SHARED_CREDENTIALS_FILE=/run/footops/s3 --entrypoint restic mongo:7.0.45 --no-cache --no-lock %s" % args)


def quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def reach(c, network, ip, port):
    """Whether a container on network (in container c's Docker) can open ip:port."""
    out = sh("docker run --rm --network %s --entrypoint bash mongo:7.0.45 -c "
             "'timeout 5 bash -c \"echo > /dev/tcp/%s/%d\" 2>/dev/null && echo ACIK || echo KAPALI'" % (network, ip, port), c=c)
    return out.strip().endswith("ACIK")


RULE = "DOCKER-USER -i br-+ ! -o br-+ -m conntrack --ctstate NEW -m comment --comment footops-prova -j REJECT"


def has_rule(c):
    return sh("iptables -w -C %s 2>/dev/null && echo y || true" % RULE, c=c).strip() == "y"


def https_code(domain, c):
    code = sh("curl -sk -o /dev/null -w '%%{http_code}' --resolve %s:443:127.0.0.1 https://%s/" % (domain, domain), c=c, check=False).strip()
    return int(code) if code.isdigit() else 0


def sites_up(specs, c):
    return all(0 < https_code(sp["domains"][0], c) < 500 for sp in specs)


def no_secret_in_log(c, secrets, what):
    """The installer's log (the recovery's and go-live's commands) holds none of secrets."""
    log = sh("cat /var/log/footops-install.log", c=c)
    if any(s in log for s in secrets):
        fail("a secret in %s's install log (%s)" % (c, what))


print("Y5 senaryoları")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
sh("systemctl start footops-agent", check=False)
wait_idle()

# ---- the source: a repository, a full backup, the trial ----
if not sh("test -e /etc/footops/secrets/backup/repository.json && echo y || true").strip():
    if sh("docker inspect -f '{{.State.Running}}' it-s3 2>/dev/null || true").strip() != "true":
        sh("docker rm -f it-s3 >/dev/null 2>&1; docker volume create it-s3 >/dev/null", check=False)
        sh("docker run --rm -v it-s3:/veri --entrypoint mkdir mongo:7.0.45 -p /veri/%s" % BUCKET)
        sh("docker run -d --name it-s3 --network host -v it-s3:/veri -e ROOT_ACCESS_KEY_ID=%s -e ROOT_SECRET_ACCESS_KEY=%s "
           "-e VGW_REGION=auto %s posix /veri >/dev/null" % (KEY, SECRET, VERSITY))
    host = sh("hostname -I").split()[0]
    _, r = api("POST", "/v1/backup/setup", {"repository": "s3:http://%s:7070/%s/y5-%d" % (host, BUCKET, time.time()),
                                            "key_id": KEY, "secret_key": SECRET, "actor": ACTOR})
    if wait_job(r["job_id"]).get("error"):
        fail("repository setup")
    api("POST", "/v1/backup/confirm", {"actor": ACTOR})
sh("docker start it-s3 >/dev/null 2>&1 || true", check=False)
sh("iptables -C INPUT -p tcp --dport 7070 -j ACCEPT 2>/dev/null || iptables -I INPUT 1 -p tcp --dport 7070 -j ACCEPT")
src_ip = sh("hostname -I").split()[0]
sec = json.loads(sh("cat /etc/footops/secrets/backup/repository.json"))
sh("mkdir -p /root/y5cfg && chmod 700 /root/y5cfg")
sh("cat > /root/y5cfg/repository", stdin=sec["repository"].encode())
sh("cat > /root/y5cfg/password", stdin=sec["password"].encode())
sh("cat > /root/y5cfg/s3", stdin=("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n" % (sec["key_id"], sec["secret_key"])).encode())
cli(["backup", "set", "paused=off"])
# A Telegram link for the copy to take over at go-live (made up: the source's own was
# removed by y4); it goes again once the backup has it.
TG = '{"token":"123456789:Y5-made-up-token","bot":"y5bot","chat_id":555,"chat":"y5","linked":"2026-11-08T00:00:00Z"}\n'
sh("umask 077 && cat > /etc/footops/secrets/notify/telegram.json", stdin=TG.encode())
t0 = time.time()
cli(["backup", "run"])
sh("rm -f /etc/footops/secrets/notify/telegram.json")
ok("kaynakta tam yedek (%.1f sn)" % (time.time() - t0))

t0 = time.time()
_, out = cli(["backup", "trial", TRIAL_SITE])
measure["trial_s"] = round(time.time() - t0, 1)
if "Deneme geri yüklemesi temiz" not in out:
    fail("trial:\n" + out)
_, bs = api("GET", "/v1/backup")
if not bs.get("trial", {}).get("ok") or bs["trial"]["site"] != TRIAL_SITE:
    fail("trial state: %s" % bs.get("trial"))
ok("aylık deneme geri yüklemesi (%s): %s (%.1f sn)" % (TRIAL_SITE, bs["trial"]["detail"], measure["trial_s"]))
_, alerts = api("GET", "/v1/alerts")
if any(a["key"] == "backup:trial" for a in alerts):
    fail("trial alert after a clean trial")

# Nothing more goes to the repository while the copy reads it.
cli(["backup", "set", "paused=on"])
_, sites = api("GET", "/v1/sites")
names = sorted(s["spec"]["name"] for s in sites)
want = {}
for s in sites:
    sp = s["spec"]
    want[sp["name"]] = {"domains": sp["domains"], "db": root_counts(sp["name"]) if sp.get("database") else None,
                        "files": tree("/srv/%s/%s/data" % (sp["group"], sp["name"])) if sp.get("data_mount") else None}
snaps_before = len(json.loads(restic("snapshots --json")))
locks_before = restic("list locks").split()
ok("kaynak: %d site (%s), depoda %d anlık görüntü" % (len(names), ", ".join(names), snaps_before))

# ---- the copy: a new container, installed from the backups ----
subprocess.run(["docker", "rm", "-f", C2], capture_output=True)
subprocess.run(["docker", "volume", "rm", C2 + "-docker", C2 + "-containerd"], capture_output=True)
subprocess.run(["docker", "run", "-d", "--name", C2, "--hostname", "fo-dr", "--privileged", "--tmpfs", "/run", "--tmpfs", "/run/lock",
                "-v", C2 + "-docker:/var/lib/docker", "-v", C2 + "-containerd:/var/lib/containerd", IMAGE], check=True, capture_output=True)
wait(lambda: subprocess.run(["docker", "exec", C2, "systemctl", "is-system-running"], capture_output=True).stdout.decode().strip()
     in ("running", "degraded"), "the copy's systemd", timeout=120)
subprocess.run(["docker", "cp", os.path.join(WORK, "footops"), C2 + ":/root/footops"], check=True)
pubkey = open(os.path.join(WORK, "id_it.pub")).read().strip()
line = "FOOTOPS-KURTARMA-1:" + base64.b64encode(json.dumps({"repository": sec["repository"], "key_id": sec["key_id"],
                                                             "secret_key": sec["secret_key"], "password": sec["password"]}).encode()).decode()
answers = [
    ("Bu cevaplarla devam edilsin mi?", "e"),
    ("Bu sunucu bir yedekten mi kurulacak?", "e"),
    ("Eski sunucunun kodu", "IT"),
    ("Hostname", "fo-dr"),
    ("Saat dilimi", ""),
    ("Kullanıcı adı", "furkan"),
    ("anahtarları yönetici kullanıcıya da eklensin mi?", "e"),
    ("SSH public key", pubkey + "\n"),
    ("sudo şifre istesin mi?", "h"),
    ("(önerilen)", ""),
    ("Panel adresi", "panel-it.example.com"),
    ("otomatik yeniden başlatılsın mı?", "h"),
    ("Kurtarma bilgisi", line),
    ("Hangi zamana dönülsün?", ""),
    ("Bu ayarlarla kuruluma başlansın mı?", "e"),
    ("Yeni kullanıcıyla giriş ve sudo çalıştı mı?", "e"),
]
t0 = time.time()
# A pull from Docker Hub hangs now and then (seen at 03:15: a layer never came): the
# installer then goes on where it stopped, answering the recovery line again.
for attempt in (1, 2):
    code, out = drive(C2, ["/root/footops", "install"], answers, os.path.join(WORK, "install-dr-%d.log" % attempt), timeout=1500, fatal=False)
    if code != 124:
        break
    print("  ↻ kurulum takıldı (%d. deneme), kaldığı yerden yeniden" % attempt)
measure["install_and_recover_s"] = round(time.time() - t0)
subprocess.run(["docker", "exec", C2, "rm", "-f", "/root/footops"])
if code != 0:
    fail("recovery install: exit %d (see install-dr-*.log)" % code)
for secret in (sec["secret_key"], sec["password"], line[30:60]):
    if secret in out:
        fail("a secret in the installer's output")
if "6 haneli kod" in out or "Panel girişi — kullanıcı" in out:
    fail("a recovery made a panel account of its own")
if "Sunucu PROVA KİPİNDE" not in out or "sudo footops go-live" not in out:
    fail("no rehearsal notes:\n" + out[-3000:])
ok("kopya yedekten kuruldu (kurulum + kurtarma %d sn)" % measure["install_and_recover_s"])

# ---- what came back ----
_, st = api("GET", "/v1/recover", c=C2)
state = st.get("state") or {}
if not st.get("rehearsal") or not state.get("finished") or state.get("error") or not state.get("server_done"):
    fail("recover state: %s" % json.dumps(st)[:2000])
_, job = api("GET", "/v1/jobs/" + state["job"], c=C2)
started = time.mktime(time.strptime(job["started"][:19], "%Y-%m-%dT%H:%M:%S"))
finished = time.mktime(time.strptime(job["finished"][:19], "%Y-%m-%dT%H:%M:%S"))
measure["recover_job_s"] = int(finished - started)
measure["recover_job_peak_mb"] = round(job["usage"].get("peak_mem", 0) / 1e6, 1)
for s in state["sites"]:
    if s["state"] not in ("ok", "warn"):
        fail("site %s: %s" % (s["name"], s))
_, sites2 = api("GET", "/v1/sites", c=C2)
got = {s["spec"]["name"]: s["spec"] for s in sites2}
if sorted(got) != names:
    fail("sites: %s, want %s" % (sorted(got), names))
for n, w in want.items():
    sp = got[n]
    if sp["domains"] != w["domains"]:
        fail("%s domains %s" % (n, sp["domains"]))
    if w["db"] is not None and root_counts(n, c=C2) != w["db"]:
        fail("%s database %s, want %s" % (n, root_counts(n, c=C2), w["db"]))
    if w["files"] is not None and tree("/srv/%s/%s/data" % (sp["group"], n), c=C2) != w["files"]:
        fail("%s files differ" % n)
    code = https_code(sp["domains"][0], C2)
    if not 0 < code < 500:
        fail("%s over HTTPS: %s" % (n, code))
ok("siteler geri geldi: %s — veritabanı sayımları, dosyalar (sahip, izin, sha256) ve HTTPS kaynakla aynı" % ", ".join(names))
certs = api("GET", "/v1/certs", c=C2)[1]
if not certs:
    fail("no certificates")
_, ss = api("GET", "/v1/backup/settings", c=C2)
_, src_settings = api("GET", "/v1/backup/settings")
if ss.get("sites") != src_settings.get("sites") or ss.get("night") != src_settings.get("night"):
    fail("backup settings: %s / %s" % (ss, src_settings))
ok("sunucu ayarları geri geldi: %d sertifika, yedek ayarları" % len(certs))

# The panel takes the old login (password + 2FA).
user, pw, totp_secret = open(LOGIN).read().split()
jar = "/tmp/y5-jar"
page = sh("curl -sS -c %s http://localhost:7701/login" % jar, c=C2)
token = re.search(r'name="csrf" value="([^"]+)"', page).group(1)
form = {"csrf": token, "username": user, "password": pw, "code": fresh_totp(totp_secret)}
res = sh("curl -sS -b %s -c %s -o /dev/null -w '%%{http_code} %%{redirect_url}' %s http://localhost:7701/login"
         % (jar, jar, " ".join("--data-urlencode " + quote("%s=%s" % kv) for kv in form.items())), c=C2)
if not res.startswith("303") or not res.rstrip().endswith("/"):
    fail("old login on the copy: %s" % res)
backup_page = sh("curl -sS -b %s http://localhost:7701/backup" % jar, c=C2)
if "Prova kipi" not in backup_page or 'action="/backup/setup"' in backup_page:
    fail("backup page in rehearsal")
ok("panel eski girişle açılıyor (şifre + 2FA); Yedekleme sayfasında prova kipi")

# A rehearsal: shut off from the outside, nowhere to write.
if not has_rule(C2):
    fail("no rehearsal rule on the copy")
net = "prj_%s_%s" % (got[TRIAL_SITE]["group"], TRIAL_SITE)
if reach(C2, net, src_ip, 7070):
    fail("a site network reaches out in a rehearsal")
if not reach(C2, "bridge", src_ip, 7070):
    fail("the default bridge (restic's) cannot reach the S3 server")
ok("prova: site ağlarından dışarıya bağlantı yok, varsayılan köprüden (restic) var")
code, out = cli(["backup", "setup", "--existing"], c=C2, stdin="%s\n%s\n%s\n%s\n" % (sec["repository"], sec["key_id"], sec["secret_key"], sec["password"]),
                check=False)
if code == 0 or "prova kipinde" not in out:
    fail("backup setup in a rehearsal: %d %s" % (code, out))
code, body = api("POST", "/v1/notify/telegram", {"token": "123:abc", "actor": ACTOR}, check=False, c=C2)
if code != 422 or "prova kipinde" not in json.dumps(body, ensure_ascii=False):
    fail("telegram link in a rehearsal: %d %s" % (code, body))
_, alerts2 = api("GET", "/v1/alerts", c=C2)
keys = {a["key"] for a in alerts2}
if "rehearsal" not in keys or "backup:setup" in keys or "rehearsal:firewall" in keys:
    fail("alerts: %s" % keys)
if len(json.loads(restic("snapshots --json"))) != snaps_before or restic("list locks").split() != locks_before:
    fail("the rehearsal wrote to the repository")
measure["copy_agent_rss_mb"] = round(rss("footops agent", c=C2) / 1e6, 1)
measure["copy_web_rss_mb"] = round(rss("footops web", c=C2) / 1e6, 1)
ok("prova: depo ve Telegram bağlanmıyor, uyarı 'rehearsal'; depoya hiçbir şey yazılmadı (%d anlık görüntü, kilit yok)" % snaps_before)
secrets = (sec["secret_key"], sec["password"], line[30:60])
no_secret_in_log(C2, secrets, "recovery")
ok("kopyanın kurulum günlüğünde depo sırrı yok")

# The rule stays while the server is a rehearsal. The boot unit puts it back on its own
# (the agent, which keeps it otherwise, is stopped meanwhile)...
sh("systemctl stop footops-agent", c=C2)
sh("iptables -w -D " + RULE, c=C2)
if has_rule(C2):
    fail("the rule is still there")
sh("systemctl restart footops-prova", c=C2)
if not has_rule(C2):
    fail("footops-prova did not put the rule back")
sh("systemctl start footops-agent", c=C2)
# ...Docker's restart keeps it (it never empties DOCKER-USER): no moment without it...
specs = list(got.values())
sh("systemctl restart docker", c=C2)
if not has_rule(C2):
    fail("the rule went with Docker's restart")
wait(lambda: sites_up(specs, C2), "the sites after Docker's restart", timeout=240)
if not has_rule(C2) or reach(C2, net, src_ip, 7070):
    fail("a site network reaches out after Docker's restart")
# ...and a reboot finds it in place before Docker starts the sites: the unit ran again
# (a later time on the same monotonic clock), and before Docker began to start.
def started(c):
    return subprocess.run(["docker", "inspect", "-f", "{{.State.StartedAt}}", c], capture_output=True).stdout.decode().strip()


def unit_time(prop, unit):
    return int(sh("systemctl show -p %s --value %s" % (prop, unit), c=C2).strip() or 0)


boot_before, prova_before = started(C2), unit_time("ActiveEnterTimestampMonotonic", "footops-prova")
t0 = time.time()
subprocess.run(["docker", "restart", C2], check=True, capture_output=True)
wait(lambda: subprocess.run(["docker", "exec", C2, "systemctl", "is-system-running"], capture_output=True).stdout.decode().strip()
     in ("running", "degraded"), "the copy's systemd after a reboot", timeout=180)
if started(C2) == boot_before:
    fail("the copy did not restart")
if not has_rule(C2):
    fail("no rule after a reboot")
prova_up = unit_time("ActiveEnterTimestampMonotonic", "footops-prova")
docker_start = unit_time("InactiveExitTimestampMonotonic", "docker")
if prova_up <= prova_before or not docker_start or prova_up > docker_start:
    fail("footops-prova at boot: ran %d (before the reboot %d), Docker started %d" % (prova_up, prova_before, docker_start))
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False, c=C2).strip() == "y", "the copy's agent after a reboot")
wait(lambda: sites_up(specs, C2), "the sites after a reboot", timeout=240)
measure["reboot_sites_up_s"] = round(time.time() - t0, 1)
if reach(C2, net, src_ip, 7070) or not api("GET", "/v1/recover", c=C2)[1].get("rehearsal"):
    fail("after a reboot: not shut off, or not a rehearsal")
ok("kural yerinde kalıyor: açılış birimi geri koyuyor, Docker yeniden başlayınca duruyor, yeniden açılışta Docker'dan önce (siteler %.0f sn'de açık)"
   % measure["reboot_sites_up_s"])

# ---- go-live, the old server off ----
sh("systemctl stop footops-agent")
t0 = time.time()
code, out = drive(C2, ["/usr/local/bin/footops", "go-live"], [("sunucu kodunu yaz", "IT")], os.path.join(WORK, "golive.log"), timeout=900)
measure["go_live_s"] = round(time.time() - t0, 1)
if code != 0 or "Sunucu canlı" not in out:
    fail("go-live: %d\n%s" % (code, out[-3000:]))
_, st = api("GET", "/v1/recover", c=C2)
if st.get("rehearsal"):
    fail("still a rehearsal")
if sh("iptables -w -S DOCKER-USER | grep -c footops-prova || true", c=C2).strip() != "0":
    fail("the rule stayed")
if sh("test -e /var/lib/footops/recover/held && echo y || true", c=C2).strip():
    fail("held files stayed")
if not reach(C2, net, src_ip, 7070):
    fail("a site network cannot reach out after go-live")
_, bs2 = api("GET", "/v1/backup", c=C2)
if not bs2.get("configured") or not bs2["settings"].get("confirmed"):
    fail("backup after go-live: %s" % bs2)
wait(lambda: not api("GET", "/v1/backup", c=C2)[1].get("running"), "the first backup", timeout=600)
runs2 = api("GET", "/v1/backup/runs?limit=5", c=C2)[1]
if not runs2 or runs2[0].get("error"):
    fail("first backup from the copy: %s" % (runs2[:1]))
snaps_after = json.loads(restic("snapshots --json"))
if len(snaps_after) <= snaps_before or not all(s["hostname"] == "IT" for s in snaps_after):
    fail("snapshots after go-live: %d (hosts %s)" % (len(snaps_after), {s["hostname"] for s in snaps_after}))
if sh("cat /etc/footops/secrets/notify/telegram.json 2>/dev/null || true", c=C2) != TG or \
        sh("stat -c '%a %U' /etc/footops/secrets/notify/telegram.json", c=C2).strip() != "600 root":
    fail("telegram not put back as it was")
no_secret_in_log(C2, secrets + (TG.split('"')[3],), "go-live")
ok("go-live (%.1f sn): prova bitti, kural kalktı, siteler dışarıya ulaşıyor; depo bağlı, kopyadan ilk yedek alındı (%d → %d anlık görüntü)"
   % (measure["go_live_s"], snaps_before, len(snaps_after)))

# ---- back as it was: one server again ----
subprocess.run(["docker", "rm", "-f", C2], capture_output=True)
subprocess.run(["docker", "volume", "rm", C2 + "-docker", C2 + "-containerd"], capture_output=True)
sh("systemctl start footops-agent")
wait(lambda: sh("test -S /run/footops/agent.sock && echo y", check=False).strip() == "y", "agent socket")
wait(lambda: api("GET", "/v1/backup", check=False)[0] == 200, "the agent", timeout=60)
cli(["backup", "set", "paused=off"])
sh("rm -rf /root/y5cfg")
ok("kopya silindi, kaynağın agent'ı ve yedekleri yeniden açık")
print(json.dumps(measure))
