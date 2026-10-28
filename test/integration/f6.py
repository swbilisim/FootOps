#!/usr/bin/env python3
"""F6 scenarios (the new-site flow) against a real install, after f0.py: a private image
in a password-protected registry inside the test server; checking it fails until the
panel logs in to the registry; the three-step form of the real panel (image check, form
filled in from it, review without values) creates the site, and the .env text arrives in
the container exactly; the site moves to another group and back (served throughout but
for a few seconds, its files with it), a move that fails half-way is put back; the DNS
check answers; logging out removes the token.

Usage: f6.py <container> <login-file>
"""
import json
import re
import subprocess
import sys
import time

from itlib import ACTOR, api, fail, fresh_totp, ok, rss, sh, wait, wait_job

C, LOGIN = sys.argv[1], sys.argv[2]
SITE, DOMAIN = "itf6", "f6.it.example.com"  # covered by f0's *.it.example.com certificate
REG, REGHOST = "it-registry", "localhost:5000"
IMAGE = REGHOST + "/itapp:1"
REGUSER, REGPASS = "it-user", "it-pass-6f2c"  # a throwaway registry inside the test server
HTPASSWD = "it-user:$2y$05$JoiA8LIRCdOD5tuqXZXi6Opj7pYmT2DHVCMxwmy0uAsWSk3XO1792\n"
JAR = "/tmp/f6-jar"
measure = {}

ENV = """# yerel .env dosyası
export NODE_ENV=production
QUOTED="iki kelime"
SINGLE='tek $HOME'
HASH=p@ss#1
COMMENTED=değer # yorum
MULTI="satır1
satır2"
DOLLAR=$5 ve ${X}
PORT = 8080
"""
WANT = {"NODE_ENV": "production", "QUOTED": "iki kelime", "SINGLE": "tek $HOME", "HASH": "p@ss#1", "COMMENTED": "değer",
        "MULTI": "satır1\nsatır2", "DOLLAR": "$5 ve ${X}", "PORT": "8080"}


def web(path, form=None, files=None):
    """A request to the panel's SSH-tunnel listener with the logged-in cookie jar. Returns
    (status, location, body)."""
    args = ""
    for k, v in (form or {}).items():
        args += " --data-urlencode %s" % sh_quote("%s=%s" % (k, v))
    for k, p in (files or {}).items():
        args += " --data-urlencode %s@%s" % (k, p)
    out = sh("curl -sS -b %s -c %s -o /tmp/f6-page -w '%%{http_code} %%{redirect_url}' %s 'http://localhost:7701%s'; echo; cat /tmp/f6-page"
             % (JAR, JAR, args, path))
    head, _, body = out.partition("\n")
    code, _, loc = head.partition(" ")
    return int(code), loc.replace("http://localhost:7701", ""), body


def sh_quote(s):
    return "'" + s.replace("'", "'\\''") + "'"


def csrf(page):
    m = re.search(r'name="csrf" value="([^"]+)"', page)
    if not m:
        fail("no CSRF token on the page")
    return m.group(1)


def https(path):
    return sh("curl -sk -o /dev/null -w '%%{http_code}' --resolve %s:443:127.0.0.1 https://%s%s" % (DOMAIN, DOMAIN, path), check=False).strip()


def body(path):
    return sh("curl -sk --resolve %s:443:127.0.0.1 https://%s%s" % (DOMAIN, DOMAIN, path), check=False)


def networks():
    return sh("docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' %s" % SITE).split()


def site_group():
    _, sites = api("GET", "/v1/sites")
    return next((s["spec"]["group"] for s in sites if s["spec"]["name"] == SITE), None)


def move(group, confirm=SITE):
    code, r = api("POST", "/v1/sites/%s/move" % SITE, {"group": group, "confirm": confirm, "actor": ACTOR}, check=False)
    if code != 202:
        return code, r
    return code, wait_job(r["job_id"])


def watch_start():
    # Requests every 50 ms while a move runs (detached in the container), to measure how
    # long the site is away.
    sh("rm -f /tmp/f6-codes /tmp/f6-stop")
    loop = ("end=$(($(date +%%s)+90)); while [ ! -e /tmp/f6-stop ] && [ $(date +%%s) -lt $end ]; do "
            "printf '%%s %%s\\n' $(date +%%s.%%N) $(curl -sk -o /dev/null -m 2 -w '%%{http_code}' --resolve %s:443:127.0.0.1 https://%s/f6.txt) >> /tmp/f6-codes; "
            "sleep 0.05; done" % (DOMAIN, DOMAIN))
    subprocess.run(["docker", "exec", "-d", C, "sh", "-c", loop], check=True)
    wait(lambda: sh("test -s /tmp/f6-codes && echo y || true").strip() == "y", "request loop")


