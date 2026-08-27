import { useEffect, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { ArrowLeft, Power, ShieldCheck, TriangleAlert, Workflow } from "lucide-react";
import { getRecipe, setRecipeEnabled } from "@/api/recipes";
import type { RecipeDetail as RecipeDetailType } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { CodeBlock } from "@/components/ui/CodeBlock";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { timeAgo } from "@/lib/utils";

export function RecipeDetailPage() {
  const { name = "" } = useParams();
  const [recipe, setRecipe] = useState<RecipeDetailType | null | undefined>(undefined);
  const [busy, setBusy] = useState(false);

  useEffect(() => { void getRecipe(name).then(setRecipe); }, [name]);

  if (recipe === undefined) return <PageSpinner />;
  if (recipe === null) return <EmptyState icon={Workflow} title="Recipe not found" body={`No recipe named "${name}".`} />;

  const toggle = async () => {
    setBusy(true);
    try {
      setRecipe(await setRecipeEnabled(recipe.name, !recipe.enabled));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <Link to="/recipes" className="mb-4 inline-flex items-center gap-1.5 text-sm text-fg-muted hover:text-fg">
        <ArrowLeft className="h-3.5 w-3.5" /> Recipes
      </Link>

      <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="mb-1 flex items-center gap-2">
            <h1 className="font-mono text-h1">{recipe.name}</h1>
            <Badge tone={recipe.source === "generated" ? "brand" : "neutral"}>{recipe.source}</Badge>
          </div>
          <p className="text-sm text-fg-muted">{recipe.trigger_label} · updated {timeAgo(recipe.updated_at)}</p>
        </div>
        <Button
          variant={recipe.enabled ? "outline" : "primary"}
          onClick={toggle}
          disabled={busy}
        >
          <Power className="h-4 w-4" />
          {recipe.enabled ? "Disable" : "Enable"}
        </Button>
      </div>

      {!recipe.enabled && (
        <div className="mb-5 flex items-start gap-2 rounded-[var(--radius-card)] border border-sun-500/60 bg-sun-300/20 px-4 py-3 text-sm text-sun-700">
          <TriangleAlert className="mt-0.5 h-4 w-4 shrink-0" />
          <span>
            This recipe is <strong>not live</strong>. It will not run on its trigger until a human presses Enable.
          </span>
        </div>
      )}

      <div className="grid grid-cols-1 gap-5 lg:grid-cols-[1fr_320px]">
        <Panel className="p-5">
          <h2 className="mb-3 text-h2">YAML</h2>
          <CodeBlock code={recipe.yaml} />
        </Panel>

        <Panel className="p-5">
          <h2 className="mb-3 flex items-center gap-2 text-h2">
            <ShieldCheck className="h-4 w-4 text-fg-subtle" /> What this does
          </h2>
          <ul className="space-y-1.5">
            {recipe.permissions.map((p, i) => (
              <li key={i} className="flex items-start gap-2 text-sm text-fg">
                <span className="mt-1.5 h-1 w-1 shrink-0 rounded-full bg-brand-500" />
                {p}
              </li>
            ))}
          </ul>
        </Panel>
      </div>
    </div>
  );
}
