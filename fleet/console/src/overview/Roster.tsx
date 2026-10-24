import type { Snapshot } from "../model";
import { AgentCard, SlotsCard } from "./AgentCard";
import { densityFor, groupAgents, isUp } from "./roster";

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
  // One card per agent; an agent with slots shows a line per slot in it.
  const groups = groupAgents(snapshot.agents);
  const up = groups.filter((g) => g.slots.some(isUp)).length;
  const density = densityFor(groups.length);
  let index = 0;
  return (
    <aside className={`roster roster-${density}`} data-count={groups.length}>
      <div className="section-head">
        <h2>
          AGENTS · {up} / {groups.length} UP
        </h2>
      </div>
      {groups.map((g) => {
        const first = index;
        index += g.slots.length;
        if (g.slots.length === 1) {
          const a = g.slots[0];
          return <AgentCard key={g.id} agent={a} snapshot={snapshot} now={now} index={first} selected={a.id === selected} onSelect={() => onSelect(a.id)} />;
        }
        return <SlotsCard key={g.id} group={g} snapshot={snapshot} now={now} selected={selected} onSelect={onSelect} />;
      })}
    </aside>
  );
}
