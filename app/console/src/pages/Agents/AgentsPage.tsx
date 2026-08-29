import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Bot } from "lucide-react";
import { listAgents } from "@/api/agents";
import type { AgentStatus, AgentSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { PageHeader } from "@/components/ui/PageHeader";
import { Badge } from "@/components/ui/Badge";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusDot, type Health } from "@/components/ui/StatusDot";

const AGENT_HEALTH: Record<AgentStatus, Health> = {
  idle: "healthy", running: "healthy", paused: "unknown",
  stopping: "degraded", stopped: "unknown", failed: "failed", crashed: "failed",
};

export function AgentsPage() {
  const [agents, setAgents] = useState<AgentSummary[] | null>(null);

  useEffect(() => { void listAgents().then(setAgents); }, []);

  if (agents === null) return <PageSpinner />;

  return (
    <div>
      <PageHeader
        title="Agents"
        register={[
          `${agents.length} installed`,
          `${agents.filter((a) => a.status === "running").length} running`,
          `${agents.reduce((n, a) => n + a.open_cases, 0)} cases open`,
        ]}
      />

      {agents.length === 0 ? (
        <EmptyState icon={Bot} title="No agents installed" />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {agents.map((a) => (
            <Link key={a.id} to={`/agents/${a.id}`}>
              <Panel className="h-full p-5 transition-colors hover:border-border-glow">
                <div className="mb-2 flex items-start justify-between gap-2">
                  <div className="flex items-center gap-2">
                    <StatusDot health={AGENT_HEALTH[a.status]} pulse={a.status === "running"} />
                    <h2 className="text-h2">{a.name}</h2>
                  </div>
                  {a.open_cases > 0 && (
                    <Badge tone="brand">{a.open_cases} open</Badge>
                  )}
                </div>
                <p className="mb-3 text-sm text-fg-muted">{a.description}</p>
                <div className="mb-3 flex flex-wrap gap-1.5">
                  {a.tags.map((t) => (
                    <Badge key={t}>{t}</Badge>
                  ))}
                  <Badge tone="neutral">{a.model}</Badge>
                </div>
                {/* What this agent may do, in the sentences an operator
                    approved. This is the most consequential thing on the page
                    and it was set at 12px grey under a fold — the one part of
                    the product nobody else has, rendered as an afterthought.
                    It is now the body of the card: full list, no truncation,
                    the verb carrying the weight. */}
                <ul className="space-y-1.5 border-t border-border pt-3">
                  {a.grants.map((g, i) => (
                    <li key={i} className="flex gap-2 text-[13px] leading-snug text-fg-muted">
                      <span aria-hidden className="mt-[7px] h-px w-2 shrink-0 bg-border-strong" />
                      <span>{emphasiseVerb(g)}</span>
                    </li>
                  ))}
                </ul>
              </Panel>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}

/**
 * Sets the verb of a permission sentence in the foreground colour.
 *
 * "may open PRs in acme/api" — the word that matters is what it can DO, and
 * scanning six of these for the difference between open, merge and push is the
 * actual task. The rest of the sentence stays muted so the verbs form a column
 * the eye can run down.
 */
function emphasiseVerb(grant: string) {
  const m = grant.match(/^(may\s+)(\S+)(.*)$/);
  if (!m) return grant;
  return (
    <>
      {m[1]}
      <span className="font-medium text-fg">{m[2]}</span>
      {m[3]}
    </>
  );
}
