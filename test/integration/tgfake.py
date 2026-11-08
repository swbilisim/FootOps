#!/usr/bin/env python3
"""A stand-in for Telegram's Bot API, for y4.py: getMe, getUpdates, sendMessage,
editMessageText, answerCallbackQuery and setMyCommands for one token. Updates come from
/tmp/tg-updates.json (written by the test); what the agent sends goes to
/tmp/tg-sent.jsonl, one JSON object a line ("_method" says which call, "_message_id" the
id a sent message got).

getUpdates behaves like Telegram's: an offset confirms the updates before it (they are
not given again), a negative one asks for the newest, and with a timeout an empty
answer waits for updates to come (up to the timeout).

Usage (in the test container): tgfake.py <port> <token>
"""
import json
import sys
import threading
import time
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, TOKEN = int(sys.argv[1]), sys.argv[2]
lock = threading.Lock()
state = {"confirmed": 0, "message_id": 0}


def updates():
    try:
        return json.load(open("/tmp/tg-updates.json"))
    except (OSError, ValueError):
        return []


def record(method, form, **extra):
    line = dict(form, _method=method, **extra)
    with lock, open("/tmp/tg-sent.jsonl", "a") as f:
        f.write(json.dumps(line) + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def answer(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def get_updates(self, form):
        offset = int(form.get("offset") or 0)
        if offset < 0:
            with lock:
                ups = [u for u in updates() if u["update_id"] >= state["confirmed"]]
            return ups[offset:]
        with lock:
            state["confirmed"] = max(state["confirmed"], offset)
        until = time.time() + min(int(form.get("timeout") or 0), 50)
        while True:
            with lock:
                ups = [u for u in updates() if u["update_id"] >= state["confirmed"]]
            if ups or time.time() >= until:
                return ups
            time.sleep(0.2)

    def do_POST(self):
        prefix = "/bot" + TOKEN + "/"
        if not self.path.startswith(prefix):
            return self.answer(401, {"ok": False, "error_code": 401, "description": "Unauthorized"})
        method = self.path[len(prefix):]
        n = int(self.headers.get("Content-Length") or 0)
        form = {k: v[0] for k, v in urllib.parse.parse_qs(self.rfile.read(n).decode()).items()}
        if method == "getMe":
            return self.answer(200, {"ok": True, "result": {"id": 42, "is_bot": True, "first_name": "FootOps", "username": "OrnekBot"}})
        if method == "getUpdates":
            return self.answer(200, {"ok": True, "result": self.get_updates(form)})
        if method == "sendMessage":
            with lock:
                state["message_id"] += 1
                mid = state["message_id"]
            record(method, form, _message_id=mid)
            return self.answer(200, {"ok": True, "result": {"message_id": mid}})
        if method == "editMessageText":
            record(method, form)
            return self.answer(200, {"ok": True, "result": {"message_id": int(form.get("message_id") or 0)}})
        if method in ("answerCallbackQuery", "setMyCommands"):
            record(method, form)
            return self.answer(200, {"ok": True, "result": True})
        return self.answer(404, {"ok": False, "error_code": 404, "description": "Not Found"})


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
