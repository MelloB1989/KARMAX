# Agent fleet — two PCs, eight standby coding agents

Status: plan, 2026-10-08, revision 2. Nothing here is built. Phase 0 is a half-day spike
(`packaging/fleet/spike.sh`) that proves the undocumented parts before any code is written.

## The goal

- Kali: KARMAX and its orchestrator, plus some agents. PC2 on the LAN: agents only.
- 8 coding agents always on standby. Each is a Claude Code session in its own container, on its own
  Claude subscription. You switch subscriptions for your own work; the fleet must not care.
- `~/code` is shared with every agent. Worktrees are made inside the agent's container.
- The orchestrator is itself a Claude Code session. It and the agents talk with Claude Code's own
  cross-session messaging (`ListAgents` / `SendMessage`) — no protocol of ours in between.
- You can prompt any agent's live session directly.
- Session management is first-class: every session is accounted for, stale ones are archived, and
  nothing archived is lost.
- KARMAX does not get bloated. The daemon changes are small, generic and off unless configured.
  Fleet management lives in its own binary.

## Facts the design rests on

Checked against the Claude Code docs and the 2.1.294 CLI, not recalled.

| fact | consequence |
|---|---|
| Same-machine messaging is a per-session Unix socket. Sessions find each other in `~/.claude/sessions/<pid>.json`, which records the socket path and a `pidDomain` (`linux:<boot-id>:pid:[<pid-ns inode>]`). The docs say a containerised session and a host session can't reach each other; two in one container can. | They fail because their **PID namespaces** differ. Containers that share one PID namespace, the `sessions/` dir and the socket dir should be "one machine" to Claude Code. Undocumented → spike S1. |
| Cross-machine messaging exists only through Remote Control, and only between sessions of **one account**. | Native messaging can never cross the Kali↔PC2 line here: every agent is a different account. That hop needs a relay (below). Everything on one PC is native. |
| A `-p` session binds an inbox like an interactive one. When it's idle and a message arrives, Claude Code starts a new turn by itself. | The orchestrator — a KARMAX harness session driven over stream-json — will emit turns nobody asked for. Today's supervisor reads stdout only inside `Send`, so such a turn stalls the process once 64 events back up, and its result is then handed to the *next* caller as their reply. Must be fixed. |
| Background (`--bg`) sessions that are idle and unattached are stopped after about an hour unless pinned with `Ctrl+T`. The CLI has no pin flag. A stopped session has no inbox. | Standby agents can't be `--bg` sessions: they'd go deaf within the hour. Each agent runs an **interactive** `claude` in tmux inside its container, which stays live and is also what makes direct prompting trivial. |
| `claude agents --json` is the documented scripting interface: id, state, status (`busy`/`waiting`/`idle`), sessionId, cwd, kind, startedAt, pid, name. `claude stop` keeps the conversation; `claude rm` deletes it. | Session management reads `agents --json`, never deletes a transcript it hasn't archived, and stops rather than removes. |

## Shape

```
┌──────────────────────────── Kali ──────────────────────────────┐         ┌──────────── PC2 (Debian 13 minimal) ────────────┐
│ KARMAX daemon (host, systemd)          fleetd (host, systemd)  │         │                                                  │
│   └ harness ─docker exec -i─┐            │ reconcile · archive │──ssh───►│ docker (context pc2)                             │
│                             ▼            │ attach · prompt     │         │                                                  │
│  ┌ PID ns: fleet-anchor ─────────────────┼────────────────┐    │         │  ┌ PID ns: fleet-anchor ──────────────────────┐  │
│  │ karmax-brain   agent-01  agent-02  agent-03            │    │         │  │ agent-04  agent-05  agent-06  agent-07  -08 │  │
│  │ (orchestrator)   tmux: claude --name agent-NN          │    │ relay   │  │   tmux: claude --name agent-NN              │  │
│  │      ▲ SendMessage ▲ over shared socket dir ▲          │◄───┼─fleetd──┼─►│   ▲ SendMessage, same mechanism ▲          │  │
│  └──────┴─────────────┴──────────────────────────┴────────┘    │         │  └──────────────────────────────────────────────┘  │
│        ~/code (bind, same absolute path)                        │◄──NFS───┤   ~/code (NFSv4 from Kali, same path)            │
└─────────────────────────────────────────────────────────────────┘         └──────────────────────────────────────────────────┘
```

| who → who | how |
|---|---|
| orchestrator ↔ Kali agents, Kali agent ↔ Kali agent, PC2 agent ↔ PC2 agent | native `SendMessage`, nothing in between |
| anything ↔ across PCs | `fleetctl tell <name> "<text>"`. fleetd starts a throwaway `claude -p --model haiku` **inside the receiver's namespace** that delivers it with `SendMessage`, so it still arrives as a native peer message. The relay's reply address dies with it, so replies go back through `fleetctl tell`. One cheap turn per hop, on the receiving side's subscription. |
| you → any agent | `fleetctl ssh` / `prompt` / `peek` (below) |
| you → orchestrator | unchanged: WhatsApp, the app, `karmax ask` |

The relay deliberately does **not** type into the receiver's terminal. Typed input counts as you —
it can answer permission prompts and carries operator authority. A message from another session
can't, and Claude Code tells the receiver it came from a peer. Peers must stay peers.

Upgrade path for the cross-PC hop, once the basic fleet runs: a small **channel** MCP server in
every session (Claude Code's documented way to push into a running session, research preview,
custom channels need `--dangerously-load-development-channels`). No per-message turn, proper reply
addresses. Not phase 1.

## The agent container

One process tree per agent, under tini:

```
agent-run                       # fleet's own loop, one per slot (see Slots)
└ tmux server
  ├ session "main":   claude --name agent-NN   --dangerously-skip-permissions [--resume <sid>]
  └ session "main-2": claude --name agent-NN:2 …           # slot 2, and so on
```

- **cwd `/work`** — the agent's desk, on its private volume. Worktrees go under `/work/wt/`, made by
  the agent itself, locked (`git worktree lock`) so a `git worktree prune` on Kali can't drop them.
- **Name `agent-NN` is the address.** Whatever session is current answers to it. `agent-run` reads
  the session to start from `/work/.fleet/next` (empty = fresh, a session id = `--resume`), so fleetd
  rotates sessions by writing that file and killing the pane — never by typing into it.
