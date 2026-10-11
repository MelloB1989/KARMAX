import { AnimatePresence, motion } from "motion/react";
import type { FleetEvent } from "../model";

const bad = (k: string) => /Failure|alert|down|OOM|cooling/i.test(k);
const mark = (k: string) => k.startsWith("fleet.") && /done|rotate|assign/.test(k);

/** Live events have id 0 until fleetd records them, so key by id only when it is real. */
export function eventKey(e: FleetEvent, i: number): string {
  return e.id ? `id:${e.id}` : `${e.agent}|${e.session ?? ""}|${e.at}|${e.kind}|${i}`;
}

/** New entries slide in from the top; the ones already shown at mount do not animate. */
export function EventStream({ events }: { events: FleetEvent[] | null }) {
  return (
    <ul className="events">
      <AnimatePresence initial={false}>
        {(events ?? []).slice(0, 40).map((e, i) => (
          <motion.li
            key={eventKey(e, i)}
            layout="position"
            initial={{ y: -18, opacity: 0 }}
            animate={{ y: 0, opacity: 1 }}
            transition={{ duration: 0.3 }}
          >
            <span className="dim">{new Date(e.at).toLocaleTimeString("en-GB")}</span>
            <span className="cyan">{e.agent}</span>
            <span className={bad(e.kind) ? "alert" : mark(e.kind) ? "amber" : ""}>{e.kind}</span>
          </motion.li>
        ))}
      </AnimatePresence>
    </ul>
  );
}
