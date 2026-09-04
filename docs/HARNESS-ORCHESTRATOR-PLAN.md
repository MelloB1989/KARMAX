# Harness orchestrator — implementation plan

Companion to `HARNESS-ORCHESTRATOR.md`, which holds the architecture and the
measurements. This is the build order, file by file, with the exact shapes
involved and what proves each step.

---

## 0. Protocol facts this plan depends on

All captured from the installed CLI, not assumed.

### Invocation

```
claude --print                       \
       --input-format  stream-json   \   # persistent: one JSON event per line on stdin
       --output-format stream-json   \
       --verbose                     \   # required for stream-json to emit anything
       --dangerously-skip-permissions\
       --session-id <uuid>           \   # NEW session (mint it ourselves so we can resume)
       --model <model>               \
       [--resume <uuid>]                 # instead of --session-id, to revive
```

`--session-id` and `--resume` are mutually exclusive; the CLI rejects both.

### Input event (one line, then flush)

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
```

### Output events, in the order they arrive

| `type` | carries | we use it for |
|---|---|---|
| `system` (`subtype:init`) | `cwd`, `session_id`, `capabilities`, version | confirm the session is live; record the real session id |
| `rate_limit_event` | `rate_limit_info` | **the budget breaker** — see below |
| `assistant` | `content[]` with `text` and `tool_use{name,id,input}` | audit trail, ToolCallRecords |
| `user` | `content[]` with `tool_result{tool_use_id,is_error}` | tool outcome for the audit |
| `result` | `total_cost_usd`, `usage`, `modelUsage`, `num_turns`, `duration_ms`, `is_error`, `api_error_status` | end of turn, ledger row |

A turn is complete when `result` arrives. That is the only reliable delimiter.

### `rate_limit_info` — the authoritative quota signal

```json
{"status":"allowed_warning","rateLimitType":"seven_day","utilization":0.88,
 "resetsAt":1788019200,"isUsingOverage":false,"surpassedThreshold":0.75,
 "unifiedWindows":{"five_hour":{"utilization":0.12,"resetsAt":1787981400},
                   "seven_day":{"utilization":0.88,"resetsAt":1788019200}}}
```

This removes the need to estimate a share from our own ledger. The harness tells
us, every turn, how much of both windows is gone. The ledger still gets written —
it is what `karmax cost` reports and what attributes spend to a *kind* — but the
breaker trips on `utilization`, which is fact rather than inference.

**Observed at time of writing: `seven_day` was already at 0.88.**

---

## 1. `internal/harness` — the supervisor

New package. Nothing in it knows what a chat is.

### 1.1 `session.go`

```go
type Kind string // "chat" | "agent" | "loop" | "task"

type Policy struct {
    Model       string        // --model
    Idle        time.Duration // close after this long with no send
    MaxTurns    int           // per-session circuit breaker
    TurnTimeout time.Duration // a single send may not exceed this
    MaxCostUSD  float64       // per-session circuit breaker
    Ephemeral   bool          // delete the transcript on close
}

type Session struct {
    Key       string // caller's handle, e.g. "chat:9039…@lid"
    Kind      Kind
    ID        string // the CLI's uuid, for --resume
    // …pid, cmd, stdin, a reader goroutine, mutexes
}

