# fleetctl TUI, ssh and device access: implementation plan

> **For agentic workers:** each task below is one fleet agent's brief. Work test-first, on your own
> branch `fleet/<agent>/<task-slug>` from `main` in the shared checkout (it has the contract), in your own worktree. Commit often. Report the
> branch when done. Steps use checkbox (`- [ ]`) syntax.

**Goal:** a branded TUI in fleetctl, `fleetctl ssh` into any agent session from anywhere, and access by
tagged Tailscale device (admin or guest), with lendable sandbox agents.

**Architecture:** fleetd stays the hub. New fleetd routes (access, watch, screens, term) check the caller
first; fleetctl and its TUI are clients. The wire contract is `fleet/internal/fleetapi` (already on
main); the TUI reads fleetd only through `fleetapi.Client` and is built against `fleetapi.Fake`.

**Tech stack:** Go 1.26; charm.land/bubbletea/v2 v2.1.0, charm.land/lipgloss/v2 v2.0.6,
charm.land/bubbles/v2 v2.2.1 (already in go.mod); coder/websocket and creack/pty (already deps);
SQLite ledger; Docker compose rendering.

**Spec:** `docs/superpowers/specs/2026-10-25-fleetctl-tui-access-design.md`. Read it first; this plan
argues from it.

## Global constraints

- The repo is public, with a PII pre-commit hook. Tests and docs use fake logins
  (`friend@example.com`), fake tailnets (`tail1234.ts.net`), fake IPs and fake home paths. Never
  `--no-verify`.
- Commit messages end with `Co-Authored by MelloB's coding agent <build@mellob.in>`.
- Never print env values or secrets; print key names only. Never put a token in a test fixture that
  looks real.
- Do not touch `internal/voice`, `internal/runtime/voice*` or `karmax.prev` (the operator's
  uncommitted work in the shared checkout). Work only in your worktree.
- Every new route is in its own file under `fleet/internal/server/`. Edit `server.go` only to register
  it, plus whatever your task names explicitly. New `Ops` methods go in a small interface in your file,
  embedded in `Ops` with one line.
- Ask who may do something through `isAdmin`, `mayUseAgent`, `lentAgents` and `me` in
  `server/allowed.go`. Task 1 gives them their final rules; nobody else changes their bodies.
- `fleet/internal/fleetapi` is the contract. Do not change existing fields or names. If your task
  truly needs a new field, add it, say so in your report, and keep the fake compiling.
- `fleet/internal/tui/widgets.go`: add functions at the end, never change existing signatures.
- Each smoke addition goes in its own clearly marked section at the end of
  `fleet/scripts/smoke-v2.sh`.
- `go vet ./...`, `go test -race ./...` and `fleet/scripts/smoke-v2.sh` pass before you report.
- Tests read like the existing ones: a sentence per test name, fakes over mocks, no sleeps where a
  channel will do.
- PC2 agents run their builds on Kali's docker (`DOCKER_HOST` is set). Never run `fleetctl image build`
  or restart fleetd, and never touch other agents' containers or the live fleet.yaml.

## Review focus

1. **A guest reaching beyond its lends through a route nobody listed.** Every route is either
   admin-only or checks `mayUseAgent`/own task. Task 1's table test walks the router's own route list
   (`GET /v1` index), so a route added later without a rule fails it.
2. **A tag stolen by renaming a device.** The same name with another StableID is denied (task 1).
3. **A sandbox container still holding something of the operator's**: an env key, a mount, the shared
   PID namespace, `DOCKER_HOST`, the relay token. Render tests assert the exact env key set and mount
   set of a sandbox service (task 2).
4. **A watcher changing the agent's screen**: resizing it, or typing through a watch session (task 3).
5. **A tiny or odd terminal crashing the TUI**: 20×5, 0×0 before the first size message, emoji and wide
   runes in task names, a nil snapshot, an empty fleet (tasks 5 and 6 test each).

---

### Task 0: Contract and skeleton (done by the operator's session, on main)

- `fleet/internal/fleetapi`: `fleetapi.go` (Snapshot, Worker, Activity, Task, Event, Account, Host,
  Container, Fleetd, Screen, TermFrame, Device, Knock, AccessList, AccessChange, routes), `client.go`
  (the `Client` interface, TaskDetail, Message, RunRequest, RunReply), and `fake.go` (`NewFake(role,
  now)`, `Step()`). Tests pin the contract's JSON names to `daemon.Row`, `ledger.Task`, `ledger.Relay`
  and `daemon.TaskResult`.
