import { beforeEach, describe, expect, it, vi } from "vitest";
import { bindForTest, dispatch, resetKeysForTest, setControlHeld } from "./keys";

const key = (k: string, mods: Partial<KeyboardEventInit> = {}) =>
  new KeyboardEvent("keydown", { key: k, cancelable: true, ...mods });

beforeEach(resetKeysForTest);

describe("keys", () => {
  it("while control is held, panel keys do not fire", () => {
    const panel = vi.fn();
    for (const k of ["1", "r", "o", "escape", "t"]) bindForTest(k, panel);
    setControlHeld(true, () => {});
    for (const k of ["1", "r", "o", "Escape", "t"]) expect(dispatch(key(k))).toBe(false);
    expect(panel).not.toHaveBeenCalled();
  });

  it("Ctrl+] releases once", () => {
    const release = vi.fn();
    setControlHeld(true, release);
    expect(dispatch(key("]", { ctrlKey: true }))).toBe(true);
    dispatch(key("]", { ctrlKey: true }));
    expect(release).toHaveBeenCalledTimes(1);
  });

  it("outside control, bound keys fire", () => {
    const one = vi.fn();
    bindForTest("1", one);
    expect(dispatch(key("1"))).toBe(true);
    expect(one).toHaveBeenCalledOnce();
  });
});
