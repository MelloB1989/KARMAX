# Harness as the orchestrator engine

KARMAX becomes the kernel: memory, events, comms, loops, recipes, logs, scheduler,
and a session supervisor. The *thinking* moves to coding harnesses (Claude Code,
Codex) driven as long-lived processes. Nothing about any particular integration —
how to answer a WhatsApp message, who to reply as — enters core; that lives in
workflows, which drive generic session primitives.

## What was measured before planning

These are the numbers the design rests on. All taken on this machine.

| measurement | result | why it matters |
|---|---|---|
| Cold `claude --print` round trip | **4.1s** | the naive per-message design |
| Cold turn that calls `karmax tool call` | **15.4s** | naive design is as slow as today |
| **Warm session**, 1st / 2nd / 3rd message | **5.3s / 1.6s / 1.8s** | the design that works |
| Today's API path per WhatsApp reply | **15–18s** | the thing being replaced |
| Session resume after the process exits | context intact | crash safety is free |
| Result event reports | `total_cost_usd`, per-model `usage`, cache reads | budget enforcement is possible |
| CLI overhead per turn | ~12.7k cache-read + ~6.2k cache-creation tokens | why the session, not the call, is the unit |

**The latency objection is inverted.** A warm session answers in ~1.6s where the
current metered path takes 15–18s. The harness is not slower; the *cold process*
is slower, and a warm session never pays that after the first message.

**The corollary is the whole cost model.** Every turn carries ~12.7k tokens of the
CLI's own overhead. Paid once per session and amortised, that is cheap. Paid per
message, it is ruinous. So the unit of work is a **session**, and the supervisor's
job is to keep the right ones warm and kill the rest.

## The split

```
┌─ KERNEL (core, generic, no use-case knowledge) ────────────┐
│  event bus · comms · memory · scheduler · loops · recipes  │
│  session supervisor  ← new                                 │
│  budget ledger + breaker  ← new                            │
│  audit sink  ← new                                         │
│  `karmax tool call` + generated cheatsheet                 │
└────────────────────────────────────────────────────────────┘
              ▲ generic primitives, no WhatsApp in sight
┌─ WORKFLOWS (apps: signed WASM loops / recipes) ────────────┐
│  wa-monitor: "open a session keyed chat:<jid>, feed it     │
│  messages, close after 10min idle, persona = …"            │
└────────────────────────────────────────────────────────────┘
```

Core never learns what a WhatsApp reply is. It learns how to run a conversation
with a subprocess.

## Session supervisor

The piece that has to be genuinely good. One process per live session, spoken to
over `--input-format stream-json --output-format stream-json`.

### State

`harness_sessions` table — the supervisor's own journal, not a cache:

| column | purpose |
|---|---|
| `key` | caller's stable handle, e.g. `chat:9039…@lid`. Unique. |
| `harness_session_id` | the CLI's uuid, for `--resume` |
| `kind` | `chat` \| `agent` \| `loop` \| `task` — selects policy |
| `model` | which model this session runs |
| `pid`, `state` | `starting` \| `live` \| `idle` \| `dead` \| `closed` |
| `started_at`, `last_activity_at`, `turns` | lifecycle + reaping |
| `cost_usd`, `input_tokens`, `output_tokens`, `cache_read` | budget, from result events |
| `workdir` | isolation root |

### Lifecycle

- **open(key, kind)** — mint a uuid, spawn with `--session-id`, register, return.
  If `key` already has a live process, reuse it. If it has a dead row with a
  known `harness_session_id`, spawn with `--resume` instead: verified above that
  context survives process death, so a crash costs a respawn, not a memory.
- **send(key, text) → reply** — write one stream-json user event to stdin, read
  until the matching `result`. Timeout per policy; on timeout mark `dead` and let
  the next send resume.
- **close(key)** — SIGTERM, then SIGKILL after a grace period. Ephemeral kinds
  delete the transcript; durable kinds keep it for `--resume`.
- **reap** — an idle sweep closes sessions past their kind's idle window, and a
  startup sweep kills orphans whose pid is gone or unknown. Both are needed: the
  daemon restarts often.

### Policies, per kind

Set in config, not in code, so a new use-case never edits core:

```yaml
harness:
  binary: claude
  kinds:
    chat:    { model: sonnet, idle: 10m, max_turns: 40, turn_timeout: 45s, max_cost_usd: 0.50 }
    agent:   { model: sonnet, idle: 30m, max_turns: 100, turn_timeout: 3m,  max_cost_usd: 2.00 }
    task:    { model: opus,   idle: 0s,  max_turns: 1,   turn_timeout: 10m, max_cost_usd: 5.00 }
```

`max_turns` and `max_cost_usd` are per-session circuit breakers — a session that
exceeds either is closed and its caller told, so one runaway conversation cannot
drain a window.

### Concurrency

