// A light that runs along the floor from the sender's desk to the receiver's for each new fleet.relay event.
import { useEffect, useRef, useState } from "react";
import { useFrame } from "@react-three/fiber";
import type { Mesh } from "three";
import type { FleetEvent } from "../model";
import { palette } from "../state/derive";
import type { DeskPlace } from "./layout";

export const RELAY_MS = 1500;

export interface Pulse {
  key: number;
  from: [number, number];
  to: [number, number];
}

/** The desk id a relay endpoint stands for: the operator is the orchestrator's desk. */
export function endpoint(name: string, orchestrator: string): string {
  return name === "operator" ? orchestrator : name;
}

/** Pulses for relay events newer than `after`, whose endpoints both have desks. */
export function newPulses(events: FleetEvent[], after: number, places: DeskPlace[], orchestrator: string): Pulse[] {
  const at = new Map(places.map((p) => [p.id, p] as const));
  const out: Pulse[] = [];
  for (const e of [...events].sort((a, b) => a.id - b.id)) {
    if (e.id <= after || e.kind !== "fleet.relay") continue;
    const from = at.get(endpoint(String((e.detail as { from?: string } | null)?.from ?? ""), orchestrator));
    const to = at.get(endpoint(e.agent, orchestrator));
    if (from && to) out.push({ key: e.id, from: [from.x, from.z], to: [to.x, to.z] });
  }
  return out;
}

function Runner({ pulse, onDone }: { pulse: Pulse; onDone: (key: number) => void }) {
  const mesh = useRef<Mesh>(null);
  const t0 = useRef<number | null>(null);
  useFrame(({ clock }) => {
    const now = clock.getElapsedTime() * 1000;
    if (t0.current === null) t0.current = now;
    const k = Math.min(1, (now - t0.current) / RELAY_MS);
    if (mesh.current) {
      mesh.current.position.set(
        pulse.from[0] + (pulse.to[0] - pulse.from[0]) * k,
        0.08,
        pulse.from[1] + (pulse.to[1] - pulse.from[1]) * k,
      );
    }
    if (k >= 1) onDone(pulse.key);
  });
  return (
    <mesh ref={mesh} position={[pulse.from[0], 0.08, pulse.from[1]]}>
      <sphereGeometry args={[0.12, 16, 12]} />
      <meshBasicMaterial color={palette.cyan} toneMapped={false} />
      <pointLight color={palette.cyan} intensity={3} distance={3} decay={2} />
    </mesh>
  );
}

export function RelayLights({ events, places, orchestrator }: { events: FleetEvent[]; places: DeskPlace[]; orchestrator: string }) {
  const seen = useRef<number | null>(null);
  const [active, setActive] = useState<Pulse[]>([]);
  useEffect(() => {
    const max = events.reduce((m, e) => Math.max(m, e.id), 0);
    if (seen.current === null) {
      seen.current = max; // what happened before the office opened is not a pulse
      return;
    }
    const fresh = newPulses(events, seen.current, places, orchestrator);
    seen.current = Math.max(seen.current, max);
    if (fresh.length) setActive((a) => [...a, ...fresh]);
  }, [events, places, orchestrator]);
  const done = (key: number) => setActive((a) => a.filter((p) => p.key !== key));
  return (
    <>
      {active.map((p) => (
        <Runner key={p.key} pulse={p} onDone={done} />
      ))}
    </>
  );
}
