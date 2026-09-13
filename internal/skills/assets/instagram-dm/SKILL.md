---
name: instagram-dm
description: Use when the operator asks to DM everyone who commented on one of their own Instagram posts or reels — "send a DM link to everyone who commented on my reel" and its variants. Turns the shared browser's already-signed-in Instagram session into a capped, resumable, opt-in-only send. Never reach for this to message an imported list, a follower scrape, or a hashtag search — it has no path to any of those, by design.
---

## What this is, and the line it will not cross

This is a comment-to-DM tool: it messages people who commented on **one post
the operator owns**, because commenting on it is how they opted in. It reads
that post's comments and nothing else — no imported list, no follower scrape,
no hashtag or explore-page search. If the operator asks for any of those,
that is a different product; say so and stop rather than bending this one to
fit.

Three more things keep this from turning into a spam tool, and they are not
optional polish:

- **One message per person, ever.** The dedupe when collecting commenters and
  the `attempted`/`sent` ledger when sending both exist to guarantee this. A
  person who already got the message never gets it again, including across a
  crashed and resumed run.
- **Capped, and dry-run first.** The very first invocation of the send script
  is `--dry-run`. It produces the real plan — how many people, in what order
  — and sends nothing. A live run has a per-run cap as a required-looking
  argument with a conservative default, not a constant buried in code.
- **It is the operator's own account.** Everything here rides on the browser
  session they signed into themselves. This skill has no sign-in flow and
  must never attempt one on their behalf.

## Preconditions — checked, not assumed

Before anything else:

1. Is the browser running? Check with the `browser` tool's `status` action.
   If it is not running, say so and stop. Do not start it and do not sign
   in for the operator.
2. Is Instagram actually signed in, in that browser? Navigate to
   `instagram.com` with Playwright MCP and look at what loads — a feed or a
   login form. If it's a login form, say so and stop. Signing in is the
   operator's step, not this skill's.

Only once both are true does the rest of this procedure start.

## Step 1 — Harvest and discover together: capturing headers *is* the harvest

There is no separate "read the cookies, then write them" step. That was the
original design here, and it does not work: `browser_run_code_unsafe` runs
in a **sandboxed VM with no Node API at all** — checked directly against the
`@playwright/mcp` version KARMAX pins: `typeof require` and `typeof process`
are both `"undefined"` in that context, and `await import(...)` throws "A
dynamic import callback was not specified." There is no way to
`page.context().cookies()` and then write a file from inside that call.
Don't attempt it.

What actually gets a credential to disk without it ever crossing back into
the conversation transcript is `browser_network_request`'s own `filename`
parameter — documented as "Filename to save the result to. If not provided,
output is returned as text." It writes in the MCP server's own process,
straight to disk; the tool's result back to you is just a confirmation, not
the content. And since a captured request's headers already include the
real `Cookie:` header the browser sent, **harvesting and discovering an
endpoint are the same action** — capture the headers of a real request to
the endpoint you need, and the credential comes with it.

**First, a private directory — this is what actually protects the file,
not its own permission bits.** Use `shell.exec` to create it with the mode
set at creation: `mkdir -m 700 <workdir>/.ig-capture`. Don't route anything
under it through `file.write` — that tool always creates files `0644` with
no way to ask for tighter, so a chmod after the fact would leave a window;
the directory being unreadable by anyone else is what has to do the work
here, and it has to exist before anything is captured into it.

**Then, per endpoint that's needed:**

1. Navigate to the reel and open its comments (or open the DM composer, for
   the send endpoint below) — this is also what generates the authenticated
   request that gets captured.
2. `browser_network_requests(filter: "instagram.com")` to see what fired,
   and find the row for the request that actually did it.
3. `browser_network_request(index: <that row's number>, part: "request-headers", filename: "<absolute path inside .ig-capture>")`.
   `part`'s accepted values are exactly `request-headers`, `request-body`,
   `response-headers`, `response-body` — confirmed from the tool's own
   parameter schema. Use an absolute path; always inside the `0700`
   directory.
   - For the **comment-list** request: save to
     `<workdir>/.ig-capture/comments-headers.txt`.
   - For the **DM-send** request: save headers to
     `<workdir>/.ig-capture/dm-headers.txt`, **and also** capture
     `part: "request-body"` to `<workdir>/.ig-capture/payload-template.json`
     — the exact field shape (recipient field name, text field name, any
     `client_context`/action fields Instagram expects) that `send_dms.py`
     needs and this skill deliberately does not guess at. In that template,
     replace the one real recipient id with the literal token
     `{recipient_id}` and the message text with the literal token `{text}`.
4. **Two things here were not verified against a live Instagram capture in
   this environment — check them on the first real run, not later:**
   - *Where the file lands.* The `filename` parameter's relative-vs-absolute
     resolution, and its interaction with an `--output-dir` flag (KARMAX
     passes none), weren't confirmed. Passing an absolute path sidesteps the
     ambiguity; after the very first capture, confirm the file actually
     exists at that exact path (`file.read` it, or `shell.exec ls`) before
     trusting anything downstream.
   - *What the captured content looks like.* `collect_comments.py` and
     `send_dms.py` both parse the file defensively — a JSON object, a JSON
     array of `{name, value}` pairs, or plain `Name: value` lines — and log
     which one matched. Check that log line on the first run; if none of
     the three match, the parser needs a fourth branch added.