type Turn struct {
    Text      string
    ToolCalls []ToolCall            // from assistant/tool_use
    Usage     Usage                 // from result
    CostUSD   float64
    Limits    *RateLimit            // from rate_limit_event, if seen
    Err       error
}
```

`Send` writes one user event, then reads events until `result`, assembling the
`Turn`. One in-flight send per session, guarded by a mutex — the protocol has no
request ids, so interleaving two sends on one process is unparseable.

### 1.2 `supervisor.go`

```go
func (s *Supervisor) Open(ctx, key string, kind Kind) (*Session, error)
func (s *Supervisor) Send(ctx, key, text string) (Turn, error)
func (s *Supervisor) Close(key string) error
func (s *Supervisor) List() []SessionInfo
func (s *Supervisor) Reap(now time.Time)          // idle sweep, called by the clock
func (s *Supervisor) ReapOrphans()                 // startup sweep
```

`Open` resolution order, and this order is the crash-safety story:

1. live process for `key` → reuse it
2. row for `key` with a `harness_session_id`, no live process → spawn `--resume <id>`
   *(verified: killed the process, resumed by id, prior context intact)*
3. otherwise → mint a uuid, spawn `--session-id <uuid>`

`Send` is the only caller-facing path and is where the breakers live:
turn timeout → mark dead, return error (next send resumes); `MaxTurns` or
`MaxCostUSD` exceeded → close and return a typed error so the caller can fall back.

### 1.3 `store.go` — migration `harness_sessions`

```sql
CREATE TABLE harness_sessions (
    key                TEXT PRIMARY KEY,
    harness_session_id TEXT NOT NULL,
    kind               TEXT NOT NULL,
    model              TEXT NOT NULL,
    pid                INTEGER,
    state              TEXT NOT NULL,      -- starting|live|idle|dead|closed
    workdir            TEXT NOT NULL,
    started_at         DATETIME NOT NULL,
    last_activity_at   DATETIME NOT NULL,
    turns              INTEGER DEFAULT 0,
    cost_usd           REAL    DEFAULT 0,
    input_tokens       INTEGER DEFAULT 0,
    output_tokens      INTEGER DEFAULT 0,
    cache_read         INTEGER DEFAULT 0,
    last_error         TEXT
);
CREATE INDEX idx_harness_state ON harness_sessions(state, last_activity_at);
```

The row is written **before** the process is spawned, so a crash between spawn and
registration still leaves something to reap — the same discipline `agent_turns`
already uses.

### 1.4 `budget.go`

```go
type Breaker struct{ …}
func (b *Breaker) Observe(rl *RateLimit)   // from every rate_limit_event
func (b *Breaker) Allow(kind Kind) (bool, string)
```

- Config `harness.window_share` (0.4) is the fraction of each window KARMAX may use.
- `Allow` is false when `five_hour.utilization` or `seven_day.utilization` exceeds
  the share, when `status` is a refusal, or when a spawn failed on auth.
- Trip and reset both emit a bus event and an `app.push`. A breaker that trips
  silently is indistinguishable from a feature nobody uses.
- **Fallback is the existing API path, unchanged.** `apiBrain` is never deleted.

### 1.5 `audit.go`

Every `tool_use` seen in an `assistant` event becomes a `harness.tool.called` bus
event: session key, tool name, and for `Bash` the command's first token. An
allowlist (`karmax`, `wacli`, `gh`, `git`, `ls`, `cat`, `rg`, …) is checked and
anything outside raises `app.push` immediately. This makes misuse visible, not
impossible — worth saying plainly, since the sessions run with full shell.

---

## 2. Config

```yaml
harness:
  binary: claude
  enabled: false            # phase 0 ships dark
  window_share: 0.4
  max_live: 6
  workdir_root: ~/.karmax/sessions
  allowlist: [karmax, wacli, gh, git, ls, cat, rg, sed, awk, jq]
  kinds:
    chat:  { model: sonnet, idle: 10m, max_turns: 40,  turn_timeout: 45s, max_cost_usd: 0.50, ephemeral: true  }
    agent: { model: sonnet, idle: 30m, max_turns: 100, turn_timeout: 3m,  max_cost_usd: 2.00, ephemeral: false }
    task:  { model: opus,   idle: 0s,  max_turns: 1,   turn_timeout: 10m, max_cost_usd: 5.00, ephemeral: true  }
