// Pure helpers that turn snapshot fields into what the panel shows.
import type { Source } from "../model";

export const palette = {
  ground: "#05080C",
  panel: "#0A1018",
  line: "#1B2A38",
  amber: "#FFB547",
  cyan: "#5CE1E6",
  text: "#D6E2EC",
  dim: "#7F93A6",
  alert: "#FF5D5D",
  frost: "#8EC9FF",
  grey: "#5B6773",
} as const;

/** Seconds between an observation time (ISO 8601) and now (ms epoch); Infinity when unknown. */
export function ageOf(at: string, now: number): number {
  const t = Date.parse(at);
  if (Number.isNaN(t)) return Infinity;
  return Math.max(0, (now - t) / 1000);
}

/** A source is stale when it failed, never reported, or is at least twice its cadence old. */
export function isStale(src: Source, cadenceS: number, now: number): boolean {
  if (!src.ok || !src.at) return true;
  return ageOf(src.at, now) >= 2 * cadenceS;
}

export interface Look {
  color: string;
  label: string;
  pulse: boolean;
}

/** The phase table from the spec; an alert of "stuck" wins over the phase. */
export function phaseLook(phase: string, alerts: string[] | null = []): Look {
  if ((alerts ?? []).some((a) => a === "stuck" || a.startsWith("stuck"))) {
    return { color: palette.alert, label: "stuck", pulse: true };
  }
  switch (phase) {
    case "working":
      return { color: palette.amber, label: "working", pulse: false };
    case "waiting":
      return { color: palette.cyan, label: "task open, idle", pulse: true };
    case "standby":
      return { color: palette.dim, label: "standby", pulse: false };
    case "cooling":
      return { color: palette.frost, label: "cooling", pulse: false };
    case "disabled":
      return { color: palette.grey, label: "disabled", pulse: false };
    case "down":
      return { color: palette.alert, label: "down", pulse: false };
    default:
      return { color: palette.dim, label: phase || "unknown", pulse: false };
  }
}

/** "40 s", "6 m 5 s"→"6 m", "6 h 11 m", "2 d 3 h". */
export function fmtAge(seconds: number): string {
  if (!Number.isFinite(seconds)) return "—";
  const s = Math.max(0, Math.floor(seconds));
  if (s < 60) return `${s} s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m} m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} h ${m % 60} m`;
  return `${Math.floor(h / 24)} d ${h % 24} h`;
}
