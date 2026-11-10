#!/usr/bin/env python3
"""Faz U4 (panel-tasarim §31.5): the first update from the panel the way it will run on
the servers. The installed binary is built from tag v0.1.0 (the release they run) and
oversees; the new one, built from this tree, applies its own standard — and this time
FootOps' own containers change (their healthchecks every minute): the plan says before
anyone agrees that the sites will be cut for a few seconds, the update measures how long
they are, and afterwards Caddy and MongoDB check every minute, a site's container (its
image changed) has its two-minute healthcheck and the image it replaced is gone, and
the panel's job removes the images no container uses."""
import json
import os
import subprocess
import time

from itlib import ACTOR, C, api, fail, ok, sh, wait, wait_job
from relit import REL, ROOT, WORK, build, publish, ready, run_record, running, serve_releases, site_up, soft, stop_serving

OLD, NEW = "v0.89.0", "v0.95.0"
SITE = "itbak"

# The installed release: tag v0.1.0's code, as the test's build (it trusts the test's key).
os.makedirs(REL, exist_ok=True)
tree = os.path.join(WORK, "u4-v0.1.0")
subprocess.run(["git", "-C", ROOT, "worktree", "remove", "--force", tree], capture_output=True)
if subprocess.run(["git", "-C", ROOT, "worktree", "add", "--detach", tree, "v0.1.0"], capture_output=True).returncode != 0:
    fail("worktree of v0.1.0")
try:
    build(OLD, os.path.join(REL, "footops-" + OLD), src=tree)
finally:
    subprocess.run(["git", "-C", ROOT, "worktree", "remove", "--force", tree], capture_output=True)
subprocess.run(["docker", "cp", os.path.join(REL, "footops-" + OLD), C + ":/root/footops"], check=True)
p = subprocess.run(["python3", "install.py", C, os.path.join(WORK, "id_it.pub"), os.path.join(WORK, "login")],
                   capture_output=True, text=True)
sh("rm -f /root/footops")
if p.returncode != 0:
    fail("install of %s:\n%s%s" % (OLD, p.stdout, p.stderr))
wait(lambda: running() == OLD, "the agent as " + OLD, timeout=90)
intervals = lambda: sh("docker inspect -f '{{.Name}} {{.Config.Healthcheck.Interval}}' footops-caddy footops-mongo").split()
if intervals()[1::2] != ["30s", "30s"]:
    fail("v0.1.0's core: %s" % intervals())
ok("%s installed (v0.1.0's code); core healthchecks every 30 s" % OLD)

serve_releases()
try:
    # The release with this tree's changes: its plan names the core's recreation.
    b = os.path.join(REL, "footops-" + NEW)
    build(NEW, b)
    publish(NEW, b)
    st = ready(NEW)
    plan = st["ready"]["plan"]
    changes = [s["name"] for s in plan["steps"] if s["change"]]
    impact = " ".join(plan.get("impact") or [])
    if not {"core-files", "core-stack", "panel-binary"} <= set(changes) or "Caddy ve/veya MongoDB yeniden oluşturulur" not in impact or plan.get("interactive"):
        fail("plan: %s\n%s" % (changes, json.dumps(plan, ensure_ascii=False, indent=1)))
    ok("plan: %s — %s" % (changes, impact[:90]))

    # Güncelle: how long the site goes unanswered while Caddy and MongoDB are made anew.
    _, job = api("POST", "/v1/update/apply", {"version": NEW, "actor": ACTOR})
    t0, worst, last_up, down_since = time.time(), 0.0, time.time(), None
    while time.time() - t0 < 300:
        r = run_record()
        if r and r.get("to") == NEW and r.get("phase") not in (None, "applying"):
            break
        now = time.time()
        if site_up():
            if down_since is not None:
                worst, down_since = max(worst, now - down_since), None
            last_up = now
        elif down_since is None:
            down_since = last_up
        time.sleep(0.2)
    r = run_record()
    if not r or r.get("phase") != "done":
        fail("update: %s\n%s" % (r, sh("tail -40 /var/lib/footops/update/%s/update.log 2>/dev/null || true" % NEW, check=False)))
    wait(lambda: running() == NEW, "the agent as " + NEW, timeout=60)
    if intervals()[1::2] != ["1m0s", "1m0s"]:
        fail("core after the update: %s" % intervals())
    wait(lambda: (soft("/v1/jobs/" + job["job_id"]) or {}).get("done") and not soft("/v1/jobs/" + job["job_id"]).get("interrupted"),
         "the update's job to be settled", timeout=60)
    if not site_up():
        fail("site down after the update")
    ok("updated %s → %s in %.1f s; the site unanswered at most %.1f s; core healthchecks every 60 s" % (OLD, NEW, time.time() - t0, worst))

    # A site's image changes and back. Away: its container gets the two-minute healthcheck,
    # and the image it left stays (other sites use it: Docker refuses). Back: the image it
    # left is gone (nothing else used it).
    spec = [x["spec"] for x in api("GET", "/v1/sites")[1] if x["spec"]["name"] == SITE][0]
    image = spec["image"]
    other = image.rsplit(":", 1)[0] + ":1.26-alpine"

    def change(img):
        u = {"domains": spec["domains"], "image": img, "port": spec["port"], "memory_mb": spec["memory_mb"],
             "max_body_mb": spec.get("max_body_mb", 0), "data_mount": spec.get("data_mount", "")}
        _, j = api("POST", "/v1/sites/%s/update" % SITE, {"update": u, "actor": ACTOR})
        done = wait_job(j["job_id"], timeout=600)
        if done.get("error"):
            fail("image %s: %s" % (img, done))
        return done
    done = change(other)
    hc = sh("docker inspect -f '{{.Config.Healthcheck.Interval}}' " + SITE).strip()
    kept = sh("docker image inspect %s >/dev/null 2>&1 && echo kaldı || echo gitti" % image).strip()
    if hc != "2m0s" or kept != "kaldı" or not any("Önceki imaj bırakıldı" in s for s in done["steps"]):
        fail("away: healthcheck %s, %s %s, steps %s" % (hc, image, kept, done["steps"]))
    done = change(image)
    gone = sh("docker image inspect %s >/dev/null 2>&1 && echo kaldı || echo gitti" % other).strip()
    if gone != "gitti" or not any("Önceki imaj silindi" in s for s in done["steps"]):
        fail("back: %s %s, steps %s" % (other, gone, done["steps"]))
    ok("%s: on %s its healthcheck every 2 min, %s kept (other sites use it); back on %s, %s removed" % (SITE, other, image, image, other))

    # The panel's clean-up: what no container uses goes, FootOps' own images stay.
    sh("docker pull -q alpine:3.20 >/dev/null")
    _, j = api("POST", "/v1/images/prune", {"actor": ACTOR})
    done = wait_job(j["job_id"], timeout=300)
    left = sh("docker image inspect alpine:3.20 >/dev/null 2>&1 && echo kaldı || echo gitti").strip()
    kept = sh("docker image inspect caddy:2.11.7-alpine mongo:7.0.45 >/dev/null 2>&1 && echo kaldı || echo gitti").strip()
    if done.get("error") or left != "gitti" or kept != "kaldı" or not done["steps"][-1].startswith("✓"):
        fail("prune: %s, alpine %s, core %s" % (done, left, kept))
    ok("unused images removed (%s): %s; FootOps' own kept" % (done.get("result"), done["steps"][-1]))
finally:
    stop_serving()
print("u4: OK")
