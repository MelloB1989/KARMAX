// The orchestrator's desk screen: its quota and the open tasks, drawn with 2D canvas calls.
import type { Orchestrator, Task } from "../model";

const BG = "#0A1018";
const LINE = "#1B2A38";
const TEXT = "#D6E2EC";
const DIM = "#7F93A6";
const AMBER = "#FFB547";
const CYAN = "#5CE1E6";
const ALERT = "#FF5D5D";

type Ctx = CanvasRenderingContext2D | OffscreenCanvasRenderingContext2D;

function bar(ctx: Ctx, x: number, y: number, w: number, label: string, pct: number): void {
  ctx.fillStyle = DIM;
  ctx.font = '28px "IBM Plex Mono", monospace';
  ctx.fillText(label, x, y);
  ctx.fillStyle = LINE;
  ctx.fillRect(x, y + 14, w, 26);
  ctx.fillStyle = pct >= 90 ? ALERT : pct >= 70 ? AMBER : CYAN;
  ctx.fillRect(x, y + 14, (w * Math.min(100, Math.max(0, pct))) / 100, 26);
  ctx.fillStyle = TEXT;
  ctx.fillText(`${Math.round(pct)}%`, x + w + 20, y + 38);
}

export function drawOrchestratorScreen(
  ctx: Ctx,
  w: number,
  h: number,
  orch: Orchestrator | null,
  tasks: Task[],
): void {
  ctx.fillStyle = BG;
  ctx.fillRect(0, 0, w, h);
  ctx.textBaseline = "alphabetic";
  ctx.fillStyle = AMBER;
  ctx.font = 'bold 56px "Barlow Condensed", sans-serif';
  ctx.fillText((orch?.name ?? "orchestrator").toUpperCase(), 48, 84);
  if (!orch) {
    ctx.fillStyle = DIM;
    ctx.font = '32px "IBM Plex Mono", monospace';
    ctx.fillText("no quota reading", 48, 150);
  } else {
    bar(ctx, 48, 160, 700, "5 h", orch.five_hour_pct);
    bar(ctx, 48, 250, 700, "7 d", orch.seven_day_pct);
  }
  ctx.strokeStyle = LINE;
  ctx.beginPath();
  ctx.moveTo(48, 330);
  ctx.lineTo(w - 48, 330);
  ctx.stroke();
  ctx.fillStyle = DIM;
  ctx.font = '28px "IBM Plex Mono", monospace';
  ctx.fillText(`OPEN TASKS  ${tasks.length}`, 48, 380);
  ctx.fillStyle = TEXT;
  tasks.slice(0, 8).forEach((t, i) => {
    ctx.fillText(`${t.agent.padEnd(9)} ${t.id}`, 48, 430 + i * 40);
  });
}
