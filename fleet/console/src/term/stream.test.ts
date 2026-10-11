import { afterEach, describe, expect, it, vi } from "vitest";
import { subscribeTerm } from "./stream";

class FakeWS {
  static OPEN = 1;
  static all: FakeWS[] = [];
  readyState = 0;
  sent: string[] = [];
  binaryType = "";
  onopen: (() => void) | null = null;
  onmessage: unknown = null;
  onclose: unknown = null;
  constructor(public url: string) {
    FakeWS.all.push(this);
  }
  send(d: string) {
    this.sent.push(d);
  }
  close() {}
  open() {
    this.readyState = 1;
    this.onopen?.();
  }
}

afterEach(() => {
  vi.unstubAllGlobals();
  FakeWS.all = [];
});

describe("terminal stream", () => {
  it("every new subscriber asks for a full screen on the shared socket", () => {
    vi.stubGlobal("WebSocket", FakeWS);
    const redraw = JSON.stringify({ type: "redraw" });
    const stopA = subscribeTerm("agent-01", () => {});
    const ws = FakeWS.all[0];
    expect(ws.sent).toEqual([]); // not open yet
    ws.open();
    expect(ws.sent).toEqual([redraw]); // the first joiner, once the socket is up
    const stopB = subscribeTerm("agent-01", () => {}); // the fly-in terminal joins the desk's socket
    expect(FakeWS.all).toHaveLength(1);
    expect(ws.sent).toEqual([redraw, redraw]);
    stopA();
    stopB();
  });
});
