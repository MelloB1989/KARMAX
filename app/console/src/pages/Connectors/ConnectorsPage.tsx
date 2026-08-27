import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import { Github, Instagram, Linkedin, MessageCircle, Plug, Ticket, Twitter } from "lucide-react";
import { listConnectors } from "@/api/connectors";
import type { ConnectorSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusLabel, connectorHealth } from "@/components/ui/StatusDot";
import { timeAgo } from "@/lib/utils";

// Keyed by connector id, which comes from each connector's own manifest — so
// this map can never be complete. KARMAX registers notion, instagram, x and
// linkedin alongside github and jira, and it gains more without the console
// being rebuilt.
//
// It used to be a closed map read as ICON[c.kind]. Any connector not in it
// yielded undefined, and rendering <Icon /> from undefined is React's "Element
// type is invalid" — a white screen for the whole page, not a missing glyph.
const ICON: Record<string, LucideIcon> = {
  slack: MessageCircle,
  github: Github,
  jira: Ticket,
  instagram: Instagram,
  linkedin: Linkedin,
  x: Twitter,
};

// Anything unrecognised still gets an icon rather than taking the page down.
function iconFor(kind: string): LucideIcon {
  return ICON[kind] ?? Plug;
}

export function ConnectorsPage() {
  const [connectors, setConnectors] = useState<ConnectorSummary[] | null>(null);

  useEffect(() => { void listConnectors().then(setConnectors); }, []);

  if (connectors === null) return <PageSpinner />;

  return (
    <div>
      <div className="mb-5">
        <h1 className="text-h1">Connectors</h1>
        <p className="text-sm text-fg-muted">Integrations registered under your own accounts.</p>
      </div>

      {connectors.length === 0 ? (
        <EmptyState icon={Plug} title="No connectors" />
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {connectors.map((c) => {
            const Icon = iconFor(c.kind);
            return (
              <Panel key={c.id} className="p-5">
                <div className="mb-3 flex items-center gap-2.5">
                  <span className="flex h-9 w-9 items-center justify-center rounded-[var(--radius-card)] bg-surface-2">
                    <Icon className="h-4.5 w-4.5 text-fg-muted" />
                  </span>
                  <div>
                    <h2 className="text-h2">{c.name}</h2>
                    <StatusLabel health={connectorHealth(c.status)} pulse={c.status === "healthy"}>
                      {c.status === "not_configured" ? "not configured" : c.status}
                    </StatusLabel>
                  </div>
                </div>
                <p className="mb-3 text-sm text-fg-muted">{c.detail}</p>
                <div className="flex items-center justify-between">
                  <span className="text-xs text-fg-subtle">
                    {c.last_checked_at ? `checked ${timeAgo(c.last_checked_at)}` : "never checked"}
                  </span>
                  <Link to={`/connectors/${c.id}`}>
                    <Button size="sm" variant="secondary">
                      {c.status === "not_configured" ? "Set up" : "Manage"}
                    </Button>
                  </Link>
                </div>
              </Panel>
            );
          })}
        </div>
      )}
    </div>
  );
}
