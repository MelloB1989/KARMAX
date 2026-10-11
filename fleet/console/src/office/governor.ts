// A rolling-fps governor: if the 2 s average falls below the floor, bloom turns
// off and stays off until a reload.
export interface Governor {
  /** Call once per rendered frame with a monotonic time in ms. */
  frame(nowMs: number): void;
  /** The measured fps over the rolling window; 0 until two frames are seen. */
  fps(): number;
  bloom(): boolean;
}

export function createGovernor(opts: { floorFps?: number; windowMs?: number } = {}): Governor {
  const floor = opts.floorFps ?? 40;
  const window = opts.windowMs ?? 2000;
  const stamps: number[] = [];
  let first = -1;
  let on = true;
  let fps = 0;
  return {
    frame(now) {
      if (first < 0) first = now;
      stamps.push(now);
      while (stamps.length > 1 && now - stamps[0] > window) stamps.shift();
      const span = now - stamps[0];
      fps = span > 0 ? ((stamps.length - 1) * 1000) / span : 0;
      if (on && now - first >= window && fps < floor) on = false;
    },
    fps: () => fps,
    bloom: () => on,
  };
}
