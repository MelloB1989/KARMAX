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

- **Agent** (`internal/agent`) — a multi-model orchestrator (main / memory / summary) built on the `karma` lib via a local Anthropic-compatible gateway. The curated `ABOUT_ME` profile is injected into context every turn (identity is never hardcoded).
- **Memory** (`internal/memory`, `internal/coldscan`) — a page-index tree over long-term memory; **hot** memory from active chats and **cold** per-chat summaries generated in the background (community/promo groups are skipped by participation); and an **agentic retrieval sub-agent** (`memory.retrieve`) that queries memory, the page-index, chat summaries, the profile, and live WhatsApp across multiple steps to return synthesized context. A **cleanup** flow lets the LLM ask you to correct low-confidence memories.
- **Human-in-the-loop** (`propose` tool + `/api/proposals`) — outward/irreversible actions become proposals you approve in the app before they execute.
- **Comms** (`internal/comms`) — WhatsApp via `wacli`, Discord; plus Expo push and ntfy.
- **Loops** (`loops:` in `karmax.yaml`) — declarative scheduled prompts (tech news, hot-sync, profile refresh, daily briefing); the agent decides how to fulfil each.
- **API** (`internal/api`) — bearer-auth HTTP API + mDNS discovery for the app.

## Setup

### Prerequisites
- Go 1.22+ (CGO enabled — SQLite).
- An Anthropic-compatible model endpoint (e.g. a local gateway), configured in `.env`.
- `claude` and/or `codex` CLIs, authenticated with their own accounts (KARMAX runs them with its gateway env stripped so they use that auth).
- `wacli` (a separate WhatsApp CLI/daemon) for the WhatsApp channel — optional.

### Configure
```bash
cp .env.example .env                  # ANTHROPIC_BASE_URL / auth, optional GOOGLE_API_KEY, KARMAX_API_TOKEN, etc.
cp karmax.yaml.example karmax.yaml    # models, loops, comms channels, target chat
```

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
```

Every `claude_code.call` delegation is told about this surface automatically, so executors can pull more context or report back to you mid-task.

## Models
Set in `karmax.yaml` (`agents:`) — by default main `claude-opus-4.6`, memory/retrieval `claude-opus-4.6`, summary `claude-sonnet-4.6`, with fallbacks. Endpoints live under `ai.providers`.

## Security
- Secrets live in `.env` (gitignored); `karmax.yaml` is gitignored too — commit only the `.example` templates.
- Set `KARMAX_API_TOKEN` to require auth on the API.
- Outward/irreversible actions are gated by the approval inbox.
