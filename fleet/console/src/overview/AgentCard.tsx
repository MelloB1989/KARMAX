import type { Agent, Snapshot } from "../model";
import { ageOf, agoText, fmtAge, isKnown, phaseLook } from "../state/derive";
import { sourceStale } from "../state/cadence";
import { isRemote, slotOf, type AgentGroup } from "./roster";
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
  // Another host's quota is fleetd's (statusline_at is when fleetd knew it).
  const lineStale = remote ? fleetdStale || !isKnown(a.statusline_at) : sourceStale(s, "statusline", now);
  const dockerStale = remote || sourceStale(s, "docker", now);
  const look = phaseLook(a.phase, a.alerts);
  const flash = useFlash(a.phase);
  const toolSecs = a.current_tool ? ageOf(a.current_tool.since, now) : 0;
  const doing =
    a.phase === "working" && a.current_tool && !remote ? (
      <span className="tool">
        {a.current_tool.name} {fmtAge(toolSecs)}
      </span>
    ) : fleetdStale ? (
      <span>idle —</span>
    ) : (
      <span>idle {fmtAge(a.idle_s)}</span>
    );

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
        {remote && (
          <span className="remote" title={`no live view: ${a.host} is not ${s.host.name}`}>
            on {a.host}
          </span>
        )}
        <span className="card-doing dim">{doing}</span>
        <span className="grow" />
        <span className="card-phase">{!isKnown(a.recorded_at) ? <span className="dim">no record</span> : fleetdStale ? <Aged at={a.recorded_at} now={now} /> : look.label}</span>
      </div>
      <div className="card-meta dim">
        {doing}
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

/** What a worker is doing, in a few words: its tool, else its task, else how long it has been idle. */
function doingText(a: Agent, remote: boolean, fleetdStale: boolean, now: number): string {
  if (a.phase === "working" && a.current_tool && !remote) return `${a.current_tool.name} ${fmtAge(ageOf(a.current_tool.since, now))}`;
  if (a.task) return a.task;
  return fleetdStale ? "idle —" : `idle ${fmtAge(a.idle_s)}`;
}

/** The phase that speaks for an agent with slots: the busiest of them. */
const RANK = ["working", "waiting", "cooling", "standby", "disabled", "down"];
function leadPhase(slots: Agent[]): Agent {
  const r = (a: Agent) => (RANK.indexOf(a.phase) + RANK.length + 1) % (RANK.length + 1);
  return [...slots].sort((a, b) => r(a) - r(b))[0];
}

/**
 * An agent with several slots: its name, host, the account's quota and the
 * container on the left (slots share all three), and a line per slot on the
 * right, each selecting that slot's terminal.
 */
export function SlotsCard({
  group: g,
  snapshot: s,
  now,
  selected,
  onSelect,
}: {
  group: AgentGroup;
  snapshot: Snapshot;
  now: number;
  selected: string | null;
  onSelect: (id: string) => void;
}) {
  const lead = leadPhase(g.slots);
  const remote = isRemote(lead, s);
  const fleetdStale = sourceStale(s, "fleetd", now);
  const dockerStale = remote || sourceStale(s, "docker", now);
  const look = phaseLook(lead.phase, lead.alerts);
  // The quota is the account's: the freshest slot's reading of it.
  const q = [...g.slots].sort((a, b) => Date.parse(b.statusline_at ?? "") - Date.parse(a.statusline_at ?? ""))[0];
  const box = g.slots.find((a) => a.container)?.container;
  const isSel = g.slots.some((a) => a.id === selected);
  return (
    <div className={`card card-slots${isSel ? " selected" : ""}`} style={{ ["--phase" as string]: look.color }} data-phase={lead.phase} data-slots={g.slots.length}>
      <div className="slots-agent">
        <span className="card-name">{g.id}</span>
        {remote && (
          <span className="remote" title={`no live view: ${lead.host} is not ${s.host.name}`}>
            on {lead.host}
          </span>
        )}
        <span className="dim slots-quota">
          {isKnown(q.statusline_at) ? `5H ${Math.round(q.five_hour_pct)}% · 7D ${Math.round(q.seven_day_pct)}%` : "quota —"}
        </span>
        <span className="dim slots-box">{dockerStale || !box ? "" : `cpu ${box.cpu_pct.toFixed(1)}% · ${mib(box.mem)}`}</span>
      </div>
      <div className="slots-lines">
        {g.slots.map((a) => {
          const l = phaseLook(a.phase, a.alerts);
          return (
            <button
              type="button"
              key={a.id}
              className={`slot-line${a.id === selected ? " selected" : ""}${l.pulse ? " pulse" : ""}`}
              style={{ ["--phase" as string]: l.color }}
              data-phase={a.phase}
              data-worker={a.id}
              title={a.id}
              onClick={() => onSelect(a.id)}
            >
              <span className="dot" />
              <span className="slot-no">S{slotOf(a)}</span>
              <span className="slot-doing">{doingText(a, isRemote(a, s), fleetdStale, now)}</span>
              <span className="slot-ctx dim">{isKnown(a.statusline_at) ? `ctx ${Math.round(a.context_pct)}%` : ""}</span>
              <span className="slot-phase">{!isKnown(a.recorded_at) ? "no record" : fleetdStale ? agoText(a.recorded_at, now) : l.label}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
