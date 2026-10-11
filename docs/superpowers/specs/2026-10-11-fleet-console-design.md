# Fleet console — design

Status: design, 2026-10-11. Approved in conversation section by section; this document is
the record. Nothing here is built.

## The goal

A control panel on PC2, the host that runs the fleet's agents. The operator RustDesks in
and sees, on one screen, what every agent is doing — in a cinematic, animated "mission
control" style — and can take the keyboard of any agent's Claude Code session from there.
A second view puts the same fleet in a 3D office: one desk per agent, and on each laptop
the agent's live terminal.

Hard rules:

- **Nothing is fake.** Every number, line and animation is driven by a real source. When a
  source is down the screen says so; it never shows stale data as live. There is no demo
  mode in any binary. Tests may use fixtures; development may use a snapshot recorded from
  the real fleet.
- **One function, three doors.** Every capability is one Go function. Its HTTP endpoint and
  its `fleetctl` subcommand are thin adapters over that function, and the panel uses only
  the public HTTP API — no endpoint exists for the panel alone. Whatever the panel can show
  or do, `fleetctl` can show or do, with `--json` for scripts.
- **Config, not source.** Ports, addresses, display mode, token paths and the roster come
  from config. No deployment value is written into the code.

## Decisions

| question | answer |
|---|---|
| Where the screen comes from | A virtual 1920×1080 output on PC2's Intel GPU (kernel-forced connector + EDID file). Fallback: an HDMI dummy plug. |
| Visual direction | "Mission Control": deep navy ground, amber `#FFB547` and cyan `#5CE1E6`, IBM Plex Mono + Barlow Condensed, fine grid, slow scanline. The 3D office uses the same palette and type. Mockups: board A of the "Fleet Console Mockups" canvas. |
| Where the server runs | On PC2, next to the agents, listening on `127.0.0.1` only. |
| Stack | Go server in the fleet module; React + TypeScript panel embedded with `go:embed`; three.js through react-three-fiber; xterm.js for terminals. |

## Architecture

```
Kali                                   PC2
fleetd ──(LAN, "console" token)──▶ fleet-console ──▶ Chrome kiosk ──▶ RustDesk
 ▲                                   │  │  │
 │                                   │  │  └ docker API: tmux attach, stats
fleetctl ──(ssh to pc2, dial 127.0.0.1)┘  └ /proc, shared ~/code (git)
```

### fleet-console (new: `fleet/cmd/fleet-console`, logic in `fleet/internal/console`)

Runs as a systemd service on PC2. Listens on `console.listen` (default `127.0.0.1:7880`).

Sources, each with its own refresh and an `at` / `ok` / `error` in every snapshot:

| source | what | refresh |
|---|---|---|
| fleetd | status rows, orchestrator quota, events, usage, history, quota | 2 s (events incremental by `since`) |
| docker (local socket) | per container: running, CPU %, memory, memory limit, net I/O | 2 s |
| host (`/proc`) | uptime, load 1/5/15, memory, CPU count | 2 s |
| git (shared code) | `main` head, `fleet/*` branches with head and age, commits in 7 days | 10 s |
| terminals | one read-only `tmux attach -r -t main` per agent, raw bytes | streamed |

A source is **stale** when its last success is older than twice its refresh; the snapshot
carries that and the panel shows the age in place of the value ("fleetd unreachable · 40 s").

### The public API (served by fleet-console; same JSON as the CLI's `--json`)

| HTTP | fleetctl | function |
|---|---|---|
| `GET /v1/snapshot` | `fleetctl console snapshot [--json]` | `Snapshot(ctx)` |
| `GET /v1/watch` (WebSocket, or NDJSON when not upgraded) | `fleetctl console watch [--json]` | `Watch(ctx)`: snapshot, then patches |
| `GET /v1/term/{agent}` (WebSocket, binary) | `fleetctl console term <agent>` | `Terminal(ctx, agent, watch)` |
| `GET /v1/term/{agent}?control=1` | `fleetctl attach <agent>` (existing, unchanged) | `Terminal(ctx, agent, control)` |
| `POST /v1/act` `{action, agent, text}` | `fleetctl tell` / `rotate` (existing) | proxied to fleetd's own endpoint |

