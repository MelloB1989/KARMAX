# Claude Code as the only brain

Status: building, 2026-09-07. Supersedes the "API path as fallback" arrangement
in HARNESS-ORCHESTRATOR.md. That document's supervisor, protocol and session
model still hold; this one changes what sits on top of them.

## Why

Three days of the local daemon on Azure gpt-5, measured:

| what | count |
|---|---|
| turns killed by a 429 with the "fallback" being the same deployment | 8 |
| turns that ran out of tool passes and answered nothing | 101 |
| operator messages dropped as stale after a 15-minute outage | every one older than 15m |
| harness declined a turn and had no fallback wired | every trip |

The operator's instruction: Claude Code for everything, tiered by model so the
subscription window is not burned on classification; the CLI able to open
sessions with a chosen model, context and prompt; events and loops that survive
a crash and a quota pause; and a task, once given, owned to completion with the
operator kept informed.

## The tiers

Every model call in the daemon goes through a harness *kind*. A kind names a
model, an effort level and a priority class:

| kind | model | effort | priority | used by |
|---|---|---|---|---|
| `classify` | haiku | low | background | loop gateway, wa-monitor triage, review judge |
| `summarize` | haiku | low | background | compaction, chat summaries, memory merge, cleanup |
| `retrieve` | haiku | low | background | memory retrieval sub-agent |
| `chat` | sonnet | medium | operator | `karmax ask`, voice |
| `agent` | sonnet | medium | operator | the orchestrator's own session |
| `task` | opus | high | background | delegated coding / research, ephemeral |
| `deep` | fable | high | background | explicitly requested hard work |

`--model` on the CLI overrides the kind's model for that session.

## Quota

The breaker reads the account's own windows from every turn. Two shares:

- `operator_share` (default 0.85): operator-priority kinds run until the window
  is nearly gone. A message from the operator *is* the operator using their
  quota.
- `window_share` (default 0.4): background kinds stop here, leaving the rest for
  the operator's interactive sessions and for answering them.

Where the quota goes, and what stops it going:

- The agent's persona goes in `--append-system-prompt` at spawn — once per
  process, not once per turn.
- The per-turn context (clock, profile, open reviews, sessions, channels,
  recent actions) is sent as a delta: a section goes only when its content
  changed since this session last saw it.
- No pre-turn memory retrieval sub-agent on the harness path. The session runs
  `karmax memory search` itself when it needs to, and is told so.
- The task keeper backs off: 10m, 20m, 40m … 4h between nudges.
- `brain-monitor` reads the breaker and the last successful turn instead of
  spending a turn to ask "are you there".

## When the window is gone (`harness.exclusive: true`)

Nothing falls back to a metered API. Instead:

- An agent turn the breaker declines is **parked**: the turn row goes to
  `waiting` with `retry_at` set to the window's reset. The retry worker
  redelivers it when the time comes. The operator is told once per pause —
  "out of Claude quota until HH:MM, N messages queued" — and once on resume.
- A loop that needs the model returns `loopkit.ErrQuotaPaused{ResetsAt}`. The
  run is rescheduled for that time without burning a retry attempt.
- Operator messages stay fresh for 24 hours, not 15 minutes. A message sent
  while the daemon was down is answered when it comes back, not discarded.

## Tasks

A task is a unit of work the operator handed over that may take more than one
turn. The agent opens one (`task.open`) before starting anything non-trivial,
notes progress (`task.update`), and closes it (`task.done`). A `task-keeper` loop
nudges the agent's session on every open task whose check time has come, and
posts a status line to the channel the task came from whenever its state
changes or an hour has passed. Blocked tasks say what they are blocked on;
failed tasks are reported, not retried forever.

Rows in `tasks`; survives restarts by construction.

##