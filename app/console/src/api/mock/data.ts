// Plausible fixture data for the org "Acme" — real-shaped keys, repos and
// channels, never lorem ipsum. Kept internally consistent: every case's
// `agent` exists in AGENTS, every event/run's `case_id` exists in CASES, etc.,
// so the mock functions in ./api.ts can join across them like a real backend.
import type {
  AgentDetail, AgentSummary, Approval, AuditEvent, Case, CaseEvent, ConnectorSetup,
  ConnectorSummary, DirectorySyncStatus, ModelProvider, OrgRole, RecipeDetail,
  RecipeDraft, RecipeSummary, SandboxRun, SandboxTokenInfo,
} from "../types";

const NOW = Date.now();
const minutesAgo = (n: number) => new Date(NOW - n * 60_000).toISOString();
const hoursAgo = (n: number) => minutesAgo(n * 60);
const daysAgo = (n: number) => hoursAgo(n * 24);

// ---- Agents ---------------------------------------------------------------

export const AGENTS: AgentDetail[] = [
  {
    id: "pr-shepherd",
    name: "PR Shepherd",
    description: "Turns groomed tickets into pull requests and shepherds them to merge.",
    tags: ["engineering", "github"],
    model: "claude-sonnet-4-6",
    provider: "anthropic",
    status: "running",
    open_cases: 4,
    grants: [
      "may open PRs in acme/api",
      "may open PRs in acme/web",
      "may push branches in acme/api",
      "may post in #eng",
      "may run sandbox builds on the docker driver",
      "may read Jira issues in project ACME",
    ],
    persona:
      "You are PR Shepherd. You take a ticket that has been groomed and marked Ready, " +
      "implement it in a sandbox, open a pull request, and post the link in #eng. You never " +
      "merge — that needs a human's approval. If a build fails twice, stop and ask for help " +
      "instead of retrying a third time.",
    tools: ["sandbox.run", "github.open_pr", "github.comment", "case.log", "slack.send"],
    mcps: [],
    restart_policy: "on-failure",
    triggers: { webhooks: [], schedules: [], events: ["jira.issue.updated"], run_on_start: false },
  },
  {
    id: "jira-triage",
    name: "Jira Triage",
    description: "Grooms new tickets: labels, estimates, and asks clarifying questions before work starts.",
    tags: ["engineering", "jira"],
    model: "claude-sonnet-4-6",
    provider: "anthropic",
    status: "idle",
    open_cases: 2,
    grants: [
      "may comment on Jira issues in project ACME",
      "may transition Jira issues in project ACME",
      "may read memory in acme/triage",
    ],
    persona:
      "You are Jira Triage. New issues land with you unlabelled. Read the description, decide " +
      "if it is a bug, a chore or a feature, add the right labels, and either move it to Ready " +
      "or comment asking the reporter for what's missing. Be terse — one comment, not a thread.",
    tools: ["jira.comment", "jira.transition", "jira.label", "case.log"],
    mcps: [],
    restart_policy: "on-failure",
    triggers: { webhooks: [], schedules: [], events: ["jira.issue.created"], run_on_start: false },
  },
  {
    id: "release-captain",
    name: "Release Captain",
    description: "Runs the Friday release train: freeze, changelog, and the go/no-go call.",
    tags: ["engineering", "releases"],
    model: "claude-sonnet-4-6",
    provider: "anthropic",
    status: "running",
    open_cases: 1,
    grants: [
      "may post in #releases",
      "may tag releases in acme/api",
      "may tag releases in acme/web",
      "hold the grant github:acme/api",
    ],
    persona:
      "You are Release Captain. Every Friday at 16:00 you open the release case, compile the " +
      "changelog from merged PRs, post it to #releases, and ask a senior dev for the go/no-go " +
      "before tagging. Never tag without an explicit approval on that case.",
    tools: ["github.tag", "github.changelog", "case.log", "slack.send"],
    mcps: [],
    restart_policy: "always",
    triggers: { webhooks: [], schedules: [{ cron: "0 0 16 * * FRI" }], events: [], run_on_start: false },
  },
  {
    id: "oncall-buddy",
    name: "Oncall Buddy",
    description: "Shadows PagerDuty incidents, keeps the case log, and drafts the postmortem.",
    tags: ["reliability", "incidents"],
    model: "claude-sonnet-4-6",
    provider: "anthropic",
    status: "paused",
    open_cases: 0,
    grants: [
      "may post in #incidents",
      "may read PagerDuty incidents",
      "may WRITE memory in acme/postmortems",
    ],
    persona:
      "You are Oncall Buddy. When PagerDuty fires, open a case, keep a timeline in the case log " +
      "as updates arrive, and once it's resolved draft a postmortem outline — do not publish it, " +
      "just propose it.",
    tools: ["case.log", "slack.send", "memory.remember"],
    mcps: [],
    restart_policy: "always",
    triggers: { webhooks: ["pagerduty"], schedules: [], events: [], run_on_start: false },
  },
];

