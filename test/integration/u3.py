#!/usr/bin/env python3
"""Faz U3 (panel-tasarim §31.3): updates from signed releases. A stand-in for GitHub's
releases page in the test container (python's http.server, the agent pointed at it with
a systemd drop-in) serves releases signed with the test's own key, which only the test's
builds trust. Then: a check readies a release (downloaded, checked, planned); Güncelle
applies it in footops-update, the agent comes back as the new version and settles its
job, the sites keep running; a release that does not come up goes back to the version
before; and going back to the kept previous binary."""
import json
import os
import shutil
import subprocess
import time

from itlib import ACTOR, C, api, fail, ok, sh, wait

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
WORK = os.path.join(os.environ.get("TMPDIR", "/tmp"), "footops-it")
SEED = os.path.join(WORK, "release-test.seed")
REL = os.path.join(WORK, "rel")
ARCH = "arm64" if subprocess.run(["docker", "info", "--format", "{{.Architecture}}"], capture_output=True,
                                 text=True).stdout.strip() in ("aarch64", "arm64") else "amd64"
V1, V2 = "v0.90.0", "v0.91.0"
URL = "http://127.0.0.1:8099"
DROPIN = "/etc/systemd/system/footops-agent.service.d/u3-release.conf"


def local(*cmd, env=None):
    p = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, env=env)
    if p.returncode != 0:
        fail("%s\n%s%s" % (" ".join(cmd), p.stdout, p.stderr))
    return p.stdout


PUB = local("go", "run", "./test/integration/relsign", "pub", SEED).strip()


def build(version, out):
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=ARCH)
    local("go", "build", "-trimpath", "-ldflags",
          "-X github.com/swbilisim/FootOps/internal/version.Version=%s -X github.com/swbilisim/FootOps/internal/release.testKey=%s"
          % (version, PUB), "-o", out, "./cmd/footops", env=env)


def publish(version, binary):
    """Signs binary as version's release and makes it the latest on the stand-in."""
    d = os.path.join(REL, version)
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    shutil.copy(binary, os.path.join(d, "footops_linux_" + ARCH))
    local("go", "run", "./test/integration/relsign", "sign", SEED, d, version, "Deneme sürümü " + version)
    sh("mkdir -p /root/rel/download/%s /root/rel/latest/download" % version)
    for f in os.listdir(d):
        subprocess.run(["docker", "cp", os.path.join(d, f), "%s:/root/rel/download/%s/%s" % (C, version, f)], check=True)
    sh("cp /root/rel/download/%s/footops-release.json* /root/rel/latest/download/" % version)


def soft(path):
    """The agent's answer, or None while it restarts."""
    out = sh("curl -sS --max-time 5 --unix-socket /run/footops/agent.sock 'http://agent%s' 2>/dev/null || true" % path, check=False)
    try:
        return json.loads(out)
    except ValueError:
        return None


def running():
    info = soft("/v1/server")
    return info.get("footops") if info else None


def run_record():
    out = sh("cat /var/lib/footops/update/run.json 2>/dev/null || true", check=False)
    return json.loads(out) if out.strip() else None


def ready(version):
    api("POST", "/v1/update/check", {"actor": ACTOR}, check=False)

    def got():
        st = soft("/v1/update")
        return st and not st.get("checking") and (st.get("ready") or {}).get("version") == version
    wait(got, "release %s to be ready" % version, timeout=180)
    return soft("/v1/update")


def site_up():
    return "200" in sh("curl -sk -o /dev/null -w '%{http_code}' --resolve it.example.com:443:127.0.0.1 https://it.example.com/ || true", check=False)


old = running()
if not old:
    fail("agent not answering")

# The stand-in and the agent pointed at it.
sh("systemctl stop it-rel 2>/dev/null; rm -rf /root/rel /var/lib/footops/update; mkdir -p /root/rel", check=False)
sh("systemd-run --unit=it-rel --collect python3 -m http.server 8099 --bind 127.0.0.1 --directory /root/rel >/dev/null")
sh("mkdir -p %s && printf '[Service]\\nEnvironment=FOOTOPS_RELEASE_URL=%s\\n' > %s && systemctl daemon-reload && systemctl restart footops-agent"
   % (os.path.dirname(DROPIN), URL, DROPIN))
