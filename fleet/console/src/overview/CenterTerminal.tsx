import type { Agent, Snapshot, TermFrame } from "../model";
import { LiveTerminal } from "../term/LiveTerminal";
import { isRemote } from "./roster";
import { ControlBar } from "../focus/ControlBar";
import { CenterHead } from "./CenterHead";
import { wantsControl, type ControlState } from "../focus/controlState";

export function CenterTerminal({
  agent,
  snapshot,
  focus,
  control,
  onFocus,
  onUnfocus,
  onTakeControl,
  onTell,
  onFrame,
  onRelease,
}: {
  agent: Agent | undefined;
  snapshot: Snapshot;
  focus: boolean;
  control: ControlState;
  onFocus: () => void;
  onUnfocus: () => void;
  onTakeControl: () => void;
  onTell: () => void;
  onFrame: (f: TermFrame) => void;
  onRelease: () => void;
}) {
  if (!agent) return <main className="center dim">no agent</main>;
  const held = control.phase === "held";
  const remote = isRemote(agent, snapshot);
  return (
    <main className={`center${held ? " held" : ""}`}>
      <CenterHead
        agent={agent}
        remote={remote}
        focus={focus}
        control={control}
        onUnfocus={onUnfocus}
        onTakeControl={onTakeControl}
        onTell={onTell}
        onRelease={onRelease}
      />
      <ControlBar state={control} />
      <div className="center-term" onClick={onFocus}>
        {remote ? (
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