export const AGENT_SUMMARIES: AgentSummary[] = AGENTS.map(
  ({ persona: _persona, tools: _tools, mcps: _mcps, restart_policy: _rp, triggers: _t, ...rest }) => rest,
);

// ---- Cases ------------------------------------------------------------------

export const CASES: Case[] = [
  {
    id: "case_c1", org: "acme", agent: "pr-shepherd", key: "jira:ACME-482",
    title: "Rate limiter drops burst traffic on /v2/orders", state: "building",
    namespace: "acme/api", thread_channel: "#eng", thread_ts: "1755999120.001200",
    created_at: daysAgo(2), updated_at: minutesAgo(6),
  },
  {
    id: "case_c2", org: "acme", agent: "jira-triage", key: "jira:ACME-489",
    title: "Add SSO metadata endpoint for Okta", state: "grooming",
    namespace: "acme/api", thread_channel: "", thread_ts: "",
    created_at: hoursAgo(9), updated_at: hoursAgo(1),
  },
  {
    id: "case_c3", org: "acme", agent: "pr-shepherd", key: "gh:acme/api#1042",
    title: "Nightly cron duplicates webhook deliveries", state: "review",
    namespace: "acme/api", thread_channel: "#eng", thread_ts: "1755912000.000400",
    created_at: daysAgo(3), updated_at: hoursAgo(4),
  },
  {
    id: "case_c4", org: "acme", agent: "pr-shepherd", key: "jira:ACME-501",
    title: "Search autocomplete times out over 200ms p95", state: "ready",
    namespace: "acme/api", thread_channel: "", thread_ts: "",
    created_at: hoursAgo(20), updated_at: hoursAgo(20),
  },
  {
    id: "case_c5", org: "acme", agent: "release-captain", key: "release:2026.08.3",
    title: "Cut release 2026.08.3", state: "open",
    namespace: "acme/releases", thread_channel: "#releases", thread_ts: "1756033200.000900",
    created_at: hoursAgo(2), updated_at: hoursAgo(2),
  },
  {
    id: "case_c6", org: "acme", agent: "oncall-buddy", key: "incident:INC-77",
    title: "Elevated 5xx on checkout-service", state: "done",
    namespace: "acme/incidents", thread_channel: "#incidents", thread_ts: "1755820000.000100",
    created_at: daysAgo(5), updated_at: daysAgo(4),
  },
  {
    id: "case_c7", org: "acme", agent: "jira-triage", key: "jira:ACME-410",
    title: "Deprecate legacy /v1/auth endpoints", state: "dropped",
    namespace: "acme/api", thread_channel: "", thread_ts: "",
    created_at: daysAgo(9), updated_at: daysAgo(7),
  },
  {
    id: "case_c8", org: "acme", agent: "pr-shepherd", key: "gh:acme/web#560",
    title: "Flaky Cypress run blocks main", state: "open",
    namespace: "acme/web", thread_channel: "#eng", thread_ts: "",
    created_at: hoursAgo(1), updated_at: hoursAgo(1),
  },
];

