import type { Agent, Bucket, Snapshot } from "../model";

/** Agent ids sort naturally: agent-2 before agent-10. */
export function orderAgents(agents: Agent[] | null): Agent[] {
  return [...(agents ?? [])].sort((a, b) => a.id.localeCompare(b.id, "en", { numeric: true }));
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

/** How tightly the roster and activity strip must pack for `n` agents: 4 or fewer keep the roomy layout. */
export type Density = "roomy" | "compact" | "dense";
export function densityFor(n: number): Density {
  return n <= 4 ? "roomy" : n <= 8 ? "compact" : "dense";
}

/** Activity strip geometry (px) per agent count; 4 agents give the original 132 px strip. */
export function trackMetrics(n: number): { rowH: number; height: number } {
  const rowH = n <= 4 ? 18 : n <= 8 ? 12 : 9;
  return { rowH, height: n <= 4 ? 132 : 50 + n * (rowH + 2) };
}
