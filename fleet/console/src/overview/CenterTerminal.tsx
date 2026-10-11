import type { Agent, Snapshot } from "../model";
import { LiveTerminal } from "../term/LiveTerminal";
import { isRemote } from "./roster";

export function CenterTerminal({ agent, snapshot }: { agent: Agent | undefined; snapshot: Snapshot }) {
  if (!agent) return <main className="center dim">no agent</main>;
  return (
    <main className="center">
      <div className="center-title">{agent.id}</div>
      <div className="center-term">
        {isRemote(agent, snapshot) ? (
          <div className="dim noview">remote · no live view</div>
        ) : (
          <LiveTerminal key={agent.id} agent={agent.id} cols={agent.term.cols} rows={agent.term.rows} fit="contain" />
        )}
      </div>
    </main>
  );
}