export const CASE_EVENTS: Record<string, CaseEvent[]> = {
  case_c1: [
    { id: "ev1", case_id: "case_c1", kind: "case.opened", payload: '{"source":"jira"}', actor: "jira", created_at: daysAgo(2) },
    { id: "ev2", case_id: "case_c1", kind: "case.state", payload: '{"from":"open","to":"grooming"}', actor: "agent:jira-triage", created_at: daysAgo(2) },
    { id: "ev3", case_id: "case_c1", kind: "case.state", payload: '{"from":"grooming","to":"ready"}', actor: "agent:jira-triage", created_at: daysAgo(1) },
    { id: "ev4", case_id: "case_c1", kind: "note", payload: '{"text":"repro: burst of 40 req/s from a single tenant trips the limiter early, drops the tail 12%"}', actor: "human:nikhil", created_at: hoursAgo(20) },
    { id: "ev5", case_id: "case_c1", kind: "case.state", payload: '{"from":"ready","to":"building"}', actor: "agent:pr-shepherd", created_at: hoursAgo(18) },
    { id: "ev6", case_id: "case_c1", kind: "sandbox.started", payload: '{"run_id":"run_a1","repo":"acme/api","branch":"fix/rate-limiter-burst"}', actor: "agent:pr-shepherd", created_at: hoursAgo(18) },
    { id: "ev7", case_id: "case_c1", kind: "slack.message", payload: '{"channel":"#eng","text":"Starting on ACME-482, will post the PR here"}', actor: "agent:pr-shepherd", created_at: hoursAgo(18) },
    { id: "ev8", case_id: "case_c1", kind: "sandbox.progress", payload: '{"run_id":"run_a1","note":"token-bucket refill now accounts for burst window"}', actor: "agent:pr-shepherd", created_at: minutesAgo(6) },
  ],
  case_c2: [
    { id: "ev9", case_id: "case_c2", kind: "case.opened", payload: '{"source":"jira"}', actor: "jira", created_at: hoursAgo(9) },
    { id: "ev10", case_id: "case_c2", kind: "note", payload: '{"text":"needs a decision on SAML vs OIDC metadata format before scoping"}', actor: "agent:jira-triage", created_at: hoursAgo(1) },
  ],
  case_c3: [
    { id: "ev11", case_id: "case_c3", kind: "case.opened", payload: '{"source":"github"}', actor: "github", created_at: daysAgo(3) },
    { id: "ev12", case_id: "case_c3", kind: "case.state", payload: '{"from":"open","to":"building"}', actor: "agent:pr-shepherd", created_at: daysAgo(3) },
    { id: "ev13", case_id: "case_c3", kind: "sandbox.finished", payload: '{"run_id":"run_a0","status":"exited","exit_code":0}', actor: "agent:pr-shepherd", created_at: daysAgo(2) },
    { id: "ev14", case_id: "case_c3", kind: "pr.opened", payload: '{"repo":"acme/api","number":1044,"url":"https://github.com/acme/api/pull/1044"}', actor: "agent:pr-shepherd", created_at: daysAgo(2) },
    { id: "ev15", case_id: "case_c3", kind: "case.state", payload: '{"from":"building","to":"review"}', actor: "agent:pr-shepherd", created_at: daysAgo(2) },
    { id: "ev16", case_id: "case_c3", kind: "note", payload: '{"text":"left a comment about the idempotency key TTL, otherwise looks right"}', actor: "human:asha", created_at: hoursAgo(4) },
  ],
  case_c4: [
    { id: "ev17", case_id: "case_c4", kind: "case.opened", payload: '{"source":"jira"}', actor: "jira", created_at: hoursAgo(20) },
    { id: "ev18", case_id: "case_c4", kind: "case.state", payload: '{"from":"open","to":"ready"}', actor: "agent:jira-triage", created_at: hoursAgo(20) },
  ],
  case_c5: [
    { id: "ev19", case_id: "case_c5", kind: "case.opened", payload: '{"source":"schedule"}', actor: "agent:release-captain", created_at: hoursAgo(2) },
    { id: "ev20", case_id: "case_c5", kind: "slack.message", payload: '{"channel":"#releases","text":"Compiling the changelog for 2026.08.3 now"}', actor: "agent:release-captain", created_at: hoursAgo(2) },
  ],
  case_c6: [
    { id: "ev21", case_id: "case_c6", kind: "case.opened", payload: '{"source":"pagerduty"}', actor: "pagerduty", created_at: daysAgo(5) },
    { id: "ev22", case_id: "case_c6", kind: "note", payload: '{"text":"root cause: connection pool exhaustion after a deploy dropped max idle conns"}', actor: "human:asha", created_at: daysAgo(5) },
    { id: "ev23", case_id: "case_c6", kind: "case.state", payload: '{"from":"review","to":"done"}', actor: "human:asha", created_at: daysAgo(4) },
  ],
  case_c7: [
    { id: "ev24", case_id: "case_c7", kind: "case.opened", payload: '{"source":"jira"}', actor: "jira", created_at: daysAgo(9) },
    { id: "ev25", case_id: "case_c7", kind: "case.state", payload: '{"from":"grooming","to":"dropped"}', actor: "human:nikhil", created_at: daysAgo(7) },
    { id: "ev26", case_id: "case_c7", kind: "note", payload: '{"text":"superseded by ACME-489, closing"}', actor: "human:nikhil", created_at: daysAgo(7) },
  ],
  case_c8: [
    { id: "ev27", case_id: "case_c8", kind: "case.opened", payload: '{"source":"github"}', actor: "github", created_at: hoursAgo(1) },
  ],
};

