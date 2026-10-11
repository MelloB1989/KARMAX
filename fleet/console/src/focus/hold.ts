// A key that must be held for a full duration before it counts.
export class HoldTracker {
  private since: number | null = null;
  private done = false;

  constructor(
    private readonly durationMs: number,
    private readonly onComplete: () => void,
  ) {}

  down(now: number): void {
    if (this.since === null) {
      this.since = now;
      this.done = false;
    }
  }

  up(): void {
    this.since = null;
    this.done = false;
  }

  /** 0..1; fires onComplete once, when the full duration has passed. */
  tick(now: number): number {
    if (this.since === null) return 0;
    const p = Math.min(1, (now - this.since) / this.durationMs);
    if (p >= 1 && !this.done) {
      this.done = true;
      this.onComplete();
    }
    return p;
  }
}

export const ROTATE_HOLD_MS = 1000;