5. **Delete `<workdir>/.ig-capture/` entirely when the run finishes or
   aborts.** `send_dms.py` deletes the one DM-headers file it was given on
   every exit path, but the directory as a whole — and
   `comments-headers.txt`, which nothing else deletes — is this skill's own
   job. Do it yourself if the procedure aborts before `send_dms.py` ever
   runs.
6. Record what was observed — endpoint URLs and header *names* (never
   values) — into the run directory too, so a later run can diff against it
   and notice when Instagram has changed something.

## Step 2 — Collect the commenters fully, before sending anything

```
python3 scripts/collect_comments.py \
  --endpoint "<observed comment-list endpoint>" \
  --headers-file <workdir>/.ig-capture/comments-headers.txt \
  --out <workdir>/commenters.json
```

It paginates the comment endpoint with the harvested cookies until exhausted,
dedupes by user id, and writes `commenters.json`. Do this fully — all pages —
before a single DM goes out; a few hundred comments is seconds of paged
requests, and the run needs to know its own size before it starts sending.
`collect_comments.py --help` lists the field-name overrides if Instagram's
response shape isn't the tool's defaults — check the response you captured
in Step 1 against them.

## Step 3 — Register the task row, before launching the send

A paced send can run for hours, which is longer than any turn. Before
starting it:

```
task.start(goal="DM everyone who commented on <reel url/description>", target="<reel url>", total=<count from commenters.json>)
```

Keep the returned `task_id` — pass it to `send_dms.py --task-id`. This is
what gives the run a live status card and a pause/cancel control; call
`task.start` **before** handing the send off, not after.

## Step 4 — Send, dry-run first, then detached and resumable

First invocation, always:

```
python3 scripts/send_dms.py --dry-run \
  --recipients <workdir>/commenters.json \
  --progress <workdir>/progress.jsonl \
  --message "Hey {username}, here's the link: {link}" --link "<link>" \
  --endpoint "<observed DM-send endpoint>" \
  --headers-file <workdir>/.ig-capture/dm-headers.txt \
  --payload-template <workdir>/.ig-capture/payload-template.json \
  --task-id <task_id>
```

`--dry-run` runs the entire pipeline — ledger writes, pacing, the cap, task
status checks — and substitutes a simulated send for the real network call.
Report the plan it prints (recipient count, cap, pace) to the operator before
doing anything live.

Only after the operator has seen that plan, launch the live run the same way
without `--dry-run`, and **detached** — `claude_code.call` with
`background: true`, or a plain detached process. Never inside the turn that
starts it: chat/agent/task turns time out in minutes, and a real run is
hours. The turn that starts it returns immediately with the task id and the
count; completion arrives later as an event.

### The resume contract — the one thing that must not be wrong

`send_dms.py` writes one JSON record per recipient to `progress.jsonl`:
`attempted` **before** the request goes out, `sent` **after** it succeeds.
On any resume — a restart after a crash, a kill-and-relaunch, a second
invocation over the same files — **a recipient with an `attempted` record
and no `sent` record is skipped, not retried.** Missing one DM is a smaller
failure than sending two; this skill would rather under-deliver than double-
message anyone. `progress.jsonl` is never deleted and never rewritten,
including on `--dry-run`, on a failure-streak halt, or on cancellation —
whatever it holds when a run stops is exactly what the next run resumes from.

### Pause and cancel, through the task row

`send_dms.py` reports `{sent, attempted, total}` to the task row once per
recipient via `karmax tool call task.progress ...`, and reads the run's
current `status` back from that same call's result:

- `paused` — it idles (without exiting) until the status changes.
- `cancelled` — it stops cleanly. `progress.jsonl` is left exactly as it is;
  that ledger is what makes a later resume safe.
- Anything else, or the daemon being unreachable — it proceeds. The task row
  is observability, not a dependency: `--task-id` is optional, and even when
  it's given, a failure to reach the daemon is logged once and never blocks
  sending. `progress.jsonl` is the real record regardless of whether the
  daemon ever sees a single progress update.

To pause or resume a run from a conversation — the operator does not need
to have kept the task id:

- `karmax tool call task.status` with no arguments lists open
  running/paused tasks.
- `karmax tool call task.status task_id=<id> status=paused` pauses one;
  `status=running` resumes it.

## Never

- Never send to anyone who did not comment on the specific post this run was
  pointed at.
- Never retry a recipient that already has an `attempted` record, sent or
  not.
- Never print, log, or return the contents of anything under
  `.ig-capture/`, any cookie value, or the raw `Cookie`/`Authorization`
  header — including inside an error message.
- Never attempt to sign in, solve a captcha, or handle an OTP on the
  operator's behalf. Hand the browser back to them with `browser` `open` and
  say what's needed.
