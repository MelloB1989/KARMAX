import { describe, expect, it } from "vitest";
import { renderToStaticMarkup } from "react-dom/server";
import fixture from "../../dev/snapshot.json";
import type { Agent, Snapshot } from "../model";
import { AgentCard } from "./AgentCard";
import { Roster } from "./Roster";
import { OpenTasks } from "./RightRail";
import { gpuName } from "../state/hooks";
import { bucketFraction, orderAgents } from "./roster";

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
    expect(html.match(/remote · no live view/g)).toHaveLength(1);
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