def watch_stop():
    sh("touch /tmp/f6-stop; sleep 1; rm -f /tmp/f6-stop")
    lines = [l.split() for l in sh("cat /tmp/f6-codes").splitlines() if len(l.split()) == 2]
    bad = [float(t) for t, c in lines if c != "200"]
    return (round(max(bad) - min(bad) + 0.05, 2) if bad else 0.0), len(lines)


print("F6 senaryoları")
agent_before = rss("footops agent")

# ---- a private image in a password-protected registry ----
sh("docker rm -f %s >/dev/null 2>&1 || true; mkdir -p /root/it-registry-auth" % REG)
sh("cat > /root/it-registry-auth/htpasswd", stdin=HTPASSWD.encode())
sh("docker run -d --name %s -p 127.0.0.1:5000:5000 -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=it "
   "-e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd -v /root/it-registry-auth:/auth registry:2.8.3 >/dev/null" % REG)
wait(lambda: sh("curl -s -o /dev/null -w '%%{http_code}' http://%s/v2/" % REGHOST, check=False).strip() == "401", "registry")
# Pushed with a login of its own (root's, not the panel's), then the local copy removed: the
# panel has to fetch it from the registry.
sh("docker pull -q nginxinc/nginx-unprivileged:1.27-alpine >/dev/null && docker tag nginxinc/nginx-unprivileged:1.27-alpine %s" % IMAGE)
sh("DOCKER_CONFIG=/root/.it-push docker login %s -u %s --password-stdin >/dev/null 2>&1" % (REGHOST, REGUSER), stdin=REGPASS.encode())
sh("DOCKER_CONFIG=/root/.it-push docker push -q %s >/dev/null && rm -rf /root/.it-push && docker rmi %s >/dev/null" % (IMAGE, IMAGE))
_, sites = api("GET", "/v1/sites")
if any(s["spec"]["name"] == SITE for s in sites):  # left by a run that failed
    j = wait_job(api("POST", "/v1/sites/%s/remove" % SITE, {"actor": ACTOR})[1]["job_id"])
sh("rm -rf /srv/*/_removed/%s-* /srv/*/%s" % (SITE, SITE))
for l in api("GET", "/v1/registries")[1]:
    if l["host"] == REGHOST:
        api("POST", "/v1/registries/remove", {"host": REGHOST, "actor": ACTOR})
ok("özel imaj parolalı kayıt defterinde (%s), sunucuda kopyası yok" % IMAGE)

# ---- without a login the check fails and says why ----
_, r = api("POST", "/v1/images/check", {"image": IMAGE, "actor": ACTOR})
j = wait_job(r["job_id"])
if not j.get("error") or "imaja erişilemedi" not in j["error"]:
    fail("check without a login: %s\n%s" % (j.get("error"), "\n".join(j["steps"])))
ok("girişsiz kontrol reddedildi: " + j["error"][:90] + "…")

# ---- the panel logs in; a wrong token is refused, the token is never echoed ----
code, r = api("POST", "/v1/registries", {"host": REGHOST, "user": REGUSER, "token": "wrong-token", "actor": ACTOR}, check=False)
if code != 422 or "reddedildi" not in r["error"]:
    fail("wrong token: %d %s" % (code, r))
t0 = time.time()
_, r = api("POST", "/v1/registries", {"host": " " + REGHOST + " ", "user": REGUSER, "token": REGPASS + "\n", "actor": ACTOR})
measure["registry_login_s"] = round(time.time() - t0, 2)
cfg = sh("cat /var/lib/footops/docker/config.json")
if REGHOST not in json.loads(cfg)["auths"]:
    fail("docker config: %s" % cfg)
meta = sh("stat -c %a /var/lib/footops/registries.json; cat /var/lib/footops/registries.json")
_, regs = api("GET", "/v1/registries")
if not meta.startswith("600") or REGPASS in meta or REGPASS in json.dumps(regs) or [(l["host"], l["user"]) for l in regs] != [(REGHOST, REGUSER)]:
    fail("registries: %s %s" % (meta, regs))
if REGPASS in sh("tail -n 5 /var/lib/footops/audit.jsonl"):
    fail("token in the audit log")
ok("panelden giriş: yanlış token reddedildi, doğrusu kaydedildi (%.1f sn); token listede, denetimde, registries.json'da yok" % measure["registry_login_s"])

