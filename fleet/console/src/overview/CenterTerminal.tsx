import type { Agent, Snapshot, TermFrame } from "../model";
import { LiveTerminal } from "../term/LiveTerminal";
import { isRemote } from "./roster";
import { ControlBar } from "../focus/ControlBar";
import { wantsControl, type ControlState } from "../focus/controlState";

export function CenterTerminal({
  agent,
  snapshot,
  focus,
  control,
  onFocus,
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
      <div className="center-head">
        <span className={`live-dot${remote ? " off" : ""}`} />
        <span className="live-tag">{remote ? "RECORDED" : "LIVE"}</span>
        <span>
          {agent.id} <span className="dim">· tmux main · {agent.term.cols}×{agent.term.rows}</span>
        </span>
        <span className="grow" />
        <span className="btn dim" title="hold R for 1 s">ROTATE · hold R</span>
        <button type="button" className="btn" onClick={onTell}>TELL</button>
        <button type="button" className="btn primary" disabled={remote} onClick={onTakeControl}>TAKE CONTROL ⏎</button>
      </div>
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
