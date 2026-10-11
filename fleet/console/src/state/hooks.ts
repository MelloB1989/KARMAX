import { useEffect, useState } from "react";

// Development only: a fixture is a recording, so its clock stops at the
// snapshot's own time. In a production build DEV is false and this is dead code.
let frozenAt: number | null = null;
export function freezeClock(ms: number): void {
  if (import.meta.env.DEV && import.meta.env.VITE_FIXTURE) frozenAt = ms;
}
const nowMs = (): number => frozenAt ?? Date.now();

/** Wall-clock ms, re-rendering every `everyMs`. */
export function useNow(everyMs = 1000): number {
  const [now, setNow] = useState(nowMs);
  useEffect(() => {
    const id = setInterval(() => setNow(nowMs()), everyMs);
    return () => clearInterval(id);
  }, [everyMs]);
  return now;
}

/** The browser's GPU, so a software renderer is visible at a glance. */
export function gpuString(): string {
  try {
    const gl = document.createElement("canvas").getContext("webgl");
    if (!gl) return "no WebGL";
    const ext = gl.getExtension("WEBGL_debug_renderer_info");
    return String(ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER));
  } catch {
    return "no WebGL";
  }
}
