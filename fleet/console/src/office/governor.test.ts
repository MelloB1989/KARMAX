import { describe, expect, it } from "vitest";
import { createGovernor } from "./governor";

function run(g: ReturnType<typeof createGovernor>, fps: number, ms: number, from = 0) {
  const step = 1000 / fps;
  for (let t = from; t <= from + ms; t += step) g.frame(t);
  return from + ms;
}

describe("governor", () => {
  it("disables bloom after 2 s under 40 fps", () => {
    const g = createGovernor();
    run(g, 30, 1500);
    expect(g.bloom()).toBe(true);
    run(g, 30, 1000, 1500);
    expect(g.bloom()).toBe(false);
    expect(g.fps()).toBeLessThan(40);
  });

  it("keeps bloom at 60 fps", () => {
    const g = createGovernor();
    run(g, 60, 5000);
    expect(g.bloom()).toBe(true);
  });

  it("stays off after fps recovers", () => {
    const g = createGovernor();
    const t = run(g, 20, 2500);
    run(g, 60, 5000, t);
    expect(g.bloom()).toBe(false);
  });
});
