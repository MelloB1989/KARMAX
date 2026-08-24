# Org platform — the build contract

Every worker on this branch codes against THIS FILE. It fixes the schema, the Go
signatures and the recipe syntax so nine people can build at once without
meeting in the middle. If something here is wrong, say so — do not quietly
diverge, because the next worker is writing a caller against it.

Plan: https://claude.ai/code/artifact/5f8c7ef6-12ce-41e1-b6b7-70fd80fee38c

## File ownership

One owner per path. Do not edit a path you do not own.

| Owner | Paths |
|---|---|
| foundation | `internal/store/migrations.go`, `pkg/loopkit/kit.go`, `internal/recipes/recipe.go` (verb constants only), `internal/sandbox/driver.go` |
| stores | `internal/store/case_store.go`, `audit_store.go`, `waiter_store.go`, `sandbox_run_store.go`, `directory_store.go` |
| recipes | `internal/recipes/*.go` EXCEPT the verb constants block |
| sandbox | `internal/sandbox/*` except `driver.go`, `packaging/sandbox/*` |
| jira | `internal/connectors/jira/*` |
| github | `internal/connectors/github/*` |
| slack | `internal/comms/slack/*` |
| console | `app/console/*` |
| compiler | `internal/recipegen/*` |
| pack | the `karmax-loops` repo |

`internal/runtime/loophost_org.go` is the foundation's; it implements the new
Kit methods against the stores.

## Schema

Added to `migrations.go` by the foundation. Canonical SQLite dialect — the
dialect layer translates it (see `internal/store/dialect.go`).

    cases(id PK, org, agent, ckey UNIQUE, title, state, namespace,
          thread_channel, thread_ts, created_at, updated_at)
    case_events(id PK, case_id, kind, payload, actor, created_at)
    waiters(id PK, execution_id, loop, step, case_id, event_kind, match_json,
            expires_at, created_at, resolved_at, result_json)
    sandbox_runs(id PK, case_id, driver, container_id, image, status, repo,
                 branch, task, started_at, finished_at, exit_code, error, log_tail)
    audit_events(id PK, actor_kind, actor_id, agent, case_id, recipe, step,
                 verb, target, decision, detail, created_at)
    directory(external_kind, external_id, member, org, name, PRIMARY KEY(external_kind, external_id))

`state` on a case is free text; the packs use `open | grooming | ready |
building | review | done | dropped`.

## Store API — exact signatures

Implement these EXACTLY. `internal/runtime/loophost_org.go` is already written
against them.

```go
// case_store.go
type Case struct {
    ID, Org, Agent, Key, Title, State, Namespace string
    ThreadChannel, ThreadTS                      string
    CreatedAt, UpdatedAt                         time.Time
}
type CaseEvent struct {
    ID, CaseID, Kind, Payload, Actor string
    CreatedAt                        time.Time
}
func (s *Store) OpenCase(c Case) (Case, error)            // upsert on Key; returns stored row
func (s *Store) CaseByKey(key string) (Case, bool, error)
func (s *Store) CaseByID(id string) (Case, bool, error)
func (s *Store) SetCaseState(id, state string) error
func (s *Store) BindCaseThread(id, channel, ts string) error
func (s *Store) ListCases(agent, state string, limit int) ([]Case, error)
func (s *Store) AppendCaseEvent(e CaseEvent) error
func (s *Store) CaseHistory(caseID string, limit int) ([]CaseEvent, error)

// audit_store.go
type AuditEvent struct {
    ID, ActorKind, ActorID, Agent, CaseID string
    Recipe, Step, Verb, Target, Decision, Detail string
    CreatedAt time.Time
}
type AuditFilter struct {
    ActorID, Agent, CaseID, Verb string
    Since                        time.Time
    Limit                        int
}
func (s *Store) AppendAudit(e AuditEvent) error
func (s *Store) QueryAudit(f AuditFilter) ([]AuditEvent, error)

// waiter_store.go
type Waiter struct {
    ID, ExecutionID, Loop, Step, CaseID, EventKind, MatchJSON string
    ExpiresAt                                                 *time.Time
    CreatedAt                                                 time.Time
    ResolvedAt                                                *time.Time
    ResultJSON                                                string
}
func (s *Store) ArmWaiter(w Waiter) error
func (s *Store) PendingWaiters(eventKind string) ([]Waiter, error)
func (s *Store) ResolveWaiter(id, resultJSON string) (bool, error) // false if already resolved
func (s *Store) WaiterResult(executionID, step string) (string, bool, error)
func (s *Store) ExpireWaiters(now time.Time) ([]Waiter, error)

// sandbox_run_store.go
type SandboxRun struct {
    ID, CaseID, Driver, ContainerID, Image, Status string
    Repo, Branch, Task, Error, LogTail             string
    ExitCode                                       int
    StartedAt                                      time.Time
    FinishedAt                                     *time.Time
}
func (s *Store) StartSandboxRun(r SandboxRun) error
func (s *Store) UpdateSandboxRun(id, status string, exitCode int, errMsg, logTail string) error
func (s *Store) LiveSandboxRuns() ([]SandboxRun, error)   // status running/starting — restart reconcile
func (s *Store) SandboxRun(id string) (SandboxRun, bool, error)
func (s *Store) ListSandboxRuns(caseID string, limit int) ([]SandboxRun, error)

// directory_store.go
type Member struct{ ExternalKind, ExternalID, Member, Org, Name string }
func (s *Store) MapMember(m Member) error
func (s *Store) MemberByExternal(kind, id string) (Member, bool, error)
func (s *Store) ListDirectory(kind string) ([]Member, error)
```