`fleetctl console …` reaches the localhost-only listener through the SSH connection it
already uses for docker on that host (`ssh <host> -W 127.0.0.1:<port>`), so nothing is opened
on the LAN.

**Snapshot shape** (the contract; the Go and TypeScript types are written first, in
`fleet/internal/console/model.go` and `fleet/console/src/model.ts`):

- `at`, `sources{fleetd,docker,host,git}` → `{ok, at, error}`
- `host` → `{name, uptime_s, load[3], mem_used, mem_total, cpus}`
- `orchestrator` → `{name, five_hour_pct, seven_day_pct, five_hour_resets, seven_day_resets}`
- `agents[]` → `{id, host, state, task, idle_s, model, models[], session, context_pct,
  five_hour_pct, seven_day_pct, five_hour_resets, current_tool{name, since}|null,
  container{running, cpu_pct, mem, mem_limit, net_rx, net_tx}, last_task{id, outcome,
  ended, cost_usd}|null, term{cols:220, rows:50}}`
- `events[]` → the last 200 `{at, agent, kind, detail}`
- `today` → `{tasks_closed, spend_usd, output_tokens}`
- `git` → `{main{sha, subject, at}, branches[{name, agent, sha, subject, at}], commits_7d}`
- `activity` → per agent, the event times of the last 24 h

`watch` sends `{"type":"snapshot","seq":n,…}` once, then `{"type":"patch","seq":n,"ops":[…]}`
(RFC 6902) whenever anything changes. A client that sees a gap in `seq` asks again.

**Terminals.** Watch mode is a read-only tmux client; control mode a read-write one. One
controller per agent at a time: a second `control=1` is refused with 409 and names who
holds it. Closing the socket releases control. Every agent's tmux window is pinned at
220×50 (`window-size manual`), so no client — the console, `fleetctl attach`, anyone —
resizes the agent's real screen.

### fleetd changes

- A third token scope, **console**: every read endpoint, plus `tell` (allowed to speak as
  `operator`) and `rotate`. Not `assign`, `done`, `enable` or `restore`. Accepted on the LAN
  listener. Its env var is named in `fleet.yaml` (`console_token_env`).
- **Usage is recorded.** `GET /v1/usage` returns nothing today; find why the OTLP or
  transcript path doesn't land rows, and fix it.
- **Context agrees with the agent.** `status` reports a context % that differs from the
  agent's own status line; use the status line's value.
- **A PreToolUse hook event**, so "the tool running now" and its start time are known.
- **Operator inbox.** Messages an agent sends to `operator` are stored. `GET /v1/inbox`
  + `fleetctl inbox [--since]`; `GET /v1/wait?agent=&task=` (long poll) + `fleetctl wait
  <agent> [--task <id>] [--timeout]`, which returns when the task is closed or the agent
  messages the operator. The agents' brief says: report to whoever assigned the task
  (`fleet.assign` records the assigner); when that is the operator, the report goes to the
  inbox instead of the orchestrator.

### agent-run change

`tmux set -g window-size manual` and `default-size 220x50` before the session starts.

### The panel (`fleet/console/`, built to `fleet/internal/console/dist`, embedded)

Layout at 1920×1080, scaled to fit the window:

- Header: name, PC2 vitals, clock.
- Left: agent roster — state, idle, model, session, context / 5 h / 7 d bars, CPU and
  memory, last task; a working agent shows its current tool and how long it has run.
- Centre: the selected agent's live terminal, with Take control, Tell and Rotate.
- Right: the orchestrator's quota rings and state, today's totals, the event stream.
- Bottom: a 24 h activity track per agent, one tick per event.

Keys: `1`–`9` select, `Enter` take control, `Ctrl+]` give it back (`Esc` belongs to Claude
Code), `T` tell, `M` brief the orchestrator, `R` rotate (hold 1 s), `O` toggle the office.
All of it is clickable as well.

Taking control turns the terminal frame amber and shows "YOU HAVE THE KEYBOARD".

Motion, all bound to real changes:

- a boot sequence that is the real connection log, each source lighting as it answers;
- numbers count to new values;
- events slide in;
- a card flashes on a state change;
- timeline ticks land as events arrive;
- a red banner when an agent is down, cooling, failed or stuck on a dialog.

No sound.

### The 3D office

