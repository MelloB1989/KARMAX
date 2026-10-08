# Agent fleet — packaging

Eight standby Claude Code agents in containers on two PCs, each on its own
subscription, orchestrated by KARMAX. The design is
[docs/AGENT-FLEET.md](../../docs/AGENT-FLEET.md). This directory holds the
assets. `fleetd` and `fleetctl` are in [`fleet/`](../../fleet), a Go module of
their own that KARMAX does not link.

**Nothing here changes KARMAX unless you configure it.** The daemon's only
changes are generic harness options (`launch`, `name`, `resident`), which are
off by default, plus a fix that applies to every harness session: a turn the
session runs by itself no longer stalls it or reaches the next caller.

| file | what |
|---|---|
| `spike.sh` | Phase 0: proves the undocumented parts. Run it first |
| `Dockerfile` | the image every agent and `karmax-brain` runs (build from the repo root) |
| `agent-run` | inside each agent: keeps `claude --name agent-NN` alive in tmux |
| `home/settings.json` | fleet-managed Claude Code settings: inbound messages, status line, hooks |
| `home/status.sh`, `home/event.sh` | the status-line and hook taps fleetd reads |
| `host-bootstrap.sh` | Phase 1: `kali` and `pc2` host setup |
| `fleet.yaml.example` | the fleet; copy to `~/.karmax/fleet/fleet.yaml` |
| `fleetd.service` | systemd user unit for fleetd on Kali |

Compose files are not here: `fleetctl render` writes them from `fleet.yaml`,
one per host, so they cannot drift from it.

## Phase 0 — the spike (Kali, ~15 min)

    claude setup-token                        # once per subscription
    export CLAUDE_CODE_OAUTH_TOKEN=…           # a subscription
    export CLAUDE_CODE_OAUTH_TOKEN_2=…         # a second one, to prove cross-account messaging
    sudo apt install jq                        # the spike needs it on the host
    packaging/fleet/spike.sh                   # …and `spike.sh clean` afterwards

What its output decides:

- **S1 socket dir.** Set `sock_dir` in `fleet.yaml` to the directory of
  `messagingSocketPath` (default `/run/fleet`, which the containers export as
  `XDG_RUNTIME_DIR`). If S1 fails, fall back to one container per host.
- **S2 background turns.** If the `-p` session took a turn by itself, the
  harness fix handles it. Check that the event shape matches what
  `internal/harness/session.go` (`opensTurn`) treats as a turn.
- **S4 StopFailure.** fleetd looks for the error type under several keys. If
  S4 shows a different one, add it to `observe.Event.ErrorType`.
- **S5 EOF through `docker exec`.** `Close` already closes stdin first and
  waits 5 s before signalling a launched session.
- **S6 quota.** If the status line carries `rate_limits`, quota is proactive;
  if not, it is only reactive, through `StopFailure`.
- **Version.** Pin `CLAUDE_VERSION` in the image build to the version the spike ran.

## Phase 1 — hosts

    sudo packaging/fleet/host-bootstrap.sh kali --pc2-ip <pc2>
    # on PC2 (Debian 13 netinst, SSH server + standard utilities only):
    sudo ./host-bootstrap.sh pc2 --kali-ip <kali> --code $HOME/code --pubkey "$(cat ~/.ssh/id_ed25519.pub)"
    docker context create pc2 --docker host=ssh://fleet@<pc2>
    docker --context pc2 ps

## Phase 2 — agents

    docker build -f packaging/fleet/Dockerfile -t karmax-fleet:2.1.294 \
      --build-arg UID=$(id -u) --build-arg GID=$(id -g) --build-arg HOME_DIR=$HOME \
      --build-arg USER_NAME=$(id -un) --build-arg CLAUDE_VERSION=2.1.294 .
    docker save karmax-fleet:2.1.294 | docker --context pc2 load

    (cd fleet && go build -o ~/.local/bin/ ./cmd/fleetd ./cmd/fleetctl)
    mkdir -p ~/.karmax/fleet && chmod 700 ~/.karmax/fleet
    cp packaging/fleet/fleet.yaml.example ~/.karmax/fleet/fleet.yaml   # edit it
    # add the tokens it names to ~/.karmax/.env (see the top of the example)
    fleetctl render          # 0600 env files, compose.<host>.yaml, briefs
    fleetctl up              # docker compose up -d on every host

