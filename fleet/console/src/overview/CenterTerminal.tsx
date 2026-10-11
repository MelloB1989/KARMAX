import type { Agent, Snapshot, TermFrame } from "../model";
import { LiveTerminal } from "../term/LiveTerminal";
import { isRemote } from "./roster";
import { ControlBar } from "../focus/ControlBar";
import type { ControlState } from "../focus/controlState";
import { wantsControl } from "../focus/controlState";

export function CenterTerminal({
  agent,
  snapshot,
  focus,
  control,
  onFocus,
  onFrame,
  onRelease,
}: {
  agent: Agent | undefined;
  snapshot: Snapshot;
  focus: boolean;
  control: ControlState;
  onFocus: () => void;
  onFrame: (f: TermFrame) => void;
  onRelease: () => void;
}) {
  if (!agent) return <main className="center dim">no agent</main>;
  const held = control.phase === "held";
  return (
    <main className="center">
      <div className="center-title">{agent.id}</div>
      <ControlBar state={control} />
      <div className={`center-term${held ? " held" : ""}`} onClick={onFocus}>
        {isRemote(agent, snapshot) ? (
          <div className="dim noview">remote · no live view</div>
        ) : (
          <LiveTerminal
            key={agent.id}
            agent={agent.id}
            cols={agent.term.cols}
            rows={agent.term.rows}
            fit={focus ? "width" : "contain"}
            control={wantsControl(control)}
            onFrame={onFrame}
            onRelease={onRelease}
          />
        )}
      </div>
    </main>
  );
}
