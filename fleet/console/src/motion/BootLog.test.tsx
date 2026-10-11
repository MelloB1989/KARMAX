import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import fixture from "../../dev/snapshot.json";
import { SOURCES, type Snapshot } from "../model";
import { BootLog } from "./BootLog";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const base = fixture as unknown as Snapshot;
let host: HTMLDivElement;
let root: Root;

beforeEach(() => {
  vi.useFakeTimers();
  // reduced motion, so removal is immediate and the test sees only the timer logic
  vi.stubGlobal("matchMedia", (q: string) => ({
    matches: q.includes("prefers-reduced-motion"), media: q, addEventListener() {}, removeEventListener() {}, addListener() {}, removeListener() {},
  }));
  host = document.createElement("div");
  document.body.append(host);
  root = createRoot(host);
});
afterEach(() => {
  act(() => root.unmount());
  host.remove();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

// in small steps, so each 200 ms tick re-renders before the next, as in a browser
const advance = (ms: number) => {
  for (let t = 0; t < ms; t += 50) act(() => void vi.advanceTimersByTime(50));
};

describe("BootLog", () => {
  it("is gone after 1 s when every source answered ok", () => {
    act(() => root.render(<BootLog snapshot={base} />));
    expect(host.querySelector(".boot")).not.toBeNull();
    advance(1000);
    expect(host.querySelector(".boot")).toBeNull();
  });

  it("with one source missing, shows it as no answer and is gone after 6.5 s", () => {
    const sources = { ...base.sources };
    delete sources[SOURCES[0]];
    act(() => root.render(<BootLog snapshot={{ ...base, sources }} />));
    advance(1000);
    expect(host.querySelector(".boot")).not.toBeNull();
    advance(5100);
    expect(host.querySelector(".boot-failed")?.textContent).toContain("no answer");
    advance(600);
    expect(host.querySelector(".boot")).toBeNull();
  });

  it("stays gone-bound when a source blips not-ok after everything was ok", () => {
    const blip = (ok: boolean): Snapshot => ({ ...base, sources: { ...base.sources, [SOURCES[0]]: { ok, at: base.sources[SOURCES[0]]?.at } } });
    act(() => root.render(<BootLog snapshot={base} />));
    advance(300);
    act(() => root.render(<BootLog snapshot={blip(false)} />));
    advance(100);
    act(() => root.render(<BootLog snapshot={base} />));
    advance(300);
    act(() => root.render(<BootLog snapshot={blip(false)} />));
    advance(100);
    act(() => root.render(<BootLog snapshot={base} />));
    advance(200); // 1 s in: the fade timer was never reset
    expect(host.querySelector(".boot")).toBeNull();
  });
});
