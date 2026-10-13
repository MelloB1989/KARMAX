# KARMAX Org Office — presenter's run-sheet

## Opener (30 seconds)

> "One agent watching **every repo and production** for the whole org.
> The **rules are your Markdown**. The **autonomy is your YAML**. And it lives in **any chat** you already use."

Then: "Let me show you four things it does, all live."

## Setup (before the audience arrives)

1. Tunnel to the demo VM (placeholders — use the real user/host from the operator, never commit them):
   ```
   ssh -L 9393:127.0.0.1:9393 -L 8787:127.0.0.1:8787 <user>@<vm>
   ```
2. On the VM, in the KARMAX checkout: `demo/up.sh` — starts fake GitHub, chat, payments, KARMAX, then runs
   `demo/act.sh seed`, which fills the org: **web-app**, **notifications**, **infra** next to payments-api,
   auth-service and template-go-service. READMEs name their dependencies on each other; **notifications has no
   CODEOWNERS and no branch protection** (the one red row in the console); every other repo has README, CLAUDE.md, CODEOWNERS, CI and protection.
3. Open the **Org Office console** at **http://127.0.0.1:9393** on the laptop.
4. Fresh state before every full run: press **Reset** (`demo/act.sh reset`, = `down.sh && up.sh`, ~30 s) or run it by hand.
   Identical chat messages are refused for 6 h, so a second pass without a reset is silent.

## Console flow (every button is one `demo/act.sh <action>`)

Press in this order; each row is what the audience should see change.

| # | Button (act.sh) | Audience sees | Say |
|---|---|---|---|
| 0 | (nothing) | six repos, payments healthy, one red ✗ on `notifications` (no CODEOWNERS) | "This is a small org: services, a template, one repo that slipped through." |
| 1 | **Leak PR** (`leak-pr`) | PR *Add S3 receipt uploader* appears; within ~20 s a REQUEST_CHANGES review with an inline comment on the line `accessKey = "AKIA…"` (line 17); the chat post shows the key redacted | "A hard-coded key can never merge. Notice the comment sits on the exact line." |
| 2 | **Clean PR** (`clean-pr`) then **CI green** (`ci <pr> success`) | PR *feat: add validated refund helper* approved; "will merge once CI is green"; after CI green it merges | "Same reviewer, same rules — clean change, approved and merged on green." |
| 3 | **Break payments** (`break`) | health goes red (500); ~15 s later 🚨 incident, then 🟡 *Approval needed* with a code | "No pager. It noticed, and asks the owner once." |
| 4 | **Approve** (`say "approve <CODE>"`) | fix PR *fix: guard nil customer in refund handler* (the diff patches the refund handler in `demo/payments/main.go`), reviewed, merged; incident → ✅ Resolved; health green | "Fixed, reviewed like any PR, loop closed. The code is five characters, typed by a person." |
| 5 | **Create repo** (`say "create repo ledger-svc from go: double-entry ledger"`) | new repo appears with README, CODEOWNERS, CLAUDE.md, PR template, CI — and a green compliance tick | "Born compliant." |
| 6 | **Index all** (`say "index all"`), then ask | 📚 *Indexed N repositories* | "Repo docs become org memory." |
| 7 | Free-form (below) | the agent answers or opens an incident | "No commands to learn." |

If a button's result does not show within 30 s, check the chat panel for the office's message, then use the fallback in the scene section below.
Keep `demo/act.sh <action>` open in a spare terminal — the buttons call exactly that.

## Free-form beat (the agent, not the office commands)

The agent has `comms.send`, memory and all office tools and answers from the knowledge cards. Run **index all** first, and
**wait until the feed stops moving** (an answer is swallowed if anything else posts during its turn). Then type as Maya:

1. `What does payments-api depend on?` → auth-service (JWT), Postgres, Stripe, from the card.
2. `which repos are missing CODEOWNERS?` → `notifications` (it calls `orgops_audit_repos`).
3. `payments is throwing 500s since the last deploy` → it calls `orgops_report_incident`; an INC appears and the owner alert follows (run **Heal** first if the earlier break is still on, and note incidents on a service dedupe).

**Say:** "Same agent, no buttons — a sentence in chat becomes an incident."
**Fallback:** if the agent stays silent, `index payments-api` posts the card; the audit is `karmax tool call orgops_audit_repos` on the VM.

