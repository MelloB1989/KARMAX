import type { Agent, Snapshot } from "../model";
import { ageOf, fmtAge, phaseLook } from "../state/derive";
import { sourceStale } from "../state/cadence";
import { isRemote } from "./roster";
import { Bar } from "./Bar";

const mb = (b: number) => `${Math.round(b / 1048576)} MB`;

/** A stale value renders as its age in dim text, never as the old number. */
function Aged({ at, now }: { at?: string; now: number }) {
  return <span className="dim">{at ? `${fmtAge(ageOf(at, now))} ago` : "no data"}</span>;
}

export function AgentCard({
  agent: a,
  snapshot: s,
  now,
  index,
  selected,
  onSelect,
}: {
  agent: Agent;
  snapshot: Snapshot;
  now: number;
  index: number;
  selected: boolean;
  onSelect: () => void;
}) {
  const remote = isRemote(a, s);
  const fleetdStale = sourceStale(s, "fleetd", now);
  const lineStale = remote || sourceStale(s, "statusline", now);
  const dockerStale = remote || sourceStale(s, "docker", now);
  const look = phaseLook(a.phase, a.alerts);
  const toolSecs = a.current_tool ? ageOf(a.current_tool.since, now) : 0;

  return (
    <button
      type="button"
      className={`card${selected ? " selected" : ""}${look.pulse ? " pulse" : ""}`}
      style={{ ["--phase" as string]: look.color }}
      data-phase={a.phase}
      onClick={onSelect}
    >
      <div className="card-head">
        <span className="card-key">{index + 1}</span>
        <span className="card-name">{a.id}</span>
        {remote && <span className="tag">remote · no live view</span>}
        <span className="card-phase">
          {fleetdStale ? <Aged at={a.recorded_at} now={now} /> : look.label}
        </span>
      </div>
      <div className="card-line">
        {a.phase === "working" && a.current_tool && !remote ? (
          <span className="tool">
            {a.current_tool.name} <span className="dim">{fmtAge(toolSecs)}</span>
          </span>
        ) : fleetdStale ? (
          <span className="dim">idle —</span>
        ) : (
          <span className="dim">idle {fmtAge(a.idle_s)}</span>
        )}
        <span className="dim card-model">{lineStale ? "" : a.model}</span>
      </div>
      <div className="card-bars">
        {lineStale ? (
          <span className="dim">
            quota <Aged at={a.statusline_at} now={now} />
          </span>
        ) : (
          <>
            <Bar label="ctx" pct={a.context_pct} />
            <Bar label="5h" pct={a.five_hour_pct} />
            <Bar label="7d" pct={a.seven_day_pct} />
          </>
        )}
      </div>
      <div className="card-foot">
        {dockerStale || !a.container ? (
          <span className="dim">
            cpu / mem <Aged at={a.container?.at} now={now} />
          </span>
        ) : (
          <span>
            {a.container.cpu_pct.toFixed(0)}% cpu · {mb(a.container.mem)}
          </span>
        )}
      </div>
      <div className="card-last dim">
        {a.last_task ? `last ${a.last_task.id} · ${a.last_task.outcome} · $${a.last_task.cost_usd.toFixed(2)}` : "no task yet"}
      </div>
    </button>
  );
}
