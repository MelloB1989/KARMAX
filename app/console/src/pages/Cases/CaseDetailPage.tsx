import { useEffect, useState, type ReactNode } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Hash, Box } from "lucide-react";
import { getCase } from "@/api/cases";
import { getAgent } from "@/api/agents";
import type { AgentSummary, CaseDetail, CaseState } from "@/api/types";
import { CaseStateChip } from "@/components/ui/CaseStateChip";
import { Panel } from "@/components/ui/Panel";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusLabel, sandboxHealth } from "@/components/ui/StatusDot";
import { formatDateTime, timeAgo } from "@/lib/utils";
import { iconForEventKind, formatPayload } from "./eventIcon";

export function CaseDetailPage() {
  const { id = "" } = useParams();
  const [detail, setDetail] = useState<CaseDetail | null | undefined>(undefined);
  const [agent, setAgent] = useState<AgentSummary | null>(null);

  useEffect(() => {
    let cancelled = false;
    void getCase(id).then((d) => {
      if (cancelled) return;
      setDetail(d);
      if (d) void getAgent(d.case.agent).then((a) => !cancelled && setAgent(a));
    });
    return () => { cancelled = true; };
  }, [id]);

  if (detail === undefined) return <PageSpinner />;
  if (detail === null) {
    return <EmptyState icon={Box} title="Case not found" body={`No case matches "${id}".`} />;
  }

  const { case: c, events, sandbox_runs } = detail;

  return (
    <div>
      <Link to="/cases" className="mb-4 inline-flex items-center gap-1.5 text-sm text-fg-muted hover:text-fg">
        <ArrowLeft className="h-3.5 w-3.5" /> Cases
      </Link>

      <div className="mb-6 flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="mb-1 flex items-center gap-2">
            <span className="font-mono text-sm font-semibold text-fg-muted">{c.key}</span>
            <CaseStateChip state={c.state as CaseState} />
          </div>
          <h1 className="text-h1">{c.title}</h1>
        </div>
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_320px]">
        <Panel className="p-5">
          <h2 className="mb-4 text-h2">Event history</h2>
          {events.length === 0 ? (
            <p className="text-sm text-fg-subtle">No events recorded yet.</p>
          ) : (
            <ol className="space-y-0">
              {events.map((e, i) => {
                const Icon = iconForEventKind(e.kind);
                const fields = formatPayload(e.payload);
                return (
                  <li key={e.id} className="relative flex gap-3 pb-5 last:pb-0">
                    {i < events.length - 1 && (
                      <span className="absolute left-[13px] top-7 h-full w-px bg-border" aria-hidden />
                    )}
                    <span className="z-10 flex h-7 w-7 shrink-0 items-center justify-center rounded-full border border-border bg-surface-2 text-fg-muted">
                      <Icon className="h-3.5 w-3.5" />
                    </span>
                    <div className="min-w-0 flex-1 pt-0.5">
                      <div className="flex flex-wrap items-baseline gap-x-2">
                        <span className="text-sm font-semibold text-fg">{e.kind}</span>
                        <span className="text-xs text-fg-subtle">{e.actor}</span>
                        <span className="text-xs text-fg-subtle">·</span>
                        <span className="text-xs text-fg-subtle" title={formatDateTime(e.created_at)}>
                          {timeAgo(e.created_at)}
                        </span>
                      </div>
                      {fields.length > 0 && (
                        <dl className="mt-1 space-y-0.5 text-sm text-fg-muted">
                          {fields.map((f, j) => (
                            <div key={j} className="flex gap-1.5">
                              {f.key && <dt className="shrink-0 font-medium text-fg-subtle">{f.key}:</dt>}
                              <dd className="min-w-0 break-words">{f.value}</dd>
                            </div>
                          ))}
                        </dl>
                      )}
                    </div>
                  </li>
                );
              })}
            </ol>
          )}
        </Panel>

        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Details</h2>
            <dl className="space-y-2.5 text-sm">
              <Row label="Agent">
                {agent ? (
                  <Link to={`/agents/${agent.id}`} className="text-fg hover:text-brand-600 hover:underline">
                    {agent.name}
                  </Link>
                ) : (
                  c.agent
                )}
              </Row>
              <Row label="Namespace"><span className="font-mono text-xs">{c.namespace}</span></Row>
              <Row label="Thread">
                {c.thread_channel ? (
                  <span className="inline-flex items-center gap-1">
                    <Hash className="h-3 w-3" />{c.thread_channel.replace(/^#/, "")}
                  </span>
                ) : (
                  <span className="text-fg-subtle">none</span>
                )}
              </Row>
              <Row label="Opened">{formatDateTime(c.created_at)}</Row>
              <Row label="Updated">{timeAgo(c.updated_at)}</Row>
            </dl>
          </Panel>

          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Sandbox runs</h2>
            {sandbox_runs.length === 0 ? (
              <p className="text-sm text-fg-subtle">No sandbox runs for this case.</p>
            ) : (
              <div className="space-y-3">
                {sandbox_runs.map((r) => (
                  <div key={r.id} className="rounded-[var(--radius-card)] border border-border p-3">
                    <div className="mb-1.5 flex items-center justify-between">
                      <StatusLabel health={sandboxHealth(r.status)} pulse={r.status === "running"}>
                        {r.status}
                      </StatusLabel>
                      {r.exit_code !== 0 && r.status === "exited" && (
                        <span className="text-xs text-failed">exit {r.exit_code}</span>
                      )}
                    </div>
                    <p className="font-mono text-xs text-fg-muted">
                      {r.repo}@{r.branch}
                    </p>
                    <p className="mt-1 text-xs text-fg-muted">{r.task}</p>
                    {r.log_tail && (
                      <pre className="mt-2 max-h-32 overflow-auto rounded bg-bg-soft p-2 font-mono text-[11px] leading-relaxed text-fg-muted">
                        {r.log_tail}
                      </pre>
                    )}
                    <p className="mt-1.5 text-[11px] text-fg-subtle">
                      started {timeAgo(r.started_at)}
                      {r.finished_at ? ` · finished ${timeAgo(r.finished_at)}` : ""}
                    </p>
                  </div>
                ))}
              </div>
            )}
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
