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
- **close(key)** — EOF, then SIGINT to the session's process group (each
  session leads its own), then SIGKILL after a grace period. Whatever the
  session started that is still running then gets SIGTERM, and SIGKILL after
  the same grace: the CLI runs each shell command in a session of its own, out
  of reach of a group signal, so its process tree is read before it is told to
  stop. Ephemeral kinds delete the transcript; durable kinds keep it for
  `--resume`.
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
    extract: { model: sonnet, idle: 5m,  max_turns: 4,   turn_timeout: 3m, ephemeral: true }   # wa-memory, gchat-memory: one chat batch per session
```

`max_turns` and `max_cost_usd` are per-session circuit breakers — a session that
exceeds either is closed and its caller told, so one runaway conversation cannot
drain a window. A kind that leaves `max_turns` out (or is not listed at all)
gets 200: without a bound, a session resumes the same transcript forever and
every turn re-reads all of it. `max_turns: -1` asks for no limit.

A session starts only the MCP servers KARMAX passes it (`--strict-mcp-config`),
not the ones in the operator's own Claude Code config: those started as a
process tree per session, whether or not a session ever called one. A kind
whose work needs them says `inherit_mcp: true`.

Nor does a session load the operator's own Claude Code config: the plugins,
skills, hooks and settings in `~/.claude`, built for their interactive
sessions. It runs with `--setting-sources project,local` — its workdir's own
config, which is also where every CLAUDE.md is read from, the data root's brief
included — plus only what KARMAX allowlists:

```yaml
harness:
  plugins: [name@marketplace, /path/to/a/plugin]   # installed id, or a directory
  skills: [fleet]                # one of the operator's skills by name, or a directory
  settings: { effortLevel: high }   # Claude Code settings, in its own shape
  kinds:
    orchestrator: { skills: [fleet] }   # a kind's own list replaces the default; [] for none
    task: { inherit_user_config: true } # the operator's whole config, as before
```

An installed plugin is enabled through `--settings` (`enabledPlugins`), a
plugin directory loaded with `--plugin-dir`. Skills are linked into one
directory per set (`<workdir_root>/.skills/<hash>/.claude/skills/`) and given
with `--add-dir`, so each keeps its own name rather than a plugin's namespace.
Auth is not a setting and is unaffected. What the operator's settings gave a
session (`effortLevel`, `modelSettings`, commit `attribution`) now comes from
`harness.settings` or not at all.

Measured on CLI 2.1.314, one haiku first turn: the operator's set put 103
skills, 8 plugins and 4 start-up hooks (12KB of injected start-up context, one
of them a prompt to use its skills before any reply) into every session, plus a
hook on every tool call — 31.6k tokens of first-turn context. Without it, 18.3k
(18.5k with the orchestrator's `fleet` skill), no hooks, and the session ready
0.2s sooner warm, 1.5s sooner cold.

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

## Work longer than a turn

A turn has a budget (`turn_timeout`); a request does not. Anything someone
asks for — an operator's message, a workflow's `ask()` proxying a message
from a chat it watches, the app's chat — is a **request**: journalled in
`agent_turns` before the turn starts, and owned until it is done and its
answer delivered.

```
request ─▶ journal row ─▶ turn ──finished──▶ answer
                            │
                            ├─ ran out of time, or failed after acting
                            ├─ screened as heavy (reflex effort)
                            └─ the model hands it over: task.create request=<id>
                                   │
                                   ▼  one transaction: turn row handed_off + task row
                               task (kind "task", its own session)
                                   │  requester told "on it" now
                                   ▼
                               rounds until done / failed / blocked
                                   │  result + FILE: lines recorded as owed with the round
                                   ▼
                               delivered to where the request came from, once
```

- **No downgrade.** A request that outran its harness turn is never retried
  on the fallback model or a workflow's lighter gateway: that model would
  have less time and none of the conversation. Once a task owns a workflow
  run's request (or the full agent gave up on it and the requester was told),
  that run's `gateway` and `harness` calls are refused and nothing more is
  sent into the chat it came from. Every loop model call about a chat is told
  which requests from it tasks already own.
- **Late results.** The rest of a turn a task took over goes into the task's
  progress (`Options.OnLate`); so does a task round's own late result, and a
  background delegation started for the request.
- **Where the answer goes.** The chat the request came from when the operator
  asked there; the operator (their channel, else the app) for anyone else's
  request through a workflow, and for the app. The task session is told not
  to send the result itself: KARMAX delivers its final `REPORT` and files.
- **The operator's own message** is never answered as if they were someone
  else: a reply to it that speaks of them as away or to be told is refused.

### Crash points

Every step that matters is a row written before or with it, so a crash at
any point resumes on the next start and nothing is done or said twice:

| killed … | on the next start |
|---|---|
| after the request was journalled, before work | an ask or app chat (its caller died too) continues as a task; an operator's message is redelivered to its agent |
| mid-turn | the message is redelivered; a turn that had only sent the slow-turn ack is not mistaken for answered; a turn whose request a task owns is not re-run |
| mid-task round | every task the dead process claimed is due at once; its session resumes its transcript |
| after a round's result was recorded, before delivery | the result is owed (`deliveries`), and sent |
| after delivery, before it was recorded | the chat shows it went out, so it is recorded and not sent again |
| during a background delegation | resumed from its row: same session when its transcript exists, else afresh under the same id; its end is announced once (derived event id) |

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
