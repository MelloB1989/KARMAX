import type { Agent } from "../model";
import { wantsControl, type ControlState } from "../focus/controlState";

/** The terminal's title row and its actions. */
export function CenterHead({
  agent,
  remote,
  focus,
  control,
  onUnfocus,
  onTakeControl,
  onTell,
  onRelease,
}: {
  agent: Agent;
  remote: boolean;
  focus: boolean;
  control: ControlState;
  onUnfocus: () => void;
  onTakeControl: () => void;
  onTell: () => void;
  onRelease: () => void;
}) {
  return (
    <div className="center-head">
      <span className={`live-dot${remote ? " off" : ""}`} />
      <span className="live-tag">{remote ? "RECORDED" : "LIVE"}</span>
      <span>
        {agent.id} <span className="dim">· tmux main · {agent.term.cols}×{agent.term.rows}</span>
      </span>
      <span className="grow" />
      {focus && (
        <button type="button" className="btn" onClick={onUnfocus}>◂ OVERVIEW · Esc</button>
      )}
      <span className="btn dim" title="hold R for 1 s">ROTATE · hold R</span>
      <button type="button" className="btn" onClick={onTell}>TELL</button>
      {/* Over a remote desktop Ctrl+] and Esc may never reach the page: every key action has a button. */}
      {wantsControl(control) ? (
        <button type="button" className="btn primary" onClick={onRelease}>GIVE BACK · Ctrl+]</button>
      ) : (
        <button type="button" className="btn primary" disabled={remote} onClick={onTakeControl}>TAKE CONTROL ⏎</button>
      )}
    </div>
  );
}