export const SANDBOX_RUNS: Record<string, SandboxRun[]> = {
  case_c1: [
    {
      id: "run_a1", case_id: "case_c1", driver: "docker", container_id: "8f2c1a9e4b71",
      image: "ocrew/sandbox:claude-code", status: "running",
      repo: "acme/api", branch: "fix/rate-limiter-burst",
      task: "Implement ACME-482: rate limiter drops burst traffic on /v2/orders",
      started_at: hoursAgo(18), finished_at: null, exit_code: 0, error: "",
      log_tail:
        "$ go test ./internal/ratelimit/...\nok  \tacme/internal/ratelimit\t0.412s\n" +
        "$ go vet ./...\n(clean)\n[claude] adjusting token-bucket refill to account for burst window\n" +
        "[claude] writing internal/ratelimit/bucket_test.go\n",
    },
  ],
  case_c3: [
    {
      id: "run_a0", case_id: "case_c3", driver: "docker", container_id: "3ad790c2115f",
      image: "ocrew/sandbox:claude-code", status: "exited",
      repo: "acme/api", branch: "fix/webhook-dedupe",
      task: "Fix ACME-1042: nightly cron duplicates webhook deliveries",
      started_at: daysAgo(3), finished_at: daysAgo(2), exit_code: 0, error: "",
      log_tail:
        "$ go test ./internal/webhooks/...\nok  \tacme/internal/webhooks\t1.190s\n" +
        "[claude] added an idempotency key keyed on (delivery_id, cron_run_id)\n" +
        "[claude] opened PR acme/api#1044\nexit 0\n",
    },
  ],
};

// ---- Recipes ----------------------------------------------------------------

const JIRA_FIX_AND_SHIP_YAML = `name: jira-fix-and-ship
on:
  event: jira.issue.updated

steps:
  - case.open:  { key: "jira:{{ .ticket }}", title: "{{ .summary }}" }
    as: c
  - case.state: { case: "{{ .c.id }}", state: prioritized }
  - case.log:   { case: "{{ .c.id }}", kind: note, payload: "asked for repro" }

  - await:
      event: jira.issue.updated
      match: { key: "{{ .ticket }}", status: Ready }
      timeout: 168h
    as: moved

  - sandbox:
      case: "{{ .c.id }}"
      repo: acme/api
      branch: main
      task: "Implement {{ .ticket }}: {{ .summary }}"
      timeout: 45m
    as: build

  - send: { to: "#eng", thread: "{{ .c.thread_ts }}", text: "PR is up" }
  - propose:
      to_role: senior-dev
      title: "Merge {{ .ticket }}?"
      summary: "Sandbox build finished for {{ .ticket }}"
      action: "Merge the PR opened by pr-shepherd for {{ .ticket }}"

grants:
  - github:acme/api
  - slack:#eng
`;

