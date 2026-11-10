"""Helpers of the update scenarios (u3, u4): releases built with the test's own key
(only the test's builds trust it), signed and served by a stand-in for GitHub's releases
page in the test container, and the agent's answers while it restarts."""
import json
import os
import shutil
import subprocess

from itlib import ACTOR, C, api, fail, sh, wait

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".."))
WORK = os.path.join(os.environ.get("TMPDIR", "/tmp"), "footops-it")
SEED = os.path.join(WORK, "release-test.seed")
REL = os.path.join(WORK, "rel")
ARCH = "arm64" if subprocess.run(["docker", "info", "--format", "{{.Architecture}}"], capture_output=True,
                                 text=True).stdout.strip() in ("aarch64", "arm64") else "amd64"
URL = "http://127.0.0.1:8099"
DROPIN = "/etc/systemd/system/footops-agent.service.d/u3-release.conf"


def local(*cmd, env=None):
    p = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True, env=env)
    if p.returncode != 0:
        fail("%s\n%s%s" % (" ".join(cmd), p.stdout, p.stderr))
    return p.stdout


PUB = local("go", "run", "./test/integration/relsign", "pub", SEED).strip()


def build(version, out, src=None):
    """Builds footops from this tree (or src, another checkout) as version, trusting the test's key."""
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=ARCH)
    p = subprocess.run(["go", "build", "-trimpath", "-ldflags",
                        "-X github.com/swbilisim/FootOps/internal/version.Version=%s -X github.com/swbilisim/FootOps/internal/release.testKey=%s"
                        % (version, PUB), "-o", out, "./cmd/footops"], cwd=src or ROOT, capture_output=True, text=True, env=env)
    if p.returncode != 0:
        fail("build %s:\n%s%s" % (version, p.stdout, p.stderr))


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




def serve_releases():
    """The stand-in releases page, and the agent pointed at it."""
    sh("systemctl stop it-rel 2>/dev/null; rm -rf /root/rel /var/lib/footops/update; mkdir -p /root/rel", check=False)
    sh("systemd-run --unit=it-rel --collect python3 -m http.server 8099 --bind 127.0.0.1 --directory /root/rel >/dev/null")
    before = running()
    sh("mkdir -p %s && printf '[Service]\\nEnvironment=FOOTOPS_RELEASE_URL=%s\\n' > %s && systemctl daemon-reload && systemctl restart footops-agent"
       % (os.path.dirname(DROPIN), URL, DROPIN))
    wait(lambda: running() == before, "the agent after its restart", timeout=60)


def stop_serving():
    sh("rm -f %s && systemctl daemon-reload && systemctl stop it-rel 2>/dev/null; rm -rf /root/rel /root/it-real-footops" % DROPIN, check=False)
    sh("systemctl restart footops-agent", check=False)
    wait(lambda: running() is not None, "the agent after the clean-up", timeout=60)
