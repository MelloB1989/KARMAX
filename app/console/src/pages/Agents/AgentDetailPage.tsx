import { useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Bot, ShieldCheck, Wrench, Zap } from "lucide-react";
import { getAgent } from "@/api/agents";
import type { AgentDetail } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Badge } from "@/components/ui/Badge";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusDot, type Health } from "@/components/ui/StatusDot";

const AGENT_HEALTH: Record<string, Health> = {
  idle: "healthy", running: "healthy", paused: "unknown",
  stopping: "degraded", stopped: "unknown", failed: "failed", crashed: "failed",
};

export function AgentDetailPage() {
  const { id = "" } = useParams();
  const [agent, setAgent] = useState<AgentDetail | null | undefined>(undefined);

  useEffect(() => { void getAgent(id).then(setAgent); }, [id]);

  if (agent === undefined) return <PageSpinner />;
  if (agent === null) return <EmptyState icon={Bot} title="Agent not found" body={`No agent matches "${id}".`} />;

  return (
    <div>
      <Link to="/agents" className="mb-4 inline-flex items-center gap-1.5 text-sm text-fg-muted hover:text-fg">
        <ArrowLeft className="h-3.5 w-3.5" /> Agents
      </Link>

      <div className="mb-6 flex items-center gap-3">
        <StatusDot health={AGENT_HEALTH[agent.status] ?? "unknown"} pulse={agent.status === "running"} />
        <h1 className="text-title text-fg">{agent.name}</h1>
        <Badge tone="neutral" className="capitalize">{agent.status}</Badge>
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_320px]">
        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-2 flex items-center gap-2 text-h2">
              <Zap className="h-4 w-4 text-fg-subtle" /> Persona
            </h2>
            <p className="whitespace-pre-wrap text-sm leading-relaxed text-fg-muted">{agent.persona}</p>
          </Panel>

          <Panel className="p-5">
            <h2 className="mb-3 flex items-center gap-2 text-h2">
              <ShieldCheck className="h-4 w-4 text-fg-subtle" /> Capability grants
            </h2>
            <p className="mb-3 text-xs text-fg-subtle">
              Exactly what this agent may do — nothing else, enforced on every call.
            </p>
            <ul className="space-y-1.5">
              {agent.grants.map((g, i) => (
                <li key={i} className="flex items-start gap-2 text-sm text-fg">
                  <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-brand-500" />
                  {g}
                </li>
              ))}
            </ul>
          </Panel>

          <Panel className="p-5">
            <h2 className="mb-3 flex items-center gap-2 text-h2">
              <Wrench className="h-4 w-4 text-fg-subtle" /> Tools
            </h2>
            <div className="flex flex-wrap gap-1.5">
              {agent.tools.map((t) => (
                <Badge key={t} className="font-mono">{t}</Badge>
              ))}
              {agent.mcps.map((m) => (
                <Badge key={m} tone="brand" className="font-mono">{m} (MCP)</Badge>
              ))}
            </div>
          </Panel>
        </div>

        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Details</h2>
            <dl className="space-y-2.5 text-sm">
              <Row label="Model">{agent.model}</Row>
              <Row label="Provider">{agent.provider}</Row>
              <Row label="Restart policy">{agent.restart_policy}</Row>
              <Row label="Open cases">{agent.open_cases}</Row>
            </dl>
          </Panel>

          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Triggers</h2>
            <dl className="space-y-2.5 text-sm">
              <Row label="Events">{agent.triggers.events.length ? agent.triggers.events.join(", ") : "none"}</Row>
              <Row label="Webhooks">{agent.triggers.webhooks.length ? agent.triggers.webhooks.join(", ") : "none"}</Row>
              <Row label="Schedules">
                {agent.triggers.schedules.length ? agent.triggers.schedules.map((s) => s.cron).join(", ") : "none"}
              </Row>
              <Row label="On start">{agent.triggers.run_on_start ? "yes" : "no"}</Row>
            </dl>
          </Panel>
        </div>
      </div>
    </div>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-fg-subtle">{label}</dt>
      <dd className="text-right text-fg">{children}</dd>
    </div>
  );
}
