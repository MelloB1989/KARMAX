import type { Snapshot } from "../model";
import { bucketFraction, orderAgents } from "./roster";

export function ActivityTrack({ snapshot: s, now }: { snapshot: Snapshot; now: number }) {
  const max = Math.max(1, ...Object.values(s.activity ?? {}).flatMap((bs) => bs.map((b) => b.n)));
  return (
    <footer className="activity">
      {orderAgents(s.agents).map((a) => (
        <div key={a.id} className="track-row">
          <span className="track-name">{a.id}</span>
          <div className="track">
            {(s.activity?.[a.id] ?? []).map((b) => (
              <i
                key={b.t}
                className="tick"
                data-frac={bucketFraction(b, now).toFixed(4)}
                style={{ left: `${bucketFraction(b, now) * 100}%`, height: `${20 + (80 * b.n) / max}%` }}
                title={`${b.t} · ${b.n}`}
              />
            ))}
          </div>
        </div>
      ))}
    </footer>
  );
}
