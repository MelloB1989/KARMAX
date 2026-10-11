import { useEffect, useRef, useState } from "react";
import { rotate } from "../api";
import { isControlHeld } from "../keys";
import { HoldTracker, ROTATE_HOLD_MS } from "./hold";
import { rotateText } from "./rotate";

const R = 18;
const C = 2 * Math.PI * R;

/** Hold R for a full second to rotate `agent`; shows fleetd's reply after. */
export function RotateRing({ agent }: { agent: string }) {
  const [p, setP] = useState(0);
  const [msg, setMsg] = useState<string | null>(null);
  const agentRef = useRef(agent);
  agentRef.current = agent;

  useEffect(() => {
    let raf = 0;
    const tracker = new HoldTracker(ROTATE_HOLD_MS, () => {
      setMsg("rotating…");
      rotate(agentRef.current)
        .then((r) => setMsg(rotateText(r)))
        .catch((e) => setMsg(e instanceof Error ? e.message : String(e)));
    });
    const loop = () => {
      setP(tracker.tick(performance.now()));
      raf = requestAnimationFrame(loop);
    };
    const isR = (e: KeyboardEvent) => e.key.toLowerCase() === "r" && !e.ctrlKey && !e.metaKey && !e.altKey;
    const typing = (e: KeyboardEvent) => ["INPUT", "TEXTAREA"].includes((e.target as HTMLElement | null)?.tagName ?? "");
    const down = (e: KeyboardEvent) => {
      if (!isR(e) || isControlHeld() || typing(e)) return;
      e.preventDefault();
      if (!e.repeat) {
        setMsg(null);
        tracker.down(performance.now());
        cancelAnimationFrame(raf);
        raf = requestAnimationFrame(loop);
      }
    };
    const up = (e: KeyboardEvent) => {
      if (!isR(e)) return;
      tracker.up();
      cancelAnimationFrame(raf);
      setP(0);
    };
    window.addEventListener("keydown", down, true);
    window.addEventListener("keyup", up, true);
    return () => {
      window.removeEventListener("keydown", down, true);
      window.removeEventListener("keyup", up, true);
      cancelAnimationFrame(raf);
    };
  }, []);

  if (p === 0 && !msg) return null;
  return (
    <div className="rotate-ring">
      <svg width="44" height="44" viewBox="0 0 44 44">
        <circle cx="22" cy="22" r={R} fill="none" stroke="#1B2A38" strokeWidth="4" />
        <circle
          cx="22" cy="22" r={R} fill="none" stroke="#FFB547" strokeWidth="4"
          strokeDasharray={C} strokeDashoffset={C * (1 - p)} transform="rotate(-90 22 22)"
        />
      </svg>
      <span>{msg ?? `hold R to rotate ${agent}: closes its open task as rotated`}</span>
    </div>
  );
}
