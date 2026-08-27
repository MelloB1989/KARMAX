import { useEffect, useState } from "react";
import { Building2, Info } from "lucide-react";
import { getOrganisation, saveOrganisation } from "@/api/organisation";
import type { OrgProfile } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Input, Label } from "@/components/ui/Input";
import { PageSpinner } from "@/components/ui/Spinner";
import { useSession } from "@/lib/session";
import { formatDateTime } from "@/lib/utils";

const CONTEXT_LIMIT = 8000;

export function OrganisationPage() {
  const { session } = useSession();
  const canEdit = session?.role === "admin";

  const [org, setOrg] = useState<OrgProfile | null>(null);
  const [draft, setDraft] = useState<OrgProfile | null>(null);
  const [saving, setSaving] = useState(false);
  const [msg, setMsg] = useState("");
  const [err, setErr] = useState("");

  useEffect(() => {
    void getOrganisation().then((o) => {
      setOrg(o);
      setDraft(o);
    });
  }, []);

  if (!org || !draft) return <PageSpinner />;

  const dirty = JSON.stringify({ ...org, briefing: "" }) !== JSON.stringify({ ...draft, briefing: "" });
  const over = draft.context.length > CONTEXT_LIMIT;

  const set = (k: keyof OrgProfile) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
    setDraft({ ...draft, [k]: e.target.value });

  const save = async () => {
    setSaving(true);
    setMsg("");
    setErr("");
    try {
      const saved = await saveOrganisation(draft);
      setOrg(saved);
      setDraft(saved);
      setMsg("Saved. Agents pick this up on their next message.");
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <div className="mb-5">
        <h1 className="text-h1">Organisation</h1>
        <p className="text-sm text-fg-muted">
          What every agent is told about the company before it answers anything.
        </p>
      </div>

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-3 text-h2">Details</h2>
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
              <div className="space-y-1">
                <Label htmlFor="name">Name</Label>
                <Input id="name" value={draft.name} onChange={set("name")} disabled={!canEdit}
                  placeholder="Zero Moblt" />
              </div>
              <div className="space-y-1">
                <Label htmlFor="domain">Domain</Label>
                <Input id="domain" value={draft.domain} onChange={set("domain")} disabled={!canEdit}
                  placeholder="zeromoblt.com" />
              </div>
              <div className="space-y-1 sm:col-span-2">
                <Label htmlFor="description">What the company does</Label>
                <Input id="description" value={draft.description} onChange={set("description")}
                  disabled={!canEdit} placeholder="We build AI agents for engineering teams." />
              </div>
              <div className="space-y-1">
                <Label htmlFor="timezone">Timezone</Label>
                <Input id="timezone" value={draft.timezone} onChange={set("timezone")} disabled={!canEdit}
                  placeholder="Asia/Kolkata" />
              </div>
            </div>
          </Panel>

          <Panel className="p-5">
            <div className="mb-1 flex items-baseline justify-between gap-3">
              <h2 className="text-h2">Context</h2>
              <span className={"font-mono text-xs " + (over ? "text-failed" : "text-fg-subtle")}>
                {draft.context.length.toLocaleString()} / {CONTEXT_LIMIT.toLocaleString()}
              </span>
            </div>
            <p className="mb-3 text-sm text-fg-muted">
              Conventions, vocabulary, who owns what — the things a new hire only has to be told
              once. This is added to every message an agent handles, so keep it to what changes
              the answer.
            </p>
            <textarea
              value={draft.context}
              onChange={set("context")}
              disabled={!canEdit}
              rows={14}
              placeholder={
                "Tickets live in YouTrack, not Jira. The Lamb project key is LAM.\n" +
                "\"oCrew\" is the product; \"KARMAX\" is the engine underneath it.\n" +
                "Deploys go out on Tuesdays. Ask before touching anything in prod."
              }
              className="w-full rounded-[var(--radius-field)] border border-border bg-surface px-3 py-2 font-mono text-[13px] leading-relaxed text-fg outline-none placeholder:text-fg-subtle focus:border-brand disabled:opacity-60"
            />
          </Panel>

          {canEdit && (
            <div className="flex items-center gap-3">
              <Button onClick={save} disabled={saving || !dirty || over}>
                {saving ? "Saving…" : "Save"}
              </Button>
              {dirty && !saving && <span className="text-sm text-fg-subtle">Unsaved changes</span>}
              {msg && <span className="text-sm text-healthy">{msg}</span>}
              {err && <span className="text-sm text-failed">{err}</span>}
            </div>
          )}
        </div>

        <div className="space-y-5">
          <Panel className="p-5">
            <h2 className="mb-1 text-h2">What the agents are told</h2>
            <p className="mb-3 text-sm text-fg-muted">
              Rendered by the server — this is the text itself, not an approximation of it.
            </p>
            {org.briefing ? (
              <pre className="max-h-96 overflow-auto whitespace-pre-wrap rounded-[var(--radius-card)] bg-surface-2 p-3 font-mono text-xs leading-relaxed text-fg">
                {org.briefing}
              </pre>
            ) : (
              <p className="flex items-start gap-2 text-sm text-fg-subtle">
                <Info className="mt-0.5 h-4 w-4 shrink-0" />
                Nothing yet — agents are told nothing about the company, and will ask questions a
                new hire would only ask once.
              </p>
            )}
            {org.updated_at && (
              <p className="mt-3 text-xs text-fg-subtle">
                Updated {formatDateTime(org.updated_at)}
                {org.updated_by && ` by ${org.updated_by}`}
              </p>
            )}
          </Panel>

          {!canEdit && (
            <Panel className="p-5">
              <p className="flex items-start gap-2 text-sm text-fg-muted">
                <Building2 className="mt-0.5 h-4 w-4 shrink-0" />
                Only an admin can change this, because it is added to every agent's prompt.
              </p>
            </Panel>
          )}
        </div>
      </div>
    </div>
  );
}
