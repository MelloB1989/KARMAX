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

- **S1b relay lockdown.** Must say the relay could not run Bash and still delivered.
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

## Phase 3 — KARMAX's orchestrator

The orchestrator is one KARMAX agent's harness session, named so agents can
message it. It runs one of two ways:

- **Native** (`orchestrator.native: true` in `fleet.yaml`): on the host that
  runs fleetd and KARMAX, outside Docker, under that host's own Claude login.
  Use it when no agent runs on that host — every agent then reaches it
  through the relay, and nothing about it lives in a container.
- **In `karmax-brain`**: a container sharing the agents' PID namespace on the
  same host, so agents there message it natively. It needs its own
  subscription token (`orchestrator.token_env`) and a `launch:` prefix.

In `karmax.yaml`, give the one agent that orchestrates its own harness kind.
Every other agent keeps the default `agent` kind and runs as before. Setting
`kinds` replaces the built-in defaults, so keep the kinds you use:

```yaml
agents:
  - id: nexus
    harness_kind: orchestrator   # only this agent thinks in the fleet's kind
    # …the rest of its definition, unchanged

harness:
  enabled: true
  kinds:
    orchestrator:
      model: sonnet
      idle: 45m
      max_turns: 500
      turn_timeout: 3m
      name: karmax          # how agents address it; = fleet.yaml orchestrator.name
      resident: true        # never reaped or evicted, revived if it dies
      # In karmax-brain only:
      # launch: [docker, exec, -i, -w, "{workdir}", karmax-brain]
    agent: {model: sonnet, idle: 45m, max_turns: 500, turn_timeout: 3m}
    # …every other kind you use

webhooks:
  enabled: true
  routes:
    - path: /fleet
      method: POST
      agent_id: nexus                 # the orchestrating agent
      # No bus_event: a custom kind is not routed to any agent, so every
      # fleet event would be dropped. The default, webhook.fired, reaches
      # agent_id's inbox.
      secret: ${FLEET_WEBHOOK_SECRET}
      signature_header: X-Fleet-Signature
```

KARMAX's environment needs:
- `FLEET_WEBHOOK_SECRET`, the route's secret, which is the same value as in
  fleetd's env file;
- `FLEET_NAME=karmax`, so the orchestrator's `fleetctl tell` is signed as
  `karmax` and agents report back to it rather than to "the operator".

A systemd drop-in with an `EnvironmentFile` holding just those two does it.
Give KARMAX's API a token too (`KARMAX_API_TOKEN` in `~/.karmax/.env`), and
put the same value in fleetd's env file: without one the API is open to every
agent on the LAN.

Check: an agent's `fleetctl tell karmax "…"` lands in the orchestrator's
session, which takes a turn by itself — KARMAX logs "a session ran a turn by
itself". Idle for an hour, it is still live, and a KARMAX restart revives it.

Limits:

- With `launch:`, the CLI's environment is the container's, not the daemon's,
  so `Thinking` (`MAX_THINKING_TOKENS`) does not cross the prefix, and the
  operator's browser tools are not offered (they reach `localhost` on the
  host, which the container cannot see).
- `harness.close` on a resident session closes it for good, until someone
  writes to it. Every other close, such as the turn limit or a model change,
  lets it come back.

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
It writes the fleet's paragraph into the orchestrator session's
`.claude/CLAUDE.md`, beside the `CLAUDE.md` the session owns. The paragraph
covers the roster and how to pick, assign, close and react.

The orchestrator reads that file when its session starts, so restart KARMAX
after a render. Set `orchestrator.report` too. A turn an agent's report
starts has no caller, so its reply reaches nobody unless the orchestrator
sends it somewhere, for example
`karmax tool call comms.send target=<number>@s.whatsapp.net content="<text>"`.

To verify, ask KARMAX on WhatsApp to fix something in a repo, and watch
`fleetctl ls` and `fleetctl events`.

## Agents' tools

- **GitHub.** `github.token_env` in `fleet.yaml` names a token in the env
  file (`gh auth token --user <account>`). Every agent gets it as `GH_TOKEN`:
  `gh` uses it, and so does git, through gh's credential helper in the
  image. The image rewrites SSH remotes, including `~/.ssh/config` aliases
  such as `git@github:`, to HTTPS, because containers hold no SSH keys.
- **Git identity.** `git.name` and `git.email`. Each agent commits as
  `<name> (agent-NN)`.
- **Heavy work.** `build.docker_host` points every agent's Docker at a
  bigger machine over SSH, and `kbuild <cmd>` runs a command there in the
  fleet image, as you, in the same shared-code directory.

## Security

- **Tokens.** Each one lives only in `~/.karmax/.env` and in its container's
  0600 env file. `render` refuses a directory others can read. Tokens are
  never in an image, a compose file, argv or `fleet.yaml`.
- **Two fleet tokens.** `FLEET_TOKEN` (full) is for you and the orchestrator.
  `FLEET_RELAY_TOKEN` (tell and status only) is for every agent, and is also
  what the OTLP receiver accepts.
- **Relay.** A relayed message is untrusted text from another host. The relay
  session gets only `SendMessage` and `ListAgents` (`--tools`), no MCP server
  (`--strict-mcp-config`), and at most 4 turns. It runs in bypass mode like
  every receiver: Claude Code holds a message from a session in another
  permission mode (`peer_message_hold`, `mode-mismatch`), and a `-p`
  receiver such as the orchestrator never reads a held one. Spike S1b checks
  that it cannot run Bash and still delivers.
- **Senders.** A relayed message names its sender (`FLEET_NAME`), and fleetd
  refuses senders that a reply can't reach. Only a full-scope token may send
  as `operator`.
- **Shared PID namespace.** Agents on one host can see and signal each other's
  processes, and read each other's environment. The plan accepts this for a
  home LAN; the anchor keeps the host's own processes out of reach.
  **That includes `karmax-brain`.** Native messaging with the orchestrator
  needs it in the same namespace, so a Kali agent can read its environment:
  the orchestrator's subscription token, the full-scope `FLEET_TOKEN` and
  `KARMAX_API_TOKEN`. The KARMAX daemon itself stays on the host, out of
  reach. If that is too much, run no agents on the orchestrator's host; all
  of them then reach it through the relay.
- **The docker group is root** on PC2. The `fleet` user is key-only.
