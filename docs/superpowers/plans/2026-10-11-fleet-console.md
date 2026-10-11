# Fleet Console Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Fleet agents:** you are given one task track by the lead with `fleetctl give`. Read the spec
> and your track's tasks, nothing else. Work in a worktree on the branch your task names,
> based on the latest `origin/main`. After each task: run its tests, commit, push, and report with
> `fleetctl tell operator "<task id> ready on <branch> — <one line>"`, then start the next task
> (branch it from your previous one). If you are blocked on a decision, ask the same way and
> wait. Heavy builds and the full test suite go through `kbuild` (see your brief).

**Goal:** A cinematic, real-data control panel and 3D office on PC2 that shows every fleet
agent live and hands the operator any agent's keyboard. Everything the panel does is
available over HTTP and `fleetctl`.

**Architecture:** fleetd on Kali stays the system of record; a new `fleet-console` on PC2
observes the agents live (PTY tmux clients, tailed hook events, status lines, `docker stats`)
and merges that with fleetd's records into one snapshot. It streams the snapshot and its
patches over a WebSocket to a React panel embedded in the binary, which Chrome shows in
kiosk mode on a virtual display that RustDesk shares.

**Tech Stack:**
- **Go 1.26** (module `github.com/MelloB1989/karmax/fleet`), with `github.com/coder/websocket`
  and `github.com/creack/pty`.
- **Panel:** React 18, TypeScript, Vite 5, Vitest; three.js through `@react-three/fiber` and
  `@react-three/drei`; `@xterm/xterm` with `@xterm/addon-webgl`, and `@xterm/headless`;
  `motion`; `fast-json-patch`.

**Spec:** `docs/superpowers/specs/2026-10-11-fleet-console-design.md`

## Global Constraints

- Nothing is fake: no demo/replay mode in any shipped binary or production bundle. Dev
  fixtures load only under `import.meta.env.DEV`, and the production `dist/` must not contain
  them.
- Every value carries the time it was observed; stale sources show their age, never a value
  presented as live.
- One function per capability: fleetd routes call a `daemon.Fleet` method; fleet-console
  routes call a `console` method; `fleetctl` commands are HTTP clients of those routes.
- Config, not source: no host name, IP, port, path, repo or token in code. Defaults that
  ship in code: `console.listen` `127.0.0.1:7880`, terminal `220×50`, refresh cadences from
  the spec.
- The console listens on a loopback address only; config validation refuses anything else.
- Every console request requires the client token (bearer header or the login cookie).
- `/etc/tmux.conf` in the agent image: `window-size manual`, `default-size 220x50`, `prefix
  None`, all keys unbound, `bind -n C-] detach-client`.
- Palette: ground `#05080C`, panel `#0A1018`, line `#1B2A38`, amber `#FFB547`, cyan
  `#5CE1E6`, text `#D6E2EC`, dim `#7F93A6`, alert `#FF5D5D`. Type: IBM Plex Mono, Barlow
  Condensed.
- Commits end with `Co-Authored by MelloB's coding agent <build@mellob.in>`. No PII in code,
  tests, fixtures or commit messages (the pre-commit hook enforces a denylist; never bypass
  it).
- Branches: `fleet/<agent>/<topic>`; never push to `main`.

## Review Focus

1. **A viewer joining a terminal mid-stream:** a second panel, or the office after the
   overview, gets a full screen, not a half-drawn one. → Task 1C,
   `TestWatchSecondSubscriberGetsFullRedraw`.
2. **Control held while the WebSocket drops** (RustDesk reconnects, the browser reloads):
   the lock releases, and the agent is not left with a dangling read-write client. → Task 1C,
   `TestControlReleasedWhenSocketDrops`.
3. **fleetd down for minutes, then back:** the fleetd-sourced fields go stale with their age,
   then recover without a restart, and the event stream has no duplicates or gaps. → Task 1A,
   `TestFleetdOutageMarksStaleThenRecovers`, and Task 1B, `TestEventMergeDedupesLiveAndRecorded`.
4. **A key pressed while control is held:** `1`, `R`, `O` and `Esc` all reach the agent;
   only `Ctrl+]` is taken. → Task 2A, `keys.test.ts`.
5. **Fixtures leaking into production:** the prod `dist/` contains no fixture data. → Task
   2A, `npm run check:dist`.

---

## File map

```
fleet/
  internal/console/          (new)  model.go, config.go — contract (Task 0)
                                    sources_*.go, hub.go, patch.go, terms.go, server.go (agent-01)
  internal/console/dist/            built panel, embedded (placeholder committed in Task 0)
  cmd/fleet-console/main.go  (new)  agent-01
  cmd/fleetctl/main.go              give/inbox/wait/whoami/api (Task 0); console subcommands, attach (agent-01); peek/prompt/archive/up/down over HTTP (agent-04)
  internal/sshdial/          (new)  agent-01: an http.Client dialer over `ssh -W`
  internal/server/server.go         scopes, index, inbox/wait/whoami/healthz (Task 0); new routes (agent-04)
  internal/daemon/daemon.go         Inbox/Wait/Assign(from,brief) (Task 0); new Fleet methods, Row fields (agent-04)
  internal/ledger/ledger.go         tasks.assigner, Inbox (Task 0); events cursor, activity, today, open tasks (agent-04)
  internal/config/config.go         Console section (Task 0)
  internal/render/render.go         console.yaml + token render (agent-04)
  internal/render/brief.md.tmpl     report to assigner (Task 0)
  internal/hostctl/          (new)  render/up/down argv, moved out of fleetctl (agent-04)
  console/                   (new)  the panel app: scaffold + contract (Task 0), overview (agent-02), office (agent-03)
packaging/fleet/                    tmux.conf, Dockerfile, home/settings.json, event.sh (agent-04)
packaging/console/           (new)  install.sh, units, EDID, README (agent-04; lead runs root steps)
docs/superpowers/fleet-friction.md (new) the friction log (lead)
```

---

### Task 0: Lead — scopes, operator inbox, wait, give, contract and scaffold

Lead only, inline on `main` (small, sequential commits). Everything else depends on it.

**Files:**
- Modify: `fleet/internal/server/server.go`, `fleet/internal/server/server_test.go`
- Modify: `fleet/internal/daemon/daemon.go`, `fleet/internal/daemon/daemon_test.go`
- Modify: `fleet/internal/ledger/ledger.go`, `fleet/internal/ledger/ledger_test.go`
- Modify: `fleet/internal/config/config.go`, `fleet/internal/config/config_test.go`
- Modify: `fleet/cmd/fleetd/main.go`, `fleet/cmd/fleetctl/main.go`
- Modify: `fleet/internal/render/brief.md.tmpl`, `fleet/internal/render/orchestrator.md.tmpl`
- Create: `fleet/internal/console/model.go`, `fleet/internal/console/config.go`, `fleet/internal/console/config_test.go`, `fleet/internal/console/dist/index.html`
- Create: `fleet/console/` scaffold (listed in 0F)
- Create: `docs/superpowers/fleet-friction.md`

**Interfaces:**
- Produces (Go):
  - `server.Tokens{Full, Relay, Console string}`.
  - `daemon.Fleet.Assign(name, task, from, brief string, now time.Time) error`.
  - `daemon.Fleet.Inbox(after int64) ([]ledger.Relay, error)`.
  - `daemon.Fleet.Wait(ctx, agent, task string, after int64) (WaitResult, error)`.
  - `console.Snapshot` and every type in `model.go`.
  - `console.Config`, `console.LoadConfig(path)`.
- Produces (HTTP):
  - `GET /v1` → `[{"route":"GET /v1/status","scopes":["full","relay","console"]}, …]`.
  - `GET /healthz` → `{"version","uptime_s"}`.
  - `GET /v1/whoami` → `{"scope","name"}`.
  - `GET /v1/inbox?after=` → `[{"id","from","at","text"}]`.
  - `GET /v1/wait?agent=&task=&after=&timeout=` → `{"reason":"message|closed|phase|timeout","message":{…}|null,"phase":"…"}`.
  - `POST /v1/assign {agent, task, from?, brief?}`.
