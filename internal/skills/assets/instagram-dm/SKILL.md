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
  argument with a conservative default, not a constant buried in code, and
  needs its own explicit `--live` flag — there is no default mode, and
  passing neither `--dry-run` nor `--live` refuses to run rather than
  guessing.
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
not its own permission bits.** Use `shell.exec` to create it, relative to
the session workdir, with the mode set at creation: `mkdir -m 700
.ig-capture`. Don't route anything under it through `file.write` — that
tool always creates files `0644` with no way to ask for tighter, so a
chmod after the fact would leave a window; the directory being unreadable
by anyone else is what has to do the work here, confirmed end to end: a
capture into a pre-created `0700` directory landed as `drwx------` on the
directory and an ordinary `0644` on the file inside it, and the `0644`
file is harmless because nothing else can traverse into the directory to
reach it. It has to exist before anything is captured into it.

**`filename` takes a path relative to the session workdir — never an
absolute one.** This is the opposite of what an earlier version of this
skill said, and it's confirmed by running it: an absolute path outside the
server's allowed roots (`<cwd>` and `<cwd>/.playwright-mcp`) is flatly
rejected —

```
Error: File access denied: /private/tmp/.../hdr-abs-probe.txt is outside allowed roots.
Allowed roots: <cwd>/.playwright-mcp, <cwd>
```

— and a relative path lands directly under the session's own working
directory (**not** under `.playwright-mcp`, despite the tool's own result
sometimes rendering the link as `./name.txt`). So: always pass a plain
relative path like `.ig-capture/comments-headers.txt`, the same relative
form `shell.exec`'s `mkdir` used above — both tools share the session
workdir as their cwd. This relative form is only for the
`browser_network_request` call itself; once written, the file also has the
ordinary absolute path `<workdir>/.ig-capture/<name>` — that's what Steps
2 and 4 below pass to the Python scripts, since a plain `open()` call has
no such allowed-roots restriction.

**Then, per endpoint that's needed:**

1. Navigate to the reel and open its comments (or open the DM composer, for
   the send endpoint below) — this is also what generates the authenticated
   request that gets captured.
2. `browser_network_requests(filter: "instagram.com")` to see what fired,
   and find the row for the request that actually did it.
3. `browser_network_request(index: <that row's number>, part: "request-headers", filename: ".ig-capture/<name>.txt")`
   — a relative path, inside the `0700` directory, as above. `part`'s
   accepted values are exactly `request-headers`, `request-body`,
   `response-headers`, `response-body`.
   - For the **comment-list** request: save to
     `.ig-capture/comments-headers.txt`.
   - For the **DM-send** request: save headers to
     `.ig-capture/dm-headers.txt`, **and also** capture
     `part: "request-body"` to `.ig-capture/payload-template.json`
     — the exact field shape (recipient field name, text field name, any
     `client_context`/action fields Instagram expects) that `send_dms.py`
     needs and this skill deliberately does not guess at. In that template,
     replace the one real recipient id with the literal token
     `{recipient_id}` and the message text with the literal token `{text}`.
4. The captured file's shape is confirmed too: plain `Name: value` text,
   one header per line, with names arriving **lowercased** (`cookie:`, not
   `Cookie:`) — both scripts already parse this shape as the expected one
   and apply every header generically, so the lowercasing doesn't matter
   downstream, but it's worth knowing if anything else ever does a
   case-sensitive lookup on this file. What genuinely remains unverified
   is Instagram's *response* shape for the comment-list endpoint — see
   Step 2.
