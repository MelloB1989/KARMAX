// Terminal bytes, one refcounted WebSocket per agent to /v1/term/{agent}.
// Binary frames are the terminal's output; text frames are TermFrame JSON.
import type { TermFrame } from "../model";

type DataFn = (b: Uint8Array) => void;
type FrameFn = (f: TermFrame) => void;

interface Stream {
  ws: WebSocket | null;
  data: Set<DataFn>;
  frames: Set<FrameFn>;
  retry: number;
  timer: ReturnType<typeof setTimeout> | null;
}

const streams = new Map<string, Stream>();

export function termURL(agent: string, control = false): string {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/v1/term/${encodeURIComponent(agent)}${control ? "?control=1" : ""}`;
}

function open(agent: string, s: Stream): void {
  const ws = new WebSocket(termURL(agent));
  ws.binaryType = "arraybuffer";
  s.ws = ws;
  ws.onopen = () => {
    s.retry = 0;
  };
  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      const f = JSON.parse(ev.data) as TermFrame;
      s.frames.forEach((fn) => fn(f));
    } else {
      const b = new Uint8Array(ev.data as ArrayBuffer);
      s.data.forEach((fn) => fn(b));
    }
  };
  ws.onclose = () => {
    if (s.ws !== ws || s.data.size === 0) return;
    s.frames.forEach((fn) => fn({ type: "reconnecting", agent }));
    s.timer = setTimeout(() => open(agent, s), Math.min(30_000, 1000 * 2 ** s.retry++));
  };
}

async function replayFixture(agent: string, onData: DataFn): Promise<void> {
  // Development only: compiled out of the production bundle.
  const files = import.meta.glob("../../dev/term-*.ans", { query: "?raw", import: "default" });
  const load = files[`../../dev/term-${agent}.ans`];
  if (load) onData(new TextEncoder().encode(((await load()) as string).replace(/\n/g, "\r\n")));
}

/** Subscribes to an agent's terminal; returns the unsubscribe. */
export function subscribeTerm(agent: string, onData: DataFn, onFrame?: FrameFn): () => void {
  if (import.meta.env.DEV && import.meta.env.VITE_FIXTURE) {
    void replayFixture(agent, onData);
    return () => {};
  }
  let s = streams.get(agent);
  if (!s) {
    s = { ws: null, data: new Set(), frames: new Set(), retry: 0, timer: null };
    streams.set(agent, s);
  }
  s.data.add(onData);
  if (onFrame) s.frames.add(onFrame);
  if (!s.ws) open(agent, s);
  const st = s;
  return () => {
    st.data.delete(onData);
    if (onFrame) st.frames.delete(onFrame);
    if (st.data.size === 0) {
      if (st.timer) clearTimeout(st.timer);
      const ws = st.ws;
      st.ws = null;
      ws?.close();
      streams.delete(agent);
    }
  };
}
