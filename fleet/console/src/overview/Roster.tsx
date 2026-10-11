import type { Snapshot } from "../model";
import { AgentCard } from "./AgentCard";
import { orderAgents } from "./roster";

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
  return (
    <aside className="roster">
      {orderAgents(snapshot.agents).map((a, i) => (
        <AgentCard key={a.id} agent={a} snapshot={snapshot} now={now} index={i} selected={a.id === selected} onSelect={() => onSelect(a.id)} />
      ))}
    </aside>
  );
}
