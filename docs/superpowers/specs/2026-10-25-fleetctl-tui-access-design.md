# fleetctl TUI, ssh into any session, and device access

Status: design approved in chat 2026-10-25; this spec awaits the operator's review.

## Goal

Three things, one release:

1. **A TUI.** `fleetctl` opens a full-screen, branded terminal UI with the console's look and the
   fleet's observability: agents, live screens, tasks, events, quota, hosts, and access.
2. **`fleetctl ssh`.** Open any agent slot's live Claude Code session in your terminal, from any
   machine that runs fleetctl: Kali, the operator's Mac, a friend's laptop.
3. **Device access.** Only devices the operator tags can use the fleet. A device is either an admin
   (everything) or a guest (only the sandbox agents lent to it). Identity is Tailscale's; there are
   no passwords and no tokens on anyone's machine.

## Decisions (the operator's)

| Question | Decision |
|---|---|
| Roles | Two: **admin** and **guest**. No member role, no login wildcards. Untagged devices get nothing. |
| Who is admin | Devices tagged admin, and fleetctl on fleetd's own machine (the full token on loopback). A login is not admin by itself. |
| What a lent agent holds | Its own lightweight sandbox with none of the operator's information. Code goes in and out with `fleetctl sync`; the agent's skill says so. |
| Keyboard | A guest may watch and type in the sessions of the agents lent to them. |
| TUI views | Fleet (console look), live screens, tasks, events and messages, quota and cost, hosts and health, access; sprites acting out what each agent is really doing. |
| Branding | KARMAX and fleetctl, "made by MelloB". |
| First sandboxes | agent-09, agent-10, agent-11. The operator names any others later. |
| Builders | Fleet agents, on branches, reviewed and merged by the operator's session. |

## Architecture

fleetd stays the hub. Every new capability is a fleetd route that checks the caller first, with a
fleetctl command (and a TUI view) as its client:

```
fleetctl / TUI ──tailnet──▶ fleetd ──┬─ access: who is this device, what may it touch
 (any machine)                       ├─ /v1/watch   one scoped snapshot stream for the TUI
                                     ├─ /v1/screens batched screen captures for previews
                                     ├─ /v1/term    websocket ⇄ pty ⇄ docker exec … tmux attach
                                     └─ existing routes (run, wait, tasks, sync, …), now guest-aware
```

New packages:

- `fleet/internal/access`: devices, tags, knocks, and the decision "what may this caller do".
- `fleet/internal/fleetapi`: the wire contract shared by fleetd and the TUI (snapshot, screens, term
  frames, access listing). Written first, so the TUI is built against a fake while fleetd is built.
- `fleet/internal/term`: fleetd's side of a terminal session (pty, modes, the one-typist lock).
- `fleet/internal/tui`: the Bubble Tea app. `cmd/fleetctl` only starts it.

## 1. Access

### Identity

The tailnet listener already asks tailscaled's LocalAPI `whois` who is calling. It now keeps the whole
answer: the node's **StableID**, its **name** (`friend-laptop.tail1234.ts.net`), and its owner's
**login**. `tailnet.Who` gains `StableID`. Tagged devices (no login) stay refused.

