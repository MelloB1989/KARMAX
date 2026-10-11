import { useState } from "react";
import { Overview } from "./overview/Overview";
import { Office } from "./office/Office";
import { useKey } from "./keys";

export type View = "overview" | "office";

export function App() {
  const [view, setView] = useState<View>("overview");
  useKey("o", () => setView((v) => (v === "overview" ? "office" : "overview")));
  return view === "overview" ? <Overview /> : <Office />;
}