5. **Delete `.ig-capture/` entirely once no more runs of any kind are
   coming for this campaign.** `send_dms.py --live` deletes the one
   DM-headers file it was given, but only on a genuine finish — every
   eligible recipient processed with no pause point below in play. It
   deliberately preserves the file, and expects to be re-run with the
   identical `--live` command, in four cases:
   - `--dry-run` (Step 4's dry-run and live commands reuse the same file
     on purpose).
   - Hitting `--cap` — a normal pause point per spec §3, not a finish.
   - A failure-streak halt — usually a transient blip or a brief
     rate-limit, the most likely reason to resume, so it would defeat the
     point to force a fresh browser harvest just to retry it.
   - The task being `cancelled` through the task row — the same reasoning
     as `--cap`: a pause the operator can resume from, not a declaration
     that the campaign is over.

   None of those four delete anything, so the directory as a whole — and
   `comments-headers.txt`, which nothing ever deletes on its own — is this
   skill's own job whenever a run genuinely will not be resumed:
   - After the campaign's last live run completes normally.
   - If the procedure aborts before `send_dms.py` ever runs.
   - **After a `--dry-run` whose plan the operator decided not to act
     on.** This is the case easiest to miss: `send_dms.py` ran and exited
     cleanly, so nothing crashed and nothing else will ever trigger
     cleanup — a full credential sits in `.ig-capture/` with nothing
     pointing back at it until you delete it yourself, the moment that
     decision is made.
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
Its ledger writes are tagged as simulated and do not count toward anyone
being "already messaged" — a dry-run can never starve the live run that
follows it. Report the plan it prints (recipient count, cap, pace) to the
operator before doing anything live.

Only after the operator has seen that plan, launch the live run — the same
command, with `--dry-run` replaced by `--live` (never omitted outright:
`send_dms.py` refuses to run with neither flag, and refuses `--endpoint`/
`--headers-file`/`--payload-template`/`--message` missing under `--live`)
— and **detached**: `claude_code.call` with `background: true`, or a plain
detached process. Never inside the turn that starts it: chat/agent/task
turns time out in minutes, and a real run is hours. The turn that starts it
returns immediately with the task id and the count; completion arrives
later as an event.

```
python3 scripts/send_dms.py --live \
  --recipients <workdir>/commenters.json \
  --progress <workdir>/progress.jsonl \
  --message "Hey {username}, here's the link: {link}" --link "<link>" \
  --endpoint "<observed DM-send endpoint>" \
  --headers-file <workdir>/.ig-capture/dm-headers.txt \
  --payload-template <workdir>/.ig-capture/payload-template.json \
  --task-id <task_id>
```

The same `.ig-capture/dm-headers.txt` from Step 1 is reused here on
purpose — the credential file survives a `--dry-run` exit, a
`--cap`-limited exit, a failure-streak halt, and a `cancelled` task,
specifically so a follow-up `--live` command can reuse it without
re-harvesting. It is deleted automatically only once a live run actually
finishes with nothing left to do (see Step 1 point 5 for all four cases
where it isn't, and for what to delete it yourself once no more runs are
coming).

**`--cap`, a failure-streak halt, and cancellation are all normal stopping
points, not a failure or a finish.** A campaign larger than one run's cap,
or interrupted by a rate-limit or an operator pause, needs the identical
live command run again — same files, same credential, nothing to
re-harvest — to keep going where the ledger says it left off.

### The resume contract — the one thing that must not be wrong

`send_dms.py` writes one JSON record per recipient to `progress.jsonl`:
`attempted` **before** the request goes out, `sent` **after** it succeeds.
On any resume — a restart after a crash, a kill-and-relaunch, a second
invocation over the same files, or a hand-edited/merged ledger — a
recipient is skipped, not retried, the moment **either** record exists for
them; a lone `sent` with no matching `attempted` still excludes, it does
not re-admit. Missing one DM is a smaller failure than sending two; this
skill would rather under-deliver than double-message anyone. Only real
records count — a `--dry-run`'s simulated `attempted`/`sent` writes are
tagged and never gate a real send, which is what lets Step 4's dry-run and
live commands safely share one `progress.jsonl`. Recipient ids are
normalized before comparison, so "1001", `1001`, and `1001.0` are always
the same person. `progress.jsonl` is never deleted and never rewritten,
including on `--dry-run`, on a `--cap` stop, on a failure-streak halt, or
on cancellation — whatever it holds when a run stops is exactly what the
next run resumes from.

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
- Never retry a recipient that already has an `attempted` **or** `sent`
  record — either one alone is enough to exclude them.
- Never print, log, or return the contents of anything under
  `.ig-capture/`, any cookie value, or the raw `Cookie`/`Authorization`
  header — including inside an error message.
- Never attempt to sign in, solve a captcha, or handle an OTP on the
  operator's behalf. Hand the browser back to them with `browser` `open` and
  say what's needed.
