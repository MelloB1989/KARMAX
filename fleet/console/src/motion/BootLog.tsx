import { useEffect, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import type { Snapshot } from "../model";
import { BOOT_TIMEOUT_MS, bootDone, bootLines } from "./boot";

export function BootLog({ snapshot }: { snapshot: Snapshot | null }) {
  const reduce = useReducedMotion();
  const [t0] = useState(() => Date.now());
  const [elapsed, setElapsed] = useState(0);
  const [gone, setGone] = useState(false);
  const lines = bootLines(snapshot, elapsed);

  useEffect(() => {
    const id = setInterval(() => setElapsed(Date.now() - t0), 200);
    return () => clearInterval(id);
  }, [t0]);
  useEffect(() => {
    if (bootDone(lines, elapsed)) {
      // linger a beat so the last line is readable before it fades
      const id = setTimeout(() => setGone(true), Math.min(500, Math.max(0, BOOT_TIMEOUT_MS - elapsed)));
      return () => clearTimeout(id);
    }
  });

  return (
    <AnimatePresence>
      {!gone && (
        <motion.div className="boot" initial={false} exit={{ opacity: 0 }} transition={{ duration: reduce ? 0 : 0.6 }}>
          <pre>
            <div className="boot-title">FLEET CONSOLE · connecting</div>
            {lines.map((l) => (
              <div key={l.key} className={`boot-${l.state}`}>
                {l.at ? new Date(l.at).toLocaleTimeString("en-GB") : "--:--:--"} {l.key.padEnd(11)}{" "}
                {l.state === "ok" ? "ok" : l.state === "pending" ? "waiting…" : (l.error ?? "no answer")}
              </div>
            ))}
          </pre>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
