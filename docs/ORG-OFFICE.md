# Org office — KARMAX as an organisation's engineering agent

The org office (`internal/orgops`) is a KARMAX agent that looks after a GitHub
organisation the way a staff engineer and an on-call SRE would, for every
repository at once:

| Job | What it does |
|---|---|
| **PR guardian** | Every PR gets deterministic standards checks (title convention, size, description, required files, protected paths, tests), a **security scan** of the added lines (cloud keys, tokens, private keys, DB URLs with passwords, TLS off, SQL concat, shell injection, open CORS), then a model review held to the org's **written rules** and the repo's own `CLAUDE.md`/`AGENTS.md`. It posts the review on GitHub with inline comments, summarises it in chat and, per policy, approves and merges once CI is green. |
| **Incident autopilot** | Built-in **probes** watch production endpoints (error rate and latency over a window, with hysteresis). Alerts from **Alertmanager, Grafana, Sentry or any JSON** are accepted too, and so is a person typing "payments is throwing 500s" in chat. The office opens an incident, alerts the owners in a thread, sends a coding agent to find the root cause, fix it and open a PR, reviews that PR like any other, merges it on green CI per policy, and closes the incident when the probe recovers. |
| **Repo factory** | "create repo ledger-svc from go: double-entry ledger" creates the repository from a template, writes the org's standard files (CODEOWNERS, PR template, CI, the org rules as `CLAUDE.md`), protects the default branch and indexes it. |
| **Standardisation sweep** | `orgops_audit_repos` lists every repository missing the org's required files. |
| **Knowledge store** | Every repository's docs are distilled into a card and kept in long-term memory (GitLoom when configured), refreshed on merge, on creation and on a schedule. The orchestrator answers "how does billing talk to auth?" from the code's own words. |

Nothing about the org is compiled in. Who owns what, which chat each team uses,
what the rules are and how much the office may do alone are all set in
`karmax.yaml` (`org_ops:`) and Markdown rule files.

## How it fits KARMAX

```
GitHub ──webhook──► /orgops/github ─┐            ┌─► orgops-pr        review · standards · threats · merge on green
Alertmanager/Sentry ► /orgops/alert ┤            ├─► orgops-incident  alert → incident → fix agent → PR → merge
probes (orgops-watch, every 30s) ───┼─► bus ─────┤
any chat ──► comms.message ─────────┤            ├─► orgops-command   approve · reject · fix · merge · create · status
agent tools (orgops_*) ─────────────┘            └─► orgops-knowledge repo cards → long-term memory
```

* It reuses KARMAX's foundations unchanged: the durable bus, loops, **cases**
  (one chat thread per PR or incident), the audit log, memory, the harness
  (Claude Code / Codex), the sandbox driver and the **fleet**.
* Slow work never blocks fast work: a 40-minute fix runs in `orgops-incident`
  while `orgops-pr` keeps reviewing and `orgops-command` keeps answering "approve".
* State that must survive a restart (incidents, pending approvals, queued
  merges, reviews, repo cards) lives in the store's KV under `orgops:*`.

## Autonomy is policy, per installation and per repository

Each action has a mode:

| mode | meaning |
|---|---|
| `off` | do nothing |
| `notify` | say what it would do |
| `approve` | prepare it and ask; anyone replying `approve <CODE>` in any chat carries it out |
| `auto` | do it, then say what was done |

Actions: `review`, `approve_pr`, `merge`, `repo_create`, `incident_fix`,
`incident_merge`, `knowledge`. `max_auto_risk` (0–1, default 0.5) turns `auto`
into `approve` for risky changes. Risk comes from diff size, hot paths (auth,
payments, migrations, infra, workflows) and findings. A **blocker** finding,
such as a leaked key or a protected path, always stops a merge.

Defaults are conservative: it reviews and alerts on its own and asks before it
changes anything (`review: auto, approve_pr: notify, merge: approve,
repo_create: approve, incident_fix: approve, incident_merge: approve`).
To get "human-free" operation, set the actions you trust to `auto`, for the
whole org or only some repositories:

