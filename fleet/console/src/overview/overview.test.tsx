import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import fixture from "../../dev/snapshot.json";
import type { Agent, Snapshot } from "../model";
import { AgentCard } from "./AgentCard";
import { Roster } from "./Roster";
import { ActivityTrack } from "./ActivityTrack";
import { OpenTasks } from "./RightRail";
import { gpuName } from "../state/hooks";
import { bucketFraction, activityRowHeight, densityFor, groupActivity, groupAgents, orderAgents, trackMetrics } from "./roster";
import { bannersFor } from "../motion/banners";

const base = fixture as unknown as Snapshot;
const t0 = Date.parse("2026-10-11T13:57:33Z");

function snap(over: Partial<Snapshot> = {}, agents?: Agent[]): Snapshot {
  return { ...base, ...over, agents: agents ?? base.agents };
}
const working = (base.agents ?? []).find((a) => a.id === "agent-01")!;

describe("overview", () => {
  it("AgentCard shows age instead of value when fleetd is stale", () => {
    const a: Agent = { ...working, phase: "working", idle_s: 7, recorded_at: new Date(t0 - 6 * 60_000).toISOString() };
    const fresh = snap({ sources: { ...base.sources, fleetd: { ok: true, at: new Date(t0).toISOString() } } }, [a]);
    const stale = snap({ sources: { ...base.sources, fleetd: { ok: true, at: new Date(t0 - 60_000).toISOString() } } }, [a]);
    const html = (s: Snapshot) =>
      renderToStaticMarkup(<AgentCard agent={a} snapshot={s} now={t0} index={0} selected={false} onSelect={() => {}} />);
    expect(html(fresh)).toContain("working");
    const h = html(stale);
    expect(h).toContain("6 m ago");
    expect(h).not.toContain("working</span>");
    expect(h).not.toContain("idle 7 s");
  });

  it("Roster orders by agent id and marks remote agents", () => {
    const mk = (id: string, host: string): Agent => ({ ...working, id, host });
    const s = snap({}, [mk("agent-10", "pc2"), mk("agent-02", "kali"), mk("agent-01", "pc2")]);
    expect(orderAgents(s.agents).map((a) => a.id)).toEqual(["agent-01", "agent-02", "agent-10"]);
    const html = renderToStaticMarkup(<Roster snapshot={s} now={t0} selected={null} onSelect={() => {}} />);
    expect(html.indexOf("agent-01")).toBeLessThan(html.indexOf("agent-02"));
    expect(html.match(/class="remote"/g)).toHaveLength(1);
    expect(html).toContain("on kali");
  });

  it("every agent whose session runs is up, on whichever host", () => {
    const mk = (id: string, host: string, phase: string): Agent => ({ ...working, id, host, phase });
    const s = snap({}, [
      mk("agent-01", "pc2", "working"), mk("agent-02", "pc2", "down"), mk("agent-09", "kali", "standby"),
      mk("agent-10", "kali", "waiting"), mk("agent-11", "kali", "unreachable"), mk("agent-12", "kali", "disabled"),
    ]);
    expect(renderToStaticMarkup(<Roster snapshot={s} now={t0} selected={null} onSelect={() => {}} />)).toContain("AGENTS · 3 / 6 UP");
  });

  it("an agent on another host shows fleetd's quota, and says so when fleetd has none", () => {
    const fresh = { ...base.sources, fleetd: { ok: true, at: new Date(t0).toISOString() } };
    const remote: Agent = { ...working, id: "agent-09", host: "kali", live: false, phase: "standby", five_hour_pct: 40, statusline_at: new Date(t0 - 30_000).toISOString() };
    const html = (a: Agent) =>
      renderToStaticMarkup(<AgentCard agent={a} snapshot={snap({ sources: fresh }, [a])} now={t0} index={8} selected={false} onSelect={() => {}} />);
    expect(html(remote)).toContain('class="card-bars"');
    expect(html(remote)).toContain("40");
    const none = html({ ...remote, statusline_at: undefined });
    expect(none).not.toContain('class="card-bars"');
    expect(none).toContain("quota");
  });

  it("ActivityTrack places a bucket at its time fraction", () => {
    const now = Date.parse("2026-10-12T00:00:00Z");
    expect(bucketFraction({ t: "2026-10-11T12:00:00Z", n: 1 }, now)).toBeCloseTo(0.5);
    expect(bucketFraction({ t: "2026-10-12T00:00:00Z", n: 1 }, now)).toBe(1);
    expect(bucketFraction({ t: "2026-10-10T00:00:00Z", n: 1 }, now)).toBe(0);
  });
});