- Produces (TS):
  - `console/src/model.ts` (mirrors `model.go`).
  - `useFleet()`, `subscribeTerm()`, `useKey()`, `setControlHeld()`.
  - `<LiveTerminal>`, `api.prompt()`, `api.rotate()`.

- [ ] **0A: scope sets.** Write the failing tests in `server_test.go`.

```go
func TestConsoleScopeReadsAndPromptsButCannotAssign(t *testing.T) {
	_, h := setupScopes(t) // Tokens{Full:"full", Relay:"relay", Console:"console"}
	if w := do(t, h, "GET", "/v1/events", "console", ""); w.Code != 200 {
		t.Fatalf("console GET /v1/events = %d", w.Code)
	}
	w := do(t, h, "POST", "/v1/assign", "console", `{"agent":"agent-03","task":"x"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "may") {
		t.Fatalf("console assign = %d %s, want 403 naming what it may do", w.Code, w.Body)
	}
}

func TestRelayMayTellTheOperator(t *testing.T) {
	f, h := setupScopes(t)
	if w := do(t, h, "POST", "/v1/tell", "relay", `{"from":"agent-03","to":"operator","text":"done"}`); w.Code != 200 {
		t.Fatalf("relay tell operator = %d %s", w.Code, w.Body)
	}
	if got := f.calls[len(f.calls)-1]; got != "tell agent-03 operator done" {
		t.Fatalf("call = %q", got)
	}
}