wait(lambda: running() == old, "the agent after its restart", timeout=60)
site_before = site_up()
try:
    # 1. A release, readied: downloaded, checked, planned.
    os.makedirs(REL, exist_ok=True)
    b1 = os.path.join(REL, "footops-" + V1)
    build(V1, b1)
    publish(V1, b1)
    st = ready(V1)
    plan = st["ready"]["plan"]
    changes = [s["name"] for s in plan["steps"] if s["change"]]
    if "panel-binary" not in changes or plan.get("interactive") or st["running"] != old or not st["newer"]:
        fail("plan: %s" % json.dumps(st, ensure_ascii=False))
    ok("%s ready (%s → %s), changes %s, impact %s" % (V1, old, V1, changes, plan.get("impact")))
    out = sh("FOOTOPS_RELEASE_URL=%s footops self-update --check" % URL)
    if V1 not in out or "Deneme sürümü" not in out:
        fail("self-update --check:\n" + out)
    ok("footops self-update --check says %s" % V1)

    # 2. Güncelle: the agent comes back as V1, its job settled, the sites up all along.
    _, job = api("POST", "/v1/update/apply", {"version": V1, "actor": ACTOR})
    t0, gaps, last_ok = time.time(), 0.0, time.time()
    while time.time() - t0 < 180:
        r = run_record()
        if r and r.get("to") == V1 and r.get("phase") in ("done", "rolled-back", "failed", "refused"):
            break
        up = sh("curl -s -o /dev/null -w '%{http_code}' --max-time 2 http://127.0.0.1:7701/login || true", check=False).strip() == "200"
        now = time.time()
        if up:
            last_ok = now
        else:
            gaps = max(gaps, now - last_ok)
        time.sleep(0.2)
    took = time.time() - t0
    r = run_record()
    if not r or r.get("phase") != "done":
        fail("update: %s\n%s" % (r, sh("tail -40 /var/lib/footops/update/%s/update.log 2>/dev/null || true" % V1, check=False)))
    wait(lambda: running() == V1, "the agent as %s" % V1, timeout=60)
    j = None

    def settled():
        global j
        j = soft("/v1/jobs/" + job["job_id"])
        return j and j.get("done") and not j.get("interrupted")
    wait(settled, "the update's job to be settled", timeout=60)
    if j.get("error") or not j["steps"][-1].startswith("✓ footops " + V1):
        fail("job: %s" % j)
    installed = sh("/usr/local/bin/footops version").split()[1]
    prev = sh("cat /var/lib/footops/update/prev/version").strip()
    if installed != V1 or prev != old or sh("test -e /var/lib/footops/update/prev.new && echo yes || true", check=False).strip():
        fail("installed %s, prev %s" % (installed, prev))
    audit = [e for e in (api("GET", "/v1/audit?limit=30")[1] or []) if e.get("action") == "server.update"]
    if not audit or audit[0]["target"] != "%s → %s" % (old, V1) or audit[0].get("detail"):
        fail("audit: %s" % audit)
    if site_before and not site_up():
        fail("the site is down after the update")
    ok("updated in %.1f s (panel unanswered at most %.1f s), job settled, prev %s, audit, site up" % (took, gaps, prev))

    # 3. A release that does not come up (it says V2, runs V1): back to V1, the way back to
    # the version before kept as it was.
    shutil.copy(b1, os.path.join(REL, "real-footops"))
    subprocess.run(["docker", "cp", os.path.join(REL, "real-footops"), C + ":/root/it-real-footops"], check=True)
    sh("chmod 755 /root/it-real-footops")
    fake = os.path.join(REL, "footops-" + V2)
    with open(fake, "w") as f:
        f.write("#!/bin/sh\nif [ \"$1\" = version ]; then echo 'footops %s (it, 2026) linux/%s'; exit 0; fi\nexec /root/it-real-footops \"$@\"\n" % (V2, ARCH))
    os.chmod(fake, 0o755)
    publish(V2, fake)
    ready(V2)
    _, job2 = api("POST", "/v1/update/apply", {"version": V2, "actor": ACTOR})
    wait(lambda: (run_record() or {}).get("to") == V2 and run_record().get("phase") != "applying", "the failed update to end", timeout=300)
    r = run_record()
    if r["phase"] != "rolled-back" or running() != V1 or "açılmadı" not in r.get("error", ""):
        fail("rollback: %s, running %s" % (r, running()))
    wait(lambda: (soft("/v1/jobs/" + job2["job_id"]) or {}).get("done"), "the rolled-back job", timeout=60)
    j2 = soft("/v1/jobs/" + job2["job_id"])
    if "yeniden çalışıyor" not in j2.get("error", ""):
        fail("after the rollback: job %s" % j2)
    # Alerts come with the sampler's rounds (10 s) and its hold.
    wait(lambda: "update:failed" in {a["key"] for a in (soft("/v1/alerts") or [])}, "the update:failed alert", timeout=90)
    if sh("cat /var/lib/footops/update/prev/version").strip() != old:
        fail("the way back changed")
    ok("%s did not come up: back to %s (%s), job error, alert, way back still %s" % (V2, V1, r["error"][:60], old))

    # 4. Going back to the kept binary.
    _, job3 = api("POST", "/v1/update/back", {"actor": ACTOR})
    wait(lambda: running() == old, "the agent as %s again" % old, timeout=180)
    wait(lambda: (run_record() or {}).get("phase") == "done", "the going back to end", timeout=60)
    st = soft("/v1/update")
    if st.get("prev") or sh("/usr/local/bin/footops version").split()[1] != old:
        fail("back: %s" % st)
    ok("back to %s; nothing further to go back to" % old)
finally:
    sh("rm -f %s && systemctl daemon-reload && systemctl stop it-rel 2>/dev/null; rm -rf /root/rel /root/it-real-footops" % DROPIN, check=False)
    sh("systemctl restart footops-agent", check=False)
    wait(lambda: running() is not None, "the agent after the clean-up", timeout=60)
print("u3: OK")
