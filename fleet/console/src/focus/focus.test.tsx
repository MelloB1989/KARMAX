import { describe, expect, it, vi } from "vitest";
import { rotate } from "../api";
import { renderToStaticMarkup } from "react-dom/server";
import { ControlBar } from "./ControlBar";
import { controlReducer, type ControlState } from "./controlState";
import { HoldTracker, ROTATE_HOLD_MS } from "./hold";
import { rotateText } from "./rotate";

const step = (s: ControlState, f: Parameters<typeof controlReducer>[1]) => controlReducer(s, f);

describe("focus and control", () => {
  it("control bar appears only after the socket says hello in control mode", () => {
    let s: ControlState = step({ phase: "idle" }, { type: "request" });
    expect(renderToStaticMarkup(<ControlBar state={s} />)).toBe("");
    s = step(s, { type: "frame", frame: { type: "hello", mode: "watch" } });
    expect(renderToStaticMarkup(<ControlBar state={s} />)).toBe("");
    s = step(s, { type: "frame", frame: { type: "hello", mode: "control" } });
    expect(renderToStaticMarkup(<ControlBar state={s} />)).toContain("YOU HAVE THE KEYBOARD");
    s = step(s, { type: "frame", frame: { type: "released" } });
    expect(renderToStaticMarkup(<ControlBar state={s} />)).toBe("");
  });

  it("409 shows the holder", () => {
    let s: ControlState = step({ phase: "idle" }, { type: "request" });
    s = step(s, { type: "frame", frame: { type: "error", holder: "operator-laptop", error: "held" } });
    s = step(s, { type: "frame", frame: { type: "released" } }); // the refusal's close keeps the reason
    expect(renderToStaticMarkup(<ControlBar state={s} />)).toContain("held by operator-laptop");
  });

  it("rotate needs a full 1 s hold", () => {
    const done = vi.fn();
    const h = new HoldTracker(ROTATE_HOLD_MS, done);
    h.down(0);
    expect(h.tick(999)).toBeLessThan(1);
    expect(done).not.toHaveBeenCalled();
    h.up(); // released early: nothing fires, and the next press starts over
    h.down(2000);
    h.tick(2999);
    expect(done).not.toHaveBeenCalled();
    expect(h.tick(3000)).toBe(1);
    h.tick(3500);
    expect(done).toHaveBeenCalledTimes(1);
  });

  it("rotateText says what fleetd will do", () => {
    expect(rotateText({ ok: true, closes: "t-1", when: "next idle tick" })).toBe(
      "closes task t-1 as rotated; takes effect at next idle tick",
    );
    expect(rotateText({ ok: true, closes: "", when: "next idle tick" })).toContain("no open task");
  });

  it("rotate previews with dry_run and sends it only when asked", async () => {
    const bodies: string[] = [];
    vi.stubGlobal("fetch", async (_u: string, init: { body: string }) => {
      bodies.push(init.body);
      return { ok: true, json: async () => ({ ok: true, closes: "T1", when: "next idle tick" }) };
    });
    await rotate("agent-01", true);
    await rotate("agent-01");
    vi.unstubAllGlobals();
    expect(bodies).toEqual([`{"agent":"agent-01","dry_run":true}`, `{"agent":"agent-01"}`]);
  });
});
