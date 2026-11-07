#!/usr/bin/env python3
"""A stand-in for Telegram's Bot API, for y4.py: getMe, getUpdates and sendMessage for
one token. Updates come from /tmp/tg-updates.json (written by the test), sent messages
go to /tmp/tg-sent.jsonl, one JSON object a line.

Usage (in the test container): tgfake.py <port> <token>
"""
import json
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, TOKEN = int(sys.argv[1]), sys.argv[2]


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
            try:
                updates = json.load(open("/tmp/tg-updates.json"))
            except (OSError, ValueError):
                updates = []
            return self.answer(200, {"ok": True, "result": updates})
        if method == "sendMessage":
            with open("/tmp/tg-sent.jsonl", "a") as f:
                f.write(json.dumps(form) + "\n")
            return self.answer(200, {"ok": True, "result": {"message_id": 1}})
        return self.answer(404, {"ok": False, "error_code": 404, "description": "Not Found"})


ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
