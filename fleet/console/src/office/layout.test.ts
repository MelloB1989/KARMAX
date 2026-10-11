import { describe, expect, it } from "vitest";
import { deskLayout } from "./layout";

const ids = (n: number) => Array.from({ length: n }, (_, i) => `agent-${i + 1}`);

describe("deskLayout", () => {
  it("puts the orchestrator at the head", () => {
    const o = deskLayout(ids(2), "mira")[0];
    expect(o).toMatchObject({ id: "mira", x: 0, z: -6 });
  });

  it("fits 4 agents in one row, centred, 3.2 m apart", () => {
    const agents = deskLayout(ids(4), "mira").slice(1);
    expect(new Set(agents.map((d) => d.z)).size).toBe(1);
    expect(agents.map((d) => d.x)).toEqual([-4.8, -1.6, 1.6, 4.8].map((v) => expect.closeTo(v, 5)));
  });

  it("makes 3 rows for 9 agents", () => {
    const agents = deskLayout(ids(9), "mira").slice(1);
    const zs = [...new Set(agents.map((d) => d.z))];
    expect(zs).toHaveLength(3);
    expect(zs[1] - zs[0]).toBeCloseTo(3.2, 5);
    expect(agents.filter((d) => d.z === zs[2])).toHaveLength(1);
  });

  it("is deterministic", () => {
    expect(deskLayout(ids(7), "mira")).toEqual(deskLayout(ids(7), "mira"));
  });
});
