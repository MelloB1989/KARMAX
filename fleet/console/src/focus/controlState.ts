// What the panel knows about a held keyboard, driven only by the frames the
// terminal socket sends. The bar says "YOU HAVE THE KEYBOARD" only after the
// server's hello in control mode; until then we are only asking.
import type { TermFrame } from "../model";

export type ControlState =
  | { phase: "idle" }
  | { phase: "requesting" }
  | { phase: "held" }
  | { phase: "refused"; holder?: string; error?: string }
  | { phase: "ended" };

export type ControlAction = { type: "request" } | { type: "frame"; frame: TermFrame } | { type: "reset" };

export function controlReducer(s: ControlState, a: ControlAction): ControlState {
  if (a.type === "reset") return { phase: "idle" };
  if (a.type === "request") return { phase: "requesting" };
  if (s.phase === "idle") return s;
  const f = a.frame;
  switch (f.type) {
    case "hello":
      return f.mode === "control" ? { phase: "held" } : s;
    case "error":
      return { phase: "refused", holder: f.holder, error: f.error };
    case "released":
      // a refusal's close must not wipe the reason it is showing
      return s.phase === "refused" ? s : { phase: "ended" };
    default:
      return s;
  }
}

/** Whether the terminal should be mounted in control mode. */
export const wantsControl = (s: ControlState): boolean => s.phase === "requesting" || s.phase === "held";

export function refusalText(s: ControlState): string | null {
  if (s.phase !== "refused") return null;
  return s.holder ? `held by ${s.holder}` : (s.error ?? "control refused");
}
