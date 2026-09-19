package dashboards

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/recipes"
	"gopkg.in/yaml.v3"
)

// A dashboard's refresh is a recipe like any other — one file in
// recipes.Dir(), picked up by the same watcher within a few seconds — so
// this package gets durable runs and a live schedule for free instead of
// building a second scheduler just for dashboards.

func recipeFileName(id string) string { return "dashboard-" + id + ".yaml" }
func recipePath(id string) string     { return filepath.Join(recipes.Dir(), recipeFileName(id)) }

// refreshFile mirrors just enough of recipes.Recipe's shape to marshal one
// (name, on.schedule, one ask step). Built and marshalled through yaml.v3
// rather than assembled as a string, so a title or brief containing a quote
// or colon comes out as valid YAML instead of a file the recipe watcher logs
// as broken and nobody reads.
type refreshFile struct {
	Name string `yaml:"name"`
	On   struct {
		Schedule string `yaml:"schedule"`
	} `yaml:"on"`
	Steps []map[string]string `yaml:"steps"`
}

// validateRefresh checks a refresh spec before anything is written. The cron
// half is left to recipes.Parse (writeRefreshRecipe below) since it already
// owns that syntax; duration gets its own check because "@every <this>" is
// this package's own translation and a bad duration should not surface as an
// opaque cron error about a schedule the caller never wrote.
func validateRefresh(r *Refresh) error {
	every := strings.TrimSpace(r.Every)
	cron := strings.TrimSpace(r.Cron)
	switch {
	case every == "" && cron == "":
		return fmt.Errorf("refresh needs \"every\" or \"cron\"")
	case every != "" && cron != "":
		return fmt.Errorf("refresh takes \"every\" or \"cron\", not both")
	}
	if strings.TrimSpace(r.Brief) == "" {
		return fmt.Errorf("refresh needs a \"brief\" describing what to refresh")
	}
	if every != "" {
		if _, err := time.ParseDuration(every); err != nil {
			return fmt.Errorf("refresh.every %q is not a duration like \"1h\" or \"30m\": %w", every, err)
		}
	}
	return nil
}

// schedule turns the tool's every/cron shorthand into what recipes.Parse's
// normaliseSchedule accepts. "@every" is cron's own descriptor — already
// understood by the same parser the scheduler runs — so a duration never
// needs converting to field syntax by hand.
func schedule(r *Refresh) string {
	if cron := strings.TrimSpace(r.Cron); cron != "" {
		return cron
	}
	return "@every " + strings.TrimSpace(r.Every)
}

// refreshPrompt is the ask step's whole instruction: what to refresh, the
// caller's brief for how, and the exact tool call to report it back with —
// spelled out because "update the data" with no mechanism is how an agent
// produces a good-looking answer that never reaches disk.
func refreshPrompt(id, title string, r *Refresh, dataNames []string) string {
	names := append([]string(nil), dataNames...)
	sort.Strings(names)
	files := "(none recorded yet — call the dashboard tool's 'get' action on this id to see its current data files)"
	if len(names) > 0 {
		files = strings.Join(names, ", ")
	}
	return fmt.Sprintf(
		"Refresh the data for dashboard %q (id %s).\n\n%s\n\n"+
			"Write each data file with the dashboard tool (action set_data, id %s, name, value) for the files: %s. "+
			"Do not change its HTML.",
		title, id, strings.TrimSpace(r.Brief), id, files)
}

// writeRefreshRecipe installs or replaces the recipe that keeps a
// dashboard's data current.
//
// Parsed with recipes.Parse before it touches disk, the same discipline
// RecipeTool applies to an agent's own recipes — except there is no agent to
// hand a parse error back to here, so a mistake in this generator would
// otherwise become a file the watcher logs once and silently never runs.
func writeRefreshRecipe(id, title string, r *Refresh, dataNames []string) error {
	f := refreshFile{Name: "dashboard-" + id}
	f.On.Schedule = schedule(r)
	f.Steps = []map[string]string{{"ask": refreshPrompt(id, title, r, dataNames)}}

	out, err := yaml.Marshal(f)
	if err != nil {
		return fmt.Errorf("dashboards: rendering the refresh recipe: %w", err)
	}
	if _, err := recipes.Parse(recipePath(id), out); err != nil {
		return fmt.Errorf("dashboards: the generated refresh recipe does not parse (this is a bug in dashboards, not your input): %w", err)
	}
	return atomicWrite(recipePath(id), out)
}

// removeRefreshRecipe deletes a dashboard's refresh recipe. Not existing is
// not an error: a dashboard that never had a refresh, or whose recipe was
// already removed, ends up in the same state either way.
func removeRefreshRecipe(id string) error {
	err := os.Remove(recipePath(id))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
