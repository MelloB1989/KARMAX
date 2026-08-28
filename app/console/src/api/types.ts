// Wire types for the console API. Field names match the JSON the Go server
// sends (snake_case) — see ../../API.md for the endpoint each type belongs to.

export type CaseState =
  | "open" | "grooming" | "ready" | "building" | "review" | "done" | "dropped";

export interface Case {
  id: string;
  org: string;
  agent: string;
  key: string;
  title: string;
  state: CaseState;
  namespace: string;
  thread_channel: string;
  thread_ts: string;
  created_at: string;
  updated_at: string;
}

export interface CaseEvent {
  id: string;
  case_id: string;
  kind: string;
  payload: string; // raw JSON, console pretty-prints it
  actor: string;
  created_at: string;
}

export type SandboxRunStatus = "starting" | "running" | "exited" | "failed" | "gone";

export interface SandboxRun {
  id: string;
  case_id: string;
  driver: string;
  container_id: string;
  image: string;
  status: SandboxRunStatus;
  repo: string;
  branch: string;
  task: string;
  started_at: string;
  finished_at: string | null;
  exit_code: number;
  error: string;
  log_tail: string;
}

export interface CaseDetail {
  case: Case;
  events: CaseEvent[];
  sandbox_runs: SandboxRun[];
}

export type AgentStatus =
  | "idle" | "running" | "paused" | "stopping" | "stopped" | "failed" | "crashed";

export interface AgentSummary {
  id: string;
  name: string;
  description: string;
  tags: string[];
  model: string;
  provider: string;
  status: AgentStatus;
  open_cases: number;
  grants: string[]; // plain-English, from broker.Describe(subject)
}

export interface AgentDetail extends AgentSummary {
  persona: string; // system_prompt, in full
  tools: string[];
  mcps: string[];
  restart_policy: string;
  triggers: {
    webhooks: string[];
    schedules: { cron: string }[];
    events: string[];
    run_on_start: boolean;
  };
}

export interface RecipeTrigger {
  event: string;
  schedule: string;
  webhook: string;
  manual: boolean;
}

export interface RecipeSummary {
  name: string;
  source: "builtin" | "generated";
  enabled: boolean;
  trigger: RecipeTrigger;
  trigger_label: string; // e.g. "daily at 08:30", "on jira.issue.created", "manual"
  steps: number;
  updated_at: string;
}

export interface RecipeDetail extends RecipeSummary {
  yaml: string;
  permissions: string[]; // recipes.Describe() output
}

export interface RecipeDraft {
  draft_id: string;
  name: string;
  yaml: string;
  trigger_label: string;
  dry_run: string[]; // DryRun.Report() lines, one action per line
  permissions: string[]; // recipes.Describe() output
  warnings: string[];
}

export type ApprovalStatus = "pending" | "approved" | "rejected" | "executed" | "failed";

export interface Approval {
  id: string;
  kind: string;
  title: string;
  summary: string;
  context: string;
  action: string;
  case_id: string | null;
  case_key: string | null;
  agent: string;
  role: string;
  channel: string;
  status: ApprovalStatus;
  note: string;
  result: string;
  created_at: string;
  decided_at: string | null;
  decided_by: string | null;
}

export interface AuditEvent {
  id: string;
  actor_kind: "human" | "agent" | "recipe" | "system";
  actor_id: string;
  agent: string;
  case_id: string;
  recipe: string;
  step: string;
  verb: string;
  target: string;
  decision: string;
  detail: string;
  created_at: string;
}

export type ConnectorStatus = "healthy" | "degraded" | "failed" | "not_configured";

export interface ConnectorSummary {
  id: string;
  name: string;
  /**
   * The connector's manifest id. Deliberately not a closed union: the server
   * derives this from whatever connectors are registered, so narrowing it here
   * would only tell TypeScript a lookup is total when it is not — which is
   * precisely how ICON[c.kind] came to return undefined and blank the page.
   */
  kind: string;
  status: ConnectorStatus;
  detail: string;
  last_checked_at: string | null;
}

export interface ConnectorSetupStep {
  title: string;
  body: string;
  /** Something to copy — a callback URL, a webhook secret. */
  value?: string;
  /**
   * A link to open. Some steps cannot be static text because the address
   * depends on what was just created: a GitHub App's install page is
   * /apps/<slug>/installations/new, and the slug does not exist until the app
   * does.
   */
  url?: string;
  /** Whether this step already looks complete. Absent means "cannot tell". */
  done?: boolean;
}

