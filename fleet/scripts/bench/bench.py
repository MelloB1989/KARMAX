#!/usr/bin/env python3
"""Load-test the agent fleet with real tasks, and measure it.

    bench.py run --profile mixed [--out DIR] [--chaos 600:restart-agent ...]
    bench.py report DIR
    bench.py clean             # delete the bench's branches from fleet-bench's origin

A run submits a profile of tasks (tasks.py) through fleetctl as the operator,
drives each one the way a careful orchestrator would — waits, answers
questions, cancels, retries a run fleetd dropped — checks every result
itself (hidden tests, checksums), closes the task, and logs every step to
DIR/log.jsonl. Faults can be injected on a schedule (--chaos AT:ACTION[:ARG],
AT in seconds from the start). The report joins the log with fleetd's ledger
for the timings.
"""

import argparse
import json
import os
import random
import re
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import tasks as T  # noqa: E402

REPO = "fleet-bench"
CODE = os.environ.get("FLEET_CODE") or os.path.expanduser("~/code")
ORIGIN = f"{CODE}/fleet-bench-origin.git"


def kali_ip():
    """Kali's LAN address, which a partition cuts PC2 off from: BENCH_KALI_IP,
    else the host in fleet.yaml's fleetd_url (how PC2's agents reach fleetd)."""
    if os.environ.get("BENCH_KALI_IP"):
        return os.environ["BENCH_KALI_IP"]
    path = os.environ.get("FLEET_CONFIG") or os.path.expanduser("~/.karmax/fleet/fleet.yaml")
    with open(path) as f:
        m = re.search(r"fleetd_url:\s*https?://([0-9.]+):", f.read())
    if not m:
        sys.exit("bench: no fleetd_url with an IP in fleet.yaml; set BENCH_KALI_IP")
    return m.group(1)
LEDGER = os.path.expanduser("~/.karmax/fleet/fleet.db")
PC2 = "pc2"
DOCKER = ["docker", "--context", PC2]

_log_lock = threading.Lock()
_log_file = None


def log(kind, **kw):
    rec = {"t": time.time(), "kind": kind, **kw}
    with _log_lock:
        _log_file.write(json.dumps(rec) + "\n")
        _log_file.flush()
    short = {k: v for k, v in kw.items() if k not in ("text", "out")}
    print(time.strftime("%H:%M:%S"), kind, json.dumps(short)[:220], flush=True)


def sh(argv, stdin=None, timeout=600):
    try:
        p = subprocess.run(argv, input=stdin, capture_output=True, text=True, timeout=timeout)
        return p.returncode, p.stdout, p.stderr
    except subprocess.TimeoutExpired:
        return 124, "", "timeout"


def fleetctl(args, stdin=None, timeout=600):
    return sh(["fleetctl", *args], stdin=stdin, timeout=timeout)


# --- driving one task -------------------------------------------------------

DEADLINE = {"slow": 45 * 60, "chain": 45 * 60, "cancel": 20 * 60}


