import { useRef } from "react";
import { useFrame } from "@react-three/fiber";
import type { Group } from "three";
import { palette } from "../state/derive";

/** A stylised seated figure; the arms type while `typing`. */
export function Figure({ typing, dim }: { typing: boolean; dim: boolean }) {
  const left = useRef<Group>(null);
  const right = useRef<Group>(null);
  useFrame(({ clock }) => {
    const t = clock.getElapsedTime();
    const k = typing ? 1 : 0;
    if (left.current) left.current.rotation.x = -1.1 + k * Math.sin(t * 14) * 0.12;
    if (right.current) right.current.rotation.x = -1.1 + k * Math.sin(t * 14 + 2) * 0.12;
  });
  const body = dim ? "#1C2733" : "#2B3C4D";
  return (
    <group>
      <mesh position={[0, 0.95, 0]}>
        <boxGeometry args={[0.5, 0.65, 0.28]} />
        <meshStandardMaterial color={body} roughness={0.7} />
      </mesh>
      <mesh position={[0, 1.5, 0]}>
        <sphereGeometry args={[0.17, 20, 16]} />
        <meshStandardMaterial color={dim ? "#3A4651" : "#8FA3B5"} roughness={0.6} />
      </mesh>
      {[-1, 1].map((s) => (
        <group key={s} ref={s < 0 ? left : right} position={[s * 0.32, 1.2, 0]}>
          <mesh position={[0, -0.28, 0]}>
            <boxGeometry args={[0.12, 0.6, 0.12]} />
            <meshStandardMaterial color={body} roughness={0.7} />
          </mesh>
        </group>
      ))}
      <mesh position={[0, 0.55, 0.1]}>
        <boxGeometry args={[0.45, 0.2, 0.55]} />
        <meshStandardMaterial color={palette.panel} roughness={0.8} />
      </mesh>
    </group>
  );
}