A device reached through Tailscale node sharing (the operator shares the fleetd machine with a
friend's email; the friend keeps their own tailnet) answers `whois` with the friend's node and login.
The build verifies this against a real shared device before release, and the knock flow below shows
the name exactly as fleetd sees it, so the operator never has to guess it.

### Roles and tags

| Role | How you get it | May |
|---|---|---|
| admin | `fleetctl access admin <device>`, or the full token on loopback | everything there is today with the full token, plus access changes |
| guest | `fleetctl access lend <device> <agent>…` | run tasks on lent agents only; read, follow up, cancel and close their own tasks; watch, type and open a shell on lent agents; sync and copy files with lent agents; see only lent agents and their own tasks |
| none | anything else | install fleetctl (`/install.sh`, `/dl/`), and be told how to ask for access |

A tag binds the device's StableID; the name is shown, never trusted. Renaming another machine to an
admin device's name gets nothing. A device's role is admin or guest, never both; `admin` on a guest
removes its lends, and `lend` on an admin is refused.

Only sandbox agents can be lent (see 2). An agent may be lent to several devices; each guest still
sees and acts on only their own tasks.

### The store

Ledger tables (the ledger's usual `CREATE TABLE IF NOT EXISTS`):

- `devices (stable_id PK, name, login, role, tagged_at, tagged_by)`
- `device_agents (stable_id, agent, PRIMARY KEY (stable_id, agent))`
- `knocks (stable_id PK, name, login, first_at, last_at, count)`

Every change is a `fleet.access.*` event with who made it. Changes take effect on the next request:
no restart, no fleet.yaml edit.

### Knocks

An untagged device that calls the API is refused with:

```
this device (friend-laptop.tail1234.ts.net, friend@example.com) may not use this fleet yet.
Ask the fleet's operator to run: fleetctl access lend friend-laptop <agent>
```

and fleetd records a knock. The first knock of a device in 24 hours notifies the operator through the
existing notifier ("friend-laptop (friend@example.com) asked for fleet access"). The TUI's Access tab
badges new knocks.

### CLI

```
fleetctl access                                 # tagged devices, lends, and recent knocks
fleetctl access admin <device>
fleetctl access lend <device> <agent>...
fleetctl access unlend <device> <agent>...
fleetctl access revoke <device>                 # removes its tag and lends
```

`<device>` is the full tailnet name, or a short name that matches exactly one known device. All
`access` routes are admin-only.

### Enforcement

The existing `member` scope becomes `guest`, keeping its own-task rules, with one addition: every
route that names or picks an agent checks it against the caller's lends. Concretely:

- `run`/`pick` choose only among the caller's lent agents (an explicit `--agent` outside them is 403).
  A guest may choose the model among the agent's models.
- `status`, `tasks`, `events`, `usage`, `quota`, `activity`, `today`, `watch` and `screens` are
  filtered to lent agents and the guest's own tasks. Host samples are cut to the lent containers
  (shown on their cards); guests get no Hosts tab.
- `peek`, `term`, `files`, `rsh` (and so `cp` and `sync`) need a lent agent.
- `memory`, `karmax` (the gateway), `secrets`, `approve`, `rotate`, `enable`, `restore`, `archives`,
  `model` (persistent), `access`, `prompt`, `assign`, `tell` and `inbox` are admin-only. A guest reads
  replies on their own tasks with `wait` and `result`.
- Open-task limit per guest login: `tailnet.guest_open` (default 2).

A task records at assignment whether it is **admin work**: assigned with the full token, or from an
admin device. `operatorWork` (memory, gateway, secrets inside a container) checks that flag instead of
matching assigner names. A guest task, and any task on a sandbox agent, is never admin work.

### Config migration

`tailnet.operators`, `tailnet.members` and `tailnet.member_open` are removed. A fleet.yaml that still
has them fails to load with a message pointing to `fleetctl access`, so nobody believes a wildcard
still works. The operator tags their devices from fleetd's machine after the deploy.

## 2. Sandbox agents

`sandbox: true` on an agent in fleet.yaml. Defaults: `slots: 1`, `memory: 1536m`.

### What its container is

Compared with an ordinary agent, a sandbox container has:

| Ordinary agent | Sandbox |
|---|---|
| the code directory bind-mounted | its own volume; `FLEET_CODE=/work/code` |
| GitHub token(s), git identity, ssh aliases | none |
| `DOCKER_HOST` (the build docker: root on that host) | none |
| shares the anchor's PID namespace | its own (tini as PID 1) |
| shares `fleet-peers` and `fleet-socks` | neither |
| relay token + agent key | one credential, the sandbox key (below) |
| KARMAX URL, memory, gateway, secrets | none |
| default network | `fleet-sandbox`, which reaches the internet and fleetd's relay and telemetry ports only |

The image is the same.

### Its one credential

`FLEET_SANDBOX_KEY = HMAC-SHA256(full token, "sandbox:" + agent)`. fleetd accepts it as a bearer
token that means exactly "sandbox agent X". There is no relay token in the container: a relay token
there would let it speak as an unverified agent. With the key it may:

- report and ask on its own task (`tell`, `ask` with its task's assigner as the only recipient),
- `whoami`, read its own task, receive follow-ups and answers,
- send telemetry (`/v1/logs`, `/v1/metrics`), attributed to the key's agent, not to the
  resource attribute.

Everything else is 403. Its questions go to the guest who assigned the task; they never escalate to
the orchestrator.

### The network

`fleet-sandbox` is a Docker bridge with a fixed subnet (`sandbox.subnet` in fleet.yaml, default
`172.31.250.0/24`) set in the rendered compose file. A root
oneshot unit, `fleet-sandbox-net.service` (installed once per host with sudo, run after docker at
boot), keeps these rules:

- fleetd's relay and telemetry ports at the address the host's `fleetd_url` names are allowed: the
  host itself on fleetd's machine, fleetd's LAN address on any other host;
- everything else to the host itself is dropped;
- forwarded traffic to RFC 1918, CGNAT/tailnet (`100.64.0.0/10`) and link-local addresses is
  dropped; the rest (the internet) is allowed.

`fleetctl render` writes the unit and its rules beside the compose file. fleetd needs no root to
check them: each hour it probes from inside every sandbox that a LAN address and a closed port on the
host refuse, and that fleetd answers.

### What it is told

The brief and the fleet-agent skill gain a sandbox variant: "You are a sandbox lent to a guest. You
have no GitHub access and none of the operator's code or accounts. Code comes in with `fleetctl sync`
from the person who briefs you, into /work/code. Leave results there (or in /work/out) and tell them
the exact `fleetctl sync <agent>:/work/... ./` to fetch them." The guest's fleetctl skill
(`fleetctl skill install` on their machine) says the same from the other side.

### Lending rules

A sandbox agent never takes an admin's task unless named explicitly (`run --agent`), and even then
its task is not admin work. An ordinary agent can never be lent.

### Caveat (stated in `fleetctl access lend`'s output)

Whoever types into an agent can read its Claude login. Lend agents whose subscription you are happy
to share, or the friend's own.

## 3. Terminals: `fleetctl ssh`

### Route

`GET /v1/term/{worker}?mode=watch|type|shell&cols=&rows=` upgrades to a websocket (coder/websocket,
already a dependency). fleetd starts, under a pty (creack/pty, already a dependency):

| mode | command |
|---|---|
| watch | `<exec> exec -it <container> tmux attach -t main-<slot> -f read-only,ignore-size` |
| type | `<exec> exec -it <container> tmux attach -t main-<slot>` |
| shell | `<exec> exec -it -w /work <container> bash -l` |

`<exec>` is the host's exec prefix (`docker`, `docker --context pc2`). Terminal bytes are binary
frames; control frames are JSON text (`hello`, `resize`, `error`, `released`, `held`), in
`fleetapi`.

### Rules

- One typist per worker. A second `type` gets `held by <who>`; an admin may `--force`, which turns
  the holder into a watcher with a `released` frame.
- Watchers never resize the agent's session (`ignore-size`). The typist sizes it while attached; on
  detach fleetd restores the session's configured size.
- Who: an admin any worker, any mode. A guest any mode on a lent agent's workers.
- Every session start and end is a `fleet.term` event (who, worker, mode, duration).
- fleetd ends a session when the websocket closes or the container stops. Idle watchers time out
  after 12 hours.

### CLI

```
fleetctl ssh <worker> [--type] [--shell] [--force]
fleetctl attach <worker>            # the same as ssh --type
```

The client puts the terminal in raw mode, sends resizes on SIGWINCH, and detaches on `Ctrl-]`,
printing how it left. It works on macOS, Linux and Windows. `attach` no longer needs fleet.yaml or
ssh. The PC2 console keeps its own terminal for PC2's screen.

### Screen previews

`GET /v1/screens?workers=a,b,c&lines=N` returns each worker's last N lines (ANSI kept). fleetd
captures with **one exec per container** for all requested slots, caches a capture for 2 seconds,
and shares it among every viewer. At most two captures run per host at once. A preview never
starts a new process in the agent's session.

## 4. The watch stream

`GET /v1/watch?since=<seq>` is a long poll. It returns at once when fleetd has a newer snapshot than
`since`, otherwise when one is made, and after 25 seconds at most. It drains on shutdown like the
other long polls. Each reply is one `fleetapi.Snapshot` scoped to the caller:

```go
type Snapshot struct {
    Seq      uint64
    At       time.Time
    Me       Me              // name, role (admin|guest), device, lent agents
    Workers  []Worker        // daemon.Row + Activity
    Tasks    []Task          // open, and closed in the last 24 h
    Events   []Event         // the last 200, newest first
    Quota    []Account       // per account: 5h/7d %, resets, burn rate, predicted run-out
    Today    Today           // tasks closed, spend, output tokens
    Hosts    []Host          // load, mem, disk, per-container cpu/mem, 60-sample history
    Fleetd   Fleetd          // version, uptime, last tick duration, tick errors
    Knocks   int             // admins only
}

type Activity struct {      // what the sprite acts out
    Kind  string            // thinking | reading | editing | running | pushing | testing | waiting | idle | cooling | down
    Tool  string            // the last tool, e.g. Bash
    Since time.Time
}
```

Sources are what fleetd already has: the reconcile state and status rows, tasks and relays, the events
ledger, quota and usage tables, host samples, and the agents' telemetry. `Activity` comes from the
agents' `tool_result` and `tool_decision` telemetry plus phase:

- Read, Grep, Glob, WebFetch are reading;
- Edit, Write, NotebookEdit are editing;
- Bash is running, except `git push` (pushing) and test commands (testing);
- a working worker with no tool event in 20 seconds is thinking.

A snapshot is made once per tick and whenever an event lands, at most once a second. The run-out
prediction is a least-squares fit of the last hour's 5-hour samples.

## 5. The TUI

### Start

`fleetctl` with no arguments on a terminal opens the TUI; `fleetctl tui` always does. With no
terminal, plain `fleetctl` prints usage as today. `--no-anim` and `NO_COLOR` are respected.

Libraries: Bubble Tea, Lip Gloss, Bubbles, Harmonica (charmbracelet; MIT), pinned to exact
versions at build time.

### Frame

```
╭─ KARMAX ▸ fleetctl ───────────────── admin · your-mac ─── 17 workers · 3 working ── 21:04 ─╮
│ Fleet  Screens  Tasks  Events  Quota  Hosts  Access                                        │
├────────────────────────────────────────────────────────────────────────────────────────────┤
│                                   (the selected tab)                                       │
├────────────────────────────────────────────────────────────────────────────────────────────┤
│ ↑↓ select · ⏎ open · s ssh · t type · r run · / filter · ? help        made by MelloB · v0.3 │
╰────────────────────────────────────────────────────────────────────────────────────────────╯
```

- The "KARMAX" wordmark renders in a gradient; the boot screen animates it once (under a second,
  skipped with any key or `--no-anim`).
- Colors come from the console's theme tokens (`fleet/console/src/theme.css`), as Lip Gloss adaptive
  colors for light and dark terminals.
- Under 80×24 the TUI shows one column and drops sparklines. It never panics on a tiny terminal.

### Tabs

| Tab | Shows | Keys |
|---|---|---|
| Fleet | a card per agent like the console: a line per slot with phase color, sprite, task, model, ctx %, 5h/7d bars, current tool; dense mode when it does not fit | ⏎ detail, s ssh, t type, m model (admin), d done, c cancel |
| Screens | a grid of live previews (only the visible tiles are fetched) | ⏎ zoom, s ssh, t type |
| Tasks | open and recent tasks; detail shows the brief, the message timeline, branch and outcome | r run (form: brief, repo, agent, model), f follow-up, a answer, c cancel, d done |
| Events | the stream, newest first, colored by kind | / filter by agent, kind, task; space pauses |
| Quota | per account: 5h and 7d bars, reset countdown, burn sparkline, predicted run-out; today's spend and tokens | |
| Hosts | per host load, memory, disk; per container cpu/mem sparklines; restarts, unreachable, stale workers; fleetd tick time | |
| Access (admins) | tagged devices, lends, knocks | a admin, l lend, u unlend, x revoke |

Guests see Fleet, Screens, Tasks, Events and Quota for their lent agents and their own tasks, and no
Hosts or Access.

### Sprites

Each slot line has a three-cell sprite that acts out its `Activity`: eyes scanning for reading, keys
clattering for editing, gears for running, a rocket for pushing, a flask for testing, a thought
bubble for thinking, `zzz` for idle, a snowflake for cooling, an X for down. Four to six frames at
about 6 fps, drawn only for visible cards, frozen with `--no-anim`. The data behind them is real; a
sprite never animates without an event behind it.

### Behavior

- Data: one `/v1/watch` long poll, and `/v1/screens` every 2 s while Screens is open. The TUI
  renders locally, so it is smooth over the tailnet.
- Disconnects show a banner and back off from 1 to 15 s. The last snapshot stays on screen, dimmed.
- `s` and `t` suspend the TUI, run the ssh session in the same terminal, and return to the same view.
- Every action is the same route as its CLI command; the TUI has no write path of its own.

## 6. Errors

- fleetd cannot reach tailscaled: 503 "fleetd cannot tell who you are", as today. The TUI shows it.
- Access store unreadable: deny everything on the tailnet listener. Loopback with the full token
  still works, so the operator can repair it.
- A term session whose container is gone: an `error` frame, then close. The client exits 1 with the
  reason.
- The sandbox network rules missing: one alert, and the sandbox agents are marked disabled
  (`sandbox network unguarded`) until the rules are back.

## 7. Testing

- `access`: table tests over (role, lends, route, agent, task owner) → allow/deny for every route,
  through the real server mux with a fake whois. A name collision (same name, other StableID) is
  denied. Mutation-checked.
- Sandbox: render tests (no code mount, no GH_TOKEN, no DOCKER_HOST, own PID namespace, no shared
  volumes, network set, key present, relay token absent); server tests for the sandbox key's scope;
  `operatorWork` refuses sandbox and guest tasks.
- Terminals: a fake docker that runs a real shell under the pty; tests for watch (input ignored),
  type, the one-typist lock, `--force`, resize, `Ctrl-]`, and container exit.
- Screens: one exec per container for many slots; the cache shares captures among viewers.
- Watch: scoping (a guest sees no other agent's row, task or event), long poll, and drain.
- TUI: model update tests, and golden views (teatest) at 80×24, 120×40 and 200×60 for an admin and a
  guest, each with and without animation.
- smoke-v2 grows: tag a fake device as guest, lend a sandbox, run a task as the guest, `ssh` watch
  and type through fleetd, `sync` in and out, and refusals for an untagged device.
- Live acceptance after the deploy, inside agent-09: no code directory, no GitHub or docker
  credentials, no other agent's process visible, the LAN and fleetd's machine's other ports
  unreachable, the internet and fleetd reachable, `fleetctl ls` shows only itself, and
  memory, gateway and secrets refused.

## 8. Rollout

1. The operator's session writes `fleetapi` and merges it, so agents build against one contract.
2. Six fleet agents (PC2 agents with quota to spare; not 09–11, which become sandboxes) build on
   branches: access, sandbox, terminals and screens, the watch stream, and the TUI in two halves
   (frame, Fleet, Tasks, Events; Screens, Quota, Hosts, Access, sprites).
3. Review, trial merge, vet, race tests, smoke-v2, mutation checks; merge to main and push.
4. Deploy fleetd and fleetctl. Install the sandbox network unit on Kali. Convert agent-09..11:
   set `sandbox: true` in fleet.yaml, render, recreate those three containers only (they are idle;
   agent-12 keeps the operator's open task untouched).
5. Remove `operators`/`members` from the live fleet.yaml. Tag the operator's devices as admin
   from Kali as they knock. Rebuild the tailnet dist so the Mac and friends get the new fleetctl.
6. Live acceptance (7). Record a short demo of the TUI for the operator.

## Out of scope

- A web version of the TUI; the PC2 console stays as is.
- Guests on ordinary agents, or guests with GitHub access.
- Per-device quotas or billing.
- Tailscale ACL tags as roles (tagged devices stay refused).
