import { describe, expect, it } from "vitest";
import { easeInOut, flyTarget, screenFrame } from "./camera";

describe("camera helpers", () => {
  it("frames a screen from in front of it, tilted back 15°", () => {
    const f = screenFrame(2, -2);
    expect(f.normal[2]).toBeCloseTo(Math.cos((15 * Math.PI) / 180), 5);
    expect(f.normal[1]).toBeCloseTo(Math.sin((15 * Math.PI) / 180), 5);
    const { pos, target } = flyTarget(f, 45, 16 / 9);
    expect(target).toEqual(f.center);
    expect(pos[2]).toBeGreaterThan(f.center[2]);
    expect(pos[0]).toBeCloseTo(2, 5);
  });

  it("eases from 0 to 1", () => {
    expect(easeInOut(0)).toBe(0);
    expect(easeInOut(1)).toBe(1);
    expect(easeInOut(0.5)).toBeCloseTo(0.5, 5);
  });
});
