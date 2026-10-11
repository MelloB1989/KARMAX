import type { RotateReply } from "../api";

/** fleetd's reply, as the sentence the operator reads. */
export function rotateText(r: RotateReply): string {
  const when = r.when ?? "the next idle tick";
  return r.closes ? `closes task ${r.closes} as rotated; takes effect at ${when}` : `no open task; takes effect at ${when}`;
}