const RELEASE_TRAIN_YAML = `name: release-train
on:
  schedule: "0 0 16 * * FRI"

steps:
  - case.open: { key: "release:{{ .version }}", title: "Cut release {{ .version }}" }
    as: c
  - tool:
      name: github.changelog
      repo: acme/api
    as: changelog
  - send: { to: "#releases", text: "{{ .changelog }}" }
  - propose:
      to_role: senior-dev
      title: "Go/no-go for {{ .version }}"
      summary: "Changelog posted in #releases"
      action: "Tag and publish {{ .version }}"

grants:
  - github:acme/api
  - github:acme/web
  - slack:#releases
`;

const INCIDENT_NOTES_YAML = `name: incident-notes
on:
  webhook: pagerduty

steps:
  - case.open: { key: "incident:{{ .incident_id }}", title: "{{ .title }}" }
    as: c
  - case.state: { case: "{{ .c.id }}", state: open }
  - notify: { title: "Incident opened", body: "{{ .title }}" }
  - case.log: { case: "{{ .c.id }}", kind: note, payload: "oncall-buddy is tracking this" }

grants:
  - slack:#incidents
`;

const STALE_REVIEW_NUDGE_YAML = `name: stale-review-nudge
on:
  schedule: "0 0 9 * * *"

steps:
  - tool:
      name: cases.list
      state: review
    as: stale
  - when: "{{ .stale }}"
    send:
      to: "#eng"
      text: "{{ len .stale }} case(s) have been in review over 48h — a nudge, not a summons."

grants:
  - slack:#eng
`;

const WEEKLY_JIRA_CLEANUP_YAML = `name: weekly-jira-cleanup
enabled: false
on:
  schedule: "0 0 8 * * MON"

steps:
  - tool:
      name: jira.search
      jql: "project = ACME AND status = Backlog AND updated < -30d"
    as: stale
  - when: "{{ .stale }}"
    propose:
      to_role: senior-dev
      title: "Close {{ len .stale }} stale backlog tickets?"
      summary: "Untouched for 30+ days"
      action: "Close each ticket in the list with a comment explaining why"

grants:
  - jira:ACME
`;

export const RECIPES: RecipeDetail[] = [
  {
    name: "jira-fix-and-ship", source: "builtin", enabled: true,
    trigger: { event: "jira.issue.updated", schedule: "", webhook: "", manual: false },
    trigger_label: "on jira.issue.updated", steps: 7, updated_at: daysAgo(30),
    yaml: JIRA_FIX_AND_SHIP_YAML,
    permissions: [
      "ask your agent to do something, with all of its tools",
      "call the tool jira.comment",
      "hold the grant github:acme/api",
      "hold the grant slack:#eng",
      "put reminders on your list",
      "SEND MESSAGES AS YOU",
      "ask for your approval before acting",
      "write to oCrew's log",
    ],
  },
  {
    name: "release-train", source: "builtin", enabled: true,
    trigger: { event: "", schedule: "0 0 16 * * FRI", webhook: "", manual: false },
    trigger_label: "every Friday at 16:00", steps: 5, updated_at: daysAgo(30),
    yaml: RELEASE_TRAIN_YAML,
    permissions: [
      "call oCrew tools by name",
      "hold the grant github:acme/api",
      "hold the grant github:acme/web",
      "hold the grant slack:#releases",
      "ask for your approval before acting",
      "SEND MESSAGES AS YOU",
    ],
  },
  {
    name: "incident-notes", source: "builtin", enabled: true,
    trigger: { event: "", schedule: "", webhook: "pagerduty", manual: false },
    trigger_label: "on the pagerduty webhook", steps: 4, updated_at: daysAgo(30),
    yaml: INCIDENT_NOTES_YAML,
    permissions: [
      "hold the grant slack:#incidents",
      "send you notifications",
      "write to oCrew's log",
    ],
  },
  {
    name: "stale-review-nudge", source: "generated", enabled: true,
    trigger: { event: "", schedule: "0 0 9 * * *", webhook: "", manual: false },
    trigger_label: "daily at 09:00", steps: 2, updated_at: daysAgo(11),
    yaml: STALE_REVIEW_NUDGE_YAML,
    permissions: [
      "call the tool cases.list",
      "hold the grant slack:#eng",
      "SEND MESSAGES AS YOU",
    ],
  },
  {
    name: "weekly-jira-cleanup", source: "generated", enabled: false,
    trigger: { event: "", schedule: "0 0 8 * * MON", webhook: "", manual: false },
    trigger_label: "every Monday at 08:00", steps: 2, updated_at: hoursAgo(3),
    yaml: WEEKLY_JIRA_CLEANUP_YAML,
    permissions: [
      "call the tool jira.search",
      "hold the grant jira:ACME",
      "ask for your approval before acting",
    ],
  },
];