Follow the package's house style: `s.mu.Lock()/RLock()`, `s.exec/query/queryRow`,
canonical SQLite SQL with `?` placeholders. Never `s.db.*` directly.

## Kit additions (`pkg/loopkit/kit.go`)

Already added by the foundation. Loops consume Kit; they do not implement it, so
growing it is safe.

```go
type AwaitSpec struct {
    Event   string            // event kind, e.g. "jira.issue.updated"
    Match   map[string]string // payload field -> expected value; all must match
    CaseID  string
    Timeout time.Duration     // 0 = no timeout
}
type SandboxSpec struct {
    CaseID, Repo, Branch, Task, Image string
    Env                               map[string]string
    Timeout                           time.Duration
}
type SandboxResult struct {
    RunID, Status, LogTail string
    ExitCode               int
}
type Case struct {
    ID, Key, Agent, Title, State, Namespace string
    ThreadChannel, ThreadTS                 string
}

// on Kit:
CaseOpen(agent, key, title string) (Case, error)   // agent names the PACK
CaseSay(ctx context.Context, caseID, channel, text string) error
CaseGet(key string) (Case, bool, error)
CaseSetState(caseID, state string) error
CaseLog(caseID, kind, payload string) error
CaseHistory(caseID string, limit int) ([]string, error)
Await(ctx context.Context, id string, spec AwaitSpec) (map[string]any, error)
SendTo(ctx context.Context, channel, thread, content string) error
ProposeTo(role, title, summary, action string) (string, error)
Sandbox(ctx context.Context, id string, spec SandboxSpec) (SandboxResult, error)
Audit(verb, target, decision, detail string) error
```

`Await` blocks the run durably: it arms a waiter, returns the stored result if
the step already resolved, and otherwise ends the run with `ErrSuspended` so the
bus redelivers when the waiter fires. `Sandbox` is `Step`-checkpointed.

## Sandbox driver (`internal/sandbox/driver.go`)

```go
type Spec struct {
    Image, Repo, Branch, Task string
    Env                       map[string]string
    Timeout                   time.Duration
}
type Status struct {
    ID, State, LogTail string   // State: starting|running|exited|failed|gone
    ExitCode           int
}
type Driver interface {
    Name() string
    Launch(ctx context.Context, s Spec) (id string, err error)
    Poll(ctx context.Context, id string) (Status, error)
    Logs(ctx context.Context, id string, tail int) (string, error)
    Kill(ctx context.Context, id string) error
}
```

## Recipe syntax

New verbs. Keep the existing shape: one verb per step, `as:` to bind a result,
`when:` to guard.

```yaml
- case.open:  { key: "jira:{{ .ticket }}", title: "{{ .summary }}" }
  as: c
- case.state: { case: "{{ .c.id }}", state: prioritized }
- case.log:   { case: "{{ .c.id }}", kind: note, payload: "asked for repro" }

- await:
    event: jira.issue.updated
    match: { key: "{{ .ticket }}", status: Prioritized }
    timeout: 168h
  as: moved

- foreach:
    in: "{{ .tickets }}"
    as: t
    steps:
      - ask: "summarise {{ .t }}"

- sandbox:
    case: "{{ .c.id }}"
    repo: acme/api
    branch: main
    task: "Implement {{ .ticket }}: {{ .summary }}"
    timeout: 45m
  as: build

- send: { to: "#eng", thread: "{{ .c.thread_ts }}", text: "PR is up" }
- propose: { to_role: senior-dev, title: "Merge?", summary: "...", action: "..." }
```

`send` keeps its old `to:`-only form working (WhatsApp target) — a bare `to:`
that is not a channel id routes as before.

## Rules for everyone

- `go build ./...` and `go test ./...` must pass when you finish. Run them.
- Comments are SPARSE — one line, explaining WHY. Read neighbouring files and
  match the voice. Never restate the code.
- No new dependency without saying so in your report.
- Every new store method gets a test in the package's existing style.
- Do not commit. Do not push.

## Amendments made during the build

Four seams only showed up once the pieces met. All four are closed:

1. **A connector may name its own event kind.** GitHub delivers every event type
   to ONE webhook URL, so a connector cannot mount a path per kind. `Host.publish`
   now prefers `payload["kind"]` over the source's fixed `EventKind`, which is
   what lets a recipe write `await: { event: github.pr.opened }` instead of
   awaiting `github.event` and matching on a field.

2. **`case.open` takes the PACK's name.** It used to record the loop that opened
   it, so six workflows produced six agents and the notify recipe's
   `agent == dev-agent` filter never matched anything.

3. **`case.say` and thread binding.** `BindCaseThread` had no caller, so
   `thread_ts` stayed empty forever and every workflow started its own
   conversation. The first `case.say` on a case now becomes its thread and every
   later one joins it. A channel that cannot thread just posts.

4. **`case.history` exists.** The Kit had `CaseHistory` and the verb list did
   not, so a recipe could not read what had already happened on its own case.
