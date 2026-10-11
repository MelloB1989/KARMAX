import { useState } from "react";
import { Overview } from "./overview/Overview";
import { Office } from "./office/Office";
import { useKey } from "./keys";
import { ViewSwitch } from "./ViewSwitch";
import { useFleet } from "./state/store";
import { useAutoReload } from "./state/autoreload";

export type View = "overview" | "office";

export function App() {
  const [view, setView] = useState<View>("overview");
  useAutoReload(useFleet().snapshot?.build);
  const toggle = () => setView((v) => (v === "overview" ? "office" : "overview"));
  useKey("o", toggle);
  return (
    <>
      {view === "overview" ? <Overview /> : <Office />}
      <ViewSwitch view={view} onToggle={toggle} />
    </>
  );
}

