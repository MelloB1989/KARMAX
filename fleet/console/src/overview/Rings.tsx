import { Num } from "../motion/tween";

/** Two concentric quota rings: 5 h outside (amber), 7 d inside (cyan). */
export function Rings({ five, seven }: { five: number; seven: number }) {
  const f = Math.min(100, Math.max(0, five));
  const d = Math.min(100, Math.max(0, seven));
  return (
    <div className="rings" style={{ background: `conic-gradient(#FFB547 0 ${f}%, #14212D ${f}% 100%)` }}>
      <div className="ring-in" style={{ background: `conic-gradient(#5CE1E6 0 ${d}%, #14212D ${d}% 100%)` }} />
      <div className="ring-core">
        <span className="ring-num">
          <Num value={f} />%
        </span>
        <span className="vital-k">5 HOUR</span>
      </div>
      <div className="ring-sweep" aria-hidden="true" />
    </div>
  );
}
