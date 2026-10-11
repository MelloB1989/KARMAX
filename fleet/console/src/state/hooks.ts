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

/** "ANGLE (Intel, Mesa Intel(R) HD Graphics 530 (SKL GT2), OpenGL 4.6)" → "Intel HD Graphics 530". */
export function gpuName(full: string): string {
  const soft = /swiftshader|llvmpipe|softpipe/i.exec(full);
  if (soft) return `${soft[0].replace(/^./, (c) => c.toUpperCase())} (software)`; // the one a person must notice
  const m = /^ANGLE \((.*)\)$/.exec(full.trim());
  const parts = (m ? m[1] : full).split(/,\s*/);
  let r = parts.length > 1 ? parts[1] : parts[0];
  r = r.replace(/\((R|TM)\)/gi, "").replace(/^Mesa\s+/i, "").replace(/\s+\(.*$/, "").replace(/\s+/g, " ").trim();
  const vendor = (parts.length > 1 ? parts[0] : "").trim();
  if (vendor && !r.toLowerCase().includes(vendor.toLowerCase())) r = `${vendor} ${r}`;
  return r || full;
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
