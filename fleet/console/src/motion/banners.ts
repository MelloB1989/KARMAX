// Banners are derived from the snapshot on every render, so they are
// dismissed only by the condition clearing; there is nothing to close.
import type { Snapshot } from "../model";
import { palette } from "../state/derive";
import { agentOf, orderAgents } from "../overview/roster";

export interface Banner {
  key: string;
  kind: "stuck" | "down" | "cooling" | "disabled";
  text: string;
  color: string;
}

export function bannersFor(s: Snapshot | null): Banner[] {
  if (!s) return [];
  const out: Banner[] = [];
  const said = new Set<string>();
  for (const a of orderAgents(s.agents)) {
    // Down is the container's, cooling and disabled the subscription's: an
    // agent's slots share both, so it is said once, by the agent's name.
    const once = (kind: Banner["kind"], text: string, color: string) => {
      if (said.has(`${agentOf(a)}:${kind}`)) return;
      said.add(`${agentOf(a)}:${kind}`);
      out.push({ key: `${agentOf(a)}:${kind}`, kind, text, color });
    };
    for (const al of a.alerts ?? []) {
      if (al === "stuck" || al.startsWith("stuck")) {
        out.push({ key: `${a.id}:${al}`, kind: "stuck", text: `${a.id} ${al}`.toUpperCase(), color: palette.alert });
      }
    }
    if (a.phase === "down") once("down", `${agentOf(a)} DOWN`, palette.alert);
    if (a.phase === "cooling") {
      const until = a.five_hour_resets ? ` · resets ${new Date(a.five_hour_resets).toLocaleTimeString("en-GB")}` : "";
      once("cooling", `${agentOf(a)} COOLING${until}`, palette.frost);
    }
    if (a.phase === "disabled") once("disabled", `${agentOf(a)} DISABLED`, palette.grey);
  }
  return out;
}
