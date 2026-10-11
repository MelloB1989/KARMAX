// The fleet's live state: one WebSocket to /v1/watch, a snapshot and then
// RFC 6902 patches. A gap in seq means a lost frame, so the socket is closed
// and the subscription starts over from a fresh snapshot.
import { useSyncExternalStore } from "react";
import { applyPatch, type Operation } from "fast-json-patch";
import type { Message, Snapshot } from "../model";
import { freezeClock } from "./hooks";

export type Conn = "connecting" | "live" | "lost";

export interface FleetState {
  snapshot: Snapshot | null;
  conn: Conn;
  seq: number;
}

export interface SocketLike {
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: unknown) => void) | null;
  close(): void;
}

type SocketFactory = (url: string) => SocketLike;

let makeSocket: SocketFactory = (url) => new WebSocket(url) as unknown as SocketLike;

/** Tests swap the socket; nothing else should. */
export function setSocketFactory(f: SocketFactory): void {
  makeSocket = f;
}

let state: FleetState = { snapshot: null, conn: "connecting", seq: 0 };
const listeners = new Set<() => void>();
let socket: SocketLike | null = null;
let retry = 0;
let retryTimer: ReturnType<typeof setTimeout> | null = null;

function set(next: Partial<FleetState>): void {
  state = { ...state, ...next };
  listeners.forEach((l) => l());
}

function watchURL(): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/v1/watch`;
}

/** Applies one frame; returns false when the stream must restart. */
export function reduce(s: FleetState, m: Message): FleetState | null {
  if (m.type === "snapshot") return { snapshot: m.snapshot, conn: "live", seq: m.seq };
  if (!s.snapshot || m.seq !== s.seq + 1) return null;
  const doc = structuredClone(s.snapshot);
  const next = applyPatch(doc, m.ops as Operation[], false, true).newDocument as Snapshot;
  return { snapshot: next, conn: "live", seq: m.seq };
}

function connect(): void {
  if (retryTimer) clearTimeout(retryTimer);
  retryTimer = null;
  const ws = makeSocket(watchURL());
  socket = ws;
  ws.onopen = () => {
    retry = 0;
  };
  ws.onmessage = (ev) => {
    if (socket !== ws) return;
    const next = reduce(state, JSON.parse(String(ev.data)) as Message);
    if (next) {
      set(next);
    } else {
      ws.close(); // a gap: start over from a fresh snapshot
      reconnect(0);
    }
  };
  ws.onclose = () => {
    if (socket !== ws) return;
    set({ conn: "lost" });
    reconnect(Math.min(30_000, 1000 * 2 ** retry++));
  };
}

function reconnect(delay: number): void {
  socket = null;
  retryTimer = setTimeout(connect, delay);
}

async function loadFixture(): Promise<void> {
  // Development only: compiled out of the production bundle.
  const mod = await import("../../dev/snapshot.json");
  const snapshot = mod.default as unknown as Snapshot;
  freezeClock(Date.parse(snapshot.at));
  set({ snapshot, conn: "live", seq: 1 });
}

/** Starts the stream on first use. */
export function start(): void {
  if (socket || retryTimer || state.snapshot) return;
  if (import.meta.env.DEV && import.meta.env.VITE_FIXTURE) {
    void loadFixture();
    return;
  }
  connect();
}

/** Tests reset between cases. */
export function resetForTest(): void {
  socket?.close();
  socket = null;
  if (retryTimer) clearTimeout(retryTimer);
  retryTimer = null;
  retry = 0;
  state = { snapshot: null, conn: "connecting", seq: 0 };
}

export function getState(): FleetState {
  return state;
}

function subscribe(l: () => void): () => void {
  listeners.add(l);
  start();
  return () => listeners.delete(l);
}

export function useFleet(): FleetState {
  return useSyncExternalStore(subscribe, getState);
}
