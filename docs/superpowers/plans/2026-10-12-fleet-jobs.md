# Fleet jobs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development for every step.
> Each track is one agent's task, on its own branch `fleet/<agent>/<topic>` from `origin/main`.
> Steps use checkbox (`- [ ]`) syntax.

**Goal:** Any Claude session on Kali, and KARMAX's orchestrator, hands work to the fleet with one command
and reads the result back by task id — every operation one daemon function, exposed over HTTP and CLI.

**Architecture:** New daemon functions in their own files under `fleet/internal/daemon/`, each wrapped by a
fleetd route in `fleet/internal/server/server.go` (added to the `Ops` interface) and by a `fleetctl`
command in its own file under `fleet/cmd/fleetctl/`, dispatched from one `case` line in `main.go`'s switch.
Reports are tied to tasks in the ledger (`relays.task`), so the task id is the caller's handle.

**Tech Stack:** Go (module `github.com/MelloB1989/karmax/fleet`), SQLite ledger (modernc), `net/http`
mux with method patterns, `go:embed`.

**Spec:** `docs/superpowers/specs/2026-10-12-fleet-jobs-design.md` — read it first; it is binding.

## Global Constraints

- Every operation: one daemon function → one HTTP route → one CLI command calling that route. No logic in
  the CLI beyond flags, reading the brief, printing and exit codes.
- Scopes: every new route is `fullOnly`, except `GET /v1/pick`, `GET /v1/tasks/{id}`, `GET /v1/guide`
  which are `readers` (full + console).
- Exit codes: 0 ok, 1 error, 3 agent down/cooling/disabled, 4 timeout, 5 no free agent.
- HTTP: 404 unknown task (`daemon.ErrNoTask`), 409 conflict (no free agent, agent busy, task closed),
  400 bad input. Add a `conflict` error type in server.go next to `notFound`/`forbidden` (track B adds
  it; tracks A and C use it only through `daemon.ErrConflict` — see Interfaces).
- Ledger changes only through `migrate()`'s `adds` list.
- Tests: `kbuild go test -race ./...` from `fleet/` must pass; `go vet ./...` clean.
- The repo is public: no names, hosts, IPs, tokens or paths of the operator in code, tests or docs. Tests
  use `agent-01`…, `kali`/`pc2`, `/code`.
- Commits end with `Co-Authored by MelloB's coding agent <build@mellob.in>`. Never rebase a reported branch.

## Review Focus

1. Two `run`s at the same instant (two Claude sessions) → different agents, or one gets 409. Never both
   on one agent. (Track B test `TestRunConcurrentPicksDiffer`.)
2. An agent that reports to `karmax` (not the operator) → `wait --task` and `result` still see the report.
   (Track A test `TestWaitByTaskSeesReportToOrchestrator`.)
3. An agent reports after its task was closed by `done` → the report still attaches to that task (the
   state keeps `Task` until the next assign). (Track A test `TestReportAfterDoneAttaches`.)
4. `cancel` on an already-closed task → 409, nothing typed into the agent. (Track C test.)
5. A brief from stdin with shell metacharacters/newlines arrives byte-for-byte. (Track B CLI test.)

## Shared interfaces (all tracks read this)

```go
// daemon (errors)
var ErrNoTask = …                                  // exists
var ErrConflict = errors.New("conflict")           // track B adds in daemon/run.go; A and C wrap it:
                                                   //   fmt.Errorf("%w: task %s is closed", ErrConflict, id)
// ledger
type Relay struct { …; Task string `json:"task,omitempty"` }   // track A
type Task  struct { …; Repo string `json:"repo,omitempty"` }   // track B
func (db *DB) OpenTask(id, agent, assigner string, at time.Time) error  // unchanged signature
func (db *DB) SetTaskRepo(id, repo string) error                         // track B
func (db *DB) TaskReports(task string, after int64) ([]Relay, error)     // track A: relays with task=?, id>after, oldest first
func (db *DB) LastRepo(agent string) (string, error)                     // track B: repo of the agent's most recent task, "" if none

// daemon
func (f *Fleet) TaskAgent(task string) (*ledger.Task, error)            // track A: the task row, ErrNoTask if unknown
func (f *Fleet) Result(task string) (TaskResult, error)                  // track A
func (f *Fleet) Wait(ctx, agent, task string, after int64) (WaitResult, error) // track A: agent may be "" if task != ""
func (f *Fleet) Pick(repo string, now time.Time) PickReply               // track B
func (f *Fleet) Run(ctx context.Context, in RunRequest, now time.Time) (RunReply, error) // track B
func (f *Fleet) Followup(ctx context.Context, task, from, text string, now time.Time) error // track C
func (f *Fleet) Cancel(ctx context.Context, task, from, reason string, now time.Time) error // track C
// Box gains: Interrupt(ctx context.Context) error   // track C: tmux send-keys -t <pane> Escape

// guide (new package fleet/internal/guide)
func Text() string   // track D: the embedded guide.md
func Skill() string  // track D: SKILL.md = frontmatter + Text()
```