export interface ConnectorAuthMethod {
  id: string;
  name: string;
  summary: string;
  recommended: boolean;
  steps: ConnectorSetupStep[];
}

export interface ConnectorField {
  key: string;
  label: string;
  type: "text" | "secret" | "url";
  placeholder?: string;
  required: boolean;
  /** Scopes the field to one auth method. Empty applies to all of them. */
  method?: string;
  description?: string;
  /** Where to find this value, in the provider's own words. */
  help?: string;
  /** Whether a value is already stored. The value itself is never sent. */
  set: boolean;
}

export interface ConnectorSetup {
  id: string;
  steps: ConnectorSetupStep[];
  fields: ConnectorField[];
  callback_url: string;
  /** The ways in. Empty for a connector with only one. */
  methods?: ConnectorAuthMethod[];
  active_method?: string;
}

/** One employee's authorisation for a per-user connector. Never carries tokens. */
export interface ConnectorConnection {
  member: string;
  account: string;
  connected_at: string;
  expires_at: string | null;
}

export interface ConnectorConnections {
  connections: ConnectorConnection[];
  /** Whether the signed-in operator has connected their own account. */
  self_connected: boolean;
}

export interface ConnectorHealthCheck {
  status: ConnectorStatus;
  detail: string;
  checked_at: string;
}

export interface ModelProvider {
  id: string;
  name: string;
  base_url: string;
  has_key: boolean;
  key_last4: string;
}

export interface SandboxTokenInfo {
  configured: boolean;
  last4: string;
  updated_at: string | null;
}

export type OrgRoleName = "admin" | "operator" | "viewer";

export interface OrgRole {
  member: string;
  name: string;
  role: OrgRoleName;
  source: "directory" | "manual";
}

export interface DirectorySyncStatus {
  last_synced_at: string | null;
  members_synced: number;
  sources: string[];
}

export interface Settings {
  model_providers: ModelProvider[];
  sandbox_token: SandboxTokenInfo;
  directory: DirectorySyncStatus;
  roles: OrgRole[];
}

export interface Session {
  token: string;
  member: string;
  name: string;
  role: OrgRoleName;
}

export interface BootstrapStatus {
  needs_bootstrap: boolean;
}

export interface ConsoleUser {
  member: string;
  name: string;
  role: "admin" | "operator" | "viewer";
  /** The address a Google sign-in matches. Empty means password-only. */
  email: string;
  self?: boolean;
}

export interface ConsoleUsers {
  users: ConsoleUser[];
  roles: string[];
}

export interface OrgProfile {
  name: string;
  domain: string;
  description: string;
  timezone: string;
  context: string;
  updated_at: string;
  updated_by: string;
  /** Exactly what the agents are told, rendered server-side. */
  briefing: string;
}

export interface GoogleSignIn {
  enabled: boolean;
  /** Restricts sign-in to one Workspace domain. Empty means no restriction. */
  domain: string;
}

export interface WebhookRow {
  id: string;
  /** platform = KARMAX knows the payload's shape; custom = it does not. */
  kind: "platform" | "custom";
  name: string;
  connector?: string;
  slug?: string;
  url: string;
  /** What a recipe triggers on. */
  event_kind: string;
  description?: string;
  enabled: boolean;
  /** Whether the route is actually mounted and answering. */
  live: boolean;
  /** Whether a delivery has to prove itself. */
  secured: boolean;
  signature_header?: string;
  agent_id?: string;
  created_by?: string;
  updated_at?: string;
}

export interface WebhookDelivery {
  id: string;
  endpoint: string;
  source: string;
  status: "accepted" | "rejected" | "disabled" | "error";
  detail: string;
  body_sample: string;
  received_at: string;
}

export interface WebhookPlatform {
  /** Empty is the Custom option. */
  id: string;
  name: string;
  event_kind: string;
  /** hmac signs the body, token sends the secret as-is, any = your choice. */
  secret_kind: "hmac" | "token" | "none" | "any";
  signature_header?: string;
  /** What the normalised event carries, so a recipe author knows what to read. */
  fields?: string[];
  setup_hint: string;
}

export interface WebhookCatalogue {
  platforms: WebhookPlatform[];
  signature_headers: string[];
  agents: string[];
  base_url: string;
}
