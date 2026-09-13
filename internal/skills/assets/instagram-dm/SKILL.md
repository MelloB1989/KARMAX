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

## Step 1 — Harvest the session, straight to a 0600 file

The credential here is a full account session. Handle it as one:

- Read it with `browser_run_code_unsafe` running `page.context().cookies()`
  (Playwright's own API, backed by CDP — not `document.cookie`, which cannot
  see the httpOnly `sessionid` cookie; see the `browser-sessions` skill).
- Filter to the cookies on `instagram.com`, and **write them to disk in that
  same `browser_run_code_unsafe` call** — do not return the cookie values as
  the tool's result and write them with a second tool call. A value that
  crosses back into the conversation as a tool result is logged in the
  transcript, which is exactly what must not happen. One call, in-process,
  reads and writes.
- Write to `<workdir>/.ig-session.json`, and set its mode to `0600` in that
  same call (e.g. `require('fs').writeFileSync(path, JSON.stringify(cookies), {mode: 0o600})`).
  Node's `writeFileSync` mode is applied at file-creation time; if the file
  might already exist from a prior attempt, `chmod` it to `0o600` right after
  as a second, cheap belt-and-suspenders step.
- Return only non-secret confirmation from the call — which cookie names were
  found (`sessionid`, `csrftoken`, `ds_user_id`, `mid`, `ig_did`) and a count.
  Never the values, never in an error message either.
- This file is never logged, never echoed into the transcript, and is
  **deleted when the run finishes or aborts.** `send_dms.py` deletes it
  itself on every exit path once sending has started. If the procedure
  aborts *before* `send_dms.py` ever runs (collect step fails, operator
  cancels), delete `<workdir>/.ig-session.json` yourself before stopping.

## Step 2 — Discover the endpoints from real traffic, never from memory

Instagram's internal endpoints are unversioned and move. Hardcoding one from
memory is how this breaks silently weeks later — so don't; observe it fresh
every time this skill runs.

1. Navigate to the reel and open its comments.
2. `browser_network_requests` to see what fired; filter to the request that
   actually loaded the comments.
3. `browser_network_request` on that request with `part: "request-headers"`
   for the full header set Instagram sent — `x-csrftoken`, `x-ig-app-id`,
   `x-asbd-id`, `user-agent`, and whatever else is present. Save this as
   `<workdir>/headers.json`.
4. Open the message composer and send yourself (or note, without sending) far
   enough to capture the DM-send request in the network log the same way.
   Capture **both** `part: "request-headers"` and `part: "request-body"` for
   it — the body matters here: it is the exact field shape (recipient field
   name, text field name, any `client_context`/action fields Instagram
   expects) that `send_dms.py` needs and this skill deliberately does not
   guess at. Save the body as a template at `<workdir>/payload-template.json`,
   replacing the one recipient id in it with the literal token
   `{recipient_id}` and the message text with the literal token `{text}`.
5. Record what was observed — endpoint URLs, header keys (not cookie values),
   and the payload template — into the run directory, so a later run can
   diff against it and notice when Instagram has changed something.

## Step 3 — Collect the commenters fully, before sending anything

```
python3 scripts/collect_comments.py \
  --endpoint "<observed comment-list endpoint>" \
  --cookie-file <workdir>/.ig-session.json \
  --headers-file <workdir>/headers.json \
  --out <workdir>/commenters.json
```

It paginates the comment endpoint with the harvested cookies until exhausted,
dedupes by user id, and writes `commenters.json`. Do this fully — all pages —
before a single DM goes out; a few hundred comments is seconds of paged
requests, and the run needs to know its own size before it starts sending.
`collect_comments.py --help` lists the field-name overrides if Instagram's
response shape isn't the tool's defaults — check the response you captured
in Step 2 against them.

## Step 4 — Register the task row, before launching the send

A paced send can run for hours, which is longer than any turn. Before
starting it:

```
task.start(goal="DM everyone who commented on <reel url/description>", target="<reel url>", total=<count from commenters.json>)
```

Keep the returned `task_id` — pass it to `send_dms.py --task-id`. This is
what gives the run a live status card and a pause/cancel control; call
`task.start` **before** handing the send off, not after.

## Step 5 — Send, dry-run first, then detached and resumable

First invocation, always:

```
python3 scripts/send_dms.py --dry-run \
  --recipients <workdir>/commenters.json \
  --progress <workdir>/progress.jsonl \
  --message "Hey {username}, here's the link: {link}" --link "<link>" \
  --endpoint "<observed DM-send endpoint>" \
  --cookie-file <workdir>/.ig-session.json \
  --headers-file <workdir>/headers.json \
  --payload-template <workdir>/payload-template.json \
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

Before each recipient, `send_dms.py` re-reads the task's `status`:

- `paused` — it idles (without exiting) until the status changes.
- `cancelled` — it stops cleanly. `progress.jsonl` is left exactly as it is;
  that ledger is what makes a later resume safe.
- Anything else, or the daemon being unreachable — it proceeds. The task row
  is observability, not a dependency: `--task-id` is optional, and even when
  it's given, a failure to reach the daemon is logged once and never blocks
  sending. `progress.jsonl` is the real record regardless of whether the
  daemon ever sees a single progress update.

## Never

- Never send to anyone who did not comment on the specific post this run was
  pointed at.
- Never retry a recipient that already has an `attempted` record, sent or
  not.
- Never print, log, or return the contents of `.ig-session.json`, any cookie
  value, or the raw `Cookie`/`Authorization` header — including inside an
  error message.
- Never attempt to sign in, solve a captcha, or handle an OTP on the
  operator's behalf. Hand the browser back to them with `browser` `open` and
  say what's needed.