Each track adds its own methods to `server.Ops` and to the server tests' fake. Expect trivial merge
conflicts in `server.go`, `main.go`, the fakes and `migrate()`'s list; the lead resolves them.

---

### Track A (agent-01): reports belong to tasks; `result`; `wait` by task

**Files:**
- Modify: `fleet/internal/ledger/ledger.go` (migration `relays.task`, `Relay.Task`, `AddRelay`, `Inbox` reads task, `TaskReports`)
- Modify: `fleet/internal/daemon/daemon.go` (`Tell` attaches the sender's task; `Wait`)
- Create: `fleet/internal/daemon/result.go` (`TaskAgent`, `TaskResult`, `Result`)
- Modify: `fleet/internal/server/server.go` (`GET /v1/tasks/{id}`, wait route agent optional)
- Create: `fleet/cmd/fleetctl/result.go` (`result`); modify `main.go` (`case "result"`, `wait` agent optional, help text)
- Tests: `ledger_test.go`, `daemon/result_test.go`, `server_test.go`, `cmd/fleetctl/main_test.go`

**Behavior:**
- `Tell`: when `from` is a known agent whose state has `Task != ""`, the stored relay's `Task` is that id —
  on the operator-inbox path and on the relayed path (recorded in `AddRelay` there too).
- `TaskResult{Task ledger.Task; State string /*open|closed*/; Reports []ledger.Relay; Commits []string;
  Ahead int; CommitsError string}` with JSON names `task,state,reports,commits,ahead,commits_error`.
- Commits: when `Repo` and `Branch` are set, run `git -C <cfg.Code>/<repo> log --format=%h %s main..<branch>`
  (through an injectable `f.git func(ctx, dir string, args ...string) (string, error)` so tests fake it;
  timeout 10 s). Reject a repo containing `..` or `/`-leading paths.
- `Wait` with task: resolve agent if empty (ErrNoTask if unknown); messages = `TaskReports(task, after)`
  first one wins (reason `message`); then closed/phase/timeout as today. Without a task: unchanged.
- `GET /v1/tasks/{id}` → `Result`; 404 unknown.
- `fleetctl result <task> [--json]`: human form prints the task line, state, each report
  (`[<time>] <from> → <to>:` then the text), then commits.
- `fleetctl wait --task X` works with no agent.

- [ ] **Step 1: failing tests** — ledger: migration on an old DB adds `relays.task`; `TaskReports` order and
  `after`. daemon: `TestReportAttachesToTask` (operator path), `TestRelayAttachesToTask` (relay path, fake
  box), `TestReportAfterDoneAttaches`, `TestWaitByTaskSeesReportToOrchestrator`, `TestWaitByTaskNoAgent`,
  `TestResultCommits` (fake git), `TestResultNoBranch`, `TestResultRejectsRepoTraversal`. server: 404 on
  unknown task; console token may read; relay token may not. CLI: `wait --task` without agent sends no
  `agent` param.
- [ ] **Step 2:** run, watch them fail for the right reason.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `kbuild go test -race ./...` and `go vet ./...` green.
- [ ] **Step 5:** commit, push, report to the assigner with the branch.

### Track B (agent-02): `pick` and `run`

**Files:**
- Modify: `fleet/internal/ledger/ledger.go` (migration `tasks.repo`, `Task.Repo` read in `Task`/`Tasks`, `SetTaskRepo`, `LastRepo`)
- Create: `fleet/internal/daemon/run.go` (`ErrConflict`, `PickReply`, `Candidate`, `Pick`, `RunRequest`, `RunReply`, `Run`, `taskID`)
- Modify: `fleet/internal/daemon/daemon.go` (a `runMu sync.Mutex` on `Fleet`)
- Modify: `fleet/internal/server/server.go` (`conflict` error type mapping `daemon.ErrConflict` → 409 with the
  error's JSON detail; `POST /v1/run`, `GET /v1/pick`)
- Create: `fleet/cmd/fleetctl/run.go` (`run`, `pick`); modify `main.go` (cases, help text)
- Tests: `daemon/run_test.go`, `server_test.go`, `cmd/fleetctl/main_test.go`

**Behavior:** exactly the spec's "run" and "pick" sections.
```go
type Candidate struct { Agent string `json:"agent"`; Eligible bool `json:"eligible"`; Why string `json:"why"`
  FiveHour float64 `json:"five_hour_pct"`; SevenDay float64 `json:"seven_day_pct"`; IdleS int64 `json:"idle_s"` }
type PickReply struct { Agent string `json:"agent"`; Why string `json:"why"`; Candidates []Candidate `json:"candidates"` }
type RunRequest struct { Brief, Repo, Agent, From, Task string; Queue time.Duration }
  // JSON: brief, repo, agent, from, task, queue ("30m")
type RunReply struct { Task string `json:"task"`; Agent string `json:"agent"`; Why string `json:"why"` }
type NoFreeAgent struct { Candidates []Candidate } // error; Unwrap() == ErrConflict; server returns its candidates in the 409 body
```
- `Run`: under `f.runMu`: pick (or check `in.Agent` is free) → id → `assign` (existing) → `SetTaskRepo` →
  release the lock → `Tell(from, agent, composed brief)`. If the tell fails, close the task as
  `undelivered` and return the error. `From` defaults to `operator`. `Queue`: retry the pick every
  `f.waitEvery` until it succeeds, the queue time passes, or ctx ends.
- `fleetctl run`: brief from args, `-` (stdin) or `--brief-file`; prints `task <id> → <agent> (<why>)`;
  `--json` prints the reply; on 409 prints the candidates table and exits 5; `--wait` then calls
  `GET /v1/wait?agent=<agent>&task=<id>&timeout=<t>` and prints the report like `wait` does (exit 3/4 as wait).
- `fleetctl pick`: table of candidates, chosen first; exit 5 when none eligible.

- [ ] **Step 1: failing tests** — `TestPickPrefersRepoAffinity`, `TestPickQuotaThenIdle`,
  `TestPickExcludes` (table: working, waiting-with-open-task, cooling, down, disabled, low, reserved),
  `TestRunConcurrentPicksDiffer` (two goroutines, 2 free agents → 2 agents; 1 free → one 409),
  `TestRunNamedBusyAgent409`, `TestRunComposesBrief` (fake box records the relayed text),
  `TestRunUndeliveredClosesTask`, `TestTaskIDShape`, `TestRunQueueWaitsForFree`; server: 409 body carries
  candidates; scopes. CLI: stdin brief byte-for-byte; exit 5; `--wait` path.
- [ ] **Step 2:** run, watch them fail.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `kbuild go test -race ./...`, `go vet ./...` green.
- [ ] **Step 5:** commit, push, report.

### Track C (agent-03): `followup`, `cancel`, `done --task`

**Files:**
- Create: `fleet/internal/daemon/followup.go` (`Followup`, `Cancel`)
- Modify: `fleet/internal/daemon/daemon.go` (`Box` gains `Interrupt`), `fleet/internal/agent/agent.go`
  (`Interrupt`: `tmux send-keys -t <Pane> Escape`), every fake Box in tests
- Modify: `fleet/internal/server/server.go` (`POST /v1/tasks/{id}/followup`, `POST /v1/tasks/{id}/cancel`;
  `/v1/done` resolves the agent from the task when `agent` is empty)
- Create: `fleet/cmd/fleetctl/followup.go` (`followup`, `cancel`); modify `main.go` (`done` agent optional with `--task`)
- Tests: `daemon/followup_test.go`, `agent` test, `server_test.go`, `cmd/fleetctl/main_test.go`

**Behavior:** the spec's "followup, cancel, done by task" section. Look the task up with
`f.db.Task(id)` (ErrNoTask on `sql.ErrNoRows`); closed → `fmt.Errorf("%w: task %s is closed", ErrConflict, id)`.
Until track B lands, declare `ErrConflict` in `followup.go` guarded by a comment; the lead dedups on merge.
Cancel order: Interrupt → relay the notice → `Close(agent, task, "cancelled", "", "")`. An interrupt
failure is reported but the task is still closed.

- [ ] **Step 1: failing tests** — `TestFollowupRelaysToTaskAgent` (text shape), `TestFollowupClosedTask409`,
  `TestFollowupUnknownTask404`, `TestCancelInterruptsTellsCloses` (order recorded by the fake box),
  `TestCancelClosedTaskDoesNothing`, `TestDoneByTaskResolvesAgent`, agent: `Interrupt` sends `Escape` to the pane.
- [ ] **Step 2:** run, watch them fail.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `kbuild go test -race ./...`, `go vet ./...` green.
- [ ] **Step 5:** commit, push, report.

### Track D (agent-04): the guide, the skill, the orchestrator's paragraph

**Files:**
- Create: `fleet/internal/guide/guide.md`, `fleet/internal/guide/guide.go` (`//go:embed guide.md`; `Text`, `Skill`)
- Modify: `fleet/internal/server/server.go` (`GET /v1/guide` → `{"text": guide.Text()}`, readers)
- Create: `fleet/cmd/fleetctl/guide.go` (`guide`; `skill install [--dir D] [--print]`, default dir
  `$HOME/.claude/skills`, writes `<dir>/fleet/SKILL.md` 0644, creating dirs 0755, prints the path; refuses
  to overwrite a file whose first line is not `---` unless `--force`)
- Modify: `fleet/internal/render/orchestrator.md.tmpl` (use `run`, `result`, `followup`, `cancel`, `done --task`)
- Modify: `fleet/internal/render/brief.md.tmpl` (one line: every message you send while you have a task is
  filed under it; answer follow-ups; on a cancel notice stop)
- Modify: `docs/AGENT-FLEET.md` and `packaging/fleet/README.md` (a short "Putting the fleet to work from
  any Claude session" section: `fleetctl skill install`, then the loop)
- Tests: `guide/guide_test.go`, `cmd/fleetctl/main_test.go`, render golden tests if present

**The guide's content** (write it for a Claude session that has never seen the fleet; ≤ 250 lines):
1. What the fleet is (N standby Claude Code agents, each with its own subscription, sharing the code at the
   same path) and when to delegate (independent, well-specified work: a bug with a repro, a feature with a
   spec, tests, reviews; not: work needing this session's context, or tiny edits).
2. Writing a brief: goal, repo, files, acceptance test, constraints, what to report. One task per agent.
3. The loop: `fleetctl run --repo R "<brief>"` → `fleetctl wait --task <id>` (or `run --wait`) →
   `fleetctl result <id>` → review the branch (`git log -p main..<branch>`, run the tests) → merge with
   `&&` gating, or `fleetctl followup <id> "<what to fix>"` → `fleetctl done --task <id>`.
4. Fan-out: several `run`s without `--wait`, collect the ids, then `wait` each; exit 5 means all busy (use
   `--queue 30m` or wait).
5. Questions from an agent arrive as reports; answer with `followup`. Stop work with `cancel`.
6. Looking around: `fleetctl ls`, `pick`, `peek <agent>`, `tasks --open`, `quota`.
7. Rules: delegated work is untrusted until its diff is read and tests pass; never print secrets or env
   values; never rebase/force-push a reported branch; don't hand an agent work that needs secrets.
8. Exit codes table and the HTTP equivalents (`POST /v1/run`, …) for scripts.
- `Skill()` frontmatter: `name: fleet`, `description:` "Use when work can be delegated to the standby
  Claude Code agent fleet — parallel or independent coding tasks, 'use the fleet/agents', fan-out, or
  long-running work — to run, wait for, review and merge agent tasks with fleetctl."

- [ ] **Step 1: failing tests** — `Skill()` starts with `---\nname: fleet\n`, contains `Text()`;
  `Text()` mentions every command in the spec's table (test iterates the list); `skill install --dir tmp`
  writes the file 0644 and refuses an unrelated existing file without `--force`; `GET /v1/guide` scopes;
  orchestrator template renders and contains `fleetctl run`.
- [ ] **Step 2:** run, watch them fail.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `kbuild go test -race ./...`, `go vet ./...` green.
- [ ] **Step 5:** commit, push, report.

### Lead (after the four reports)

- [ ] Merge A, B, C, D into main in that order, resolving the shared-file conflicts; `go test -race ./...` && push.
- [ ] Rebuild and restart fleetd and `fleetctl` on Kali; `fleetctl render` (orchestrator paragraph, briefs).
- [ ] `fleetctl skill install`.
- [ ] End-to-end: from a fresh Claude Code session on Kali (`claude -p`), with only the skill as guidance,
  run a small real task on the live fleet and read it back with `result`; then `done --task`.
- [ ] Friction log entries for anything that got in the way.
