"""Helpers shared by the scenario scripts: commands in the test container, the agent API,
waiting, measuring, TOTP. The container name is the first command-line argument."""
import base64
import hashlib
import hmac
import json
import struct
import subprocess
import sys
import time

C = sys.argv[1]
SITE, DOMAIN = "itsite", "it.example.com"


def sh(cmd, stdin=None, check=True):
    args = ["docker", "exec", "-i", C, "sh", "-c", cmd] if stdin is not None else ["docker", "exec", C, "sh", "-c", cmd]
    p = subprocess.run(args, input=stdin, capture_output=True)
    if check and p.returncode != 0:
        fail("command failed: %s\n%s%s" % (cmd, p.stdout.decode(errors="replace"), p.stderr.decode(errors="replace")))
    return p.stdout.decode(errors="replace")


def fail(msg):
    print("FAIL:", msg)
    sys.exit(1)


def ok(msg):
    print("  ✓", msg)


def api(method, path, body=None, check=True):
    # A request without a body reads no stdin: docker exec -i now and then loses an
    # empty stdin's end, and curl then waits for it forever. --max-time bounds the rest.
    # The path is quoted: a query string's "&" would otherwise end the command.
    data = json.dumps(body).encode() if body is not None else None
    out = sh("curl -sS --max-time 300 -w '\\n%%{http_code}' -X %s -H 'Content-Type: application/json' %s"
             "--unix-socket /run/footops/agent.sock 'http://agent%s'"
             % (method, "--data-binary @- " if data is not None else "", path.replace("'", "%27")), stdin=data)
    text, code = out.rsplit("\n", 1)
    code = int(code)
    if check and code // 100 != 2:
        fail("%s %s -> %d %s" % (method, path, code, text))
    return code, (json.loads(text) if text.strip() else None)


ACTOR = {"user": "it", "ip": "test", "via": "ssh"}


def wait_job(jid, timeout=600):
    end = time.time() + timeout
    while time.time() < end:
        _, j = api("GET", "/v1/jobs/" + jid)
        if j["done"]:
            return j
        time.sleep(0.5)
    fail("job %s did not finish" % jid)


def wait(cond, what, timeout=120):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return
        time.sleep(0.3)
    fail("timed out waiting for " + what)


def tools():
    return sh("docker ps -a --filter name=footops-job- --format '{{.Names}}'").split()


def rss(pattern):
    out = sh("ps -eo rss=,args= | grep '%s' | grep -v grep || true" % pattern)
    return max([int(l.split()[0]) for l in out.splitlines() if l.strip()] or [0]) * 1024


def totp(secret):
    key = base64.b32decode(secret.upper() + "=" * (-len(secret) % 8))
    h = hmac.new(key, struct.pack(">Q", int(time.time() // 30)), hashlib.sha1).digest()
    o = h[-1] & 0x0F
    return "%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000)


def fresh_totp(secret):
    """A code from a TOTP step no earlier login used: codes are single-use, and the
    scenarios log in one after another."""
    time.sleep(31 - time.time() % 30)
    return totp(secret)
