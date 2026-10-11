import { useEffect, useMemo, useRef } from "react";
import { useFrame } from "@react-three/fiber";
import { CanvasTexture, SRGBColorSpace, type PointLight, type MeshStandardMaterial } from "three";
import type { Agent, Orchestrator, Task } from "../model";
import { palette, phaseLook } from "../state/derive";
import { createDeskScreen } from "./painter";
import { drawOrchestratorScreen } from "./orchestratorScreen";
import { Figure } from "./Figure";
import { Label } from "./Label";

export const SCREEN_W = 1.1;
export const SCREEN_H = 0.5;
/** Where the laptop screen's centre sits, relative to the desk group. */
export const SCREEN_POS: [number, number, number] = [0, 1.05, 0.02];
const FRAME_MS = 1000 / 30;
const DEFAULT_TERM = { cols: 220, rows: 50 };

export interface DeskProps {
  id: string;
  x: number;
  z: number;
  rotY: number;
  agent?: Agent;
  orchestrator?: { info: Orchestrator | null; tasks: Task[] };
  onSelect?: (id: string) => void;
}

function useScreenTexture(id: string, agent: Agent | undefined, orch: DeskProps["orchestrator"]) {
  const cols = agent?.term?.cols || DEFAULT_TERM.cols;
  const rows = agent?.term?.rows || DEFAULT_TERM.rows;
  const isOrch = !!orch;
  const paint = useRef<{ tick: (now: number) => void }>({ tick: () => {} });
  const orchRef = useRef(orch);
  orchRef.current = orch;

  const tex = useMemo(() => {
    const canvas = document.createElement("canvas");
    return new CanvasTexture(canvas);
  }, []);

  useEffect(() => {
    tex.colorSpace = SRGBColorSpace;
    if (isOrch) {
      const c = document.createElement("canvas");
      c.width = cols * 8;
      c.height = rows * 16;
      tex.image = c;
      const ctx = c.getContext("2d")!;
      let last = "";
      let at = 0;
      paint.current.tick = (now) => {
        if (now - at < 500) return;
        at = now;
        const o = orchRef.current!;
        const key = JSON.stringify([o.info, o.tasks]);
        if (key === last) return;
        last = key;
        drawOrchestratorScreen(ctx, c.width, c.height, o.info, o.tasks);
        tex.needsUpdate = true;
      };
      return () => void (paint.current.tick = () => {});
    }
    const screen = createDeskScreen(id, cols, rows);
    tex.image = screen.canvas;
    screen.paint();
    tex.needsUpdate = true;
    let at = 0;
    paint.current.tick = (now) => {
      if (now - at < FRAME_MS || !screen.dirty()) return;
      at = now;
      screen.paint();
      tex.needsUpdate = true;
    };
    return () => {
      paint.current.tick = () => {};
      screen.dispose();
    };
  }, [id, cols, rows, isOrch, tex]);

  useEffect(() => () => tex.dispose(), [tex]);
  useFrame(() => paint.current.tick(performance.now()));
  return tex;
}