```

A new use-case adds a `kind` here. It does not touch core.

---

## 3. The brain seam

`MainModelSession` is concrete: 13 exported methods, 16 call sites, 4 constructors.
Only three are the *thinking* path; the rest are history and token plumbing.

```go
type Brain interface {
    ProcessMessageWithheld(ctx context.Context, userMessage string,
        lent []tools.Tool, withhold map[string]bool) (string, []karmahelper.ToolCallRecord, error)
    SetTurnContext(dynamicContext string)
    NeedsCompaction() bool
    GetHistory() *models.AIChatHistory
    SetHistory(models.AIChatHistory)
    GetTotalTokens() int64
    GetKeepRecent() int
    ResetTokenCount()
}
```

`MainModelSession` already satisfies it — Phase 2 is `var _ Brain = (*MainModelSession)(nil)`
plus a `harnessBrain` beside it.

`harnessBrain` maps the interface onto a session:
- `SetTurnContext` buffers the dynamic context; the next `ProcessMessage…` sends it
  as the first paragraph of the user event (the same placement the API path uses).
- `ProcessMessageWithheld` → `Supervisor.Send`, returning the turn's text and its
  `ToolCalls` mapped to `ToolCallRecord` — which is what keeps the act-evidence
  guard, `recentactions`, and the duplicate-send counters working.
- `withhold` is enforced in the **prompt** plus a post-hoc audit assertion, since
  the CLI has its own toolset. An observe pass therefore also gets its outbound
  KARMAX tools withheld at the `karmax tool call` layer, which is the real gate.
- History/compaction: the CLI owns the transcript. `NeedsCompaction` returns false,
  `GetHistory` returns the KARMAX-side view used for context injection only. **Two
  memories of one conversation is what produced the meltdown loop**, so the CLI's
  transcript is authoritative and KARMAX's is context, never replayed back in.

---

## 4. Workflow surface (this is the modularity contract)

`pkg/loopkit`:

```go
// Session returns a handle to a durable conversation identified by key.
// Core has no idea what the key means.
Session(key string, kind string) SessionHandle

type SessionHandle interface {
    Send(ctx context.Context, text string) (string, error)
    Close() error
}
```

wa-monitor then does, in the workflow and nowhere else:

```go
s := kit.Session("chat:"+chatID, "chat")
reply, err := s.Send(ctx, prompt)   // ~1.6s warm
```

The persona, the idle window, when to answer, who to answer as — all workflow.
Core supplies a conversation with a subprocess.

---

## 5. Phases, with the check that closes each

| phase | lands | proof |
|---|---|---|
| **0** | `internal/harness`, migration, config (`enabled: false`) | unit tests on the parser against a recorded transcript; open→3 sends→close by hand: turns 2 and 3 under 2s; `kill -9` then send resumes with context; ledger rows appear |
| **1** | `karmax session open\|send\|list\|close\|reap` | drive it entirely by hand, including the idle reaper and the `max_live` eviction |
| **2** | `Brain` interface + `harnessBrain`; route **`karmax ask` only** | A/B 20 prompts against both brains; compare option-menu rate and self-announcement rate — the two signals that characterised the gpt-5 regression |
| **3** | `kit.Session` + wa-monitor warm chat sessions | a real chat answered ≈1.6s; session closes on idle; a second chat opens its own; cap holds under a burst |
| **4** | gateway / summaries / review move over | a few days of Phase 3 with no incident first |
| **5** | breaker tuning; fallback drills | revoke auth and confirm nothing stops; force `window_share` to 0 and confirm clean API degradation |

Each phase is revertible on its own, and `harness.enabled: false` disables the lot.

---

## 6. Risks, ranked by what they would actually cost

1. **The weekly window is at 0.88 today.** Phase 2 onwards competes with the
   operator's own dev sessions. Do the A/B when `five_hour` is low, and keep
   `window_share` conservative until a full week of data exists.
2. **12.7k tokens of CLI overhead per turn.** If idle windows are set too short,
   sessions churn and this design costs *more* than the API path. The `chat` idle
   window is the single most important number in the config.
3. **Full shell.** Audited, not prevented.
4. **Quality is unproven.** The harness is a different brain, not a better one.
   Phase 2 exists to find out before anything user-facing depends on it.
5. **Codex is not installed here.** The supervisor is binary-agnostic, but
   "fall back to the other harness" cannot be tested on this machine today.