Check: `fleetctl attach agent-01` shows the session (detach with `Ctrl-b d`).
Two agents on one host can message each other. `docker restart agent-01`
brings it back on the same session.

## Phase 3 — KARMAX runs its orchestrator in `karmax-brain`

In `karmax.yaml`. Setting `kinds` replaces the built-in defaults, so keep the
other kinds you use:

```yaml
harness:
  enabled: true
  kinds:
    agent:
      model: sonnet
      idle: 30m
      max_turns: 500
      turn_timeout: 12m
      name: karmax          # how agents address it; = fleet.yaml orchestrator.name
      resident: true        # never reaped or evicted, revived if it dies
      launch: [docker, exec, -i, -w, "{workdir}", karmax-brain]
    chat: {model: sonnet, idle: 20m, max_turns: 200, turn_timeout: 4m}
    task: {model: opus, idle: 5m, max_turns: 20, turn_timeout: 20m}

webhooks:
  enabled: true
  routes:
    - path: /fleet
      method: POST
      agent_id: karmax-agent          # your agent's id
      bus_event: fleet.agent
      secret: ${FLEET_WEBHOOK_SECRET}
      signature_header: X-Fleet-Signature
```

Check: from an agent, `/list-agents` shows `karmax`. An agent messaging it
while idle produces `harness.turn.background` on the bus, and the next
`karmax ask` gets its own answer. Idle for an hour, it is still live.

Known limits of a launched kind:

- The CLI's environment is the container's, not the daemon's, so
  `Thinking` (`MAX_THINKING_TOKENS`) does not cross the prefix.
- The operator's browser tools reach `localhost` on Kali, which the container
  cannot see.

## Phase 4 — fleetd

    install -D -m 644 packaging/fleet/fleetd.service ~/.config/systemd/user/fleetd.service
    systemctl --user daemon-reload && systemctl --user enable --now fleetd
    fleetctl ls

Make the Fleet dashboard once, with KARMAX's dashboard tool, using id `fleet`.
fleetd rewrites its `data/agents.json`, `quota.json`, `usage.json`,
`events.json` and `orchestrator.json` every tick.

    fleet/scripts/smoke.sh   # fleetd + fleetctl end to end, with a fake docker

## Phase 5 — the orchestrator

Set `orchestrator.karmax_agent` in `fleet.yaml`, then run `fleetctl render`.
It writes the fleet's paragraph (the roster; how to pick, assign, close and
react) into the orchestrator session's `.claude/CLAUDE.md`, beside the
`CLAUDE.md` the session owns. To verify, ask KARMAX on WhatsApp to fix
something in a repo, and watch `fleetctl ls` and `fleetctl events`.

## Security

- **Tokens.** Each one lives only in `~/.karmax/.env` and in its container's
  0600 env file. `render` refuses a directory others can read. Tokens are
  never in an image, a compose file, argv or `fleet.yaml`.
- **Two fleet tokens.** `FLEET_TOKEN` (full) is for you and the orchestrator.
  `FLEET_RELAY_TOKEN` (tell and status only) is for every agent, and is also
  what the OTLP receiver accepts.
- **Relay.** A relayed message is untrusted text from another host. The relay
  session gets only `SendMessage` and `ListAgents`, with no permission bypass,
  and is capped at 4 turns.
- **Shared PID namespace.** Agents on one host can see and signal each other's
  processes, and read each other's environment. The plan accepts this for a
  home LAN; the anchor keeps the host's own processes out of reach.
- **The docker group is root** on PC2. The `fleet` user is key-only.
