#!/usr/bin/env python3
"""Chat surface + live Org Office console for the KARMAX demo (stdlib only).

POST /in          KARMAX's outbound webhook posts ({"text","channel"[,"target"][,"thread"]})
POST /send        {"text","user","thread"?} -> forwarded to KARMAX_COMMS_URL
GET  /messages    ?since=N -> [{id, ts, from, text, thread}] with id > N
GET  /            the console (demo/console/index.html and its assets)
GET  /api/state   one JSON: messages, health history, fake GitHub (PRs, reviews, repo compliance), policy
GET  /api/pr      ?repo=org/name&n=N -> the PR with its diff files
POST /api/act     {"action","args":[...]} -> runs demo/act.sh (presenter controls); {"action":"approve"} fills the pending code

Env: CHAT_ADDR, KARMAX_COMMS_URL, FAKE_GITHUB, PAYMENTS_URL, KARMAX_CONFIG, ACT_SH.
"""
import base64, json, mimetypes, os, re, subprocess, threading, time, urllib.parse, urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ADDR = os.environ.get("CHAT_ADDR", "127.0.0.1:9393")
COMMS_URL = os.environ.get("KARMAX_COMMS_URL", "http://127.0.0.1:9090/comms/chat?token=demo-token")
GH = os.environ.get("FAKE_GITHUB", "http://127.0.0.1:8787").rstrip("/")
PAY = os.environ.get("PAYMENTS_URL", "http://127.0.0.1:9494").rstrip("/")
HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)  # demo/
CONSOLE = os.path.join(ROOT, "console")
CONFIG = os.environ.get("KARMAX_CONFIG", os.path.join(ROOT, "karmax.yaml"))
ACT = os.environ.get("ACT_SH", os.path.join(ROOT, "act.sh"))
CHECKS = [("readme", "README.md"), ("codeowners", "CODEOWNERS"), ("claude", "CLAUDE.md"),
          ("ci", ".github/workflows/ci.yml")]

MSGS, LOCK = [], threading.Lock()
HEALTH = []  # ring buffer of {t, ok, code, ms}
HEALTH_MAX = 70  # ~2 min at 2 s
GHSTATE = {"ok": False, "error": "not polled yet", "state": {}, "repos": [], "ci": {}, "t": 0}
POLICY = {"mtime": 0, "policy": {}, "error": ""}


def add(frm, text, thread=""):
    with LOCK:
        m = {"id": len(MSGS) + 1, "ts": time.time(), "from": frm, "text": text, "thread": thread or ""}
        MSGS.append(m)
    return m


