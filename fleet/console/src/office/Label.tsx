import { Html } from "@react-three/drei";
import { palette } from "../state/derive";

export interface LabelProps {
  name: string;
  task?: string;
  fiveHourPct: number | null;
  color: string;
  y?: number;
}

function Ring({ pct, color }: { pct: number; color: string }) {
  const r = 9;
  const c = 2 * Math.PI * r;
  return (
    <svg width="24" height="24" viewBox="0 0 24 24" aria-label={`5 h ${Math.round(pct)}%`}>
      <circle cx="12" cy="12" r={r} fill="none" stroke={palette.line} strokeWidth="3" />
      <circle
        cx="12" cy="12" r={r} fill="none" stroke={color} strokeWidth="3"
        strokeDasharray={`${(c * Math.min(100, pct)) / 100} ${c}`}
        transform="rotate(-90 12 12)"
      />
    </svg>
  );
}

export function Label({ name, task, fiveHourPct, color, y = 2.2 }: LabelProps) {
  return (
    <Html position={[0, y, 0]} center distanceFactor={9} style={{ pointerEvents: "none" }}>
      <div
        style={{
          display: "flex", gap: 8, alignItems: "center", padding: "4px 10px",
          background: "rgba(10,16,24,0.82)", border: `1px solid ${palette.line}`,
          borderLeft: `3px solid ${color}`, color: palette.text, whiteSpace: "nowrap",
          font: '13px "IBM Plex Mono", monospace',
        }}
      >
        {fiveHourPct !== null && <Ring pct={fiveHourPct} color={color} />}
        <div>
          <div style={{ font: '600 17px "Barlow Condensed", sans-serif', letterSpacing: 1 }}>{name}</div>
          <div style={{ color: palette.dim, maxWidth: 220, overflow: "hidden", textOverflow: "ellipsis" }}>
            {task || "no task"}
          </div>
        </div>
      </div>
    </Html>
  );
}
