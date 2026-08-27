package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/loopinstall"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/scheduler"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// runtimeWithScheduler is the smallest runtime applyRecipes will run against.
func runtimeWithScheduler(t *testing.T) *KarmaxRuntime {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "r.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	b := bus.NewLog(db, store.DefaultWorkspace, zap.NewNop())
	return &KarmaxRuntime{
		store:        db,
		log:          zap.NewNop(),
		scheduler:    scheduler.New(db, b, zap.NewNop()),
		recipeLoops:  map[string]*recipes.Recipe{},
		loopkitLoops: map[string]loopkit.Loop{},
	}
}

func loadedRecipe(name, cron string) recipes.Loaded {
	return recipes.Loaded{
		Path: name + ".yaml",
		Recipe: &recipes.Recipe{
			Name: name,
			Path: name + ".yaml",
			On:   recipes.Trigger{Schedule: cron},
		},
	}
}

func scheduled(rt *KarmaxRuntime, id string) bool {
	for _, j := range rt.scheduler.ListJobs() {
		if j.ID == id {
			return true
		}
	}
	return false
}

// `karmax loops disable` used to be a no-op for recipes: it wrote the name to
// loops-disabled.txt, `loops list` printed "disabled", and applyRecipes went on
// scheduling the recipe with Enabled: true. The command reported success and
// the recipe still fired on its cron.
func TestADisabledRecipeIsNotScheduled(t *testing.T) {
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())
	if err := loopinstall.SetLoopDisabled("noisy", true); err != nil {
		t.Fatal(err)
	}

	rt := runtimeWithScheduler(t)
	rt.applyRecipes(context.Background(), []recipes.Loaded{
		loadedRecipe("noisy", "0 0 9 * * *"),
		loadedRecipe("wanted", "0 0 10 * * *"),
	})

	if _, loaded := rt.recipeLoops["noisy"]; loaded {
		t.Error("a disabled recipe was still registered")
	}
	if scheduled(rt, "recipe:noisy") {
		t.Error("a disabled recipe still has a scheduler job — this is the bug")
	}

	// The operator disabled one recipe, not the directory.
	if _, loaded := rt.recipeLoops["wanted"]; !loaded {
		t.Error("an enabled recipe was not registered")
	}
	if !scheduled(rt, "recipe:wanted") {
		t.Error("an enabled recipe has no scheduler job")
	}
}

// Disabling something already running has to take its job away, not just stop
// re-adding it — otherwise the schedule survives in the store and fires on the
// next boot.
func TestDisablingARunningRecipeRemovesItsJob(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("KARMAX_DATA_DIR", dataDir)

	rt := runtimeWithScheduler(t)
	rt.applyRecipes(context.Background(), []recipes.Loaded{loadedRecipe("noisy", "0 0 9 * * *")})
	if !scheduled(rt, "recipe:noisy") {
		t.Fatal("recipe was not scheduled to begin with")
	}

	if err := loopinstall.SetLoopDisabled("noisy", true); err != nil {
		t.Fatal(err)
	}
	rt.applyRecipes(context.Background(), []recipes.Loaded{loadedRecipe("noisy", "0 0 9 * * *")})

	if scheduled(rt, "recipe:noisy") {
		t.Error("job survived the recipe being disabled")
	}
	if _, loaded := rt.recipeLoops["noisy"]; loaded {
		t.Error("recipe is still registered after being disabled")
	}
}

// The YAML `enabled:` key is the AUTHOR's switch and stays independent of the
// operator's list — recipes.Valid drops it before applyRecipes ever sees it.
func TestAnAuthorDisabledRecipeIsAlsoNotScheduled(t *testing.T) {
	t.Setenv("KARMAX_DATA_DIR", t.TempDir())

	off := false
	l := loadedRecipe("shipped-off", "0 0 9 * * *")
	l.Recipe.Enabled = &off

	rt := runtimeWithScheduler(t)
	rt.applyRecipes(context.Background(), []recipes.Loaded{l})

	if scheduled(rt, "recipe:shipped-off") {
		t.Error("a recipe carrying enabled:false was scheduled")
	}
}
