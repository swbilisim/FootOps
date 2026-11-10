#!/usr/bin/env python3
"""Faz U1 (panel-tasarim §31.1): `footops doctor` on the test server — as root in a
terminal and as JSON —, the agent's run in its read-only transient service, which must
see what the command saw, what a run costs, a deviation that becomes an alert at the
second run and goes once put right, and a port Docker publishes past the firewall."""
import json
import re
import time

from itlib import ACTOR, api, fail, ok, sh, wait


def levels(r):
    return {f["id"]: f["level"] for f in r["findings"]}


def problems(r):
    return {f["id"]: f["title"] for f in r["findings"] if f["level"] in ("warn", "bad")}


def agent_run():
    """Asks the agent for a run and returns its report once it is in."""
    _, before = api("GET", "/v1/doctor")
    code, _ = api("POST", "/v1/doctor/run", {"actor": ACTOR}, check=False)
    if code != 202:
        fail("run: %d" % code)
    state = {}

    def done():
        _, st = api("GET", "/v1/doctor")
        state["st"] = st
        return not st["running"] and st.get("tried") != before.get("tried")
    wait(done, "the agent's check", timeout=300)
    st = state["st"]
    if st.get("error"):
        fail("the agent's check failed: %s" % st["error"])
    return st


def alert_keys():
    _, alerts = api("GET", "/v1/alerts")
    return {a["key"] for a in alerts or [] if a["key"].startswith("doctor:")}


# 1. The command, for a person: sections and a summary; 1 when there is something to fix.
out = sh("footops doctor; echo \"exit=$?\"")
for want in ["FootOps kontrolü — ", "Kurulum standardı", "Sistem", "Ağ", "Sürümler", "Özet: "]:
    if want not in out:
        fail("terminal report lacks %r:\n%s" % (want, out))
print(out.rstrip())
cli = json.loads(sh("footops doctor --json"))
if cli["format"] != 1 or not cli["findings"]:
    fail("JSON report: %s" % cli)
code = re.search(r"exit=(\d+)", out).group(1)
if (code == "1") != bool(problems(cli)):
    fail("exit %s with problems %s" % (code, problems(cli)))
steps = [f for f in cli["findings"] if f["id"].startswith("step:")]
if len(steps) < 15 or any(f["title"].startswith("Denetlenemedi") for f in steps):
    fail("standard: %s" % [(f["id"], f["level"], f["title"]) for f in steps])
ok("footops doctor: %d findings, %d steps, exit %s, problems %s" % (len(cli["findings"]), len(steps), code, problems(cli) or "none"))

# 2. The agent's run in the read-only service sees the same.
st = agent_run()
agent = st["report"]
diff = {k: (v, levels(agent).get(k)) for k, v in levels(cli).items() if levels(agent).get(k) != v}
if diff:
    fail("the service saw differently (command, service): %s\n%s" % (diff, json.dumps(agent, indent=1, ensure_ascii=False)))
ok("the agent's run agrees with the command (%d findings, %d ms)" % (len(agent["findings"]), agent["took_ms"]))
mode = sh("stat -c '%a %U' /var/lib/footops/doctor.json").strip()
if mode != "600 root":
    fail("doctor.json: %s" % mode)
if sh("systemctl list-units --all --plain --no-legend 'footops-doctor*'").strip():
    fail("the transient service stayed")
# What it cost, as systemd counted it when the service ended.
cost = sh("journalctl -u footops-doctor.service -n 20 --no-pager -o cat | grep -E 'Consumed' | tail -1", check=False).strip()
ok("cost: %s" % (cost or "(no accounting line)"))

# 3. A deviation: fail2ban stopped. Seen once: no alert yet; twice: an alert; put right: gone.
sh("systemctl stop fail2ban")
try:
    st = agent_run()
    f = {x["id"]: x for x in st["report"]["findings"]}["step:fail2ban"]
    if f["level"] != "bad" or "doctor:step:fail2ban" in alert_keys() or st.get("next") is None:
        fail("first sighting: %s, alerts %s" % (f, alert_keys()))
    ok("fail2ban stopped: %s (%s), no alert yet" % (f["title"], f["level"]))
    st = agent_run()
    wait(lambda: "doctor:step:fail2ban" in alert_keys(), "the alert", timeout=30)
    ok("second run: alert doctor:step:fail2ban")
finally:
    sh("systemctl start fail2ban")
agent_run()
wait(lambda: "doctor:step:fail2ban" not in alert_keys(), "the alert to go", timeout=30)
ok("fail2ban started: the alert is gone")

# 4. A port Docker publishes past UFW is serious.
sh("docker rm -f u1-port >/dev/null 2>&1 || true; docker run -d --name u1-port -p 8089:80 caddy:2.11.7-alpine >/dev/null")
try:
    r = json.loads(sh("footops doctor --json"))
    f = {x["id"]: x for x in r["findings"]}["ports"]
    if f["level"] != "bad" or "tcp 8089 → u1-port" not in f["title"]:
        fail("published port: %s" % f)
    ok("published port: %s" % f["title"])
finally:
    sh("docker rm -f u1-port >/dev/null")
print("u1: OK")
