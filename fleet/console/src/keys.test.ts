import { beforeEach, describe, expect, it, vi } from "vitest";
import { bindForTest, dispatch, resetKeysForTest, setControlHeld } from "./keys";

const key = (k: string, mods: Partial<KeyboardEventInit> = {}) =>
  new KeyboardEvent("keydown", { key: k, cancelable: true, ...mods });

beforeEach(resetKeysForTest);

describe("keys", () => {
  it("while control is held, 1, R, O, Esc, T reach the terminal handler and no panel handler fires", () => {
    const panel = vi.fn();
    for (const k of ["1", "r", "o", "escape", "t"]) bindForTest(k, panel);
    setControlHeld(true, () => {});
    for (const k of ["1", "r", "o", "Escape", "t"]) expect(dispatch(key(k))).toBe(false);
    expect(panel).not.toHaveBeenCalled();
  });

  it("Ctrl+] releases and calls onRelease once", () => {
    const release = vi.fn();
    setControlHeld(true, release);
    expect(dispatch(key("]", { ctrlKey: true }))).toBe(true);
    dispatch(key("]", { ctrlKey: true }));
    expect(release).toHaveBeenCalledTimes(1);
  });

  it("outside control, 1-9 select, F focuses, Enter requests control", () => {
    const h = { one: vi.fn(), nine: vi.fn(), f: vi.fn(), enter: vi.fn() };
    bindForTest("1", h.one);
    bindForTest("9", h.nine);
    bindForTest("f", h.f);
    bindForTest("enter", h.enter);
    for (const k of ["1", "9", "f", "Enter"]) expect(dispatch(key(k))).toBe(true);
    for (const fn of Object.values(h)) expect(fn).toHaveBeenCalledOnce();
  });

  it("keys typed into an input belong to the input", () => {
    const f = vi.fn();
    bindForTest("f", f);
    const input = document.createElement("input");
    expect(dispatch(Object.defineProperty(key("f"), "target", { value: input }))).toBe(false);
    expect(f).not.toHaveBeenCalled();
  });

  it("xterm's helper textarea does not swallow panel keys in watch mode, but a real textarea does", () => {
    const f = vi.fn();
    bindForTest("f", f);
    const helper = document.createElement("textarea");
    helper.className = "xterm-helper-textarea";
    expect(dispatch(Object.defineProperty(key("f"), "target", { value: helper }))).toBe(true);
    expect(f).toHaveBeenCalledOnce();
    const real = document.createElement("textarea");
    expect(dispatch(Object.defineProperty(key("f"), "target", { value: real }))).toBe(false);
    // held: the terminal has every key, helper textarea included
    setControlHeld(true, () => {});
    expect(dispatch(Object.defineProperty(key("f"), "target", { value: helper }))).toBe(false);
    expect(f).toHaveBeenCalledOnce();
  });
});
