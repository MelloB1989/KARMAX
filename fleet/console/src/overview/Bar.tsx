import { palette } from "../state/derive";
import { Num } from "../motion/tween";

export function Bar({ label, pct, color = palette.cyan }: { label: string; pct: number; color?: string }) {
  const p = Math.min(100, Math.max(0, pct));
  return (
    <div className="bar" title={`${label} ${p.toFixed(0)}%`}>
      <span className="bar-label">{label}</span>
      <span className="bar-track">
        <span className="bar-fill" style={{ width: `${p}%`, background: p >= 90 ? palette.alert : color }} />
      </span>
      <span className="bar-num"><Num value={p} /></span>
    </div>
  );
}
