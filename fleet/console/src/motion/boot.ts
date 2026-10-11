// The boot log is the real connection log: one line per source, lit when the
// snapshot says it answered ok.
import { SOURCES, type Snapshot, type SourceKey } from "../model";

export const BOOT_TIMEOUT_MS = 6000;

export interface BootLine {
  key: SourceKey;
  state: "ok" | "failed" | "pending";
  at?: string;
  error?: string;
}

export function bootLines(s: Snapshot | null, elapsedMs: number): BootLine[] {
  return SOURCES.map((key) => {
    const src = s?.sources[key];
    if (src?.ok) return { key, state: "ok", at: src.at };
    if (src && !src.ok) return { key, state: elapsedMs >= BOOT_TIMEOUT_MS ? "failed" : "pending", at: src.at, error: src.error };
    return { key, state: elapsedMs >= BOOT_TIMEOUT_MS ? "failed" : "pending" };
  });
}

/** Done when every source has answered ok, or when the 6 s are up. */
export function bootDone(lines: BootLine[], elapsedMs: number): boolean {
  return lines.every((l) => l.state === "ok") || elapsedMs >= BOOT_TIMEOUT_MS;
}