# ---- the real panel: log in, check the image, the form, the review, create ----
user, pw, secret = open(LOGIN).read().split()
sh("rm -f %s" % JAR)
_, _, page = web("/login")
code, loc, _ = web("/login", {"csrf": csrf(page), "username": user, "password": pw, "code": fresh_totp(secret)})
if code != 303 or loc != "/":
    fail("login: %d %s" % (code, loc))
_, _, page = web("/registries")
if REGHOST not in page or REGPASS in page:
    fail("registries page")
_, _, page = web("/sites/new")
tok = csrf(page)
t0 = time.time()
code, loc, _ = web("/sites/new", {"csrf": tok, "template": "static", "image": IMAGE, "action": "check"})
m = re.match(r"^/jobs/([A-Za-z0-9]+)\?tpl=static$", loc)
if code != 303 or not m:
    fail("image check: %d %s" % (code, loc))
j = wait_job(m.group(1))
measure["image_check_s"] = round(time.time() - t0, 2)
if j.get("error"):
    fail("image check: %s\n%s" % (j["error"], "\n".join(j["steps"])))
info = json.loads(j["result"])
if info["ports"] != [8080] or info["uid"] != 101 or info["platform"] != info["server_platform"]:
    fail("image info: %s" % info)
print("    " + "\n    ".join(j["steps"]))
_, _, page = web("/jobs/%s?tpl=static" % m.group(1))
if "/sites/new/form?check=%s&amp;tpl=static" % m.group(1) not in page:
    fail("job page lacks the way to the form")
_, _, page = web("/sites/new/form?check=%s&tpl=static" % m.group(1))
if 'name="port" value="8080"' not in page or "İmaj kontrol edildi" not in page or 'name="memory_mb" value="64"' not in page:
    fail("form not filled in from the check")
sh("cat > /tmp/f6.env", stdin=ENV.encode())
code, loc, page = web("/sites/new/form", {"csrf": tok, "template": "static", "check": m.group(1), "name": SITE, "group": "musteri",
                                          "domains": DOMAIN, "image": IMAGE, "port": "8080", "memory_mb": "64",
                                          "data_mount": "/usr/share/nginx/html", "db_var": "MONGODB_URI"}, files={"env": "/tmp/f6.env"})
if code != 303 or not loc.startswith("/sites/new/review/"):
    fail("form: %d %s\n%s" % (code, loc, page[:3000]))
review = loc
_, _, page = web(review)
if "/srv/musteri/%s" % SITE not in page or "QUOTED" not in page or "yorum sayıldı" not in page:
    fail("review page:\n" + page[:3000])
for v in ("iki kelime", "p@ss#1", "satır2", "tek $HOME"):
    if v in page:
        fail("review page shows a value: %r" % v)
t0 = time.time()
code, loc, _ = web(review, {"csrf": tok, "action": "create"})
if code != 303 or not loc.startswith("/jobs/"):
    fail("create: %d %s" % (code, loc))
j = wait_job(loc[len("/jobs/"):])
measure["create_s"] = round(time.time() - t0, 2)
if j.get("error"):
    fail("create: %s\n%s" % (j["error"], "\n".join(j["steps"])))
code, loc, _ = web(review, {"csrf": tok, "action": "create"})
if code != 303 or loc != "/sites/new":
    fail("a review created twice: %d %s" % (code, loc))
env = dict(kv.split("=", 1) for kv in json.loads(sh("docker inspect -f '{{json .Config.Env}}' %s" % SITE)))
for k, v in WANT.items():
    if env.get(k) != v:
        fail("container env %s = %r, want %r" % (k, env.get(k), v))
if sh("stat -c %%a /srv/musteri/%s/compose.yaml" % SITE).strip() != "600":
    fail("compose.yaml mode")
sh("echo f6-veri > /srv/musteri/%s/data/f6.txt && chown 101:101 /srv/musteri/%s/data/f6.txt" % (SITE, SITE))
if body("/f6.txt").strip() != "f6-veri":
    fail("site does not serve its data: %r" % body("/f6.txt"))
ok("panelden 3 adımda site: kontrol %.1f sn, oluşturma %.1f sn; özet değer göstermedi, tek kullanımlık; .env (export, tırnak, çok satır, # yorum, $) container'a birebir" %
   (measure["image_check_s"], measure["create_s"]))

# ---- DNS check: no public record for the test domain, but Caddy here serves it ----
_, checks = api("GET", "/v1/sites/%s/dns" % SITE)
# Any answer of the app counts here (nginx says 403 for its empty root), as in site checks.
if len(checks) != 1 or not 0 < checks[0]["local"] < 500 or checks[0]["dns"] in ("cloudflare", "here"):
    fail("dns check: %s" % checks)
