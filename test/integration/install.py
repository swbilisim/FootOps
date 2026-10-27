#!/usr/bin/env python3
"""Runs `footops install` inside the test container and answers its questions.

The installer reads answers line by line from a pipe just as from a terminal. Prompts end
without a newline, so output is read as it arrives and matched against known prompts.
The panel login printed at the end is written to a file for the scenario tests.

Usage: install.py <container> <ssh-public-key-file> <login-out-file>
"""
import base64
import hashlib
import hmac
import os
import re
import select
import struct
import subprocess
import sys
import time

container, keyfile, login_out = sys.argv[1:4]
pubkey = open(keyfile).read().strip()

ANSWERS = [
    ("Bu cevaplarla devam edilsin mi?", "e"),
    ("Sunucu kodu", "IT"),
    ("Hostname", "fo-it"),
    ("Saat dilimi", ""),
    ("Kullanıcı adı", "furkan"),
    ("anahtarları yönetici kullanıcıya da eklensin mi?", "e"),
    ("SSH public key", pubkey + "\n"),
    ("sudo şifre istesin mi?", "h"),
    ("(önerilen)", ""),
    ("Panel adresi", "panel-it.example.com"),
    ("otomatik yeniden başlatılsın mı?", "h"),
    ("Bu ayarlarla kuruluma başlansın mı?", "e"),
    ("Cloudflare Tunnel şimdi kurulsun mu?", "h"),
    ("Yeni kullanıcıyla giriş ve sudo çalıştı mı?", "e"),
    ("Panel kullanıcı adı", "furkan"),
]


def totp(secret, t=None):
    key = base64.b32decode(secret.upper() + "=" * (-len(secret) % 8))
    counter = int((t or time.time()) // 30)
    h = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    o = h[-1] & 0x0F
    return "%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000)


p = subprocess.Popen(["docker", "exec", "-i", container, "/root/footops", "install"],
                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
buf = ""
# The log holds the (throwaway) panel password and 2FA secret: owner-only.
log = os.fdopen(os.open(os.path.join(os.path.dirname(login_out), "install.log"),
                        os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w")
secret = None
deadline = time.time() + 1800
while True:
    if time.time() > deadline:
        p.kill()
        sys.exit("install: timed out")
    r, _, _ = select.select([p.stdout], [], [], 1.0)
    if not r:
        if p.poll() is not None:
            break
        continue
    chunk = os.read(p.stdout.fileno(), 65536).decode("utf-8", "replace")
    if not chunk:
        break
    log.write(chunk)
    log.flush()
    buf += chunk
    m = re.search(r"anahtarı elle gir: ([A-Z2-7]+)", buf)
    if m:
        secret = m.group(1)
    # A prompt is the last line ending in ":" (Ask/YesNo leave the cursor after it, Lines
    # prints a newline).
    tail = buf.rstrip()
    if not tail.endswith(":"):
        continue
    last = tail.split("\n")[-1]
    answer = None
    if "6 haneli kod" in last and secret:
        answer = totp(secret)
    else:
        for prompt, a in ANSWERS:
            if prompt in last:
                answer = a
                break
    if answer is None:
        continue
    p.stdin.write((answer + "\n").encode())
    p.stdin.flush()
    buf = ""

code = p.wait()
log.close()
full = open(os.path.join(os.path.dirname(login_out), "install.log")).read()
m = re.search(r"Panel girişi — kullanıcı: (\S+)\s+şifre: (\S+)", full)
if m and secret:
    with open(login_out, "w") as f:
        f.write("%s\n%s\n%s\n" % (m.group(1), m.group(2), secret))
    os.chmod(login_out, 0o600)
if code != 0:
    sys.exit("install: exit status %d (see install.log)" % code)
print("install: ok")
