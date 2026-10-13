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
3. `payments is throwing 500s since the last deploy, please open an incident` → the agent calls `orgops_report_incident`
   and an 🚨 INC appears in chat. **Known flaw:** the model guesses the tool's argument names, so the incident can show an
   empty title and "I can't map this to a repository"; the answer is then `fix INC-XXXXX in acme/payments-api`. For a clean
   incident story use the **Break payments** button (scene 2), which goes through the probe. Without "please open an
   incident" the agent often investigates and asks questions instead. Treat this item as optional.

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
- **If it stalls (no model answer in 75 s):** the script falls back to `index payments-api`, which posts the payments-api card (with its Dependencies bullets) into chat — say "here's the card the answer comes from". Re-running needs `demo/down.sh` (identical messages are refused).

## Gotchas that matter on stage

- **Approve code is exactly 5 characters** (`approve K7F2Q`). The office only takes a code it has pending; a typo is not an office command, so it goes to the agent instead. Retype it.
- **Identical messages are refused for 6 h** (chat de-dup). Running a scene twice produces silence the second time → always `demo/down.sh && demo/up.sh` between runs; it wipes `demo/data`.
- **Model wording needs harness auth.** `up.sh` loads `~/.config/karmax-demo/env` (`CLAUDE_CODE_OAUTH_TOKEN`); KARMAX passes that one variable to every Claude Code session. Without it reviews and cards still post (deterministic checks, raw docs), without the model's wording.
- **Free-form messages go to the agent; office commands do not.** `approve <CODE>`, `create repo …`, `index …`, `status` are handled by the office alone; anything else is answered by the agent with the office's tools. A question asked while the feed is still moving is fine.
- Commands are plain words so they work in any chat. With the generic preset every PR and incident carries a `thread` key, which is how the console groups them.
- Deploy is `git pull && demo/down.sh && demo/up.sh` (it builds every time, cached). The console needs no restart for UI-only changes: reload the page.

## Final rehearsal (operator, main 0d34cff, fresh stack, every step through the console's buttons)

| beat | time | what the audience saw |
|---|---|---|
| Leaked PR + clean PR | 18 s | #1 changes requested, the comment under line 17 (`AKIAIO••••` in chat); #2 approved, merged on green CI |
| Incident | 47 s | Production turns red → INC with 🚨 and the approve code → `approve` → fix PR #3 reviewed, merged → healthy, all 7 stages ticked |
| Repo factory | 2 s | `ledger-svc` arrives with a NEW tag and every compliance tick, protection `1 review · ci` |
| Knowledge | 32 s | "What does payments-api depend on?" → auth-service (JWT), Postgres, Stripe; web-app and notifications depend on it |
| Audit (free-form) | 12 s | "which repos are missing CODEOWNERS?" → only acme/notifications (the red row) |
| Incident in plain English | ~30 s | "checkout on web-app is timing out for some customers" → INC opened on acme/web-app, owners alerted, approval asked |

Fixed in KARMAX core during preparation (no workarounds left in the demo): office commands are answered once
(the office claims them before the agent sees them); an agent's answer is no longer withheld when the office posts
during its turn, nor lost when its own `comms.send` fails; JSON chat posts are accepted whatever the content type and
the generic preset carries thread keys; the harness gets `CLAUDE_CODE_OAUTH_TOKEN`; chat-routed agents start; the fake
GitHub reads branch protection back; an agent in a Claude Code session receives its configured system prompt; and
`karmax tool call` checks arguments against the tool's parameters (`karmax tool show <name>` lists them).