- One desk per roster agent, laid out from the roster; the orchestrator's desk at the head.
- Each laptop screen is that agent's terminal: an xterm rendered to a canvas, used as a
  texture, redrawn only on change, at most 30 fps. The orchestrator's screen shows its
  quota and open work.
- State shows in the scene:
  - working: amber screen glow, typing figure;
  - waiting on the operator: pulse;
  - idle: dimmed;
  - cooling: frost blue;
  - down: screen and lamp off.
- A floating label per desk: name, task, 5 h ring.
- A light runs along the floor from desk to desk for each `fleet.relay` message.
- Camera: slow drift, drag to orbit. Clicking a desk flies in (about 1.2 s), and the
  texture cross-fades to the real DOM terminal: `Enter` takes control, `Ctrl+]` flies back.
- Everything is modelled in code; no model files.
- Bloom renders at half resolution and turns itself off below 40 fps.

## PC2 host (`packaging/console/`)

- **Virtual output:** kernel `video=HDMI-A-1:1920x1080@60e drm.edid_firmware=HDMI-A-1:edid/<file>.bin`,
  with a generated EDID in `/lib/firmware/edid` and an initramfs rebuild. The connector
  and mode come from config.
- **Display:** Xorg, no desktop environment (RustDesk's unattended mode needs X11). A
  systemd service autologins the kiosk user, starts `xinit`, and runs Google Chrome
  (`.deb`; Ubuntu ships Chromium only as a snap) with `--kiosk --app=<console URL>`,
  restarting it if it exits. DPMS and blanking are off.
- **RustDesk:** a pinned `.deb`, run as a service, on the public servers; the operator sets
  the permanent password.
- **Install script:** idempotent; it never prints a secret. fleet-console runs as a systemd
  service, and its config is at `~/.config/fleet-console/config.yaml`.
- **Token:** in `~/.config/fleet-console/token` (0600), on the host, outside every container.

## Security

- The console listens on localhost only; the way in is RustDesk (its password) or SSH.
  Whoever is through either can drive the agents — the same as SSH today.
- The console token cannot assign, close, enable or restore.
- Agent containers mount neither the console's config nor its token.

## Error handling

- Any source failing marks only that source stale; the rest stays live.
- A terminal stream reconnects with backoff and shows "reconnecting" over the last frame,
  dimmed.
- fleet-console exits non-zero on a bad config, naming the key; it never starts half
  configured.
- The kiosk restarts Chrome; systemd restarts fleet-console.

## Testing

- Go:
  - each source against fixtures;
  - staleness;
  - patch generation (a snapshot plus its patches equals the next snapshot);
  - the console scope's allowed and refused routes;
  - the operator inbox and `wait`;
  - terminal watch and control against a real tmux, including the one-controller rule;
  - every CLI subcommand against an `httptest` server.
- Panel: unit tests of the state reducer (snapshot + patches, seq gaps, staleness display).
- Playwright against the real console on PC2: screenshots of both views, and a frame-rate
  sample of the office in PC2's Chrome.
- Acceptance:
  1. From the Mac over RustDesk, the panel shows.
  2. A real task given to an agent is visible in both views while it runs.
  3. Taking control and typing reaches the session.
  4. With fleetd stopped, the panel says fleetd is unreachable while terminals stay live.
  5. Every panel value can also be fetched with `fleetctl console snapshot --json`.

## Who builds what

- **Lead (on Kali):**
  - operator inbox, `wait`, the console scope, the brief change;
  - the model contract (`model.go`, `model.ts`);
  - integration, the PC2 host setup (needs sudo there), deployment.
- **agent-01:** fleet-console — sources, snapshot/watch, terminals, actions, `fleetctl console`.
- **agent-02:** the panel.
- **agent-03:** the 3D office.
- **agent-04:**
  - the usage and context fixes, the PreToolUse event;
  - the agent-run pin;
  - `packaging/console/` (the lead runs its sudo steps).

Agents work on `fleet/agent-NN/<topic>` branches; the lead reviews and merges to `main`.
Each point of friction in working with the agents is logged and fixed in the fleet tooling
as part of this work.

## Out of scope

- Sound.
- Access from anywhere but PC2's own screen (and SSH, for the CLI).
- Mobile layout.
- Editing fleet config from the panel.
