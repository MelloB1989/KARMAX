import { describe, expect, it } from "vitest";
import { ageOf, fmtAge, isStale, palette, phaseLook } from "./derive";

const now = Date.parse("2026-10-11T12:00:00Z");
const ago = (s: number) => new Date(now - s * 1000).toISOString();

describe("phaseLook", () => {
  it("follows the spec table row for row", () => {
    expect(phaseLook("working", [])).toEqual({ color: palette.amber, label: "working", pulse: false });
    expect(phaseLook("waiting", [])).toEqual({ color: palette.cyan, label: "task open, idle", pulse: true });
    expect(phaseLook("standby", null)).toMatchObject({ color: palette.dim, pulse: false });
    expect(phaseLook("cooling", [])).toMatchObject({ color: palette.frost, label: "cooling" });
    expect(phaseLook("disabled", [])).toMatchObject({ color: palette.grey, label: "disabled" });
    expect(phaseLook("down", [])).toMatchObject({ color: palette.alert, label: "down" });
  });
  it("a stuck alert overrides the phase with a red banner look", () => {
    expect(phaseLook("working", ["stuck"])).toMatchObject({ color: palette.alert, label: "stuck" });
  });
  it("an unknown phase is shown as itself, dim", () => {
    expect(phaseLook("novel", [])).toMatchObject({ color: palette.dim, label: "novel" });
  });
});

describe("isStale", () => {
  it("is stale at exactly 2x cadence, fresh just under", () => {
    expect(isStale({ ok: true, at: ago(10) }, 5, now)).toBe(true);
    expect(isStale({ ok: true, at: ago(9.9) }, 5, now)).toBe(false);
  });
  it("is stale when not ok or never reported", () => {
    expect(isStale({ ok: false, at: ago(0) }, 5, now)).toBe(true);
    expect(isStale({ ok: true }, 5, now)).toBe(true);
  });
});

describe("age", () => {
  it("ageOf is seconds, never negative, Infinity when unparseable", () => {
    expect(ageOf(ago(40), now)).toBe(40);
    expect(ageOf(ago(-5), now)).toBe(0);
    expect(ageOf("", now)).toBe(Infinity);
  });
  it("fmtAge", () => {
    expect(fmtAge(40)).toBe("40 s");
    expect(fmtAge(125)).toBe("2 m");
    expect(fmtAge(6 * 3600 + 11 * 60)).toBe("6 h 11 m");
    expect(fmtAge(2 * 86400 + 3 * 3600)).toBe("2 d 3 h");
    expect(fmtAge(Infinity)).toBe("—");
  });
});
