import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { ScrollText, Search } from "lucide-react";
import { queryAudit } from "@/api/audit";
import { listAgents } from "@/api/agents";
import type { AgentSummary, AuditEvent } from "@/api/types";
import { Badge } from "@/components/ui/Badge";
import { Select } from "@/components/ui/Input";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { formatDateTime, cn } from "@/lib/utils";

const WINDOWS: { key: string; label: string; hours?: number }[] = [
  { key: "24h", label: "Last 24h", hours: 24 },
  { key: "7d", label: "Last 7 days", hours: 24 * 7 },
  { key: "30d", label: "Last 30 days", hours: 24 * 30 },
  { key: "all", label: "All time" },
];

const ACTOR_TONE: Record<AuditEvent["actor_kind"], "neutral" | "brand"> = {
  human: "brand", agent: "neutral", recipe: "neutral", system: "neutral",
};

export function AuditPage() {
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [agentFilter, setAgentFilter] = useState("");
  const [actorFilter, setActorFilter] = useState("");
  const [verbFilter, setVerbFilter] = useState("");
  const [windowKey, setWindowKey] = useState("all");

  useEffect(() => { void listAgents().then(setAgents); }, []);

  const since = useMemo(() => {
    const w = WINDOWS.find((x) => x.key === windowKey);
    if (!w?.hours) return undefined;
    return new Date(Date.now() - w.hours * 3_600_000).toISOString();
  }, [windowKey]);

  useEffect(() => {
    let cancelled = false;
    void queryAudit({
      agent: agentFilter || undefined,
      actor_id: actorFilter.trim() || undefined,
      verb: verbFilter.trim() || undefined,
      since,
    }).then((evs) => { if (!cancelled) setEvents(evs); });
    return () => { cancelled = true; };
  }, [agentFilter, actorFilter, verbFilter, since]);

  return (
    <div>
      <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-h1">Audit</h1>
          <p className="mt-1.5 font-mono text-register text-fg-subtle">
            {events === null
              ? "loading"
              : `${events.length} event${events.length === 1 ? "" : "s"}`}
            <span className="mx-2 text-border-strong">/</span>
            {events === null
              ? "\u00a0"
              : `${events.filter((e) => e.decision === "denied").length} denied`}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div className="relative">
            <Search className="pointer-events-none absolute left-2.5 top-2.5 h-3.5 w-3.5 text-fg-subtle" />
            <input
              value={actorFilter}
              onChange={(e) => setActorFilter(e.target.value)}
              placeholder="Actor id…"
              className="h-8 w-36 rounded-[var(--radius-card)] border border-border bg-bg pl-8 pr-2.5 text-sm outline-none focus:border-border-glow"
            />
          </div>
          <input
            value={verbFilter}
            onChange={(e) => setVerbFilter(e.target.value)}
            placeholder="Verb contains…"
            className="h-8 w-36 rounded-[var(--radius-card)] border border-border bg-bg px-2.5 text-sm outline-none focus:border-border-glow"
          />
          <Select value={agentFilter} onChange={(e) => setAgentFilter(e.target.value)} className="h-8 w-40 py-0 text-sm">
            <option value="">All agents</option>
            {agents.map((a) => <option key={a.id} value={a.id}>{a.name}</option>)}
          </Select>
          <Select value={windowKey} onChange={(e) => setWindowKey(e.target.value)} className="h-8 w-36 py-0 text-sm">
            {WINDOWS.map((w) => <option key={w.key} value={w.key}>{w.label}</option>)}
          </Select>
        </div>
      </div>

      {events === null ? (
        <PageSpinner />
      ) : events.length === 0 ? (
        <EmptyState icon={ScrollText} title="No matching audit events" />
      ) : (
        <div className="overflow-hidden rounded-[var(--radius-panel)] border border-border">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-border bg-surface text-left text-xs font-semibold uppercase tracking-wide text-fg-subtle">
                <th className="px-4 py-2.5">When</th>
                <th className="px-4 py-2.5">Actor</th>
                <th className="px-4 py-2.5">Agent / recipe</th>
                <th className="px-4 py-2.5">Case</th>
                <th className="px-4 py-2.5">Verb</th>
                <th className="px-4 py-2.5">Target</th>
                <th className="px-4 py-2.5">Decision</th>
              </tr>
            </thead>
            <tbody>
              {events.map((e, i) => (
                <tr key={e.id} className={cn("border-b border-border last:border-0", i % 2 === 1 && "bg-bg-soft/50")}>
                  <td className="whitespace-nowrap px-4 py-2.5 align-top text-fg-muted">{formatDateTime(e.created_at)}</td>
                  <td className="px-4 py-2.5 align-top">
                    <Badge tone={ACTOR_TONE[e.actor_kind]}>{e.actor_kind}</Badge>{" "}
                    <span className="text-fg">{e.actor_id}</span>
                  </td>
                  <td className="px-4 py-2.5 align-top text-fg-muted">
                    {e.agent || "—"}{e.recipe ? ` / ${e.recipe}` : ""}
                  </td>
                  <td className="px-4 py-2.5 align-top">
                    {e.case_id ? (
                      <Link to={`/cases/${e.case_id}`} className="font-mono text-xs text-brand-600 hover:underline">
                        {e.case_id}
                      </Link>
                    ) : (
                      <span className="text-fg-subtle">—</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 align-top font-mono text-xs text-fg">{e.verb}</td>
                  <td className="px-4 py-2.5 align-top text-fg-muted">{e.target || "—"}</td>
                  <td className="px-4 py-2.5 align-top">
                    <Badge tone={e.decision === "denied" ? "failed" : e.decision === "allowed" ? "healthy" : "neutral"}>
                      {e.decision || "—"}
                    </Badge>
                    {e.detail && <p className="mt-1 max-w-xs text-xs text-fg-subtle">{e.detail}</p>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
