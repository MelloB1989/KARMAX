// A held keyboard: /v1/term/{agent}?control=1. The server refuses a second
// controller with an error frame naming the holder.
import type { TermFrame } from "../model";
import { termURL } from "./stream";

export interface Control {
  send(b: Uint8Array | string): void;
  close(): void;
  onFrame(cb: (f: TermFrame) => void): void;
  onData(cb: (b: Uint8Array) => void): void;
}

export function openControl(agent: string): Control {
  const ws = new WebSocket(termURL(agent, true));
  ws.binaryType = "arraybuffer";
  const frames: ((f: TermFrame) => void)[] = [];
  const data: ((b: Uint8Array) => void)[] = [];
  const enc = new TextEncoder();
  ws.onmessage = (ev) => {
    if (typeof ev.data === "string") {
      const f = JSON.parse(ev.data) as TermFrame;
      frames.forEach((cb) => cb(f));
    } else {
      const b = new Uint8Array(ev.data as ArrayBuffer);
      data.forEach((cb) => cb(b));
    }
  };
  ws.onclose = () => frames.forEach((cb) => cb({ type: "released", agent }));
  return {
    send(b) {
      if (ws.readyState === WebSocket.OPEN) ws.send(typeof b === "string" ? enc.encode(b) : b);
    },
    close() {
      ws.close();
    },
    onFrame(cb) {
      frames.push(cb);
    },
    onData(cb) {
      data.push(cb);
    },
  };
}
