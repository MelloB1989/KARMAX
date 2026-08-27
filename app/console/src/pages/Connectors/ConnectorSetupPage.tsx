import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Plug, Stethoscope } from "lucide-react";
import { getConnectorSetup, listConnectors, runConnectorHealthCheck, saveConnectorCredentials } from "@/api/connectors";
import type { ConnectorHealthCheck, ConnectorSetup, ConnectorSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { CopyField } from "@/components/ui/CopyField";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { StatusLabel, connectorHealth } from "@/components/ui/StatusDot";
import { formatDateTime } from "@/lib/utils";

export function ConnectorSetupPage() {
  const { id = "" } = useParams();
  const [setup, setSetup] = useState<ConnectorSetup | null | undefined>(undefined);
  const [summary, setSummary] = useState<ConnectorSummary | null>(null);
  const [fields, setFields] = useState<Record<string, string>>({});
  const [savedMsg, setSavedMsg] = useState("");
  const [saving, setSaving] = useState(false);
  const [checking, setChecking] = useState(false);
  const [check, setCheck] = useState<ConnectorHealthCheck | null>(null);

  useEffect(() => {
    void getConnectorSetup(id).then(setSetup);
    void listConnectors().then((cs) => setSummary(cs.find((c) => c.id === id) ?? null));
  }, [id]);

  if (setup === undefined) return <PageSpinner />;
  if (setup === null) return <EmptyState icon={Plug} title="Connector not found" body={`No connector named "${id}".`} />;

  const save = async () => {
    setSaving(true);
    setSavedMsg("");
    try {
      const updated = await saveConnectorCredentials(id, fields);
      setSummary(updated);
      setSavedMsg("Credentials saved.");
    } finally {
      setSaving(false);
    }
  };

  const check_ = async () => {
    setChecking(true);
    try {
      setCheck(await runConnectorHealthCheck(id));
    } finally {
      setChecking(false);
    }
  };

  return (
    <div>
      <Link to="/connectors" className="mb-4 inline-flex items-center gap-1.5 text-sm text-fg-muted hover:text-fg">
        <ArrowLeft className="h-3.5 w-3.5" /> Connectors
      </Link>

      <div className="mb-5 flex items-center gap-3">
        <h1 className="text-h1 capitalize">{id} setup</h1>
        {summary && (
          <StatusLabel health={connectorHealth(summary.status)}>{summary.status.replace("_", " ")}</StatusLabel>
        )}
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_360px]">
        <Panel className="p-5">
          <h2 className="mb-4 text-h2">Register your own {id} app</h2>
          <ol className="space-y-4">
            {setup.steps.map((s, i) => (
              <li key={i} className="flex gap-3">
                <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-surface-2 text-xs font-semibold text-fg-muted">
                  {i + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-semibold text-fg">{s.title}</p>
                  <p className="mt-0.5 text-sm text-fg-muted">{s.body}</p>
                  {s.value && <div className="mt-2"><CopyField value={s.value} /></div>}
                </div>
              </li>
            ))}
          </ol>
        </Panel>

        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Credentials</h2>
            <div className="space-y-3">
              {setup.fields.map((f) => (
                <div key={f.key} className="space-y-1">
                  <Label htmlFor={f.key}>{f.label}{f.required && " *"}</Label>
                  <Input
                    id={f.key}
                    type={f.type === "secret" ? "password" : "text"}
                    placeholder={f.placeholder}
                    value={fields[f.key] ?? ""}
                    onChange={(e) => setFields((prev) => ({ ...prev, [f.key]: e.target.value }))}
                  />
                </div>
              ))}
              <Button variant="primary" onClick={save} disabled={saving} className="w-full">
                {saving ? "Saving…" : "Save credentials"}
              </Button>
              {savedMsg && <p className="text-xs text-healthy">{savedMsg}</p>}
            </div>
          </Panel>

          <Panel className="p-5">
            <h2 className="mb-3 flex items-center gap-2 text-h2">
              <Stethoscope className="h-4 w-4 text-fg-subtle" /> Health check
            </h2>
            <Button variant="secondary" onClick={check_} disabled={checking} className="w-full">
              {checking ? "Checking…" : "Run live check"}
            </Button>
            {check && (
              <div className="mt-3 space-y-1">
                <StatusLabel health={connectorHealth(check.status)}>{check.status.replace("_", " ")}</StatusLabel>
                <p className="text-xs text-fg-muted">{check.detail}</p>
                <p className="text-xs text-fg-subtle">checked {formatDateTime(check.checked_at)}</p>
              </div>
            )}
          </Panel>
        </div>
      </div>
    </div>
  );
}
