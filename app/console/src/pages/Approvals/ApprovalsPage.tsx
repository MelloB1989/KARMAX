import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Check, CircleCheck, Hash, X } from "lucide-react";
import { decideApproval, listApprovals } from "@/api/approvals";
import type { Approval, ApprovalStatus } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Modal } from "@/components/ui/Modal";
import { Textarea } from "@/components/ui/Input";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { formatDateTime, timeAgo } from "@/lib/utils";

const STATUS_TONE: Record<ApprovalStatus, "neutral" | "healthy" | "failed" | "degraded"> = {
  pending: "degraded", approved: "healthy", executed: "healthy", rejected: "failed", failed: "failed",
};

const TABS: { key: "pending" | "all"; label: string }[] = [
  { key: "pending", label: "Pending" },
  { key: "all", label: "All" },
];

export function ApprovalsPage() {
  const [approvals, setApprovals] = useState<Approval[] | null>(null);
  const [tab, setTab] = useState<"pending" | "all">("pending");
  const [decisionTarget, setDecisionTarget] = useState<{ approval: Approval; decision: "approve" | "reject" } | null>(null);
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);

  const load = () => void listApprovals().then(setApprovals);
  useEffect(load, []);

  const visible = (approvals ?? []).filter((a) => tab === "all" || a.status === "pending");

  const confirmDecision = async () => {
    if (!decisionTarget) return;
    setBusy(true);
    try {
      await decideApproval(decisionTarget.approval.id, { decision: decisionTarget.decision, note: note.trim() || undefined });
      setDecisionTarget(null);
      setNote("");
      load();
    } finally {
      setBusy(false);
    }
  };

  if (approvals === null) return <PageSpinner />;

  return (
    <div>
      <div className="mb-5">
        <h1 className="text-h1">Approvals</h1>
        <p className="mt-1.5 font-mono text-register text-fg-subtle">
          {approvals === null
            ? "loading"
            : `${approvals.filter((a) => a.status === "pending").length} waiting on you`}
          <span className="mx-2 text-border-strong">/</span>
          {approvals === null ? "\u00a0" : `${approvals.length} total`}
        </p>
      </div>

      <div className="mb-4 flex gap-1 border-b border-border">
        {TABS.map((t) => (
          <button
            key={t.key}
            onClick={() => setTab(t.key)}
            className={`border-b-2 px-3 py-2 text-sm font-medium transition-colors ${
              tab === t.key ? "border-brand-500 text-fg" : "border-transparent text-fg-muted hover:text-fg"
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      {visible.length === 0 ? (
        <EmptyState icon={CircleCheck} title="Nothing waiting" body="Every proposal has been decided." />
      ) : (
        <div className="space-y-3">
          {visible.map((a) => (
            <Panel key={a.id} className="p-5">
              <div className="mb-2 flex flex-wrap items-start justify-between gap-2">
                <div>
                  <div className="flex items-center gap-2">
                    <h2 className="text-h2">{a.title}</h2>
                    <Badge tone={STATUS_TONE[a.status]}>{a.status}</Badge>
                  </div>
                  <p className="mt-0.5 text-xs text-fg-subtle">
                    {a.agent} · asked {a.role} · {timeAgo(a.created_at)}
                    {a.channel && (
                      <span className="ml-1 inline-flex items-center gap-0.5">
                        · <Hash className="h-3 w-3" />{a.channel.replace(/^#/, "")}
                      </span>
                    )}
                  </p>
                </div>
                {a.status === "pending" && (
                  <div className="flex gap-2">
                    <Button variant="danger" size="sm" onClick={() => setDecisionTarget({ approval: a, decision: "reject" })}>
                      <X className="h-3.5 w-3.5" /> Reject
                    </Button>
                    <Button variant="primary" size="sm" onClick={() => setDecisionTarget({ approval: a, decision: "approve" })}>
                      <Check className="h-3.5 w-3.5" /> Approve
                    </Button>
                  </div>
                )}
              </div>

              <p className="mb-2 text-sm text-fg-muted">{a.summary}</p>
              {a.context && <p className="mb-2 text-xs text-fg-subtle">{a.context}</p>}
              <div className="rounded-[var(--radius-card)] border border-border bg-bg-soft px-3 py-2 text-sm text-fg">
                <span className="font-semibold text-fg-muted">Will do: </span>{a.action}
              </div>

              {a.case_key && (
                <Link to={`/cases/${a.case_id}`} className="mt-2 inline-block font-mono text-xs text-brand-600 hover:underline">
                  {a.case_key}
                </Link>
              )}

              {a.status !== "pending" && (a.note || a.result) && (
                <div className="mt-3 space-y-1 border-t border-border pt-3 text-xs text-fg-subtle">
                  {a.decided_by && <p>decided by {a.decided_by} · {formatDateTime(a.decided_at)}</p>}
                  {a.note && <p>note: {a.note}</p>}
                  {a.result && <p>result: {a.result}</p>}
                </div>
              )}
            </Panel>
          ))}
        </div>
      )}

      <Modal
        open={decisionTarget !== null}
        onClose={() => setDecisionTarget(null)}
        title={decisionTarget?.decision === "approve" ? "Approve this action?" : "Reject this action?"}
      >
        {decisionTarget && (
          <div className="space-y-3">
            <p className="text-sm text-fg-muted">{decisionTarget.approval.title}</p>
            <Textarea
              rows={3}
              placeholder="Optional note (visible to the agent and in the audit log)…"
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
            <div className="flex justify-end gap-2">
              <Button variant="secondary" onClick={() => setDecisionTarget(null)}>Cancel</Button>
              <Button
                variant={decisionTarget.decision === "approve" ? "primary" : "danger"}
                onClick={confirmDecision}
                disabled={busy}
              >
                {busy ? "Working…" : decisionTarget.decision === "approve" ? "Approve" : "Reject"}
              </Button>
            </div>
          </div>
        )}
      </Modal>
    </div>
  );
}