describe("pc2 polish", () => {
  it("OpenTasks lists at most 6 rows (agent · task · age) and says how many more", () => {
    const tasks = Array.from({ length: 10 }, (_, i) => ({ id: `T${i}`, agent: "agent-01", assigner: "operator", since: new Date(t0 - 60_000 * (i + 1)).toISOString() }));
    const html = renderToStaticMarkup(<OpenTasks tasks={tasks} now={t0} />);
    expect(html.match(/class="orch-task"/g)).toHaveLength(6);
    expect(html).toContain("+4 more");
    expect(html).toContain("1 m");
    expect(renderToStaticMarkup(<OpenTasks tasks={tasks.slice(0, 2)} now={t0} />)).not.toContain("more");
  });

  it("gpuName shortens ANGLE strings to the renderer", () => {
    expect(gpuName("ANGLE (Intel, Mesa Intel(R) HD Graphics 530 (SKL GT2), OpenGL 4.6)")).toBe("Intel HD Graphics 530");
    expect(gpuName("ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero) (0x0000C0DE)), SwiftShader driver)")).toBe("SwiftShader (software)");
    expect(gpuName("Mali-G78")).toBe("Mali-G78");
  });
});

describe("agent count sizing", () => {
  const many = (n: number) => Array.from({ length: n }, (_, i) => ({ ...working, id: `agent-${String(i + 1).padStart(2, "0")}` }));

  it("8 agents render 8 compact cards and 8 activity rows", () => {
    const s = snap({}, many(8));
    const roster = renderToStaticMarkup(<Roster snapshot={s} now={t0} selected={null} onSelect={() => {}} />);
    expect(roster).toContain("roster-compact");
    expect(roster).toContain('data-count="8"');
    expect(roster.match(/class="card[ "]/g)).toHaveLength(8);
    expect(roster.match(/class="card-bars"/g)).toHaveLength(8);
    const track = renderToStaticMarkup(<ActivityTrack snapshot={s} now={t0} />);
    expect(track.match(/class="track-row"/g)).toHaveLength(8);
    expect(track).toContain("agent-08");
    expect(track).toContain(`height:${trackMetrics(8).height}px`);
    expect(track).toContain("--row-h:12px");
  });

  it("density follows the count; 4 agents keep the original 132 px strip, 12 stay dense", () => {
    expect(densityFor(4)).toBe("roomy");
    expect(densityFor(8)).toBe("compact");
    expect(densityFor(12)).toBe("dense");
    expect(trackMetrics(4).height).toBe(132);
    const s12 = snap({}, many(12));
    expect(renderToStaticMarkup(<Roster snapshot={s12} now={t0} selected={null} onSelect={() => {}} />)).toContain("roster-dense");
    expect(renderToStaticMarkup(<ActivityTrack snapshot={s12} now={t0} />).match(/class="track-row"/g)).toHaveLength(12);
    expect(renderToStaticMarkup(<Roster snapshot={snap({}, many(4))} now={t0} selected={null} onSelect={() => {}} />)).toContain("roster-roomy");
  });
});

describe("height budget at 1080", () => {
  it("header 72 + main + activity row (strip + 2x20 margin) = 1080 for 4, 8 and 12 agents", () => {
    const rows = [4, 8, 12].map((n) => activityRowHeight(n));
    expect(rows).toEqual([172, 202, 222]);
    // main = 1080 - 72 - activity row: 836, 806, 786 px, never negative
    expect(rows.map((r) => 1080 - 72 - r)).toEqual([836, 806, 786]);
  });
});

