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
