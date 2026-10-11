// The console's contract, mirroring fleet/internal/console/model.go field for
// field. Times are ISO 8601 strings; a zero Go time is omitted.

export const SOURCES = ["terminals", "events", "statusline", "docker", "host", "git", "fleetd"] as const;
export type SourceKey = (typeof SOURCES)[number];

export interface Source {
  ok: boolean;
  at?: string;
  error?: string;
}

export interface Snapshot {
  at: string;
  sources: Partial<Record<SourceKey, Source>>;
  host: HostInfo;
  orchestrator: Orchestrator | null;
  tasks: Task[] | null;
  agents: Agent[] | null;
  events: FleetEvent[] | null;
  today: Today;
  git: Git;
  activity: Record<string, Bucket[]> | null;
}

export interface HostInfo {
  name: string;
  uptime_s: number;
  load: [number, number, number];
  mem_used: number;
  mem_total: number;
  cpus: number;
  at: string;
}

export interface Orchestrator {
  name: string;
  five_hour_pct: number;
  seven_day_pct: number;
  five_hour_resets?: string;
  seven_day_resets?: string;
  at: string;
}

export interface Task {
  id: string;
  agent: string;
  assigner: string;
  since: string;
}

export const PHASES = ["standby", "working", "waiting", "cooling", "disabled", "down"] as const;
export type Phase = (typeof PHASES)[number];

export interface Agent {
  id: string;
  host: string;
  live: boolean;

  phase: Phase | string;
  alerts: string[] | null;
  task?: string;
  idle_s: number;
  models: string[] | null;
  session?: string;
  recorded_at: string;

  model?: string;
  context_pct: number;
  five_hour_pct: number;
  seven_day_pct: number;
  five_hour_resets?: string;
  statusline_at?: string;

  current_tool: Tool | null;
  last_event_at?: string;

  container: Container | null;
  last_task: LastTask | null;
  term: TermSize;
}

export interface Tool {
  name: string;
  since: string;
}

export interface Container {
  running: boolean;
  cpu_pct: number;
  mem: number;
  mem_limit: number;
  net_rx: number;
  net_tx: number;
  at: string;
}

export interface LastTask {
  id: string;
  outcome: string;
  ended: string;
  cost_usd: number;
}

export interface TermSize {
  cols: number;
  rows: number;
}

export interface FleetEvent {
  id: number;
  at: string;
  agent: string;
  kind: string;
  session?: string;
  detail?: unknown;
}

export interface Today {
  since: string;
  tasks_closed: number;
  spend_usd: number;
  output_tokens: number;
}

export interface Git {
  repos: Repo[] | null;
}

export interface Repo {
  name: string;
  main: Commit;
  branches: Branch[] | null;
  commits_7d: number;
  at: string;
}

export interface Commit {
  sha: string;
  subject: string;
  at: string;
}

export interface Branch {
  name: string;
  agent: string;
  sha: string;
  subject: string;
  at: string;
}

export interface Bucket {
  t: string;
  n: number;
}

export interface PatchOp {
  op: "add" | "remove" | "replace";
  path: string;
  value?: unknown;
}

export type Message =
  | { type: "snapshot"; seq: number; snapshot: Snapshot }
  | { type: "patch"; seq: number; ops: PatchOp[] };

export interface TermFrame {
  type: "hello" | "error" | "reconnecting" | "released";
  agent?: string;
  mode?: "watch" | "control";
  cols?: number;
  rows?: number;
  holder?: string;
  error?: string;
}