A hard cap on live sessions (`max_live`, default 6). Beyond it, the least
recently used idle session is closed to make room; if none is idle, the caller
gets a busy error and falls back. Without this, one busy group chat spawns
processes until the machine dies.

## Budget and the breaker

Claude's 5-hour and weekly limits are per **account**, and this machine also runs
the operator's own interactive sessions. KARMAX therefore gets a *share*, not the
pool.

- Every `result` event carries `total_cost_usd` and `usage`. The supervisor writes
  both into the existing `model_usage` table with `kind = "harness:<kind>"`, so
  `karmax cost` shows harness spend beside API spend with no new reporting.
- A rolling 5h window and a 7d window are computed from that ledger.
- `window_share: 0.4` — when KARMAX's own consumption crosses its share of the
  configured window budget, the **breaker trips**.
- Tripped means: `harnessBrain` refuses, and every call falls back to the existing
  metered API path. Nothing stops working; it gets more expensive and slightly
  worse until the window rolls.
- The breaker also trips on a real quota rejection from the CLI, which is the
  authoritative signal — the ledger is an estimate, the refusal is a fact.
- Trip and reset are both logged and pushed to the operator. A silent degradation
  is how you discover a month later that nothing has used the harness since June.

## Permissions and audit

Sessions run with `--dangerously-skip-permissions` (the brain needs real tools) and
`cwd` set to a per-session directory, not the repo.

Every tool the harness invokes is captured from the stream-json events and written
to the event bus as `harness.tool.called`. An allowlist of expected commands
(`karmax`, `wacli`, `gh`, `git`, read-only shell) is checked; anything outside it
raises an alert to the operator immediately. This does not prevent a bad action —
it makes one impossible to miss, which is the honest description.

## Tool exposure

Phase 1 uses the CLI surface that already exists. `karmax tools cheatsheet`
generates a compact index — name, one-line description, argument shapes — from the
live registry, injected into the session's system prompt at open time. Generated,
never hand-written, so a new tool appears without anyone remembering to document it.

MCP remains the upgrade path: it would make the tools native and typed and would
let sessions run `--restricted`. It is deliberately not phase 1.

## Phasing

Each phase lands verifiable and revertible on its own. The API path is never
removed — it is the fallback, permanently.

**Phase 0 — supervisor, no routing changes.** Table, spawn/send/close/reap,
budget ledger, audit sink. Nothing calls it yet.
*Verify:* open a session, send three messages, confirm 2nd and 3rd are ~1.6s;
kill -9 the process and confirm the next send resumes with context; confirm cost
rows land in `model_usage`.

**Phase 1 — `karmax session` CLI.** Drive the supervisor by hand: `open`, `send`,
`list`, `close`. This is how the thing gets debugged for the rest of its life.
*Verify:* by hand, including the reaper and the concurrency cap.

**Phase 2 — the brain seam.** Extract the 8-method interface `MainModelSession`
already satisfies; add `harnessBrain` alongside `apiBrain`. Route **`karmax ask`
only** — operator-facing, easy to judge, zero blast radius.
*Verify:* A/B the same prompts against both; confirm the option-menu and
self-announcement rates that characterised the gpt-5 regression.

**Phase 3 — session primitives in loopkit.** `kit.Session(key, kind).Send(text)`.
wa-monitor drives warm per-chat sessions; core still knows nothing about WhatsApp.
*Verify:* a real chat answered in ~1.6s; session closes after idle; a second chat
gets its own session; the cap holds.

**Phase 4 — the rest.** Gateway classification, summaries, review move over once
Phase 3 has run for a few days without incident.

**Phase 5 — hardening.** Breaker tuning against real windows, fallback drills
(revoke auth and confirm nothing breaks), orphan reaping across restarts.

## Failure modes, and what happens

| failure | behaviour |
|---|---|
| Harness process dies mid-turn | session marked dead; next send resumes by session id |
| Quota exhausted | breaker trips → API fallback → operator notified |
| Auth expired | detected on spawn → breaker trips → operator notified |
| Session leak | idle reaper + `max_live` cap + startup orphan sweep |
| Runaway session cost | per-session `max_cost_usd` closes it |
| Harness runs something unexpected | audited to the bus, alert on non-allowlisted |
| Malformed stream-json | turn fails, session marked dead, next send resumes |
| Daemon restart with live sessions | pids are stale; startup sweep marks dead, resume on demand |
| Codex absent | it is not installed here; the supervisor is binary-agnostic and Codex is a config change, not a code change |

## What this does not solve

- **Quality is not guaranteed to improve.** The harness is a different brain, not
  a better one by construction. Phase 2's A/B is what decides.
- **The 12.7k-token floor is real.** A session that answers one message and closes
  is more expensive than an API call. The supervisor's idle windows are the whole
  economics; get them wrong and the bill goes up, not down.
- **Full shell remains full shell.** The audit makes misuse visible, not impossible.
