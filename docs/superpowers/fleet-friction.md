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
| 9 | lead, chasing 4B's question | No telemetry ever reached fleetd: Claude Code's JS OTLP exporter sends `OTEL_EXPORTER_OTLP_HEADERS` without percent-decoding, so `Bearer%20<token>` got 401 on every export. Found only with a packet capture: fleetd logs no auth failures on :4318. | fixed (4B): usage rows land since 20:00; open: log refused exports |
| 10 | agent-04, 4B | `set -g window-size manual` in a tmux 3.5a config file crashes the server on the first session. | fixed: set from a hook; verified 220x50/no prefix in all four agents |
| 11 | the first briefs | A brief told agents to report with `tell operator` before fleetd could take it; they fell back to files. Rolling out fleetd and briefing agents need an order. | lead process: ship fleetd changes before the briefs that use them |
| 12 | lead | Rolling out a brief change (`fleetctl render && up`) restarts every agent mid-task. | open: render should say which containers change, and up should wait for idle agents |
| 13 | agents 02 and 03 | Agents building a UI cannot see it: no browser runs in the image, and the mockup canvas is private to the operator. | mockups committed (54917fc); lead screenshots into `.fleet-out/screens/`; agent-04 adds Chromium's libraries to the image |
| 14 | agent-02 | An agent rebased a branch the lead had already merged, then reported it again; merging it conflicted in 6 files. | brief: never rebase or force-push a reported branch; new work starts from origin/main |
| 15 | lead, first live run | Every `tell` delivery runs `claude -p` inside the receiver; its hook events land in the agent's own log, so stream and activity are full of sessions that are not the agent's work. | fixed: relay sessions run with `disableAllHooks` (agent-03), verified live; the console also filters by session (agent-01) |
| 16 | agent-04, 4B | A test that crashed tmux left two 5 MB core dumps, and `git add -A` pushed them to the public repo (no secrets in them). | squashed out of main, branch deleted; `core*` ignored; the shared pre-commit hook refuses core dumps and files over 1 MB |
| 17 | lead | `fleetctl inbox --follow` exits on the first failed poll, so a fleetd restart ends every watch. | open: retry with backoff and say so on stderr |
| 18 | lead, image rollout | A rollout restarts agents mid-command; their sessions resume idle and need a nudge (`fleetctl prompt`) to carry on. | agent-04: `fleetctl up --when-idle` (#12); fleetd could prompt resumed sessions itself |
| 19 | lead, console on PC2 | Docker 29 (PC2) wraps streamed `docker stats` lines in ANSI codes; Docker 28 (Kali, where the console was first run) does not, so the parser silently found no stats. Agents build and test against the build host's Docker, not the target's. | agent-01: strip ANSI; open: record each host's docker version in fleetd and show it |
| 20 | lead | `fleetctl up` has no `--dry-run`: the plan of what it would recreate is only printed on the way to doing it. | open: agent-04 |
| 21 | lead, PC2 reboot | Containers started before the NFS automount was really mounted: one failed to create (Docker never retries), three bound the empty autofs point and saw no code until restarted. | drop-in on PC2 orders docker after the mount; agent-03: host-bootstrap.sh installs it |
| 22 | lead, PC2 reboot | fleetd froze for minutes: a tick's `docker --context pc2` was killed on timeout, but its ssh child held the pipes open, so `cmd.Wait` never returned. | agent-01: WaitDelay + kill the process group, everywhere the fleet execs |
| 23 | lead, PC2 reboot | Every restarted (not recreated) agent stops on Claude Code's Bypass Permissions dialog, though agent-run sets the accepted flag; fleetd could not alert because it was frozen (#22). | agent-02: find what 2.1.294 checks; detect the dialog as stuck |
| 24 | lead, kiosk | The kiosk's Chrome opened at half the screen (no window manager) and showed UTC. | fixed: window size/position, DISPLAY_TZ |