export const RECIPE_SUMMARIES: RecipeSummary[] = RECIPES.map(
  ({ yaml: _yaml, permissions: _permissions, ...rest }) => rest,
);

export function makeDraft(description: string): RecipeDraft {
  const name = "generated-" + Math.random().toString(36).slice(2, 8);
  const yaml = `name: ${name}
on:
  schedule: "0 0 9 * * MON"

# Generated from: "${description.replace(/"/g, "'")}"
steps:
  - tool:
      name: jira.search
      jql: "project = ACME AND status = 'In Review' AND updated < -2d"
    as: stuck
  - when: "{{ .stuck }}"
    send:
      to: "#eng"
      text: "{{ len .stuck }} review(s) have gone quiet for 2+ days."

grants:
  - jira:ACME
  - slack:#eng
`;
  return {
    draft_id: "draft_" + Math.random().toString(36).slice(2, 10),
    name,
    yaml,
    trigger_label: "every Monday at 09:00",
    dry_run: [
      "call the tool jira.search",
      "wait: the condition `.stuck` is truthy in this rehearsal",
      "SEND WhatsApp to #eng: [2] review(s) have gone quiet for 2+ days.",
    ],
    permissions: [
      "call the tool jira.search",
      "hold the grant jira:ACME",
      "hold the grant slack:#eng",
      "SEND MESSAGES AS YOU",
    ],
    warnings: [],
  };
}

// ---- Approvals ----------------------------------------------------------------

export const APPROVALS: Approval[] = [
  {
    id: "appr_1", kind: "pr_merge", title: "Merge ACME-1042 fix?",
    summary: "Sandbox build finished for ACME-1042 — idempotency key added for webhook dedupe.",
    context: "PR acme/api#1044, 3 files changed, tests passing.",
    action: "Merge acme/api#1044 into main",
    case_id: "case_c3", case_key: "gh:acme/api#1042", agent: "pr-shepherd", role: "senior-dev",
    channel: "#eng", status: "pending", note: "", result: "",
    created_at: hoursAgo(4), decided_at: null, decided_by: null,
  },
  {
    id: "appr_2", kind: "generic", title: "Go/no-go for 2026.08.3",
    summary: "Changelog posted in #releases, 14 PRs since last cut.",
    context: "No open P0/P1 issues against acme/api or acme/web.",
    action: "Tag and publish 2026.08.3",
    case_id: "case_c5", case_key: "release:2026.08.3", agent: "release-captain", role: "senior-dev",
    channel: "#releases", status: "pending", note: "", result: "",
    created_at: hoursAgo(1), decided_at: null, decided_by: null,
  },
  {
    id: "appr_3", kind: "generic", title: "Close 6 stale backlog tickets?",
    summary: "Untouched for 30+ days in project ACME.",
    context: "ACME-201, ACME-233, ACME-240, ACME-266, ACME-301, ACME-318",
    action: "Close each ticket with a comment explaining why",
    case_id: null, case_key: null, agent: "jira-triage", role: "senior-dev",
    channel: "", status: "approved", note: "go ahead, ping me if anyone objects",
    result: "Closed all 6 tickets with a comment linking this decision.",
    created_at: daysAgo(2), decided_at: daysAgo(2), decided_by: "nikhil",
  },
  {
    id: "appr_4", kind: "pr_merge", title: "Merge dependency bump: undici 6.21 -> 7.2?",
    summary: "Patch bump pulled in by Renovate, no changelog red flags.",
    context: "PR acme/web#558",
    action: "Merge acme/web#558 into main",
    case_id: null, case_key: null, agent: "pr-shepherd", role: "senior-dev",
    channel: "#eng", status: "rejected", note: "hold until we've triaged the flaky Cypress run first",
    result: "",
    created_at: daysAgo(1), decided_at: hoursAgo(20), decided_by: "asha",
  },
];