def http_json(url, timeout=3, data=None):
    """-> (status, parsed-json-or-None). Raises only on connection errors."""
    req = urllib.request.Request(url, data, {"Content-Type": "application/json"} if data else {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw, code = r.read(), r.status
    except urllib.error.HTTPError as e:
        raw, code = e.read(), e.code
    try:
        return code, json.loads(raw or b"null")
    except ValueError:
        return code, None


# ---- production health probe -------------------------------------------------
def health_loop():
    while True:
        t0 = time.time()
        try:
            req = urllib.request.Request(PAY + "/health")
            try:
                with urllib.request.urlopen(req, timeout=2) as r:
                    code = r.status
            except urllib.error.HTTPError as e:
                code = e.code
            e = {"t": t0, "ok": 200 <= code < 300, "code": code, "ms": int((time.time() - t0) * 1000)}
        except Exception:
            e = {"t": t0, "ok": False, "code": 0, "ms": int((time.time() - t0) * 1000), "down": True}
        HEALTH.append(e)
        del HEALTH[:-HEALTH_MAX]
        time.sleep(max(0.2, 2 - (time.time() - t0)))


# ---- fake GitHub poller ------------------------------------------------------
def gh_file(repo, path):
    try:
        code, _ = http_json("%s/repos/%s/contents/%s" % (GH, repo, urllib.parse.quote(path)), 2)
        return code == 200
    except Exception:
        return False


def gh_protection(repo):
    try:
        code, j = http_json("%s/repos/%s/branches/main/protection" % (GH, repo), 2)
    except Exception:
        return None
    if code != 200 or not isinstance(j, dict):
        return None
    pr = j.get("required_pull_request_reviews") or {}
    rs = j.get("required_status_checks") or {}
    return {"reviews": pr.get("required_approving_review_count", 0), "checks": rs.get("contexts") or []}


def gh_loop():
    while True:
        try:
            code, st = http_json(GH + "/_fake/state", 3)
            if code != 200 or not isinstance(st, dict):
                raise RuntimeError("HTTP %s" % code)
            repos = []
            for full in st.get("repos") or []:
                if full.endswith("/template-go-service"):
                    continue  # the factory template is plumbing, not part of the org's estate
                c = {k: gh_file(full, p) for k, p in CHECKS}
                c["protection"] = gh_protection(full)
                repos.append({"full": full, "name": full.split("/")[-1], "checks": c,
                              "created": full in (st.get("created") or [])})
            ci = {}
            for p in st.get("prs") or []:
                try:
                    _, s = http_json("%s/repos/%s/commits/%s/status" % (GH, p["repo"], p["sha"]), 2)
                    ci["%s#%d" % (p["repo"], p["number"])] = (s or {}).get("state", "pending") if (s or {}).get("statuses") else "pending"
                except Exception:
                    pass
            GHSTATE.update(ok=True, error="", state=st, repos=repos, ci=ci, t=time.time())
        except Exception as e:
            GHSTATE.update(ok=False, error="fake GitHub unreachable at %s (%s)" % (GH, e), t=time.time())
        time.sleep(1.5)


# ---- autonomy policy (read from karmax.yaml; refreshed when the file changes) --
def read_policy():
    try:
        mt = os.stat(CONFIG).st_mtime
    except OSError as e:
        POLICY.update(policy={}, error="cannot read %s" % os.path.basename(CONFIG))
        return
    if mt == POLICY["mtime"]:
        return
    pol, inside, indent = {}, False, 0
    try:
        with open(CONFIG, encoding="utf-8") as f:
            for line in f:
                s = line.split("#", 1)[0].rstrip()
                if not s.strip():
                    continue
                ind = len(s) - len(s.lstrip())
                if not inside:
                    if re.match(r"\s*policy:\s*$", s):
                        inside, indent = True, ind
                    continue
                if ind <= indent:
                    break
                m = re.match(r"\s*([\w.-]+):\s*[\"']?([^\"']+?)[\"']?\s*$", s)
                if m:
                    pol[m.group(1)] = m.group(2)
        POLICY.update(mtime=mt, policy=pol, error="")
    except Exception as e:
        POLICY.update(error=str(e))


def policy_loop():
    while True:
        read_policy()
        time.sleep(1)


# ---- presenter actions -------------------------------------------------------
def pending_code():
    """Latest 'approve CODE' request in chat that has not been answered."""
    with LOCK:
        msgs = list(MSGS)
    for i in range(len(msgs) - 1, -1, -1):
        m = re.search(r"`approve ([A-Z0-9]{5})`", msgs[i]["text"]) if msgs[i]["from"] == "karmax" else None
        if m:
            code = m.group(1)
            done = any(code in x["text"] and (x["text"].startswith("👍") or x["text"].startswith("🛑")) for x in msgs[i + 1:])
            return None if done else code
    return None


def stub_act(action, args):
    """Used until demo/act.sh exists: the same calls the scene scripts make."""
    def pr(payload):
        _, j = http_json(GH + "/_fake/pulls", 5, json.dumps(payload).encode())
        return {"ok": True, "action": action, "pr": j["number"]}
    if action == "leak-pr":
        return pr({"repo": "acme/payments-api", "title": "added aws uploader", "body": "quick thing", "author": "dev1",
                   "files": [{"filename": "uploader.go", "patch": "@@ -0,0 +1,4 @@\n+package main\n+\n+const awsKey = \"AKIAIOSFODNN7EXAMPLE\"\n+const awsSecret = \"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\""}]})
    if action == "clean-pr":
        patch = "@@ -0,0 +1,6 @@\n+package payments\n+\n+// Refund validates and refunds a charge.\n+func Refund(id string, amount int64) error {\n+\treturn nil\n+}"
        return pr({"repo": "acme/payments-api", "title": "feat: add validated refund helper", "author": "dev2",
                   "body": "Adds Refund with validation and table-driven tests.",
                   "files": [{"filename": "refunds.go", "status": "added", "patch": patch}]})
    if action == "ci" and len(args) >= 2:
        http_json(GH + "/_fake/ci", 5, json.dumps({"repo": "acme/payments-api", "number": int(args[0]), "state": args[1]}).encode())
        return {"ok": True, "action": action, "pr": int(args[0]), "state": args[1]}
    if action in ("break", "heal"):
        http_json(PAY + "/fault/" + ("on" if action == "break" else "off"), 5, b"{}")
        return {"ok": True, "action": action}
    if action == "say" and args:
        return say(args[0], args[1] if len(args) > 1 else "Maya", action)
    return {"ok": False, "error": "action %r needs demo/act.sh (not present)" % action}


def say(text, user, action="say"):
    m = add(user, text)
    out = {"text": text, "user": user, "channel": "eng"}
    try:
        code, _ = http_json(COMMS_URL, 10, json.dumps(out).encode())
        if code >= 300:
            return {"ok": False, "action": action, "error": "KARMAX answered HTTP %d" % code}
    except Exception as e:
        return {"ok": False, "action": action, "error": "could not reach KARMAX: %s" % e}
    return {"ok": True, "action": action, "text": text, "user": user, "id": m["id"]}


def run_act(action, args):
    if action == "approve":
        code = pending_code()
        if not code:
            return {"ok": False, "action": action, "error": "nothing is waiting for approval"}
        r = say("approve " + code, "Maya", action)
        r["code"] = code
        return r
    if not re.fullmatch(r"[a-z-]+", action or ""):
        return {"ok": False, "error": "bad action"}
    args = [str(a) for a in args]
    if not os.path.exists(ACT):
        try:
            return stub_act(action, args)
        except Exception as e:
            return {"ok": False, "action": action, "error": str(e)}
    try:
        p = subprocess.run(["bash", ACT, action] + args, capture_output=True, text=True, timeout=30,
                           cwd=os.path.dirname(ROOT))
    except subprocess.TimeoutExpired:
        return {"ok": False, "action": action, "error": "act.sh timed out"}
    lines = [l for l in p.stdout.strip().splitlines() if l.strip().startswith("{")]
    try:
        return json.loads(lines[-1])
    except Exception:
        return {"ok": False, "action": action, "error": (p.stderr or p.stdout or "no output").strip()[-300:]}


# ---- HTTP ---------------------------------------------------------------------
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

    def static(self, rel):
        rel = rel.lstrip("/") or "index.html"
        full = os.path.realpath(os.path.join(CONSOLE, rel))
        if not full.startswith(os.path.realpath(CONSOLE) + os.sep) or not os.path.isfile(full):
            return self.reply(404, {"error": "not found"})
        ctype = mimetypes.guess_type(full)[0] or "application/octet-stream"
        with open(full, "rb") as f:
            self.reply(200, f.read(), ctype)

    def do_GET(self):
        path, _, qs = self.path.partition("?")
        q = dict(urllib.parse.parse_qsl(qs))
        if path == "/messages":
            since = int(q["since"]) if q.get("since", "").isdigit() else 0
            with LOCK:
                return self.reply(200, [m for m in MSGS if m["id"] > since])
        if path == "/api/state":
            with LOCK:
                msgs = list(MSGS)
            st = GHSTATE
            return self.reply(200, {
                "now": time.time(), "messages": msgs,
                "health": {"url": PAY + "/health", "points": list(HEALTH)},
                "github": {"ok": st["ok"], "error": st["error"], "state": st["state"], "repos": st["repos"], "ci": st["ci"]},
                "policy": {"values": POLICY["policy"], "error": POLICY["error"], "mtime": POLICY["mtime"]},
                "pending": pending_code()})
        if path == "/api/pr":
            repo, n = q.get("repo", ""), q.get("n", "")
            if not re.fullmatch(r"[\w.-]+/[\w.-]+", repo) or not n.isdigit():
                return self.reply(400, {"error": "want repo=org/name&n=N"})
            try:
                _, pr = http_json("%s/repos/%s/pulls/%s" % (GH, repo, n), 3)
                _, files = http_json("%s/repos/%s/pulls/%s/files" % (GH, repo, n), 3)
            except Exception as e:
                return self.reply(502, {"error": "fake GitHub unreachable: %s" % e})
            return self.reply(200, {"pr": pr, "files": files or []})
        if path.startswith("/api/"):
            return self.reply(404, {"error": "not found"})
        self.static(path)

    def do_POST(self):
        path = self.path.partition("?")[0]
        d = self.body()
        if not isinstance(d, dict):
            return self.reply(400, {"error": "bad json"})
        text = str(d.get("text") or d.get("content") or "")
        if path == "/api/act":
            r = run_act(str(d.get("action") or ""), d.get("args") or [])
            return self.reply(200 if r.get("ok") else 500, r)
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
    for fn in (health_loop, gh_loop, policy_loop):
        threading.Thread(target=fn, daemon=True).start()
    host, _, port = ADDR.rpartition(":")
    print("console on http://%s -> %s" % (ADDR, COMMS_URL.split("?")[0]), flush=True)
    ThreadingHTTPServer((host or "127.0.0.1", int(port)), H).serve_forever()
