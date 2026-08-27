import { useState } from "react";
import { useNavigate, Link } from "react-router-dom";
import { ArrowLeft, Play, Power, Save, ShieldCheck, Sparkles, TriangleAlert } from "lucide-react";
import { generateRecipe, saveRecipe, setRecipeEnabled } from "@/api/recipes";
import type { RecipeDraft } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Input";
import { CodeBlock } from "@/components/ui/CodeBlock";
import { Spinner } from "@/components/ui/Spinner";

const EXAMPLES = [
  "Every Monday morning, find Jira reviews that have been stuck for 2+ days and nudge #eng.",
  "When a PagerDuty incident closes, draft a postmortem outline and ask a senior dev to review it.",
  "Nightly, check for GitHub PRs with no reviewer after 24h and ping the author.",
];

export function RecipeBuilderPage() {
  const navigate = useNavigate();
  const [description, setDescription] = useState("");
  const [draft, setDraft] = useState<RecipeDraft | null>(null);
  const [generating, setGenerating] = useState(false);
  const [saving, setSaving] = useState<"save" | "enable" | null>(null);

  const generate = async () => {
    if (!description.trim()) return;
    setGenerating(true);
    setDraft(null);
    try {
      setDraft(await generateRecipe(description.trim()));
    } finally {
      setGenerating(false);
    }
  };

  const saveDisabled = async () => {
    if (!draft) return;
    setSaving("save");
    try {
      await saveRecipe(draft.name, draft.yaml);
      navigate(`/recipes/${draft.name}`);
    } finally {
      setSaving(null);
    }
  };

  const saveAndEnable = async () => {
    if (!draft) return;
    setSaving("enable");
    try {
      await saveRecipe(draft.name, draft.yaml);
      await setRecipeEnabled(draft.name, true);
      navigate(`/recipes/${draft.name}`);
    } finally {
      setSaving(null);
    }
  };

  return (
    <div>
      <Link to="/recipes" className="mb-4 inline-flex items-center gap-1.5 text-sm text-fg-muted hover:text-fg">
        <ArrowLeft className="h-3.5 w-3.5" /> Recipes
      </Link>

      <div className="mb-5">
        <h1 className="text-h1">Build a recipe</h1>
        <p className="text-sm text-fg-muted">Describe what you want in plain English. Nothing runs until you enable it.</p>
      </div>

      <Panel className="mb-5 p-5">
        <Textarea
          rows={3}
          placeholder="e.g. Every Monday morning, find Jira reviews stuck for 2+ days and nudge #eng…"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
        <div className="mt-2 flex flex-wrap gap-1.5">
          {EXAMPLES.map((ex) => (
            <button
              key={ex}
              type="button"
              onClick={() => setDescription(ex)}
              className="rounded-[var(--radius-chip)] border border-border bg-surface-2 px-2.5 py-1 text-xs text-fg-muted hover:text-fg"
            >
              {ex}
            </button>
          ))}
        </div>
        <div className="mt-3 flex justify-end">
          <Button variant="primary" onClick={generate} disabled={generating || !description.trim()}>
            {generating ? <Spinner className="text-white" /> : <Sparkles className="h-4 w-4" />}
            Generate
          </Button>
        </div>
      </Panel>

      {draft && (
        <>
          <div className="mb-5 flex items-start gap-2 rounded-[var(--radius-card)] border border-sun-500/60 bg-sun-300/20 px-4 py-3 text-sm text-sun-700">
            <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0" />
            <span>
              This is a <strong>draft</strong>. It is not saved and not live — nothing runs until you press Enable below.
            </span>
          </div>

          <div className="mb-5 grid grid-cols-1 gap-5 lg:grid-cols-2">
            <Panel className="p-5">
              <h2 className="mb-3 text-h2">Generated YAML</h2>
              <CodeBlock code={draft.yaml} />
              <p className="mt-2 text-xs text-fg-subtle">{draft.trigger_label}</p>
            </Panel>

            <div className="space-y-5">
              <Panel className="p-5">
                <h2 className="mb-3 flex items-center gap-2 text-h2">
                  <Play className="h-4 w-4 text-fg-subtle" /> Dry-run trace
                </h2>
                <p className="mb-2 text-xs text-fg-subtle">
                  What this would do, rehearsed against fake data — nothing here actually happened.
                </p>
                <ol className="space-y-1 font-mono text-xs text-fg-muted">
                  {draft.dry_run.map((line, i) => (
                    <li key={i}>{i + 1}. {line}</li>
                  ))}
                </ol>
              </Panel>

              <Panel className="p-5">
                <h2 className="mb-3 flex items-center gap-2 text-h2">
                  <ShieldCheck className="h-4 w-4 text-fg-subtle" /> Permission summary
                </h2>
                <ul className="space-y-1.5">
                  {draft.permissions.map((p, i) => (
                    <li key={i} className="flex items-start gap-2 text-sm text-fg">
                      <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-brand-500" />
                      {p}
                    </li>
                  ))}
                </ul>
              </Panel>
            </div>
          </div>

          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={saveDisabled} disabled={saving !== null}>
              <Save className="h-4 w-4" /> Save without enabling
            </Button>
            <Button variant="primary" onClick={saveAndEnable} disabled={saving !== null}>
              <Power className="h-4 w-4" /> Enable
            </Button>
          </div>
        </>
      )}
    </div>
  );
}