class Driver(threading.Thread):
    def __init__(self, task, inflight):
        super().__init__(daemon=True)
        self.task, self.inflight = task, inflight
        self.agent = None
        self.delivered_at = None
        self.sent_back = False

    def run(self):
        t = self.task
        try:
            self._run()
        except Exception as e:  # a bench bug must not stop the others
            log("driver_error", task=t.id, error=repr(e))
        finally:
            self.inflight.pop(t.id, None)

    def _run(self):
        t = self.task
        log("submit", task=t.id, task_kind=t.kind)
        # A careful orchestrator retries a run fleetd dropped, with the same
        # task id, so a run that did land is not given out twice.
        for attempt in range(20):
            rc, out, err = fleetctl(["run", "-", "--repo", REPO, "--from", "operator", "--task", t.id,
                                     "--queue", "120m", "--json"], stdin=t.brief, timeout=125 * 60)
            if rc == 0:
                r = json.loads(out)
                self.agent = r["agent"]
                self.inflight[t.id] = self
                log("assigned", task=t.id, agent=self.agent, why=r.get("why"), attempt=attempt)
                break
            log("run_failed", task=t.id, rc=rc, error=(err or out).strip()[:400], attempt=attempt)
            if rc == 5 or "unreachable" in err or "connection refused" in err or "EOF" in err or rc == 124:
                time.sleep(15)
                continue
            return
        else:
            return
        self.delivered_at = time.time()
        if t.cancel_after:
            threading.Timer(t.cancel_after, self.cancel).start()
        deadline = time.time() + DEADLINE.get(t.kind, 40 * 60) + t.min_secs
        after = 0
        while time.time() < deadline:
            args = ["wait", "--task", t.id, "--timeout", "5m", "--json"]
            if after:
                args += ["--after", str(after)]
            rc, out, err = fleetctl(args, timeout=7 * 60)
            try:
                w = json.loads(out)
            except ValueError:
                log("wait_error", task=t.id, rc=rc, error=(err or out).strip()[:300])
                time.sleep(10)
                continue
            reason = w.get("reason")
            if reason == "message":
                m = w["message"]
                after = m["id"]
                text = m.get("text", "")
                log("report", task=t.id, agent=self.agent, relay=m["id"], text=text[:2000])
                if text.startswith(f"QUESTION {t.id}") or (t.answer and "QUESTION" in text[:40]):
                    if PARTITION_ON_QUESTION and not PARTITION_ON_QUESTION.is_set():
                        PARTITION_ON_QUESTION.set()  # the first question: cut pc2 off, then answer
                        chaos("partition", "120", self.inflight)
                        time.sleep(5)
                    if t.answer:
                        rc, _, err = fleetctl(["followup", t.id, f"Answer: {t.answer}."])
                        log("answered", task=t.id, answer=t.answer, rc=rc, error=err.strip()[:200])
                    continue
                if text.lstrip().startswith(f"DONE {t.id}"):  # the brief's form; "I will send DONE…" is progress
                    ok, detail = self.verify(text)
                    log("verified", task=t.id, agent=self.agent, ok=ok, detail=detail[:1500],
                        early=(time.time() - self.delivered_at) < t.min_secs * 0.95)
                    if not ok and not self.sent_back:
                        # As an operator would: say what is wrong, once, and wait for the fix.
                        self.sent_back = True
                        rc, _, err = fleetctl(["followup", t.id, f"Not done: {detail[:300]}. Fix it, then report DONE {t.id} again."])
                        log("sent_back", task=t.id, agent=self.agent, rc=rc, error=err.strip()[:200])
                        continue
                    rc, _, err = fleetctl(["done", "--task", t.id, "--outcome", "done" if ok else "failed"])
                    log("done", task=t.id, agent=self.agent, rc=rc, error=err.strip()[:200])
                    return
                continue  # a progress note
            if reason == "closed":
                log("closed", task=t.id, agent=self.agent, outcome=(w.get("task") or {}).get("outcome"))
                if t.cancel_after:
                    self.after_cancel()
                return
            if reason == "phase":
                log("phase", task=t.id, agent=self.agent, phase=w.get("phase"))
                time.sleep(30)
                continue
            log("wait_timeout", task=t.id, agent=self.agent)
        log("deadline", task=t.id, agent=self.agent)
        rc, _, err = fleetctl(["cancel", t.id, "--reason", "the bench's deadline passed"])
        log("cancelled_at_deadline", task=t.id, rc=rc, error=err.strip()[:200])

    def cancel(self):
        rc, _, err = fleetctl(["cancel", self.task.id, "--reason", "the bench cancels it"])
        log("cancel", task=self.task.id, agent=self.agent, rc=rc, error=err.strip()[:300])

    def after_cancel(self):
        # Cancelled work must stop: nothing of it left running in the container.
        time.sleep(60)
        rc, out, _ = sh([*DOCKER, "exec", self.agent, "sh", "-c", f"pgrep -af 'slow.sh {self.task.id}' || true"])
        log("cancel_leftovers", task=self.task.id, agent=self.agent, procs=out.strip()[:400])

    def verify(self, text):
        t = self.task
        if t.kind in ("slow", "chain"):
            want = t.expected()
            missing = [w for w in want if w not in text]
            return not missing, f"checksums want {want}, missing {missing}"
        return verify_code(t)