- **Auth:** `CLAUDE_CODE_OAUTH_TOKEN` from the agent's own 0600 env file (next section).
- **Settings** (`~/.claude/settings.json`, seeded): `crossSessionInbound: accept` (explicit, so the
  bypass-vs-prompting default never holds a message), `dialogExpiry: "never"`, `DISABLE_AUTOUPDATER=1`
  (version is pinned in the image), and the three telemetry taps in [Fleet ledger](#fleet-ledger--logs-metadata-status-quota):
  a status line script, a set of hooks, OpenTelemetry export. The one-time bypass-mode acceptance
  happens in `fleetctl init`.
- **`CLAUDE.md`** (seeded): you're `agent-NN` on `<host>`; your local peers are …; reach remote ones
  and the orchestrator from PC2 with `fleetctl tell`; one worktree per task on `fleet/agent-NN/<topic>`;
  commit early; never push to `main`; when a task ends, report to `karmax` and say "done".

Volumes per agent:

| mount | source | shared |
|---|---|---|
| `$HOME/code` | bind on Kali, NFS on PC2, **same absolute path everywhere** (worktree gitdirs and Claude's per-project keys are absolute) | all |
| `~/.claude/sessions` | named volume `fleet-peers` | per host |
| socket dir (exact path from S1) | named volume `fleet-socks` | per host |
| `~/.claude` (rest: transcripts, settings) | `agent-NN-home` | private |
| `/work` | `agent-NN-work` | private, survives rebuilds |

**After a reboot: the code mount.** A container's bind mount is resolved when it *starts*. On PC2
the NFS share is an `x-systemd.automount`, whose stub satisfies Docker's mount dependency, so after a
power loss Docker can start the agents on the empty autofs point — and nothing inside can fix that,
because the bind never changes. So `agent-run` checks first that `$FLEET_CODE` (the same absolute
path, from the container env) has entries; if not it logs to stderr and `agent-run.code_missing` to
the event log, sleeps `FLEET_CODE_RETRY` (20 s, so the restart policy does not spin) and **exits 1**
— it never loops. The compose service has `restart: unless-stopped`, which restarts on any exit with
a doubling backoff capped at a minute, and the restart re-binds the (by then mounted) share.
fleetd shows such an agent as `code_missing` (not `waiting` or `down`) and excludes it from task
assignment; the first session to come up clears it. At boot this is a race agent-run usually wins on
its next restart, so you are told (once) only if it lasts 3 minutes. Does not cover a share that stays
unmounted (friction #40: a changed DHCP address) — the agent then restarts every ~20 s, loudly, until
the share is fixed.

Two more things happen after a power cut (seen 2026-10-14). Docker starts the agents before PC2's
network or Kali is up, each one triggers the automount, and the failed attempts hit systemd's start
limit in a second: the mount unit fails, the automount with it, and nothing mounts the share again.
`host-bootstrap.sh pc2` installs a drop-in with `StartLimitIntervalSec=0` beside the mount unit, so
every later access retries. And a container whose bind Docker could not make at start (`no such
device`, exit 128) is left stopped: the restart policy never retries a failed start. fleetd starts it
again, once a minute, while Docker's `State.Error` is set — a container someone stopped has none, and
one `fleetctl down` removed is not there — and alerts if that goes on for 10 minutes.

### Slots — several sessions per agent

One agent — one container, one subscription — runs several sessions at once, each with tasks of its
own. Each is a **slot**, and each slot is a **worker**: fleetd observes it, decides on it, picks it
for tasks and archives its sessions exactly as it does an agent. `slots:` at the top of
`fleet.yaml` sets how many (default 2); `agents.<name>.slots` overrides it for one agent; 1 is the
old one-session agent, 8 the most.

| | slot 1 | slot k ≥ 2 |
|---|---|---|
| worker name (`claude --name`, SendMessage, `fleetctl`, the ledger, archives) | `agent-03` | `agent-03:k` |
| tmux session | `main` | `main-k` |
| state dir (`next`, `current`, `outbox/`) | `/work/.fleet` | `/work/.fleet/slot-k` |
| status line, hook log | `~/.claude/fleet/status.json`, `events.jsonl` | the same names in `~/.claude/fleet/slot-k/` |

Slot 1 keeps the agent's own name and files, so every state row, ledger row and archive from before
slots stays its own. `:` is safe in every place the name goes: Claude Code treats a `to` as an
address only when it starts `uds:`, `bridge:` or `did:`; Go's router, the archive paths and the
ledger take it as text. No configured agent may have `:` in its name, so none can be named like a
worker. `agent-run` (FLEET_SLOTS=N in the container env, from `fleet.yaml`) checks the code mount and
HOME once for the container, then runs one supervising loop per slot. Each slot's claude carries
`FLEET_AGENT` and `FLEET_NAME` (its worker), `FLEET_SLOT` and `FLEET_STATE` (its state dir), so its
hooks, status line, `fleetctl tell` and its outbox are its own. Every tmux target is an exact match
(`=main:`): tmux would otherwise take `main` for `main-2` while slot 1's session is gone.

What slots share, and how fleetd treats it:

- **The container.** Starting a container Docker left stopped, and saying it is down, is slot 1's;
  its siblings show slot 1's phase (`down`, `code_missing`) and recover when it does.
- **The subscription.** Quota and cooling are the account's: a sibling's newer reading of a window
  is every slot's, and a rate limit cools every slot until it resets. `fleetctl enable` re-enables all
  of an agent's slots.
- **Sessions are told apart by `FLEET_AGENT`** in each claude's environment, so a sibling's session
  is never a stray of another slot: never archived, stopped or resumed by it.
- **`/work` and the shared code.** A worktree another slot's current session has named in its
  transcript is that slot's (`used_by_slot`): a rotation never removes it.
- **One key, one `/work`: slots are not an isolation boundary.** A member's task (a tailnet login
  that is not an operator) and an operator-side task (the operator, their sessions, the orchestrator,
  a tailnet operator) never share a container: a worker is not eligible while a sibling holds a task
  from the other side, and the 409 says so.

The pick spreads first: a worker on an agent whose slots are all free ranks before a worker on an
agent that already has a busy slot; repo affinity (any slot's last task), quota and idle time decide
after that.

**Sizing.** Each slot is a claude process and what it runs: allow about **1.25 GB of container
memory per slot** (`agents.<name>.memory`), so 2.5 GB for the default two. tmux 3.5a crashed when a
second session's window was born under a global `window-size manual`; the image's `tmux.conf` now
sets it per window and resizes each new window to 220×50, so a slot started while someone watches
another keeps the fixed size. A changed `slots:` takes a new image (`agent-run`, the hooks,
`tmux.conf`) and recreated agents.

Every container on a host joins one `fleet-anchor` PID namespace. Not `--pid host`: that would also
make your own Kali sessions messageable, but every agent runs in bypass mode as your UID and could
then read `/proc/<pid>/environ` of KARMAX and your shell — their secrets. The anchor keeps agents
walled off from the host.

## Subscriptions — one per agent

- `fleet.yaml` binds each agent to a subscription: `agent-03: { host: kali, token_env:
  CLAUDE_TOKEN_AGENT_03, models: [sonnet, opus] }`. The token (`claude setup-token`, run once while
  logged into that subscription) lives only in `~/.karmax/.env`; `fleetctl` renders it into
  `agents/agent-03.env` (0600, refuses to render into a group/world-readable dir). Never in the image,
  compose file, argv or `fleet.yaml`.
- Your `/login` on Kali changes only your `~/.claude/.credentials.json`. Containers never see it.
- The orchestrator has its own subscription the same way — `karmax-brain` is a container with its own env file.
- `models` exists because plans differ; the orchestrator's `CLAUDE.md` says which agents may take Opus/Fable work.
  `fleetctl run --model M` considers only agents whose `models` list M.
- **Which model an agent runs.** `model:` in `fleet.yaml` is its default: agent-run passes it as `--model`
  when a session starts. `fleetctl model <agent> <model>` sets an override without recreating the
  container (`--reset` clears it); it is kept in the ledger, so it survives fleetd restarts. `run --model`
  sets one task's model, and the agent goes back to its override or default when the task closes and the
  session rotates. fleetd applies the wanted model by typing `/model <m>` into the session when it is
  idle (never mid-turn), once per change, and checks the status line afterwards; a fresh session, process
  or resume is asked again, and a session that still shows another model after two minutes is alerted once.
- **Messaging across subscriptions** is socket-level, not account-level. S1 runs the two ends on two
  accounts to prove it.

## The orchestrator: a Claude Code session in the Kali namespace

The orchestrator is KARMAX's existing `agent`-kind harness session. It just has to run *inside* the
anchor namespace, and survive being messaged. Daemon changes, all in `internal/harness` and its config,
all generic, all inert unless configured:

| change | why | size |
|---|---|---|
| **`launch:` per kind** — argv prefix put before the binary, e.g. `[docker, exec, -i, -w, "{workdir}", karmax-brain]`. The workdir root is bind-mounted into `karmax-brain` at the same path, so `CLAUDE.md` writes need no special case. | Runs the session in the fleet namespace without moving KARMAX into a container. Also works for `ssh host`, podman, kubectl. | ~30 |
| **Background turns.** The reader goroutine routes events to a waiting `Send` if there is one, else assembles them into a turn and hands it to `OnBackgroundTurn(key, Turn)` → bus event `harness.turn.background` (text, tools, cost). Usage is recorded and the breaker observes it. A `Send` arriving mid-background-turn waits for that turn's `result` before writing. | Today an unsolicited turn stalls the process and its result is misdelivered to the next caller. Needed by *any* harness session that can be messaged — including when you message KARMAX from your own sessions. | ~100 + tests |
| **`resident: true` per kind** — never idle-reaped, never LRU-evicted, not counted in `max_live`. | An orchestrator reaped after 30 idle minutes has no inbox, and every agent's report bounces. `idle: 0` today means "10 minutes". | ~15 |
| **`name:` per kind** → `--name`. | Agents address the orchestrator as `karmax`, not a generated name. | ~5 |
| **Close sends EOF first.** `Close` flushes and then signals the process. Through `docker exec`, the signal reaches the `docker` client, not `claude`. Closing stdin first is the polite exit that crosses the boundary. | Without it a stopped orchestrator can linger inside the container. | ~5 |

**What leaves the plan:** revision 1's `pool` sandbox driver, `sandbox.tell`, the credential and
reconcile seams it needed. The orchestrator assigns work by messaging a standby agent, not by
spawning a sandbox. No change to `sandbox`, `runtime`, `tools`, the store or loopkit.

The orchestrator's persona gains one paragraph: the roster and who's local; assign work with
`SendMessage` to an idle agent; track it with `task.open` / `task.update` as today; tell fleetd
when a task is finished (`fleetctl done agent-03 --task <id>`); use `fleetctl tell` for PC2 agents.

## Session management — fleetd

A separate small Go binary, **not linked into the KARMAX daemon** (its own module or repo; it needs
nothing from KARMAX). `fleetd` runs as a systemd service on Kali; `fleetctl` is its CLI. It reaches
every container through an exec prefix per host (`docker`, `docker --context pc2`), so it is as
generic as the rest. It is also the fleet's system of record — see [Fleet ledger](#fleet-ledger--logs-metadata-status-quota).

### Lifecycle

```
            spawn                 message/prompt              idle, task open
  (fresh) ─────────► standby ─────────────────────► working ◄──────────────► waiting
                        ▲                               │ rate_limit             │
                        │ rotate (fresh session)        ▼                        │ stale rules
                        └──────────── archived ◄──── cooling ──(reset)──► resume │
                                          ▲                                       │
                                          └───────────────────────────────────────┘
```

Every 30 s fleetd reconciles each agent against "exactly one live session, named `agent-NN`, inbox
bound", using `claude agents --json` (status, sessionId, pid), tmux pane liveness, the `StopFailure`
files, the status-line snapshot (quota, context) and transcript mtime/size:

| observation | action |
|---|---|
| no live session | restart the pane with `--resume <last sid>`; three failures in 10 min → archive, start fresh, alert |
| container stopped because Docker failed to start it (`State.Error`) | `docker start` it once a minute; alert after 10 min |
| session back, idle, in a new process (boot id + pane pid), with a turn its hooks opened and no `Stop` closed — or its task still owed a word (the last message on the task went to it: it was waiting on a background command of its own) | type a carry-on prompt: check the worktree, continue the task from there, do not start over; push `resumed` to the orchestrator |
| a second session in the container, or one not named `agent-NN` | archive it |
| task closed (`fleetctl done`, or the agent says done and the orchestrator confirms) | archive, rotate to a fresh standby session — **one task, one session**, so context never leaks between tasks;
whatever the old session left running (a job it detached, a server) is ended — every command Claude
Code runs carries the session's id in `CLAUDE_CODE_SESSION_ID`, and claude itself does not. fleetd looks every 5 s until it has rotated, and the agent takes no new task until its fresh session is up. A session `claude agents` calls busy only because a background command it left still runs (its hooks saw `Stop`) is rotated anyway, and any session 3 min after `done` |
| the agent's host does not answer (`docker inspect` fails: at boot before the network, the host off, Docker restarting) | nothing: the agent is unknown, not down — no push, no restart, its phase kept. It takes no new task. After 5 min it shows `unreachable`, `wait` returns, and you get one alert per host |
| standby, idle > 12 h, transcript non-trivial | archive, rotate (standby sessions must not accumulate junk) |
| waiting, owing its task a word (the last message on the task went to it), idle 10 min | type into its session: report now, resend a report that failed, or say what blocks you — once per instruction |
| waiting (task open) idle > 2 h | message the orchestrator: "agent-03 idle 2 h on <task>" |
| waiting idle > 24 h | archive (worktree kept), rotate, tell the orchestrator and you |
| transcript past a size threshold while idle | type `/compact` into the pane — a fleet action you configured, not a peer's |
| 5-hour window ≥ 80 % (status line) | `low` — still works, but the orchestrator's roster marks it so new tasks prefer other agents |
| `StopFailure: rate_limit` | `cooling` until the window's `resets_at` (from the last status-line snapshot; else 1 h). Not stale, never archived for this. Resume in place when it clears. |
| 7-day window ≥ 90 % | `reserved` — small tasks only until the weekly reset; you get one alert |
| `StopFailure: authentication_failed / billing_error / account_on_hold` | `disabled`, alert you once |

All thresholds live in `fleet.yaml`.

**Messages are not lost to a restart or an outage.** Every relay is written to the ledger *before* it is sent
and marked delivered after, so a brief fleetd was typing when it stopped (a power cut, a deploy)
is found by the next fleetd and sent again, once its receiver's session is up, with a note that it
may be a second copy. A relay counts as delivered only when its `SendMessage` says so for the
receiver (the relay's `claude -p` exits 0 either way); one that did not get through — the session
was restarting or rotating — is tried twice more, 5 s and 15 s later. A follow-up, cancel notice or
`tell` whose receiver still cannot take it — its host does not answer, its container is down, or the
relay has not gone through in 75 s — is **held**: fleetd answers `queued` (HTTP 202; fleetctl prints
the line and exits 0), keeps the message on the ledger, and sends it, in order with anything held
before it, when the receiver is back, headed by a note saying when it was sent. A message to a
receiver with one still held waits behind it. A held message has no age limit and no limit on tries:
it stays until it is delivered or its task closes (the cancel notice, written as the task closes,
still goes). Tries back off — 30 s, doubled after each failure in a row, up to 5 min — and a receiver
whose host is out of reach or whose container is down is not tried at all. When one has been held
longer than `held_alert_after` (default 1 h) you get one alert, and `fleetctl result` and
`fleetctl inbox` mark it `(held: not delivered yet)`. A brief is never held: a `run` whose brief does not get through closes
the task `undelivered` and fails, so its caller places it again. In the other direction,
`fleetctl tell` in an agent's container retries for a few seconds and then queues the message in
`/work/.fleet/outbox/`; fleetd reads the outbox with each look at the container, delivers it as the
agent's, and removes it. An item that cannot be sent stays in the outbox, however long (one to a name
fleetd does not know is dropped, and you are told). A `run` repeated with the same `--task` (its answer was lost) returns the
agent that already has it instead of giving the task to a second one.

**A power cut does not close a task.** PC2 has been power-cycled dozens of times in a night, and every
open task must come back where it was. Three clocks would otherwise mistake a host that was off for
an agent that failed, so none of them runs while the host is out of reach or the container is down:
the 12 h / 24 h idle clocks (fleetd adds the time away to a per-state `idle_away` and idle ignores
it), and crash rotation. For `host_settle` (default 10 min) after a host returns, or after its boot id
changes, a pane that will not start is restarted without being counted toward `restart_failures`, and
the count is cleared; inside the container `agent-run` likewise does not count a pane's quick deaths
in the first `FLEET_BOOT_GRACE` seconds (default 300) of the host's uptime. A session that really
crashes still rotates once the host has settled.

A session that was working on a task when it was cut short is told, once, to carry on. Besides the
base text, fleetd adds what it already has: the task and the first lines of its brief, the messages
that arrived while it was down (they are sent next), each worktree's branch and `git status --short`
summary, and that background commands it started were killed and must be run again if still needed.
Worktrees are found under `/work/wt` and under `<code>/.worktrees/<agent>/`, so both appear in
archive manifests.

### Archive — copy, verify, then stop

1. Only act on an idle session (`status: idle`). A busy one waits for the next tick.
2. Write `~/.karmax/fleet/archive/<agent>/<date>-<sid8>/`:
   - `manifest.json`: agent, host, account, session id/name, task id, started/ended, turn count and usage
     summed from the transcript, cwd, last assistant message
   - `transcript.jsonl.zst` plus any subagent transcripts
   - `worktrees.json`: path, branch, upstream, clean?, unpushed commits
3. Verify by hash. Only then stop the session (`claude stop` / kill the pane).
4. Worktrees: clean and pushed → `git worktree remove`. "Pushed" is every commit on some remote
   branch, not on the upstream: agents push without `-u`, and a worktree made from `origin/main` has
   main as its upstream. (Judged by the upstream, every task's worktree was kept, and resets slowed
   with each one.) Anything uncommitted or unpushed, or a worktree git cannot read → stays, locked,
   listed in the manifest and in `fleetctl ls`. Never discarded by fleetd.
5. Write `/work/.fleet/next` empty and restart the pane → fresh standby `agent-NN`.
6. Optional: hand the manifest's summary to `karmax memory add`, so the orchestrator remembers what was
   done without re-reading a transcript.

**Restore:** `fleetctl restore <archive-id>` puts the transcript back in that agent's `~/.claude/projects/…`,
archives whatever is current, writes the id to `/work/.fleet/next` and restarts the pane. Same agent by
default — its worktrees live on its private volume. Retention: forever (compressed transcripts are
small); `fleetctl archive prune --older 180d` exists for when they aren't.

## Fleet ledger — logs, metadata, status, quota

Everything about every agent is recorded by **fleetd**, not by the orchestrator's model. Bookkeeping
done in LLM turns would cost quota and miss things; a reconciler loop doesn't. The orchestrator
*reads* the ledger, and is *told* when something changes.

### What each agent emits

Three documented Claude Code taps, configured in the image, plus what fleetd observes from outside:

| tap | gives | how |
|---|---|---|
| **Status line** | Per account: `rate_limits.five_hour` / `seven_day` → `used_percentage`, `resets_at`. Per session: `context_window.used_percentage`, `cost.total_cost_usd` (list-price estimate), model, `session_id`, cwd. | `statusLine` command = a 5-line script that writes the stdin JSON atomically to `~/.claude/fleet/status.json` and prints a short line. `refreshInterval: 30` keeps idle sessions reporting. Claude Code also refreshes it when a window passes `resets_at`. |
| **Hooks** | Activity timeline: `SessionStart`, `UserPromptSubmit`, `PostToolUse` (name + success only), `Stop`, `StopFailure` (+ error type), `Notification`, `SessionEnd`. | One hook script appends one JSON line per event to `~/.claude/fleet/events.jsonl` — session id, event, time, a few fields. |
| **OpenTelemetry** | Authoritative accounting: `api_request` (model, input/output/cache tokens, cost, duration), `api_error`, `tool_result`, plus counters for sessions, lines changed, commits, PRs, active time. | `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_{METRICS,LOGS}_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`, endpoint = fleetd's OTLP receiver, `OTEL_RESOURCE_ATTRIBUTES=fleet.agent=agent-03,fleet.host=kali`. Prompts, responses and tool parameters stay **redacted** (the default) — the transcript is the full record, and it is archived. |
| fleetd, from outside | Liveness, busy/idle (`claude agents --json`), pane alive, container CPU/RAM (`docker stats`), restarts, OOM kills, image and Claude Code version, disk use of `/work` and `~/.claude`. | Polled every 30 s through the exec prefix. |

Files inside the container are only a buffer: fleetd ingests them each tick and truncates
what it has stored.

### What fleetd keeps

SQLite at `~/.karmax/fleet/fleet.db`:

| table | holds | retention |
|---|---|---|
| `agents` | name, host, account label, plan, allowed models, container id, image digest, Claude Code version, resources, enabled/disabled + reason | forever |
| `sessions` | session id, agent, task, started/ended, end reason (task done, stale, crashed, restored), turns, tokens by kind, list-price cost, peak context %, cwd, worktrees, archive id | forever |
| `quota` | per account: 5 h and 7 d used %, `resets_at`, source (status line or `StopFailure`), time | every sample 7 days, hourly after that, forever |
| `usage` | tokens and cost per agent × model × hour, from OTel | forever (it's small) |
| `events` | hook events, OTel errors, fleetd's own actions (spawn, rotate, archive, restore, relay, cooling, disable, restart, OOM) | 90 days raw, then daily counts |
| `tasks` | KARMAX task id, agent, assigned/closed, outcome, branch, PR | forever |
| `relays` | cross-PC messages: from, to, time, size, delivered? (text kept 30 days) | 30 days |
| `host` | CPU/RAM per container, 1-min samples | 7 days |

Plus the archives (transcripts and manifests) and fleetd's own log in journald.

### Quota, specifically

- **It is the account's truth, not the fleet's guess.** The status line reports the subscription's
  real window. That includes anyone else using it: if you work on agent-03's subscription yourself,
  agent-03's quota drops and the fleet sees it on the next refresh.
- **Only after a first API call.** A freshly rotated session has no `rate_limits` until it does
  something, so fleetd keeps the last known value per account, ages it out at `resets_at`, and shows
  "as of HH:MM". No probing — a probe would spend the quota it measures.
- **The orchestrator's own windows** already reach KARMAX's breaker from every harness turn, and the
  existing `harness.list` tool returns them as `rate_limits`. fleetd reads that for the `karmax` row,
  so the roster covers all nine subscriptions.
- `cost.total_cost_usd` is a list-price estimate. On subscriptions nothing is billed per token; it's
  kept to compare tasks and agents, labelled as such.

### Who sees it

| reader | how |
|---|---|
| **The orchestrator** | `fleetctl status --json`: one compact row per agent — state, task, idle, 5 h %, 7 d %, reset times, context %, allowed models. Read before assigning work. Its persona says: don't give an agent at `low` or `reserved` a big task when another is free. |
| **The orchestrator, pushed** | fleetd POSTs state changes (`fleet.agent.cooling`, `.disabled`, `.stale`, `.idle_on_task`, `.crashed`) to a KARMAX **webhook route** — `webhooks.routes` in `karmax.yaml` already turns a POST into a bus event — so they arrive as orchestrator turns. Existing feature, no daemon code. |
| **You, at a terminal** | `fleetctl ls` (AGENT HOST STATE TASK IDLE 5H% 7D% RESET CTX% WORKTREES), `fleetctl quota`, `fleetctl events agent-03 --since 2h`, `fleetctl usage --by agent --since 7d`, `fleetctl history agent-03`. All take `--json`. |
| **You, in the KARMAX app** | A **Fleet** dashboard, created once with KARMAX's existing `dashboard` tool. fleetd rewrites its `data/*.json` under `~/.karmax/dashboards/<id>/` every tick; the app renders straight off those files. No daemon code. |
| **You, on your phone** | `karmax notify` for the few things worth a push: an agent disabled, all agents cooling, a crash loop, a 7-day window ≥ 90 %. |

## Putting the fleet to work from any Claude session

Any Claude Code session on the machine running `fleetctl` (and the orchestrator) can hand work to the
fleet and read it back by task id, without knowing which agent is free:

    fleetctl skill install        # writes ~/.claude/skills/fleet/SKILL.md (and fleet-memory); `fleetctl guide` prints the same text
    fleetctl run --repo <repo> "<brief>"      # picks a free agent, prints the task id
    fleetctl wait --task <id>                 # blocks until the agent reports
    fleetctl result <id>                      # reports, branch commits, state
    fleetctl followup <id> "<fix this>"       # or: fleetctl cancel <id>
    fleetctl done --task <id>                 # after reviewing and merging

The skill carries the whole loop (briefs, fan-out, review rules, exit codes), so a fresh session that
sees a task it can delegate knows how.

## The operator's memory, read-only

The orchestrator keeps the operator's long-term memory in a gitloom namespace. fleetd lets the fleet read it
(`memory:` in fleet.yaml; no section, no memory):

    memory:
      url: http://127.0.0.1:8787     # gitloom on fleetd's host
      key_env: GITLOOM_API_KEY       # its key, in the env file
      namespace: mira                # the orchestrator's namespace

    fleetctl memory recall "<query>" [--limit N] [--json]   # GET /v1/memory/recall
    fleetctl memory suggest "<fact>" [--why "..."]           # POST /v1/memory/suggest
    claude mcp add --scope user fleet -- fleetctl mcp       # the same, as MCP tools memory_recall and memory_suggest

- **Only the orchestrator writes.** A gitloom key cannot be made read-only, so it stays in fleetd, which
  sends it only on GETs of gitloom's read routes and never returns it, in an answer or an error. A
  suggestion is a message to the orchestrator (filed under the agent's task); it saves it with its own
  tools, or not.
- **Who may.** The full scope (the operator, their sessions, the orchestrator) always. An agent only while
  its current task, in the ledger, was assigned by the operator, an operator session or the orchestrator;
  never on a member's task. A member on the tailnet and the console never, not even to read. Secrets and
  the KARMAX gateway follow the same rule (`operatorWork` in fleet/internal/server).
- **Which agent.** Every agent has the same relay token, so each container also gets `FLEET_AGENT_KEY`
  from `fleetctl render`: an HMAC of its agent's name under the full token, which no agent holds. fleetctl
  sends it as `X-Fleet-Agent: <name>:<key>`; fleetd checks it and refuses one that does not verify. A
  worker in the container (agent-03:2, agent-03-s2) uses its container's key. An agent with a key tells as
  itself only. After upgrading fleetd, `fleetctl render` and recreate the agents to hand out the keys.
- **The skill.** `fleetctl skill install` writes `~/.claude/skills/fleet-memory/SKILL.md` too, for the
  scopes that may use memory (full: the operator, by token or on the tailnet; relay: an agent): recall
  before work the operator has history with, suggest durable facts, never secrets. It asks fleetd who it
  is (`GET /v1/whoami`); for a member it writes the fleet skill alone and removes a fleet-memory skill
  fleetctl wrote earlier (one whose frontmatter is fleetctl's; nothing else in the directory). When
  fleetd cannot say, it leaves fleet-memory as it is.
- **The MCP server lists what you may use.** `fleetctl mcp` asks `GET /v1/whoami` at `initialize` (or
  the first `tools/list`), and lists `memory_*` and `karmax_*` only for full and relay; a member, and
  anyone fleetd cannot place, sees none of them, and a `tools/call` of one is refused with JSON-RPC
  error -32602 before fleetd is asked. fleetd still decides each call; this only keeps a session from
  being offered tools it would be refused. A whoami that fails is asked again on the next request.

## From anyone's machine — the tailnet

fleetd has a fourth listener, on Kali's Tailscale address (`tailnet:` in fleet.yaml). It takes no token:
each request is whoever tailscaled's WhoIs says owns the calling address, and that login's role decides
what it may do.

    tailnet:
      listen: 100.64.0.1:7878        # bound even before tailscale0 is up (IP_FREEBIND)
      operators: [you@example.com]   # the full token's scope
      members: ["*@example.com"]     # their own tasks only
      member_open: 2                 # open tasks one member may hold

- **Who is nobody.** A tagged device (a subnet router, whose traffic may be anyone's), an address
  tailscaled does not know (a subnet-routed VPC range: the ACL lets it reach Kali, WhoIs does not name
  it), a login of another domain shared into the tailnet, anyone on neither list: 403. A token sent
  here counts for nothing.
- **A member** runs tasks as themselves (`from` is their login, whatever they send), at most
  `member_open` open at once, and sees and acts on only their own: `wait --task`, `result`,
  `followup`, `cancel`, `done --task`, `tasks`, and `peek` of an agent at work on theirs; plus `ls`,
  `pick`, `quota`, `guide`. Not the operator's inbox, events, `prompt`, `tell`, `assign`, rotate,
  enable, restore, archives, or the hosts. The agent reports to their login (`fleetctl tell
  name@example.com`); fleetd files it under the task, and their `wait --task` returns it.
- **Install.** fleetd serves fleetctl to anyone on the list: `curl -fsSL http://<kali>.<tailnet>.ts.net:7878/install.sh | sh`
  (Windows: `irm http://<kali>.<tailnet>.ts.net:7878/install.ps1 | iex`) downloads the build for
  their OS from `/dl/`, runs `fleetctl connect` (saved in their config dir; it prints who fleetd knows
  them as), and `fleetctl skill install` (the fleet skill; fleet-memory too for an operator). Then, if
  `claude` is on their PATH, it registers the MCP server in their Claude Code user config:
  `claude mcp add --scope user fleet -- <installed fleetctl> mcp`. A `fleet` server already there with
  the same fleetctl is left alone; one with another path is removed and added again; no other server is
  touched. It prints one line saying which, or, without `claude`, the command to run later. The builds come from `fleet/scripts/dist.sh`, into
  `~/.karmax/fleet/dist`; rerun it after changing fleetctl.
- **What the agents can read.** Anyone on the list can brief an agent, and an agent reads all of
  `~/code` and pushes as the fleet's GitHub account. Credentials kept under `~/code` are hidden from
  agents with `hide_from_agents: [secrets]`: each path is covered by an empty read-only tmpfs in the
  agent containers (not in the orchestrator's). Anything else under `~/code` an agent can read, a member
  can ask for. To give an agent a key for one task, use a secret (next section), not a file under `~/code`.

## Secrets — a key for one task

Every agent's env file holds the same fixed set of credentials, so before this a key meant for one
task ended up in the shared code directory, where every agent and every member's task could read it.
Now the operator, the operator's sessions and the orchestrator (the full scope) give the key to fleetd,
and grant it to a task or an agent:

    fleetctl secret put openai < key.txt            # stdin (one trailing newline dropped), or --file
    fleetctl secret grant openai --task T42         # or --agent agent-02 [--ttl 2h]
    fleetctl secret ls                              # names, grants, last read: never a value
    fleetctl secret revoke openai --task T42 ; fleetctl secret rm openai

The brief names the secret, never its value. The agent uses it without seeing it:
`fleetctl secret exec OPENAI_API_KEY=openai -- <cmd>` runs a command with the value in its environment,
and `fleetctl secret get deploy-key --to deploy_key` writes it to a 0600 file under `/work/.secrets` (other
paths are refused). `--print` sends it to stdout; the agent brief says not to use it, because an agent's
output becomes its model's context.

- **At rest.** Values are sealed with AES-256-GCM, with the secret's name as associated data, in the
  ledger's `secrets` table. The key is `secrets.key` in fleetd's state dir (0600, 32 random bytes),
  created on first use. A key file that others can read is refused. If you lose the key you lose the
  secrets, so back it up apart from the ledger. A value is never logged, put in an event or an error, or
  returned by `ls`.
- **Who may read.** Only an agent whose identity fleetd has verified (the agent-identity header, not a
  name the agent sends), whose current task is open and its own on the ledger, and whose task was
  assigned by the operator, the orchestrator, an operator's session (any assigner name without an @) or a
  tailnet operator login. A task a member ran never gets one, even if granted. The grant must name that
  task or that agent. A task's grant ends when the task closes (done, close, cancel or superseded), and
  any grant ends at its `--ttl`. Members (the tailnet `member` scope) and the console cannot use the
  secret routes at all, not even `ls`.
- **Audit.** Every read is a `fleet.secret.read` event naming the agent, task and secret. A refusal is
  `fleet.secret.denied`. Put, grant, revoke and rm are events too. `secret ls` shows the last read.
- **Redaction.** Before fleetd stores or delivers any message (tell, report, brief, follow-up, the outbox),
  it replaces every stored secret's value with `[secret:<name>]`. That covers secrets granted now or
  earlier, and each long line of a multi-line key. This is one hook, `redactSecrets` in
  `fleet/internal/daemon/secrets.go`. It is a backstop, not a licence: agents are told never to print
  or send a value.

## The operator's integrations — the KARMAX gateway

KARMAX holds the operator's integrations: WhatsApp (wacli), Google mail, calendar and drive, GitHub,
Slack, Notion, Jira and more, behind its tool API (`GET /api/tools`, `POST /api/tools/{name}`) and a
token that can do everything, a shell included. Agents never get that token. fleetd holds it and calls
the tools for them, under the operator's rules:

    fleetctl karmax tools                       # what this agent may call, each read or write
    fleetctl karmax call <tool> k=v … --reason "<why>"   # k:=<json> for numbers and lists; --json-args '{}'

- **A read runs at once.** A tool is a read when fleet.yaml's `karmax.tools` says so, else when a word
  of its name after the first (words split on dots and underscores) is a read verb (`list`, `get`,
  `read`, `search`, `view`, `show`, `find`, `recall`, `retrieve`, `monitored`, `lookup`, `query`,
  `history`, `status`, …) and none is a write verb (`send`, `set`, `delete`, `create`, `edit`, `add`,
  `update`, `sync`, …): `google.mail.search`, `notion.query_database`, `whatsapp.monitored`,
  `whatsapp_list_chats`, but not `whatsapp_set_chat_locked`. Every read is free, so KARMAX's read-only
  tools whose names do not say so are reads too (checked against their descriptions): `slack.channels`,
  `slack.user`, `google.chat.spaces`, `instagram.inbox`, `instagram.reads`, `instagram.commenters`,
  `instagram.call` (Instagram reads only), `keka.departments`, `keka.employees`, `keka.leave`,
  `activity.recent`, `cost.report`, `karmax.capabilities`, `tools.load`, and wacli's `whatsapp_status`,
  `whatsapp_logs`, `whatsapp_message_receipts`, `whatsapp_webhook_deliveries`, `whatsapp_group_info`,
  `whatsapp_call_status`, `whatsapp_check_numbers`, `whatsapp_resolve`, `whatsapp_download_media`.
  `whatsapp_group_participants` (it adds and removes people), `whatsapp_group_invite_link` (it can
  reset the link) and `whatsapp_sync` stay writes.
- **A write waits for the orchestrator.** Everything else is a write. fleetd files it on the ledger
  (`approvals`: id, agent, task, tool, arguments, reason, state, who decided, when) and tells the
  orchestrator at once through the relay: the tool, its arguments, the reason, the first line of the
  task's brief, and how to answer. She approves low-risk writes herself; for anything destructive, or
  that sends or deletes on the operator's behalf, she asks the operator on WhatsApp first and decides
  from their answer: `fleetctl approve <id>`, `fleetctl deny <id> --reason "…"`, `fleetctl approvals`
  for what is pending. The agent's `call` blocks until then (`--timeout`, 30m by default; a request
  nobody decides expires), runs the tool once, and prints the result — or exits 1 with the refusal.
  Nobody decides on their own request, and an agent cannot decide at all (the routes are full scope).
- **Some tools are always denied**, whatever `karmax.tools` says: `shell.exec`, `file.*`,
  `http.request`, `harness.*`, `task.*`, `scheduler.*`, `proposal.*`, `orgops*`, `memory.ingest` and
  `memory.forget` (the orchestrator alone writes memory), anything named for credentials or secrets,
  anything fleet-related, and anything that runs code or starts an agent: `claude_code.call`,
  `codex.call`, `subagent.spawn`, `sandbox.start`, `orchestrator.send`, and every `*.exec`, `*.run` and
  `*.spawn`. They are left out of `karmax tools` and refused with nothing filed.
- **From an MCP client.** `fleetctl mcp` serves the same two calls as tools, `karmax_tools` and
  `karmax_call` (`tool`, `args`, `reason`, `timeout`); a write's call waits for its approval like the
  CLI's, while the session's other calls go on.
- **Who may use it** is decided by fleetd, as for memory and secrets: the full scope (the operator,
  their sessions, the orchestrator) may; a member on the tailnet never may, not even to read; an agent
  may only while its current task — by the ledger's assigner, never by anything the agent sends — came
  from the operator, an operator's session or the orchestrator, and it proves which agent it is with its
  container's key (`X-Fleet-Agent`).
- **Audit.** Every call and decision is an event (`karmax.call`, `karmax.decision`) with the tool, agent,
  task, class and decision. Events carry argument names, never their values; the values are in the
  approvals ledger, which only the full scope reads.

    karmax:
      api_url: http://127.0.0.1:9091     # KARMAX's API; fleetd's env file holds KARMAX_API_TOKEN
      tools:                             # explicit classes override the name; prefix* entries allowed
        linkedin.post: deny
        github.*: write
        x.post: deny

## Files to and from an agent

An agent's `/work` is a private volume, and fleetctl users on other machines have no Docker access, so
files travel through fleetd:

    fleetctl cp ./notes.pdf ./data agent-03            # into /work/inbox/ (a directory is copied whole)
    fleetctl cp ./logs agent-03:/work/scratch          # into another directory
    fleetctl cp agent-03:/work/wt/topic/out ./results  # out of the container, into ./results
    fleetctl run --attach ./spec.pdf --attach ./data "Build to the spec."
    fleetctl sync ./site agent-03:/work/site           # rsync; or: sync agent-03:/work/site ./site

The transport is a tar stream over HTTP (`PUT` and `GET /v1/agents/{agent}/files?path=`). fleetd pipes
it into `tar -x` or out of `tar -c` run in the container through the host's exec prefix, and never
holds an archive in memory. Rules, enforced by fleetd, by the container-side script and again by
fleetctl on whatever it unpacks:

- Inside the container only `/work` and `/tmp` (a path that leads out through a symlink is refused too).
- File modes are kept, minus setuid, setgid and sticky; owners are dropped.
- Only files, directories and symlinks. A symlink must be relative and point inside the copied tree;
  an absolute link or one that climbs out is refused (copying a tree that holds one fails, naming it),
  and no entry is written through a link the archive made. Nothing is followed on the way in or out.
- One transfer is at most `files.max_bytes` of tar (`files:` in fleet.yaml, default 2 GiB); past it the
  stream is cut with a clear error (HTTP 413). Files unpacked before that stay.

`run --attach` places the task (`attach: true` on `POST /v1/run`), copies the files into
`/work/inbox/<task>/`, and only then delivers the brief (`POST /v1/tasks/{id}/brief`), with one more
sentence naming that directory. If the copy fails the task is closed as `undelivered`. The task id must
be letters, digits, `.`, `_` and `-`.

Access: the operator's token and tailnet operators may copy to and from any agent. A tailnet member
only to and from an agent at work on one of their own open tasks (the same ownership rule as `peek`).
Agents (relay scope) and the console may not.

`fleetctl sync` runs `rsync -a -e "fleetctl rsh"`. `fleetctl rsh <agent> <cmd…>` is a stream both ways
through `POST /v1/agents/{agent}/rsh` that runs the command in the container, and it is full scope
only. It needs `rsync` in the image (the Dockerfile installs it), so it works once the new image is
deployed, and rsync on the machine that runs `sync`.

## Prompting an agent directly

The live panel, the 3D office and the console's own keyboard handover (`fleetctl console attach`) are described in [FLEET-CONSOLE.md](FLEET-CONSOLE.md).

| command | does | authority |
|---|---|---|
| `fleetctl ssh agent-05` | the live session in your terminal, from any machine with fleetctl, through fleetd (below): read-only | you, or a guest on a lent agent |
| `fleetctl ssh agent-05 --type` (`attach`) | the same with the keyboard: one typist per worker. `Ctrl-]` leaves; the session keeps running and receiving messages | you, or a guest on a lent agent |
| `fleetctl ssh agent-05 --shell` | `bash -l` in the container, in `/work` | you, or a guest on a lent agent |
| `fleetctl prompt agent-05 "…"` | types into the pane (`tmux send-keys -l`, then Enter). If it's mid-turn, Claude Code queues it as your next message. | you |
| `fleetctl peek agent-05 [-n 80]` | last lines of the pane, no attach | — |
| `fleetctl ls` | AGENT HOST ACCOUNT STATE SESSION TASK IDLE WORKTREES | — |
| `fleetctl history agent-05` / `archive ls --since 7d` | past sessions and archives | — |
| `fleetctl tell agent-05 "…"` | peer message (the relay) | peer |

### `fleetctl ssh` — terminals through fleetd

`fleetctl ssh <worker>` opens a websocket to fleetd's `GET /v1/term/{worker}`, and fleetd runs, under a
pty, the host's exec prefix with `exec -it <container>`:

| mode | in the container |
|---|---|
| watch (default) | `tmux attach -t =<pane> -f read-only,ignore-size`: keystrokes are dropped, and the window keeps its size |
| `--type` | `tmux attach -t =<pane>`: the window takes your terminal's size while you type, and goes back to 220×50 when you leave |
| `--shell` | `bash -l` in `/work` |

(`<pane>` is slot 1's tmux session `main`, or slot k's `main-k`.) It needs no fleet.yaml, ssh or docker on your machine:
it works from wherever fleetctl reaches fleetd (a fleet host, or `fleetctl connect` over the tailnet), on
macOS, Linux and Windows. The terminal goes raw, a resize is sent at the start and on every change, and
`Ctrl-]` leaves, printing `[detached from agent-05]`.

- One typist per worker. A second `--type` is told who holds it (`held by …`) and exits 1; an admin may
  `--force`, which turns the holder into a watcher (it is told so, and watches on).
- Watchers are many and never resize the session. A watcher that sends nothing for 12 hours is closed.
- An admin may use any worker in any mode; a guest only the workers of the agents lent to its device.
- Every session's start and end is a `fleet.term` event (who, worker, mode, length, how it ended).
- When the session's websocket closes, fleetd ends the exec and whatever it left in the container (each
  session's processes carry `FLEET_TERM=<id>`). A container that is gone ends the session with its error.

`GET /v1/screens?workers=a,b&lines=N` is the TUI's screen previews: each worker's last lines with ANSI.
fleetd captures every asked slot of a container in **one** exec (`tmux capture-pane`, which starts nothing
in the agent's session), shares a capture among every viewer for 2 seconds (and viewers who ask at once
share the capture in flight), and runs at most two captures per host at once.

`prompt` is your keyboard, so it is not a model-callable tool anywhere: KARMAX's orchestrator uses
`SendMessage` / `tell`, never `prompt`. From your phone, go through the orchestrator. If you later want
operator-authority prompts from WhatsApp, make it a fixed command KARMAX parses itself, not a tool the
model can reach for.

## Hosts

The operator's panel on PC2 (virtual display, kiosk, RustDesk) and how to install, undo and troubleshoot it: [FLEET-CONSOLE.md](FLEET-CONSOLE.md).

**Kali (orchestrator + 3 agents).** Docker from Kali's repos (`docker.io`, `docker-compose`),
`nfs-kernel-server` exporting `~/code` to PC2's IP only (`rw,sync,no_subtree_check`, NFSv4), firewall
allowing NFS and fleetd's relay port from PC2 only. NFSv4 delegations are off (`fs.leases-enable = 0`
in `/etc/sysctl.d/90-fleet-nfs.conf`): the code is used on Kali too, and with them an open on Kali of a
file PC2 holds one on waits for PC2 to hand it back — 45 s a file when PC2 is cut off or powered down
(seen after a partition: every review stalled 45 s). Kali is rolling — `apt-mark hold` docker and nfs,
and don't `full-upgrade` with agents mid-task.

**PC2 (5 agents).** Don't build a custom distro: a hand-rolled ISO is one more thing to maintain.
Install **Debian 13 netinst** with only "SSH server" + "standard system utilities" (~250 MB idle,
same base as the containers). `host-bootstrap.sh`: Docker CE, `nfs-common`, a `fleet` user in the
`docker` group with key-only SSH (docker group ≈ root), `$HOME/code` via fstab
`_netdev,x-systemd.automount`, suspend masked, `unattended-upgrades` for security only, compose up at
boot. Flatcar is the immutable alternative if you want an appliance.

**Sizing.** ~3–4 GB RAM and 2 cores per agent (the CLI is ~0.5 GB; builds and tests are the rest).
The 3/5 split is a starting point — change `fleet.yaml`, nothing else.

`gc.auto=0` in the image's gitconfig; `git gc` runs from a Kali cron, so eight agents never gc one
`.git` at once.

## Sandbox agents — lent to guests

`sandbox: true` on an agent in `fleet.yaml` makes it one the operator can lend to a guest device
(`fleetctl access lend`). Defaults: `slots: 1`, `memory: 1536m`. The point is that nothing of the
operator's is in its container, so `fleetctl render` gives it, compared with an ordinary agent:

- its own `<agent>-code` volume at `/work/code` (`FLEET_CODE`), not the code directory, and none of
  `fleet-peers`, `fleet-socks` or the hidden-path tmpfs;
- its own PID namespace (`init: true`), not the anchor's;
- no `DOCKER_HOST`, build key, GitHub tokens, git identity, SSH aliases or KARMAX address;
- one fleet credential: `FLEET_SANDBOX_KEY`, HMAC-SHA256 of its name under the full token, which fleetd
  takes as scope `sandbox` (whoami, its own task, and tell or ask its task's assigner) and as its
  telemetry token. No relay token and no agent key;
- the `fleet-sandbox` network: a bridge on `sandbox.subnet` (default `172.31.250.0/24`), no traffic
  between its containers.

Its brief and the fleet-agent skill tell it what it is: code comes in with `fleetctl sync` into
`/work/code`, and results go back the same way. Picks pass a sandbox over unless the run is a guest's
it is lent to; an admin may still name it with `--agent`. A question on a guest's task is told to the
guest again after `question_escalate`, never to the orchestrator.

### The network rules — install once per host, with sudo

`fleetctl render` writes, for each host that runs a sandbox, `sandbox-net.<host>.sh` and
`fleet-sandbox-net.<host>.service` beside its compose file. The rules let the subnet reach fleetd's
relay and telemetry ports (on fleetd's host, the host itself; elsewhere, the address `fleetd_url`
names), drop everything else to the host, and drop forwarded traffic to 10/8, 172.16/12, 192.168/16,
100.64/10 and 169.254/16; the internet is allowed. They live in chains of their own
(`FLEET-SANDBOX-IN`, `FLEET-SANDBOX-FWD`, from `INPUT` and `DOCKER-USER`), flushed and added again on
each run. On each such host, as root:

    install -m 0755 sandbox-net.<host>.sh /usr/local/sbin/fleet-sandbox-net.sh
    install -m 0644 fleet-sandbox-net.<host>.service /etc/systemd/system/fleet-sandbox-net.service
    systemctl daemon-reload && systemctl enable --now fleet-sandbox-net

Install them again after a render that changes `fleetd_url`, `otlp_url` or the subnet. The unit runs after
Docker and again whenever Docker restarts. fleetd needs no root to check them: each hour it runs a probe
inside every sandbox (ssh on the host and on fleetd's address must not answer within 3 s; fleetd's
`/healthz` must). A sandbox that fails is disabled (`sandbox network unguarded`) and you are told once; it
comes back by itself when the probe passes. The image makes `/work/code` the agent's, so rebuild it
(`fleetctl image build`) before the first sandbox starts.

## Packaging — `packaging/fleet/` (assets only)

`Dockerfile` (debian:trixie-slim, tini, tmux, git, gh, jq, ripgrep, Go, Node 22, Bun, Python, pinned
Claude Code, the `karmax` and `fleetctl` CLIs, your UID/GID), `agent-run`, `home/settings.json`,
`home/CLAUDE.md.tmpl`, `compose.yaml` (anchor + `karmax-brain` + agents, parameterised per host),
`fleet.yaml.example`, `host-bootstrap.sh`, `spike.sh`.

## Phases

Each lands on its own and is verified before the next.

**0 — Spike (Kali, half a day).** `spike.sh`:
- S1: two containers in one anchor namespace, **on two subscriptions**; a session in one messages an
  interactive tmux session in the other, which writes a file. A control container outside the
  namespace must fail.
- S2: a `-p` stream-json session gets messaged while idle — does it start a turn on its own, and what
  does that look like on stdout? (Shapes the background-turn change.)
- S3: `tmux send-keys` into a live session runs as a prompt (`fleetctl prompt`).
- S4: a session started with a bad token: the `StopFailure` hook fires; print its payload.
- S5: closing a stream-json session's stdin through `docker exec -i` ends the process inside.
- S6: the ledger taps — inside a container on a `setup-token` subscription, the status line carries
  `rate_limits` (5 h / 7 d used %, `resets_at`), and the hooks write the event log.
- Plus, by hand: PC2 over NFS — `git worktree add --lock`, and a `go build` timed on `/work` vs `~/code`.
- *Decides:* the socket dir path, the version to pin, the `StopFailure` fields fleetd reads, the
  background-turn wire shape, whether quota is proactive (status line) or only reactive (`StopFailure`).

**1 — Hosts.** Kali prep, PC2 install + `host-bootstrap.sh`, `docker context create pc2
--docker host=ssh://fleet@pc2`. *Verify:* `docker --context pc2 ps`; a file touched on PC2 in `~/code`
shows on Kali with your UID.

**2 — Agents by hand.** Image, compose, `agent-run`, settings, `CLAUDE.md`, the status-line and hook
scripts, OTel env. *Verify:* `docker exec -it … tmux attach` works; two agents on one host message each
other; kill a container and it comes back on the same session; `status.json` shows the account's
windows and `events.jsonl` grows; OTel lines arrive at a throwaway `nc -l` listener.

**3 — KARMAX harness changes.** The table above, with tests. *Verify:* the orchestrator runs in
`karmax-brain`; `/list-agents` from an agent shows `karmax`; an agent messages it while idle →
`harness.turn.background` on the bus, and the next `karmax ask` gets *its own* answer; idle an hour →
still live.

**4 — fleetd / fleetctl.** Reconcile, archive, restore, attach/prompt/peek/ls, relay, the ledger
(ingest, OTLP receiver, retention), the webhook push and the dashboard writer. *Verify:* `fleetctl ls`
matches what each pane shows; usage per agent agrees with the transcripts to within rounding; push one
agent past 80 % and see `low` in the roster; kill a
pane → resumed; `fleetctl done` → archived and fresh; restore it; leave a dirty worktree and confirm
archive keeps it; a relayed message Kali→PC2 and its reply; a bad token → `disabled` + one alert.

**5 — Orchestrator wiring.** The persona paragraph. *Verify:* ask KARMAX on WhatsApp to fix
something in a repo; watch it pick an idle agent, message it, get the report, close the task;
`fleetctl ls` shows the agent back on standby with the old session archived.

## Risks

- **The anchor-namespace trick is undocumented.** If a Claude Code release changes discovery, the
  fallback is one container per host holding all its agents (a tmux window each, own token per window
  via env). fleetd doesn't change: a target is an exec prefix plus a pane name.
- **`agents --json` and the registry are young interfaces.** Pin the version; a contract test runs
  `agents --json` against the image on every rebuild.
- **Shared namespace, shared blast radius.** Agents on a host see and can signal each other's processes.
  Acceptable on a home LAN; the fallback has the same property.
- **Quota is per agent.** One agent can't starve another, but a big task can outlast its agent's
  5-hour window. It cools and resumes in place. If it can't wait, the orchestrator starts it on another
  agent from the branch the first one committed so far. The transcript doesn't move with it: two agents
  believing they did the same work is worse than a handover note.
- **Nine long-lived tokens.** Only in `~/.karmax/.env` and 0600 per-agent env files. `setup-token`
  tokens can only make model requests, which caps what a leaked one can do.
- **Cross-PC is the weak link.** It is a relay because Claude Code offers nothing native between
  different accounts on different machines. If Kali can hold all eight agents, there is no relay at all.
