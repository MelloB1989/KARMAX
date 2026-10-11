// A desk's screen as a texture. The canvas and texture are built together at the right size and the
// texture's image is never swapped: WebGL fixes a texture's size at its first upload, so a texture
// created on a default 300x150 canvas and re-pointed at the real one later renders blank.
import { CanvasTexture, LinearFilter, SRGBColorSpace } from "three";
import type { Orchestrator, Task } from "../model";
import { createDeskScreen, CELL_H, CELL_W } from "./painter";
import { drawOrchestratorScreen } from "./orchestratorScreen";

export const FRAME_MS = 1000 / 30;

export interface Surface {
  canvas: HTMLCanvasElement;
  texture: CanvasTexture;
  /** Repaints and flags the texture for upload when there is something new; call once per frame. */
  tick(now: number): void;
  /** Streams an agent's terminal into the screen; returns the detach. */
  attachTerminal(agent: string): () => void;
  /** Draws the orchestrator's quota and tasks; `get` is read on each tick. Returns the detach. */
  attachOrchestrator(get: () => { info: Orchestrator | null; tasks: Task[] }): () => void;
  dispose(): void;
}

export function createSurface(cols: number, rows: number): Surface {
  const canvas = document.createElement("canvas");
  canvas.width = cols * CELL_W;
  canvas.height = rows * CELL_H;
  const texture = new CanvasTexture(canvas);
  texture.colorSpace = SRGBColorSpace;
  // 30 uploads a second per desk: no mip chain to rebuild each time
  texture.generateMipmaps = false;
  texture.minFilter = LinearFilter;
  let step: (now: number) => void = () => {};

  return {
    canvas,
    texture,
    tick: (now) => step(now),
    attachTerminal(agent) {
      const screen = createDeskScreen(agent, cols, rows, canvas);
      screen.paint(); // the first frame, before any data
      texture.needsUpdate = true;
      let at = -Infinity;
      step = (now) => {
        if (now - at < FRAME_MS || !screen.dirty()) return;
        at = now;
        screen.paint();
        texture.needsUpdate = true;
      };
      return () => {
        step = () => {};
        screen.dispose();
      };
    },
    attachOrchestrator(get) {
      const ctx = canvas.getContext("2d");
      let last = "";
      let at = -Infinity;
      step = (now) => {
        if (!ctx || now - at < 500) return;
        at = now;
        const o = get();
        const key = JSON.stringify([o.info, o.tasks]);
        if (key === last) return;
        last = key;
        drawOrchestratorScreen(ctx, canvas.width, canvas.height, o.info, o.tasks);
        texture.needsUpdate = true;
      };
      return () => {
        step = () => {};
      };
    },
    dispose: () => texture.dispose(),
  };
}
