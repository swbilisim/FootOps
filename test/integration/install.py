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
import struct
import sys
import time

from itlib import drive

container, keyfile, login_out = sys.argv[1:4]
pubkey = open(keyfile).read().strip()

ANSWERS = [
    ("Bu cevaplarla devam edilsin mi?", "e"),
    ("Bu sunucu bir yedekten mi kurulacak?", "h"),
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


secret = None


def code_answer(out):
    global secret
    m = re.findall(r"anahtarı elle gir: ([A-Z2-7]+)", out)
    secret = m[-1] if m else None
    return totp(secret)


code, full = drive(container, ["/root/footops", "install"], [("6 haneli kod", code_answer)] + ANSWERS,
                   os.path.join(os.path.dirname(login_out), "install.log"))
m = re.search(r"Panel girişi — kullanıcı: (\S+)\s+şifre: (\S+)", full)
if m and secret:
    with open(login_out, "w") as f:
        f.write("%s\n%s\n%s\n" % (m.group(1), m.group(2), secret))
    os.chmod(login_out, 0o600)
if code != 0:
    sys.exit("install: exit status %d (see install.log)" % code)
print("install: ok")
