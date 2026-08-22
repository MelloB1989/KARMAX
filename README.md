<p align="center">
  <img src="app/assets/images/icon.png" alt="KARMAX logo" width="128" />
</p>

<h1 align="center">KARMAX</h1>

A personal AI "Jarvis" — an always-on **orchestration daemon** you can delegate anything to, with a phone app as the cockpit. KARMAX senses (WhatsApp, news, your profile), **proposes** actions for your approval, **acts** through real tools, and **remembers** everything.

> Personal project. KARMAX runs with broad access to your machine and accounts, gated by a human-in-the-loop approval flow. Review it before pointing it at real accounts.

## What it is

KARMAX is **orchestration-only**: a Go daemon that coordinates, remembers, and communicates, delegating the actual heavy work (coding, web research, building) to coding harnesses (**Claude Code** / **Codex**). It runs autonomous loops, maintains long-term memory, and talks to you over WhatsApp, Discord, and a companion app.

The core loop: **Sense → Propose → Approve → Act → Remember.**

## Repository layout

- `/` — the KARMAX daemon (Go).
- `/app` — the companion app (Expo / React Native): the cockpit — **chat**, **approvals inbox**, **activity**, **memory explorer** (entries · 2D tree · 3D page-index graph · profile · cleanup), and **config**.

## Architecture

KARMAX is a Go daemon built around a **durable event log**. Everything that happens — a
WhatsApp message, a timer firing, a delegation finishing — becomes an event on disk
*before* any subscriber sees it, and each subscriber records how far it has read. A crash
resumes where it left off rather than starting blind. Delivery is at-least-once, so every
handler is written to tolerate seeing an event twice.

```mermaid
flowchart LR
    subgraph Senses
        WA[WhatsApp · wacli]
        CH[Discord · Slack · Telegram]
        TR[Issue trackers · code hosts]
        CL[Clock · scheduler]
        API[Phone app · CLI]
    end

    BUS[("Durable event log<br/><i>internal/bus</i><br/>at-least-once · per-subscriber offsets")]

    WA --> BUS
    CH --> BUS
    TR --> BUS
    CL --> BUS
    API --> BUS

    BUS --> RT["Runtime router<br/><i>internal/runtime</i>"]

    RT --> AG["Orchestrator agent<br/><i>internal/agent</i>"]
    RT --> LOOPS["Automation tiers"]

    AG --> TOOLS["Tools · memory · comms"]
    AG -.delegates heavy work.-> HARNESS["Claude Code / Codex"]

    LOOPS --> TOOLS
    HARNESS --> TOOLS

    TOOLS --> OUT["Actions:<br/>send · remember · schedule · propose"]
```

The core cycle is **Sense → Propose → Approve → Act → Remember**, though most actions skip
the approval step by design: outbound messages go straight out and the operator is shown
what was sent, while only genuinely irreversible things (spending money, posting publicly,
deleting data) become proposals in the app's inbox.

### The orchestrator is deliberately thin

KARMAX coordinates, remembers and communicates. It does **not** do the heavy work itself —
coding, research and building are delegated to coding harnesses (**Claude Code** / **Codex**)
running under their own accounts. This keeps the router's per-turn context small enough to
be cheap and fast, which matters because the router runs on metered inference.

Models run on **AWS Bedrock** (SigV4, `ap-south-1`) via the Anthropic-compatible path, so
the whole tool-calling loop is preserved:

| Role | Model | Why |
| --- | --- | --- |
| Main / routing | Claude Sonnet 4.6 | judgement, tool use |
| Memory retrieval | Claude Haiku 4.5 | mostly searching and reading, not judgement |
| Summary / compaction | Claude Haiku 4.5 | text-only, cheap, reliable |
| Fallback | Claude Haiku 4.5 | a fallback fires when something is *already* wrong — the worst moment to switch to the most expensive model available |

Every model call is metered (`internal/cost`), so `karmax cost` reports real spend against a
budget rather than an estimate.

### Three tiers of automation

Anything that runs on your behalf sits in one of three tiers, in order of how much it can do:

```mermaid
flowchart TD
    R["<b>Recipes</b><br/>one YAML file<br/>no install, no rebuild"]
    L["<b>Signed loops</b><br/>WASM in a sandbox<br/>capability-gated"]
    C["<b>Compiled-in</b><br/>first-party Go<br/>full authority"]

    R -->|"karmax recipe"| BROKER
    L -->|"karmax wloop"| BROKER
    C --> BROKER

    BROKER{{"Broker<br/><i>internal/broker</i><br/>decides what each may do,<br/>and meters what it did"}}
    BROKER --> HOST["Host functions:<br/>log · memory · send · harness · notify"]
```

