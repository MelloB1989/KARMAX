import type { Agent, Bucket, Snapshot } from "../model";

/** The agent a worker is a slot of, and its slot. */
export const agentOf = (a: Agent): string => a.slot_of || a.id;
export const slotOf = (a: Agent): number => a.slot || 1;

/** Workers sort by agent, naturally (agent-2 before agent-10), then by slot. */
export function orderAgents(agents: Agent[] | null): Agent[] {
  return [...(agents ?? [])].sort((a, b) => agentOf(a).localeCompare(agentOf(b), "en", { numeric: true }) || slotOf(a) - slotOf(b));
}

/** One agent and its slots' workers, in slot order: a card on the roster, a row on the activity strip. */
export interface AgentGroup {
  id: string;
  slots: Agent[];
}

export function groupAgents(agents: Agent[] | null): AgentGroup[] {
  const out: AgentGroup[] = [];
  for (const a of orderAgents(agents)) {
    const last = out[out.length - 1];
    if (last && last.id === agentOf(a)) last.slots.push(a);
    else out.push({ id: agentOf(a), slots: [a] });
  }
  return out;
}

/** An agent's activity: its slots' buckets summed per bucket. */
export function groupActivity(g: AgentGroup, activity: Record<string, Bucket[]> | null): Bucket[] {
  const by = new Map<string, number>();
  for (const w of g.slots) for (const b of activity?.[w.id] ?? []) by.set(b.t, (by.get(b.t) ?? 0) + b.n);
  return [...by].map(([t, n]) => ({ t, n })).sort((a, b) => Date.parse(a.t) - Date.parse(b.t));
}

/** Phases whose session runs: the agent takes work, or is doing it, on whichever host. */
const UP = new Set(["standby", "working", "waiting", "cooling"]);
export function isUp(a: Agent): boolean {
  return UP.has(a.phase);
}

/** An agent on another host (or without a live view) has recorded data only. */
export function isRemote(a: Agent, s: Snapshot): boolean {
  return !a.live || a.host !== s.host.name;
}

/** Where a bucket sits on a track covering the last `windowS` seconds: 0 = oldest, 1 = now. */
export function bucketFraction(b: Bucket, now: number, windowS = 24 * 3600): number {
  const f = 1 - (now - Date.parse(b.t)) / 1000 / windowS;
  return Math.min(1, Math.max(0, f));
}

/** How tightly the roster and activity strip must pack for `n` agents (cards, not slots): 4 or fewer keep the roomy layout. */
export type Density = "roomy" | "compact" | "dense";
export function densityFor(n: number): Density {
  return n <= 4 ? "roomy" : n <= 8 ? "compact" : "dense";
}

/** Activity strip geometry (px) per agent count; 4 agents give the original 132 px strip. */
export function trackMetrics(n: number): { rowH: number; height: number } {
  const rowH = n <= 4 ? 18 : n <= 8 ? 12 : 9;
  return { rowH, height: n <= 4 ? 132 : 50 + n * (rowH + 2) };
}

/** Height of the bottom grid row: the strip plus its 20 px top and bottom margins (172 px for 4 agents). */
export function activityRowHeight(n: number): number {
  return trackMetrics(n).height + 40;
}
