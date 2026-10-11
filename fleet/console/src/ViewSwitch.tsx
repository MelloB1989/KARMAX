import type { View } from "./App";

/** The O key, as a button: over a remote desktop keys may not reach the page. */
export function ViewSwitch({ view, onToggle }: { view: View; onToggle: () => void }) {
  return (
    <button type="button" className="view-switch" onClick={onToggle} title="O">
      {view === "overview" ? "OFFICE ▸" : "◂ MISSION CONTROL"} · O
    </button>
  );
}