def verify_code(t):
    rc, out, _ = sh(["git", "--git-dir", ORIGIN, "for-each-ref", "--format=%(refname:short)", "refs/heads/"])
    branches = [b for b in out.split() if b.endswith("/" + t.id) or b.endswith(t.id)]
    if not branches:
        return False, f"no branch for {t.id} in origin"
    d = tempfile.mkdtemp(prefix="bench-")
    try:
        rc, _, err = sh(["git", "clone", "-q", "--branch", branches[0], ORIGIN, d + "/r"])
        if rc:
            return False, "clone: " + err
        pkg = f"{d}/r/tasks/{t.id}"
        if not os.path.isdir(pkg):
            return False, f"branch {branches[0]} has no tasks/{t.id}"
        lang, name, body = t.hidden
        with open(f"{pkg}/{name}", "w") as f:
            f.write(body)
        if lang == "go":
            rc, out, err = sh(["sh", "-c", f"cd {d}/r && go test -count=1 ./tasks/{t.id}/"], timeout=300)
        else:
            rc, out, err = sh(["sh", "-c", f"cd {pkg} && python3 -m unittest discover -s . -p 'test*.py'"], timeout=300)
        return rc == 0, f"{branches[0]}: rc={rc} {(out + err)[-1200:]}"
    finally:
        shutil.rmtree(d, ignore_errors=True)


# --- faults -------------------------------------------------------------------

def pick_victim(inflight, prefer=("chain", "slow")):
    ds = [d for d in list(inflight.values()) if d.agent]
    for kind in prefer:
        c = [d for d in ds if d.task.kind == kind]
        if c:
            return random.choice(c)
    return random.choice(ds) if ds else None


def chaos(action, arg, inflight):
    if action == "restart-agent":  # the container goes away under a working agent
        d = pick_victim(inflight)
        if d:
            rc, _, err = sh([*DOCKER, "restart", d.agent], timeout=180)
            log("chaos", action=action, agent=d.agent, task=d.task.id, rc=rc, error=err.strip()[:200])
    elif action == "kill-pane":  # claude dies inside a running container
        d = pick_victim(inflight)
        if d:
            rc, _, err = sh([*DOCKER, "exec", d.agent, "tmux", "kill-session", "-t", "main"])
            log("chaos", action=action, agent=d.agent, task=d.task.id, rc=rc, error=err.strip()[:200])
    elif action == "restart-fleetd":  # fleetd restarts mid-whatever (a deploy, a crash)
        rc, _, err = sh(["systemctl", "--user", "restart", "fleetd"])
        log("chaos", action=action, rc=rc, error=err.strip()[:200])
    elif action == "run-then-restart-fleetd":  # fleetd dies while it delivers a brief
        t = T.make(random.choice(list(T.GO)), new_id("cut"))
        d = Driver(t, inflight)
        d.start()
        time.sleep(float(arg or 3))
        rc, _, err = sh(["systemctl", "--user", "restart", "fleetd"])
        log("chaos", action=action, task=t.id, rc=rc, error=err.strip()[:200])
        return d
    elif action == "stop-fleetd":  # fleetd away for a while: agents report into the void
        sh(["systemctl", "--user", "stop", "fleetd"])
        log("chaos", action=action, secs=arg)
        time.sleep(float(arg or 120))
        rc, _, err = sh(["systemctl", "--user", "start", "fleetd"])
        log("chaos", action=action + ":back", rc=rc, error=err.strip()[:200])
    elif action == "docker-restart":  # pc2's Docker restarts: every container with it
        rc, _, err = sh(["sh", "-c", f"ssh {PC2} 'sudo -S -p \"\" systemctl restart docker' < ~/.karmax/secrets/pc2_sudo_pass"], timeout=300)
        log("chaos", action=action, rc=rc, error=err.strip()[:200])
    elif action == "partition":  # pc2 cannot reach Kali (fleetd, NFS) for a while
        secs = int(arg or 180)
        # Both ends are timers, so this ssh returns before the cut drops it.
        kali = kali_ip()
        cmd = (f"sudo -S -p '' sh -c 'systemd-run --collect --on-active={secs + 2} --unit=bench-heal iptables -D OUTPUT -d {kali} -j DROP; "
               f"systemd-run --collect --on-active=2 --unit=bench-cut iptables -I OUTPUT -d {kali} -j DROP'")
        rc, _, err = sh(["sh", "-c", f"ssh {PC2} \"{cmd}\" < ~/.karmax/secrets/pc2_sudo_pass"], timeout=60)
        log("chaos", action=action, secs=secs, rc=rc, error=err.strip()[:300])
    elif action == "reboot-pc2":  # a power cut of pc2 alone, near enough
        rc, _, err = sh(["sh", "-c", f"ssh {PC2} 'sudo -S -p \"\" systemctl reboot' < ~/.karmax/secrets/pc2_sudo_pass"], timeout=60)
        log("chaos", action=action, rc=rc, error=err.strip()[:200])
    elif action == "nfs-restart":  # Kali's NFS server restarts under the agents
        rc, _, err = sh(["sh", "-c", "sudo -S -p '' systemctl restart nfs-server < ~/code/secrets/karmax/karmax_sudo_pass"], timeout=120)
        log("chaos", action=action, rc=rc, error=err.strip()[:200])
    else:
        log("chaos_unknown", action=action)


