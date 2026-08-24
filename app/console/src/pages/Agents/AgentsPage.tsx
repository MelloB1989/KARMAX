import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Bot } from "lucide-react";
import { listAgents } from "@/api/agents";
import type { AgentStatus, AgentSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
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
      <div className="mb-5">
        <h1 className="text-h1">Agents</h1>
        <p className="text-sm text-fg-muted">Installed packs, their persona, and exactly what they're allowed to do.</p>
      </div>

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
                <div className="space-y-1 border-t border-border pt-3">
                  {a.grants.slice(0, 3).map((g, i) => (
                    <p key={i} className="text-xs text-fg-muted before:mr-1.5 before:text-fg-subtle before:content-['—']">
                      {g}
                    </p>
                  ))}
                  {a.grants.length > 3 && (
                    <p className="text-xs font-medium text-brand-600">+{a.grants.length - 3} more</p>
                  )}
                </div>
              </Panel>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
