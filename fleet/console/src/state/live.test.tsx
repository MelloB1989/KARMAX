import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import fixture from "../../dev/snapshot.json";
import type { Agent, Snapshot } from "../model";
import { ageOf, isKnown, isStale } from "./derive";
import { AgentCard } from "../overview/AgentCard";
import { RightRail } from "../overview/RightRail";
import { eventKey } from "../overview/EventStream";

const base = fixture as unknown as Snapshot;
const now = Date.parse(base.at);
const ZERO = "0001-01-01T00:00:00Z";

describe("live-console fixes", () => {
  it("a time before 2000 is unknown: infinite age, stale, and the card says 'no record'", () => {
    expect(ageOf(ZERO, now)).toBe(Infinity);
    expect(isKnown(ZERO)).toBe(false);
    expect(isStale({ ok: true, at: ZERO }, 5, now)).toBe(true);
    const a: Agent = { ...(base.agents ?? [])[0], recorded_at: ZERO };
    const html = renderToStaticMarkup(<AgentCard agent={a} snapshot={base} now={now} index={0} selected={false} onSelect={() => {}} />);
    expect(html).toContain("no record");
    expect(html).not.toMatch(/\d{4,} d/);
  });

  it("live events with id 0 get distinct keys; recorded ones key by id", () => {
    const ev = { id: 0, at: "2026-10-11T10:00:00Z", agent: "agent-01", kind: "Stop", session: "s" };
    expect(eventKey(ev, 0)).not.toBe(eventKey(ev, 1));
    expect(eventKey({ ...ev, id: 7 }, 3)).toBe("id:7");
  });

  it("a null orchestrator reads 'no data from fleetd'; unanswered fleetd reads unreachable", () => {
    const s: Snapshot = { ...base, orchestrator: null, sources: { ...base.sources, fleetd: { ok: false, error: "dial tcp: refused" } } };
    const html = renderToStaticMarkup(<RightRail snapshot={s} now={now} />);
    expect(html).toContain("orchestrator: no data from fleetd");
    expect(html).toContain("fleetd unreachable");
    expect(html).toContain("dial tcp: refused");
    expect(html).not.toContain("no orchestrator");
  });
});
