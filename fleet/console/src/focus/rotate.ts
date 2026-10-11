import type { RotateReply } from "../api";

/** fleetd's reply, as the sentence the operator reads. */
export function rotateText(r: RotateReply): string {
  const when = r.when ?? "the next idle tick";
  return r.closes ? `closes task ${r.closes} as rotated; takes effect at ${when}` : `no open task; takes effect at ${when}`;
}

/**
 * Decides when a held R may fire: the hold has completed AND the dry-run
 * preview for the same agent has come back ok, while R is still down.
 * Anything that changes the picture (release, another agent) cancels it.
 */
export class RotateGate {
  private press = 0;
  private agent = "";
  private down_ = false;
  private completed = false;
  private previewOk = false;

  constructor(private readonly fire: (agent: string) => void) {}

  /** R went down on `agent`; returns the press id the preview reply must quote. */
  down(agent: string): number {
    this.agent = agent;
    this.down_ = true;
    this.completed = false;
    this.previewOk = false;
    return ++this.press;
  }

  previewDone(press: number, ok: boolean): void {
    if (press !== this.press) return;
    this.previewOk = ok;
    this.maybe();
  }

  currentPress(): number {
    return this.press;
  }

  complete(): void {
    this.completed = true;
    this.maybe();
  }

  cancel(): void {
    this.press++;
    this.down_ = false;
    this.completed = false;
    this.previewOk = false;
  }

  private maybe(): void {
    if (!this.down_ || !this.completed || !this.previewOk) return;
    this.down_ = false;
    this.fire(this.agent);
  }
}