# --- profiles -----------------------------------------------------------------

_seq = [0]

# The held profile's trigger: set once its first question is in.
PARTITION_ON_QUESTION = None


def new_id(kind):
    _seq[0] += 1
    return f"b{time.strftime('%m%d%H%M')}-{kind}-{_seq[0]}{random.randrange(16**3):03x}"


CODE_KINDS = list(T.GO) + list(T.PY)


def profile(name, rng):
    """[(offset seconds, kind)]"""
    if name == "smoke":
        return [(0, "roman"), (0, "rle")]
    if name == "burst":  # more short tasks than agents, all at once
        return [(0, k) for k in rng.sample(CODE_KINDS * 2, 12)]
    if name == "mixed":  # everything, over ten minutes
        kinds = rng.sample(CODE_KINDS, 6) + ["slow", "slow", "chain", "question", "question", "cancel"]
        rng.shuffle(kinds)
        return [(i * 50, k) for i, k in enumerate(kinds)]
    if name == "outage":  # short tasks whose reports land while fleetd is away
        return [(0, k) for k in rng.sample(CODE_KINDS, 6)]
    if name == "held":  # answers sent while pc2 is cut off (cmd_run partitions at the first question)
        return [(0, "question"), (0, "question"), (0, "question"), (0, "roman"), (0, "rle")]
    if name == "long":  # long waits: the shape a power cut hurts most
        return [(i * 20, k) for i, k in enumerate(["chain", "chain", "slow", "slow", "slow", "chain", "question", "roman"])]
    raise SystemExit(f"unknown profile {name}")


def soak(hours, rng, inflight, stop):
    """Keep eight tasks in flight, of every kind, for hours."""
    weights = {k: 3 for k in CODE_KINDS} | {"slow": 2, "chain": 2, "question": 2, "cancel": 1}
    end = time.time() + hours * 3600
    drivers = []
    while time.time() < end and not stop.is_set():
        if len(inflight) + sum(1 for d in drivers if d.is_alive() and not d.agent) < 8:
            k = rng.choices(list(weights), list(weights.values()))[0]
            d = Driver(T.make(k, new_id(k)), inflight)
            d.start()
            drivers.append(d)
            time.sleep(20)
        else:
            time.sleep(15)
    return drivers


def cmd_run(a):
    global _log_file
    out = a.out or os.path.join(os.path.dirname(os.path.abspath(__file__)), "runs", time.strftime("%Y%m%d-%H%M%S") + "-" + a.profile)
    os.makedirs(out, exist_ok=True)
    _log_file = open(os.path.join(out, "log.jsonl"), "a")
    rng = random.Random(a.seed)
    random.seed(a.seed)
    global PARTITION_ON_QUESTION
    PARTITION_ON_QUESTION = threading.Event() if a.profile == "held" else None
    inflight = {}
    plan = sorted((int(at), act, arg) for at, act, arg in (
        (c.split(":") + [""])[:3] for c in a.chaos))
    log("start", profile=a.profile, chaos=a.chaos, seed=a.seed, hours=a.hours)
    start = time.time()
    stop = threading.Event()

    def chaos_loop():
        extra = []
        for at, act, arg in plan:
            while time.time() < start + at and not stop.is_set():
                time.sleep(1)
            if stop.is_set():
                return
            d = chaos(act, arg, inflight)
            if d:
                extra.append(d)
        for d in extra:
            d.join()

    ct = threading.Thread(target=chaos_loop, daemon=True)
    ct.start()
    drivers = []
    if a.profile == "soak":
        drivers = soak(a.hours, rng, inflight, stop)
    else:
        for at, kind in profile(a.profile, rng):
            while time.time() < start + at:
                time.sleep(1)
            t = T.make(kind, new_id(kind))
            if a.profile == "held" and kind == "question":
                t = next(x for x in (T.make(kind, t.id, seed=i) for i in range(100)) if x.answer == "descending")  # not the guess
            d = Driver(t, inflight)
            d.start()
            drivers.append(d)
    for d in drivers:
        d.join()
    stop.set()
    ct.join(timeout=5)
    log("end", secs=round(time.time() - start))
    time.sleep(45)  # the last rotation happens on the tick after done
    print("log:", os.path.join(out, "log.jsonl"))
    report(out)