## Scene 1 — PR guardian  (`demo/scene1.sh`)

- **Audience sees:** a PR that adds a hard-coded AWS key → KARMAX posts **REQUEST_CHANGES** with an **inline comment** on the line, and a **redacted** summary in chat (the key never appears). Then a clean PR → approved and **merged once CI is green**.
- **Say:** "Every PR gets standards checks, a security scan, and a review against *your* written rules. A leaked secret can never be merged — that's a hard stop, not a suggestion."
- **If it stalls:** re-run `demo/scene1.sh`; open `/_fake/state` and point at `reviews` / `merges`; the model summary line needs harness auth (deterministic checks post without it).

## Scene 2 — Incident autopilot  (`demo/scene2.sh`, or `--auto-approve`)

- **Audience sees:** payments service starts returning 500s → probe trips → **INC-XXXXX** opened → **🚨 owner alert** in chat with a code → presenter types `approve <CODE>` → fix PR appears → reviewed → **merged on green** → probe sees recovery → incident closed.
- **Say:** "No pager, no ticket. It noticed, asked the owner once, fixed, reviewed its own fix like any other PR, and closed the loop."
- **Be honest on stage:** the fixer here is a **simulated engineer for the offline demo** (`demo/fakefixer.sh`) — the fake GitHub runs no git, so a real agent can't push. Track B (a real GitHub org + the real fix agent: Claude Code harness, or a fleet/sandbox agent with `gh` credentials) swaps only the fixer; probes, alerts, approvals, review and merge are the real code path.
- **Code:** type exactly the 5 characters shown, e.g. `approve K7F2Q` (digits count; case-insensitive).
- **If it stalls:** re-run scene 2 after `demo/down.sh && demo/up.sh`; `--auto-approve` posts the approval for you; show `/_fake/state` (`prs`, `merges`) and `curl 127.0.0.1:9494/health`.

### The autonomy beat (right after scene 2)

> "That asked a human because our policy says `incident_fix: approve`. Watch me change one line."

