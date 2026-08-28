import { useCallback, useEffect, useState } from "react";
import { Plus, Trash2, Webhook, Zap } from "lucide-react";
import { createWebhook, deleteWebhook, listDeliveries, listWebhooks, updateWebhook } from "@/api/webhooks";
import type { WebhookDelivery, WebhookRow } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { Badge } from "@/components/ui/Badge";
import { CopyField } from "@/components/ui/CopyField";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusDot } from "@/components/ui/StatusDot";
import { timeAgo } from "@/lib/utils";

const DELIVERY_TONE: Record<string, "healthy" | "failed" | "degraded" | "neutral"> = {
  accepted: "healthy", rejected: "failed", error: "failed", disabled: "degraded",
};

const BLANK = {
  slug: "", name: "", description: "", event_kind: "",
  secret: "", signature_header: "", agent_id: "", enabled: true,
};

export function WebhooksPage() {
  const [rows, setRows] = useState<WebhookRow[] | null>(null);
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState({ ...BLANK });
  const [err, setErr] = useState("");
  const [focus, setFocus] = useState<string>("");

  const refresh = useCallback(async () => {
    const [w, d] = await Promise.all([listWebhooks(), listDeliveries(focus || undefined)]);
    setRows(w.webhooks);
    setDeliveries(d);
  }, [focus]);

  useEffect(() => { void refresh().catch((e) => setErr(String(e))); }, [refresh]);

  if (rows === null) return <PageSpinner />;

  const run = async (fn: () => Promise<unknown>) => {
    setErr("");
    try {
      await fn();
      await refresh();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    }
  };

  const platform = rows.filter((r) => r.kind === "platform");
  const custom = rows.filter((r) => r.kind === "custom");

  return (
    <div>
      <div className="mb-5 flex items-start justify-between gap-4">
        <div>
          <h1 className="text-h1">Webhooks</h1>
          <p className="text-sm text-fg-muted">
            Where things that happen elsewhere come in. Each one publishes an event, and a recipe
            can act on it — no agent has to read the payload.
          </p>
        </div>
        <Button onClick={() => setAdding((v) => !v)}>
          <Plus className="mr-1.5 h-4 w-4" />
          Add webhook
        </Button>
      </div>

      {err && (
        <Panel className="mb-4 border-failed/40 p-3">
          <p className="text-sm text-failed">{err}</p>
        </Panel>
      )}

      {adding && (
        <Panel className="mb-4 p-5">
          <h2 className="mb-1 text-h2">Custom webhook</h2>
          <p className="mb-3 text-sm text-fg-muted">
            For a service KARMAX has no integration with. The payload becomes the event as it
            arrives, under the kind you choose here — which is what you write in a recipe's{" "}
            <code className="font-mono text-xs">on.event</code>.
          </p>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <div className="space-y-1">
              <Label htmlFor="slug">Path *</Label>
              <Input id="slug" value={draft.slug} placeholder="stripe-prod"
                onChange={(e) => setDraft({ ...draft, slug: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="wname">Name</Label>
              <Input id="wname" value={draft.name} placeholder="Stripe (production)"
                onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="kind">Event kind</Label>
              <Input id="kind" value={draft.event_kind} placeholder={`custom.${draft.slug || "…"}`}
                onChange={(e) => setDraft({ ...draft, event_kind: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="secret">Secret</Label>
              <Input id="secret" type="password" value={draft.secret} placeholder="leave blank for an open endpoint"
                onChange={(e) => setDraft({ ...draft, secret: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="sig">Signature header</Label>
              <Input id="sig" value={draft.signature_header} placeholder="X-Hub-Signature-256"
                onChange={(e) => setDraft({ ...draft, signature_header: e.target.value })} />
            </div>
            <div className="space-y-1">
              <Label htmlFor="agent">Also send to agent</Label>
              <Input id="agent" value={draft.agent_id} placeholder="optional"
                onChange={(e) => setDraft({ ...draft, agent_id: e.target.value })} />
            </div>
          </div>
          <p className="mt-2 text-xs text-fg-subtle">
            With a signature header the secret is checked as an HMAC of the body (sha256 or sha1,
            with or without a prefix). Without one it is a shared token, sent as{" "}
            <code className="font-mono">X-Webhook-Token</code>, a bearer token, or{" "}
            <code className="font-mono">?token=</code>. Leave the secret blank and anyone who knows
            the URL can post.
          </p>
          <div className="mt-3 flex gap-2">
            <Button
              disabled={!draft.slug}
              onClick={() => run(async () => {
                await createWebhook(draft);
                setDraft({ ...BLANK });
                setAdding(false);
              })}
            >
              Create
            </Button>
            <Button variant="ghost" onClick={() => setAdding(false)}>Cancel</Button>
          </div>
        </Panel>
      )}

      <section className="mb-6">
        <h2 className="mb-1 text-h2">Platforms KARMAX understands</h2>
        <p className="mb-3 text-sm text-fg-muted">
          These arrive in a shape KARMAX already knows, so deliveries are decoded into typed
          events before anything acts on them.
        </p>
        {platform.length === 0 ? (
          <EmptyState icon={Webhook} title="No platform webhooks" body="Connectors declare these." />
        ) : (
          <div className="space-y-2">
            {platform.map((r) => <Row key={r.id} r={r} onFocus={setFocus} focused={focus === r.slug} />)}
          </div>
        )}
      </section>

      <section className="mb-6">
        <h2 className="mb-1 text-h2">Custom</h2>
        <p className="mb-3 text-sm text-fg-muted">
          Anything else. The payload is published as-is under the kind you chose.
        </p>
        {custom.length === 0 ? (
          <EmptyState icon={Zap} title="No custom webhooks" body="Add one to receive from a service KARMAX has no integration with." />
        ) : (
          <div className="space-y-2">
            {custom.map((r) => (
              <Row
                key={r.id}
                r={r}
                focused={focus === r.slug}
                onFocus={setFocus}
                onToggle={() => run(() => updateWebhook(r.id, { enabled: !r.enabled }))}
                onDelete={() => run(() => deleteWebhook(r.id))}
              />
            ))}
          </div>
        )}
      </section>

      <section>
        <div className="mb-3 flex items-baseline justify-between gap-3">
          <div>
            <h2 className="text-h2">Recent deliveries{focus && ` — ${focus}`}</h2>
            <p className="text-sm text-fg-muted">
              A webhook that silently does nothing is the usual failure. This is the evidence.
            </p>
          </div>
          {focus && <Button variant="ghost" onClick={() => setFocus("")}>Show all</Button>}
        </div>
        {deliveries.length === 0 ? (
          <EmptyState icon={Webhook} title="Nothing has arrived yet" />
        ) : (
          <Panel className="overflow-hidden">
            <table className="w-full text-sm">
              <thead className="border-b border-border text-left text-xs uppercase tracking-wide text-fg-subtle">
                <tr>
                  <th className="px-4 py-2.5 font-medium">When</th>
                  <th className="px-4 py-2.5 font-medium">Endpoint</th>
                  <th className="px-4 py-2.5 font-medium">Result</th>
                  <th className="px-4 py-2.5 font-medium">Detail</th>
                </tr>
              </thead>
              <tbody>
                {deliveries.map((d) => (
                  <tr key={d.id} className="border-b border-border last:border-0 align-top">
                    <td className="whitespace-nowrap px-4 py-2.5 text-fg-muted">{timeAgo(d.received_at)}</td>
                    <td className="px-4 py-2.5 font-mono text-xs text-fg">{d.endpoint}</td>
                    <td className="px-4 py-2.5">
                      <Badge tone={DELIVERY_TONE[d.status] ?? "neutral"}>{d.status}</Badge>
                    </td>
                    <td className="px-4 py-2.5 text-fg-muted">
                      {d.detail || "—"}
                      {d.source && <span className="ml-2 text-xs text-fg-subtle">via {d.source}</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </Panel>
        )}
      </section>
    </div>
  );
}

function Row({
  r, focused, onFocus, onToggle, onDelete,
}: {
  r: WebhookRow;
  focused: boolean;
  onFocus: (slug: string) => void;
  onToggle?: () => void;
  onDelete?: () => void;
}) {
  return (
    <Panel className={"p-4 " + (focused ? "border-brand" : "")}>
      <div className="mb-2 flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            {/* live, not enabled: a platform webhook only mounts once its
                connector has credentials, so it can be configured and still not
                be answering. */}
            <StatusDot health={r.live ? "healthy" : "degraded"} />
            <h3 className="truncate text-sm font-semibold text-fg">{r.name || r.slug}</h3>
            {!r.secured && <Badge tone="degraded">open</Badge>}
            {!r.live && <Badge tone="neutral">not receiving</Badge>}
          </div>
          <p className="mt-1 text-xs text-fg-subtle">
            triggers <code className="font-mono text-fg-muted">{r.event_kind}</code>
            {r.agent_id && <> · also sent to <span className="text-fg-muted">{r.agent_id}</span></>}
          </p>
        </div>
        <div className="flex shrink-0 gap-1.5">
          <Button variant="ghost" onClick={() => onFocus(r.slug || r.connector || "")}>Deliveries</Button>
          {onToggle && <Button variant="ghost" onClick={onToggle}>{r.enabled ? "Disable" : "Enable"}</Button>}
          {onDelete && (
            <Button variant="ghost" onClick={onDelete}>
              <Trash2 className="h-3.5 w-3.5 text-failed" />
            </Button>
          )}
        </div>
      </div>
      <CopyField value={r.url} />
      {r.description && <p className="mt-2 text-xs text-fg-subtle">{r.description}</p>}
      {!r.live && r.kind === "platform" && (
        <p className="mt-2 text-xs text-fg-subtle">
          Mounted at startup for connectors that have credentials — save them, then restart.
        </p>
      )}
    </Panel>
  );
}
