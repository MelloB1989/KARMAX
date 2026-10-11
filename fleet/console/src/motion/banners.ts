// Banners are derived from the snapshot on every render, so they are
// dismissed only by the condition clearing; there is nothing to close.
import type { Snapshot } from "../model";
import { palette } from "../state/derive";
import { orderAgents } from "../overview/roster";

export interface Banner {
  key: string;
  kind: "stuck" | "down" | "cooling" | "disabled";
  text: string;
  color: string;
}

export function bannersFor(s: Snapshot | null): Banner[] {
  if (!s) return [];
  const out: Banner[] = [];
  for (const a of orderAgents(s.agents)) {
    for (const al of a.alerts ?? []) {
      if (al === "stuck" || al.startsWith("stuck")) {
        out.push({ key: `${a.id}:${al}`, kind: "stuck", text: `${a.id} ${al}`.toUpperCase(), color: palette.alert });
      }
    }
    if (a.phase === "down") out.push({ key: `${a.id}:down`, kind: "down", text: `${a.id} DOWN`, color: palette.alert });
    if (a.phase === "cooling") {
      const until = a.five_hour_resets ? ` · resets ${new Date(a.five_hour_resets).toLocaleTimeString("en-GB")}` : "";
      out.push({ key: `${a.id}:cooling`, kind: "cooling", text: `${a.id} COOLING${until}`, color: palette.frost });
    }
    if (a.phase === "disabled") out.push({ key: `${a.id}:disabled`, kind: "disabled", text: `${a.id} DISABLED`, color: palette.grey });
  }
  return out;
}
