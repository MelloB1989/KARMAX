import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { getState, resetForTest, setSocketFactory, start, type SocketLike } from "./store";
import type { Snapshot } from "../model";

class FakeSocket implements SocketLike {
  static all: FakeSocket[] = [];
  onopen: SocketLike["onopen"] = null;
  onmessage: SocketLike["onmessage"] = null;
  onclose: SocketLike["onclose"] = null;
  closed = false;
  constructor() {
    FakeSocket.all.push(this);
  }
  send(m: unknown) {
    this.onmessage?.({ data: JSON.stringify(m) });
  }
  close() {
    this.closed = true;
  }
}

const snap = (load: number) => ({ at: "t", host: { load: [load, 0, 0] } }) as unknown as Snapshot;

beforeEach(() => {
  vi.useFakeTimers();
  FakeSocket.all = [];
  setSocketFactory(() => new FakeSocket());
  resetForTest();
});
afterEach(() => vi.useRealTimers());

describe("store", () => {
  it("applies a snapshot then patches in seq order", () => {
    start();
    const ws = FakeSocket.all[0];
    ws.send({ type: "snapshot", seq: 7, snapshot: snap(1) });
    ws.send({ type: "patch", seq: 8, ops: [{ op: "replace", path: "/host/load/0", value: 2 }] });
    expect(getState().snapshot?.host.load[0]).toBe(2);
    expect(getState().seq).toBe(8);
    expect(getState().conn).toBe("live");
  });

  it("resubscribes on a seq gap and replaces state with the new snapshot", () => {
    start();
    const ws = FakeSocket.all[0];
    ws.send({ type: "snapshot", seq: 1, snapshot: snap(1) });
    ws.send({ type: "patch", seq: 3, ops: [] });
    expect(ws.closed).toBe(true);
    vi.runOnlyPendingTimers();
    expect(FakeSocket.all.length).toBe(2);
    FakeSocket.all[1].send({ type: "snapshot", seq: 40, snapshot: snap(9) });
    expect(getState().snapshot?.host.load[0]).toBe(9);
    expect(getState().seq).toBe(40);
  });

  it('marks conn "lost" after the socket closes and "live" again after reconnect', () => {
    start();
    const ws = FakeSocket.all[0];
    ws.send({ type: "snapshot", seq: 1, snapshot: snap(1) });
    expect(getState().conn).toBe("live");
    ws.onclose?.({});
    expect(getState().conn).toBe("lost");
    vi.runOnlyPendingTimers();
    expect(FakeSocket.all.length).toBe(2);
    FakeSocket.all[1].send({ type: "snapshot", seq: 2, snapshot: snap(3) });
    expect(getState().conn).toBe("live");
  });
});