```yaml
org_ops:
  policy: { incident_fix: auto, incident_merge: approve }
  repos:
    - match: "acme/internal-*"
      policy: { merge: auto, incident_merge: auto }
```

## Rules are Markdown

* `org_ops.rules_files`: the org-wide rulebook (`~/.karmax/ORG.md`, say).
  These are quoted to every review and fix and written into new repositories
  as `CLAUDE.md`.
* Each repository's own `CLAUDE.md`, `AGENTS.md`, `.karmax/rules.md` and
  `CONTRIBUTING.md` (configurable through `repo_rules_files`) are added for
  work in that repository.

## Any chat

The office never names a platform. It posts to destinations written
`<channel-id>:<target>`, where `channel-id` is any channel in `comms.channels`.
Slack, Discord, Telegram and WhatsApp work as before. The new **`webhook`**
channel type adds any webhook-capable chat with config only:

```yaml
comms:
  channels:
    - id: gchat
      type: webhook
      settings:
        preset: googlechat        # googlechat | slack | discord | teams | mattermost | generic
        url: https://chat.googleapis.com/v1/spaces/AAA/messages?key=…&token=…
        url.spaces/BBB: https://chat.googleapis.com/v1/spaces/BBB/messages?…   # more targets
        inbound_token: <random>   # required on inbound posts (?token= or X-Karmax-Token)
```

* **Outbound:** posts to the target's URL. Google Chat threads by `threadKey`,
  so each PR or incident stays in one thread. Discord messages are split at
  2000 characters.
* **Inbound:** `POST /comms/<id>` on the webhook server. It understands Google
  Chat app events, Slack/Mattermost outgoing webhooks (form-encoded) and plain
  `{"text","user","channel","thread"}` JSON, so any bridge can feed it.

Chat commands, read on every channel except WhatsApp unless `listen_channels`
says otherwise:

```
approve K7F2Q            reject K7F2Q
fix INC-AB2CD [in org/repo]
merge payments-api#12    review acme/auth-service#3
create repo ledger-svc from go: double-entry ledger
index all | index payments-api
status
```

Free-form requests go to the orchestrator agent, which has these tools:
`orgops_status`, `orgops_report_incident`, `orgops_fix_incident`,
`orgops_approve`, `orgops_reject`, `orgops_review_pr`, `orgops_merge_pr`,
`orgops_create_repo`, `orgops_index`, `orgops_knowledge`,
`orgops_audit_repos`. So "payments is returning 500s since the deploy, fix it"
in Google Chat becomes an incident, then a fix, then a PR.

## Who writes the fix

`org_ops.fixer.dispatcher`:

* `harness` (the default): KARMAX's own Claude Code / Codex harness.
* `fleet`: `fleetctl run - --repo <repo> --task <inc> --wait` hands the brief
  to a free standby agent on the fleet.
* `sandbox`: a container from the sandbox driver (`fixer.image`).

The agent is told to push to `karmax/fix-<incident>` and finish with
`KARMAX_RESULT {"pr_url","root_cause","summary"}`. The office finds the PR by
its branch, so a fix still gets tracked if that line is missing. The PR loop
leaves merge decisions on `karmax/fix-*` branches to the incident policy.

## Endpoints (KARMAX webhook server)

| Path | What |
|---|---|
| `POST /orgops/github` | GitHub org webhook. Needs `github.webhook_secret` and HMAC SHA-256; it refuses to run unsigned. Events: pull_request, pull_request_review, check_suite. Use this **or** the GitHub connector's `/hooks/connectors/github`, not both. |
| `POST /orgops/alert` | Alerts (Alertmanager, Grafana, Sentry, generic JSON). Optional `alert_token`. |
| `POST /comms/<id>` | Inbound messages for each `webhook` chat channel. |

## Trying it locally, with no GitHub org

`cmd/fakegithub` is an in-memory GitHub covering the API the office uses (including reading branch protection back), plus
control endpoints that open PRs and set CI and send signed webhooks at KARMAX.

Requirements:

