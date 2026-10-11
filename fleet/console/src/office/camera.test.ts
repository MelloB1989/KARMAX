import { describe, expect, it } from "vitest";
import { DESK_EYE, LOOK_LIFT, easeInOut, flyTarget, homeView, screenFrame } from "./camera";
import { deskLayout } from "./layout";

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

  it("aims the home view at the desks' centroid, 25° up", () => {
    const places = deskLayout(["a1", "a2", "a3", "a4", "a5"], "mira");
    const { pos, target } = homeView(places, 45, 16 / 9);
    const cx = places.reduce((s, p) => s + p.x, 0) / places.length;
    const cz = places.reduce((s, p) => s + p.z, 0) / places.length;
    expect(target[0]).toBeCloseTo(cx, 6);
    expect(target[2]).toBeCloseTo(cz, 6);
    const dy = pos[1] - (target[1] - LOOK_LIFT);
    const dz = pos[2] - target[2];
    expect(target[1]).toBeCloseTo(DESK_EYE + LOOK_LIFT, 6);
    expect(Math.atan2(dy, dz) * (180 / Math.PI)).toBeCloseTo(25, 4);
  });

  it("fills ~75% of the width with the desks", () => {
    const places = deskLayout(["a1", "a2", "a3", "a4"], "mira");
    const aspect = 16 / 9;
    const { pos, target } = homeView(places, 45, aspect);
    const d = Math.hypot(pos[1] - (target[1] - LOOK_LIFT), pos[2] - target[2]);
    const visible = 2 * Math.tan((45 * Math.PI) / 360) * aspect * d;
    expect((4.8 * 2 + 2.2) / visible).toBeCloseTo(0.75, 4);
  });
});