export function Desk({ id, x, z, rotY, agent, orchestrator, onSelect }: DeskProps) {
  const tex = useScreenTexture(id, agent, orchestrator);
  const phase = orchestrator ? "working" : (agent?.phase ?? "standby");
  const look = phaseLook(phase, agent?.alerts ?? []);
  const down = phase === "down";
  const off = down || phase === "disabled";
  const dim = phase === "standby" || phase === "cooling" || off;
  const lamp = useRef<PointLight>(null);
  const bulb = useRef<MeshStandardMaterial>(null);
  const screenMat = useRef<MeshStandardMaterial>(null);

  useFrame(({ clock }) => {
    const t = clock.getElapsedTime();
    const pulse = look.pulse ? 0.6 + 0.4 * Math.sin(t * 3) : 1;
    const base = off ? 0 : phase === "working" ? 2.2 : phase === "standby" ? 0.5 : phase === "cooling" ? 0.8 : 1.4;
    if (lamp.current) lamp.current.intensity = base * pulse;
    if (bulb.current) bulb.current.emissiveIntensity = base * pulse;
    if (screenMat.current) screenMat.current.emissiveIntensity = down ? 0 : (dim ? 0.45 : 0.9) * (look.pulse ? 0.8 + 0.2 * Math.sin(t * 3) : 1);
  });

  const wood = phase === "cooling" ? "#2A3A4A" : "#1B2733";
  return (
    <group
      position={[x, 0, z]}
      rotation={[0, rotY, 0]}
      onClick={(e) => {
        e.stopPropagation();
        onSelect?.(id);
      }}
    >
      {/* desk: top, legs, front panel */}
      <mesh position={[0, 0.74, 0]}>
        <boxGeometry args={[1.8, 0.06, 0.8]} />
        <meshStandardMaterial color={wood} roughness={0.55} metalness={0.2} />
      </mesh>
      {[-0.85, 0.85].map((lx) => (
        <mesh key={lx} position={[lx, 0.36, 0]}>
          <boxGeometry args={[0.06, 0.72, 0.7]} />
          <meshStandardMaterial color={palette.line} roughness={0.6} metalness={0.3} />
        </mesh>
      ))}
      <mesh position={[0, 0.5, 0.36]}>
        <boxGeometry args={[1.7, 0.45, 0.04]} />
        <meshStandardMaterial color={palette.panel} roughness={0.7} />
      </mesh>
      {/* laptop: base plus an open screen at 105° */}
      <mesh position={[0, 0.79, 0.05]}>
        <boxGeometry args={[1.12, 0.03, 0.6]} />
        <meshStandardMaterial color="#222E3A" roughness={0.4} metalness={0.6} />
      </mesh>
      <group position={[0, 0.8, -0.25]} rotation={[-(15 * Math.PI) / 180, 0, 0]}>
        <mesh position={[0, SCREEN_H / 2 + 0.02, 0]}>
          <boxGeometry args={[SCREEN_W + 0.06, SCREEN_H + 0.06, 0.03]} />
          <meshStandardMaterial color="#1A242E" roughness={0.4} metalness={0.6} />
        </mesh>
        <mesh position={[0, SCREEN_H / 2 + 0.02, 0.02]} name="screen">
          <planeGeometry args={[SCREEN_W, SCREEN_H]} />
          <meshStandardMaterial
            ref={screenMat}
            map={tex}
            emissiveMap={tex}
            emissive="#ffffff"
            color={down ? "#000000" : "#ffffff"}
            toneMapped={false}
          />
        </mesh>
      </group>
      {/* lamp */}
      <group position={[0.7, 0.77, -0.2]}>
        <mesh position={[0, 0.25, 0]}>
          <cylinderGeometry args={[0.012, 0.012, 0.5]} />
          <meshStandardMaterial color="#222E3A" metalness={0.6} roughness={0.4} />
        </mesh>
        <mesh position={[0, 0.52, 0.03]}>
          <coneGeometry args={[0.1, 0.12, 16, 1, true]} />
          <meshStandardMaterial ref={bulb} color="#10161D" emissive={look.color} side={2} />
        </mesh>
        <pointLight ref={lamp} position={[0, 0.5, 0.1]} color={look.color} distance={4} decay={2} />
      </group>
      {/* a lock on a disabled desk */}
      {phase === "disabled" && (
        <group position={[-0.7, 0.9, 0.2]}>
          <mesh>
            <boxGeometry args={[0.14, 0.11, 0.06]} />
            <meshStandardMaterial color={palette.grey} />
          </mesh>
          <mesh position={[0, 0.08, 0]}>
            <torusGeometry args={[0.04, 0.012, 8, 16, Math.PI]} />
            <meshStandardMaterial color={palette.grey} />
          </mesh>
        </group>
      )}
      {/* frost on a cooling desk */}
      {phase === "cooling" && (
        <mesh position={[0, 0.775, 0.05]}>
          <boxGeometry args={[1.84, 0.01, 0.84]} />
          <meshStandardMaterial color={palette.frost} transparent opacity={0.35} emissive={palette.frost} emissiveIntensity={0.2} />
        </mesh>
      )}
      {/* chair and figure, on the far side of the desk */}
      <group position={[0, 0, -0.85]} rotation={[0, Math.PI, 0]}>
        <mesh position={[0, 0.45, 0]}>
          <cylinderGeometry args={[0.22, 0.22, 0.06, 20]} />
          <meshStandardMaterial color={palette.line} roughness={0.7} />
        </mesh>
        <mesh position={[0, 0.2, 0]}>
          <cylinderGeometry args={[0.03, 0.03, 0.4]} />
          <meshStandardMaterial color="#222E3A" metalness={0.6} />
        </mesh>
        <mesh position={[0, 0.85, 0.2]}>
          <boxGeometry args={[0.45, 0.7, 0.06]} />
          <meshStandardMaterial color={palette.line} roughness={0.8} />
        </mesh>
        <Figure typing={phase === "working"} dim={dim} />
      </group>
      <Label
        name={id}
        task={orchestrator ? `${orchestrator.tasks.length} open` : agent?.task}
        fiveHourPct={orchestrator ? (orchestrator.info?.five_hour_pct ?? null) : (agent?.five_hour_pct ?? null)}
        color={look.color}
      />
    </group>
  );
}