- `fleet/internal/tui`: `tui.go` (Options, Ctx, Tab, messages), `theme.go`, `widgets.go` (Bar,
  Spark, Age, Since, Bytes, Fit), `sprite.go` (placeholder `Sprite`).
- `fleet/internal/server/allowed.go`: `isAdmin`, `mayUseAgent`, `lentAgents`, `me`.
- `config.Agent.Sandbox` and `config.Worker.Sandbox` (the field only; no behavior yet).

---

### Task 1: Device access (agent-01)

**Files:**
- Modify: `fleet/internal/tailnet/tailnet.go`. `Who` gains `StableID string` from whois `Node.StableID`.
- Create: `fleet/internal/access/access.go` (+ `_test.go`). Resolution, the store's API, short-name
  lookup.
- Create: `fleet/internal/ledger/access.go` (+ test). The `devices`, `device_agents` and `knocks`
  tables.
- Modify: `fleet/internal/server/tailnet.go`. Resolve the device: admin → full scope; guest → guest
  scope; unknown → knock + 403.
- Modify: `fleet/internal/server/server.go`. Rename `member` scope to `guest`. `own`, `holds` and
  `memberFrom` keep working for guests. Add the lend checks listed in the spec's "Enforcement".
- Modify: `fleet/internal/server/allowed.go`. The final bodies.
- Create: `fleet/internal/server/access.go`. `GET/POST /v1/access`, admin-only.
- Modify: `fleet/internal/config/config.go`. Drop `Operators`, `Members` and `MemberOpen`, add
  `GuestOpen` (default 2). Loading a file that still has `operators`, `members` or `member_open`
  fails with "tailnet.operators/members are gone: tag devices with `fleetctl access`".