ok("DNS kontrolü: %s, sunucuda HTTP %d" % (checks[0]["dns"] or "çözülemedi (" + checks[0].get("dns_error", "")[:40] + ")", checks[0]["local"]))

# ---- moving to another group and back ----
code, r = move("kisisel", confirm="yanlis")
if code != 422:
    fail("move without the name: %d %s" % (code, r))
watch_start()
t0 = time.time()
code, j = move("kisisel")
measure["move_s"] = round(time.time() - t0, 2)
measure["move_down_s"], polls = watch_stop()
if code != 202 or j.get("error"):
    fail("move: %s\n%s" % (j, "\n".join(j.get("steps", []))))
print("    " + "\n    ".join(j["steps"]))
if site_group() != "kisisel" or networks() != ["prj_kisisel_" + SITE] or sh("test -e /srv/musteri/%s && echo y || true" % SITE).strip():
    fail("after move: group %s networks %s" % (site_group(), networks()))
if sh("docker network ls --format '{{.Name}}' | grep -x prj_musteri_%s || true" % SITE).strip():
    fail("old network left")
if body("/f6.txt").strip() != "f6-veri" or sh("stat -c %%u /srv/kisisel/%s/data/f6.txt" % SITE).strip() != "101":
    fail("data after move")
if '"group": "kisisel"' not in sh("cat /srv/kisisel/%s/footops.json" % SITE) or "prj_kisisel_" + SITE not in sh("cat /srv/kisisel/%s/compose.yaml" % SITE):
    fail("files after move")
ok("grup değişti: musteri → kisisel (%.1f sn, site %.2f sn cevap vermedi, %d istekle ölçüldü); veri, ağ, compose, footops.json yerinde"
   % (measure["move_s"], measure["move_down_s"], polls))

# A move that fails half-way (Caddy down: the new configuration cannot be loaded) is put back.
sh("docker stop footops-caddy >/dev/null")
try:
    code, j = move("musteri")
finally:
    sh("docker start footops-caddy >/dev/null")
steps = "\n".join(j.get("steps", []))
if code != 202 or not j.get("error") or "Eski hâline dönülüyor" not in steps:
    fail("failed move: %s\n%s" % (j.get("error"), steps))
if site_group() != "kisisel" or networks() != ["prj_kisisel_" + SITE] or sh("test -e /srv/musteri/%s && echo y || true" % SITE).strip():
    fail("not put back: group %s networks %s\n%s" % (site_group(), networks(), steps))
if sh("docker network ls --format '{{.Name}}' | grep -x prj_musteri_%s || true" % SITE).strip():
    fail("new network of the failed move left")
wait(lambda: https("/f6.txt") == "200", "site after Caddy is back", timeout=60)
ok("yarıda kalan taşıma (Caddy kapalı) geri alındı: klasör, ağ, container eski yerinde; Caddy açılınca site 200")

code, j = move("musteri")
if code != 202 or j.get("error") or site_group() != "musteri" or body("/f6.txt").strip() != "f6-veri":
    fail("move back: %s" % j)
ok("geri taşındı: kisisel → musteri")

# ---- logout, cleanup ----
api("POST", "/v1/registries/remove", {"host": REGHOST, "actor": ACTOR})
if REGHOST in sh("cat /var/lib/footops/docker/config.json") or REGHOST in sh("cat /var/lib/footops/registries.json"):
    fail("login left after logout")
_, r = api("POST", "/v1/images/check", {"image": IMAGE + "-yok", "actor": ACTOR})
if not wait_job(r["job_id"]).get("error"):
    fail("check after logout")
ok("çıkış: token docker yapılandırmasından ve kayıttan silindi")
j = wait_job(api("POST", "/v1/sites/%s/remove" % SITE, {"actor": ACTOR})[1]["job_id"])
if j.get("error"):
    fail("remove: %s" % j["error"])
sh("rm -rf /srv/musteri/_removed/%s-* %s /tmp/f6-page /tmp/f6.env /tmp/f6-codes; docker rm -f %s >/dev/null; docker rmi -f %s >/dev/null 2>&1 || true"
   % (SITE, JAR, REG, IMAGE))
measure.update(agent_rss_before=agent_before, agent_rss_after=rss("footops agent"), web_rss=rss("footops web"))
print("ÖLÇÜMLER " + json.dumps(measure))
print("F6: tüm senaryolar geçti")