func TestIndexListsEveryRouteWithItsScopes(t *testing.T) {
	_, h := setupScopes(t)
	w := do(t, h, "GET", "/v1", "relay", "")
	var idx []struct{ Route string; Scopes []string }
	_ = json.Unmarshal(w.Body.Bytes(), &idx)
	if len(idx) < 12 || idx[0].Route == "" {
		t.Fatalf("index = %s", w.Body)
	}
}
```

Run: `cd fleet && go test ./internal/server/` — Expected: FAIL (no `Console` field, `/v1` missing).

Implement:
- `scope` becomes a named string set: `full`, `relay`, `console`.
- `route(pattern string, h handler, allowed ...scope)` records `{pattern, allowed}` in a slice that `GET /v1` serves.
- The 403 says "this token (scope X) may: <comma list of its routes>".
- Route scopes:
  - every read: full + console; `status` and `/v1` also relay;
  - `tell`: all three;
  - `prompt` and `rotate`: full + console;
  - `assign`, `done`, `enable`, `restore`: full.
- A relay or console token may set `from=operator` only when `to` is not `operator`. A console
  token always speaks as `operator` (the handler overrides `from`).

Run the tests again — Expected: PASS. Commit `fleetd: scope sets, a console scope, and an index of routes`.

- [ ] **0B: operator inbox.** Write the failing tests.

```go
// daemon_test.go
func TestTellToOperatorIsStoredNotRelayed(t *testing.T) {
	f, boxes := newFleet(t) // existing helper
	if err := f.Tell(context.Background(), "agent-03", "operator", "ready on fleet/agent-03/x", now); err != nil {
		t.Fatal(err)
	}
	if n := boxes["agent-03"].relays; n != 0 {
		t.Fatalf("relayed %d times, want 0", n)
	}
	msgs, _ := f.Inbox(0)
	if len(msgs) != 1 || msgs[0].From != "agent-03" || msgs[0].Text != "ready on fleet/agent-03/x" {
		t.Fatalf("inbox = %+v", msgs)
	}
}
```

Run: `go test ./internal/daemon/` — Expected: FAIL (`Inbox` undefined; `operator` unknown).

Implement:
- `Tell`: `to == "operator"` → `db.AddRelay{Delivered: true}` and the `fleet.relay` event, with no box call.
- `ledger.Inbox(after int64)`: `SELECT id, sender, at, text FROM relays WHERE recipient='operator' AND id > ? ORDER BY id`.
- `Relay` gains `ID int64` (`json:"id"`).
- Route `GET /v1/inbox` (full, console).
- `fleetctl inbox [--after N] [--follow]`: `--follow` polls every 2 s and prints new messages as `HH:MM:SS from: text`.

Test: `TestInboxRouteReturnsOperatorMessagesAfterAnID` in `server_test.go`. Run, PASS, commit
`fleetd: an operator inbox — agents can report to the operator`.

- [ ] **0C: assign with assigner and brief, `fleetctl give`.** Write the failing tests.

```go
func TestAssignWithBriefRecordsAssignerAndDeliversTheBrief(t *testing.T) {
	f, boxes := newFleet(t)
	if err := f.Assign("agent-03", "t1", "operator", "build the thing", now); err != nil {
		t.Fatal(err)
	}
	task, _ := f.DB().Task("t1")
	if task.Assigner != "operator" {
		t.Fatalf("assigner = %q", task.Assigner)
	}
	if boxes["agent-03"].lastRelay != "build the thing" {
		t.Fatalf("brief not delivered: %q", boxes["agent-03"].lastRelay)
	}
}
```

Run — Expected: FAIL (signature).

Implement:
- **Ledger:** `tasks` gains `assigner TEXT`, added with `ALTER TABLE tasks ADD COLUMN assigner TEXT` when it is missing (check `PRAGMA table_info`). `OpenTask(id, agent, assigner, at)`.
- **Assign:** `Assign(name, task, from, brief, now)`. With a non-empty brief it calls `f.Tell(ctx, from, name, brief, now)` after recording the task. The context is passed in: the Ops interface gets `Assign(ctx, name, task, from, brief string, now time.Time) error`, so update the fake in `server_test.go`.
- **Route:** body `{agent, task, from, brief}`; a full token defaults `from` to `operator`.
- **CLI:** `fleetctl give <agent> --task <id> "<brief…>"` and `fleetctl assign <agent> --task <id> [--brief …] [--from …]`.

Run, PASS, commit `fleetd: assign records who assigned it and can deliver the brief; fleetctl give`.

- [ ] **0D: wait.** Write the failing tests.

```go
func TestWaitReturnsOnTheAgentsMessageToTheOperator(t *testing.T) {
	f, _ := newFleet(t)
	done := make(chan WaitResult, 1)
	go func() { r, _ := f.Wait(context.Background(), "agent-03", "", 0); done <- r }()
	time.Sleep(50 * time.Millisecond)
	_ = f.Tell(context.Background(), "agent-03", "operator", "ready", time.Now())
	select {
	case r := <-done:
		if r.Reason != "message" || r.Message.Text != "ready" {
			t.Fatalf("wait = %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestWaitReturnsWhenTheTaskCloses(t *testing.T) { /* Assign t1, Wait(agent,"t1",0) in a goroutine, Done(t1) → Reason "closed" */ }
func TestWaitReturnsOnDownCoolingOrDisabled(t *testing.T) { /* setState Phase=Down → Reason "phase", Phase "down" */ }
func TestWaitTimesOut(t *testing.T) { /* ctx with 100ms deadline → Reason "timeout", nil error */ }
```

Write the three stubbed tests out in full, in the first test's style.

Implement:
- `type WaitResult struct{ Reason string; Message *ledger.Relay; Phase string }`.
- `Wait` checks every 500 ms. Its pollers are injectable in tests via `f.waitEvery`.
- Route `GET /v1/wait?agent=&task=&after=&timeout=30m` (full, console). The handler wraps the
  context with the timeout.
- `fleetctl wait <agent> [--task id] [--timeout 30m]`:
  - `after` defaults to the newest inbox id at start;
  - the client's timeout is `--timeout` + 30 s;
  - it prints the result, with exit codes 0 = message or closed, 3 = phase, 4 = timeout.

Commit `fleetd: wait on an agent — a message, a closed task or a phase`.

- [ ] **0E: whoami, healthz, brief.**
  - `GET /v1/whoami` returns `{"scope","name"}`: the name is `operator` for full and console,
    `agent` for relay.
  - `GET /healthz` needs no token and returns `{"version","uptime_s"}`. `version` comes from a
    `-ldflags -X` variable, default `dev`.
  - `fleetctl whoami` and `fleetctl api` (prints the `/v1` index).
  - **Tests:** `TestHealthzNeedsNoToken`, `TestWhoamiNamesTheScope`.
  - **Briefs:**
    - `brief.md.tmpl`: replace "message {{.Orchestrator}}" with "report to whoever assigned the
      task: `fleetctl tell <assigner> "<report>"` — the assigner is in the task's brief, and is
      `operator` when the brief comes from `fleetctl give`."
    - `fleetctl give` prefixes the brief with `Task <id> from <from>.`
    - `orchestrator.md.tmpl`: the orchestrator passes `--from <its name>` to `assign`.
  - **Render test** `TestBriefSaysReportToTheAssigner`.
  - Commit `fleet: whoami, healthz, and agents report to their assigner`.

- [ ] **0F: the console contract and scaffold.**

`fleet/internal/console/model.go`:

```go
// Package console is the fleet console: it observes the agents on its own
// host live, merges fleetd's records, and serves the result as one snapshot
// and its patches. model.go is the contract the panel (console/src/model.ts)
// mirrors field for field.
package console

import (
	"encoding/json"
	"time"
)

// Source keys.
const (
	SrcTerminals  = "terminals"
	SrcEvents     = "events"
	SrcStatusline = "statusline"
	SrcDocker     = "docker"
	SrcHost       = "host"
	SrcGit        = "git"
	SrcFleetd     = "fleetd"
)

type Source struct {
	OK    bool      `json:"ok"`
	At    time.Time `json:"at,omitzero"`
	Error string    `json:"error,omitempty"`
}

type Snapshot struct {
	At           time.Time           `json:"at"`
	Sources      map[string]Source   `json:"sources"`
	Host         HostInfo            `json:"host"`
	Orchestrator *Orchestrator       `json:"orchestrator"`
	Tasks        []Task              `json:"tasks"`
	Agents       []Agent             `json:"agents"`
	Events       []Event             `json:"events"`
	Today        Today               `json:"today"`
	Git          Git                 `json:"git"`
	Activity     map[string][]Bucket `json:"activity"`
}

type HostInfo struct {
	Name     string     `json:"name"`
	UptimeS  int64      `json:"uptime_s"`
	Load     [3]float64 `json:"load"`
	MemUsed  int64      `json:"mem_used"`
	MemTotal int64      `json:"mem_total"`
	CPUs     int        `json:"cpus"`
	At       time.Time  `json:"at"`
}

type Orchestrator struct {
	Name           string    `json:"name"`
	FiveHourPct    float64   `json:"five_hour_pct"`
	SevenDayPct    float64   `json:"seven_day_pct"`
	FiveHourResets time.Time `json:"five_hour_resets,omitzero"`
	SevenDayResets time.Time `json:"seven_day_resets,omitzero"`
	At             time.Time `json:"at"`
}

type Task struct {
	ID       string    `json:"id"`
	Agent    string    `json:"agent"`
	Assigner string    `json:"assigner"`
	Since    time.Time `json:"since"`
}

// Phases, as fleetd's reconciler names them.
const (
	PhaseStandby  = "standby"
	PhaseWorking  = "working"
	PhaseWaiting  = "waiting"
	PhaseCooling  = "cooling"
	PhaseDisabled = "disabled"
	PhaseDown     = "down"
)

type Agent struct {
	ID   string `json:"id"`
	Host string `json:"host"`
	Live bool   `json:"live"` // on the console's host: terminals and live sources exist

	// From fleetd (recorded, ~30 s).
	Phase      string    `json:"phase"`
	Alerts     []string  `json:"alerts"`
	Task       string    `json:"task,omitempty"`
	IdleS      int64     `json:"idle_s"`
	Models     []string  `json:"models"`
	Session    string    `json:"session,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`

	// From the status line (live, 5 s).
	Model          string    `json:"model,omitempty"`
	ContextPct     float64   `json:"context_pct"`
	FiveHourPct    float64   `json:"five_hour_pct"`
	SevenDayPct    float64   `json:"seven_day_pct"`
	FiveHourResets time.Time `json:"five_hour_resets,omitzero"`
	StatuslineAt   time.Time `json:"statusline_at,omitzero"`

	// From live hook events.
	CurrentTool *Tool     `json:"current_tool"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`

	Container *Container `json:"container"`
	LastTask  *LastTask  `json:"last_task"`
	Term      TermSize   `json:"term"`
}

type Tool struct {
	Name  string    `json:"name"`
	Since time.Time `json:"since"`
}

type Container struct {
	Running  bool      `json:"running"`
	CPUPct   float64   `json:"cpu_pct"`
	Mem      int64     `json:"mem"`
	MemLimit int64     `json:"mem_limit"`
	NetRx    int64     `json:"net_rx"`
	NetTx    int64     `json:"net_tx"`
	At       time.Time `json:"at"`
}

type LastTask struct {
	ID      string    `json:"id"`
	Outcome string    `json:"outcome"`
	Ended   time.Time `json:"ended"`
	CostUSD float64   `json:"cost_usd"`
}

type TermSize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// Event is one stream entry. ID is fleetd's; 0 while only the live tail has it.
type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	Agent   string          `json:"agent"`
	Kind    string          `json:"kind"`
	Session string          `json:"session,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// StreamKinds are the events a person reads; tool-level events drive
// current_tool and activity only. A kind matches when equal, or when the
// entry ends in "." and the kind has it as a prefix.
var StreamKinds = []string{"fleet.", "SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure", "Notification"}

type Today struct {
	Since        time.Time `json:"since"`
	TasksClosed  int       `json:"tasks_closed"`
	SpendUSD     float64   `json:"spend_usd"`
	OutputTokens int64     `json:"output_tokens"`
}

type Git struct {
	Repos []Repo `json:"repos"`
}

type Repo struct {
	Name      string    `json:"name"`
	Main      Commit    `json:"main"`
	Branches  []Branch  `json:"branches"`
	Commits7d int       `json:"commits_7d"`
	At        time.Time `json:"at"`
}

type Commit struct {
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type Branch struct {
	Name    string    `json:"name"`
	Agent   string    `json:"agent"`
	SHA     string    `json:"sha"`
	Subject string    `json:"subject"`
	At      time.Time `json:"at"`
}

type Bucket struct {
	T time.Time `json:"t"`
	N int       `json:"n"`
}

// Message is one frame of /v1/watch.
type Message struct {
	Type     string    `json:"type"` // "snapshot" | "patch"
	Seq      uint64    `json:"seq"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Ops      []PatchOp `json:"ops,omitempty"`
}

// PatchOp is one RFC 6902 operation (add, remove, replace).
type PatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// TermFrame is a JSON text frame on /v1/term; terminal bytes are binary frames.
type TermFrame struct {
	Type   string `json:"type"` // "hello" | "error" | "reconnecting" | "released"
	Agent  string `json:"agent,omitempty"`
	Mode   string `json:"mode,omitempty"` // "watch" | "control"
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Holder string `json:"holder,omitempty"`
	Error  string `json:"error,omitempty"`
}
```

`fleet/internal/console/config.go`, which `fleetctl render` writes and fleet-console reads:

```go
type Config struct {
	Host            string      `yaml:"host"`
	Listen          string      `yaml:"listen"`            // loopback only; default 127.0.0.1:7880
	FleetdURL       string      `yaml:"fleetd_url"`        // fleetd's relay listener
	FleetdTokenFile string      `yaml:"fleetd_token_file"` // console-scope token for fleetd
	ClientTokenFile string      `yaml:"client_token_file"` // the token clients present here
	Orchestrator    string      `yaml:"orchestrator"`
	Repos           []string    `yaml:"repos"`
	Term            TermSize    `yaml:"term"`              // default 220×50
	Agents          []AgentConf `yaml:"agents"`
}

type AgentConf struct {
	Name      string   `yaml:"name"`
	Host      string   `yaml:"host"`
	Container string   `yaml:"container"`
	Exec      []string `yaml:"exec"` // the docker prefix on the console's host, e.g. [docker]
	Live      bool     `yaml:"live"` // Host == the console's host
}

// LoadConfig reads, expands ~, fills defaults and validates; every error names its key.
func LoadConfig(path string) (*Config, error)
```

Validation, each tested in `config_test.go` with an error that names the key:
- `listen` must be a loopback IP or `localhost` (`listen: 0.0.0.0:7880 is not a loopback address`);
- `fleetd_url` must be http(s);
- both token files must be set;
- agent names must be unique and non-empty;
- a live agent needs `container` and `exec`.

`fleet/internal/config/config.go` gains:

```go
type Console struct {
	Host      string   `yaml:"host"`       // which fleet host runs the console
	Listen    string   `yaml:"listen"`     // on that host; default 127.0.0.1:7880
	FleetdURL string   `yaml:"fleetd_url"` // how the console reaches fleetd
	TokenEnv  string   `yaml:"token_env"`  // console-scope token, in env_file
	Repos     []string `yaml:"repos"`      // git repos the console watches
	Dir       string   `yaml:"dir"`        // config dir on the console host; default ~/.config/fleet-console
}
// Config.Console *Console `yaml:"console"` (nil = no console)
```

fleetd reads `Console.TokenEnv` from the env file into `Tokens.Console`.

**Scaffold, `fleet/console/`:**
- `package.json`, with exact versions:
  - `react@18.3.1`, `react-dom@18.3.1`, `three@0.169.0`, `@react-three/fiber@8.17.10`,
    `@react-three/drei@9.114.3`, `@react-three/postprocessing@2.16.3`;
  - `@xterm/xterm@5.5.0`, `@xterm/headless@5.5.0`, `@xterm/addon-webgl@0.18.0`;
  - `motion@11.11.17`, `fast-json-patch@3.1.1`;
  - dev: `vite@5.4.10`, `@vitejs/plugin-react@4.3.3`, `typescript@5.6.3`, `vitest@2.1.4`,
    `jsdom@25.0.1`, `@types/react@18.3.12`, `@types/react-dom@18.3.1`, `@types/three@0.169.0`.
- Scripts:
  - `dev`, `build` (into `../internal/console/dist`), `test` (vitest run), `typecheck`;
  - `check:dist`: `! grep -rl "__FIXTURE__" ../internal/console/dist`. Fixture files carry the
    marker `"__FIXTURE__": true`.
- `vite.config.ts`: in dev, proxies `/v1` and `/login` (with `ws: true`) to `VITE_CONSOLE` when
  it is set.
- `src/model.ts`: the TS mirror of `model.go`. Times are ISO strings.
- `src/state/store.ts`: `useFleet(): {snapshot: Snapshot | null, conn: "connecting" | "live" | "lost", seq: number}`.
  - It opens `/v1/watch`, applies patches with fast-json-patch, and on a seq gap closes and
    resubscribes.
  - Under `import.meta.env.DEV && import.meta.env.VITE_FIXTURE` it loads `dev/snapshot.json`
    instead.
- `src/term/stream.ts`: `subscribeTerm(agent: string, onData: (b: Uint8Array) => void, onFrame?: (f: TermFrame) => void): () => void`.
  - One refcounted WebSocket per agent to `/v1/term/{agent}`.
  - DEV fixture: replays `dev/term-<agent>.ans` once.
- `src/term/control.ts`: `openControl(agent): {send(b: Uint8Array): void; close(): void; onFrame(cb): void; onData(cb): void}`,
  using `?control=1`.
- `src/keys.ts`: `useKey(combo: string, handler: (e: KeyboardEvent) => void, opts?: {scope?: string})`,
  `setControlHeld(held: boolean, onRelease: () => void)`.
  - While control is held, only `Ctrl+]` is handled, and every other key goes to the focused
    terminal.
- `src/term/LiveTerminal.tsx`: `<LiveTerminal agent cols rows control={boolean} onRelease={() => void} fit="width" | "contain" />`.
  - xterm.js with the WebGL addon (DOM fallback), sized from `cols`×`rows` and scaled by CSS
    transform to fit.
- `src/api.ts`: `prompt(agent, text)`, `rotate(agent)`, both POSTs to the console.
- `src/App.tsx`: renders `<Overview/>` or `<Office/>` (state `view`). `src/overview/Overview.tsx`
  and `src/office/Office.tsx` are stubs that render the view's name; agents 02 and 03 replace
  them.
- `dev/snapshot.json`: built from the real fleet's status, quota and events at the time of
  writing, mapped onto `Snapshot`, with `"__FIXTURE__": true`.
- `dev/term-agent-0N.ans`: `tmux capture-pane -e -p` of each real agent.
- Tests: `src/state/store.test.ts` (applies a patch; resubscribes on a gap) and `src/keys.test.ts`
  (the control-held rule).

`fleet/internal/console/dist/index.html`: a committed placeholder that says "panel not built —
run npm run build in fleet/console". It is embedded with `//go:embed all:dist` (agent-01). A
real build overwrites it, and `.gitignore` ignores `dist/assets/`.

Run `npm ci && npm test && npm run typecheck && npm run build && npm run check:dist`, then
`git checkout fleet/internal/console/dist/index.html`.

Run `go test ./...` (fleet) — PASS. Commit `Fleet console: the contract (model.go, model.ts) and the panel scaffold`.

- [ ] **0G: deploy and start the agents.** Steps:
  1. Push `main`.
  2. Rebuild and restart fleetd on Kali (`go build -o ~/.local/bin/fleetd ./cmd/fleetd`, then
     `systemctl --user restart fleetd`) and `fleetctl`.
  3. Add `console:` to `~/.karmax/fleet/fleet.yaml`, and a generated `FLEET_CONSOLE_TOKEN` to
     the env file.
  4. `fleetctl render && fleetctl up pc2`, so agents get the new brief.
  5. Smoke: `fleetctl whoami`, `fleetctl api | head`, `fleetctl give agent-04 --task smoke "reply
     with fleetctl tell operator pong"`, then `fleetctl wait agent-04 --timeout 5m` returns
     `message: pong`.
  6. Then `fleetctl give` each agent its track (Tasks 1–4) and log each friction point in
     `docs/superpowers/fleet-friction.md`.

---

### Task 1A (agent-01): console sources — branch `fleet/agent-01/console-sources`

**Files:**
- Create in `fleet/internal/console/`: `src_host.go`, `src_docker.go`, `src_git.go`,
  `src_statusline.go`, `src_events.go`, `src_fleetd.go`, `stream.go`, each with its `_test.go`.
- Modify: `fleet/internal/outbound/outbound.go` (`ParseDockerStats` keeps the memory limit and
  net I/O).
- Modify: `fleet/go.mod` (add `github.com/creack/pty v1.1.24`, `github.com/coder/websocket v1.8.12`).

**Interfaces:**
- Consumes: `console.Config`, `model.go`, `target.Container`, `observe.ParseEvents`,
  `observe.ParseStatus`.
- Consumes: fleetd routes from Task 4A (JSON below).
- Produces: `type Sources struct{…}`, `NewSources(cfg *Config, run target.Runner, now func() time.Time) *Sources`,
  `(*Sources).Run(ctx, onChange func())`, `(*Sources).Fill(s *Snapshot)` (writes every field
  it owns, with its `at`).

Every source is a struct with:
- `run(ctx)`;
- the last good value;
- `Source{OK, At, Error}`;
- `stale(now) bool` = `now - At > 2×cadence` (streamed: the process is not running).

Cadences come from constants in `stream.go`: statusline 5 s, host 2 s, git 10 s, fleetd 5 s. A
streamed source restarts with backoff: 1 s, doubling, capped at 30 s.

fleetd JSON this task consumes (Task 4A produces; agree exactly):
- `GET /v1/status` → `{"agents":[Row…], "orchestrator":{"FiveHour","SevenDay","FiveHourResets","SevenDayResets"}|absent}`.
  - `Row` gains `idle_s` (number), `model`, `alerts` ([]string), and `recorded_at`.
- `GET /v1/tasks?open=1` → `[{"id","agent","assigner","assigned","closed","outcome","branch","pr"}]`.
- `GET /v1/events?after=<id>&kinds=<comma>&limit=<n>` → `[{"id","agent","at","kind","session","detail"}]`,
  ascending by id.
- `GET /v1/activity?since=24h&bucket=15m` → `{"bucket_s":900,"agents":{"agent-01":[{"t":"…","n":3}]}}`.
- `GET /v1/today` → `{"since","tasks_closed","spend_usd","output_tokens"}`.
- `GET /v1/history?agent=&n=1` (exists) and `GET /v1/tasks?agent=&n=1`, for `last_task`
  (outcome from tasks, cost from the session with that task).

- [ ] **Step 1: host.** Write `TestHostParsesProcFixtures`: fixture `/proc/loadavg`, `/proc/meminfo`
  and `/proc/uptime` under `testdata/proc/`. `readHost(root string) (HostInfo, error)` with `root`
  injectable. Expected: Load `[0.00 0.05 0.00]`, MemTotal from `MemTotal`, MemUsed = `MemTotal - MemAvailable`.
  Run (FAIL), implement, run (PASS).
- [ ] **Step 2: docker stats stream.**
  - Extend `outbound.ParseDockerStats` to parse `MemUsage` "128.3MiB / 1.5GiB" into used and
    limit, and `NetIO` "2.38MB / 1.78MB" into rx and tx. Test with the real line format:
    `{"Name":"agent-01","CPUPerc":"1.31%","MemUsage":"128.3MiB / 1.5GiB","NetIO":"2.38MB / 1.78MB"}`.
  - `src_docker.go` runs `docker stats --format '{{json .}}'` (no `--no-stream`) as one
    long-lived process, reads lines, and keys them by container name.
  - Test: `TestDockerStreamUpdatesOnEachLine`, with a fake process that writes two frames.
- [ ] **Step 3: git.**
  - `TestGitReportsMainBranchesAndCommits` builds a temp repo with `git init`, two commits on
    `main`, and a branch `fleet/agent-02/x`.
  - Expected: `Main.Subject`, `Branches[0].Agent == "agent-02"`, `Commits7d == 2`.
  - Implement with `git for-each-ref --format=%(refname:short)%09%(objectname:short)%09%(committerdate:iso-strict)%09%(subject) refs/heads/fleet refs/remotes/origin/fleet`
    and `git rev-list --count --since=7.days main`.
- [ ] **Step 4: status line.**
  - Per live agent, every 5 s: `Container.Run(ctx, "cat", "<home>/.claude/fleet/status.json")`.
    The home comes from `observe`; the path is the same one fleetd reads, found by grepping
    `status.json` in `internal/agent/agent.go`.
  - Parse it with `observe.ParseStatus` and fill `Model`, `ContextPct`, `FiveHourPct`,
    `SevenDayPct`, `FiveHourResets` and `StatuslineAt`. `StatuslineAt` is the file's mtime from
    `stat -c %Y`, not the fetch time.
  - Test with a fake Runner returning the fixture JSON captured from a real agent:
    `TestStatuslineUsesTheFilesMtime`.
- [ ] **Step 5: live events.**
  - Per live agent, one long-lived `docker exec <c> tail -n0 -F <home>/.claude/fleet/events.jsonl`
    (built with `Container.InteractiveArgv`'s prefix but without `-t`), parsed with
    `observe.ParseEvents`.
  - `PreToolUse` sets `CurrentTool{Name, Since: event time}`.
  - `PostToolUse`, or `Stop` with the same tool, clears it.
  - Every event updates `LastEventAt` and goes to a 24 h activity counter. Stream kinds also go
    to a live list.
  - Tests:
    - `TestPreThenPostSetsAndClearsCurrentTool`;
    - `TestToolEventsStayOutOfTheStream`;
    - `TestTailSurvivesTheFileBeingMoved`: a real `tail -F` in a temp dir, with the file renamed
      and recreated mid-stream, the way fleetd's drain does it; every line arrives exactly once.
- [ ] **Step 6: fleetd client.**
  - `src_fleetd.go` polls `status`, `tasks?open=1`, `events?after=<last>&kinds=…`,
    `activity` (every 60 s) and `today` (every 30 s), with the bearer from `FleetdTokenFile`.
  - Tests with `httptest`:
    - `TestFleetdFillsRecordedFields`;
    - `TestFleetdOutageMarksStaleThenRecovers`: the server returns 503 for 3 polls, so
      `sources.fleetd.ok` is false and `stale(now)` is true after 10 s; then it returns 200,
      and `ok` is true with no restart.
- [ ] **Step 7: `Sources.Fill`** merges everything into a `Snapshot`.
  - Agents are ordered by name.
  - A non-live agent's live fields are zero, and it has `Live: false`.
  - Test: `TestFillMarksRemoteAgentsNotLive`.
  - Run `go test ./internal/console/ ./internal/outbound/`, all PASS. Commit
    `fleet-console: live and recorded sources`, push, report.

### Task 1B (agent-01): hub, patches, server — branch `fleet/agent-01/console-hub`

**Files:**
- Create in `fleet/internal/console/`: `hub.go`, `patch.go`, `events_merge.go`, `server.go`,
  `auth.go`, `embed.go`, with tests.
- Create: `fleet/cmd/fleet-console/main.go`.

**Interfaces:**
- Produces:
  - `NewHub(src *Sources) *Hub`, `(*Hub).Snapshot() Snapshot`,
    `(*Hub).Subscribe(ctx) (<-chan Message, func())`;
  - `Diff(prev, next []byte) ([]PatchOp, error)` (JSON in, RFC 6902 out);
  - `Handler(h *Hub, t *Terms, clientToken string, fleetd *FleetdClient) http.Handler`.

- [ ] **Step 1: patch.**
  - `TestDiffThenApplyEqualsNext`: for 200 random mutations of the fixture snapshot (agent
    fields, appended events, removed tasks), `apply(prev, Diff(prev, next)) == next`. Apply is
    implemented in the test file.
  - Arrays: replace an array whole when its length changed, else diff it element by element.
  - Implement over `map[string]any` and `[]any` from `json.Unmarshal`.
- [ ] **Step 2: event merge.**
  - `TestEventMergeDedupesLiveAndRecorded`: a live event with no id, then fleetd's copy (same
    agent, session, second, kind) with id 42, gives one entry with id 42.
  - Also: newest first, at most 200 entries, only stream kinds.
- [ ] **Step 3: hub.** The hub rebuilds the snapshot on each source change, at most 10 times
  a second. It marshals the snapshot, diffs it against the previous one, and sends
  `{type:"patch", seq}` to subscribers; a new subscriber gets `{type:"snapshot"}` first. A slow
  subscriber (its channel full) is dropped, and the client resubscribes.
  - Tests:
    - `TestSubscriberGetsSnapshotThenPatches`;
    - `TestSlowSubscriberIsDroppedNotBlocking`;
    - `TestRebuildsAreBatchedToTenPerSecond`.
- [ ] **Step 4: auth.**
  - `GET /login?k=<token>`: on a match, it sets the cookie `fc` (HttpOnly, SameSite=Strict,
    Path=/) and replies 303 to `/`; otherwise 401.
  - Every other route except `/healthz` and the static assets needs the bearer header or the
    cookie.
  - The token compare is constant-time.
  - Tests:
    - `TestLoginSetsCookieAndRedirectsWithoutTheToken`;
    - `TestAPIRefusesWithoutToken`;
    - `TestStaticNeedsNoTokenButAPIDoes`.
- [ ] **Step 5: routes.**
  - `GET /v1/snapshot`.
  - `GET /v1/watch`, a WebSocket (coder/websocket) that writes each `Message` as a text frame.
  - `POST /v1/prompt` and `POST /v1/rotate`, proxied to fleetd and returning fleetd's body
    verbatim.
  - `GET /healthz` → `{"sources":{…},"version"}`.
  - `/` serves the embedded `dist` with SPA fallback to `index.html`.
  - Tests:
    - `TestWatchOverWebSocket` (`httptest.NewServer` plus `websocket.Dial`);
    - `TestPromptIsProxiedAsIs`.
- [ ] **Step 6: main.** `fleet-console --config <path> [--version]`.
  - It loads the config (exits 2 naming the key on error), reads both token files (they must
    be mode 0600, or it refuses), starts the sources, terms, hub and server, and shuts down on
    SIGTERM.
  - Test: `TestRefusesAWorldReadableTokenFile`.
  - Commit `fleet-console: hub, patches, auth and the HTTP API`, push, report.

### Task 1C (agent-01): terminals — branch `fleet/agent-01/console-terms`

**Files:**
- Create: `fleet/internal/console/terms.go`, `terms_test.go`.
- Modify: `server.go` (`/v1/term/{agent}`).

**Interfaces:**
- Produces: `NewTerms(cfg *Config, start func(argv []string) (*os.File, *exec.Cmd, error)) *Terms`,
  `(*Terms).Watch(ctx, agent string, out func([]byte), frame func(TermFrame)) error`,
  `(*Terms).Control(ctx, agent, who string, in <-chan []byte, out func([]byte), frame func(TermFrame)) error`.
  `start` defaults to `pty.StartWithSize` at the configured cols×rows.

**Watch:**
- One shared `<exec> exec -it <c> tmux attach -r -t main` per agent, under a PTY of
  `cols×rows`.
- It fans out to subscribers.
- A new subscriber triggers `tmux refresh-client -t <that client's tty>`; the tty is found with
  `tmux list-clients -F '#{client_tty} #{client_readonly} #{client_pid}'` and matched by the
  readonly flag and our pid. That redraws the full screen for everyone.
- The client closes 30 s after its last subscriber leaves.

**Control:**
- One per agent, guarded by a mutex map.
- A second `Control` returns `ErrHeld{Holder}`; the route answers 409 with `{"holder"}`.
- Input frames go to the PTY. Closing the context kills the client process and releases.

**Tests** run against a real tmux in a container-less mode: `exec: []`, with `container` empty
meaning native, as in `target.Container.native`. They use `tmux -L test-<random> -f
packaging/fleet/tmux.conf` and start a session running `cat`.

- [ ] `TestWatchStreamsWhatTheSessionPrints`: `tmux send-keys -t main 'hello' Enter`; the
  watcher sees `hello` within 2 s.
- [ ] `TestWatchSecondSubscriberGetsFullRedraw`: the session prints `MARK`, then a second
  subscriber joins and receives `MARK` within 2 s without anything new being printed.
- [ ] `TestControlTypesIntoTheSession`: input `echo hi\r` → `capture-pane` shows `hi`.
- [ ] `TestOnlyOneController`: the second Control returns `ErrHeld` naming the first `who`.
- [ ] `TestControlReleasedWhenSocketDrops`: cancel the first controller's ctx; a new Control
  succeeds within 1 s, and `tmux list-clients` shows no read-write client left.
- [ ] `TestCtrlBracketDetachesNotTmuxPrefix`: control input `\x02d` (Ctrl-b d) does not detach;
  `\x1d` (Ctrl-]) detaches the control client, and a `released` frame is sent.
- [ ] `TestWindowStays220x50AfterASmallClient`: attach a 80×24 PTY client; `tmux display -p
  '#{window_width}x#{window_height}'` is still `220x50`.

Commit `fleet-console: terminals — shared watch, one controller, no tmux prefix`, push, report.
These tests need `packaging/fleet/tmux.conf` from Task 4B; if it hasn't landed, create it
with exactly the Global Constraints' content in this branch and say so in the report.

### Task 1D (agent-01): CLI doors — branch `fleet/agent-01/console-cli`

**Files:**
- Create: `fleet/internal/sshdial/sshdial.go`, `sshdial_test.go`.
- Modify: `fleet/cmd/fleetctl/main.go`.

**Interfaces:**
- Produces: `sshdial.Client(host, addr string) *http.Client` (its `DialContext` runs
  `ssh -o BatchMode=yes <host> -W <addr>` and wraps the stdio as `net.Conn`) and
  `sshdial.Dialer(host, addr)` for `websocket.Dial`'s HTTPClient.

- [ ] `TestDialRunsSSHWithW`: a fake `ssh` script on PATH that `exec`s `nc 127.0.0.1 <port>`
  to an `httptest` server; `GET /healthz` through `Client` succeeds, and the fake recorded
  `-W 127.0.0.1:<port>`.
- [ ] The console's address comes from fleet.yaml's `console.host` and `console.listen`; the
  client token is read over SSH:
  `ssh <host> cat <console.dir>/client.token`. Add the commands:
  - `fleetctl console snapshot [--json]`: without `--json`, it prints a table of agents (phase,
    tool, ctx, 5 h, cpu, mem) and the sources with their ages;
  - `fleetctl console watch [--json]`: NDJSON;
  - `fleetctl console term <agent>`: writes the watch bytes to stdout (raw);
  - `fleetctl console health`.
- [ ] `fleetctl attach <agent>`: when `console:` is configured for the agent's host, it puts
  the local terminal in raw mode (`golang.org/x/term`) and connects to
  `/v1/term/<agent>?control=1`. On 409 it prints `held by <holder>`. Otherwise it falls back
  to `docker exec -it`.
- [ ] Tests against an `httptest` console:
  - `TestConsoleSnapshotPrintsATable`;
  - `TestConsoleWatchPrintsNDJSON`;
  - `TestAttachReportsWhoHoldsControl`.
- [ ] Commit `fleetctl console: snapshot, watch, term, health; attach goes through the console`,
  push, report.

---

### Task 2A (agent-02): panel state and keys — branch `fleet/agent-02/panel-state`

**Files:** `fleet/console/src/state/store.ts`, `store.test.ts`; `src/keys.ts`, `keys.test.ts`;
`src/state/derive.ts`, `derive.test.ts`.

**Interfaces:**
- Consumes: Task 0F's `model.ts`, `useFleet`, `useKey`, `setControlHeld`.
- Produces: `derive.ts`:
  - `ageOf(at: string, now: number): number`;
  - `isStale(src: Source, cadenceS: number, now: number): boolean`;
  - `phaseLook(phase: string, alerts: string[]): {color: string, label: string, pulse: boolean}`;
  - `fmtAge(seconds: number): string` ("40 s", "6 h 11 m").

- [ ] `store.test.ts`:
  - `applies a snapshot then patches in seq order`;
  - `resubscribes on a seq gap and replaces state with the new snapshot`;
  - `marks conn "lost" after the socket closes and "live" again after reconnect`.

  Use a fake WebSocket class injected through `store.ts`'s `setSocketFactory`.
- [ ] `keys.test.ts`:
  - `while control is held, 1, R, O, Esc, T reach the terminal handler and no panel handler fires`;
  - `Ctrl+] releases and calls onRelease once`;
  - `outside control, 1-9 select, F focuses, Enter requests control`.
- [ ] `derive.test.ts`: the phase table from the spec, row for row (working amber, waiting
  cyan pulse "task open, idle", standby dim, cooling frost with reset, disabled grey lock, down
  red, stuck alert red banner); `isStale` at exactly 2× cadence is stale.
- [ ] `npm run check:dist` after `npm run build` → no fixture marker. Commit
  `panel: state, keys and the phase table`, push, report.

### Task 2B (agent-02): Overview — branch `fleet/agent-02/panel-overview`

**Files:** `src/overview/Overview.tsx`, `Header.tsx`, `Roster.tsx`, `AgentCard.tsx`,
`CenterTerminal.tsx`, `RightRail.tsx`, `EventStream.tsx`, `ActivityTrack.tsx`, `theme.css`,
with tests for the pure parts.

- [ ] Build the layout of mockup board A (canvas "Fleet Console Mockups") at 1920×1080. The
  root scales to fit the window, keeping the aspect ratio. Every value comes from `useFleet()`;
  nothing is hard-coded.
  - **Header:** host vitals, a ticking clock, and the browser's GPU string from `WEBGL_debug_renderer_info`.
  - **Agent cards:** a card for a working agent shows `current_tool.name` and its running
    time, ticking.
  - **Stale values:** a stale value renders as its age in dim text, never as the old number.
- [ ] `CenterTerminal` uses `<LiveTerminal fit="contain">` for the selected agent. A remote
  agent shows "remote · no live view".
- [ ] Tests (vitest + jsdom):
  - `AgentCard shows age instead of value when fleetd is stale`;
  - `Roster orders by agent id and marks remote agents`;
  - `ActivityTrack places a bucket at its time fraction`.
- [ ] Run `VITE_FIXTURE=1 npm run dev` and screenshot it with Playwright
  (`npx playwright screenshot`) into the report. Commit `panel: the overview`, push, report.

### Task 2C (agent-02): Focus and control — branch `fleet/agent-02/panel-focus`

- [ ] **Focus.** `F` or a click on the terminal: the side columns slide out (motion, about
  450 ms), and the terminal fills the width at 220 columns. `Esc` goes back.
- [ ] **Control.** `Enter` opens `openControl(agent)`:
  - the frame turns amber and a "YOU HAVE THE KEYBOARD" bar appears;
  - `setControlHeld(true, release)`;
  - every keystroke is sent as bytes by xterm's `onData`;
  - `Ctrl+]` releases (and tmux also detaches on it);
  - a 409 shows "held by <holder>".
- [ ] **Prompt** (`P`): an input bar; Enter calls `api.prompt`. It shows pending and then the
  result.
- [ ] **Rotate** (`R`): calls the preview text ("closes task X as rotated; takes effect at the
  next idle tick", from fleetd's reply), and requires holding R for 1 s with a filling ring.
- [ ] **Brief the orchestrator** (`M`): the same bar, sent to the orchestrator via `api.prompt`.
- [ ] Tests:
  - `control bar appears only after the socket says hello in control mode`;
  - `409 shows the holder`;
  - `rotate needs a full 1 s hold`.
- [ ] Commit `panel: focus, control, prompt and rotate`, push, report.

### Task 2D (agent-02): motion — branch `fleet/agent-02/panel-motion`

- [ ] **Boot sequence.** On first load, a full-screen log lists each source as it reports `ok`
  (from `snapshot.sources`), with real timestamps. It fades out after all have answered or
  after 6 s; sources that haven't answered stay listed in red.
- [ ] **Number tween:** 400 ms.
- [ ] **Event stream:** new entries slide in from the top.
- [ ] **Phase change:** the card flashes (amber 600 ms).
- [ ] **Activity:** ticks land with a short drop animation.
- [ ] **Background:** a scanline at 7 s per pass, and the grid.
- [ ] **Banners** for `down`, `cooling`, `disabled` and the `stuck` alert, stacked at the top
  and dismissed only by the condition clearing.
- [ ] **Reduced motion:** honour `prefers-reduced-motion`. RustDesk viewers may have it set;
  motion turns off, and the data stays the same.
- [ ] Test `banner clears when the alert clears`. Commit `panel: motion`, push, report.

---

### Task 3A (agent-03): desk painter — branch `fleet/agent-03/office-painter`

**Files:** `fleet/console/src/office/painter.ts`, `painter.test.ts`.

**Interfaces:**
- Produces: `createDeskScreen(agent: string, cols: number, rows: number): {canvas: HTMLCanvasElement | OffscreenCanvas, dirty: () => boolean, dispose(): void}`.
  - It subscribes with `subscribeTerm` and feeds an `@xterm/headless` Terminal.
  - On `onWriteParsed`, it marks itself dirty; `paint()` draws `buffer.active` cell by cell
    (fg/bg colours from the 256-colour palette and truecolour, bold, cursor) into a 2D canvas at
    8×16 px per cell.

- [ ] Tests (jsdom + `canvas` mock via `vi.fn` 2D context):
  - `paints a written line's characters at their cells` (assert `fillText` calls);
  - `marks dirty only after new data`;
  - `uses truecolour fg from SGR 38;2`.
- [ ] Commit `office: headless-xterm desk painter`, push, report.

### Task 3B (agent-03): the room — branch `fleet/agent-03/office-room`

**Files:** `src/office/Office.tsx`, `Room.tsx`, `Desk.tsx`, `Figure.tsx`, `Label.tsx`,
`layout.ts`, `layout.test.ts`.

- [ ] `layout.ts`: `deskLayout(agentIds: string[], orchestrator: string): {id, x, z, rotY}[]`.
  The orchestrator sits at the head (z = -6). Agents go in rows of up to 4, 3.2 m apart,
  centred. Test: 4 agents fit one row, 9 agents make 3 rows, and the layout is deterministic.
- [ ] **The room:** a dark floor with a fine cyan grid, low walls, a desk (top, legs, front
  panel), a laptop (base plus an open screen at 105°), a chair and a stylised figure. All of it
  is built from primitives, with `meshStandardMaterial` in the palette.
- [ ] **The screen** is a `CanvasTexture` from `createDeskScreen`. In `useFrame` it is
  repainted if dirty, at most 30 fps per desk. The orchestrator's screen is drawn by a small
  2D-canvas renderer of its quota and open tasks.
- [ ] **Phase looks:** per the spec, driven by `phaseLook`. Working adds a typing animation on
  the figure's arms; down turns the screen black and the lamp off.
- [ ] **Labels:** drei `<Html>` or `<Text>` — name, task, 5 h ring.
- [ ] Commit `office: the room, desks and screens`, push, report.

### Task 3C (agent-03): camera, control and performance — branch `fleet/agent-03/office-camera`

- [ ] **Camera:** a slow drift (a Lissajous of ±4°, 24 s); drag to orbit (drei
  `CameraControls`). A click on a desk flies in (1.2 s ease-in-out) to frame its screen. After
  the fly-in, an absolutely positioned DOM `<LiveTerminal fit="contain">` cross-fades over the
  screen's projected rectangle. `Enter` takes control, through the same path as the panel's
  Focus. `Esc` flies back out.
- [ ] **Relay lights:** for each new `fleet.relay` event, a light pulse travels along the floor
  from the sender's desk to the receiver's desk in 1.5 s. The orchestrator's desk is the
  endpoint for `operator` and the orchestrator.
- [ ] **Performance governor:** bloom (`@react-three/postprocessing`) at half resolution. A
  rolling 2 s fps average below 40 turns bloom off, and it stays off until a reload. The
  current fps is shown small in a corner, as a real measured value.
- [ ] Test `governor disables bloom after 2 s under 40 fps` (fake clock).
- [ ] `O` in Overview switches to the office and back. Commit
  `office: fly-in, relay lights, performance governor`, push, report.

---

### Task 4A (agent-04): fleetd — every capability over HTTP — branch `fleet/agent-04/fleetd-routes`

**Files:**
- Modify: `fleet/internal/daemon/daemon.go` (new methods, Row fields), `fleet/internal/ledger/ledger.go`.
- Modify: `fleet/internal/server/server.go`, `fleet/cmd/fleetctl/main.go`.
- Create: `fleet/internal/hostctl/hostctl.go` (argv for render/up/down moved out of `fleetctl`).
- Tests in each package.

**Interfaces:** produces exactly the JSON in Task 1A's list, plus:
- `GET /v1/peek?agent=&n=80` → `{"agent","text"}` (console, full);
- `POST /v1/prompt {agent,text}` → `{"ok":true}` (console, full);
- `GET /v1/archives/{id}` → the manifest plus `transcript_lines` (full);
- `DELETE /v1/archives?older=180d` → `{"removed":n}` (full);
- `POST /v1/render`, `/v1/up {hosts:[…]}`, `/v1/down {hosts:[…]}` → `{"ok":true,"output":"…"}` (full);
- the rotate reply: `{"ok":true,"closes":"<task or empty>","when":"next idle tick"}`.

- [ ] **`Fleet` methods.** Write a failing test per method, then implement it as a `Fleet`
  method; handlers only parse and call:
  - `Events(agent, after, kinds, limit)`, `Usage(by, since)`, `History(agent, n)`, `Quota()`,
    `Tasks(open bool, agent string, n int)`, `Activity(since, bucket)`, `Today(now)`
    (local-midnight `since`), `Peek(agent, n)`, `Prompt(agent, text)`.
  - Defaults live in the method.
- [ ] **Row:**
  - `Idle` stays and `idle_s` is added;
  - `model` is the last status line's model, stored in `reconcile.State`;
  - `alerts` holds the keys of `st.Alerted` that are active;
  - `recorded_at` is the tick time of the state.

  Test: `TestRowCarriesIdleSecondsModelAlertsAndRecordedAt`.
- [ ] **Events cursor:** `id > after`, ascending, `limit` (default 200, max 1000), `kinds` as
  a comma list, with prefix matching for entries that end in `.`. Test the boundary: two events
  in the same second are both returned exactly once across two pages.
- [ ] **Activity:** `SELECT agent, (at/bucket)*bucket, count(*) … GROUP BY 1,2`. Test with
  events across 3 buckets.
- [ ] **CLI:** switch `peek`, `prompt` and `archive ls/prune` to HTTP; `render`, `up` and
  `down` call `hostctl`, both directly on the host and through the route. Add `fleetctl tasks
  [--open]`, `fleetctl activity`, `fleetctl today`. Test each against `httptest`.
- [ ] Run `go test -race ./...` via `kbuild`. Commit `fleetd: every capability over HTTP`,
  push, report.

### Task 4B (agent-04): data fixes and the image — branch `fleet/agent-04/fleet-data`

- [ ] **Usage.** Find why `/v1/usage` is empty; evidence first.
  - Inside an agent: `env | grep OTEL_` (print names only, never values), and
    `curl -s -o /dev/null -w '%{http_code}' $OTEL_EXPORTER_OTLP_ENDPOINT/v1/logs` from the
    agent.
  - On fleetd, look for an `otel.*` event in the events table.
  - Fix the broken link and add a regression test where the link was code (for example
    `TestOTLPApiRequestLandsInUsage`, with a real captured payload). Report the cause.
- [ ] **PreToolUse:**
  - Add `PreToolUse` to `packaging/fleet/home/settings.json` hooks, calling `event.sh`.
  - `event.sh` records `{t, event, session, tool}` for Pre (no `ok`), with one `jq` and exit 0.
  - Test in `internal/agent/scripts_test.go`: `TestEventScriptRecordsPreToolUse`.
- [ ] **Status line:** `refreshInterval: 5` in `settings.json`.
- [ ] **tmux:** create `packaging/fleet/tmux.conf` with exactly:

```
set -g window-size manual
set -g default-size 220x50
set -g prefix None
set -g prefix2 None
unbind-key -a
unbind-key -a -T copy-mode
unbind-key -a -T copy-mode-vi
bind-key -n C-] detach-client
set -g status off
set -g escape-time 0
```

  - Install it as `/etc/tmux.conf` in `packaging/fleet/Dockerfile`.
  - Test in `scripts_test.go` with a real tmux: `TestTmuxConfPinsSizeAndHasNoPrefix`.
- [ ] **Release:** build the image with `kbuild`, then report "image ready" with its tag. The
  lead rolls it out (`fleetctl render && fleetctl up`), because restarting agents restarts
  you.
- [ ] Commit `fleet: usage fixed, PreToolUse, 5 s status line, tmux.conf`, push, report.

### Task 4C (agent-04): console config rendering — branch `fleet/agent-04/console-render`

- [ ] `render.Render`, for the host named by `console.host`, writes `<dir>/<host>/console/console.yaml`
  (a `console.Config`) and `<dir>/<host>/console/fleetd.token` (0600).
  - `agents[]`: every roster agent; `live` = agent.Host == console.host; `exec` = that host's
    exec prefix as seen from the console's host, which is `[docker]` for its own agents.
  - `fleetd_url` = `console.fleetd_url`.
  - The token file paths point at `console.dir`.
- [ ] Tests:
  - `TestRenderWritesConsoleConfigForItsHost`, which loads the output with
    `console.LoadConfig`;
  - `TestRenderSkipsConsoleWhenNotConfigured`;
  - `TestConsoleTokenFileIsPrivate`.
- [ ] Update `packaging/fleet/fleet.yaml.example` and `README.md` with the `console:` section.
  Commit `fleet render: the console's config and token`, push, report.

### Task 4D (agent-04): PC2 host packaging — branch `fleet/agent-04/console-packaging`

**Files:** `packaging/console/install.sh`, `fleet-console.service`, `fleet-kiosk.service`,
`kiosk-session.sh`, `edid/make-edid.py`, `README.md`, `console.env.example`.

- [ ] **`install.sh`** is idempotent and runs as the kiosk user. Steps that need root go
  through `sudo`, one function each. Every value comes from `console.env` (connector, mode,
  user, Chrome and RustDesk versions, console dir).
  1. **`check`:** `/sys/class/drm/*-<conn>/status` exists, `/dev/dri/renderD*` exists, the
     user is in `video` and `render`.
  2. **`edid`:** `make-edid.py 1920 1080 60 > /lib/firmware/edid/fleet-<w>x<h>.bin`. The EDID
     1.3 block has a valid checksum (tested in Python, `test_make_edid.py`). An initramfs hook
     copies it.
  3. **`cmdline`:** back up `/etc/default/grub`; add
     `video=<conn>:<w>x<h>@<hz>e drm.edid_firmware=<conn>:edid/<file>` once (detect
     `drm_kms_helper.edid_firmware` vs `drm.edid_firmware` from `modinfo`); run `update-grub`
     and `update-initramfs -u`; print the undo line.
  4. **`packages`:**
     - `xserver-xorg-core`, `xserver-xorg-video-modesetting` (or the core driver), `xinit`,
       `x11-xserver-utils`, `unclutter`;
     - Google Chrome from its apt repo;
     - RustDesk's pinned `.deb`, its sha256 checked against `console.env`.
  5. **`units`:**
     - `fleet-console.service` is a user service: `ExecStart=%h/.local/bin/fleet-console
       --config <dir>/console.yaml`, `Restart=always`, with lingering enabled.
     - `fleet-kiosk.service` is a system service running `getty@tty1`-style autologin into
       `kiosk-session.sh`. That script runs `xset s off -dpms`, `unclutter`, then a loop:
       `google-chrome --kiosk --no-first-run --noerrdialogs --disable-session-crashed-bubble
       --ignore-gpu-blocklist "http://<listen>/login?k=$(cat <dir>/client.token)"`.
  6. **`rustdesk`:** enable the service and point it at `:0` with the kiosk's `XAUTHORITY`. With
     `console.env` `RUSTDESK_MODE=direct`, set the RustDesk options for direct IP access only.
  7. **`token`:** generate `<dir>/client.token` (32 random bytes, hex, 0600) if it is missing.
- [ ] Use `shellcheck` on all scripts. The `--dry-run` flag prints every command without
  running it; test that through `bats` or a shell test in `packaging/console/test.sh`, which
  asserts the dry-run output contains the GRUB line and the undo line.
- [ ] `README.md`: what it does, the reboot, how to undo from GRUB, the dummy plug fallback,
  RustDesk's password step (`sudo rustdesk --password …`, typed by the operator).
- [ ] Commit `packaging/console: PC2 kiosk, virtual display, RustDesk`, push, report.

---

## Lead integration (after each report)

Per branch:
1. Read the diff.
2. Run its tests via `kbuild`.
3. Merge to `main` (`git merge --no-ff`), push.
4. Note friction in `docs/superpowers/fleet-friction.md`.

Then:
- **Release:** after 4B, roll out the image. After 4C and 1B, build `fleet-console` (`npm ci &&
  npm run build` in `fleet/console`, then `go build ./cmd/fleet-console`), copy it to PC2, run
  `install.sh` (root steps with the PC2 sudo password from `~/.karmax/secrets/pc2_sudo_pass`),
  and reboot PC2 once for the display.
- **Checks:** Playwright and acceptance per the spec. Then a final whole-branch review by a
  fresh reviewer.