# --- report -------------------------------------------------------------------

def pct(xs, p):
    xs = sorted(x for x in xs if x is not None)
    if not xs:
        return None
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]


def report(out):
    recs = [json.loads(l) for l in open(os.path.join(out, "log.jsonl"))]
    t_start = min(r["t"] for r in recs)
    t_end = max(r["t"] for r in recs)
    by = {}
    for r in recs:
        if "task" in r:
            by.setdefault(r["task"], []).append(r)
    db = sqlite3.connect(f"file:{LEDGER}?mode=ro", uri=True)
    rows = []
    for tid, rs in by.items():
        ev = {r["kind"]: r for r in reversed(rs)}  # first of each kind
        kind = (ev.get("submit") or {}).get("task_kind", "?")
        agent = (ev.get("assigned") or {}).get("agent")
        sub = ev.get("submit", {}).get("t")
        asg = ev.get("assigned", {}).get("t")
        first_report = ev.get("report", {}).get("t")
        done = ev.get("done", {}).get("t")
        q = lambda sql, *p: db.execute(sql, p).fetchall()
        # fleetd's own record of the assignment: the run returns only once the
        # brief is delivered.
        fa = q("SELECT at FROM events WHERE kind='fleet.assign' AND detail LIKE ? ORDER BY id LIMIT 1", f'%"{tid}"%')
        fa = fa[0][0] if fa else (asg and int(asg))
        row = {"task": tid, "kind": kind, "agent": agent,
               "queued_s": fa - sub if fa and sub else None,
               "deliver_s": asg - fa if asg and fa else None,
               "verified": ([{}] + [r for r in rs if r["kind"] == "verified"])[-1].get("ok"),  # the last: after any fix
               "sent_back": sum(1 for r in rs if r["kind"] == "sent_back"),
               "early": ev.get("verified", {}).get("early"),
               "first_report_s": first_report - fa if first_report and fa else None,
               "done_s": done - fa if done and fa else None,
               "run_failures": sum(1 for r in rs if r["kind"] == "run_failed"),
               "wait_timeouts": sum(1 for r in rs if r["kind"] == "wait_timeout"),
               "phases": [r.get("phase") for r in rs if r["kind"] == "phase"],
               "outcome": "done" if "done" in ev else ("closed" if "closed" in ev else ("deadline" if "deadline" in ev else "unfinished"))}
        # From the ledger: delivery, first turn, resumes, resends, rotation.
        if agent and asg:
            rel = q("SELECT at, delivered, error FROM relays WHERE task=? AND recipient=? ORDER BY id", tid, agent)
            row["relays_to_agent"] = len(rel)
            row["relays_undelivered"] = sum(1 for x in rel if not x[1])
            ups = q("SELECT at FROM events WHERE agent=? AND kind='UserPromptSubmit' AND at>=? ORDER BY at LIMIT 1", agent, fa)
            row["first_turn_s"] = (ups[0][0] - fa) if ups else None
            end = int(done or t_end) + 600
            # By the task each event names, not the agent's time: a later
            # task's resume is not this one's.
            row["resumed"] = q("SELECT COUNT(*) FROM events WHERE agent=? AND kind='fleet.agent.resumed' AND detail LIKE ?", agent, f"%{tid}%")[0][0]
            row["resent"] = q("SELECT COUNT(*) FROM events WHERE kind='fleet.resent' AND detail LIKE ?", f'%"task":"{tid}"%')[0][0]
            row["held"] = q("SELECT COUNT(*) FROM events WHERE kind='fleet.relay' AND detail LIKE ? AND detail LIKE '%\"held\":true%'", f'%"task":"{tid}"%')[0][0]
            row["nudged"] = q("SELECT COUNT(*) FROM events WHERE agent=? AND kind='fleet.agent.idle_on_task' AND at BETWEEN ? AND ?", agent, int(asg), end)[0][0]
            if done:
                rot = q("SELECT at FROM events WHERE agent=? AND kind='fleet.rotate' AND at>=? ORDER BY id LIMIT 1", agent, int(done) - 2)
                row["rotate_s"] = (rot[0][0] - int(done)) if rot else None
            late = q("SELECT COUNT(*) FROM relays WHERE task=? AND sender=? AND at > ?", tid, agent, int(done or ev.get("closed", {}).get("t") or t_end) + 5)
            row["late_reports"] = late[0][0]
        rows.append(row)
    chaos = [r for r in recs if r["kind"] == "chaos"]
    alerts = db.execute("SELECT datetime(at,'unixepoch','localtime'), agent, detail FROM events WHERE kind='fleet.alert' AND at BETWEEN ? AND ?",
                        (int(t_start), int(t_end) + 600)).fetchall()
    with open(os.path.join(out, "tasks.json"), "w") as f:
        json.dump(rows, f, indent=1)

    def stat(key, rs):
        xs = [r.get(key) for r in rs if r.get(key) is not None]
        if not xs:
            return "-"
        return f"{pct(xs, 50):.0f}/{pct(xs, 90):.0f}/{max(xs):.0f}"

    print(f"\n== {os.path.basename(out)}: {len(rows)} tasks over {(t_end - t_start) / 60:.0f} min ==")
    print("seconds as p50/p90/max")
    print(f"{'kind':10} {'n':>3} {'ok':>3} {'queued':>14} {'deliver':>12} {'1st turn':>12} {'1st report':>14} {'done':>14} {'rotate':>10}")
    kinds = sorted({r["kind"] for r in rows})
    for k in kinds + ["ALL"]:
        rs = [r for r in rows if r["kind"] == k or k == "ALL"]
        ok = sum(1 for r in rs if r["verified"]) + sum(1 for r in rs if r["kind"] == "cancel" and r["outcome"] == "closed")
        print(f"{k:10} {len(rs):>3} {ok:>3} {stat('queued_s', rs):>14} {stat('deliver_s', rs):>12} {stat('first_turn_s', rs):>12} "
              f"{stat('first_report_s', rs):>14} {stat('done_s', rs):>14} {stat('rotate_s', rs):>10}")
    print("\nanomalies:")
    for r in rows:
        bad = []
        if r["outcome"] != "done" and not (r["kind"] == "cancel" and r["outcome"] == "closed"):
            bad.append("outcome=" + r["outcome"])
        if r.get("verified") is False:
            bad.append("verification failed")
        if r.get("early"):
            bad.append("reported before its job could have run")
        if r["kind"] not in ("slow", "chain", "cancel") and r.get("wait_timeouts"):
            bad.append(f"wait_timeouts={r['wait_timeouts']}")
        for k in ("run_failures", "relays_undelivered", "held", "sent_back", "resumed", "resent", "nudged", "late_reports"):
            if r.get(k):
                bad.append(f"{k}={r[k]}")
        if r.get("phases"):
            bad.append("phases=" + ",".join(p or "?" for p in r["phases"]))
        if bad:
            print(f"  {r['task']} ({r['kind']}, {r['agent']}): " + "; ".join(bad))
    for c in chaos:
        print(f"  chaos at +{c['t'] - t_start:.0f}s: {c.get('action')} {c.get('agent') or ''} {c.get('task') or ''} rc={c.get('rc')}")
    for a in alerts:
        print(f"  alert {a[0]} {a[1]}: {a[2][:160]}")


def cmd_clean(a):
    rc, out, _ = sh(["git", "--git-dir", ORIGIN, "for-each-ref", "--format=%(refname:short)", "refs/heads/"])
    gone = [b for b in out.split() if b != "main"]
    for b in gone:
        sh(["git", "--git-dir", ORIGIN, "branch", "-D", b])
    print(f"deleted {len(gone)} branches from {ORIGIN}")


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--profile", required=True)
    r.add_argument("--out")
    r.add_argument("--seed", type=int, default=int(time.time()))
    r.add_argument("--hours", type=float, default=2)
    r.add_argument("--chaos", nargs="*", default=[])
    rp = sub.add_parser("report")
    rp.add_argument("dir")
    sub.add_parser("clean")
    a = p.parse_args()
    if a.cmd == "run":
        cmd_run(a)
    elif a.cmd == "report":
        report(a.dir)
    else:
        cmd_clean(a)


if __name__ == "__main__":
    main()
