// Mission Control: header, roster, the selected agent's terminal, the right
// rail and the 24 h activity track, laid out at 1920×1080 and scaled to fit.
import { useEffect, useMemo, useRef, useState } from "react";
import { useFleet } from "../state/store";
import { useNow, gpuString } from "../state/hooks";
import { useKey } from "../keys";
import { Header } from "./Header";
import { Roster } from "./Roster";
import { CenterTerminal } from "./CenterTerminal";
import { RightRail } from "./RightRail";
import { ActivityTrack } from "./ActivityTrack";
import { orderAgents } from "./roster";
import "./theme.css";

const W = 1920;
const H = 1080;

function useFitScale(): number {
  const [scale, setScale] = useState(1);
  useEffect(() => {
    const f = () => setScale(Math.min(window.innerWidth / W, window.innerHeight / H));
    f();
    window.addEventListener("resize", f);
    return () => window.removeEventListener("resize", f);
  }, []);
  return scale;
}

const SELECT_KEYS = ["1", "2", "3", "4", "5", "6", "7", "8", "9"] as const;

function SelectKey({ n, onSelect }: { n: number; onSelect: (i: number) => void }) {
  useKey(String(n + 1), () => onSelect(n));
  return null;
}

export function Overview() {
  const { snapshot, conn } = useFleet();
  const now = useNow(1000);
  const scale = useFitScale();
  const gpu = useMemo(gpuString, []);
  const [selected, setSelected] = useState<string | null>(null);
  const agents = orderAgents(snapshot?.agents ?? null);
  const current = agents.find((a) => a.id === selected) ?? agents[0];
  const sel = useRef(agents);
  sel.current = agents;

  return (
    <div className="stage">
      {SELECT_KEYS.map((_, i) => (
        <SelectKey key={i} n={i} onSelect={(k) => sel.current[k] && setSelected(sel.current[k].id)} />
      ))}
      <div className="root" style={{ transform: `translate(-50%,-50%) scale(${scale})` }}>
        <Header snapshot={snapshot} conn={conn} now={now} gpu={gpu} />
        {snapshot ? (
          <>
            <Roster snapshot={snapshot} now={now} selected={current?.id ?? null} onSelect={setSelected} />
            <CenterTerminal agent={current} snapshot={snapshot} />
            <RightRail snapshot={snapshot} now={now} />
            <ActivityTrack snapshot={snapshot} now={now} />
          </>
        ) : (
          <div className="waiting dim">connecting…</div>
        )}
      </div>
    </div>
  );
}
