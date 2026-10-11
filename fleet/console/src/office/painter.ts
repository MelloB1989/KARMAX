// Paints an agent's terminal into a 2D canvas for use as a desk-screen texture.
// The terminal is parsed by @xterm/headless (no renderer, no WebGL context).
import { Terminal } from "@xterm/headless";
import type { IBufferCell } from "@xterm/headless";
import { subscribeTerm } from "../term/stream";

export const CELL_W = 8;
export const CELL_H = 16;
const BG = "#0A1018";
const FG = "#D6E2EC";
const CURSOR = "#FFB547";

const ANSI16 = [
  "#000000", "#cd3131", "#0dbc79", "#e5e510", "#2472c8", "#bc3fbc", "#11a8cd", "#e5e5e5",
  "#666666", "#f14c4c", "#23d18b", "#f5f543", "#3b8eea", "#d670d6", "#29b8db", "#ffffff",
];

const hex = (n: number) => n.toString(16).padStart(2, "0");
const rgb = (n: number) => `#${hex((n >> 16) & 255)}${hex((n >> 8) & 255)}${hex(n & 255)}`;

function palette(i: number): string {
  if (i < 16) return ANSI16[i];
  if (i < 232) {
    const c = i - 16;
    const lv = (v: number) => (v === 0 ? 0 : 55 + v * 40);
    return `#${hex(lv(Math.floor(c / 36)))}${hex(lv(Math.floor(c / 6) % 6))}${hex(lv(c % 6))}`;
  }
  const g = 8 + (i - 232) * 10;
  return `#${hex(g)}${hex(g)}${hex(g)}`;
}

function fgOf(c: IBufferCell): string | null {
  if (c.isFgRGB()) return rgb(c.getFgColor());
  if (c.isFgPalette()) return palette(c.getFgColor());
  return null;
}

function bgOf(c: IBufferCell): string | null {
  if (c.isBgRGB()) return rgb(c.getBgColor());
  if (c.isBgPalette()) return palette(c.getBgColor());
  return null;
}

export interface DeskScreen {
  canvas: HTMLCanvasElement | OffscreenCanvas;
  /** True when the buffer changed since the last paint(). */
  dirty: () => boolean;
  /** Draws the active buffer and clears dirty. */
  paint: () => void;
  dispose(): void;
}

/**
 * Paints into `into` when given, so a texture built on that canvas never has its image swapped
 * (a WebGL texture's size is fixed at its first upload).
 */
export function createDeskScreen(
  agent: string,
  cols: number,
  rows: number,
  into?: HTMLCanvasElement | OffscreenCanvas,
): DeskScreen {
  const canvas: HTMLCanvasElement | OffscreenCanvas =
    into ?? (typeof document !== "undefined" ? document.createElement("canvas") : new OffscreenCanvas(1, 1));
  canvas.width = cols * CELL_W;
  canvas.height = rows * CELL_H;
  const ctx = canvas.getContext("2d") as CanvasRenderingContext2D | null;

  const term = new Terminal({ cols, rows, allowProposedApi: true, scrollback: 0 });
  let changed = false;
  let disposed = false;
  const unsub = subscribeTerm(agent, (b) => {
    // @xterm/headless 5.5.0 has no onWriteParsed; the write callback is its equivalent.
    if (!disposed) term.write(b, () => (changed = true));
  });

  const cell = term.buffer.active.getNullCell();

  function paint(): void {
    changed = false;
    if (!ctx) return;
    const buf = term.buffer.active;
    ctx.fillStyle = BG;
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    ctx.textBaseline = "top";
    let font = "";
    const setFont = (bold: boolean) => {
      const f = `${bold ? "bold " : ""}13px "IBM Plex Mono", monospace`;
      if (f !== font) ctx.font = font = f;
    };
    for (let y = 0; y < rows; y++) {
      const line = buf.getLine(buf.viewportY + y);
      if (!line) continue;
      // a run is consecutive cells of one fg and weight, drawn with a single fillText
      let run = "";
      let runX = 0;
      let runFg = "";
      let runBold = false;
      const flush = () => {
        const text = run.trimEnd();
        if (text) {
          ctx.fillStyle = runFg;
          setFont(runBold);
          ctx.fillText(text, runX * CELL_W, y * CELL_H + 1);
        }
        run = "";
      };
      for (let x = 0; x < cols; x++) {
        const c = line.getCell(x, cell);
        if (!c) continue;
        const w = c.getWidth();
        if (w === 0) continue; // the right half of a wide character
        let fg = fgOf(c);
        let bg = bgOf(c);
        if (c.isInverse()) [fg, bg] = [bg ?? BG, fg ?? FG];
        if (bg) {
          ctx.fillStyle = bg;
          ctx.fillRect(x * CELL_W, y * CELL_H, CELL_W * Math.max(1, w), CELL_H);
        }
        const ch = c.getChars();
        const visible = !!ch && ch !== " " && !c.isInvisible();
        const style = fg ?? FG;
        const bold = !!c.isBold();
        if (!visible) {
          // a gap keeps the run going; the next glyph decides whether it continues
          if (run) run += " ";
          continue;
        }
        if (run && (style !== runFg || bold !== runBold || w > 1)) flush();
        if (!run) {
          runX = x;
          runFg = style;
          runBold = bold;
        }
        run += ch;
        if (w > 1) flush();
      }
      flush();
    }
    ctx.fillStyle = CURSOR;
    ctx.fillRect(buf.cursorX * CELL_W, buf.cursorY * CELL_H + CELL_H - 2, CELL_W, 2);
  }

  return {
    canvas,
    dirty: () => changed,
    paint,
    dispose() {
      disposed = true;
      unsub();
      term.dispose();
    },
  };
}
