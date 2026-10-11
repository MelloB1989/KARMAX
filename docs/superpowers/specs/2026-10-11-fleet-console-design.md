# Fleet console — design

Status: design, 2026-10-11, revision 2. Approved in conversation section by section, then
reviewed against the code by agent-01; its findings are folded in. Nothing here is built.

## The goal

A control panel on PC2, the host that runs the fleet's agents. The operator RustDesks in
and sees, on one screen, what every agent is doing — in a cinematic, animated "mission
control" style — and can take the keyboard of any agent's Claude Code session from there.
A second view puts the same fleet in a 3D office: one desk per agent, and on each laptop
the agent's live terminal.

Hard rules:

- **Nothing is fake.** Every number, line and animation is driven by a real source. Each
  value carries the time it was observed, not the time it was fetched. When a source is
  down or a value is old, the screen says so; it never shows stale data as live. No binary
  has a demo or replay mode. Tests use fixtures; panel development uses a snapshot recorded
  from the real fleet, loaded by the dev server, never by a shipped binary.
- **One function, every door.** A capability is one Go function on the server side. Its
  HTTP endpoint is a thin adapter over it, and its `fleetctl` subcommand is a thin HTTP
  client of that endpoint, the way `fleetctl status` already is. The panel uses only the
  public HTTP API. Whatever the panel shows or does, `fleetctl` shows or does, with `--json`
  for scripts. Commands that today bypass HTTP get routes (see "fleetd: every capability over
  HTTP").
- **Config, not source.** Ports, addresses, hosts, repos, display mode and token paths come
  from config. No deployment value is written into the code.

## Decisions

| question | answer |
|---|---|
| Where the screen comes from | A virtual 1920×1080 output on PC2's Intel GPU (kernel-forced connector + EDID file). Fallback: an HDMI dummy plug. |
| Visual direction | "Mission Control": deep navy ground, amber `#FFB547` and cyan `#5CE1E6`, IBM Plex Mono + Barlow Condensed, fine grid, slow scanline. The 3D office uses the same palette and type. Mockups: board A of the "Fleet Console Mockups" canvas. |
| Where the server runs | On PC2, next to the agents, listening on `127.0.0.1` only. |
| Stack | Go server in the fleet module; React + TypeScript panel embedded with `go:embed`; three.js through react-three-fiber; xterm.js for terminals. |

## Two clocks

fleetd reconciles every 30 s (`thresholds.tick`) and stays the system of record: states,
tasks, sessions, quota, usage, history, archives. A 30 s clock cannot drive a live panel,
so the console observes the agents on its own host directly:

| live, from the console (seconds) | recorded, from fleetd (30 s) |
|---|---|
| terminal bytes | phase (`standby`, `working`, `waiting`, `cooling`, `disabled`, `down`) and alerts |
| hook events as they are written (current tool, prompts, stops) | tasks: open, closed, outcome, assigner |
| the status line (model, context %, 5 h / 7 d) | sessions, cost, usage, quota history |
| container CPU, memory, network | archives |
| host load, memory, uptime; git | the orchestrator's quota |

When the two disagree, live wins for display, and the snapshot carries both `at` times. For
the live values to be live, the status line's `refreshInterval` drops from 30 s to 5 s.

## Architecture

```
Kali                                            PC2
fleetd ◀──(LAN :7879, "console" token)── fleet-console ──▶ Chrome kiosk ──▶ RustDesk
 ▲                                          │   │   │
 │                                          │   │   └ docker exec (PTY): tmux attach,
fleetctl ──(ssh pc2, stdio-dial 127.0.0.1)──┘   │       tail -F events.jsonl, status.json
                                                └ docker stats (stream), /proc, git
```

### Roster and hosts

The console shows every roster agent. Agents on the console's own host get the live
sources (terminals, live events, container stats). An agent on another host shows fleetd's
recorded data only, marked "remote · no live view". Today all four agents are on PC2.

The console's config is rendered by `fleetctl render` from `fleet.yaml`'s new `console:`
section, as `console.yaml` beside the host's compose file, and installed on the console
host. It contains:

- the roster and each agent's exec prefix (reused from `internal/target`);
- `listen`, the `fleetd_url` (the relay listener), the token file paths;
- the git repos to watch.

### fleet-console (new: `fleet/cmd/fleet-console`, logic in `fleet/internal/console`)

A systemd service on PC2, on `console.listen` (default `127.0.0.1:7880`).

New dependencies:

- `github.com/coder/websocket` — already used by the KARMAX module;
- `github.com/creack/pty` — `docker exec -it` needs a TTY.

Sources. Each has `ok`, `at` and `error` in every snapshot, and per-agent fields carry their
own `at`:

| source | how | cadence |
|---|---|---|
| terminals | per agent, `docker exec -it <c> tmux attach -r -t main` under a PTY | streamed |
| live events | per agent, `docker exec <c> tail -n0 -F ~/.claude/fleet/events.jsonl` | streamed |
| status line | per agent, `status.json` through the exec prefix | 5 s |
| docker | one long-running `docker stats --format '{{json .}}'` (it streams); `ParseDockerStats` extended with memory limit and net I/O | streamed (~1 s) |
| host | `/proc` | 2 s |
| git | the configured repos: `main` head, `fleet/*` branches, commits in 7 days | 10 s |
| fleetd | `status`, `tasks`, `events?after=`, `activity`, `usage`, `quota` | 5 s |

A source is **stale** when its last success is older than twice its cadence. A streamed
source is stale when its process has exited and not yet been restarted.

### The console's public API

All of it requires the console token (below), as `Authorization: Bearer` or the session
cookie.

| HTTP | fleetctl | function |
|---|---|---|
| `GET /v1/snapshot` | `fleetctl console snapshot [--json]` | `Hub.Snapshot()` |
| `GET /v1/watch` (WebSocket) | `fleetctl console watch [--json]` (NDJSON out) | `Hub.Subscribe()`: snapshot, then patches |
| `GET /v1/term/{agent}` (WebSocket, binary) | `fleetctl console term <agent>` (prints the stream) | `Terms.Watch(agent)` |
| `GET /v1/term/{agent}?control=1` | `fleetctl attach <agent>` | `Terms.Control(agent, who)` |
| `POST /v1/prompt {agent, text}` | `fleetctl prompt <agent> <text>` | proxied to fleetd `/v1/prompt` |
| `POST /v1/rotate {agent}` | `fleetctl rotate <agent>` | proxied to fleetd `/v1/rotate` |
| `GET /healthz` | `fleetctl console health` | sources and their ages |

`fleetctl attach` changes: when the target host runs a console, it goes through
`/v1/term?control=1`, so the one-controller rule binds it too. When there is no console,
it falls back to `docker exec -it`, as today.

`fleetctl console …` reaches the localhost-only listener through SSH. Its `http.Client`
gets a `DialContext` that runs `ssh <host> -W 127.0.0.1:<port>` and wraps the stdio pipes
as a `net.Conn`. Nothing is opened on the LAN.

**Watch protocol.** The first message is `{"type":"snapshot","seq":n,…}`, then
`{"type":"patch","seq":n,"ops":[…]}` (RFC 6902) on every change, batched at most 10 per
second. A client that sees a gap in `seq` closes and resubscribes. On the WebSocket the
patches are text frames; `fleetctl console watch` prints one JSON object per line.

**Snapshot shape.** This is the contract. The Go and TypeScript types are written first, in
`fleet/internal/console/model.go` and `fleet/console/src/model.ts`.

- `at`, `sources{terminals,events,statusline,docker,host,git,fleetd}` → `{ok, at, error}`
- `host` → `{name, uptime_s, load[3], mem_used, mem_total, cpus}`
- `orchestrator` → `{name, five_hour_pct, seven_day_pct, five_hour_resets, seven_day_resets, at}`
  (null while fleetd cannot reach it)
- `tasks[]` → the open ones: `{id, agent, assigner, since}`
- `agents[]`:
  - identity: `id, host, live (bool: on this host)`;
  - from fleetd: `phase, alerts[], task, idle_s, models[], session`, plus `recorded_at`;
  - from the status line: `model, context_pct, five_hour_pct, seven_day_pct, five_hour_resets`,
    plus `statusline_at`;
  - from live events: `current_tool{name, since}|null, last_event_at`;
  - from docker: `container{running, cpu_pct, mem, mem_limit, net_rx, net_tx, at}`;
  - from fleetd's tasks and sessions: `last_task{id, outcome, ended, cost_usd}|null`;
  - the terminal: `term{cols, rows}`.
- `events[]` → the last 200 of the **stream kinds** (below), newest first,
  `{id, at, agent, kind, detail}`
- `today` → `{tasks_closed, spend_usd, output_tokens}`, counted from local midnight
  (fleetd computes it; see below)
- `git` → `{repos[{name, main{sha, subject, at}, branches[{name, agent, sha, subject, at}], commits_7d}]}`
- `activity` → per agent, event counts in 15-minute buckets for the last 24 h

Stream kinds are the events a person reads: `fleet.*`, `SessionStart`, `SessionEnd`,
`UserPromptSubmit`, `Stop`, `StopFailure`, `Notification`. Tool-level events (`PreToolUse`,
`PostToolUse`, OTLP `tool_result`) drive `current_tool` and the activity ticks, but don't
enter the stream.

A hook event from a live agent shows as soon as the console tails it. fleetd records the
same event up to 30 s later, with an id. The console matches the two on (agent, session,
`at`, kind) and keeps one entry, taking fleetd's id. `fleet.*` events exist only in fleetd
and arrive with its 5 s poll. On start, the stream is backfilled from fleetd.

### Terminals

- **Size.** Every agent's tmux runs with `/etc/tmux.conf` from the image:
  `window-size manual`, `default-size 220x50`. A config file survives the rotations and
  restarts that create a new tmux server. No client resizes the agent's window; a smaller
  terminal sees it cropped.
- **Keys.** The same file sets `prefix None`, removes every binding, and binds
  `Ctrl+]` (root table) to `detach-client`. A held keyboard can therefore never run a tmux
  command (no `Ctrl-b :kill-session`), Claude Code gets `Ctrl+B` back, and `Ctrl+]` means
  "give the keyboard back" everywhere: the panel, the office, `fleetctl attach`.
- **Watch** is a `tmux attach -r` client under a PTY: one per agent, opened when the first
  viewer subscribes and closed 30 s after the last one leaves.
- **Control** is a normal attach under a PTY. There is one controller per agent: a second
  request gets 409 with the holder's name, and closing the socket releases it. The lock is
  in the console process; on restart, its control clients die with it (they are its
  children), so no stale lock survives.

### fleetd changes

**Scopes.** `route()` takes a set of scopes instead of a minimum. There are three:

- `full`: everything, as today.
- `relay` (agents): `status`, `tell` as themselves, `tell` to `operator`.
- `console`: every read, plus `prompt` and `rotate`, speaking as `operator`.

The 403 names what the token may do. The console token's env var is `console_token_env` in
`fleet.yaml`. `fleetctl render` writes the token into the console host's `console.yaml`
directory with mode 0600.

**Operator inbox.** `tell` to `operator` is stored, not relayed (the `relays` table already
keeps text), and an agent may send it.

- `GET /v1/inbox?after=` and `fleetctl inbox [--follow]` read it.
- `GET /v1/wait?agent=&task=&timeout=` is a long poll, and `fleetctl wait <agent> [--task]
  [--timeout 30m]` uses it. The CLI's client timeout follows `--timeout`. It returns when:
  - the agent messages the operator;
  - the task closes; or
  - the agent goes `down`, `cooling` or `disabled`.

**Assigner.** `assign` gains `from` (default: the token's identity; `operator` for full and
console tokens, the orchestrator's name when it calls) and an optional `brief`. With a
brief, it delivers the brief to the agent in the same call. That makes `fleetctl give
<agent> --task <id> "<brief>"` one command that records and delivers. The agents' brief
says: report to the task's assigner. When that is `operator`, `fleetctl tell operator …`
lands in the inbox.

**Every capability over HTTP.** Each new route is a `daemon.Fleet` method, and each
`fleetctl` command becomes its HTTP client:

| capability | route (scope) | today |
|---|---|---|
| peek | `GET /v1/peek?agent=&n=` (console, full) | host-only CLI |
| prompt | `POST /v1/prompt` (console, full) | host-only CLI |
| tasks | `GET /v1/tasks?open=1` (console, full) | none |
| activity | `GET /v1/activity?since=24h&bucket=15m` (console, full) | none |
| today | `GET /v1/today` (console, full) | none |
| events | `GET /v1/events?after=<id>&kinds=&limit=` (adds an id cursor) | `since` only, second resolution |
| archive read | `GET /v1/archives/{id}` (full) | none |
| archive prune | `DELETE /v1/archives?older=` (full) | host-only CLI |
| render, up, down | `POST /v1/render`, `/v1/up`, `/v1/down` (full); argv logic moves out of `fleetctl` into `internal/hostctl` | CLI-only logic |
| health | `GET /healthz` (no token: version and uptime only) | none |
| who am I | `GET /v1/whoami` (any token), `fleetctl whoami` | none |
| index | `GET /v1` lists every route and its scopes, `fleetctl api` | none |

The handlers for `events`, `usage`, `history` and `quota` stop calling `ledger` directly;
each becomes a `Fleet` method. Defaults live in the method, not in the handler and the CLI.

`fleetctl` commands that need a host's docker (`attach` without a console, `render` to a
directory, bootstrap) remain host-side, and say so in `fleetctl help`.

**Data fixes:**

- **Usage.** `GET /v1/usage` returns nothing in this deployment. The code path exists:
  OTLP `api_request` → `AddUsage`. Find which link is missing — `otlp_url`, the relay token
  in `OTEL_EXPORTER_OTLP_HEADERS`, reachability of `:4318` from PC2, or Claude Code not
  emitting `api_request` — and fix it. Evidence first.
- **PreToolUse hook** → `event.sh`, so the console knows the tool running now. `event.sh`
  stays one `jq` call that appends and exits 0.
- **`Row` gains** `idle_s` (number), `model`, `alerts[]` and `recorded_at`.
- **The `rotate` reply** says what it will do: "closes task <id> as rotated; takes effect at
  the next idle tick". The panel shows that before you confirm.

### The panel (`fleet/console/`, built to `fleet/internal/console/dist`, embedded)

Two layouts at 1920×1080, scaled to fit the window:

- **Overview.**
  - Header: name, host vitals, clock, browser GPU (WebGL renderer string, so a software
    fallback is visible).
  - Left: agent roster — phase, alerts, idle, model, context / 5 h / 7 d bars, CPU and
    memory, last task, and for a working agent its current tool and running time.
  - Centre: the selected agent's terminal, scaled to fit. It is real and legible at a
    glance, not for reading code.
  - Right: the orchestrator, open tasks, today's totals, the event stream.
  - Bottom: a 24 h activity track per agent.
- **Focus** (`F`, or a click on the terminal). The side columns slide away and the terminal
  fills the width: 220 columns at about 14 px. Taking control always happens in Focus.

Phases on screen:

| phase or alert | look |
|---|---|
| `working` | amber |
| `waiting` | cyan pulse, labelled "task open, idle" |
| `standby` | dim |
| `cooling` | frost blue, with the reset time |
| `disabled` | grey, with a lock |
| `down` | red |
| `stuck` (alert) | red banner, naming the dialog |

Keys, outside control: `1`–`9` select, `F` focus, `Enter` take control, `P` prompt, `M`
brief the orchestrator, `R` rotate (hold 1 s, after the reply's warning), `O` office, `Esc`
back. While you hold control, every key goes to the agent except `Ctrl+]`, which the panel
intercepts and the tmux config also binds.

Taking control turns the terminal frame amber and shows "YOU HAVE THE KEYBOARD".

**Prompt** types into the session as the operator (fleetd `prompt`). It is instant and
costs nothing; there is no relay hop.

Motion, all bound to real changes:

- a boot sequence that is the real connection log, each source lighting as it answers;
- numbers count to new values;
- events slide in;
- a card flashes on a phase change;
- ticks land as live events arrive;
- banners for `down`, `cooling`, `disabled` and `stuck`.

No sound.

### The 3D office

- One desk per roster agent, laid out from the roster; the orchestrator's desk at the
  head.
- **Desk screens.** Each laptop screen is the agent's terminal, parsed by
  `@xterm/headless` (no renderer, no WebGL context per desk) and painted by our own code
  into a 2D canvas used as a `CanvasTexture`. It is repainted only when the buffer
  changes, at most 30 fps. The orchestrator's screen shows its quota and open tasks.
- **Phases in the scene:**
  - working: amber glow, a typing figure;
  - waiting: pulse;
  - standby: dimmed;
  - cooling: frost;
  - disabled: lamp off, lock;
  - down: screen and lamp off.
- A floating label per desk: name, task, 5 h ring.
- A light runs along the floor from desk to desk for each `fleet.relay` event.
- **Camera.** Slow drift, drag to orbit. Clicking a desk flies in (about 1.2 s), and the
  texture cross-fades to the real DOM terminal (xterm.js, WebGL renderer) in Focus:
  `Enter` takes control, `Ctrl+]` gives it back, and `Esc` flies out.
- **Models and effects.** Everything is modelled in code; there are no model files.
  Bloom renders at half resolution and turns itself off below 40 fps.

## PC2 host (`packaging/console/`)

- **Virtual output.** The connector comes from config and is checked against
  `/sys/class/drm/*/status` before anything is written. A generated EDID goes in
  `/lib/firmware/edid/`, plus an initramfs hook that copies it; the kernel command line
  gets `video=<conn>:1920x1080@60e drm.edid_firmware=<conn>:edid/<file>.bin` (the
  parameter name is checked against the running kernel). This needs a reboot. Before
  rebooting, the script prints how to undo it from GRUB. The previous command line is
  kept, and SSH is unaffected either way. If the output does not come up, the fallback is
  the dummy plug.
- **Display.** Xorg with the `modesetting` driver, and no desktop environment (RustDesk's
  unattended mode needs X11). The kiosk user is in `video` and `render`.
  `fleet-kiosk.service` autologins on tty1, runs `xinit`, and runs Google Chrome (`.deb`;
  Ubuntu ships Chromium only as a snap; Chrome updates through apt, and the panel reports
  its GPU) with `--kiosk` on the console's login URL. It restarts Chrome if Chrome exits,
  and turns DPMS and blanking off.
- **Login.** The kiosk opens `http://127.0.0.1:7880/login?k=<token>`. The console checks
  the token, sets an HttpOnly cookie, and redirects to `/`, so the token is not left in the
  page's URL.
- **RustDesk.** A pinned `.deb`, run as a service, attached to the kiosk's display (`:0`,
  its `XAUTHORITY`). It uses the public rendezvous servers by default; `console.rustdesk.mode:
  direct` switches to direct IP over LAN or Tailscale only. The operator sets the permanent
  password.
- **The install script** is idempotent, prints no secret, and splits its steps into the ones
  that need root and the ones that don't.

## Security

- **The console listens on localhost only**, and every request needs the console token.
  The agents' containers sit on a bridge network and cannot reach the host's loopback; the
  token is defence in depth.
- **Tokens:**
  - **console → fleetd:** the `console` scope. It cannot assign, close, enable or restore.
  - **client → console:** a separate token, generated at install.
  - Both are in `~/.config/fleet-console/` (0600) on the PC2 host. Agent containers mount
    only `~/code`, so they see neither.
- **Holding the keyboard is holding the agent,** but never tmux: the prefix is gone.
- **RustDesk is not SSH.** It is a password, reachable through third-party rendezvous
  servers (end-to-end encrypted). Use a long password, or `direct` mode. Self-hosting the
  rendezvous server is a later option.
- **Found during review, out of this design's scope but on the list:**
  - Every agent's env holds `FLEET_BUILD_KEY`, the build host's private SSH key. An agent
    printed its env while auditing, so the key is in a session transcript.
  - `tell`'s `from` is taken from the request, not bound to the token.

## Error handling

- Any source failing marks only that source, and the fields from it, stale; the rest stays
  live.
- Streamed sources restart with backoff (1 s, doubling to 30 s).
- A terminal shows "reconnecting" over its last frame, dimmed.
- fleet-console exits non-zero on a bad config, naming the key; it never starts half
  configured.
- systemd restarts fleet-console; the kiosk restarts Chrome.

## Testing

- **Go:**
  - each source against fixtures;
  - staleness, including per-agent `at`;
  - patches (a snapshot plus its patches equals the next snapshot);
  - the scope sets, both allowed and refused routes;
  - inbox, `wait` (each wake-up reason), `assign --brief`;
  - every new fleetd route;
  - every new `fleetctl` command against an `httptest` server;
  - the ssh stdio dialer against a fake `ssh` on PATH;
  - terminal watch and control against a real tmux with the image's `tmux.conf`: the window
    keeps 220×50 after a resized client, `Ctrl+]` detaches, the prefix does nothing, one
    controller only.

  They run on the build host (`kbuild`).
- **Panel:** unit tests of the reducer (snapshot + patches, seq gaps, staleness), the key
  map (nothing but `Ctrl+]` is intercepted under control), and the headless-xterm painter.
- **Lead, on PC2:** Playwright against the real console — screenshots of Overview, Focus
  and the office; the office's frame rate in PC2's Chrome; the WebGL renderer is the Intel
  GPU, not SwiftShader.
- **Acceptance (lead and operator):**
  1. From the Mac over RustDesk, the panel shows and moves smoothly.
  2. A real task given with `fleetctl give` is visible in both views while it runs, with
     the current tool live.
  3. Taking control in Focus and typing reaches the session; `Ctrl+]` gives it back.
  4. With fleetd stopped, the fleetd-sourced fields go stale and say so, while terminals and
     live events carry on.
  5. Every value the panel shows is in `fleetctl console snapshot --json`.
  6. `fleetctl wait` returns when the agent reports to the operator.

## Who builds what

**Lead, on Kali, first** (everything else depends on it):

- the scope sets, `console` scope, operator inbox, `wait`, `assign --from/--brief`,
  `fleetctl give`, `whoami`, `healthz`, `GET /v1`;
- the brief's report-to-assigner rule;
- the model contract (`model.go`, `model.ts`).

**Then, in parallel:**

| agent | builds |
|---|---|
| agent-01 | fleet-console: sources, hub and patches, terminals (PTY, lock), auth and login, `/healthz`, the ssh dialer and `fleetctl console …`, `fleetctl attach` through the console |
| agent-02 | the panel: Overview, Focus, keys, motion, the reducer |
| agent-03 | the 3D office, and the headless-xterm desk painter |
| agent-04 | fleetd: every-capability routes (peek, prompt, tasks, activity, today, events cursor, archives, `internal/hostctl` with render/up/down), `Fleet` methods for the ledger reads, `Row` fields, the usage fix, the PreToolUse hook, the status-line interval, `/etc/tmux.conf` in the image, `console:` in `fleet.yaml` and its rendering, `packaging/console/` (the lead runs its root steps) |

**Lead, last:** reviewing and merging each branch to `main`, the PC2 host setup (root and a
reboot), deployment, Playwright, acceptance.

Agents work on `fleet/agent-NN/<topic>` branches and are given work with `fleetctl give`.
Each point of friction in working with them is logged in `docs/superpowers/fleet-friction.md`
and fixed in the fleet tooling as part of this work. The log so far:

1. An agent cannot report to the operator: `tell operator` is "unknown agent".
2. There is no way to wait on an agent; following progress means peeking at its screen.
3. A `tell` takes about 6.5 s and runs a model on the receiver's subscription.
4. `done` shows in `status` only at the next tick.
5. Agents cannot read their own events, usage, history or quota (403), or peek peers.
6. There is no `whoami`, no `healthz` and no route index.

## Out of scope

- Sound.
- Access from anywhere but PC2's screen (and SSH, for the CLI).
- Mobile layout.
- Editing fleet config from the panel.
- Live views of agents on other hosts.
- Per-agent tokens and binding `from` to identity.
- Moving the build key out of the agents' env.
