import { beforeEach, describe, expect, it, vi } from "vitest";

const captures = import.meta.glob("../../dev/term-*.ans", { query: "?raw", import: "default", eager: true }) as Record<string, string>;
const feeds = new Map<string, (b: Uint8Array) => void>();
vi.mock("../term/stream", () => ({
  subscribeTerm: (agent: string, onData: (b: Uint8Array) => void) => {
    feeds.set(agent, onData);
    return () => feeds.delete(agent);
  },
}));

import { createSurface } from "./surface";

const texts: string[] = [];
const ctx = {
  fillStyle: "", font: "", textBaseline: "",
  strokeStyle: "",
  fillRect: vi.fn(),
  beginPath: vi.fn(),
  moveTo: vi.fn(),
  lineTo: vi.fn(),
  stroke: vi.fn(),
  fillText: vi.fn((t: string) => texts.push(t)),
};
beforeEach(() => {
  texts.length = 0;
  feeds.clear();
  HTMLCanvasElement.prototype.getContext = vi.fn(() => ctx) as never;
});

const settle = () => new Promise((r) => setTimeout(r, 30));

describe("desk surfaces", () => {
  it("sizes the canvas before the texture exists and never swaps the texture's image", async () => {
    const s = createSurface(220, 50);
    const image = s.texture.image;
    expect(image).toBe(s.canvas);
    expect([s.canvas.width, s.canvas.height]).toEqual([1760, 800]);
    const detach = s.attachTerminal("agent-01");
    expect(s.texture.image).toBe(image);
    expect(s.texture.image.width).toBe(1760);
    detach();
  });

  it("every desk paints its first frame, and its captured text", async () => {
    for (const agent of ["agent-01", "agent-02", "agent-03", "agent-04"]) {
      const s = createSurface(220, 50);
      const v0 = s.texture.version;
      const detach = s.attachTerminal(agent);
      expect(s.texture.version, `${agent} first frame uploaded`).toBeGreaterThan(v0);

      const raw = captures[`../../dev/term-${agent}.ans`].replace(/\n/g, "\r\n");
      feeds.get(agent)!(new TextEncoder().encode(raw));
      await settle();
      texts.length = 0;
      const v1 = s.texture.version;
      s.tick(1000);
      expect(s.texture.version, `${agent} repainted after data`).toBeGreaterThan(v1);
      expect(texts.filter((t) => t.trim()).length, `${agent} text runs`).toBeGreaterThan(5);
      detach();
    }
  });

  it("the orchestrator's screen draws on the first tick", () => {
    const s = createSurface(220, 50);
    const v0 = s.texture.version;
    s.attachOrchestrator(() => ({ info: null, tasks: [] }));
    s.tick(1000);
    expect(s.texture.version).toBeGreaterThan(v0);
  });
});
