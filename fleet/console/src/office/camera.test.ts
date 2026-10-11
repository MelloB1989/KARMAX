import { describe, expect, it } from "vitest";
import { DESK_EYE, HOME_FILL, LOOK_LIFT, easeInOut, flyTarget, homeView, screenFrame } from "./camera";
import { deskLayout } from "./layout";
import { PerspectiveCamera, Vector3 } from "three";

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

  it("fills HOME_FILL of the width with the desks", () => {
    const places = deskLayout(["a1", "a2", "a3", "a4"], "mira");
    const aspect = 16 / 9;
    const { pos, target } = homeView(places, 45, aspect);
    const d = Math.hypot(pos[1] - (target[1] - LOOK_LIFT), pos[2] - target[2]);
    const visible = 2 * Math.tan((45 * Math.PI) / 360) * aspect * d;
    expect((4.8 * 2 + 2.2) / visible).toBeCloseTo(HOME_FILL, 4);
  });
});

describe("the home framing, projected", () => {
  // the corners of every desk with its figure, as the camera sees them: 0..1 across and down the frame
  function project(aspect: number) {
    const places = deskLayout(["a1", "a2", "a3", "a4"], "mira");
    const { pos, target } = homeView(places, 45, aspect);
    const cam = new PerspectiveCamera(45, aspect, 0.1, 100);
    cam.position.set(...pos);
    cam.lookAt(...target);
    cam.updateMatrixWorld();
    cam.updateProjectionMatrix();
    const at = (x: number, y: number, z: number) => {
      const v = new Vector3(x, y, z).project(cam);
      return { x: (v.x + 1) / 2, y: (1 - v.y) / 2 };
    };
    const box = (px: number, pz: number) => {
      const pts = [];
      for (const dx of [-1.1, 1.1]) for (const dz of [-0.5, 0.5, -1.2]) for (const y of [0, 0.8, 1.9]) pts.push(at(px + dx, y, pz + dz));
      return pts;
    };
    return { places, box };
  }

  for (const [w, h] of [[1920, 1080], [1280, 720]]) {
    it(`keeps every desk inside the frame at ${w}x${h}, the row centred near 55%`, () => {
      const { places, box } = project(w / h);
      for (const p of places) {
        for (const pt of box(p.x, p.z)) {
          expect(pt.x, `desk at x=${p.x}`).toBeGreaterThan(0.03);
          expect(pt.x, `desk at x=${p.x}`).toBeLessThan(0.97);
          expect(pt.y).toBeGreaterThan(0.03);
          expect(pt.y).toBeLessThan(0.97);
        }
      }
      const row = places.filter((p) => p.z > -5).flatMap((p) => box(p.x, p.z).map((q) => q.y));
      const centre = (Math.min(...row) + Math.max(...row)) / 2;
      expect(centre).toBeGreaterThan(0.5);
      expect(centre).toBeLessThan(0.6);
    });
  }

  it("shows the head desk", () => {
    const { places, box } = project(16 / 9);
    const head = places[0];
    expect(head.z).toBe(-6);
    const ys = box(head.x, head.z).map((q) => q.y);
    expect(Math.min(...ys)).toBeGreaterThan(0.1);
    expect(Math.max(...ys)).toBeLessThan(0.9);
  });
});
