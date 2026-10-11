// Where a desk's laptop screen is in the world, and where the camera must be to frame it.
import { SCREEN_H, SCREEN_W } from "./Desk";

const TILT = (15 * Math.PI) / 180; // the screen leans back 15° past vertical
export const FLY_MS = 1200;

export type V3 = [number, number, number];

export interface ScreenFrame {
  center: V3;
  normal: V3;
  /** The four corners: top-left, top-right, bottom-right, bottom-left. */
  corners: [V3, V3, V3, V3];
}

/** The screen of the desk at (x, z), facing +z when rotY is 0. */
export function screenFrame(x: number, z: number, rotY = 0): ScreenFrame {
  const c = Math.cos(rotY);
  const s = Math.sin(rotY);
  const world = (lx: number, ly: number, lz: number): V3 => [x + lx * c + lz * s, ly, z - lx * s + lz * c];
  // hinge at (0, 0.8, -0.25); local screen centre (0, SCREEN_H/2 + 0.02, 0.02)
  const ly = SCREEN_H / 2 + 0.02;
  const lz = 0.02;
  const yy = (v: number, w: number) => v * Math.cos(TILT) + w * Math.sin(TILT);
  const zz = (v: number, w: number) => -v * Math.sin(TILT) + w * Math.cos(TILT);
  const pt = (px: number, py: number) => world(px, 0.8 + yy(ly + py, lz), -0.25 + zz(ly + py, lz));
  const hw = SCREEN_W / 2;
  const hh = SCREEN_H / 2;
  const n = world(0, Math.sin(TILT), Math.cos(TILT));
  const o = world(0, 0, 0);
  return {
    center: pt(0, 0),
    normal: [n[0] - o[0], n[1] - o[1] + 0, n[2] - o[2]],
    corners: [pt(-hw, hh), pt(hw, hh), pt(hw, -hh), pt(-hw, -hh)],
  };
}

/** Camera position and target that frame the screen with a margin, for a vertical fov (deg). */
export function flyTarget(f: ScreenFrame, fovDeg: number, aspect: number): { pos: V3; target: V3 } {
  const t = Math.tan((fovDeg * Math.PI) / 360);
  const d = Math.max((SCREEN_H * 1.3) / (2 * t), (SCREEN_W * 1.3) / (2 * t * aspect));
  return {
    pos: [f.center[0] + f.normal[0] * d, f.center[1] + f.normal[1] * d, f.center[2] + f.normal[2] * d],
    target: f.center,
  };
}

export const easeInOut = (t: number) => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2);

export const HOME_ELEVATION = (25 * Math.PI) / 180;
const DESK_W = 2.2;
export const DESK_EYE = 0.9; // the camera sits relative to desk height
/** The camera aims this far above the centroid, which seats the desks lower in the frame. */
export const LOOK_LIFT = 1.7;

/** The overview: the camera looks over the desks' centroid (x, z), 25° up, with the desks filling ~75% of the width. */
export function homeView(
  places: { x: number; z: number }[],
  fovDeg: number,
  aspect: number,
  fill = 0.75,
): { pos: V3; target: V3 } {
  const n = Math.max(1, places.length);
  const cx = places.reduce((s, p) => s + p.x, 0) / n;
  const cz = places.reduce((s, p) => s + p.z, 0) / n;
  const xs = places.map((p) => p.x);
  const span = (xs.length ? Math.max(...xs) - Math.min(...xs) : 0) + DESK_W;
  const t = Math.tan((fovDeg * Math.PI) / 360);
  const d = span / fill / (2 * t * aspect);
  return {
    target: [cx, DESK_EYE + LOOK_LIFT, cz],
    pos: [cx, DESK_EYE + Math.sin(HOME_ELEVATION) * d, cz + Math.cos(HOME_ELEVATION) * d],
  };
}
