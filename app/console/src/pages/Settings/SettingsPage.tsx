import { useEffect, useState } from "react";
import { KeyRound, RefreshCw, Users, Container } from "lucide-react";
import { getSettings, saveModelProvider, saveSandboxToken, setOrgRole, syncDirectory } from "@/api/settings";
import type { OrgRoleName, Settings } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Input, Label, Select } from "@/components/ui/Input";
import { Badge } from "@/components/ui/Badge";
import { PageSpinner } from "@/components/ui/Spinner";
import { formatDateTime, timeAgo } from "@/lib/utils";

const ROLES: OrgRoleName[] = ["admin", "operator", "viewer"];

export function SettingsPage() {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [keyDrafts, setKeyDrafts] = useState<Record<string, string>>({});
  const [tokenDraft, setTokenDraft] = useState("");
  const [syncing, setSyncing] = useState(false);
  const [savingId, setSavingId] = useState<string | null>(null);

  const load = () => void getSettings().then(setSettings);
  useEffect(load, []);

  if (settings === null) return <PageSpinner />;

  const saveProvider = async (id: string, baseUrl: string) => {
    setSavingId(id);
    try {
      await saveModelProvider(id, { base_url: baseUrl, api_key: keyDrafts[id] || undefined });
      setKeyDrafts((p) => ({ ...p, [id]: "" }));
      load();
    } finally {
      setSavingId(null);
    }
  };

  const saveSandbox = async () => {
    if (!tokenDraft.trim()) return;
    setSavingId("sandbox");
    try {
      await saveSandboxToken(tokenDraft.trim());
      setTokenDraft("");
      load();
    } finally {
      setSavingId(null);
    }
  };

  const sync = async () => {
    setSyncing(true);
    try {
      await syncDirectory();
      load();
    } finally {
      setSyncing(false);
    }
  };

  const changeRole = async (member: string, role: OrgRoleName) => {
    await setOrgRole(member, role);
    load();
  };

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-h1">Settings</h1>
        <p className="text-sm text-fg-muted">Model credentials, sandbox access, directory sync and org roles.</p>
      </div>

      <Panel className="p-5">
        <h2 className="mb-3 flex items-center gap-2 text-h2">
          <KeyRound className="h-4 w-4 text-fg-subtle" /> Model credentials
        </h2>
        <p className="mb-4 text-xs text-fg-subtle">Bring your own key — KARMAX calls these providers directly, on your account.</p>
        <div className="space-y-4">
          {settings.model_providers.map((p) => (
            <div key={p.id} className="rounded-[var(--radius-card)] border border-border p-4">
              <div className="mb-2 flex items-center justify-between">
                <span className="text-sm font-semibold text-fg">{p.name}</span>
                <Badge tone={p.has_key ? "healthy" : "unknown"}>
                  {p.has_key ? `key ····${p.key_last4}` : "no key set"}
                </Badge>
              </div>
              <div className="grid grid-cols-1 gap-2 sm:grid-cols-2">
                <div className="space-y-1">
                  <Label htmlFor={`${p.id}-base`}>Base URL</Label>
                  <Input id={`${p.id}-base`} defaultValue={p.base_url} onChange={(e) => (p.base_url = e.target.value)} />
                </div>
                <div className="space-y-1">
                  <Label htmlFor={`${p.id}-key`}>API key</Label>
                  <Input
                    id={`${p.id}-key`}
                    type="password"
                    placeholder={p.has_key ? "leave blank to keep current key" : "sk-…"}
                    value={keyDrafts[p.id] ?? ""}
                    onChange={(e) => setKeyDrafts((prev) => ({ ...prev, [p.id]: e.target.value }))}
                  />
                </div>
              </div>
              <div className="mt-3 flex justify-end">
                <Button size="sm" onClick={() => saveProvider(p.id, p.base_url)} disabled={savingId === p.id}>
                  {savingId === p.id ? "Saving…" : "Save"}
                </Button>
              </div>
            </div>
          ))}
        </div>
      </Panel>

      <Panel className="p-5">
        <h2 className="mb-3 flex items-center gap-2 text-h2">
          <Container className="h-4 w-4 text-fg-subtle" /> Claude Code sandbox token
        </h2>
        <p className="mb-4 text-xs text-fg-subtle">
          Handed into every sandbox container so agents can push branches and open PRs on your behalf.
        </p>
        <div className="flex items-end gap-2">
          <div className="flex-1 space-y-1">
            <Label htmlFor="sandbox-token">
              {settings.sandbox_token.configured
                ? `Current token ends ····${settings.sandbox_token.last4} (rotated ${timeAgo(settings.sandbox_token.updated_at)})`
                : "No token configured"}
            </Label>
            <Input
              id="sandbox-token"
              type="password"
              placeholder="ghp_… or a scoped Claude Code token"
              value={tokenDraft}
              onChange={(e) => setTokenDraft(e.target.value)}
            />
          </div>
          <Button onClick={saveSandbox} disabled={savingId === "sandbox" || !tokenDraft.trim()}>
            {savingId === "sandbox" ? "Saving…" : "Save"}
          </Button>
        </div>
      </Panel>

      <Panel className="p-5">
        <h2 className="mb-3 flex items-center gap-2 text-h2">
          <RefreshCw className="h-4 w-4 text-fg-subtle" /> Directory sync
        </h2>
        <div className="flex items-center justify-between">
          <div className="text-sm text-fg-muted">
            <p>{settings.directory.members_synced} members from {settings.directory.sources.join(", ")}</p>
            <p className="text-xs text-fg-subtle">
              last synced {settings.directory.last_synced_at ? formatDateTime(settings.directory.last_synced_at) : "never"}
            </p>
          </div>
          <Button variant="secondary" onClick={sync} disabled={syncing}>
            {syncing ? "Syncing…" : "Sync now"}
          </Button>
        </div>
      </Panel>

      <Panel className="p-5">
        <h2 className="mb-3 flex items-center gap-2 text-h2">
          <Users className="h-4 w-4 text-fg-subtle" /> Org roles
        </h2>
        <table className="w-full border-collapse text-sm">
          <thead>
            <tr className="border-b border-border text-left text-xs font-semibold uppercase tracking-wide text-fg-subtle">
              <th className="py-2">Member</th>
              <th className="py-2">Source</th>
              <th className="py-2">Role</th>
            </tr>
          </thead>
          <tbody>
            {settings.roles.map((r) => (
              <tr key={r.member} className="border-b border-border last:border-0">
                <td className="py-2 text-fg">{r.name}</td>
                <td className="py-2 text-fg-subtle">{r.source}</td>
                <td className="py-2">
                  <Select
                    value={r.role}
                    onChange={(e) => changeRole(r.member, e.target.value as OrgRoleName)}
                    className="h-8 w-32 py-0 text-sm"
                  >
                    {ROLES.map((role) => <option key={role} value={role}>{role}</option>)}
                  </Select>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>
    </div>
  );
}
