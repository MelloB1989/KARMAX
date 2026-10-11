import { AnimatePresence, motion } from "motion/react";
import type { FleetEvent } from "../model";

/** New entries slide in from the top; the ones already shown at mount do not animate. */
export function EventStream({ events }: { events: FleetEvent[] | null }) {
  return (
    <ul className="events">
      <AnimatePresence initial={false}>
        {(events ?? []).slice(0, 60).map((e) => (
          <motion.li
            key={e.id}
            layout="position"
            initial={{ y: -18, opacity: 0 }}
            animate={{ y: 0, opacity: 1 }}
            transition={{ duration: 0.3 }}
          >
            <span className="dim">{new Date(e.at).toLocaleTimeString("en-GB")}</span> <span className="ev-agent">{e.agent}</span>{" "}
            <span className="ev-kind">{e.kind}</span>
          </motion.li>
        ))}
      </AnimatePresence>
    </ul>
  );
}
