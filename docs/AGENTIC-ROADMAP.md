# KARMAX Agentic Roadmap

**Goal:** evolve KARMAX from an orchestration daemon into a full agentic clone —
a system that knows the operator deeply, acts on their behalf across their
digital life, learns from every interaction, and proposes its own automations.

This plan is grounded in a study of two systems (2026-07-04):
[NousResearch hermes-agent](https://github.com/NousResearch/hermes-agent) (agentic
behavior, self-improvement, routines) and [Supermemory](https://supermemory.ai)
(memory infrastructure), compared against KARMAX's current architecture.

---

## 1. What the studied systems do

### Hermes-agent (agentic behavior)

| Mechanism | How it works |
| --- | --- |
| **Background self-improvement review** | After every turn, a forked agent (daemon thread, tool-whitelisted to memory+skill tools only) replays the conversation snapshot and asks itself "should any skill/memory be saved or updated?" Writes go straight to the stores; the main conversation and prompt cache are never touched. |
| **Skills as learned procedures** | The agent authors its own `SKILL.md` files (via `/learn` or the background review) — markdown SOPs with strict authoring standards, indexed in the system prompt by 60-char descriptions. Skills are *procedural* memory ("how I do X"), distinct from facts. |
| **Curator** | An idle-triggered background task that maintains agent-created skills: consolidates duplicates, archives stale ones, patches errors. Never deletes; pinned skills are immune. |
| **Consent-first automation suggestions** | Every automation proposal flows through ONE surface: suggestions. Four sources — curated catalog, skill blueprints, **usage-pattern review** ("you keep asking this; want a scheduled job?"), and integration connections (connected Gmail → offer mail automations). The user accepts with one tap (creates a real cron job) or dismisses (latched by dedup key, never re-offered). Suggestions never auto-create jobs. |
| **MemoryProvider abstraction** | One pluggable interface: `prefetch(query)` pre-turn (background-threaded, cached), `sync_turn` post-turn, `on_session_end` (full-conversation ingest), `on_pre_compress` (extract before compaction discards), `on_delegation` (parent remembers subagent task+result), `on_memory_write` (mirror built-in writes). Exactly one external provider at a time. |
| **Built-in curated memory** | Two files: `MEMORY.md` (agent's world model) + `USER.md` (about the user). Frozen snapshot in the system prompt (prompt-cache friendly); live writes to disk apply next session. |
| **Routines** | Cron jobs with prompts + delivery channel, webhook subscriptions with payload templating (`{pull_request.title}`), script pre-processing. KARMAX's loops already match or exceed this. |

### Supermemory (memory infrastructure)

| Mechanism | How it works |
| --- | --- |
| **Ingestion pipeline** | queued → extract → chunk → embed → index. Documents become "memories": semantic chunks with meaning, embedded for similarity, connected through relationships. |
| **Knowledge-graph relationships** | Three edge types: **Updates** (new info supersedes old, tracked via `isLatest` — conflicts are versioned, not deleted), **Extends** (enriches without replacing), **Derives** (inferred connections). |
| **User profile: static + dynamic** | Static = stable identity facts (role, preferences, expertise). Dynamic = recent episodic context (current projects, upcoming events). Profiles "build themselves" from conversations via fact extraction that handles knowledge updates, temporal change, and forgetting. Injected into prompts as the broad foundation; search covers specifics. |
| **Conversations endpoint** | Full sessions ingested once at session end — this (not per-message writes) drives entity extraction and profile building, while keeping a retrievable transcript. |
| **Container tags** | The isolation boundary (per user / project / agent). Hermes uses `hermes-{identity}` per profile. |

Notably, hermes-agent ships a first-party **supermemory plugin**: auto-recall
before each turn, cleaned turn capture after, one full-conversation ingest per
session, and explicit save/search/forget/profile tools.

---

## 2. Where KARMAX stands

**Already strong (keep, don't rebuild):**
- Event-driven core: bus, comms webhooks (zero-polling WhatsApp), loops with 4
  trigger types (schedule/webhook/event/manual) + a public marketplace.
- Deterministic outcome discipline: ACTED→notify, APPROVE→approvals inbox,
  REMIND→phone reminder. Consent-first for irreversible actions.
- Delegation with continuity: `claude_code.call` session resume, operator
  context injection, full CLI parity so executors can reach every harness tool.
- Memory hygiene primitives: importance/pinned/TTL, forgetting curve
  (memory-maintenance loop), hot/cold chat pipeline, ABOUT_ME profile,
  agentic `memory.retrieve` sub-agent, page-index tree.

**Gaps this plan closes:**

| # | Gap | Evidence |
| --- | --- | --- |
| G1 | **Retrieval is lexical, not semantic.** `Manager.SearchSemantic` is keyword LIKE-matching over SQLite; there are no embeddings anywhere in the codebase. "Call the doctor" will never recall "phone Dr. Reddy". | `internal/memory/manager.go:546` |
| G2 | **No post-turn learning loop.** Facts are only saved when the main model *decides* to call `memory.ingest` mid-turn — the weakest link, especially on small models. Delegation results (`claude_code` outputs) are truncated into coding_sessions and never distilled into memory. | hermes `background_review.py`, `on_delegation` |
| G3 | **No procedural memory.** KARMAX loops are compiled Go — great for automations, far too heavy for "here's how I like my invoices chased". There is nowhere to store learned *procedures*. | hermes skills |
| G4 | **No memory versioning.** Contradictions are handled only by explicit `memory.forget`; there are no supersedes/extends links and no `isLatest`. (`memory_link_store.go` exists but holds only page-index links.) | supermemory Updates/isLatest |
| G5 | **Profile is one static blob.** ABOUT_ME.md mixes "who I am" with "what I'm doing this week"; the dynamic half goes stale between 12h profile-refresh runs. | supermemory static/dynamic split |
| G6 | **Automations aren't self-proposing.** The loops marketplace exists, but nothing watches usage and says "you ask for this every Monday — install/schedule it?" | hermes suggestions |
| G7 | **Episodes vanish.** Chat compaction summarizes-and-discards; sessions are never ingested as retrievable episodes. | supermemory conversations endpoint |
| G8 | **Digital-life coverage is partial.** WhatsApp (deep), Google Chat, calendar/reminders, Google Workspace via gws. Missing as first-class surfaces: email triage, GitHub, browsing/research on a schedule, and a learned *voice* for writing as the operator. | — |

---

## 3. The plan

### Phase 1 — Memory core: semantic, versioned, layered  *(foundation; do first)*

**1a. `MemoryProvider` interface (Go), hermes-style.**
One interface in `internal/memory`: `Prefetch(query)`, `SyncTurn(user, asst)`,
`OnSessionEnd(messages)`, `OnDelegation(task, result)`, `Search`, `Ingest`,
`Forget`, `Profile()`. Two implementations:
- `builtin` — today's SQLite store behind the interface (no behavior change).
- `supermemory` — their v3/v4 API: container tag per namespace (`karmax-nexus`),
  turn capture, session-end conversation ingest, profile endpoint, hybrid
  search. ~a day of work; instantly buys embeddings, entity extraction,
  auto-profile, and Updates/isLatest semantics.

*Privacy decision (operator's call):* supermemory means the distilled contents
of your digital life live with a third party. Recommended posture: **local-first
default, supermemory optional** behind the interface — and that's exactly why
the interface comes first. A later `local-vec` provider (sqlite-vec + any
embedding endpoint) can close G1 privately.

**1b. Memory versioning in the builtin store.**
Add `supersedes` edges + `is_latest` flag (reuse `memory_link_store`).
`memory.ingest` gains an optional `updates: <id|query>` param; retrieval
filters to `is_latest` but can walk history. Contradiction handling stops
being delete-only. The cleanup/correction flow marks superseded instead of
deleting.

**1c. Split the profile: ABOUT_ME.md (static) + NOW.md (dynamic).**
- ABOUT_ME: identity, relationships, preferences, long-term goals —
  `profile-refresh` keeps it (12h is fine for static facts).
- NOW: active projects, this week's commitments, open threads, current focus —
  rewritten by `hot-sync` every 4h from fresh WhatsApp/memory signal.
- Both injected every turn (NOW is small); `karmax` CLI + Kit expose both.

**1d. Episodic ingest.**
On compaction and app-conversation reset, the summary model distills the
discarded messages into an *episode* memory (`kind=episode`, linked to
mentioned entities) — mirroring `on_pre_compress` + the conversations-endpoint
pattern. Sessions stop vanishing.

### Phase 2 — The learning loop  *(what makes it feel alive)*

**2a. Post-turn background review.**
After each operator-facing turn (comms message or app chat), fire a cheap
background pass — summary model first, harness fallback — with exactly one
question: *"durable facts, corrections to existing memory, or a procedure worth
saving?"* Output grammar: `FACT:` / `SUPERSEDE <id>:` / `SKILL:` / `NOTHING`.
Writes go through `memory.ingest` / skills store. Never blocks the turn; never
touches main history. This removes the dependency on the main model
remembering to remember (G2) — critical while the brain is a mini model.

**2b. Delegation observation.**
Every `claude_code.call` completion feeds the background review with
(task, final output): "did we learn anything about the operator's projects,
prefs, or infrastructure?" Today that knowledge dies in a 5000-char truncated
session row.

**2c. Skills — procedural memory.**
`~/.karmax/skills/<name>.md` (frontmatter: name, description ≤60 chars, tags,
pinned). New agent tools `skill.save` / `skill.list`; skill index (name +
description only) injected into the system prompt; bodies fetched on demand
via `memory.retrieve`-style relevance + injected into `claude_code.call`
context when matched. Learned three ways: the operator says "remember how to
do this", the background review proposes one, or `karmax skills learn <desc>`.
A monthly `skill-curator` loop (marketplace) consolidates/archives — never
deletes, pinned immune (hermes invariants).

**2d. Voice profile — write as the operator.**
A distilled style guide (`VOICE.md`) learned from the operator's own sent
messages (wacli `--from-me` corpus, which cold-scan already reads): greeting
habits, formality by audience, emoji usage, typical message length, language
mixing. Injected into every proxy prompt (`wa-monitor`, `chat-sweep`,
`act-on-pending`) so "acts in the operator's voice" is grounded in evidence,
not the model's guess. Refreshed monthly by a loop. This is the single
highest-leverage "clone" feature.

### Phase 3 — Proactive autonomy: the assistant proposes its own automations

**3a. Suggestions pipeline (consent-first, hermes model).**
One store + API (`/api/suggestions`) + app inbox section. A suggestion =
ready-to-run automation spec: either *install marketplace loop X*, *create
YAML prompt-loop*, or *schedule job*. Accept = it happens; dismiss = latched
by dedup key, never re-offered. **Never auto-created** — same philosophy as
the approvals inbox, so it lands in familiar UX.

**3b. Suggestion sources** (each is itself a loop):
- `usage-review` (weekly): reads the event log + app conversations, finds
  recurring asks and repeated manual actions → proposes loops. ("You've asked
  for a CampX payment status 4 Mondays running — want a Monday-morning
  check-in loop?")
- `integration-scan`: notices reachable tools (gws authed? gh authed? new
  wacli chats monitored?) → proposes the obvious automations from the
  marketplace catalog (`default`-tagged loops it doesn't have installed).
- Marketplace catalog: `karmax loops browse` data, surfaced in the app.

**3c. Autonomy policy file.**
`~/.karmax/policy.yaml`: per-surface act/approve/never rules (e.g. whatsapp:
act for monitored chats, approve for money; email: draft-only; social: never
without approval). Today this policy lives inside prompts; extracting it makes
the clone's boundaries explicit, auditable, and editable from the app. Enforced
where it's deterministic (propose-tool kinds), injected where it's judgment.

### Phase 4 — Digital-life coverage (all as marketplace loops)

| Loop | Trigger | What it does |
| --- | --- | --- |
| `mail-triage` | every 15m (gws) | New-mail sweep: junk→skip, FYI→digest, needs-reply→APPROVE with draft in operator's voice, operator-only→REMIND. The missing sibling of `wa-monitor`. |
| `calendar-prep` | 07:30 daily | For each meeting today: pull related memory/episodes/chat context → briefing note per meeting. |
| `gh-watch` | event/webhook | Mentions, review requests, failing CI on owned repos → act (claude_code) or APPROVE. `gchat-watch` pattern applied to GitHub. |
| `commitment-tracker` | daily | Cross-references promises in memory ("X said they'd get back by Friday") against reality; nudges or REMINDs. |
| `weekly-review` | Sunday | Digest of what the clone did all week (acted/approved/reminded/learned), plus suggestions. Trust is built by legibility. |

Each ships to the marketplace; the Kit API (Ask/Harness/Summarize/Propose/
Remind/memory) is already sufficient for all of them — Phase 4 needs **no core
changes**, which validates the extensibility work already done.

---

## 4. What NOT to adopt

- **Hermes's multi-provider plugin registry** — one Go interface with 2–3
  implementations is enough; KARMAX is a personal system, not a framework.
- **Skills-as-code** — KARMAX already has compiled loops for code; skills stay
  markdown-only (prompts/SOPs) to keep the boundary crisp.
- **Per-turn threaded prefetch caching** — KARMAX's proactive `memory.retrieve`
  already covers this; revisit only if turn latency becomes a problem.
- **Supermemory as the *only* memory** — the daemon must keep working offline
  and private; hosted memory is an optional provider, never a dependency.

## 5. Sequencing & effort (rough)

| Phase | Effort | Depends on |
| --- | --- | --- |
| 1a provider interface + supermemory impl | 1–2 days | — |
| 1b versioning (supersedes/isLatest) | 1 day | — |
| 1c profile split | 0.5 day | — |
| 1d episodic ingest | 0.5 day | 1b |
| 2a background review | 1 day | 1b |
| 2b delegation observation | 0.5 day | 2a |
| 2c skills | 1–2 days | — |
| 2d voice profile | 1 day | — |
| 3a suggestions pipeline + app UI | 2 days | — |
| 3b suggestion-source loops | 1 day | 3a |
| 3c policy file | 1 day | — |
| 4 surface loops | 0.5–1 day each | Kit as-is |

The order matters: **memory quality (1) → learning (2) → proactivity (3) →
coverage (4)**. A clone that acts everywhere but remembers badly is worse than
one that acts in two places and never forgets.
