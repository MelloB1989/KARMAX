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
| you → any agent | `fleetctl attach` / `prompt` / `peek` (below) |
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
agent-run                       # fleet's own loop, ~40 lines of sh
└ tmux server
  └ window "main": claude --name agent-NN --dangerously-skip-permissions [--resume <sid>]
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
| a second session in the container, or one not named `agent-NN` | archive it |
| task closed (`fleetctl done`, or the agent says done and the orchestrator confirms) | archive, rotate to a fresh standby session — **one task, one session**, so context never leaks between tasks |
| standby, idle > 12 h, transcript non-trivial | archive, rotate (standby sessions must not accumulate junk) |
| waiting (task open) idle > 2 h | message the orchestrator: "agent-03 idle 2 h on <task>" |
| waiting idle > 24 h | archive (worktree kept), rotate, tell the orchestrator and you |
| transcript past a size threshold while idle | type `/compact` into the pane — a fleet action you configured, not a peer's |
| 5-hour window ≥ 80 % (status line) | `low` — still works, but the orchestrator's roster marks it so new tasks prefer other agents |
| `StopFailure: rate_limit` | `cooling` until the window's `resets_at` (from the last status-line snapshot; else 1 h). Not stale, never archived for this. Resume in place when it clears. |
| 7-day window ≥ 90 % | `reserved` — small tasks only until the weekly reset; you get one alert |
| `StopFailure: authentication_failed / billing_error / account_on_hold` | `disabled`, alert you once |

All thresholds live in `fleet.yaml`.

### Archive — copy, verify, then stop

1. Only act on an idle session (`status: idle`). A busy one waits for the next tick.
2. Write `~/.karmax/fleet/archive/<agent>/<date>-<sid8>/`:
   - `manifest.json`: agent, host, account, session id/name, task id, started/ended, turn count and usage
     summed from the transcript, cwd, last assistant message
   - `transcript.jsonl.zst` plus any subagent transcripts
   - `worktrees.json`: path, branch, upstream, clean?, unpushed commits
3. Verify by hash. Only then stop the session (`claude stop` / kill the pane).
4. Worktrees: clean and pushed → `git worktree remove`. Anything uncommitted or unpushed → stays,
   locked, listed in the manifest and in `fleetctl ls`. Never discarded by fleetd.
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

## Prompting an agent directly

| command | does | authority |
|---|---|---|
| `fleetctl attach agent-05` | `docker --context pc2 exec -it agent-05 tmux attach -t main` — the live session's full TUI. Detach with `Ctrl-b d`; it keeps running and keeps receiving messages. | you |
| `fleetctl prompt agent-05 "…"` | types into the pane (`tmux send-keys -l`, then Enter). If it's mid-turn, Claude Code queues it as your next message. | you |
| `fleetctl peek agent-05 [-n 80]` | last lines of the pane, no attach | — |
| `fleetctl ls` | AGENT HOST ACCOUNT STATE SESSION TASK IDLE WORKTREES | — |
| `fleetctl history agent-05` / `archive ls --since 7d` | past sessions and archives | — |
| `fleetctl tell agent-05 "…"` | peer message (the relay) | peer |

`prompt` is your keyboard, so it is not a model-callable tool anywhere: KARMAX's orchestrator uses
`SendMessage` / `tell`, never `prompt`. From your phone, go through the orchestrator. If you later want
operator-authority prompts from WhatsApp, make it a fixed command KARMAX parses itself, not a tool the
model can reach for.

## Hosts

**Kali (orchestrator + 3 agents).** Docker from Kali's repos (`docker.io`, `docker-compose`),
`nfs-kernel-server` exporting `~/code` to PC2's IP only (`rw,sync,no_subtree_check`, NFSv4), firewall
allowing NFS and fleetd's relay port from PC2 only. Kali is rolling — `apt-mark hold` docker and nfs,
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