// ---- Audit --------------------------------------------------------------------

export const AUDIT_EVENTS: AuditEvent[] = [
  { id: "aud_1", actor_kind: "agent", actor_id: "pr-shepherd", agent: "pr-shepherd", case_id: "case_c1", recipe: "jira-fix-and-ship", step: "sandbox", verb: "sandbox.run", target: "acme/api@fix/rate-limiter-burst", decision: "allowed", detail: "", created_at: minutesAgo(6) },
  { id: "aud_2", actor_kind: "human", actor_id: "asha", agent: "", case_id: "case_c4", recipe: "", step: "", verb: "approval.decision", target: "appr_4", decision: "rejected", detail: "hold until we've triaged the flaky Cypress run first", created_at: hoursAgo(20) },
  { id: "aud_3", actor_kind: "recipe", actor_id: "release-train", agent: "release-captain", case_id: "case_c5", recipe: "release-train", step: "propose", verb: "propose", target: "senior-dev", decision: "allowed", detail: "", created_at: hoursAgo(1) },
  { id: "aud_4", actor_kind: "agent", actor_id: "pr-shepherd", agent: "pr-shepherd", case_id: "case_c3", recipe: "jira-fix-and-ship", step: "send", verb: "slack.send", target: "#eng", decision: "allowed", detail: "", created_at: daysAgo(2) },
  { id: "aud_5", actor_kind: "agent", actor_id: "jira-triage", agent: "jira-triage", case_id: "case_c2", recipe: "", step: "", verb: "jira.comment", target: "ACME-489", decision: "allowed", detail: "", created_at: hoursAgo(1) },
  { id: "aud_6", actor_kind: "human", actor_id: "nikhil", agent: "", case_id: "case_c7", recipe: "", step: "", verb: "case.state", target: "jira:ACME-410", decision: "allowed", detail: "moved to dropped", created_at: daysAgo(7) },
  { id: "aud_7", actor_kind: "agent", actor_id: "oncall-buddy", agent: "oncall-buddy", case_id: "case_c6", recipe: "incident-notes", step: "notify", verb: "notify", target: "operator", decision: "allowed", detail: "", created_at: daysAgo(5) },
  { id: "aud_8", actor_kind: "system", actor_id: "broker", agent: "", case_id: "", recipe: "weekly-jira-cleanup", step: "", verb: "jira.transition", target: "ACME-201", decision: "denied", detail: "recipe is disabled — draft only", created_at: hoursAgo(3) },
  { id: "aud_9", actor_kind: "human", actor_id: "nikhil", agent: "", case_id: "", recipe: "", step: "", verb: "connector.credentials", target: "jira", decision: "allowed", detail: "rotated API token", created_at: daysAgo(6) },
  { id: "aud_10", actor_kind: "agent", actor_id: "pr-shepherd", agent: "pr-shepherd", case_id: "case_c1", recipe: "jira-fix-and-ship", step: "sandbox", verb: "github.push", target: "acme/api@fix/rate-limiter-burst", decision: "allowed", detail: "", created_at: hoursAgo(17) },
];

// ---- Connectors -----------------------------------------------------------------

export const CONNECTORS: ConnectorSummary[] = [
  { id: "slack", name: "Slack", kind: "slack", status: "healthy", detail: "Connected as @ocrew to Acme Inc", last_checked_at: minutesAgo(4) },
  { id: "github", name: "GitHub", kind: "github", status: "healthy", detail: "App installed on acme/api, acme/web", last_checked_at: minutesAgo(4) },
  { id: "jira", name: "Jira", kind: "jira", status: "degraded", detail: "Token expires in 3 days", last_checked_at: hoursAgo(1) },
];

