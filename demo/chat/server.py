#!/usr/bin/env python3
"""Tiny local chat surface for the KARMAX Org Office demo (stdlib only).

POST /in        KARMAX's outbound webhook posts ({"text","channel"[,"target"][,"thread"]})
GET  /          the chat page
POST /send      {"text","user","thread"?} -> forwarded to KARMAX_COMMS_URL
GET  /messages  ?since=N -> [{id, ts, from, text, thread}] with id > N
"""
import json, os, threading, time, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ADDR = os.environ.get("CHAT_ADDR", "127.0.0.1:9393")
COMMS_URL = os.environ.get("KARMAX_COMMS_URL", "http://127.0.0.1:9090/comms/chat?token=demo-token")
HERE = os.path.dirname(os.path.abspath(__file__))
MSGS, LOCK = [], threading.Lock()


def add(frm, text, thread=""):
    with LOCK:
        m = {"id": len(MSGS) + 1, "ts": time.time(), "from": frm, "text": text, "thread": thread or ""}
        MSGS.append(m)
    return m


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, code, body, ctype="application/json"):
        b = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype + "; charset=utf-8")
        self.send_header("Content-Length", str(len(b)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(b)

    def body(self):
        try:
            return json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        except Exception:
            return None

    def do_GET(self):
        path, _, qs = self.path.partition("?")
        if path == "/":
            with open(os.path.join(HERE, "index.html"), "rb") as f:
                return self.reply(200, f.read(), "text/html")
        if path == "/messages":
            since = 0
            for kv in qs.split("&"):
                if kv.startswith("since="):
                    since = int(kv[6:] or 0) if kv[6:].isdigit() else 0
            with LOCK:
                return self.reply(200, [m for m in MSGS if m["id"] > since])
        self.reply(404, {"error": "not found"})

    def do_POST(self):
        path = self.path.partition("?")[0]
        d = self.body()
        if not isinstance(d, dict):
            return self.reply(400, {"error": "bad json"})
        text = str(d.get("text") or d.get("content") or "")
        if path == "/in":
            if not text:
                return self.reply(400, {"error": "no text"})
            return self.reply(200, add("karmax", text, str(d.get("thread") or "")))
        if path == "/send":
            user, thread = str(d.get("user") or "Maya"), str(d.get("thread") or "")
            if not text:
                return self.reply(400, {"error": "empty message"})
            out = {"text": text, "user": user, "channel": "eng"}
            if thread:
                out["thread"] = thread
            m = add(user, text, thread)
            req = urllib.request.Request(COMMS_URL, json.dumps(out).encode(), {"Content-Type": "application/json"})
            try:
                with urllib.request.urlopen(req, timeout=10) as r:
                    return self.reply(200, {"ok": True, "status": r.status, "message": m})
            except urllib.error.HTTPError as e:
                err = "KARMAX answered HTTP %d: %s" % (e.code, e.read(300).decode("utf-8", "replace"))
            except Exception as e:
                err = "could not reach KARMAX at %s: %s" % (COMMS_URL.split("?")[0], e)
            return self.reply(502, {"ok": False, "error": err, "message": m})
        self.reply(404, {"error": "not found"})


if __name__ == "__main__":
    host, _, port = ADDR.rpartition(":")
    print("chat on http://%s -> %s" % (ADDR, COMMS_URL.split("?")[0]), flush=True)
    ThreadingHTTPServer((host or "127.0.0.1", int(port)), H).serve_forever()
