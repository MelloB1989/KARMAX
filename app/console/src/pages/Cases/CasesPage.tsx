import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { Hash, KanbanSquare, MessageSquare, Search } from "lucide-react";
import { listCases } from "@/api/cases";
import { listAgents } from "@/api/agents";
import type { AgentSummary, Case, CaseState } from "@/api/types";
import { CASE_STATES, CaseStateChip, caseStateStripe } from "@/components/ui/CaseStateChip";
import { ChipGroup } from "@/components/ui/ChipGroup";
import { EmptyState } from "@/components/ui/EmptyState";
import { PageSpinner } from "@/components/ui/Spinner";
import { PageHeader } from "@/components/ui/PageHeader";
import { cn, timeAgo } from "@/lib/utils";

export function CasesPage() {
  const [cases, setCases] = useState<Case[] | null>(null);
  const [agents, setAgents] = useState<AgentSummary[]>([]);
  const [agentFilter, setAgentFilter] = useState("");
  const [stateFilter, setStateFilter] = useState("");
  const [q, setQ] = useState("");

  useEffect(() => {
    void listAgents().then(setAgents);
  }, []);

  useEffect(() => {
    let cancelled = false;
    void listCases({ agent: agentFilter || undefined, state: stateFilter || undefined }).then((cs) => {
      if (!cancelled) setCases(cs);
    });
    return () => { cancelled = true; };
  }, [agentFilter, stateFilter]);

  const filtered = useMemo(() => {
    if (!cases) return [];
    if (!q.trim()) return cases;
    const needle = q.trim().toLowerCase();
    return cases.filter((c) => c.title.toLowerCase().includes(needle) || c.key.toLowerCase().includes(needle));
  }, [cases, q]);

  const agentName = (id: string) => agents.find((a) => a.id === id)?.name ?? id;

  // Counted over what is ON SCREEN, not over the whole board — a summary that
  // disagrees with the rows above it is worse than no summary. With no filter
  // set, which is the default, they are the same thing.
  const counts = useMemo(() => {
    const tally = new Map<CaseState, number>();
    for (const c of filtered) {
      const st = c.state as CaseState;
      tally.set(st, (tally.get(st) ?? 0) + 1);
    }
    // Ordered by how much each one wants attention, not alphabetically.
    return (["building", "review", "ready", "grooming", "open", "done", "dropped"] as CaseState[])
      .map((state) => ({ state, n: tally.get(state) ?? 0 }))
      .filter(({ n }) => n > 0);
  }, [filtered]);

  return (
    <div>
      <PageHeader
        title="Cases"
        register={[
          cases === null ? "loading" : `${cases.length} cases`,
          cases !== null && filtered.length !== cases.length && `${filtered.length} shown`,
          // The state tally lives in the strip at the foot of the table, where
          // it summarises what is directly above it. Repeating it here would
          // be the same fact twice on one screen.
        ]}
      >
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-2 h-3.5 w-3.5 text-fg-subtle" />
          <input
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="Search title or key…"
            className="h-8 w-64 rounded-[var(--radius-card)] border border-border bg-bg pl-8 pr-2.5 font-mono text-[11.5px] outline-none placeholder:text-fg-subtle focus:border-border-glow"
          />
        </div>
      </PageHeader>

      <div className="mb-3 flex flex-wrap items-center gap-x-5 gap-y-2 border-y border-border py-2">
        <ChipGroup
          label="Agent"
          value={agentFilter}
          onChange={setAgentFilter}
          options={[{ value: "", label: "all" }, ...agents.map((a) => ({ value: a.id, label: a.id }))]}
        />
        <span aria-hidden="true" className="h-4 w-px bg-border" />
        <ChipGroup
          label="State"
          value={stateFilter}
          onChange={setStateFilter}
          options={[{ value: "", label: "all" }, ...CASE_STATES.map((st) => ({ value: st, label: st }))]}
        />
      </div>

      {cases === null ? (
        <PageSpinner />
      ) : filtered.length === 0 ? (
        <EmptyState
          icon={KanbanSquare}
          title="No cases match"
          body="Nothing is open for this agent and state right now."
        />
      ) : (
        <div className="overflow-hidden rounded-[var(--radius-panel)] border border-border">
          <table className="w-full table-fixed border-collapse">
            <thead>
              <tr className="border-b border-border bg-bg-soft text-left text-label font-semibold uppercase text-fg-subtle">
                <th className="w-[3px] p-0" />
                <th className="w-[172px] px-4 py-2 font-semibold">Key</th>
                <th className="w-full px-4 py-2 font-semibold">Title</th>
                <th className="w-[150px] px-4 py-2 font-semibold">Agent</th>
                <th className="w-[124px] px-4 py-2 font-semibold">State</th>
                <th className="w-[116px] px-4 py-2 font-semibold">Thread</th>
                <th className="w-[112px] whitespace-nowrap px-4 py-2 text-right font-semibold">Last activity</th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((c) => (
                <tr
                  key={c.id}
                  className="h-11 border-b border-border/60 last:border-0 transition-colors hover:bg-surface"
                >
                  {/* The stripe is the row's state at a glance, before any
                      reading happens — and it survives a greyscale screenshot. */}
                  <td className="w-[3px] p-0">
                    <div className={cn("h-11 w-[3px]", caseStateStripe(c.state as CaseState))} />
                  </td>
                  <td className="px-4 align-middle">
                    <Link
                      to={`/cases/${c.id}`}
                      className="font-mono text-[12px] font-medium text-fg hover:text-brand-400"
                    >
                      {c.key}
                    </Link>
                  </td>
                  <td className="max-w-0 px-4 align-middle">
                    <Link to={`/cases/${c.id}`} className="block truncate text-[13px] text-fg-muted hover:text-fg">
                      {c.title}
                    </Link>
                  </td>
                  <td className="truncate px-4 align-middle font-mono text-[11.5px] text-fg-muted">{agentName(c.agent)}</td>
                  <td className="px-4 align-middle">
                    <CaseStateChip state={c.state as CaseState} />
                  </td>
                  <td className="whitespace-nowrap px-4 align-middle font-mono text-[11px] text-fg-subtle">
                    {c.thread_channel ? (
                      <span className="inline-flex items-center gap-1">
                        <Hash className="h-3 w-3" />
                        {c.thread_channel.replace(/^#/, "")}
                      </span>
                    ) : (
                      <span className="inline-flex items-center gap-1 text-fg-subtle">
                        <MessageSquare className="h-3 w-3" />
                        none
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-4 text-right align-middle font-mono text-[11px] text-fg-subtle">{timeAgo(c.updated_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>

          {/* The board's shape in one line. Same vocabulary as the rows — the
              stripe colour is the legend, so nothing new has to be learned. */}
          <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5 border-t border-border bg-bg-soft px-4 py-2.5">
            <span className="font-mono text-[11px] text-fg-subtle">
              {filtered.length} {filtered.length === 1 ? "case" : "cases"}
            </span>
            {counts.map(({ state, n }) => (
              <span key={state} className="flex items-center gap-2">
                <span className={cn("h-1.5 w-1.5 rounded-[1px]", caseStateStripe(state))} />
                <span className="font-mono text-[11px] text-fg-muted">
                  {n} {state}
                </span>
              </span>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
