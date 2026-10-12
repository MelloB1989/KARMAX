# Fleet jobs: any Claude session puts the fleet to work

Status: approved 2026-10-12 (design in chat; the operator asked to go straight to implementation).

## Goal

A Claude Code session on Kali, or KARMAX's orchestrator, hands work to the fleet and gets the result
back without knowing which agent is free, how reports travel, or what the fleet's internals are. Every
operation is one daemon function, exposed both as an HTTP route on fleetd and as a `fleetctl` command
(the CLI is a thin client of the route).

Callers: sessions on Kali and the KARMAX orchestrator. Both use the full-scope token. No new scope;
agents do not delegate to agents (their relay token cannot assign).

## The task id is the handle

A task's reports belong to the task. When an agent tells anyone something while it has a task, fleetd
records the message with that task's id (`relays.task`). A caller waits on, and reads, its own task id;
it never has to read a shared inbox, so two sessions never see each other's results. The operator's
inbox is unchanged.

## Operations

| CLI | HTTP | daemon |
|---|---|---|
| `run <brief…>` `[--repo R] [--agent A] [--task ID] [--from NAME] [--queue 30m] [--wait] [--timeout 2h] [--json]`; brief `-` reads stdin, `--brief-file F` reads a file | `POST /v1/run` | `Run` |
| `pick [--repo R] [--json]` | `GET /v1/pick?repo=` | `Pick` |
| `result <task> [--json]` | `GET /v1/tasks/{id}` | `Result` |
| `wait [<agent>] --task <id> [--after N] [--timeout] [--json]` (agent optional when a task is named) | `GET /v1/wait` | `Wait` |
| `followup <task> <text…> [--from NAME]` | `POST /v1/tasks/{id}/followup` | `Followup` |
| `cancel <task> [--from NAME] [--reason TEXT]` | `POST /v1/tasks/{id}/cancel` | `Cancel` |
| `done --task <id>` (agent optional) | `POST /v1/done` (agent resolved from the task) | `Done` / `Close` |
| `guide` | `GET /v1/guide` | `guide.Text` |
| `skill install [--dir D] [--print]` | — (writes local files) | `guide.Skill` |

### run

- Picks an agent unless `--agent` names one, makes a task id unless `--task` gives one, records the task
  with its repo and assigner, and delivers the brief. Pick and assign are one critical section in
  fleetd, so two concurrent runs never get the same agent.
- The delivered brief is composed by fleetd, not the CLI: `Task <id> from <from>. Repo: <repo>.` (the repo
  sentence only when given) followed by the caller's text.
- Task id when not given: `YYYYMMDD-<slug>-<4 hex>`, slug = the brief's first 4 words, lowercase,
  `[a-z0-9]` joined by `-`, at most 32 chars.
- No free agent: HTTP 409 with the candidates and why each was passed over; `fleetctl run` exits 5 and
  prints them. `--queue D` makes fleetd retry the pick every few seconds for up to D before giving up.
- `--agent A` with A not free: 409 naming its phase/task, unless A is free. (`give` remains for
  deliberately piling work on a busy agent.)
- `--wait`: after assigning, waits on the task (as `wait --task`) and prints the report, exit codes as
  `wait`.
- Reply: `{task, agent, why}`.

### pick (the rules run uses)

Eligible: enabled, phase `standby` (or `waiting` with its task closed), not cooling, down, disabled,
`low` or `reserved`. Among the eligible, best first:
1. its most recent task had the same repo (affinity), when a repo is given;
2. lowest 5 h quota use;
3. lowest 7 d quota use;
4. longest idle.

Reply: `{agent, why, candidates: [{agent, eligible, why, five_hour_pct, seven_day_pct, idle_s}]}`;
`agent` empty when none is eligible.

### result

`{task: {...the task row, incl. repo}, state: open|closed, reports: [relays with this task id, oldest
first], commits: ["<short sha> <subject>", …], ahead: N}`. Commits are `git log --format='%h %s'
main..<branch>` in `<code>/<repo>` on fleetd's host, when the task has both a repo and a branch; any
git failure leaves `commits` empty and sets `commits_error`.

### wait

As today, plus: the agent may be omitted when a task is named (fleetd resolves it from the task); with
a task named, a "message" is any relay with that task id and an id above `after`, whatever its
recipient (so reports to `karmax` count, not only to the operator); the reply's message is that relay.

### followup, cancel, done by task

- `followup` relays `Task <id> follow-up from <from>. <text>` to the task's agent. The task must be open.
- `cancel` interrupts the agent (Escape into its session, like a person pressing Esc), relays
  `Task <id> was cancelled by <from>: stop work on it now, leave the branch as it is, and report
  nothing further.` (plus `--reason`), and closes the task as `cancelled`, keeping the session.
- `done --task <id>` with no agent resolves the agent from the task.

### guide and skill

One text, embedded in `fleetctl` and `fleetd` (`fleet/internal/guide/guide.md`): when to delegate, the
run → wait → result → review → merge loop, fan-out (several `run`s without `--wait`, then a `wait` per
task), follow-ups and answering an agent's question, cancel, reading the roster, and the rules
(delegated work is untrusted until its diff is read and its tests pass; merge with `&&`; never print
secrets; never rebase a branch an agent reported). `skill install` writes `~/.claude/skills/fleet/SKILL.md`:
frontmatter (`name: fleet`, a description that triggers on "use the fleet / agents / delegate / parallel
work") plus the same text. The orchestrator's CLAUDE.md paragraph uses `run`/`result`/`followup`/`cancel`.

## Ledger

Migrations (the existing `migrate()` list): `relays.task TEXT`, `tasks.repo TEXT`. `ledger.Relay` gains
`Task string json:"task,omitempty"`; `ledger.Task` gains `Repo string json:"repo,omitempty"`.

## Errors and exit codes

`fleetctl`: 0 ok; 1 error; 3 agent down/cooling/disabled (wait); 4 timeout (wait); 5 no free agent (run,
pick with no eligible agent prints and exits 5 too). HTTP: 404 unknown task, 409 no free agent / agent
busy / task already closed, 400 bad input.

## Testing

Unit: pick ordering and every exclusion; two concurrent `Run`s get different agents; id generation;
report attachment on both Tell paths; Result with and without branch; Wait by task with a non-operator
recipient; followup/cancel on open and closed tasks; done by task. Server: each route's scopes and
status codes. CLI: exit codes and `--wait`. End to end: a Kali session runs a small task on the live
fleet and reads it back with `result`.
