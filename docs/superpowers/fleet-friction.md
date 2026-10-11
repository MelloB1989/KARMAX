# Fleet friction log

Every point where working with the fleet's agents was harder than it should be, found while
building the fleet console (spec `specs/2026-10-11-fleet-console-design.md`). Each gets a fix or
a reason it stays.

| # | found | friction | status |
|---|---|---|---|
| 1 | 2026-10-11, first delegated review | An agent cannot report to the operator: `tell operator` is "unknown agent". | fixed (f2e145f): operator inbox, `fleetctl inbox` |
| 2 | same | No way to wait on an agent; following progress means peeking at its screen. | fixed (f2e145f): `fleetctl wait` |
| 3 | same | A `tell` takes ~6.5 s and runs a model on the receiver's subscription. | the panel types with `prompt` instead; relay stays for cross-host |
| 4 | same | `done` shows in `status` only at the next tick. | open |
| 5 | agent-02's audit | Agents cannot read their own events, usage, history or quota (403), or peek peers. | open (needs per-agent tokens) |
| 6 | same | No `whoami`, no `healthz`, no route index. | fixed (f2e145f) |
| 7 | agent-04, 4B | A nested `claude` an agent ran in a tool call failed auth (no login in the tool env); fleetd blamed the subscription and disabled the agent mid-task. | fixed: only the agent's own session can disable it |
| 8 | agent-04, 4B | Hook payloads (StopFailure `raw`) carry full transcript paths and prompts into the ledger's events, which the console streams. | open: keep `error` and drop the rest in `event.sh` |
| 9 | lead, chasing 4B's question | No telemetry ever reached fleetd: Claude Code's JS OTLP exporter sends `OTEL_EXPORTER_OTLP_HEADERS` without percent-decoding, so `Bearer%20<token>` got 401 on every export. Found only with a packet capture: fleetd logs no auth failures on :4318. | agent-04, 4B (decode server-side); log refused exports |
| 10 | agent-04, 4B | `set -g window-size manual` in a tmux 3.5a config file crashes the server on the first session. | agent-04, 4B (set it from a hook) |
| 11 | the first briefs | A brief told agents to report with `tell operator` before fleetd could take it; they fell back to files. Rolling out fleetd and briefing agents need an order. | lead process: ship fleetd changes before the briefs that use them |
| 12 | lead | Rolling out a brief change (`fleetctl render && up`) restarts every agent mid-task. | open: render should say which containers change, and up should wait for idle agents |
| 13 | agents 02 and 03 | Agents building a UI cannot see it: no browser runs in the image, and the mockup canvas is private to the operator. | mockups committed (54917fc); lead screenshots into `.fleet-out/screens/`; agent-04 adds Chromium's libraries to the image |
| 14 | agent-02 | An agent rebased a branch the lead had already merged, then reported it again; merging it conflicted in 6 files. | brief: never rebase or force-push a reported branch; new work starts from origin/main |
| 15 | lead, first live run | Every `tell` delivery runs `claude -p` inside the receiver; its hook events land in the agent's own log, so stream and activity are full of sessions that are not the agent's work. | console filters by the agent's session (agent-01); longer term, relay sessions should not run the agent's hooks |
| 16 | agent-04, 4B | A test that crashed tmux left two 5 MB core dumps, and `git add -A` pushed them to the public repo (no secrets in them). | squashed out of main, branch deleted; `core*` ignored; the shared pre-commit hook refuses core dumps and files over 1 MB |
