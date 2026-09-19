package runtime

import (
	"sort"

	"github.com/MelloB1989/karmax/internal/api"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/wasmloop"
)

// listLoopInfos is what GET /api/loops answers — every tier the runtime
// knows about, not only what happens to be scheduled right now. Before this,
// a disabled loop simply vanished from the listing instead of reading "off",
// and the whole recipe and prompt tiers were absent because they don't live
// in rt.loopkitLoops at all (see loophost.go's startRecipes and this file's
// callers for where each tier actually runs).
//
// Built as a map keyed by name so a loop that is both currently active AND
// separately discoverable on disk (the common case) is reported once.
func (rt *KarmaxRuntime) listLoopInfos() []api.LoopInfo {
	byName := map[string]api.LoopInfo{}

	installedWorkflows := map[string]bool{}
	in := &wasmloop.Installer{Dir: wasmloop.Dir()}
	if entries, err := in.Installed(); err == nil {
		for _, e := range entries {
			installedWorkflows[e.Name] = true
			if _, active := rt.loopkitLoops[e.Name]; !active {
				// Installed but not scheduled means the operator disabled it
				// (see startWasmLoops, which skips a disabled entry entirely
				// rather than loading it inert).
				byName[e.Name] = api.LoopInfo{Name: e.Name, Kind: "workflow", Enabled: e.Enabled}
			}
		}
	}

	// Compiled-in and signed-workflow loops both live in rt.loopkitLoops,
	// distinguished only by whether the lockfile above claims the name.
	for _, l := range rt.loopkitLoops {
		kind := "compiled"
		if installedWorkflows[l.Name] {
			kind = "workflow"
		}
		byName[l.Name] = api.LoopInfo{
			Name: l.Name, Description: l.Description, Schedule: l.Schedule.CronExpr(),
			Webhook: l.Webhook, Events: l.Events, Kind: kind, Enabled: true,
		}
	}

	// Recipes: rt.recipeLoops holds only the ones currently scheduled (author
	// enabled AND not operator-disabled — see loophost.go's applyRecipes), so
	// a disabled one is read straight off disk instead, same as the workflow
	// case above.
	disabled := loopinstall.LoadDisabledLoops()
	rt.recipeMu.RLock()
	active := make(map[string]*recipes.Recipe, len(rt.recipeLoops))
	for name, r := range rt.recipeLoops {
		active[name] = r
	}
	rt.recipeMu.RUnlock()
	for name, r := range active {
		byName[name] = recipeLoopInfo(r, true)
	}
	for _, l := range recipes.LoadAll(recipes.Dir()) {
		if l.Recipe == nil {
			continue
		}
		if _, done := byName[l.Recipe.Name]; done {
			continue
		}
		byName[l.Recipe.Name] = recipeLoopInfo(l.Recipe, l.Recipe.IsEnabled() && !disabled[l.Recipe.Name])
	}

	// Prompt loops (karmax.yaml's declarative `loops:`) fire straight at an
	// agent and never touch rt.loopkitLoops or rt.recipeLoops at all — see
	// the scheduler.AddJob loop just above startLoopkitLoops in runtime.go.
	for _, lc := range rt.cfg.Loops {
		if _, done := byName[lc.Name]; done {
			continue
		}
		byName[lc.Name] = api.LoopInfo{
			// firstLineOf is taskrunner.go's helper — no length cap, but a
			// loop prompt is written by an operator, not fetched off the
			// network, so an unusually long first line is their own choice.
			Name: lc.Name, Description: firstLineOf(lc.Prompt), Schedule: lc.Cron,
			Kind: "prompt", Enabled: lc.Enabled == nil || *lc.Enabled,
		}
	}

	out := make([]api.LoopInfo, 0, len(byName))
	for _, li := range byName {
		out = append(out, li)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func recipeLoopInfo(r *recipes.Recipe, enabled bool) api.LoopInfo {
	li := api.LoopInfo{
		Name: r.Name, Description: recipes.Summary(r), Kind: "recipe", Enabled: enabled,
		Schedule: r.On.Schedule, Webhook: r.On.Webhook,
	}
	if r.On.Event != "" {
		li.Events = []string{r.On.Event}
	}
	return li
}
