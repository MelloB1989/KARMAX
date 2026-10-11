// The panel's key map. While the keyboard is held for an agent, every key
// belongs to the agent except Ctrl+], which gives it back (tmux binds the
// same key to detach, so it works even if this handler never ran).
import { useEffect, useRef } from "react";

type Handler = (e: KeyboardEvent) => void;

interface Binding {
  combo: string;
  handler: { current: Handler };
}

const bindings = new Set<Binding>();
let held = false;
let release: (() => void) | null = null;

/** "ctrl+]", "enter", "1", "shift+r" — lowercased, modifiers first. */
export function comboOf(e: KeyboardEvent): string {
  const parts: string[] = [];
  if (e.ctrlKey) parts.push("ctrl");
  if (e.altKey) parts.push("alt");
  if (e.metaKey) parts.push("meta");
  if (e.shiftKey && e.key.length > 1) parts.push("shift");
  parts.push(e.key.toLowerCase());
  return parts.join("+");
}

/**
 * Whether the key is going into a text field. xterm's helper textarea does
 * not count: clicking a watched terminal focuses it, and it only matters
 * while control is held (when dispatch has already stepped aside).
 */
export function isTyping(e: KeyboardEvent): boolean {
  const t = e.target as HTMLElement | null;
  if (!t || (t.tagName !== "INPUT" && t.tagName !== "TEXTAREA")) return false;
  return !t.classList?.contains("xterm-helper-textarea");
}

export function isControlHeld(): boolean {
  return held;
}

/** Holds the keyboard for an agent; onRelease runs once, on Ctrl+]. */
export function setControlHeld(h: boolean, onRelease?: () => void): void {
  held = h;
  release = h ? (onRelease ?? null) : null;
}

/** Dispatches one key event; exported for tests. Returns true when handled. */
export function dispatch(e: KeyboardEvent): boolean {
  const combo = comboOf(e);
  if (held) {
    if (combo === "ctrl+]") {
      const r = release;
      setControlHeld(false);
      r?.();
      e.preventDefault();
      return true;
    }
    return false; // the terminal has it
  }
  if (isTyping(e)) return false;
  let hit = false;
  for (const b of bindings) {
    if (b.combo === combo) {
      b.handler.current(e);
      hit = true;
    }
  }
  if (hit) e.preventDefault();
  return hit;
}

let installed = false;
function install(): void {
  if (installed || typeof window === "undefined") return;
  installed = true;
  window.addEventListener("keydown", (e) => dispatch(e), true);
}

/** Binds a key for the lifetime of the calling component. */
export function useKey(combo: string, handler: Handler): void {
  const ref = useRef(handler);
  ref.current = handler;
  useEffect(() => {
    install();
    const b: Binding = { combo: combo.toLowerCase(), handler: ref };
    bindings.add(b);
    return () => {
      bindings.delete(b);
    };
  }, [combo]);
}

/** Tests reset between cases. */
export function resetKeysForTest(): void {
  bindings.clear();
  setControlHeld(false);
}

/** Tests bind without React. */
export function bindForTest(combo: string, handler: Handler): void {
  bindings.add({ combo, handler: { current: handler } });
}