export const CONNECTOR_SETUPS: Record<string, ConnectorSetup> = {
  slack: {
    id: "slack",
    steps: [
      { title: "Create your Slack app", body: "Go to api.slack.com/apps -> Create New App -> From an app manifest, and pick your Acme Inc workspace." },
      { title: "Set the redirect URL", body: "Under OAuth & Permissions, add this exact Redirect URL.", value: "https://ocrew.acme.internal/api/console/connectors/slack/oauth/callback" },
      { title: "Grant scopes", body: "Add these Bot Token Scopes: chat:write, channels:read, channels:history, users:read." },
      { title: "Install to workspace", body: "Click Install to Workspace, approve, then copy the Bot User OAuth Token below." },
    ],
    fields: [
      { key: "bot_token", label: "Bot User OAuth Token", type: "secret", placeholder: "xoxb-...", required: true },
      { key: "signing_secret", label: "Signing Secret", type: "secret", placeholder: "", required: true },
    ],
    callback_url: "https://ocrew.acme.internal/api/console/connectors/slack/oauth/callback",
  },
  github: {
    id: "github",
    steps: [
      { title: "Create a GitHub App", body: "In your GitHub org settings -> Developer settings -> GitHub Apps -> New GitHub App." },
      { title: "Set the webhook URL", body: "Paste this as the Webhook URL.", value: "https://ocrew.acme.internal/api/webhooks/github" },
      { title: "Set the callback URL", body: "Paste this as the Setup URL / Callback URL.", value: "https://ocrew.acme.internal/api/console/connectors/github/oauth/callback" },
      { title: "Grant repository permissions", body: "Contents: Read & write, Pull requests: Read & write, Issues: Read & write. Subscribe to the pull_request and push events." },
      { title: "Install the app", body: "Install it on acme/api and acme/web, then paste the App ID, Installation ID and private key below." },
    ],
    fields: [
      { key: "app_id", label: "App ID", type: "text", required: true },
      { key: "installation_id", label: "Installation ID", type: "text", required: true },
      { key: "private_key", label: "Private key (PEM)", type: "secret", required: true },
    ],
    callback_url: "https://ocrew.acme.internal/api/console/connectors/github/oauth/callback",
  },
  jira: {
    id: "jira",
    steps: [
      { title: "Create an API token", body: "In your Atlassian account settings -> Security -> Create and manage API tokens -> Create API token." },
      { title: "Set the webhook URL", body: "In Jira -> Settings -> System -> WebHooks -> Create a WebHook pointing here.", value: "https://ocrew.acme.internal/api/webhooks/jira" },
      { title: "Scope it to project ACME", body: "Limit the webhook to issue created, updated and deleted events on project ACME, to keep noise down." },
      { title: "Enter your credentials", body: "Your Atlassian site URL, account email, and the API token from step 1." },
    ],
    fields: [
      { key: "site_url", label: "Site URL", type: "url", placeholder: "https://acme.atlassian.net", required: true },
      { key: "email", label: "Account email", type: "text", placeholder: "you@acme.com", required: true },
      { key: "api_token", label: "API token", type: "secret", required: true },
    ],
    callback_url: "https://ocrew.acme.internal/api/webhooks/jira",
  },
};

// ---- Settings -------------------------------------------------------------------

export const MODEL_PROVIDERS: ModelProvider[] = [
  { id: "anthropic", name: "Anthropic", base_url: "https://api.anthropic.com", has_key: true, key_last4: "7f3a" },
  { id: "openai", name: "OpenAI (fallback)", base_url: "https://api.openai.com/v1", has_key: false, key_last4: "" },
];

export const SANDBOX_TOKEN: SandboxTokenInfo = {
  configured: true, last4: "9c2e", updated_at: daysAgo(14),
};

export const DIRECTORY_SYNC: DirectorySyncStatus = {
  last_synced_at: hoursAgo(6), members_synced: 11, sources: ["slack", "github", "jira"],
};

export const ORG_ROLES: OrgRole[] = [
  { member: "nikhil", name: "Nikhil", role: "admin", source: "manual" },
  { member: "asha", name: "Asha Rao", role: "operator", source: "directory" },
  { member: "devraj", name: "Devraj Singh", role: "operator", source: "directory" },
  { member: "priya", name: "Priya Menon", role: "viewer", source: "directory" },
];
