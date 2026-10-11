import { type ControlState, refusalText } from "./controlState";

export function ControlBar({ state }: { state: ControlState }) {
  if (state.phase === "held") {
    return (
      <div className="control-bar held">
        YOU HAVE THE KEYBOARD <span className="dim">· Ctrl+] to give it back</span>
      </div>
    );
  }
  const refusal = refusalText(state);
  if (refusal) return <div className="control-bar refused">{refusal}</div>;
  return null;
}
