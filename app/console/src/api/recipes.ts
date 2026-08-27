import { USE_MOCK } from "./config";
import { get, post, put } from "./client";
import type { RecipeDetail, RecipeDraft, RecipeSummary } from "./types";
import { RECIPES, makeDraft } from "./mock/data";
import { delay } from "./mock/util";

// Re-derived from RECIPES on every call (not a cached export) so a recipe
// created or enabled during this session shows up without a reload.
export async function listRecipes(): Promise<RecipeSummary[]> {
  if (USE_MOCK) {
    return delay(RECIPES.map(({ yaml: _yaml, permissions: _permissions, ...rest }) => rest));
  }
  return get<{ recipes: RecipeSummary[] }>("/api/console/recipes").then((r) => r.recipes);
}

export async function getRecipe(name: string): Promise<RecipeDetail | null> {
  if (USE_MOCK) return delay(RECIPES.find((r) => r.name === name) ?? null);
  return get<RecipeDetail>(`/api/console/recipes/${encodeURIComponent(name)}`);
}

/** Asks the compiler to turn a description into a recipe. Nothing is saved or
 * enabled by this call — see enableRecipe. */
export async function generateRecipe(description: string): Promise<RecipeDraft> {
  if (USE_MOCK) return delay(makeDraft(description), 900);
  return post<RecipeDraft>("/api/console/recipes/generate", { description });
}

/** Persists a draft (or a hand-edited YAML) as a DISABLED recipe. */
export async function saveRecipe(name: string, yaml: string): Promise<RecipeDetail> {
  if (USE_MOCK) {
    const rec: RecipeDetail = {
      name, source: "generated", enabled: false,
      trigger: { event: "", schedule: "", webhook: "", manual: true },
      trigger_label: "manual", steps: (yaml.match(/^\s*-\s+\w/gm) ?? []).length,
      updated_at: new Date().toISOString(), yaml,
      permissions: ["review the YAML to see exactly what this would do"],
    };
    RECIPES.unshift(rec);
    return delay(rec);
  }
  return post<RecipeDetail>("/api/console/recipes", { name, yaml });
}

export async function setRecipeEnabled(name: string, enabled: boolean): Promise<RecipeDetail> {
  if (USE_MOCK) {
    const rec = RECIPES.find((r) => r.name === name);
    if (!rec) throw new Error("no such recipe: " + name);
    rec.enabled = enabled;
    rec.updated_at = new Date().toISOString();
    return delay(rec);
  }
  return post<RecipeDetail>(`/api/console/recipes/${encodeURIComponent(name)}/${enabled ? "enable" : "disable"}`);
}

export async function updateRecipeYaml(name: string, yaml: string): Promise<RecipeDetail> {
  if (USE_MOCK) {
    const rec = RECIPES.find((r) => r.name === name);
    if (!rec) throw new Error("no such recipe: " + name);
    rec.yaml = yaml;
    rec.updated_at = new Date().toISOString();
    return delay(rec);
  }
  return put<RecipeDetail>(`/api/console/recipes/${encodeURIComponent(name)}`, { yaml });
}
