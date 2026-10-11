import { AnimatePresence, motion } from "motion/react";
import type { Snapshot } from "../model";
import { bucketFraction, orderAgents } from "./roster";

export function ActivityTrack({ snapshot: s, now }: { snapshot: Snapshot; now: number }) {
  const max = Math.max(1, ...Object.values(s.activity ?? {}).flatMap((bs) => bs.map((b) => b.n)));
  return (
    <footer className="activity">
      <div className="activity-head">
        <span>ACTIVITY · LAST 24 H · ONE TICK PER 15 MIN</span>
        <span>−24 H · −18 · −12 · −6 · NOW</span>
      </div>
      {orderAgents(s.agents).map((a) => (
        <div key={a.id} className="track-row">
          <span className="track-name cyan">{a.id}</span>
          <div className="track">
            <AnimatePresence initial={false}>
              {(s.activity?.[a.id] ?? []).map((b) => (
                <motion.i
                  key={b.t}
                  className="tick"
                  initial={{ y: -14, opacity: 0 }}
                  animate={{ y: 0, opacity: 0.45 + (0.55 * b.n) / max }}
                  transition={{ type: "spring", stiffness: 500, damping: 22 }}
                  data-frac={bucketFraction(b, now).toFixed(4)}
                  style={{ left: `${bucketFraction(b, now) * 100}%` }}
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