A signed loop is a WASM module plus a manifest of what it needs. Installing verifies both
signatures and the module's digest before any byte is loaded, then grants **exactly** what
the manifest declared — nothing more. The sandbox has no filesystem, no environment and no
arguments; a loop reaches the outside world only through host functions the broker has
granted it. That boundary is real: a loop whose manifest forgets a capability fails at the
call, which is how a drafted reply once ended up queued nowhere.

### Memory

Long-term memory is a **page-index tree** over subject documents, held remotely in GitLoom
with the local SQL store (SQLite by default; see [Configure](#configure)) used when no
remote is configured.

```mermaid
flowchart LR
    EV["An event<br/>happens"] --> ING["Workflow ingests<br/>what it did"]
    ING --> MEM[("Long-term memory<br/>subject documents<br/>dated sections")]

    MEM --> RECALL["Recall<br/>before deciding"]
    RECALL --> DEC["The next decision<br/>knows what was<br/>already done"]

    MEM --> MERGE["memmerge<br/>consolidates duplicates"]
    MEM --> REVIEW["review<br/>asks about stale facts<br/>— once, not repeatedly"]
    MERGE --> MEM
    REVIEW --> MEM
```

A write folds into the document that already holds its subject rather than replacing it, so
the fortieth fact about a project does not delete the previous thirty-nine. Two properties
of that store shape everything built on it: a write is a **whole-file overwrite**, and reads
are **eventually consistent** by several seconds. Bulk writers must read each fact back
before writing the next, or they silently overwrite themselves.

### Sending

Every outbound message — from the agent, a loop, a recipe, the CLI or an alert — crosses one
function, which is what makes it possible to say a thing only once regardless of who
produced it:

```mermaid
flowchart LR
    A["Orchestrator<br/>comms.send"] --> S
    B["wa-monitor loop"] --> S
    C["chat-sweep outbox"] --> S
    D["karmax send · recipes"] --> S
    E["Alerts"] --> S

    S{{"Manager.send<br/><i>internal/comms</i>"}}
    S --> DUP{"Said this<br/>already?"}
    DUP -->|"identical &lt; 6h<br/>reworded &lt; 10m"| REF["Refused<br/>ErrDuplicateSend"]
    DUP -->|no| CHAN["WhatsApp · Discord<br/>Slack · Telegram"]
    CHAN --> REC["Recorded + operator notified"]
```

### The rest

- **Human-in-the-loop** (`propose` tool + `/api/proposals`) — irreversible actions become
  proposals you approve in the app.
- **Safety** (`internal/safety`) — checks applied to text and requests KARMAX did not write
  itself, including a privacy guard every social post must pass.
- **Connectors** (`internal/connectors`, `pkg/connectorkit`) — integrations that mount their
  own tools, auth and event sources: GitHub, LinkedIn, X.
- **Mesh** (`internal/mesh`) — lets KARMAX instances find each other, agree to talk, and
  exchange work, with a hop limit so a chain cannot run away.
- **Voice** (`internal/voice`) — the interface a voice integration plugs into; WhatsApp calls
  are answered and spoken through it.
- **vorg** (`internal/vorg`) — instantiates a virtual organisation of agents from a
  declarative spec.
- **API** (`internal/api`) — bearer-auth HTTP API plus mDNS discovery for the app.

## Install (prebuilt)

Each tagged release ships installers that drop the `karmax` binary and register
a background service that starts on login and **restarts aggressively** if it
ever stops — systemd (`Restart=always` + linger) on Linux, launchd `KeepAlive`
on macOS, a hidden supervised Scheduled Task on Windows.

**Linux / macOS**
```bash
curl -fsSL https://github.com/MelloB1989/KARMAX/releases/latest/download/install.sh | bash
```

**Windows** (PowerShell)
```powershell
irm https://github.com/MelloB1989/KARMAX/releases/latest/download/install.ps1 | iex
```

The binary lands in `~/.local/bin` (Linux/macOS) or `%LOCALAPPDATA%\KARMAX`
(Windows); data and config live in `~/.karmax`. After installing, set your
provider credentials in `~/.karmax/.env` and review `~/.karmax/karmax.yaml`.

> Releases are built by [`.github/workflows/release.yml`](.github/workflows/release.yml):
> native CGO builds per OS — Linux amd64/arm64, a macOS universal binary, and
> Windows amd64. Cut one by pushing a `v*` tag.

### Code signing (optional)

Builds are unsigned until you add the signing secrets, at which point the
pipeline codesigns + notarizes automatically (no workflow edits). Add these in
**Settings ▸ Secrets and variables ▸ Actions**:

**macOS** (Developer ID + notarization — needs a paid Apple Developer account):
| Secret | What it is |
| --- | --- |
| `MACOS_CERTIFICATE` | base64 of your *Developer ID Application* `.p12` |
| `MACOS_CERTIFICATE_PWD` | the `.p12` export password |
| `MACOS_SIGN_IDENTITY` | e.g. `Developer ID Application: Name (TEAMID)` |
| `MACOS_NOTARY_KEY` | base64 of the App Store Connect API key `.p8` |
| `MACOS_NOTARY_KEY_ID` | the API key ID |
| `MACOS_NOTARY_ISSUER_ID` | the API key issuer UUID |

**Windows** (Authenticode — any OV/EV code-signing `.pfx`):
| Secret | What it is |
| --- | --- |
| `WINDOWS_CERTIFICATE` | base64 of your code-signing `.pfx` |
| `WINDOWS_CERTIFICATE_PWD` | the `.pfx` password |

`base64 -w0 cert.p12` (Linux) or `base64 -i cert.p12` (macOS) produces the value.
A bare CLI binary can't be *stapled*, so macOS notarization is verified online
on first run; the macOS installer also clears the Gatekeeper quarantine, so it
runs cleanly either way.

## Setup (from source)

### Prerequisites
- Go 1.22+ (CGO enabled — SQLite).
- AWS credentials with `bedrock:InvokeModel` on the configured models, or any other
  Anthropic-compatible endpoint, configured in `.env`.
- `claude` and/or `codex` CLIs, authenticated with their own accounts (KARMAX runs them with its gateway env stripped so they use that auth).
- `wacli` (a separate WhatsApp CLI/daemon) for the WhatsApp channel — optional.

### Configure
```bash
cp .env.example .env                  # Bedrock/AWS or ANTHROPIC_BASE_URL auth, KARMAX_API_TOKEN, etc.
cp karmax.yaml.example karmax.yaml    # models, loops, comms channels, target chat
```

By default the store is a SQLite file under `data_dir/db` — nothing to configure. Point it
at Postgres or MySQL instead with `database.url` in `karmax.yaml` or the `KARMAX_DB_URL` env
var (env wins), e.g. `postgres://user:pass@host:5432/karmax?sslmode=require`.

### Run the daemon
```bash
go build -o karmax ./cmd/karmax
./karmax config validate
./karmax start
```

### Run the app
```bash
cd app
bun install
bunx eas-cli build --platform ios --profile development   # device build (enables 3D, push, calendar)
bunx expo start --dev-client
```
The app auto-detects the daemon over mDNS / your network; enter the host in **Settings** if needed.

## CLI = full harness parity
Everything the orchestrator agent can do is also reachable from the `karmax` CLI (it talks to the running daemon's API), so delegated harnesses (Claude Code) and scripts have the same powers as the agent:

```bash
karmax tool list                        # every live tool (built-in + memory/profile + MCP)
karmax tool call <name> [k=v ...]       # invoke ANY tool (or --json '{...}')
karmax memory search "<query>"          # recall long-term memory
karmax memory add "<fact>"              # save a durable fact
karmax notify "<title>" "<body>"        # push to the phone app (feed + push)
karmax send "<target>" "<message>"      # WhatsApp/Discord via the default channel
karmax ask "<prompt>"                   # ask the orchestrator agent itself
karmax loops list|run <name>            # inspect / trigger loops
karmax wloop sign|install|list          # package, verify and install signed WASM loops
karmax cost                             # what the models have consumed, against the budget
karmax status                           # what every loop has actually been doing
```

Every `claude_code.call` delegation is told about this surface automatically, so executors can pull more context or report back to you mid-task.

## Models
Set in `karmax.yaml` (`agents:`) — main Sonnet 4.6, memory/retrieval and summary Haiku 4.5,
with a Haiku fallback. Inference runs on AWS Bedrock via SigV4 (`KARMA_ANTHROPIC_BEDROCK=1`,
`AWS_REGION`), using the *Anthropic* client path rather than a native Bedrock provider — the
native one carries no tool definitions and would silently break every tool call. Model ids are
global inference profiles; the bare id is not invokable on demand. See the table under
[Architecture](#the-orchestrator-is-deliberately-thin) for why each role gets the model it does.

## Security
- Secrets live in `.env` (gitignored); `karmax.yaml` is gitignored too — commit only the `.example` templates.
- Set `KARMAX_API_TOKEN` to require auth on the API.
- Outward/irreversible actions are gated by the approval inbox.
