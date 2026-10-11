import { afterEach, describe, expect, it, vi } from "vitest";
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { setControlHeld, resetKeysForTest } from "../keys";
import { buildReducer, initialBuild, setReloader, shouldReload, useAutoReload } from "./autoreload";

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

afterEach(resetKeysForTest);

describe("build id reducer", () => {
  it("remembers the first id and goes stale only when a different one arrives", () => {
    let s = buildReducer(initialBuild, undefined);
    expect(shouldReload(s, false)).toBe(false); // an old console sends none
    s = buildReducer(s, "v1-aaa");
    s = buildReducer(s, "v1-aaa");
    expect(shouldReload(s, false)).toBe(false);
    s = buildReducer(s, "v1-bbb");
    expect(shouldReload(s, false)).toBe(true);
    expect(shouldReload(s, true)).toBe(false); // keyboard held
  });
});

describe("useAutoReload", () => {
  const mount = () => {
    const root = createRoot(document.createElement("div"));
    const C = ({ b }: { b?: string }) => {
      useAutoReload(b);
      return null;
    };
    return { root, render: (b?: string) => act(() => root.render(createElement(C, { b }))) };
  };

  it("reloads when the build changes", () => {
    const r = vi.fn();
    setReloader(r);
    const m = mount();
    m.render("a");
    m.render("a");
    expect(r).not.toHaveBeenCalled();
    m.render("b");
    expect(r).toHaveBeenCalledOnce();
    act(() => m.root.unmount());
  });

  it("waits while the keyboard is held, then reloads on release", () => {
    const r = vi.fn();
    setReloader(r);
    const m = mount();
    m.render("a");
    act(() => setControlHeld(true, () => {}));
    m.render("b");
    expect(r).not.toHaveBeenCalled();
    act(() => setControlHeld(false));
    expect(r).toHaveBeenCalledOnce();
    act(() => m.root.unmount());
  });
});
