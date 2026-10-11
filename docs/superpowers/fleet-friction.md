# Fleet friction log

Every point where working with the fleet's agents was harder than it should be, found while
building the fleet console (spec `specs/2026-10-11-fleet-console-design.md`). Each gets a fix or
a reason it stays.

| # | found | friction | status |
|---|---|---|---|
| 1 | 2026-10-11, first delegated review | An agent cannot report to the operator: `tell operator` is "unknown agent". | plan Task 0B |
| 2 | same | No way to wait on an agent; following progress means peeking at its screen. | plan Task 0D |
| 3 | same | A `tell` takes ~6.5 s and runs a model on the receiver's subscription. | the panel types with `prompt` instead; relay stays for cross-host |
| 4 | same | `done` shows in `status` only at the next tick. | open |
| 5 | agent-02's audit | Agents cannot read their own events, usage, history or quota (403), or peek peers. | open (needs per-agent tokens) |
| 6 | same | No `whoami`, no `healthz`, no route index. | plan Task 0E |
| 7 | agent-04, 4B | A nested `claude` an agent ran in a tool call failed auth (no login in the tool env); fleetd blamed the subscription and disabled the agent mid-task. | fixed: only the agent's own session can disable it |
| 8 | agent-04, 4B | Hook payloads (StopFailure `raw`) carry full transcript paths and prompts into the ledger's events, which the console streams. | open: keep `error` and drop the rest in `event.sh` |
