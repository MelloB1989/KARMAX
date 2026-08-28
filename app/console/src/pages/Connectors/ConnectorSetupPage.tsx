import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Check, ExternalLink, Plug, Stethoscope, Upload, UserCheck, UserPlus } from "lucide-react";
import { disconnect, getConnectorSetup, listConnections, listConnectors, runConnectorHealthCheck, saveConnectorCredentials, startConnect } from "@/api/connectors";
import type { ConnectorConnections, ConnectorHealthCheck, ConnectorSetup, ConnectorSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Badge } from "@/components/ui/Badge";
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
  const [saveErr, setSaveErr] = useState("");
  const [saving, setSaving] = useState(false);
  const [checking, setChecking] = useState(false);
  const [check, setCheck] = useState<ConnectorHealthCheck | null>(null);
  const [conns, setConns] = useState<ConnectorConnections | null>(null);
  const [connectErr, setConnectErr] = useState("");
  const [method, setMethod] = useState("");
  const [fileNames, setFileNames] = useState<Record<string, string>>({});

  useEffect(() => {
    void getConnectorSetup(id).then((s) => {
      setSetup(s);
      // Whichever method is already in use, so returning to the page shows the
      // setup that exists rather than defaulting back to the recommended one.
      setMethod(s?.active_method ?? s?.methods?.[0]?.id ?? "");
    });
    void listConnectors().then((cs) => setSummary(cs.find((c) => c.id === id) ?? null));
    // 404s for an install-wide connector, which is not an error — it just means
    // there are no per-person accounts to show.
    void listConnections(id).then(setConns).catch(() => setConns(null));
  }, [id]);

  if (setup === undefined) return <PageSpinner />;
  if (setup === null) return <EmptyState icon={Plug} title="Connector not found" body={`No connector named "${id}".`} />;

  const save = async () => {
    setSaving(true);
    setSavedMsg("");
    try {
      const updated = await saveConnectorCredentials(id, method ? { ...fields, auth_method: method } : fields);
      setSummary(updated);
      setSavedMsg("Credentials saved.");
      setSaveErr("");
    } catch (e) {
      // Validation refuses a wrong .pem here, which is the whole point of
      // checking before storing — showing it is what makes that useful.
      setSaveErr(e instanceof Error ? e.message : String(e));
      setSavedMsg("");
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
            {stepsFor(setup, method).map((s, i) => (
              <li key={i} className="flex gap-3">
                {/* A done step shows a tick instead of its number, so a
                    half-finished setup says which half. `done` is optional and
                    absent means "cannot tell" — which stays a plain number
                    rather than a green tick nobody earned. */}
                <span
                  className={
                    "flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-xs font-semibold " +
                    (s.done ? "bg-healthy/15 text-healthy" : "bg-surface-2 text-fg-muted")
                  }
                >
                  {s.done ? <Check className="h-3.5 w-3.5" /> : i + 1}
                </span>
                <div className="min-w-0 flex-1">
                  <p className="text-sm font-semibold text-fg">{s.title}</p>
                  <p className="mt-0.5 text-sm text-fg-muted">{s.body}</p>
                  {s.value && <div className="mt-2"><CopyField value={s.value} /></div>}
                  {s.url && (
                    <a
                      href={s.url}
                      target="_blank"
                      rel="noreferrer noopener"
                      className="mt-2 inline-flex items-center gap-1.5 text-sm font-medium text-brand hover:underline"
                    >
                      Open <ExternalLink className="h-3.5 w-3.5" />
                    </a>
                  )}
                </div>
              </li>
            ))}
          </ol>
        </Panel>

        <div className="space-y-5">
          {conns && (
            <Panel className="p-5">
              <h2 className="mb-1 text-h2">Your account</h2>
              <p className="mb-3 text-sm text-fg-muted">
                This connector acts as an individual person. The org sets up the app once below;
                everyone connects their own account, and the agent uses whichever one it is
                helping.
              </p>

              {conns.self_connected ? (
                <div className="flex items-center justify-between gap-3 rounded-[var(--radius-card)] bg-surface-2 px-3 py-2.5">
                  <span className="flex min-w-0 items-center gap-2 text-sm">
                    <UserCheck className="h-4 w-4 shrink-0 text-healthy" />
                    <span className="truncate">
                      {conns.connections.find((c) => c.account)?.account ?? "Connected"}
                    </span>
                  </span>
                  <Button
                    variant="ghost"
                    onClick={async () => {
                      await disconnect(id);
                      setConns(await listConnections(id));
                    }}
                  >
                    Disconnect
                  </Button>
                </div>
              ) : (
                <Button
                  onClick={async () => {
                    setConnectErr("");
                    try {
                      const { authorize_url } = await startConnect(id);
                      // Full navigation, not a popup: Google refuses to render
                      // its consent screen inside one from an unknown opener.
                      window.location.href = authorize_url;
                    } catch (e) {
                      setConnectErr(e instanceof Error ? e.message : String(e));
                    }
                  }}
                >
                  <UserPlus className="mr-1.5 h-4 w-4" />
                  Connect my account
                </Button>
              )}

              {connectErr && <p className="mt-2 text-sm text-failed">{connectErr}</p>}

              {conns.connections.length > 0 && (
                <div className="mt-4">
                  <p className="mb-1.5 text-xs font-medium uppercase tracking-wide text-fg-subtle">
                    Connected ({conns.connections.length})
                  </p>
                  <ul className="space-y-1">
                    {conns.connections.map((c) => (
                      <li key={c.member} className="flex items-baseline justify-between gap-3 text-sm">
                        <span className="truncate text-fg">{c.member}</span>
                        <span className="shrink-0 truncate text-xs text-fg-subtle">{c.account}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              )}
            </Panel>
          )}

          {(setup.methods?.length ?? 0) > 1 && (
            <Panel className="p-5">
              <h2 className="mb-1 text-h2">How to connect</h2>
              <p className="mb-3 text-sm text-fg-muted">
                These are not interchangeable — pick one and only its fields are asked for.
              </p>
              <div className="space-y-2">
                {setup.methods!.map((m) => (
                  <label
                    key={m.id}
                    className={
                      "flex cursor-pointer gap-3 rounded-[var(--radius-card)] border p-3 " +
                      (method === m.id ? "border-brand bg-surface-2" : "border-border")
                    }
                  >
                    <input
                      type="radio"
                      name="auth-method"
                      className="mt-1"
                      checked={method === m.id}
                      onChange={() => setMethod(m.id)}
                    />
                    <span className="min-w-0">
                      <span className="flex items-center gap-2 text-sm font-semibold text-fg">
                        {m.name}
                        {m.recommended && <Badge tone="brand">recommended</Badge>}
                      </span>
                      <span className="mt-0.5 block text-sm text-fg-muted">{m.summary}</span>
                    </span>
                  </label>
                ))}
              </div>
            </Panel>
          )}

          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Credentials</h2>
            <div className="space-y-3">
              {setup.fields
                // Only the chosen method's fields, plus the ones belonging to
                // no method. Showing all of them is what made this form ask for
                // a token AND an app id AND a private key at once.
                .filter((f) => !f.method || f.method === method)
                .map((f) => (
                <div key={f.key} className="space-y-1">
                  <Label htmlFor={f.key}>
                    {f.label}{f.required && " *"}
                    {f.set && <span className="ml-2 font-normal text-fg-subtle">· saved</span>}
                  </Label>
                  {f.multiline ? (
                    <>
                      {f.accept && (
                        <label className="mb-1.5 flex w-fit cursor-pointer items-center gap-1.5 rounded-[var(--radius-field)] border border-border px-2.5 py-1.5 text-xs text-fg-muted hover:border-brand">
                          <Upload className="h-3.5 w-3.5" />
                          {fileNames[f.key] ?? "Choose file…"}
                          <input
                            type="file"
                            accept={f.accept}
                            className="hidden"
                            onChange={async (e) => {
                              const file = e.target.files?.[0];
                              if (!file) return;
                              // Read in the browser and put the text straight
                              // into the field. There is no upload endpoint on
                              // purpose: a private key that never leaves as a
                              // file cannot be left behind in a temp directory
                              // or a request log.
                              const text = await file.text();
                              setFields((prev) => ({ ...prev, [f.key]: text.trim() }));
                              setFileNames((prev) => ({ ...prev, [f.key]: file.name }));
                            }}
                          />
                        </label>
                      )}
                      <textarea
                        id={f.key}
                        rows={5}
                        spellCheck={false}
                        placeholder={
                          f.set ? "leave blank to keep the saved value" : "-----BEGIN RSA PRIVATE KEY-----"
                        }
                        value={fields[f.key] ?? ""}
                        onChange={(e) => setFields((prev) => ({ ...prev, [f.key]: e.target.value }))}
                        className="w-full rounded-[var(--radius-field)] border border-border bg-surface px-3 py-2 font-mono text-xs leading-relaxed text-fg outline-none placeholder:text-fg-subtle focus:border-brand"
                      />
                    </>
                  ) : (
                    <Input
                      id={f.key}
                      type={f.type === "secret" ? "password" : "text"}
                      placeholder={f.set && f.type === "secret" ? "leave blank to keep the saved value" : f.placeholder}
                      value={fields[f.key] ?? ""}
                      onChange={(e) => setFields((prev) => ({ ...prev, [f.key]: e.target.value }))}
                    />
                  )}
                  {f.help && <p className="text-xs leading-relaxed text-fg-subtle">{f.help}</p>}
                </div>
              ))}
              <Button variant="primary" onClick={save} disabled={saving} className="w-full">
                {saving ? "Saving…" : "Save credentials"}
              </Button>
              {savedMsg && <p className="text-xs text-healthy">{savedMsg}</p>}
              {saveErr && <p className="text-xs leading-relaxed text-failed">{saveErr}</p>}
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

/** The chosen method's own instructions, falling back to the connector's. */
function stepsFor(setup: ConnectorSetup, method: string) {
  const m = setup.methods?.find((x) => x.id === method);
  if (m && m.steps.length > 0) return m.steps;
  return setup.steps;
}
