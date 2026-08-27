# Console API

Everything the admin console (`app/console/*`) needs from the Go server. It
does not exist yet — the console is built and fully demoable against mock
data behind a flag (see "Mock mode" below); this document is what the next
worker implements so the console can point at the real thing.

All endpoints are namespaced under `/api/console/` (separate from the phone
app's `/api/*` endpoints already in `internal/api/server.go`, so the two
clients can evolve independently). The console is served by the same binary
at the same origin, so there is no CORS concern and no base URL to configure
client-side.

## Conventions

- All request/response bodies are JSON. `Content-Type: application/json`.
- Timestamps are RFC3339 strings (`time.RFC3339`), UTC. A nullable timestamp
  is `null`, never an empty string.
- IDs are opaque strings (the store's UUIDs).
- Every error response is `{"error": "human-readable message"}` with a
  non-2xx status, matching the existing `writeJSON` convention in
  `internal/api/server.go`.
- Every endpoint below requires a console session (see Auth), sent as
  `Authorization: Bearer <token>`, EXCEPT the two auth bootstrap/status calls.
  This is a separate token from `KARMAX_API_TOKEN` (which gates the phone
  app's `/api/*`) — the console has human operators with names and roles, the
  phone app has one bearer secret.
- List endpoints return `{"<plural>": [...]}, e.g. `{"cases": [...]}` —
  matching the existing convention in `internal/api/server.go` (`{"loops":
  [...]}`, `{"proposals": [...]}`, etc.), not a bare array. Detail endpoints
  return the object directly (or wrapped once, as noted per-endpoint).

## Mock mode

`app/console/src/api/` has one file per resource (`cases.ts`, `agents.ts`,
`recipes.ts`, `approvals.ts`, `audit.ts`, `connectors.ts`, `settings.ts`,
`auth.ts`) plus `types.ts` for the wire types and `mock/data.ts` for
fixtures. Every function checks `USE_MOCK` (from `src/api/config.ts`,
default ON) and either returns fixture data or calls the real endpoint below.
Append `?mock=0` to the console's URL once to switch it to real calls
(persisted in `localStorage`); `?mock=1` switches back. Nothing here needs to
change in the frontend once the backend exists — just flip the flag.

---

## Auth

Self-hosted, single org, no signup. The very first admin is created once via
`bootstrap`; every login after that goes through `login`.

### `GET /api/console/auth/bootstrap-status`

No auth required. Response:

```json
{ "needs_bootstrap": true }
```

`true` iff no console user has ever been created (e.g. an `org_members` /
console-users table is empty). The frontend shows a "create the first admin"
form instead of a login form when this is true.

### `POST /api/console/auth/bootstrap`

No auth required. Only succeeds once (returns 409 if an admin already
exists — re-check `needs_bootstrap` server-side, don't trust the client).

Request:

```json
{ "name": "Nikhil", "member": "nikhil", "password": "…" }
```

Response (200): a `Session`, and the account is created with role `admin`.

```json
{ "token": "…", "member": "nikhil", "name": "Nikhil", "role": "admin" }
```

### `POST /api/console/auth/login`

No auth required.

Request: `{ "member": "nikhil", "password": "…" }`
Response (200): a `Session` (same shape as bootstrap). 401 on bad credentials.

### `GET /api/console/auth/me`

Auth required. Returns the caller's `Session` (200), or 401 if the token is
invalid/expired.

### `POST /api/console/auth/logout`

Auth required. Invalidates the session token server-side. 200, empty body.

---

## Cases

Backed by `store.Case` / `store.CaseEvent` / `store.SandboxRun` exactly as
defined in `docs/org-platform.md`. This is a straight read-through — no new
store methods needed beyond `ListCases`, `CaseByID`, `CaseHistory`,
`ListSandboxRuns`, all of which already exist.

### `GET /api/console/cases`

Query params (all optional): `agent`, `state`, `limit` (default 50). Maps
directly onto `Store.ListCases(agent, state, limit)`.

Response:

```json
{ "cases": [ Case, ... ] }
```

`Case`:

```ts
{
  id: string; org: string; agent: string; key: string; title: string;
  state: "open"|"grooming"|"ready"|"building"|"review"|"done"|"dropped";
  namespace: string;
  thread_channel: string; thread_ts: string;
  created_at: string; updated_at: string;
}
```

(`state` is free text per the schema — the console renders any unrecognised
value with a neutral fallback chip, so new pack-defined states don't break
it. The seven listed are the ones packs currently use.)

### `GET /api/console/cases/{id}`

`{id}` is the case's store ID (`CaseByID`) — the console never round-trips
the `key` for this one, since keys can contain `/` and `:`.

Response:

```json
{
  "case": Case,
  "events": [ CaseEvent, ... ],
  "sandbox_runs": [ SandboxRun, ... ]
}
```

`events` from `Store.CaseHistory(id, limit)` (use a generous limit, e.g. 200
— the console doesn't paginate this yet), oldest first. `sandbox_runs` from
`Store.ListSandboxRuns(id, limit)`.

`CaseEvent`:

```ts
{ id: string; case_id: string; kind: string; payload: string /* raw JSON, console parses it for display */; actor: string; created_at: string }
```

`SandboxRun`:

```ts
{
  id: string; case_id: string; driver: string; container_id: string; image: string;
  status: "starting"|"running"|"exited"|"failed"|"gone";
  repo: string; branch: string; task: string;
  started_at: string; finished_at: string | null;
  exit_code: number; error: string; log_tail: string;
}
```

404 with `{"error": "..."}` if the case doesn't exist.

---

## Agents

Built from `agent.Registry` (installed packs) plus `Broker.Describe(subject)`
for grants (see `internal/broker/broker.go` — this renderer already exists
and produces exactly the "may open PRs in acme/api" style strings; the
console just displays its output verbatim, one bullet per string). The
subject to describe for agent `id` is whatever convention the agent runtime
uses for its broker subject (e.g. `"agent:" + id` — pick whatever the runtime
already grants agents under; there is no new grant scheme to invent here).

### `GET /api/console/agents`

Response: `{ "agents": [ AgentSummary, ... ] }`

`AgentSummary`:

```ts
{
  id: string; name: string; description: string; tags: string[];
  model: string; provider: string;
  status: "idle"|"running"|"paused"|"stopping"|"stopped"|"failed"|"crashed"; // agent.AgentStatus
  open_cases: number; // count of this agent's cases where state is not "done" or "dropped"
  grants: string[]; // Broker.Describe(subject) output, verbatim
}
```

### `GET /api/console/agents/{id}`

Response: `AgentDetail` — `AgentSummary` plus:

```ts
{
  persona: string; // AgentDef.SystemPrompt, in full
  tools: string[]; // AgentDef.Tools (built-ins/connector tools; excludes MCP-only names — see mcps)
  mcps: string[]; // AgentDef.MCPs
  restart_policy: string; // AgentDef.RestartPolicy
  triggers: {
    webhooks: string[];
    schedules: { cron: string }[];
    events: string[];
    run_on_start: boolean;
  };
}
```

404 if no agent with that ID is registered.

---

## Recipes

Backed by `internal/recipes`. List/get read from wherever installed recipes
live (`~/.karmax/recipes/*.yaml` per the package doc, plus `internal/recipes/builtin`
for the shipped ones); `generate` calls the compiler
(`internal/recipegen`, owned by a different worker on this branch — this
endpoint is the seam between the two). `enable`/`disable` just flip
`Recipe.Enabled` and persist; `POST /recipes` writes a NEW recipe file to
disk with `enabled: false` REGARDLESS of what the draft or caller says — a
generated recipe must never be born live. Enabling is always a separate,
explicit second call.

### `GET /api/console/recipes`

Response: `{ "recipes": [ RecipeSummary, ... ] }`

`RecipeSummary`:

```ts
{
  name: string;
  source: "builtin" | "generated"; // builtin = internal/recipes/builtin, generated = anything created via POST below
  enabled: boolean;
  trigger: { event: string; schedule: string; webhook: string; manual: boolean }; // recipes.Trigger, verbatim
  trigger_label: string; // human summary, e.g. "daily at 08:30", "on jira.issue.created", "manual" — render server-side from `trigger` so the console never re-implements cron parsing
  steps: number; // len(Recipe.Steps)
  updated_at: string; // file mtime
}
```

### `GET /api/console/recipes/{name}`

Response: `RecipeDetail` — `RecipeSummary` plus:

```ts
{
  yaml: string; // the raw file content
  permissions: string[]; // recipes.Describe(r) output, verbatim — already includes verb descriptions, named tools ("call the tool X"), and raw grants ("hold the grant X")
}
```

404 if no recipe by that name.

### `POST /api/console/recipes/generate`

The natural-language builder. Request:

```json
{ "description": "Every Monday morning, find Jira reviews stuck for 2+ days and nudge #eng." }
```

Calls the compiler (`internal/recipegen`) to turn the description into a
recipe, then runs it through `recipes.NewDryRun` and `recipes.Describe`.
**Nothing is written to disk by this call.** Response:

```ts
{
  draft_id: string; // opaque handle if the implementation wants to cache the draft server-side; the console currently just re-POSTs the yaml verbatim to /recipes, so this can be a no-op UUID if there's nowhere convenient to cache it
  name: string; // proposed recipe name
  yaml: string; // generated YAML, human-editable before saving
  trigger_label: string;
  dry_run: string[]; // DryRun.Report() lines, one action per array entry (strip the numbering — the console adds its own)
  permissions: string[]; // recipes.Describe() output for the generated recipe
  warnings: string[]; // anything the compiler wants to flag (e.g. "no matching connector for jira:ACME yet") — empty array if none
}
```

If generation fails (bad description, compiler error), respond 422 with
`{"error": "..."}` — the console shows it as a validation message, not a
crash.

### `POST /api/console/recipes`

Saves a recipe as a **disabled** file, whether it came from the builder or
was hand-written. Request:

```json
{ "name": "stale-review-nudge", "yaml": "name: stale-review-nudge\n..." }
```

Parses via `recipes.Parse` first (reject with 422 + the `*recipes.Error`'s
message/line/fix on failure); on success, writes the file with
`enabled: false` forced regardless of what the YAML says, and responds 200
with the resulting `RecipeDetail`. 409 if a recipe with that name already
exists (use `PUT` below to edit one).

### `PUT /api/console/recipes/{name}`

Request: `{ "yaml": "..." }`. Overwrites an existing recipe's YAML in place
(enabled state is preserved from the existing file — this is an edit, not a
re-creation). Same `recipes.Parse` validation as above. Response:
`RecipeDetail`.

### `POST /api/console/recipes/{name}/enable`

No body. Flips `enabled: true` and persists. Response: `RecipeDetail`. This
is the ONLY way a generated recipe goes live — the console's UI makes this a
distinct, deliberate button, never a side effect of saving.

### `POST /api/console/recipes/{name}/disable`

No body. Flips `enabled: false` and persists. Response: `RecipeDetail`.

---

## Approvals

Mirrors what the Slack integration shows via `Kit.ProposeTo(role, title,
summary, action)`. This is very likely the SAME underlying proposal
mechanism as the phone app's `/api/proposals` (see
`internal/api/server.go`'s `handleProposals`/`handleProposalDecision`), just
exposed under `/api/console/` with two additions the phone app doesn't need:
a link to the case that raised it, and the `role`/`channel` context so the
operator can see this is the exact thing Slack showed. If the underlying
store row genuinely has no case linkage, `case_id`/`case_key` are `null` —
see `appr_3`/`appr_4` in the mock fixtures for that shape (a stale-ticket
cleanup and a dependency bump proposal, neither tied to a case).

### `GET /api/console/approvals`

Query param (optional): `status` = `pending`|`approved`|`rejected`|`executed`|`failed`.
Omit for all.

Response: `{ "approvals": [ Approval, ... ] }`, newest `created_at` first.

`Approval`:

```ts
{
  id: string; kind: string; // e.g. "pr_merge", "generic" — whatever ProposeTo's caller tags it
  title: string; summary: string; context: string; action: string; // action = what executing this would do, shown as "Will do: <action>"
  case_id: string | null; case_key: string | null;
  agent: string; role: string; channel: string; // channel = where this was also posted in Slack, "" if nowhere
  status: "pending"|"approved"|"rejected"|"executed"|"failed";
  note: string; result: string; // note = operator's decision comment, result = what happened after approval (e.g. "Merged PR #1044")
  created_at: string; decided_at: string | null; decided_by: string | null; // decided_by = the human member id
}
```

### `POST /api/console/approvals/{id}/decision`

Request:

```json
{ "decision": "approve", "note": "go ahead" }
```

`decision` is `"approve"` or `"reject"`; `note` optional. Same semantics as
`internal/api/server.go`'s `handleProposalDecision`: approving executes the
action (async — respond immediately with `status` still transitioning, the
console polls `GET /api/console/approvals` to see it settle to
`executed`/`failed`); rejecting with a note feeds it back for revision,
rejecting without one just drops it. 409 if it's already been decided.

Response: the updated `Approval`.

---

## Audit

Read-only, straight through `Store.QueryAudit(AuditFilter)`.

### `GET /api/console/audit`

Query params (all optional, all AND-ed together):

| param | maps to |
|---|---|
| `actor_id` | `AuditFilter.ActorID` |
| `agent` | `AuditFilter.Agent` |
| `case_id` | `AuditFilter.CaseID` |
| `verb` | `AuditFilter.Verb` — the console sends this as a substring filter, so either do a `LIKE` server-side or document that it must be exact and the console will switch to exact-match filtering |
| `since` | `AuditFilter.Since`, RFC3339 |
| `limit` | `AuditFilter.Limit`, default 100 |

Response: `{ "events": [ AuditEvent, ... ] }`, newest first.

`AuditEvent` (matches `store.AuditEvent` field-for-field):

```ts
{
  id: string; actor_kind: "human"|"agent"|"recipe"|"system"; actor_id: string;
  agent: string; case_id: string; recipe: string; step: string;
  verb: string; target: string; decision: string; detail: string;
  created_at: string;
}
```

`case_id`/`agent`/`recipe`/`step`/`target`/`decision`/`detail` may be `""` —
the console renders empty string as `—`, never `null` here (matches the
store's plain-string columns).

---

## Connectors

Wraps `internal/connectors.Host` (Slack/GitHub/Jira, per the file-ownership
table — `internal/comms/slack`, `internal/connectors/github`,
`internal/connectors/jira`). "Setup" is entirely static/computed instruction
text plus this server's own callback URLs filled in — no third-party API
calls happen until the operator submits credentials.

### `GET /api/console/connectors`

Response: `{ "connectors": [ ConnectorSummary, ... ] }`

```ts
{
  id: string; // "slack" | "github" | "jira"
  name: string; kind: "slack"|"github"|"jira";
  status: "healthy"|"degraded"|"failed"|"not_configured";
  detail: string; // one line, e.g. "Connected as @karmax to Acme Inc" or "Token expires in 3 days"
  last_checked_at: string | null;
}
```

`not_configured` = no credentials saved yet (`Host.Get(id)` registered but
`Store.Credential(id)` empty). `healthy`/`degraded`/`failed` come from the
connector's own last health check (see below) — cache the last result rather
than re-checking on every list call, since this is a status DISPLAY not a
live poll.

### `GET /api/console/connectors/{id}/setup`

Response:

```ts
{
  id: string;
  steps: { title: string; body: string; value?: string }[]; // ordered instructions; `value`, when present, is something to copy — e.g. a callback URL with THIS server's real host filled in, never a placeholder
  fields: { key: string; label: string; type: "text"|"secret"|"url"; placeholder?: string; required: boolean }[];
  callback_url: string; // the single most important URL to copy, surfaced separately in case the console wants to feature it
}
```

`steps`/`fields` are effectively static per connector kind (see
`mock/data.ts`'s `CONNECTOR_SETUPS` for the exact copy the console already
ships — reuse that wording, or close to it, so the demo and the real thing
don't diverge). The one dynamic part is that every URL must be THIS server's
actual reachable address (same logic as `localAddresses()` in
`internal/api/server.go`, or the operator's configured public hostname if
there is one), not a placeholder — a setup wizard with a fake callback URL
is worse than no wizard.

### `POST /api/console/connectors/{id}/credentials`

Request: `{ [fieldKey: string]: string }` — one entry per `fields[].key`
from the setup response.

Saves via the existing credential store (`Store.Credential`/whatever
`Host.credentials` reads), does NOT itself hit the third-party API. Response:
the updated `ConnectorSummary` (status likely flips to whatever the
"unchecked" state is — the console tells the operator to run the health
check next).

### `POST /api/console/connectors/{id}/health-check`

No body. Makes a real, cheap call to the connector (e.g. Slack
`auth.test`, GitHub `GET /app`, Jira `GET /myself`) and returns:

```ts
{ status: "healthy"|"degraded"|"failed"|"not_configured"; detail: string; checked_at: string }
```

Also persist this as the connector's cached status so the next `GET
/connectors` reflects it.

---

## Settings

### `GET /api/console/settings`

Response:

```ts
{
  model_providers: {
    id: string; name: string; base_url: string;
    has_key: boolean; key_last4: string; // NEVER return the full key
  }[];
  sandbox_token: { configured: boolean; last4: string; updated_at: string | null };
  directory: { last_synced_at: string | null; members_synced: number; sources: string[] };
  roles: { member: string; name: string; role: "admin"|"operator"|"viewer"; source: "directory"|"manual" }[];
}
```

`roles` is effectively `Store.ListDirectory("")` (or per-kind, de-duplicated
by `member`) joined with wherever console role assignments live — a new,
small mapping if one doesn't exist yet (`member -> role`), defaulting
unassigned directory members to `"viewer"`.

### `PUT /api/console/settings/model/{id}`

Request: `{ "base_url": "https://api.anthropic.com", "api_key": "sk-..." }`
— `api_key` optional (omit/blank to keep the existing key, matching the
console's "leave blank to keep current key" UI). Response: 200, no body
required (the console reloads `GET /settings`).

### `PUT /api/console/settings/sandbox-token`

Request: `{ "token": "..." }`. Stores it wherever `sandbox.Spec.Env`
credentials for the coding-agent token are sourced from. Response: 200.

### `POST /api/console/settings/directory/sync`

No body. Triggers a directory sync across enabled connectors (writes via
`Store.MapMember` for each). Response: the updated
`DirectorySyncStatus` (same shape as `settings.directory` above). This can
be slow (network calls to Slack/GitHub/Jira) — the console shows a spinner
and awaits it synchronously; if a real deployment needs it async, respond
202 with the same shape and add polling later — not needed for v1.

### `PUT /api/console/settings/roles/{member}`

Request: `{ "role": "operator" }`. Response: the updated role row.

---

## Go-side wiring checklist (not done by this worker — `app/console/*` is
owned by console, everything below is for whoever wires the server)

1. `app/console/dist/` is the build output (`bun run build` / `npm run
   build` from `app/console/`) — static `index.html` + hashed
   `assets/*.js|css|woff2`. It is a client-side-routed SPA (React Router,
   `BrowserRouter`), so the server must serve `index.html` for ANY path that
   isn't a real asset file or an `/api/*` route (a catch-all/history-fallback
   handler), or deep links like `/cases/case_c1` 404 on refresh.
2. `//go:embed` the `app/console/dist` directory (e.g. from a new
   `internal/api/console.go` or wherever static assets are wired), and mount
   it on the existing `http.ServeMux` in `internal/api/server.go` — e.g.
   `mux.Handle("/", http.FileServer(http.FS(consoleDist)))` plus the SPA
   fallback described above. `vite.config.ts` already sets `base: "./"` so
   the built asset paths are relative and work whether the console is
   mounted at `/` or a subpath.
3. Register every `/api/console/*` route from this document on the same mux,
   guarded by the console's own session auth (NOT the existing `srv.auth`
   bearer-token wrapper, which is for `KARMAX_API_TOKEN` / the phone app —
   the console needs its own middleware checking the session token from
   Auth above).
4. `go build` needs `app/console/dist` to exist before it can embed it — add
   a `Makefile`/CI step that runs the frontend build first (mirrors how
   `karmax-web`'s `dist/` is built before its own deploy), and `.gitignore`
   the `dist/` output like `app/console/.gitignore` already does, so it's a
   build artifact, not a committed one.
