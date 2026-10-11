import { useEffect, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import type { Snapshot } from "../model";
import { bootDone, bootLines } from "./boot";

export function BootLog({ snapshot }: { snapshot: Snapshot | null }) {
  const reduce = useReducedMotion();
  const [t0] = useState(() => Date.now());
  const [elapsed, setElapsed] = useState(0);
  const [gone, setGone] = useState(false);
  const lines = bootLines(snapshot, elapsed);
  const done = bootDone(lines, elapsed);

  // Tick only until done; once done, the fade timer below must not be reset by re-renders.
  useEffect(() => {
    if (done) return;
    const id = setInterval(() => setElapsed(Date.now() - t0), 200);
    return () => clearInterval(id);
  }, [t0, done]);
  useEffect(() => {
    if (!done) return;
    // linger a beat so the last line is readable before it fades
    const id = setTimeout(() => setGone(true), 500);
    return () => clearTimeout(id);
  }, [done]);

  const log = (
    <pre>
      <div className="boot-title">FLEET CONSOLE · connecting</div>
      {lines.map((l) => (
        <div key={l.key} className={`boot-${l.state}`}>
          {l.at ? new Date(l.at).toLocaleTimeString("en-GB") : "--:--:--"} {l.key.padEnd(11)}{" "}
          {l.state === "ok" ? "ok" : l.state === "pending" ? "waiting…" : (l.error ?? "no answer")}
        </div>
      ))}
    </pre>
  );

  // Reduced motion: no fade, the log simply goes.
  if (reduce) return gone ? null : <div className="boot">{log}</div>;
  return (
    <AnimatePresence>
      {!gone && (
        <motion.div className="boot" initial={false} exit={{ opacity: 0 }} transition={{ duration: 0.6 }}>
          {log}
        </motion.div>
      )}
    </AnimatePresence>
  );
}
