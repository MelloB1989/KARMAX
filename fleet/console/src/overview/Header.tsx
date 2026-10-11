import type { Snapshot } from "../model";
import type { Conn } from "../state/store";
import { ageOf, fmtAge } from "../state/derive";
import { sourceStale } from "../state/cadence";

export function Header({ snapshot: s, conn, now, gpu }: { snapshot: Snapshot | null; conn: Conn; now: number; gpu: string }) {
  const hostStale = s ? sourceStale(s, "host", now) : true;
  const h = s?.host;
  return (
    <header className="header">
      <div className="brand">FLEET CONSOLE</div>
      <div className="vitals">
        {h && (hostStale ? (
          <span className="dim">{h.name} · host {fmtAge(ageOf(h.at, now))} ago</span>
        ) : (
          <span>
            {h.name} · load {h.load.map((l) => l.toFixed(2)).join(" ")} · mem {Math.round((h.mem_used / h.mem_total) * 100)}% · up {fmtAge(h.uptime_s)}
          </span>
        ))}
      </div>
      <div className="grow" />
      <div className="dim gpu" title="WebGL renderer">{gpu}</div>
      <div className={`conn conn-${conn}`}>{conn}</div>
      <div className="clock">{new Date(now).toLocaleTimeString("en-GB")}</div>
    </header>
  );
}
