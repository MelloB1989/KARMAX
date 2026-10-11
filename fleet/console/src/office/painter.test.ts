import { beforeEach, describe, expect, it, vi } from "vitest";

let feed: (b: Uint8Array) => void = () => {};
vi.mock("../term/stream", () => ({
  subscribeTerm: (_agent: string, onData: (b: Uint8Array) => void) => {
    feed = onData;
    return () => {};
  },
}));

import { createDeskScreen } from "./painter";

const calls: { text: string; x: number; y: number; fill: string }[] = [];
const ctx = {
  fillStyle: "",
  font: "",
  textBaseline: "",
  fillRect: vi.fn(),
  fillText: vi.fn(function (this: { fillStyle: string }, text: string, x: number, y: number) {
    calls.push({ text, x, y, fill: this.fillStyle });
  }),
};

beforeEach(() => {
  calls.length = 0;
  ctx.fillText.mockClear();
  HTMLCanvasElement.prototype.getContext = vi.fn(() => ctx) as never;
});

const write = async (s: string) => {
  feed(new TextEncoder().encode(s));
  await new Promise((r) => setTimeout(r, 20));
};

describe("desk painter", () => {
  it("paints a written line's characters at their cells", async () => {
    const s = createDeskScreen("agent-01", 20, 5);
    await write("hi\r\nyo");
    s.paint();
    const at = (t: string, cx: number, cy: number) =>
      calls.some((c) => c.text === t && c.x === cx * 8 && c.y === cy * 16 + 1);
    expect(at("h", 0, 0)).toBe(true);
    expect(at("i", 1, 0)).toBe(true);
    expect(at("y", 0, 1)).toBe(true);
    expect(at("o", 1, 1)).toBe(true);
    s.dispose();
  });

  it("marks dirty only after new data", async () => {
    const s = createDeskScreen("agent-01", 20, 5);
    expect(s.dirty()).toBe(false);
    await write("x");
    expect(s.dirty()).toBe(true);
    s.paint();
    expect(s.dirty()).toBe(false);
    s.dispose();
  });

  it("uses truecolour fg from SGR 38;2", async () => {
    const s = createDeskScreen("agent-01", 20, 5);
    await write("\x1b[38;2;255;0;128mZ\x1b[0m");
    s.paint();
    const z = calls.find((c) => c.text === "Z");
    expect(z?.fill).toBe("#ff0080");
    s.dispose();
  });
});
