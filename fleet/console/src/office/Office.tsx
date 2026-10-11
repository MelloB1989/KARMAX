import { Canvas } from "@react-three/fiber";
import { useFleet } from "../state/store";
import { palette } from "../state/derive";
import { Room } from "./Room";
import { Desk } from "./Desk";
import { deskLayout } from "./layout";

export function Office() {
  const { snapshot } = useFleet();
  const agents = snapshot?.agents ?? [];
  const orchName = snapshot?.orchestrator?.name ?? "orchestrator";
  const places = deskLayout(agents.map((a) => a.id), orchName);
  const byId = new Map(agents.map((a) => [a.id, a]));

  return (
    <div style={{ position: "fixed", inset: 0, background: palette.ground }}>
      <Canvas camera={{ position: [0, 5, 9], fov: 45 }} dpr={[1, 1.5]}>
        <color attach="background" args={[palette.ground]} />
        <ambientLight intensity={0.35} />
        <directionalLight position={[4, 8, 6]} intensity={0.5} />
        <Room />
        {places.map((p) => (
          <Desk
            key={p.id}
            {...p}
            agent={byId.get(p.id)}
            orchestrator={p.id === orchName ? { info: snapshot?.orchestrator ?? null, tasks: snapshot?.tasks ?? [] } : undefined}
          />
        ))}
      </Canvas>
    </div>
  );
}
