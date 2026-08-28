import { useCallback, useEffect, useState } from "react";
import { Plus, Trash2, Webhook } from "lucide-react";
import { createWebhook, deleteWebhook, getCatalogue, listDeliveries, listWebhooks, updateWebhook } from "@/api/webhooks";
import type { WebhookCatalogue, WebhookDelivery, WebhookRow } from "@/api/types";
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
  slug: "", name: "", description: "", platform: "", event_kind: "",
  secret: "", signature_header: "", agent_id: "", enabled: true,
};

/** A dropdown, because the point is to be told the options rather than guess. */
function Select({
  id, value, onChange, children,
}: {
  id: string; value: string; onChange: (v: string) => void; children: React.ReactNode;
}) {
  return (
    <select
      id={id}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="h-9 w-full rounded-[var(--radius-field)] border border-border bg-surface px-2.5 text-sm text-fg outline-none focus:border-brand"
    >
      {children}
    </select>
  );
}

export function WebhooksPage() {
  const [rows, setRows] = useState<WebhookRow[] | null>(null);
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState({ ...BLANK });
  const [err, setErr] = useState("");
  const [focus, setFocus] = useState<string>("");
  const [cat, setCat] = useState<WebhookCatalogue | null>(null);

  useEffect(() => { void getCatalogue().then(setCat).catch(() => setCat(null)); }, []);

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
          <h2 className="mb-1 text-h2">New webhook</h2>
          <p className="mb-3 text-sm text-fg-muted">
            Pick the platform first — if KARMAX knows it, deliveries are decoded into a typed
            event and a recipe reads named fields instead of raw JSON.
          </p>

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <div className="space-y-1">
              <Label htmlFor="platform">Platform *</Label>
              <Select
                id="platform"
                value={draft.platform}
                onChange={(v) => {
                  const p = cat?.platforms.find((x) => x.id === v);
                  // The platform decides its own event kind and signature
                  // header; showing them as chosen beats leaving blanks the
                  // operator would try to fill in.
                  setDraft({
                    ...draft,
                    platform: v,
                    event_kind: p && p.id ? p.event_kind : "",
                    signature_header: p?.signature_header ?? "",
                  });
                }}
              >
                {(cat?.platforms ?? []).map((p) => (
                  <option key={p.id || "custom"} value={p.id}>{p.name}</option>
                ))}
              </Select>
            </div>

            <div className="space-y-1">
              <Label htmlFor="slug">Path *</Label>
              <Input id="slug" value={draft.slug} placeholder="acme-api-prod"
                onChange={(e) => setDraft({ ...draft, slug: e.target.value })} />
            </div>

            <div className="space-y-1">
              <Label htmlFor="wname">Name</Label>
              <Input id="wname" value={draft.name} placeholder="Acme API (production)"
                onChange={(e) => setDraft({ ...draft, name: e.target.value })} />
            </div>

            <div className="space-y-1">
              <Label htmlFor="kind">Event kind</Label>
              {selected(cat, draft.platform)?.id ? (
                // Fixed by the platform. Shown, not editable: a typed kind that
                // does not match is a webhook that silently never fires.
                <Input id="kind" value={draft.event_kind} readOnly className="opacity-70" />
              ) : (
                <Input id="kind" value={draft.event_kind} placeholder={`custom.${draft.slug || "…"}`}
                  onChange={(e) => setDraft({ ...draft, event_kind: e.target.value })} />
              )}
            </div>

            <div className="space-y-1">
              <Label htmlFor="secret">Secret</Label>
              <Input id="secret" type="password" value={draft.secret}
                placeholder={secretHint(selected(cat, draft.platform))}
                onChange={(e) => setDraft({ ...draft, secret: e.target.value })} />
            </div>

            {!selected(cat, draft.platform)?.id && (
              <div className="space-y-1">
                <Label htmlFor="sig">Signature header</Label>
                <Select id="sig" value={draft.signature_header}
                  onChange={(v) => setDraft({ ...draft, signature_header: v })}>
                  <option value="">None — the secret is a shared token</option>
                  {(cat?.signature_headers ?? []).map((h) => <option key={h} value={h}>{h}</option>)}
                </Select>
              </div>
            )}

            <div className="space-y-1">
              <Label htmlFor="agent">Also send to agent</Label>
              <Select id="agent" value={draft.agent_id}
                onChange={(v) => setDraft({ ...draft, agent_id: v })}>
                <option value="">No — recipes only</option>
                {(cat?.agents ?? []).map((a) => <option key={a} value={a}>{a}</option>)}
              </Select>
            </div>
          </div>

          {selected(cat, draft.platform) && (
            <div className="mt-3 rounded-[var(--radius-card)] bg-surface-2 p-3">
              <p className="text-xs text-fg-muted">{selected(cat, draft.platform)!.setup_hint}</p>
              {(selected(cat, draft.platform)!.fields ?? []).length > 0 && (
                <p className="mt-1.5 text-xs text-fg-subtle">
                  A recipe can read:{" "}
                  {selected(cat, draft.platform)!.fields!.map((f) => (
                    <code key={f} className="mr-1 font-mono text-fg-muted">{f}</code>
                  ))}
                </p>
              )}
            </div>
          )}

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

      {rows.length === 0 ? (
        <EmptyState
          icon={Webhook}
          title="No webhooks yet"
          body="Add one to receive events from GitHub, Jira, YouTrack, or anything else."
        />
      ) : (
        <div className="mb-6 space-y-2">
          {rows.map((r) => (
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

function selected(cat: WebhookCatalogue | null, id: string) {
  return cat?.platforms.find((p) => p.id === id);
}

function secretHint(p: ReturnType<typeof selected>): string {
  if (!p) return "";
  switch (p.secret_kind) {
    case "hmac":
      return "signs the body";
    case "token":
      return "sent as ?token=";
    default:
      return "leave blank for an open endpoint";
  }
}
