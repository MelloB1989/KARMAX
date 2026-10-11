import { describe, expect, it } from "vitest";
import { deskLayout } from "./layout";
import { newPulses } from "./RelayLights";
import type { FleetEvent } from "../model";

const places = deskLayout(["agent-01", "agent-02"], "mira");
const ev = (id: number, agent: string, from: string, kind = "fleet.relay"): FleetEvent => ({
  id, at: "2026-10-11T00:00:00Z", agent, kind, detail: { from },
});

describe("relay pulses", () => {
  it("runs from the sender's desk to the receiver's, for new events only", () => {
    const p = newPulses([ev(1, "agent-02", "agent-01"), ev(2, "agent-02", "agent-01")], 1, places, "mira");
    expect(p).toHaveLength(1);
    expect(p[0].key).toBe(2);
  });

  it("ends at the orchestrator's desk for the operator", () => {
    const [p] = newPulses([ev(5, "operator", "agent-01")], 0, places, "mira");
    expect(p.to).toEqual([0, -6]);
  });

  it("skips other kinds and unknown desks", () => {
    expect(newPulses([ev(1, "agent-02", "agent-01", "Stop"), ev(2, "ghost", "agent-01")], 0, places, "mira")).toEqual([]);
  });
});
