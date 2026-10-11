import type { Snapshot } from "../model";
import { AgentCard } from "./AgentCard";
import { isRemote, orderAgents } from "./roster";

export function Roster({
  snapshot,
  now,
  selected,
  onSelect,
}: {
  snapshot: Snapshot;
  now: number;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const agents = orderAgents(snapshot.agents);
  const up = agents.filter((a) => a.phase !== "down" && !isRemote(a, snapshot)).length;
  return (
    <aside className="roster">
      <div className="section-head">
        <h2>
          AGENTS · {up} / {agents.length} UP
        </h2>
      </div>
      {agents.map((a, i) => (
        <AgentCard key={a.id} agent={a} snapshot={snapshot} now={now} index={i} selected={a.id === selected} onSelect={() => onSelect(a.id)} />
      ))}
    </aside>
  );
}
