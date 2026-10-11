// The kiosk's Chrome keeps the bundle it loaded at boot. The console reports
// its build id in every snapshot; when it differs from the one this page
// loaded with, a newer console has been deployed and the page reloads itself,
// but never while the keyboard is held.
import { useEffect, useReducer } from "react";
import { isControlHeld, onControlHeldChange } from "../keys";

export interface BuildState {
  /** The build id this page first saw; null until a snapshot says. */
  loaded: string | null;
  /** The newest build id seen. */
  seen: string | null;
}

export const initialBuild: BuildState = { loaded: null, seen: null };

/** A snapshot's build id arrived (undefined from a console too old to send one: ignored). */
export function buildReducer(s: BuildState, build: string | undefined): BuildState {
  if (!build) return s;
  return { loaded: s.loaded ?? build, seen: build };
}

export const isStale = (s: BuildState): boolean => s.loaded !== null && s.seen !== s.loaded;

/** Whether to reload now: stale, and the keyboard is not held. */
export const shouldReload = (s: BuildState, held: boolean): boolean => isStale(s) && !held;

let reload = () => location.reload();

/** Tests swap the reload. */
export function setReloader(f: () => void): void {
  reload = f;
}

export function useAutoReload(build: string | undefined): void {
  const [s, push] = useReducer(buildReducer, initialBuild);
  useEffect(() => push(build), [build]);
  useEffect(() => {
    if (!isStale(s)) return;
    const go = () => {
      if (shouldReload(s, isControlHeld())) reload();
    };
    go();
    return onControlHeldChange(go); // waiting for the keyboard to be given back
  }, [s]);
}
