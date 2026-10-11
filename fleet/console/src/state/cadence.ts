// Refresh cadences from the spec's source table, in seconds. Streamed sources
// have no poll interval; they are stale when the source reports not ok.
import type { Snapshot, SourceKey } from "../model";
import { agoText, isKnown, isStale } from "./derive";

export const CADENCE_S: Record<SourceKey, number> = {
  terminals: 1,
  events: 1,
  statusline: 5,
  docker: 1,
  host: 2,
  git: 10,
  fleetd: 5,
};

export function sourceStale(s: Snapshot, key: SourceKey, now: number): boolean {
  const src = s.sources[key];
  return !src || isStale(src, CADENCE_S[key], now);
}

/** What to say of a source we cannot show values from: unreachable if it never answered, else its age. */
export function sourceNote(s: Snapshot, key: SourceKey, now: number): { text: string; title?: string } {
  const src = s.sources[key];
  if (!src || !isKnown(src.at)) return { text: `${key} unreachable`, title: src?.error };
  return { text: `${key} ${agoText(src.at, now)}`, title: src.error };
}
