// Mission Control: header, roster, the selected agent's terminal, the right
// rail and the 24 h activity track, laid out at 1920×1080 and scaled to fit.
import { useEffect, useMemo, useReducer, useRef, useState } from "react";
import { MotionConfig, motion, useReducedMotion } from "motion/react";
import { useFleet } from "../state/store";
import { useNow, gpuString } from "../state/hooks";
import { useKey } from "../keys";
import { Header } from "./Header";
import { Roster } from "./Roster";
import { CenterTerminal } from "./CenterTerminal";
import { RightRail } from "./RightRail";
import { ActivityTrack } from "./ActivityTrack";
import { isRemote, orderAgents } from "./roster";
import { controlReducer, wantsControl } from "../focus/controlState";
import { PromptBar } from "../focus/PromptBar";
import { RotateRing } from "../focus/RotateRing";
import { Banners } from "../motion/Banners";
import { BootLog } from "../motion/BootLog";
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

/** Side columns slide out in Focus (about 450 ms) and back on Esc. */
function Collapse({ side, hidden, children }: { side: "left" | "right" | "bottom"; hidden: boolean; children: React.ReactNode }) {
  const reduce = useReducedMotion();
  const vertical = side === "bottom";
  const full = vertical ? 190 : 420;
  return (
    <motion.div
      className={`collapse collapse-${side}`}
      initial={false}
      animate={vertical ? { height: hidden ? 0 : full } : { width: hidden ? 0 : full }}
      transition={{ duration: reduce ? 0 : 0.45, ease: [0.4, 0, 0.2, 1] }}
    >
      {children}
    </motion.div>
  );
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

  const [focus, setFocus] = useState(false);
  const [control, dispatch] = useReducer(controlReducer, { phase: "idle" });
  const [bar, setBar] = useState<{ target: string; label: string } | null>(null);
  const remote = !!current && !!snapshot && isRemote(current, snapshot);
  const orch = snapshot?.orchestrator?.name;

  const releaseControl = () => dispatch({ type: "reset" });
  const selectAgent = (id: string) => {
    if (id !== current?.id) releaseControl();
    setSelected(id);
  };
  useKey("f", () => setFocus(true));
  useKey("escape", () => {
    if (bar) setBar(null);
    else if (control.phase !== "idle") releaseControl();
    else setFocus(false);
  });
  useKey("enter", () => {
    if (!current || remote || wantsControl(control)) return;
    setFocus(true);
    dispatch({ type: "request" });
  });
  useKey("p", () => current && setBar({ target: current.id, label: `PROMPT → ${current.id}` }));
  useKey("m", () => orch && setBar({ target: orch, label: `BRIEF → ${orch}` }));

  return (
    <MotionConfig reducedMotion="user">
    <div className="stage">
      {SELECT_KEYS.map((_, i) => (
        <SelectKey key={i} n={i} onSelect={(k) => sel.current[k] && selectAgent(sel.current[k].id)} />
      ))}
      <div className="root" style={{ transform: `translate(-50%,-50%) scale(${scale})` }}>
        <div className="bg-grid" />
        <div className="scanline" />
        <Header snapshot={snapshot} conn={conn} now={now} gpu={gpu} />
        {snapshot ? (
          <>
            <Collapse side="left" hidden={focus}>
              <Roster snapshot={snapshot} now={now} selected={current?.id ?? null} onSelect={selectAgent} />
            </Collapse>
            <CenterTerminal
              agent={current}
              snapshot={snapshot}
              focus={focus}
              control={control}
              onFocus={() => setFocus(true)}
              onFrame={(f) => dispatch({ type: "frame", frame: f })}
              onRelease={releaseControl}
            />
            <Collapse side="right" hidden={focus}>
              <RightRail snapshot={snapshot} now={now} />
            </Collapse>
            <Collapse side="bottom" hidden={focus}>
              <ActivityTrack snapshot={snapshot} now={now} />
            </Collapse>
            {bar && (
              <div className="bar-dock">
                <PromptBar key={bar.target} target={bar.target} label={bar.label} onClose={() => setBar(null)} />
              </div>
            )}
            {current && !remote && <RotateRing agent={current.id} />}
          </>
        ) : (
          <div className="waiting dim">connecting…</div>
        )}
        <Banners snapshot={snapshot} />
      </div>
      <BootLog snapshot={snapshot} />
    </div>
    </MotionConfig>
  );
}
