import { palette } from "../state/derive";

export function Room({ width = 22, depth = 24 }: { width?: number; depth?: number }) {
  const cz = (depth - 14) / 2 - 8;
  return (
    <group>
      <mesh rotation={[-Math.PI / 2, 0, 0]} position={[0, 0, cz]}>
        <planeGeometry args={[width, depth]} />
        <meshStandardMaterial color="#0C0F17" roughness={0.9} />
      </mesh>
      {/* a faint 2 m grid: depth cue, not decoration */}
      <gridHelper args={[Math.max(width, depth), Math.max(width, depth) / 2, palette.cyan, palette.cyan]} position={[0, 0.005, cz]}>
        <lineBasicMaterial attach="material" color={palette.cyan} transparent opacity={0.07} depthWrite={false} />
      </gridHelper>
      <mesh position={[0, 0.5, cz - depth / 2]}>
        <boxGeometry args={[width, 1, 0.2]} />
        <meshStandardMaterial color="#10141D" roughness={0.9} />
      </mesh>
      {[-1, 1].map((s) => (
        <mesh key={s} position={[(s * width) / 2, 0.5, cz]}>
          <boxGeometry args={[0.2, 1, depth]} />
          <meshStandardMaterial color="#10141D" roughness={0.9} />
        </mesh>
      ))}
    </group>
  );
}
