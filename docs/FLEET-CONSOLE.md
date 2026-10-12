# Fleet console — the operator's guide

The fleet console is a live panel on PC2, the host that runs the agents. It shows every agent at once,
lets you take the keyboard of any agent's Claude Code session, and shows the same fleet as a 3D office.
Everything it shows or does is also available over HTTP and `fleetctl`.

This guide describes what is built. The design is `superpowers/specs/2026-10-11-fleet-console-design.md`; the
fleet itself is `AGENT-FLEET.md`.

- [What it shows, and where the data comes from](#what-it-shows-and-where-the-data-comes-from)
- [Getting in](#getting-in)
- [Keys](#keys)
- [The CLI and HTTP doors](#the-cli-and-http-doors)
- [Install, upgrade, undo on PC2](#install-upgrade-undo-on-pc2)
- [Troubleshooting](#troubleshooting)
- [Working with the fleet as a lead](#working-with-the-fleet-as-a-lead)

## What it shows, and where the data comes from

**Overview** (1920×1080, scaled to fit the window):

- **Header:** console name, host vitals (uptime, load, memory), the clock, the connection state, and the
  browser's WebGL renderer string, so a software fallback is visible.
- **Left — roster:** one card per agent: phase, alerts, idle time, model, context / 5 h / 7 d bars, CPU and
  memory, its last task, and for a working agent the tool it is running and for how long.
- **Centre:** the selected agent's real terminal, scaled to fit.
- **Right:** the orchestrator's quota, the open tasks, today's totals, and the event stream.
- **Bottom:** a 24 h activity track per agent.

**Focus** hides the side columns and gives the terminal the full width. **Office** is the 3D view.

Phases on screen:

| phase or alert | look |
|---|---|
| `working` | amber |
| `waiting` | cyan pulse, "task open, idle" |
| `standby` | dim |
| `cooling` | frost blue, with the reset time |
| `disabled` | grey, with a lock |
| `down` | red |
| `stuck` (alert) | red banner naming it |

### Live and recorded: two clocks

fleetd reconciles every 30 s and is the system of record. A 30 s clock cannot drive a live panel, so the
console observes the agents on its own host directly. When the two disagree, live wins for display.

| live, from the console (seconds) | recorded, from fleetd (30 s) |
|---|---|
| terminal bytes | phase and alerts |
| hook events as they are written (current tool, prompts, stops) | tasks: open, closed, outcome, assigner |
| the status line (model, context %, 5 h / 7 d) | sessions, cost, usage, quota history |
| container CPU, memory, network | archives |
| host load, memory, uptime; git | the orchestrator's quota |

An agent on a different host from the console has recorded data only and says "remote · no live view".
Today all four agents are on PC2.

### Staleness: nothing shows an old value as live

Every source in the snapshot carries `ok`, the time it last succeeded (`at`), and an `error`. The sources and
how often each is refreshed:

| source | what | cadence |
|---|---|---|
| `terminals` | each agent's tmux, read-only | streamed |
| `events` | each agent's hook events, tailed | streamed |
| `statusline` | each agent's `status.json` | 5 s |
| `docker` | container stats | streamed (~1 s) |
| `host` | `/proc` | 2 s |
| `git` | the watched repos | 10 s |
| `fleetd` | status, tasks, events, activity, usage, quota | 5 s |

A source is **stale** when it failed, never answered, or last succeeded at least twice its cadence ago. A
stale source's values are replaced by their age ("6 m ago"), or "no data" / "no record" when it never
answered. The agent card shows how old fleetd's record is instead of the phase when fleetd is stale. The event
stream is one list: the console matches a live event with fleetd's later copy and keeps one.

## Getting in

The console listens on `127.0.0.1` only (any other address in its config is refused) and every request needs
the client token.

**RustDesk to PC2's kiosk (the normal way).** PC2 runs Chrome in kiosk mode on a virtual 1920×1080 display,
and RustDesk shares that display. Connect with RustDesk, using the unattended password you set (below). The
kiosk logs itself in; you do nothing.

**SSH tunnel and `/login` (no RustDesk).** Forward the console's port and log in once with the client token;
the console sets a cookie and redirects, so the token does not stay in the address bar:

```
ssh -L 7880:127.0.0.1:7880 pc2
# on your machine, open:  http://127.0.0.1:7880/login?k=<client token>
```

The token is the file `client.token` in the console's directory on PC2 (mode 0600). Fetch it without printing
it into a log, for example `ssh pc2 cat <dir>/client.token | pbcopy`. For scripts, send it as
`Authorization: Bearer <token>` instead.

**Setting RustDesk's password** is the one step the install script never does. On PC2, once:
`sudo rustdesk --password <a password you choose>`.

## Keys

Outside control:

| key | does |
|---|---|
| `1`–`9` | select that agent |
| `F`, or click the terminal | Focus: the terminal fills the width |
| `Enter` | take control of the selected agent (always in Focus) |
| `P` | prompt bar: type into the selected agent's session as you; `Enter` sends, `Esc` closes |
| `M` | the same, to the orchestrator (a brief) |
| hold `R` for 1 s | rotate the selected agent (below) |
| `O` | switch to the 3D office, and back |
| `Esc` | close the prompt bar; else release control; else leave Focus |

**Control.** `Enter` opens a control connection and the frame turns amber with **YOU HAVE THE KEYBOARD**. From
then on every key goes to the agent — `1`, `R`, `O`, `Esc` included — except **`Ctrl+]`**, which gives the
keyboard back. (The agents' tmux also binds `Ctrl+]` to detach, and has no prefix and no other binding, so a
held keyboard can never run a tmux command.) There is one controller per agent: a second request is refused
with the holder's name, and closing the connection, or reloading the browser, releases it.

**Every key has a button.** Over a remote desktop, `Ctrl+]` or `Esc` may be taken by your own machine and never
reach the page. While you hold the keyboard, TAKE CONTROL becomes **GIVE BACK**. In Focus, **◂ OVERVIEW** leaves
it, giving the keyboard back first. In the office, the desk's **TAKE CONTROL**, **GIVE BACK** and **LEAVE**
buttons do the same. **OFFICE ▸** / **◂ MISSION CONTROL**, bottom right, is the `O` key.

**Prompt** types into the session as the operator through fleetd. It is instant and costs nothing; there is no
relay hop.

**Rotate** archives the agent's session and starts a fresh one. Pressing `R` first asks fleetd what it would
do and shows the answer — "closes task `<id>` as rotated; takes effect at the next idle tick", or "no open
task" — while a ring fills. Keep holding for a second to confirm; releasing early drops it. The same preview
is `fleetctl rotate <agent> --dry-run`.

**Office.** Drag to orbit; the camera also drifts slowly. Click a desk to fly in (about 1.2 s); the agent's
real terminal fades in over its laptop screen. `Enter` takes control (same rules as above), `Ctrl+]` gives it
back, `Esc` or a click on empty floor flies out. Each desk shows the agent's name, task and 5 h ring; its look
follows the phase table; a light runs along the floor from desk to desk for each message between agents (and
to the orchestrator's desk for the operator). The corner shows the measured frame rate; below 40 fps, bloom
turns itself off until the page is reloaded.

## The CLI and HTTP doors

One capability is one function on the server; its HTTP route is a thin adapter, and its `fleetctl` command a
thin client of that route. The panel uses only these public routes.

### The console (on PC2, loopback only)

| HTTP | fleetctl | what |
|---|---|---|
| `GET /login?k=` | — | sets the session cookie, redirects to `/` |
| `GET /healthz` | `fleetctl console health [--json]` | every source and its age (no token needed) |
| `GET /v1/snapshot` | `fleetctl console snapshot [--json]` | agents, tools, load, every source with its age |
| `GET /v1/watch` (WebSocket) | `fleetctl console watch [--json]` | the snapshot, then RFC 6902 patches, one JSON object per line |
| `GET /v1/term/{agent}` (WebSocket) | `fleetctl console term <agent>` | the agent's terminal bytes, raw |
| `GET /v1/term/{agent}?control=1` (WebSocket) | `fleetctl attach <agent>` | the held keyboard: one controller, `Ctrl+]` to give it back |
| `POST /v1/prompt` | `fleetctl prompt <agent> <text…>` | proxied to fleetd's prompt |
| `POST /v1/rotate` | `fleetctl rotate <agent> [--dry-run]` | proxied to fleetd's rotate |

`fleetctl console …` reaches the loopback listener over SSH: it reads the client token from the console's
directory on its host and dials through `ssh <host> -W`, so nothing is opened on the LAN. The host and
directory come from the `console:` section of `fleet.yaml`. To reach a console you have tunnelled yourself, set
`FLEET_CONSOLE_URL` and `FLEET_CONSOLE_TOKEN`. `fleetctl attach` goes through the console when the agent's host
runs one, so the one-controller rule binds it too; otherwise it falls back to `docker exec -it`.

### fleetd (the system of record)

Three token scopes: `full` (you and the orchestrator), `relay` (agents), and `console` (the console, speaking as
the operator: every read, plus `prompt` and `rotate`). A 403 names what the token may do. `fleetctl whoami`
says which scope you hold; `GET /healthz` returns only the version and uptime and needs no token.

The table is the API index, `GET /v1` — what `fleetctl api` prints. A test
(`TestGuideRoutesMatchIndex` in `fleet/internal/server`) fails when this table and the index differ, so it is
the index written out, not a separate list. To refresh it after a route changes: run
`FLEET_PRINT_ROUTES=1 go test ./internal/server -run TestGuideRoutes -v` in `fleet/` and update the rows.

<!-- routes:begin -->
| route | scopes | fleetctl | what |
|---|---|---|---|
| `GET /v1` | full, relay, console | `fleetctl api` | the index of every route and the scopes that may call it |
| `GET /v1/whoami` | full, relay, console | `fleetctl whoami` | which scope and name this token has |
| `GET /v1/status` | full, relay, console | `fleetctl status`, `ls` | one row per agent (phase, task, idle, model, alerts), plus the orchestrator's quota |
| `POST /v1/tell` | full, relay, console | `fleetctl tell` | deliver a message to a session; to `operator` it is stored in the inbox |
| `POST /v1/assign` | full | `fleetctl assign`, `give` | record a task for an agent, and with a brief deliver it; a new task supersedes the agent's old one |
| `POST /v1/done` | full | `fleetctl done` | close a task (`agent` may be left out when `task` is named); archives and rotates the session unless `keep_session` |
| `POST /v1/tasks/{id}/followup` | full | `fleetctl followup` | relay more instructions to the agent on an open task (409 when closed) |
| `POST /v1/tasks/{id}/cancel` | full | `fleetctl cancel` | interrupt the agent, tell it, close the task as `cancelled` (409 when closed) |
| `POST /v1/rotate` | full, console | `fleetctl rotate` | archive the session and start fresh; `dry_run` replies with what it would close and when |
| `POST /v1/enable` | full | `fleetctl enable` | re-enable an agent disabled for a subscription problem |
| `POST /v1/restore` | full | `fleetctl restore` | restore an archived session |
| `GET /v1/inbox` | full, console | `fleetctl inbox` | what agents reported to the operator (`after=<id>` cursor) |
| `GET /v1/wait` | full, console | `fleetctl wait` | long poll: returns when the agent reports, its task closes, or it stops; with `task` (`agent` then optional) it returns the agent's first report after the task's latest instruction (brief, follow-up), to whomever it was sent, unless `after` is set |
| `GET /v1/events` | full, console | `fleetctl events` | an agent's recorded events |
| `GET /v1/usage` | full, console | `fleetctl usage` | tokens and cost, by agent or model |
| `GET /v1/history` | full, console | `fleetctl history` | an agent's past tasks |
| `GET /v1/quota` | full, console | `fleetctl quota` | each subscription's 5 h and 7 d windows |
| `GET /v1/tasks/{id}` | full, console | `fleetctl result` | a task with its conversation (brief, reports, follow-ups, oldest first, each with from/to) and the commits on its branch; 404 unknown |
| `GET /v1/tasks` | full, console | `fleetctl tasks` | tasks, open or all |
| `GET /v1/activity` | full, console | `fleetctl activity` | event counts per agent in time buckets |
| `GET /v1/today` | full, console | `fleetctl today` | tasks closed and spend since local midnight |
| `GET /v1/peek` | full, console | `fleetctl peek` | the last lines of an agent's pane |
| `POST /v1/prompt` | full, console | `fleetctl prompt` | type into an agent's session as the operator |
| `GET /v1/archives` | full, console | `fleetctl archive ls` | list archived sessions |
| `GET /v1/archives/{id...}` | full | `fleetctl archive show` | one archive |
| `DELETE /v1/archives` | full | `fleetctl archive prune` | delete archives older than a cutoff |
| `POST /v1/up` | full | `fleetctl up` | docker compose up for hosts (the plan and `--when-idle` are host-side) |
| `POST /v1/down` | full | `fleetctl down` | docker compose down for hosts |
| `POST /v1/render` | full | `fleetctl render` | regenerate env files, compose files and briefs |
<!-- routes:end -->

## Install, upgrade, undo on PC2

Everything is in `packaging/console/`; its README has the full detail. The short version:

**Install.** As the kiosk user — on PC2 the host's existing operator user, which must be in the `docker`,
`video` and `render` groups:

```
cd packaging/console
cp console.env.example console.env     # connector, mode, user, paths, RustDesk URL and sha256
./install.sh --dry-run                 # prints every command and file; changes nothing
./install.sh                           # sudo asks for the root steps
```

First put the rendered `console.yaml`, the `fleetd.token` (the console-scope token) and the `fleet-console`
binary where `console.env` says; `install.sh` stops before enabling anything if one is missing, naming it. The
steps, in order: `check` (connector, render node, groups), `edid` (a generated EDID for the virtual output and an
initramfs hook), `cmdline` (kernel parameters for the virtual output), `packages` (Xorg, Chrome from its apt
repo, RustDesk's pinned `.deb` with its sha256 checked), `token` (the client token, 0600, never printed),
`units` (the console as a user service, the kiosk as a system service) and `rustdesk` (attached to the kiosk's
display). `RUSTDESK_MODE=direct` sets `direct-server Y` and points RustDesk's rendezvous, relay and API servers at
127.0.0.1 (RustDesk has no off switch, so this is a workaround; `direct-server Y` alone keeps the public
registration). It exposes TCP `RUSTDESK_DIRECT_PORT`, password-protected, and cannot connect by ID. It is unverified
against a live RustDesk 1.4.9: check that `ss -tnp | grep rustdesk` shows no public connection while idle. Xorg's
`Xwrapper.config` allows only the kiosk user (`allowed_users=$KIOSK_USER`). It is safe to run again; `--only <step>` runs one step.

**The reboot.** The virtual output needs kernel parameters, so the display appears after one reboot. SSH is
unaffected either way.

**Upgrade.** Build the panel and the binary (`npm ci && npm run build` in `fleet/console`, then
`go build -ldflags "-X github.com/MelloB1989/karmax/fleet/internal/console.Version=$(git describe --tags --always --dirty)" ./cmd/fleet-console`
in `fleet/`; without the flag it reports `dev`), put the binary at its path on PC2, and restart the user service
(`systemctl --user restart fleet-console`). The panel
is embedded in the binary, so there is nothing else to deploy. The kiosk reloads itself: `/healthz` and every watch
snapshot carry a `build` id (the version plus a hash of the embedded panel), and a panel that sees it change calls
`location.reload()`, waiting if the keyboard is held until it is given back. `sudo systemctl restart fleet-kiosk`
is only needed for a console too old to send `build`.

**Undo.** From a shell (SSH is enough):

```
sudo cp -p /etc/default/grub.fleet-console.bak /etc/default/grub && sudo update-grub && sudo update-initramfs -u
```

The first backup is the original and is never overwritten. If the machine shows nothing after the reboot: at the
GRUB menu press `e`, delete the two added parameters (`video=…` and `…edid_firmware=…`) from the `linux` line,
boot with `Ctrl+x`, then run the line above.

**If the virtual output does not come up.** Some GPU and connector pairs ignore a forced EDID. The fallback is an
HDMI dummy plug in the connector named by `CONNECTOR`; the rest is unchanged.

**Docker after NFS.** PC2 mounts the code directory over NFS. After a reboot, Docker could start before the mount
was real: one container failed to create (Docker never retries that) and others bound the empty mount point and
saw no code. `host-bootstrap.sh pc2` now installs a `docker.service` drop-in
(`docker.service.d/fleet-code-mount.conf`) that waits for the mount unit, derived from the code path with
`systemd-escape`. `host-bootstrap.sh docker-dropin --code <path>` prints it. If a container is dead or sees an
empty code directory after a reboot, restart it once the mount is up.

## Troubleshooting

Start with `fleetctl console health`: it lists every source, whether it is ok, and how old it is.

**The boot log is stuck.** The log at start-up is the real connection log: one line per source, lit when that
source answers, and it leaves once every source has answered or after 6 s. A line stuck on "waiting…" or "no
answer" names the source:

| source | what to check |
|---|---|
| `terminals` | the console user can run `docker exec` (docker group); tmux is running in the agent |
| `events` | `~/.claude/fleet/events.jsonl` exists in the agent; the hooks are installed |
| `statusline` | `status.json` is being written (the status line's refresh) |
| `docker` | `docker stats` streams; newer Docker versions wrap its lines in ANSI codes, which the console strips |
| `host` | `/proc` readable (it nearly always is) |
| `git` | the repos in `console.yaml` exist on PC2 |
| `fleetd` | `fleetd_url` is reachable from PC2 and the console token (`fleetd.token`) is the right one |

**"no record" / ages instead of phases.** fleetd has never reported on that agent, or fleetd is unreachable or
stale. The live columns (tool, context, CPU) keep working because they come from the console's own sources;
the phase and tasks come back on their own when fleetd answers, with no restart.

**A frozen fleetd.** If `fleetctl status` hangs and the cards show ages, fleetd is stuck, not the console. It
once froze when a timed-out `docker` call's `ssh` child kept its pipes open and the call never returned (friction
#22; the fix, a wait delay and killing the process group, was not on main when this was written). If it happens,
restart fleetd. The console needs no restart.

**An agent shows STUCK on the Bypass Permissions dialog.** A restarted Claude Code can stop on that dialog. The
panel raises a red banner for it. The fix is in the image (the settings carry the accepted flag); until a host
has that image, take control (`Enter`), accept the dialog in the terminal, and give the keyboard back (`Ctrl+]`).

**Control is refused: "held by …".** Another viewer or `fleetctl attach` holds that agent's keyboard. There is one
controller per agent; it is released when that client closes or its socket drops.

**The office is slow or has no glow.** The corner shows the real frame rate. Below 40 fps over 2 s, bloom turns
off and stays off until reload. A software GL renderer (the header names it) will do this.

**The panel asks to log in / 401.** The cookie is gone. Open `/login?k=<token>` again (or send the bearer token).

## Working with the fleet as a lead

From the friction log (`superpowers/fleet-friction.md`), the commands that make delegating cheap:

- **`fleetctl give <agent> --task <id> "<brief>"`** records the task and delivers the brief in one command. The
  agent reports to whoever assigned it: to you, `fleetctl tell operator "…"` lands in your inbox. A new task
  supersedes the agent's old open one.
- **`fleetctl wait <agent> [--task <id>] [--timeout 30m]`** blocks until the agent reports, its task closes, or
  it stops. Exit 0 for a message or a closed task; 3 if it went down, cooling or disabled; 4 on timeout. Use it
  instead of peeking at the screen.
- **`fleetctl inbox [--follow]`** is what agents reported to you; `--follow` retries across a fleetd restart.
- **`fleetctl prompt <agent> "<text>"`** types into a session as you: instant, no model run. Use `tell` only to
  reach a session on another host: it runs a short model session in the receiver (about 6 s), with the fleet's
  hooks turned off so it does not count as the agent's work.
- **`fleetctl rotate <agent> --dry-run`** says which task a rotation would close, before you do it.
- **`fleetctl up --dry-run`** (or `--plan`) lists which containers a rollout would recreate and why; **`up
  --when-idle`** recreates each agent alone, once fleetd says it is not working, so a rollout does not restart
  agents mid-task. A resumed session may need a nudge: `fleetctl prompt <agent> "carry on"`.
- **`fleetctl done <agent> --task <id> --keep-session`** closes a finished task without archiving or rotating
  the session, so the agent keeps its context for the next one.
- **Order of a rollout:** ship fleetd changes before the briefs that use them; a brief that names a command the
  running fleetd lacks makes agents fall back to files.
- **Branches:** an agent never rebases or force-pushes a branch it has reported; new work starts from
  `origin/main`.
- **UI work:** agents verify a panel change with `npm run e2e` in `fleet/console` (builds the panel, runs it
  against replayed fixtures in headless Chromium, saves screenshots) before they report it.
