import { AnimatePresence, motion } from "motion/react";
import type { Snapshot } from "../model";
import { bannersFor } from "./banners";

export function Banners({ snapshot }: { snapshot: Snapshot | null }) {
  return (
    <div className="banners">
      <AnimatePresence initial={false}>
        {bannersFor(snapshot).map((b) => (
          <motion.div
            key={b.key}
            className={`banner banner-${b.kind}`}
            style={{ ["--banner" as string]: b.color }}
            initial={{ height: 0, opacity: 0 }}
            animate={{ height: 34, opacity: 1 }}
            exit={{ height: 0, opacity: 0 }}
            transition={{ duration: 0.25 }}
          >
            {b.kind === "disabled" ? "🔒 " : ""}
            {b.text}
          </motion.div>
        ))}
      </AnimatePresence>
    </div>
  );
}
