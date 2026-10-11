import { useEffect, useMemo, useRef, useState } from "react";
import { Canvas, useFrame, useThree } from "@react-three/fiber";
import { CameraControls } from "@react-three/drei";
import { Bloom, EffectComposer } from "@react-three/postprocessing";
import { Vector3, type PerspectiveCamera } from "three";
import type CameraControlsImpl from "camera-controls";
import { useFleet } from "../state/store";
import { palette } from "../state/derive";
import { useKey } from "../keys";
import { LiveTerminal } from "../term/LiveTerminal";
import type { TermFrame } from "../model";
import { Room } from "./Room";
import { Desk } from "./Desk";
import { deskLayout } from "./layout";
import { RelayLights } from "./RelayLights";
import { createGovernor, type Governor } from "./governor";
import { FLY_MS, easeInOut, flyTarget, homeView, screenFrame, type V3 } from "./camera";
import type { DeskPlace } from "./layout";

const FOV = 45;
const DEG = Math.PI / 180;

interface Rect {
  left: number;
  top: number;
  width: number;
  height: number;
}

interface Fly {
  from: { pos: V3; target: V3 };
  to: { pos: V3; target: V3 };
  t0: number | null;
  done?: () => void;
}

interface RigProps {
  places: DeskPlace[];
  selected: string | null;
  focusAt: { x: number; z: number } | null;
  flown: boolean;
  onFlown: () => void;
  onRect: (r: Rect | null) => void;
  governor: Governor;
  onStats: (fps: number, bloom: boolean) => void;
}

/** Camera drift, orbit, fly-in/out, the screen's projected rectangle, and the fps governor. */
function Rig({ places, selected, focusAt, flown, onFlown, onRect, governor, onStats }: RigProps) {
  const controls = useRef<CameraControlsImpl>(null);
  const { camera, size } = useThree();
  const fly = useRef<Fly | null>(null);
  const dragging = useRef(false);
  const base = useRef({ az: 0, pol: 0, ready: false });
  const lastRect = useRef<Rect | null>(null);
  const lastStats = useRef(0);
  const prevSelected = useRef<string | null>(null);

  const aspect = size.width / size.height;
  const home = useMemo(() => homeView(places, FOV, aspect), [places, aspect]);
  const homeRef = useRef(home);
  homeRef.current = home;

  // frame the desk cluster whenever the roster or the window changes (not mid-selection)
  useEffect(() => {
    const c = controls.current;
    if (!c || prevSelected.current) return;
    c.setLookAt(...home.pos, ...home.target, false);
    base.current.ready = false;
  }, [home]);

  useEffect(() => {
    const c = controls.current;
    if (!c) return;
    c.minDistance = 2;
    c.maxDistance = 26;
    c.maxPolarAngle = 88 * DEG;
    const start = () => (dragging.current = true);
    const end = () => {
      dragging.current = false;
      base.current.ready = false; // re-anchor the drift where the operator left the camera
    };
    c.addEventListener("controlstart", start);
    c.addEventListener("controlend", end);
    return () => {
      c.removeEventListener("controlstart", start);
      c.removeEventListener("controlend", end);
    };
  }, []);

  // start a fly when the selection changes
  useEffect(() => {
    const c = controls.current;
    if (!c || prevSelected.current === selected) return;
    prevSelected.current = selected;
    const pos = c.getPosition(new Vector3(), true).toArray() as V3;
    const target = c.getTarget(new Vector3(), true).toArray() as V3;
    if (selected && focusAt) {
      const to = flyTarget(screenFrame(focusAt.x, focusAt.z), FOV, size.width / size.height);
      fly.current = { from: { pos, target }, to, t0: null, done: onFlown };
      c.enabled = false;
    } else {
      fly.current = {
        from: { pos, target },
        to: { pos: homeRef.current.pos, target: homeRef.current.target },
        t0: null,
        done: () => {
          c.enabled = true;
          base.current.ready = false;
        },
      };
    }
    onRect(null);
    lastRect.current = null;
  }, [selected, focusAt, size.width, size.height, onFlown, onRect]);

  useFrame(({ clock }) => {
    const c = controls.current;
    const nowMs = performance.now();
    governor.frame(nowMs);
    if (nowMs - lastStats.current > 500) {
      lastStats.current = nowMs;
      onStats(governor.fps(), governor.bloom());
    }
    if (!c) return;
    const t = clock.getElapsedTime();

    const f = fly.current;
    if (f) {
      if (f.t0 === null) f.t0 = nowMs;
      const k = Math.min(1, (nowMs - f.t0) / FLY_MS);
      const e = easeInOut(k);
      const mix = (a: V3, b: V3): V3 => [a[0] + (b[0] - a[0]) * e, a[1] + (b[1] - a[1]) * e, a[2] + (b[2] - a[2]) * e];
      c.setLookAt(...mix(f.from.pos, f.to.pos), ...mix(f.from.target, f.to.target), false);
      if (k >= 1) {
        fly.current = null;
        f.done?.();
      }
      return;
    }

    if (selected) {
      // flown in: keep the DOM terminal on the screen's projected rectangle
      if (!flown || !focusAt) return;
      const fr = screenFrame(focusAt.x, focusAt.z);
      camera.updateMatrixWorld();
      const pts = fr.corners.map((p) => new Vector3(...p).project(camera as PerspectiveCamera));
      const xs = pts.map((p) => ((p.x + 1) / 2) * size.width);
      const ys = pts.map((p) => ((1 - p.y) / 2) * size.height);
      const r = { left: Math.min(...xs), top: Math.min(...ys), width: Math.max(...xs) - Math.min(...xs), height: Math.max(...ys) - Math.min(...ys) };
      const l = lastRect.current;
      if (!l || Math.abs(l.left - r.left) + Math.abs(l.top - r.top) + Math.abs(l.width - r.width) + Math.abs(l.height - r.height) > 0.5) {
        lastRect.current = r;
        onRect(r);
      }
      return;
    }

    if (dragging.current) return;
    // idle drift: a Lissajous of ±4° over 24 s
    const driftAz = 4 * DEG * Math.sin((2 * Math.PI * t) / 24);
    const driftPol = 2 * DEG * Math.sin((2 * Math.PI * t) / 16 + 1);
    const b = base.current;
    if (!b.ready) {
      b.az = c.azimuthAngle - driftAz;
      b.pol = c.polarAngle - driftPol;
      b.ready = true;
    }
    void c.rotateAzimuthTo(b.az + driftAz, false);
    void c.rotatePolarTo(b.pol + driftPol, false);
  });

  return <CameraControls ref={controls} makeDefault />;
}