describe("slots", () => {
  // 12 agents of 2 slots each: 24 workers, one card per agent.
  const workers = (agents: number, slots: number): Agent[] =>
    Array.from({ length: agents }, (_, i) => `agent-${String(i + 1).padStart(2, "0")}`).flatMap((id) =>
      Array.from({ length: slots }, (_, k) => ({ ...working, id: k === 0 ? id : `${id}:${k + 1}`, slot_of: id, slot: k + 1 })),
    );

  it("24 workers make 12 dense cards with a selectable line per slot", () => {
    const s = snap({}, workers(12, 2).reverse());
    const html = renderToStaticMarkup(<Roster snapshot={s} now={t0} selected="agent-03:2" onSelect={() => {}} />);
    expect(html).toContain("roster-dense");
    expect(html).toContain('data-count="12"');
    expect(html.match(/class="card card-slots/g)).toHaveLength(12);
    expect(html.match(/class="slot-line/g)).toHaveLength(24);
    expect(html).toContain("AGENTS · 12 / 12 UP");
    // agent-01's slots, in order, before agent-02's; the selected line is marked
    expect(html.indexOf('data-worker="agent-01"')).toBeLessThan(html.indexOf('data-worker="agent-01:2"'));
    expect(html.indexOf('data-worker="agent-01:2"')).toBeLessThan(html.indexOf('data-worker="agent-02"'));
    expect(html).toMatch(/class="slot-line selected[^"]*"[^>]*data-worker="agent-03:2"/);
    // the strip is per agent too: 12 rows, the same height as 12 one-slot agents
    const track = renderToStaticMarkup(<ActivityTrack snapshot={s} now={t0} />);
    expect(track.match(/class="track-row"/g)).toHaveLength(12);
    expect(groupAgents(s.agents)).toHaveLength(12);
    expect(activityRowHeight(groupAgents(s.agents).length)).toBe(222);
  });

  it("a slot line says what that slot does; the card shows the account's quota once", () => {
    const [one, two] = workers(1, 2);
    const s = snap({ sources: { ...base.sources, fleetd: { ok: true, at: new Date(t0).toISOString() } } }, [
      { ...one, phase: "working", task: "20261024-fix-login-a1b2", five_hour_pct: 61, statusline_at: new Date(t0).toISOString() },
      { ...two, phase: "standby", idle_s: 300, five_hour_pct: 20, statusline_at: new Date(t0 - 3_600_000).toISOString() },
    ]);
    const html = renderToStaticMarkup(<Roster snapshot={s} now={t0} selected={null} onSelect={() => {}} />);
    expect(html).toContain("20261024-fix-login-a1b2");
    expect(html).toContain("idle 5 m");
    expect(html).toContain("5H 61%");
    expect(html).not.toContain("5H 20%");
    expect(html).toContain('data-phase="working" data-slots="2"');
  });

  it("an activity row adds up its agent's slots", () => {
    const g = { id: "agent-01", slots: workers(1, 2) };
    const act = { "agent-01": [{ t: "2026-10-11T12:00:00Z", n: 2 }], "agent-01:2": [{ t: "2026-10-11T12:00:00Z", n: 3 }, { t: "2026-10-11T12:15:00Z", n: 1 }] };
    expect(groupActivity(g, act)).toEqual([{ t: "2026-10-11T12:00:00Z", n: 5 }, { t: "2026-10-11T12:15:00Z", n: 1 }]);
  });

  it("a down container and a cooling account are one banner per agent, not per slot", () => {
    const ws = workers(2, 2).map((a) => ({ ...a, phase: a.slot_of === "agent-01" ? "down" : "cooling" }));
    const kinds = bannersFor(snap({}, ws)).map((b) => b.text.split(" · ")[0]);
    expect(kinds).toEqual(["agent-01 DOWN", "agent-02 COOLING"]);
  });
});
