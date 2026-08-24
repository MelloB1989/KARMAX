import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { Plus, Workflow } from "lucide-react";
import { listRecipes } from "@/api/recipes";
import type { RecipeSummary } from "@/api/types";
import { Panel } from "@/components/ui/Panel";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { PageSpinner } from "@/components/ui/Spinner";
import { EmptyState } from "@/components/ui/EmptyState";
import { timeAgo } from "@/lib/utils";

export function RecipesPage() {
  const [recipes, setRecipes] = useState<RecipeSummary[] | null>(null);

  useEffect(() => { void listRecipes().then(setRecipes); }, []);

  if (recipes === null) return <PageSpinner />;

  return (
    <div>
      <div className="mb-5 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="text-h1">Recipes</h1>
          <p className="text-sm text-fg-muted">Declarative workflows your agents run on a schedule or event.</p>
        </div>
        <Link to="/recipes/new">
          <Button variant="primary"><Plus className="h-4 w-4" /> Build with AI</Button>
        </Link>
      </div>

      {recipes.length === 0 ? (
        <EmptyState icon={Workflow} title="No recipes yet" body="Describe one in plain English to get started." />
      ) : (
        <div className="space-y-2">
          {recipes.map((r) => (
            <Link key={r.name} to={`/recipes/${r.name}`}>
              <Panel className="flex items-center justify-between gap-4 p-4 transition-colors hover:border-border-glow">
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <h2 className="truncate font-mono text-sm font-semibold text-fg">{r.name}</h2>
                    <Badge tone={r.source === "generated" ? "brand" : "neutral"}>{r.source}</Badge>
                    {!r.enabled && <Badge tone="degraded">disabled — not live</Badge>}
                  </div>
                  <p className="mt-1 text-sm text-fg-muted">
                    {r.trigger_label} · {r.steps} step{r.steps === 1 ? "" : "s"}
                  </p>
                </div>
                <div className="shrink-0 text-right text-xs text-fg-subtle">
                  updated {timeAgo(r.updated_at)}
                </div>
              </Panel>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
