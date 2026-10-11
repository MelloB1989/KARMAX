import { palette } from "../state/derive";
import { Num } from "../motion/tween";

/** A thin quota bar: label and percentage over a 4 px track. */
export function Bar({ label, pct, color = palette.cyan }: { label: string; pct: number; color?: string }) {
  const p = Math.min(100, Math.max(0, pct));
  return (
    <div className="bar" title={`${label} ${p.toFixed(0)}%`}>
      <div className="bar-top">
        <span className="dim">{label}</span>
        <span>
          <Num value={p} />%
        </span>
      </div>
      <div className="bar-track">
        <div className="bar-fill" style={{ width: `${Math.max(p, 1)}%`, background: p >= 90 ? palette.alert : color }} />
      </div>
    </div>
  );
}
