#!/usr/bin/env python3
"""Fake services for scripts/smoke-v2.sh, each a small local HTTP server.

  smoke-fakes.py gitloom <port> <dir> <key>
      GET /v1/retrieve answers one fixed memory, and only with the key.
  smoke-fakes.py karmax <port> <dir> <token>
      GET /api/tools lists a read tool and a write tool; POST /api/tools/<name>
      records the call in <dir>/karmax-calls.jsonl and answers ok.
  smoke-fakes.py tailscaled <socket> <login>
      tailscaled's LocalAPI on a unix socket: every whois is <login>.

Anything else a fake is asked is a bug in fleetd: it answers 500 and appends
a line to <dir>/violations, which fails the run.
"""
import json
import os
import socketserver
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

MEMORY = {
    "path": "ops/deploys.md",
    "title": "Deploys",
    "content": "Deploys go out at 21:00 from pc2, after the smoke test passes.",
    "score": 0.9,
}

TOOLS = [
    {"name": "mail.search", "description": "Search the operator's mail.",
     "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}},
    {"name": "mail.send", "description": "Send a mail as the operator.",
     "parameters": {"type": "object", "properties": {"to": {"type": "string"}}}},
]


class Fake(BaseHTTPRequestHandler):
    kind, dir, secret = "", "", ""

    def log_message(self, *args):
        pass

    def answer(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def violation(self, why):
        with open(os.path.join(self.dir, "violations"), "a") as f:
            f.write(f"{self.kind}: {self.command} {self.path}: {why}\n")
        self.answer(500, {"error": f"smoke fake {self.kind}: {why}"})

    def authorized(self):
        return self.headers.get("Authorization") == "Bearer " + self.secret

    def body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return json.loads(self.rfile.read(n) or b"{}")

    def do_GET(self):
        path, _, _ = self.path.partition("?")
        if self.kind == "gitloom" and path == "/v1/retrieve":
            if not self.authorized():
                return self.violation("no gitloom key")
            return self.answer(200, {"memories": [MEMORY]})
        if self.kind == "karmax" and path == "/api/tools":
            if not self.authorized():
                return self.violation("no KARMAX token")
            return self.answer(200, {"tools": TOOLS})
        if self.kind == "tailscaled" and path == "/localapi/v0/whois":
            return self.answer(200, {"Node": {"Name": "laptop.example.ts.net.", "Tags": []},
                                     "UserProfile": {"LoginName": self.secret, "DisplayName": "Ram"}})
        self.violation("no such route")

    def do_POST(self):
        path, _, query = self.path.partition("?")
        if self.kind == "karmax" and path.startswith("/api/tools/"):
            if query:  # nothing but the name and the arguments is passed on
                return self.violation("a query on a tool call")
            name = path[len("/api/tools/"):]
            args = self.body()
            if name == "harness.list":  # fleetd reads the orchestrator's quota each tick
                return self.answer(200, {"tool": name, "ok": True, "output": {"harnesses": []}})
            with open(os.path.join(self.dir, "karmax-calls.jsonl"), "a") as f:
                f.write(json.dumps({"tool": name, "args": args, "token_ok": self.authorized()}) + "\n")
            return self.answer(200, {"tool": name, "ok": True, "output": {"echo": args}})
        self.violation("no such route")

    def do_PUT(self):
        self.violation("no such method")

    do_DELETE = do_PATCH = do_PUT


class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        conn, _ = super().get_request()
        return conn, ("local", 0)


def main():
    kind, where = sys.argv[1], sys.argv[2]
    Fake.kind = kind
    if kind == "tailscaled":
        Fake.secret, Fake.dir = sys.argv[3], os.path.dirname(where)
        if os.path.exists(where):
            os.unlink(where)
        UnixHTTPServer(where, Fake).serve_forever()
        return
    Fake.dir, Fake.secret = sys.argv[3], sys.argv[4]
    ThreadingHTTPServer(("127.0.0.1", int(where)), Fake).serve_forever()


if __name__ == "__main__":
    main()
