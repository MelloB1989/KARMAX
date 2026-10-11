import { AnimatePresence, motion } from "motion/react";
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
            <AnimatePresence initial={false}>
            {(s.activity?.[a.id] ?? []).map((b) => (
              <motion.i
                key={b.t}
                className="tick"
                initial={{ y: -14, opacity: 0 }}
                animate={{ y: 0, opacity: 1 }}
                transition={{ type: "spring", stiffness: 500, damping: 22 }}
                data-frac={bucketFraction(b, now).toFixed(4)}
                style={{ left: `${bucketFraction(b, now) * 100}%`, height: `${20 + (80 * b.n) / max}%` }}
                title={`${b.t} · ${b.n}`}
              />
            ))}
            </AnimatePresence>
          </div>
        </div>
      ))}
    </footer>
  );
}