export function Office() {
  const { snapshot } = useFleet();
  const agents = snapshot?.agents ?? [];
  const orchName = snapshot?.orchestrator?.name ?? "orchestrator";
  const places = useMemo(() => deskLayout(agents.map((a) => a.id), orchName), [agents, orchName]);
  const byId = new Map(agents.map((a) => [a.id, a]));

  const [selected, setSelected] = useState<string | null>(null);
  const [flown, setFlown] = useState(false);
  const [rect, setRect] = useState<Rect | null>(null);
  const [control, setControl] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [stats, setStats] = useState({ fps: 0, bloom: true });
  const governor = useRef(createGovernor()).current;

  const place = places.find((p) => p.id === selected) ?? null;
  const focusAt = useMemo(() => (place ? { x: place.x, z: place.z } : null), [place?.x, place?.z]); // eslint-disable-line react-hooks/exhaustive-deps
  const agent = selected ? byId.get(selected) : undefined;

  const select = (id: string) => {
    setFlown(false);
    setControl(false);
    setError(null);
    setSelected(id);
  };
  const leave = () => {
    setFlown(false);
    setControl(false);
    setSelected(null);
  };
  useKey("escape", () => selected && leave());
  useKey("enter", () => {
    if (selected && flown && agent && !control) setControl(true);
  });

  const onFrame = (f: TermFrame) => {
    if (f.type === "error") {
      setControl(false);
      setError(f.holder ? `held by ${f.holder}` : (f.error ?? "control refused"));
    }
  };

  return (
    <div style={{ position: "fixed", inset: 0, background: "radial-gradient(ellipse 1100px 700px at 50% 60%,#141826 0%,#07080C 72%)" }}>
      <Canvas camera={{ position: [0, 8, 14], fov: FOV }} dpr={[1, 1.5]} onPointerMissed={() => selected && !control && leave()}>
        <color attach="background" args={["#07080C"]} />
        <fog attach="fog" args={["#07080C", 16, 42]} />
        <ambientLight intensity={0.28} color="#8FA0B5" />
        <directionalLight position={[3, 7, 8]} intensity={0.25} />
        {/* rim light from behind the desks: edges of figures and chairs against the dark */}
        <directionalLight position={[0, 5, -16]} intensity={1.1} color="#8BE9FF" />
        <Rig
          places={places}
          selected={selected}
          focusAt={focusAt}
          flown={flown}
          onFlown={() => setFlown(true)}
          onRect={setRect}
          governor={governor}
          onStats={(fps, bloom) => setStats({ fps, bloom })}
        />
        <Room />
        {places.map((p) => (
          <Desk
            key={p.id}
            {...p}
            agent={byId.get(p.id)}
            orchestrator={p.id === orchName ? { info: snapshot?.orchestrator ?? null, tasks: snapshot?.tasks ?? [] } : undefined}
            onSelect={select}
          />
        ))}
        <RelayLights events={snapshot?.events ?? []} places={places} orchestrator={orchName} />
        {stats.bloom && (
          <EffectComposer multisampling={0} resolutionScale={0.5}>
            <Bloom intensity={0.9} luminanceThreshold={0.7} luminanceSmoothing={0.2} mipmapBlur resolutionScale={0.5} />
          </EffectComposer>
        )}
      </Canvas>

      {selected && flown && agent && rect && (
        <div
          style={{
            position: "absolute", ...rect, background: "#03060A", opacity: 1, animation: "office-fade 300ms ease-out",
            outline: control ? `2px solid ${palette.amber}` : "none",
          }}
        >
          <LiveTerminal
            agent={agent.id}
            cols={agent.term?.cols || 220}
            rows={agent.term?.rows || 50}
            control={control}
            fit="contain"
            onFrame={onFrame}
            onRelease={() => setControl(false)}
          />
        </div>
      )}
      {selected && flown && (
        <div style={{ position: "absolute", left: 16, bottom: 16, color: palette.dim, font: '12px "IBM Plex Mono", monospace' }}>
          {error && <span style={{ color: palette.alert }}>{error} · </span>}
          {control ? "Ctrl+] gives the keyboard back" : agent ? "Enter takes control · Esc leaves" : "Esc leaves"}
        </div>
      )}
      <div style={{ position: "absolute", right: 12, top: 8, color: palette.dim, font: '11px "IBM Plex Mono", monospace' }}>
        {stats.fps > 0 ? `${Math.round(stats.fps)} fps` : "— fps"}
        {!stats.bloom && " · bloom off"}
      </div>
      <div
        aria-hidden
        style={{ position: "absolute", inset: 0, pointerEvents: "none", background: "radial-gradient(ellipse at 50% 55%, transparent 55%, rgba(0,0,0,.45) 100%)" }}
      />
      <style>{"@keyframes office-fade{from{opacity:0}to{opacity:1}}"}</style>
    </div>
  );
}