1. In `demo/karmax.yaml` change `incident_fix: approve` → `incident_fix: auto`.
2. `demo/down.sh && demo/up.sh` (restarts KARMAX and wipes state so the alert isn't deduped).
3. Re-run `demo/scene2.sh` — **no human needed**, no 🚨 approval prompt, straight to fix → merge → closed.
4. **Say:** "Human-free where the org allows it, governed where it doesn't. Same agent, one line of YAML — per org or per repo."
   (Flip it back to `approve` afterwards if you'll run it again.)

## Scene 3 — Repo factory  (`demo/scene3.sh`)

- **Command (typed in chat as Maya):** `create repo ledger-svc from go: double-entry ledger`
- **Audience sees:** `📦 Created *acme/ledger-svc* from acme/template-go-service`, then `Standard files: …` and `Branch protection on main: 1 review + checks ci`. The script then proves it from the fake GitHub: repo listed in `/_fake/state`, and README, CODEOWNERS, CLAUDE.md (the org rules!), PR template and CI present. Takes ~2 s.
- **Say:** "A new repo is born compliant — org standard files, the org's rules shipped as `CLAUDE.md` so any coding agent reads them, and branch protection. Not chased into compliance later."
- **If it stalls:** re-run needs a fresh name (`REPO=ledger-svc-2 demo/scene3.sh`) or `demo/down.sh && demo/up.sh`; `--auto-approve` if repo_create policy is `approve`; show `/_fake/state` `.created`.
- **Limit of the fake:** the branch-protection settings aren't readable back from the fake GitHub, so protection is shown by KARMAX's own chat line, not by a lookup.

## Scene 4 — Org knowledge  (`demo/scene4.sh`)

- **Commands (in chat):** `index all`, then `What does payments-api depend on?`
- **Audience sees:** `📚 Indexed N repositories into the knowledge store.` (each repo's README/CLAUDE.md distilled into a card by the model), then the answer: payments-api depends on **auth-service** (JWT validation), **Postgres** and **Stripe**.
- **Say:** "Every repo's own docs become org memory — refreshed on merge, on creation, nightly. Ask in plain English across the whole org and the answer comes from the code's own words."
- **If it stalls (no model answer in 25 s):** the script falls back to `index payments-api`, which posts the payments-api card (with its Dependencies bullets) into chat — say "here's the card the answer comes from". Re-running needs `demo/down.sh` (identical messages are refused).

## Gotchas that matter on stage

- **Approve code is exactly 5 characters** (`approve K7F2Q`). The office only takes a code it has pending; a typo is not an office command, so it goes to the agent instead. Retype it.
- **Identical messages are refused for 6 h** (chat de-dup). Running a scene twice produces silence the second time → always `demo/down.sh && demo/up.sh` between runs; it wipes `demo/data`.
- **Model summary needs harness auth.** `~/.config/karmax-demo/env` (`CLAUDE_CODE_OAUTH_TOKEN`) must be loaded in the shell that runs `up.sh`. Without it reviews/cards still post (deterministic checks / raw docs) but without the model's wording.
- **Free-form questions go to the orchestrator agent** (anything that isn't `approve`, `create repo`, `index`, `status`…); it needs the harness token and `run_on_start`.
- Commands are plain words so they work in any chat; KARMAX uses Slack-style `*bold*` and posts no thread ids with the generic preset.

## Rehearsal log (scenes 3 and 4, against the merged demo/karmax.yaml, fc05b2d)

- Scene 3: ~2 s, all of it as above — repo, CODEOWNERS, CLAUDE.md, PR template, CI, "Branch protection on main: 1 review + checks ci".
- Scene 4: `index all` → "📚 Indexed 4 repositories"; the free-form question is answered by the model from the cards: *"auth-service (validates JWTs), Postgres (its database), Stripe (the external payment provider) … no queues mentioned."*
  Run scene 4 **before** scene 2 (or on fresh state): if an incident is on file the model may add a remark about it.
- Needs `agents[].triggers.run_on_start: true` in the config (without it the agent never starts and free-form chat dead-letters with `agent demo-agent is not running`) and the harness token loaded (`up.sh` does both).
- Posting to `/comms/chat` by hand needs `Content-Type: application/json` (the scene scripts already send it).
- `demo/scene2.sh --auto-approve` posts `approve <CODE>` for you.
- The fake GitHub can't read back branch protection, so it is shown by KARMAX's chat line, not a lookup.
- The VM's `bin/karmax` must be built with cgo (`CGO_ENABLED=1`, needs `gcc`); a non-cgo build dies with `go-sqlite3 requires cgo to work. This is a stub`.

## Final rehearsal (operator, main 371d854, fresh `down.sh && up.sh`, scenes in pitch order)

Run twice end to end on the demo VM; both runs green.

| scene | time | result |
|---|---|---|
| 1 | 16 s | #1 REQUEST_CHANGES (key shown as `AKIAIO••••`, never in full), #2 APPROVE → merged on green CI |
| 2 | 44 s | 🚨 INC → 🟡 approval → `approve <CODE>` typed via the chat page → fix PR #3 reviewed, merged → ✅ Resolved, `/health` 200 |
| 3 | 2 s | `acme/ledger-svc` created with README, CODEOWNERS, CLAUDE.md, PR template, CI; protection line in chat |
| 4 | 24 s | `index all` → 4 repos; "What does payments-api depend on?" → auth-service (JWT), PostgreSQL, Stripe |

`/_fake/state` after the run: merges `acme/payments-api#2`, `acme/payments-api#3`.

### Known core issues visible on stage (not worked around in core)

- **The orchestrator agent also answers office commands.** After `approve <CODE>` it posts "👀 on it…", and after
  `index all` it can post "I can't run index all…" before the office's own "📚 Indexed …". Every chat message
  reaches both the office and the agent. Say: "that's the general assistant chiming in; the office did the work."
- **An agent's answer can be swallowed.** If anything else posts to the same chat target during the agent's turn,
  KARMAX withholds the answer: `chat already answered this turn; withholding the final text`
  (internal/agent/agent.go, `repliedDuringTurn`). scene4.sh waits for a quiet chat before asking. **Live, wait
  until the feed stops moving before typing the question.** If nothing comes, `index payments-api` shows the card.
- The agent's session reaches KARMAX through the `karmax` CLI and the API (`api:` on 127.0.0.1:9091 in
  demo/karmax.yaml, `bin/` on PATH in up.sh). Without both, it answers "the KARMAX API is unreachable" and
  "karmax: command not found".
- Memory retrieval logs `proactive memory retrieval failed … api.anthropic.com` on each turn, because the
  agent's API provider is a placeholder (the harness answers first). It's harmless for the demo.
