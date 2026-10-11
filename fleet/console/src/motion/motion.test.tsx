import { describe, expect, it } from "vitest";
import fixture from "../../dev/snapshot.json";
import type { Snapshot } from "../model";
import { bannersFor } from "./banners";
import { BOOT_TIMEOUT_MS, bootDone, bootLines } from "./boot";
import { easeOut } from "./tween";

const base = fixture as unknown as Snapshot;
const withAgent = (patch: object): Snapshot => ({
  ...base,
  agents: (base.agents ?? []).map((a, i) => (i === 0 ? { ...a, ...patch } : { ...a, phase: "standby", alerts: [] })),
});

describe("motion", () => {
  it("banner clears when the alert clears", () => {
    expect(bannersFor(withAgent({ phase: "working", alerts: ["stuck"] })).map((b) => b.kind)).toEqual(["stuck"]);
    expect(bannersFor(withAgent({ phase: "working", alerts: [] }))).toEqual([]);
  });
  it("down, cooling and disabled each raise a banner and clear with the phase", () => {
    for (const phase of ["down", "cooling", "disabled"]) {
      expect(bannersFor(withAgent({ phase, alerts: [] })).map((b) => b.kind)).toEqual([phase]);
      expect(bannersFor(withAgent({ phase: "standby", alerts: [] }))).toEqual([]);
    }
  });
  it("boot lists unanswered sources, red after 6 s, done when all ok or timed out", () => {
    const some = { ...base, sources: { host: { ok: true, at: "2026-10-11T13:57:33Z" } } } as Snapshot;
    expect(bootDone(bootLines(some, 1000), 1000)).toBe(false);
    const late = bootLines(some, BOOT_TIMEOUT_MS);
    expect(late.filter((l) => l.state === "failed")).toHaveLength(6);
    expect(bootDone(late, BOOT_TIMEOUT_MS)).toBe(true);
    expect(bootDone(bootLines(base, 100), 100)).toBe(true);
  });
  it("tween easing runs 0 to 1", () => {
    expect(easeOut(0)).toBe(0);
    expect(easeOut(1)).toBe(1);
  });
});
