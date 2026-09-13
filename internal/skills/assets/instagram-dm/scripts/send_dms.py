#!/usr/bin/env python3
"""Send paced, resumable DMs to a collected commenter list.

The one behaviour that must not be wrong: progress.jsonl records an
`attempted` event BEFORE the request and a `sent` event AFTER it succeeds.
On any resume — crash, kill, a second invocation over the same files — a
recipient with `attempted` and no `sent` is SKIPPED, never retried. Missing
one DM is a smaller failure than sending two.

--dry-run runs this entire pipeline — ledger writes, pacing, the cap, task
status checks — and substitutes a simulated success for the real network
call. It is what this skill runs first, always.

--task-id is optional. With it absent, the daemon-reporting function below
is never called, and this script still runs end to end and still writes
progress.jsonl with no daemon involved at all — that's what makes the
resume property testable with no network and no daemon (see the
instagram-dm task's Step 4 test). With it present, this script reports
{sent, attempted, total} to a `tasks` row once per recipient and reads the
current status back from that SAME call — but the task row is
observability, not a dependency: any failure to reach it is logged once
and otherwise ignored, and progress.jsonl (not the daemon) remains the
authoritative ledger regardless of whether a single report ever lands.

Daemon contract this script assumes (Task 10, landing concurrently with
this one — see this task's report for exactly which parameter names were
assumed here, to reconcile against what Task 10 actually shipped): the
route is the `karmax` CLI, not raw HTTP and never the daemon's SQLite file
directly —

    karmax tool call task.progress task_id=<id> sent=<n> attempted=<n> total=<n> last_at=<iso8601>

— printing the tool's JSON result to stdout on success (exit 0), with a
"status" field in it ("running" | "paused" | "cancelled" | ...) that IS the
control signal this script reads back; a non-zero exit or unparseable
output is treated as "reporting unavailable right now", never a crash. All
of that is isolated in one method, TaskReporter.report() below, so if
Task 10 ships slightly different parameter or field names, that method is
what changes — not the send loop.

Zero third-party dependencies beyond the `karmax` CLI itself: stdlib only
(urllib for the actual DM request, subprocess for the CLI call), so nothing
needs installing before a run.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def redact(err: BaseException) -> str:
    """A short, safe-to-print description of a failure — never the
    exception's raw str(), which for some HTTP/urllib errors can embed
    request details. Cookie and header values must never reach output,
    including inside an error message."""
    if isinstance(err, urllib.error.HTTPError):
        return f"HTTP {err.code} {err.reason}"
    if isinstance(err, urllib.error.URLError):
        return f"connection error ({type(err.reason).__name__})"
    return type(err).__name__


# ---------------------------------------------------------------------------
# Recipients and the progress ledger
# ---------------------------------------------------------------------------

def load_recipients(path: str) -> list[dict]:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    if not isinstance(data, list):
        raise SystemExit(f"--recipients {path!r} must be a JSON list")
    seen = set()
    out = []
    for row in data:
        if not isinstance(row, dict) or "id" not in row:
            continue
        rid = str(row["id"])
        if rid in seen:
            continue
        seen.add(rid)
        out.append({"id": rid, "username": row.get("username")})
    return out


def load_ledger(progress_path: str) -> tuple[set[str], set[str]]:
    """Read progress.jsonl into (attempted_ids, sent_ids).

    A missing file is a fresh run, not an error. A malformed trailing line
    — plausible if a previous run was killed mid-write — is skipped with a
    warning rather than aborting the whole resume; the ledger is the thing
    crash-safety depends on, so reading it must itself be crash-tolerant.
    """
    attempted: set[str] = set()
    sent: set[str] = set()
    if not os.path.exists(progress_path):
        return attempted, sent
    with open(progress_path, "r", encoding="utf-8") as f:
        for lineno, line in enumerate(f, start=1):
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                print(f"progress.jsonl:{lineno}: unparseable line, skipping it", file=sys.stderr)
                continue
            rid = rec.get("recipient_id")
            event = rec.get("event")
            if rid is None:
                continue
            rid = str(rid)
            if event == "attempted":
                attempted.add(rid)
            elif event == "sent":
                sent.add(rid)
    return attempted, sent


def append_event(fp, event: str, recipient: dict, dry_run: bool, extra: dict | None = None) -> None:
    """Write one ledger line and make it durable before returning.

    The fsync is what makes the "attempted written before the request"
    guarantee real rather than theoretical — a write sitting in a buffer
    when the process is killed is indistinguishable from a write that never
    happened.
    """
    rec = {
        "event": event,
        "recipient_id": recipient["id"],
        "username": recipient.get("username"),
        "ts": now_iso(),
    }
    if dry_run:
        rec["dry_run"] = True
    if extra:
        rec.update(extra)
    fp.write(json.dumps(rec) + "\n")
    fp.flush()
    os.fsync(fp.fileno())


# ---------------------------------------------------------------------------
# The task row — optional, best-effort, and isolated to this one function
# ---------------------------------------------------------------------------

class TaskReporter:
    """Reports {sent, attempted, total} to a tasks row via `karmax tool call
    task.progress` and reads the run's control status back from that same
    call's result.

    This is the one place the daemon-integration guess lives (see the
    module docstring and this task's report). Every failure — the `karmax`
    binary missing, the daemon unreachable, a non-zero exit, unparseable
    output, a missing "status" key — is swallowed and returns None, meaning
    "unknown, proceed as running." The task row is observability, not a
    dependency: a send run must complete correctly with this function
    returning None on every single call. A failure is logged once, not on
    every recipient, so an hours-long run against a down daemon doesn't
    spam its own transcript.
    """

    def __init__(self, karmax_bin: str, task_id: str, timeout: float = 15.0):
        self.karmax_bin = karmax_bin
        self.task_id = task_id
        self.timeout = timeout
        self._warned = False

    def _warn(self, reason: str) -> None:
        if not self._warned:
            print(
                f"task reporting unavailable ({reason}); continuing without it — "
                "progress.jsonl remains the real record",
                file=sys.stderr,
            )
            self._warned = True

    def report(self, sent: int, attempted: int, total: int) -> str | None:
        args = [
            self.karmax_bin, "tool", "call", "task.progress",
            f"task_id={self.task_id}", f"sent={sent}", f"attempted={attempted}",
            f"total={total}", f"last_at={now_iso()}",
        ]
        try:
            proc = subprocess.run(args, capture_output=True, text=True, timeout=self.timeout)
        except (OSError, subprocess.TimeoutExpired) as e:
            self._warn(redact(e))
            return None
        if proc.returncode != 0:
            self._warn((proc.stderr or "").strip().splitlines()[-1] if proc.stderr else f"exit {proc.returncode}")
            return None
        try:
            result = json.loads(proc.stdout)
        except json.JSONDecodeError:
            self._warn("unparseable output")
            return None
        if not isinstance(result, dict):
            self._warn("unexpected output shape")
            return None
        status = result.get("status")
        return status if isinstance(status, str) else None


# ---------------------------------------------------------------------------
# Sending
# ---------------------------------------------------------------------------

def load_cookie_header(path: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        data = json.load(f)
    try:
        os.chmod(path, stat.S_IRUSR | stat.S_IWUSR)  # enforce 0600 regardless of how it arrived
    except OSError:
        pass
    if not isinstance(data, dict) or not data:
        raise SystemExit(f"cookie file {path!r} did not contain a JSON object of cookies")
    print(f"loaded {len(data)} cookie(s) from {path}: {', '.join(sorted(data))}", file=sys.stderr)
    return "; ".join(f"{k}={v}" for k, v in data.items())


def load_headers(path: str | None) -> dict:
    if not path:
        return {}
    with open(path, "r", encoding="utf-8") as f:
        headers = json.load(f)
    if not isinstance(headers, dict):
        raise SystemExit(f"headers file {path!r} must be a JSON object")
    return headers


def load_payload_template(path: str):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def substitute(obj, recipient_id: str, text: str):
    """Walk a JSON value replacing the literal tokens {recipient_id} and
    {text} inside any string, recursively through dicts/lists."""
    if isinstance(obj, str):
        return obj.replace("{recipient_id}", recipient_id).replace("{text}", text)
    if isinstance(obj, dict):
        return {k: substitute(v, recipient_id, text) for k, v in obj.items()}
    if isinstance(obj, list):
        return [substitute(v, recipient_id, text) for v in obj]
    return obj


def send_dm(endpoint: str, cookie_header: str, extra_headers: dict, payload_template,
            recipient: dict, text: str, timeout: float) -> tuple[bool, str | None]:
    body_obj = substitute(payload_template, recipient["id"], text)
    if isinstance(body_obj, (dict, list)):
        data = json.dumps(body_obj).encode("utf-8")
        content_type = "application/json"
    else:
        data = str(body_obj).encode("utf-8")
        content_type = "application/x-www-form-urlencoded"

    req = urllib.request.Request(endpoint, data=data, method="POST")
    req.add_header("Cookie", cookie_header)
    if "content-type" not in {k.lower() for k in extra_headers}:
        req.add_header("Content-Type", content_type)
    for k, v in extra_headers.items():
        req.add_header(k, v)

    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            resp.read()
            return True, None
    except (urllib.error.HTTPError, urllib.error.URLError) as e:
        return False, redact(e)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def parse_args(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--recipients", required=True, help="Path to commenters.json (a JSON list of {id, username}).")
    p.add_argument("--progress", required=True, help="Path to progress.jsonl — the crash-safe ledger. Never deleted.")
    p.add_argument("--dry-run", action="store_true", help="Run the whole pipeline but substitute a simulated send for the real request. This is the default first invocation.")
    p.add_argument("--message", default=None, help='Message template. "{username}" and "{link}" are substituted. Required for a live (non-dry-run) send.')
    p.add_argument("--link", default="", help="Link substituted into --message's {link}.")
    p.add_argument("--endpoint", default=None, help="DM-send URL, observed from real traffic (SKILL.md Step 2). Required for a live send.")
    p.add_argument("--cookie-file", default=None, help="Path to the 0600 harvested-session JSON file. Required for a live send.")
    p.add_argument("--headers-file", default=None, help="Path to a JSON object of extra headers observed from real traffic.")
    p.add_argument("--payload-template", default=None, help="Path to a JSON body template with {recipient_id} and {text} tokens, captured from the real DM request. Required for a live send.")
    p.add_argument("--cap", type=int, default=25, help="Maximum NEW recipients attempted in this run (default: 25, conservative on purpose).")
    p.add_argument("--interval-seconds", type=float, default=45.0, help="Base pacing interval between sends (default: 45).")
    p.add_argument("--jitter-seconds", type=float, default=15.0, help="Random +/- jitter added to the interval (default: 15).")
    p.add_argument("--max-consecutive-failures", type=int, default=3, help="Halt after this many sends in a row fail (default: 3).")
    p.add_argument("--timeout", type=float, default=20.0, help="Per-request timeout in seconds (default: 20).")
    p.add_argument("--task-id", default=None, help="Optional. A tasks-row id from task.start, for progress reporting and pause/cancel. Absent means no daemon call is ever made.")
    p.add_argument("--karmax-bin", default="karmax", help="The karmax CLI to shell out to for task reporting (default: karmax, found on PATH).")
    p.add_argument("--pause-poll-seconds", type=float, default=10.0, help="How often to re-report (and so re-check status) while paused (default: 10).")
    return p.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv)

    if not args.dry_run:
        missing = [name for name, val in [
            ("--endpoint", args.endpoint),
            ("--cookie-file", args.cookie_file),
            ("--payload-template", args.payload_template),
            ("--message", args.message),
        ] if not val]
        if missing:
            print(f"a live send needs {', '.join(missing)} (or run with --dry-run)", file=sys.stderr)
            return 1

    recipients = load_recipients(args.recipients)
    attempted_ids, sent_ids = load_ledger(args.progress)
    eligible = [r for r in recipients if r["id"] not in attempted_ids]
    already_done = len(recipients) - len(eligible)

    print(f"{len(recipients)} recipient(s) total; {already_done} already have an attempted "
          f"record (skipped, never retried — {len(sent_ids)} of those confirmed sent); "
          f"{len(eligible)} eligible this run", file=sys.stderr)

    cookie_header = load_cookie_header(args.cookie_file) if args.cookie_file else ""
    extra_headers = load_headers(args.headers_file)
    payload_template = load_payload_template(args.payload_template) if args.payload_template else None

    reporter = TaskReporter(args.karmax_bin, args.task_id) if args.task_id else None

    total_sent = len(sent_ids)
    total_attempted = len(attempted_ids)
    consecutive_failures = 0
    new_attempts = 0
    new_sends = 0
    halted = False
    cancelled = False

    try:
        with open(args.progress, "a", encoding="utf-8") as progress_fp:
            for recipient in eligible:
                if new_attempts >= args.cap:
                    print(f"reached --cap {args.cap} for this run, stopping", file=sys.stderr)
                    break

                if consecutive_failures >= args.max_consecutive_failures:
                    print(f"{consecutive_failures} consecutive failures, halting — this is the signal "
                          "to stop and look, not to keep grinding", file=sys.stderr)
                    halted = True
                    break

                append_event(progress_fp, "attempted", recipient, args.dry_run)
                total_attempted += 1
                new_attempts += 1

                if args.dry_run:
                    ok, err = True, None
                else:
                    text = (args.message or "").format(
                        username=recipient.get("username") or recipient["id"], link=args.link
                    )
                    ok, err = send_dm(args.endpoint, cookie_header, extra_headers,
                                       payload_template, recipient, text, args.timeout)

                if ok:
                    append_event(progress_fp, "sent", recipient, args.dry_run)
                    total_sent += 1
                    new_sends += 1
                    consecutive_failures = 0
                else:
                    append_event(progress_fp, "failed", recipient, args.dry_run, {"error": err})
                    consecutive_failures += 1
                    print(f"send to {recipient['id']} failed: {err}", file=sys.stderr)

                if reporter is not None:
                    status = reporter.report(total_sent, total_attempted, len(recipients))
                    while status == "paused":
                        print("task paused, idling", file=sys.stderr)
                        time.sleep(args.pause_poll_seconds)
                        status = reporter.report(total_sent, total_attempted, len(recipients))
                    if status == "cancelled":
                        print("task cancelled, stopping cleanly (progress.jsonl left intact)", file=sys.stderr)
                        cancelled = True
                        break

                is_last = recipient is eligible[-1]
                if not is_last and new_attempts < args.cap and not halted:
                    delay = max(0.0, args.interval_seconds + random.uniform(-args.jitter_seconds, args.jitter_seconds))
                    time.sleep(delay)
    finally:
        if args.cookie_file and os.path.exists(args.cookie_file):
            try:
                os.remove(args.cookie_file)
                print(f"deleted {args.cookie_file}", file=sys.stderr)
            except OSError as e:
                print(f"could not delete {args.cookie_file}: {redact(e)}", file=sys.stderr)

    print(f"this run: {new_attempts} attempted, {new_sends} sent, "
          f"{new_attempts - new_sends} failed. lifetime: {total_sent}/{len(recipients)} sent."
          + (" (stopped: task cancelled)" if cancelled else ""))

    if halted:
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