- Modify: the `operatorWork` path. Add a `tasks.admin_work` column (`ALTER TABLE … ADD COLUMN`, as the
  ledger's other migrations do), set when a task is assigned with the full scope. `operatorWork`
  requires it, and also refuses any worker whose config agent has `Sandbox`.
- Create: `fleet/cmd/fleetctl/access.go` (+ test). `fleetctl access [admin|lend|unlend|revoke]`, with a
  table output and the Claude-login caveat printed after `lend`.
- Modify: the knock notifier. The first knock of a device in 24 h calls the daemon's Notifier once.
- Modify: `docs/AGENT-FLEET.md`, the "From anyone's machine" section: devices, admin and guest, knocks.

**Interfaces:**
- Consumes: `fleetapi.Device`, `Knock`, `AccessList`, `AccessChange`, `Access*` ops; `tailnet.Who`.
- Produces:
  ```go
  package access
  type Store interface { // the ledger implements it
      Device(stableID string) (fleetapi.Device, bool, error)
      Devices() ([]fleetapi.Device, error)
      Knock(w tailnet.Who, now time.Time) (first bool, err error) // first knock in 24 h
      Knocks(limit int) ([]fleetapi.Knock, error)
      Tag(d fleetapi.Device) error          // replaces role and agents
      Untag(stableID string) error
      Seen(stableID string, now time.Time) error
  }
  // Resolve is who a tailnet caller is: role "" means refused.
  func Resolve(s Store, w tailnet.Who) (fleetapi.Device, error)
  // Change applies an AccessChange made by `by`; sandbox(agent) says whether an agent may be lent.
  func Change(s Store, c fleetapi.AccessChange, by string, sandbox func(string) bool, now time.Time) (fleetapi.AccessList, error)
  // Find is a device by full name, unique short name, or StableID, among tagged devices and knocks.
  func Find(s Store, ref string) (stableID, name, login string, err error)
  ```
  `server.Caller` gains `Device, StableID string` and `Lent []string`.

- [ ] **Step 1: Failing tests for `access`.**
  - `TestAnUntaggedDeviceIsRefusedAndKnocks`
  - `TestATagFollowsTheStableIDNotTheName`: same name, other StableID → refused.
  - `TestOnlySandboxAgentsCanBeLent`
  - `TestAdminOnAGuestDropsItsLends`
  - `TestLendOnAnAdminIsRefused`
  - `TestAShortNameMustMatchOneDevice`
  - `TestTheFirstKnockInADayIsTheOnlyOneTold`
- [ ] **Step 2: Implement `access` and `ledger/access.go` until they pass.** Commit.
- [ ] **Step 3: Failing server tests**, through the real mux with a fake whois.
  - `TestEveryRouteHasARuleForAGuest`: walk `GET /v1`'s index. For each route, a guest with no lends
    gets 403 or 404, except the guest-allowed list (whoami, guide, run, pick, wait, tasks,
    tasks/{id}, followup, cancel, answer, done, peek, files, rsh, watch, screens, term, events,
    status, quota, usage, activity, today), which must filter or check.
  - `TestAGuestRunsOnlyOnItsLentAgents`: an explicit agent outside its lends → 403; pick chooses
    only among its lends.
  - `TestAGuestSeesOnlyItsLentAgentsAndOwnTasks`: covers status, tasks and events.
  - `TestAnAdminDeviceIsTheFullScope`
  - `TestALoginAloneIsNotAdmin`: vector-style login on an untagged device → refused.
  - `TestTheOldTailnetKeysFailToLoad`
  - `TestOperatorWorkNeedsAnAdminAssignedTaskOnAnOrdinaryAgent`: a guest task → refused; a sandbox
    worker with an admin task → refused; the full token's task → allowed.
- [ ] **Step 4: Implement until they pass.** Mutation-check the decision points: flip each `mayUseAgent`
  branch, the StableID comparison and the admin_work check, and see a test fail for each. Commit.
- [ ] **Step 5: `fleetctl access` CLI** with tests (an httptest fleetd; output columns DEVICE, LOGIN,
  ROLE, AGENTS, SEEN; knocks below). Commit.
- [ ] **Step 6: smoke-v2 section "access".**
  - The fake tailscaled answers whois for two devices.
  - Tag one admin, lend agent-03 to the other as a sandbox (the fake config marks it).
  - The guest runs a task and is refused another agent's status and memory.
  - An untagged device is refused, and its knock is listed.
- [ ] **Step 7: Docs, vet, race, smoke. Report.**

---

### Task 2: Sandbox agents (agent-04)

**Files:**
- Modify: `fleet/internal/render/render.go` (+ test). For `Sandbox` agents:
  - no code bind mount; a `<name>-code` volume at `/work/code`, and `FLEET_CODE=/work/code`;
  - none of `fleet-peers`, `fleet-socks` or the hidden tmpfs;
  - no `pid: service:fleet-anchor`; set `init: true`;
  - no `DOCKER_HOST`, `FLEET_BUILD_IMAGE`, `KARMAX_API_URL`, GitHub env, `GIT_*` identity, or SSH
    aliases;
  - network `fleet-sandbox` (a bridge with `sandbox.subnet`, default `172.31.250.0/24`), plus
    `extra_hosts` for host.docker.internal;
  - in the env file: the Claude token, `FLEET_SANDBOX_KEY`, and the OTLP header carrying the sandbox
    key. No relay token, no `FLEET_AGENT_KEY`, no GH tokens.
  - Sandbox defaults: `slots: 1`, `memory: 1536m` when unset.
- Create: `fleet/internal/identity/sandbox.go`: `SandboxKey(full, agent string) string` =
  hex(HMAC-SHA256(full, "sandbox:"+agent)).
- Create: `fleet/internal/server/sandbox.go`. `Tokens` learns sandbox keys: a bearer equal to
  `SandboxKey(full, a)` for a configured sandbox agent `a` is scope `sandbox`, whose verified name is
  `a`. Allowed:
  - `GET /v1/whoami`;
  - `POST /v1/tell`, only to its current task's assigner;
  - `POST /v1/ask`, with the same recipient rule;
  - `GET /v1/tasks/{its own task}`;
  - `GET /v1/wait` on its task.
  - Everything else is 403.
- Modify: `fleet/internal/otlp/otlp.go`. Accept a sandbox key as well as the relay token, and attribute
  telemetry to the key's agent, ignoring the `fleet.agent` resource attribute for that request.
- Modify: question escalation (daemon). A question on a task assigned by a guest login never escalates
  to the orchestrator; after `question_escalate` it is re-told once to the guest.
- Modify: the daemon pick. Sandbox agents are skipped for any run whose caller is not a guest with that
  agent lent, unless the admin names the agent with `--agent`.
- Create: `fleet/internal/render/sandbox-net.sh.tmpl` and a systemd unit
  `fleet-sandbox-net.service.tmpl`. Render writes both beside the host's compose file. Rules, as
  iptables with comments tagged `fleet-sandbox`, idempotent (flush own chain, re-add):
  - INPUT from the subnet: allow TCP to fleetd's relay and OTLP ports when this host runs fleetd, drop
    the rest;
  - FORWARD (`DOCKER-USER`) from the subnet: allow TCP to fleetd's address on those ports; drop
    10/8, 172.16/12, 192.168/16, 100.64/10 and 169.254/16; allow the rest.
- Create: the sandbox probe in daemon. Hourly, per sandbox container, run (via the agent exec) a shell
  check:
  - a LAN address (fleetd's host LAN IP on port 22) must refuse or time out within 3 s;
  - fleetd's relay URL `/healthz` must answer.
  - On failure, alert once and mark the agent disabled with reason `sandbox network unguarded`; clear
    it when the probe passes.
- Modify: `fleet/internal/render/brief.md.tmpl` and the fleet-agent skill. A sandbox variant of the
  "What you have" section, per the spec's "What it is told".
- Modify: `fleet/internal/guide/guide.md`. A short "Guests and sandboxes" section for the guest side:
  `fleetctl sync ./repo agent-09:/work/code/repo`, and fetching results back. Keep the 250-line cap.
- Modify: `docs/AGENT-FLEET.md`. Sandbox agents and installing the network unit (sudo, once per host).

**Interfaces:**
- Consumes: `config.Agent.Sandbox`, `config.Worker.Sandbox`.
- Produces: `identity.SandboxKey`; a server scope `sandbox`; `render` output paths
  `render/sandbox-net.<host>.sh` and `render/fleet-sandbox-net.<host>.service`.

- [ ] **Step 1: Failing render tests.**
  - `TestASandboxServiceHasExactlyItsOwnMounts`: the volumes list equals `[<n>-home:…/.claude,
    <n>-work:/work, <n>-code:/work/code]`, with no pid key and init true.
  - `TestASandboxEnvHoldsOnlyItsKeys`: the env file's key set equals the allowed set exactly, and
    `FLEET_TOKEN`, `FLEET_AGENT_KEY`, `GH_TOKEN*` and `DOCKER_HOST` are absent.
  - `TestASandboxIsOnTheSandboxNetwork`
  - `TestOrdinaryAgentsAreUnchanged`: a golden comparison of a non-sandbox service before and after.
  - `TestTheNetworkRulesNameFleetdsPorts`
- [ ] **Step 2: Implement render.** Commit.
- [ ] **Step 3: Failing server and otlp tests.**
  - `TestASandboxKeyIsOnlyItsOwnScope`: walk the route index; only the allowed list works.
  - `TestASandboxTellsOnlyItsAssigner`
  - `TestASandboxKeyOfAnotherAgentIsRefused`
  - `TestTelemetryWithASandboxKeyIsTheKeysAgents`: a spoofed `fleet.agent` attribute is ignored.
  - `TestAGuestsQuestionDoesNotEscalateToTheOrchestrator`
  - `TestAPickSkipsSandboxesForAdminsUnlessNamed`
- [ ] **Step 4: Implement until they pass. Mutation-check the scope switch and the recipient rule.**
  Commit.
- [ ] **Step 5: The probe**, with a fake box whose exec answers pass or fail:
  - `TestAnUnguardedSandboxIsDisabledAndToldOnce`
  - `TestAGuardedSandboxComesBack`
  
  Commit.
- [ ] **Step 6: Brief, skill, guide and docs.** `go test ./internal/guide ./internal/render` (the cap
  test). Commit.
- [ ] **Step 7: smoke-v2 section "sandbox".**
  - Render a sandbox and grep the compose file and env file for absent keys.
  - The fake fleetd accepts the sandbox key for whoami and refuses it for status.
- [ ] **Step 8: Vet, race, smoke. Report.**

---

### Task 3: Terminals, screens and `fleetctl ssh` (agent-05)

**Files:**
- Create: `fleet/internal/term/term.go` (+ test):
  - `Session` starts a command under a pty (creack/pty) and pumps bytes to and from a websocket
    (coder/websocket).
  - `Locks` is the one-typist-per-worker rule.
- Create: `fleet/internal/server/term.go`: `GET /v1/term/{worker}?mode=&cols=&rows=`.
  - Check `mayUseAgent(r, worker)` before the upgrade.
  - Commands per the spec's table, using the worker's host exec prefix and `main-<slot>`.
  - watch uses `tmux attach -t main-<slot> -f read-only,ignore-size`.
  - type takes the lock: a held lock sends a `held` frame and closes; `force=1` (admins only) sends
    `released` to the holder.
  - On a type session's end, restore the window size with `tmux resize-window -t main-<slot> -x
    <cols> -y <rows>` from the agent's configured size, if it has one (otherwise
    `set-option -u window-size`).
  - Record `fleet.term` events at start and end.
  - Idle watchers close after 12 h.
- Create: `fleet/internal/server/screens.go`: `GET /v1/screens?workers=&lines=`.
  - Filter by `mayUseAgent`.
  - One exec per container covering all its requested slots (`tmux capture-pane -p -e -t main-<k>
    -S -<lines>` for each, separated by a marker line).
  - A 2 s cache shared by all callers.
  - At most two captures in flight per host (a semaphore).
- Create: `fleet/cmd/fleetctl/ssh.go` (+ test): `fleetctl ssh <worker> [--type] [--shell] [--force]`.
  - Dial `fleetapi.PathTerm` on the configured server (tailnet or loopback with token).
  - Raw mode; SIGWINCH sends resize frames (on Windows, poll the size each 250 ms).
  - `Ctrl-]` detaches.
  - On exit, print `[detached from agent-03:2]`, or the error frame's text.
  - Export `func AttachCommand(worker string, mode fleetapi.TermMode) tea.ExecCommand` (Run, SetStdin,
    SetStdout, SetStderr), so the TUI can suspend into it.
- Modify: `fleet/cmd/fleetctl/main.go`. `attach <worker>` = `ssh --type`. The console path and
  `--direct` are removed from `attach`; `fleetctl console attach` keeps the console's own terminal.
- Modify: `fleet/internal/guide/guide.md` (a line on `ssh`) and `docs/AGENT-FLEET.md`.

**Interfaces:**
- Consumes: `fleetapi.TermFrame`, `TermMode`, `Frame*`, `Screen`, `ScreensReply`, `PathTerm`,
  `PathScreens`; `mayUseAgent`, `isAdmin`; the agent exec prefix from config.
- Produces: `cmd/fleetctl.AttachCommand` (package main; the TUI gets it through `tui.Options.Attach`);
  the two routes.

- [ ] **Step 1: Failing `term` tests**, with a fake exec that runs `/bin/sh -c 'stty size; cat'` under
  the pty.
  - `TestWatchIgnoresInput`: the watch command gets no stdin; assert the argv has `-f
    read-only,ignore-size`.
  - `TestTypeEchoes`
  - `TestASecondTypistIsHeld`
  - `TestAForcedTakeoverReleasesTheHolder`
  - `TestResizeReachesThePty`
  - `TestTheSessionEndsWhenTheProcessExits`: an exit frame.
  - `TestTheSessionEndsWhenTheSocketCloses`: no leaked process (check with a pid file).
- [ ] **Step 2: Implement `term`.** Commit.
- [ ] **Step 3: Failing server tests.**
  - `TestTermRefusesAWorkerTheCallerMayNotUse`, before the upgrade.
  - `TestTermBuildsTheRightCommandPerMode`: for each mode and host, assert the argv, including
    `--context pc2` and `main-2` for slot 2.
  - `TestScreensTakeOneExecPerContainer`: four workers on two containers → two execs.
  - `TestScreensAreSharedForTwoSeconds`
  - `TestScreensFilterWhatTheCallerMayUse`
- [ ] **Step 4: Implement the routes.** Commit.
- [ ] **Step 5: `fleetctl ssh`** with tests against an httptest websocket server.
  - `TestSSHSendsResizeFirst`
  - `TestCtrlBracketDetaches`
  - `TestAHeldFrameExitsWithTheHolder`
  - Windows: the build passes with `GOOS=windows go build ./cmd/fleetctl`.
  
  Commit.
- [ ] **Step 6: smoke-v2 section "term".**
  - The fake docker's `exec -it` runs a tiny script that prints the slot and echoes.
  - `fleetctl ssh agent-03:2 --type` through fleetd, with `script(1)` for the pty, sees its echo.
  - A watch session never sees typed input reach the script.
- [ ] **Step 7: Vet, race, smoke. Report.**

---

### Task 4: The watch stream (agent-06)

**Files:**
- Create: `fleet/internal/daemon/activity.go` (+ test):
  ```go
  // Activity is what a worker is doing from its state and its latest tool
  // event (kind "otel.tool_result" or "otel.tool_decision", detail has tool_name
  // and, for Bash, the command if the event carries it).
  func Activity(row Row, last ToolEvent, now time.Time) fleetapi.Activity
  type ToolEvent struct { Tool, Command string; At time.Time }
  ```
  The mapping follows the `fleetapi.Act*` comments. The test-command patterns are `go test`,
  `npm test`, `pnpm test`, `pytest`, `cargo test`, `make test`, and `vitest`. Pushing is
  `git push`. A working worker with no event in 20 s is thinking.
- Modify: `fleet/internal/otlp/otlp.go`. Also record `tool_decision`, and keep `tool_name` plus a
  Bash command's first 120 bytes in the detail. Never record tool input other than that. Tell the
  agent on task 2 if you touch the auth lines; better, do not touch them.
- Create: `fleet/internal/daemon/snapshot.go` (+ test). `func (f *Fleet) Snapshot(now time.Time, all
  bool, agents []string, me fleetapi.Me) *fleetapi.Snapshot` builds the snapshot from:
  - the status rows (with `Container` and `Sandbox` from config);
  - tasks: open, plus those closed in the last 24 h, with thread count and last message;
  - the last 200 events;
  - quota accounts with history, a least-squares burn over the last hour, and run-out;
  - today;
  - hosts;
  - fleetd: version, start, last tick duration and errors.
  
  Scoping: when `!all`, keep only workers whose agent is in `agents`, tasks on them assigned by
  `me.Name`, events of those agents and tasks, and quota of their accounts; host samples are cut to
  their containers. Knocks is set by the server for admins.
- Create: host samples. Once a minute per host, one exec into any running agent container on that
  host: `cat /proc/loadavg; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo; nproc; df -Pk <code
  dir or /work>`. Keep 60 samples in memory, per host and per container (from the existing docker
  stats reader).
- Modify: the tick. Measure its duration and errors for `Fleetd`, and bump the snapshot seq when the
  tick ends and when an event is recorded, at most once a second (a `Notify` channel the watch route
  waits on).
- Create: `fleet/internal/server/watch.go`: `GET /v1/watch?since=`.
  - A long poll of at most 25 s, ending with 503 on drain like the others.
  - `Knocks` for admins: one call through a small interface the access task implements; until then
    it returns 0. Name it `KnockCount() int` on Ops, and implement it as 0 in the daemon.
- Modify: `fleet/cmd/fleetd/main.go`. Pass the version and start time.

**Interfaces:**
- Consumes: `fleetapi.*`; `lentAgents`, `me`, `isAdmin`; ledger events, tasks, relays, quota and
  usage.
- Produces: `Fleet.Snapshot`, `Fleet.SnapshotSeq() uint64`, `Fleet.Changed() <-chan struct{}`, and
  the route.

- [ ] **Step 1: Failing `Activity` tests**, table-driven over each kind, including stale-tool →
  thinking and a git push inside `&&`. Implement. Commit.
- [ ] **Step 2: Failing snapshot tests.**
  - `TestASnapshotForAGuestHoldsOnlyItsAgentsAndTasks`: workers, tasks, events, quota, hosts.
  - `TestBurnAndRunOut`: from 40 % with a reset in 2 h, a steady 10 %/h never reaches 100 before the
    reset (RunOut zero); a steady 40 %/h reaches it at 1 h 30 m (RunOut = now + 90 m, ± a minute).
  - `TestTheTaskListHasItsLastMessage`
  - `TestHostSamplesKeepSixty`
- [ ] **Step 3: Implement.** Commit.
- [ ] **Step 4: Failing watch-route tests.**
  - `TestWatchAnswersAtOnceWhenBehind`
  - `TestWatchWaitsForAChange`
  - `TestWatchEndsOnDrain`
  - `TestWatchIsScopedToTheCaller`, using `lentAgents` via a guest-scoped request stub (as the
    existing member tests do).
- [ ] **Step 5: Implement. Mutation-check the scoping filter.** Commit.
- [ ] **Step 6: Vet, race, smoke (add a "watch" section: one long poll returns a snapshot with the
  fake fleet's workers). Report.**

---

### Task 5: TUI frame, Fleet, Tasks, Events, and `fleetctl tui` (agent-08)

**Files:**
- Create: `fleet/internal/tui/app.go` (+ test). The `tea.Model`:
  - **Boot screen:** the KARMAX wordmark in big block letters, painted with `Theme.Gradient`, a sweep
    over about 800 ms, any key skips; skipped entirely under `NoAnim`.
  - **Frame:** the header `KARMAX ▸ fleetctl` (gradient) with `role · device`, worker and working
    counts, and the clock; then the tab bar; then the body; then the footer with key hints and `made
    by MelloB · <version>`.
  - **Data and timing:** the watch loop (a `tea.Cmd` per long poll, backing off 1→15 s on errors with
    a banner, keeping the last snapshot dimmed); a `FrameMsg` ticker at 6 fps only while something
    animates.
  - **Keys:** `1`–`7` and Tab/Shift-Tab switch tabs; `q` and Ctrl-C quit; `?` shows help.
  - **Theme:** dark or light from `tea.BackgroundColorMsg`.
  - `Run(ctx, opts Options) error`.
- Create: `fleet/internal/tui/tab_fleet.go`. One card per agent, as the console:
  - a phase-colored left border;
  - a line per slot: `Sprite`, worker, phase label, task, model, ctx %, 5h `Bar`, current tool;
  - the agent header shows host, `sandbox` for sandboxes, and container cpu/mem.
  - Dense mode drops the card padding and the bars when the agents don't fit.
  - Keys: ⏎ shows a detail pane (the full row, alerts, quota resets, the last 5 events of that worker),
    `s` ssh watch, `t` ssh type, `m` model (admins; a picker of the agent's models), `d` done and `c`
    cancel on the selected worker's task (with a confirm).
- Create: `fleet/internal/tui/tab_tasks.go`.
  - A list of open tasks, then closed ones, with assigner, agent, age, and the last message (a
    question in cyan with a `?` badge).
  - ⏎ opens the detail: the thread from `Client.Task`, scrollable in a bubbles viewport.
  - Keys: `r` opens the run form (bubbles textarea for the brief; repo, agent and model fields; agent
    choices from the snapshot, only lent ones for guests), `f` follow-up, `a` answer (when the last
    message is a question), `c` cancel, `d` done. Each action posts a ToastMsg with the result.
- Create: `fleet/internal/tui/tab_events.go`. Newest first, colored by kind prefix (fleet.alert red,
  fleet.question cyan, fleet.report amber, fleet.access violet, otel dim). `/` opens a filter field
  (agent, kind prefix, task), and space pauses.
- Create: `fleet/cmd/fleetctl/tui.go` (+ test):
  - an HTTP `fleetapi.Client` over the existing client: watch, screens, tasks/{id}, run, followup,
    answer, cancel, done, model, access;
  - `fleetctl tui [--demo] [--no-anim] [--guest]`, where `--demo` runs on `fleetapi.NewFake` with a
    2 s `Step` ticker and `--guest` shows the guest view of the demo;
  - `fleetctl` with no args on a TTY → the TUI.
  - `Options.Attach` is wired to `AttachCommand` from task 3; until that merges, a function returning
    an ExecCommand that prints "ssh lands with the terminals work". The merge replaces it.
- Modify: `fleet/internal/guide/guide.md`, a line on `fleetctl tui`.

**Interfaces:**
- Consumes: `fleetapi.Client`, `Fake`, `Snapshot`; `tui.Options`, `Ctx`, `Tab`, messages, `Theme`, the
  widgets, and `Sprite` (task 6 owns its body).
- Produces: `tui.Run`; the tab registry in `app.go`:
  ```go
  // tabs is the TUI's tabs in order; task 6's tabs are appended by name.
  func defaultTabs() []Tab { return []Tab{newFleet(), newScreens(), newTasks(), newEvents(), newQuota(), newHosts(), newAccess()} }
  ```
  Task 6 provides `newScreens`, `newQuota`, `newHosts` and `newAccess`. Until it merges, put stubs
  in `tab_stubs.go` (a tab that says "coming"); the merge deletes that file.

- [ ] **Step 1: Failing app tests.** Drive `Update` with messages; call `View()` at fixed sizes; fixed
  clock; Fake.
  - `TestBootSkipsOnAKey`
  - `TestNoAnimHasNoBoot`
  - `TestTabsSwitchByNumberAndTab`
  - `TestAGuestHasNoHostsOrAccessTab`
  - `TestAWatchErrorKeepsTheLastSnapshotAndShowsABanner`
  - `TestTinyTerminalsDoNotPanic`: 0×0, 20×5, 40×12, before and after the first snapshot.
  - `TestWideRunesInATaskNameKeepTheFrame`: every line of `View()` is exactly the terminal's width.
- [ ] **Step 2: Implement app.go.** Commit.
- [ ] **Step 3: Golden views** in `testdata/*.golden`, written with `-update`: Fleet, Tasks and Events
  at 80×24, 120×40 and 200×60, as admin and as guest, with `NoAnim`, ANSI stripped for the comparison
  (`ansi.Strip`). Commit.
- [ ] **Step 4: The Fleet, Tasks and Events tabs**, with tests:
  - `TestRunFormSendsTheRequest`: Fake.Calls records it.
  - `TestAnswerOnlyOnAQuestion`
  - `TestDoneAsksFirst`
  - `TestEventsFilterByAgent`
  - `TestDenseModeWhenAgentsDoNotFit`
  
  Commit.
- [ ] **Step 5: `fleetctl tui` and the HTTP client**, tested against an httptest server that serves
  the contract's routes (`TestTheHTTPClientSpeaksTheContract`). Commit.
- [ ] **Step 6: Vet, race. Record `fleetctl tui --demo` with `script` at 120×40 and attach the
  transcript path to your report. Report.**

---

### Task 6: TUI Screens, Quota, Hosts, Access, and sprites (agent-01:2)

**Files:**
- Modify: `fleet/internal/tui/sprite.go`. The animated set:
  - four to six frames each;
  - exactly `SpriteWidth` (3) cells in every frame;
  - eyes scanning (reading), keys clattering (editing), gears turning (running), a rocket lifting
    (pushing), a bubbling flask (testing), a thought bubble growing (thinking), a blinking `?`
    (waiting), drifting `zzz` (idle), a snowflake (cooling), a static ✕ (down).
  - Frozen on frame 0 when the frame number is fixed.
  - Use only runes whose width lipgloss measures as 1. Test every frame's width, and pick a different
    rune where one is ambiguous.
- Create: `fleet/internal/tui/tab_screens.go`.
  - A grid of tiles; the column count is the floor of width ÷ 60, at least 1.
  - Each tile is a bordered box, titled with the worker and phase color, holding the last lines from
    `Client.Screens`, fetched every 2 s for the visible tiles only. Cut to the tile with
    `ansi.Truncate` per line, keeping ANSI.
  - ⏎ zooms one tile to the whole body; `s` and `t` ssh into it.
- Create: `fleet/internal/tui/tab_quota.go`.
  - One row per account: name, agents, 5h `Bar` and %, reset countdown, a 5h history `Spark`, burn
    %/h, run-out ("runs out 20:40" in amber, or "ok till reset" dim), and the 7d `Bar`.
  - The orchestrator's row is last. A "Today" panel: tasks closed, spend $, and output tokens.
- Create: `fleet/internal/tui/tab_hosts.go`.
  - Per host: reachable, load and its `Spark`, mem and disk `Bar`s with `Bytes`.
  - Then per container: cpu `Spark`, mem `Bar`, and restarts, plus stale workers (recorded_at older
    than 3 ticks) and unreachable markers.
  - A fleetd line: version, uptime, tick ms, and tick errors.
- Create: `fleet/internal/tui/tab_access.go` (admins).
  - Tagged devices (name, login, role, agents, last seen), then knocks with a badge.
  - Keys: `a` admin, `l` lend (a picker of the sandbox agents from the snapshot), `u` unlend, `x`
    revoke (confirm). Each calls `Client.ChangeAccess` and shows the result list.
  - Show the Claude-login caveat after a lend.

**Interfaces:**
- Consumes: as task 5, plus `Client.Screens`, `Access`, `ChangeAccess`.
- Produces: `newScreens()`, `newQuota()`, `newHosts()` and `newAccess()`, each returning a `Tab`, and
  the final `Sprite`.

- [ ] **Step 1: Failing sprite tests.**
  - `TestEverySpriteFrameIsThreeCells`
  - `TestSpritesMoveOnlyWithFrames`: frame fixed → identical.
  - `TestEveryActivityHasASprite`
  
  Implement. Commit.
- [ ] **Step 2: Failing tab tests** with the Fake (`Fake.Screens` and `Fake.Access` give data).
  - `TestScreensFetchOnlyVisibleTiles`: Fake records which workers were asked for. Add a `ScreensAsked`
    record to the Fake if you need one; keep the fake compiling.
  - `TestZoomFillsTheBody`
  - `TestQuotaShowsRunOutOnlyBeforeReset`
  - `TestHostsMarkStaleWorkers`
  - `TestLendOffersOnlySandboxes`
  - `TestRevokeAsksFirst`
  - `TestTilesKeepTheirWidthWithWideRunes`
  
  Implement. Commit.
- [ ] **Step 3: Golden views** for each tab at 80×24, 120×40 and 200×60 (`NoAnim`, ANSI stripped).
  Commit.
- [ ] **Step 4: Vet, race. Report.**

---

## Merge order (the operator's session)

1. Task 4 (watch), then task 1 (access); task 1 rebinds `allowed.go`, and task 4's scoping tests must
   still pass.
2. Task 2 (sandbox), then task 3 (terminals).
3. Task 5 (TUI frame), then task 6 (tabs; delete `tab_stubs.go`, wire `AttachCommand`).
4. After each merge: vet, race, smoke-v2, smoke.sh, and a mutation spot-check of that task's decision
   points.
5. Deploy per the spec's Rollout.
