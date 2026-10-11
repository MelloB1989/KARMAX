// agent-02 replaces this with the Mission Control overview (plan Task 2B).
import { useFleet } from "../state/store";

export function Overview() {
  const { snapshot, conn } = useFleet();
  return (
    <pre style={{ color: "#D6E2EC", padding: 24 }}>
      overview · {conn} · {snapshot?.agents?.length ?? 0} agents
    </pre>
  );
}
