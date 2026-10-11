import type { Agent, Snapshot } from "../model";
import { ageOf, agoText, fmtAge, isKnown, phaseLook } from "../state/derive";
import { sourceStale } from "../state/cadence";
import { isRemote } from "./roster";
import { Bar } from "./Bar";
import { Num } from "../motion/tween";
import { useFlash } from "../motion/useFlash";

const mib = (b: number) => `${Math.round(b / 1048576)} MiB`;

/** A stale value renders as its age in dim text, never as the old number. */
function Aged({ at, now }: { at?: string; now: number }) {
  return <span className="dim">{agoText(at, now)}</span>;
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
  const flash = useFlash(a.phase);
  const toolSecs = a.current_tool ? ageOf(a.current_tool.since, now) : 0;

  return (
    <button
      type="button"
      className={`card${selected ? " selected" : ""}${look.pulse ? " pulse" : ""}${flash ? " flash" : ""}`}
      style={{ ["--phase" as string]: look.color }}
      data-phase={a.phase}
      onClick={onSelect}
    >
      <div className="card-head">
        <span className="dot" />
        <span className="card-name">{a.id}</span>
        <span className="card-key dim">{index + 1}</span>
        {remote && <span className="remote">remote · no live view</span>}
        <span className="grow" />
        <span className="card-phase">{!isKnown(a.recorded_at) ? <span className="dim">no record</span> : fleetdStale ? <Aged at={a.recorded_at} now={now} /> : look.label}</span>
      </div>
      <div className="card-meta dim">
        {a.phase === "working" && a.current_tool && !remote ? (
          <span className="tool">
            {a.current_tool.name} {fmtAge(toolSecs)}
          </span>
        ) : fleetdStale ? (
          <span>idle —</span>
        ) : (
          <span>idle {fmtAge(a.idle_s)}</span>
        )}
        <span>{lineStale ? "" : a.model}</span>
        <span>{a.session ? `sess ${a.session.slice(0, 8)}` : ""}</span>
      </div>
      {lineStale ? (
        <div className="card-stale dim">
          quota <Aged at={a.statusline_at} now={now} />
        </div>
      ) : (
        <div className="card-bars">
          <Bar label="CTX" pct={a.context_pct} />
          <Bar label="5H" pct={a.five_hour_pct} />
          <Bar label="7D" pct={a.seven_day_pct} />
        </div>
      )}
      <div className="card-foot">
        <span className="dim">
          last <span className="text">{a.last_task ? `${a.last_task.id}` : "none yet"}</span>
          {a.last_task ? ` · $${a.last_task.cost_usd.toFixed(2)}` : ""}
        </span>
        {dockerStale || !a.container ? (
          <span className="dim">
            cpu / mem <Aged at={a.container?.at} now={now} />
          </span>
        ) : (
          <span className="dim">
            cpu <Num value={a.container.cpu_pct} digits={1} />% · {mib(a.container.mem)}
          </span>
        )}
      </div>
    </button>
  );
}