- **cgo and a C compiler.** KARMAX stores state in SQLite, which needs cgo. A
  `CGO_ENABLED=0` build compiles but dies at start with `go-sqlite3 requires cgo
  to work. This is a stub`. Install gcc (`build-essential` on Debian/Ubuntu) and
  build with `CGO_ENABLED=1`.
- **Harness auth.** Review summaries and knowledge cards run through the Claude
  Code harness. On a host with no `~/.claude` login, export
  `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) before `karmax start`;
  KARMAX passes that one variable to the harness. It never passes
  `ANTHROPIC_API_KEY` or other provider keys, which would switch Claude Code to
  API billing. Without any auth the review fails with `Not logged in`.
  Other variables can be let through with `KARMAX_HARNESS_ENV_PASSTHROUGH`
  (comma-separated), but that is the operator's explicit choice.
- **An agent that receives chat.** Chat is answered by the agent a comms channel
  names in `agent_id`. Agents only start at boot with `triggers.run_on_start:
  true`; KARMAX also starts any agent a channel or webhook route targets (and
  logs that it did), so messages are not dead-lettered. A channel naming an agent
  that does not exist is a config error.

```bash
CGO_ENABLED=1 go build -o fakegithub ./cmd/fakegithub && CGO_ENABLED=1 go build -o karmax ./cmd/karmax
./fakegithub -addr 127.0.0.1:8787 -webhook http://127.0.0.1:9090/orgops/github -secret dev -seed acme
# karmax.yaml: org_ops.github.api_url: http://127.0.0.1:8787, webhook_secret: dev, org: acme
./karmax start

# Chat: post plain JSON to a webhook-chat (a post with no message gets 400 and the reason)
# channel (comms channel id "chat", type "webhook", settings.preset "generic",
# settings.inbound_token "demo-token", agent_id naming a defined agent):
curl -XPOST '127.0.0.1:9090/comms/chat?token=demo-token' -H 'Content-Type: application/json' -d '{"text":"status"}'

# A PR that leaks a key → REQUEST_CHANGES review + chat summary
curl -XPOST 127.0.0.1:8787/_fake/pulls -d '{"repo":"acme/payments-api","title":"add stripe client","author":"dev1",
  "files":[{"filename":"stripe.go","patch":"@@ -1,1 +1,2 @@\n package main\n+var k = \"AKIAABCDEFGHIJKLMNOP\""}]}'
# A clean PR → review, then merge once CI is green (per policy)
curl -XPOST 127.0.0.1:8787/_fake/pulls -d '{"repo":"acme/payments-api","title":"feat: refunds","body":"adds refunds","author":"dev1",
  "files":[{"filename":"refunds.go","patch":"@@ -0,0 +1 @@\n+package main"},{"filename":"refunds_test.go","patch":"@@ -0,0 +1 @@\n+package main"}]}'
curl -XPOST 127.0.0.1:8787/_fake/ci -d '{"repo":"acme/payments-api","number":2,"state":"success"}'
# An alert → incident → (approve) → fix agent
curl -XPOST 127.0.0.1:9090/orgops/alert -d '{"title":"5xx on /charges","service":"payments","severity":"critical"}'
# Everything the office did
curl 127.0.0.1:8787/_fake/state
```

Seeded repositories: `acme/template-go-service`, `acme/payments-api` (with a
`CLAUDE.md` of rules) and `acme/auth-service`.

The fake does not run `git`, so against it a real fix agent cannot push. For
the full fix loop, point `api_url` at GitHub (or GHE) and give the harness or
fleet agents `gh` credentials for the demo org.

`go test ./internal/orgops/...` runs each flow end to end against the fake:
leaked-secret block, auto-merge on green, approval in chat (including a double
approve), incident to fix PR to merge to resolved, the default policy asking
first, probe hysteresis, the repo factory, knowledge indexing, rules reaching
the prompt, webhook signature checks, alert formats and command parsing.

## Configuration reference

See the `org_ops:` block in `karmax.yaml.example`. The fields are documented
in `internal/orgops/config.go`.
