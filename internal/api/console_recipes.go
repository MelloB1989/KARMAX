package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/recipegen"
	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/google/uuid"
)

// Recipes: the workflow files on disk, as the console sees them.
//
// Two rules hold throughout, and both exist because a recipe is executable:
//
//  1. A recipe is ALWAYS born disabled, whatever the YAML or the caller says.
//     Generating a workflow from a sentence and having it start firing on a
//     cron in the same breath is not a feature.
//  2. Enabling is its own explicit call, never a side effect of saving.

// recipeNamePattern is what may become a filename. Anything else is rejected
// rather than sanitised — quietly writing "a/../b" to "a-b" hides a traversal
// attempt instead of reporting it.
var recipeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type recipeTrigger struct {
	Event    string `json:"event"`
	Schedule string `json:"schedule"`
	Webhook  string `json:"webhook"`
	Manual   bool   `json:"manual"`
}

type recipeSummary struct {
	Name         string        `json:"name"`
	Source       string        `json:"source"`
	Enabled      bool          `json:"enabled"`
	Trigger      recipeTrigger `json:"trigger"`
	TriggerLabel string        `json:"trigger_label"`
	Steps        int           `json:"steps"`
	UpdatedAt    string        `json:"updated_at"`
}

type recipeDetail struct {
	recipeSummary
	YAML        string   `json:"yaml"`
	Permissions []string `json:"permissions"`
}

// recipePath resolves a recipe name to its file, refusing anything that is not
// a plain name.
func recipePath(name string) (string, error) {
	if !recipeNamePattern.MatchString(name) {
		return "", errors.New("a recipe name must be lowercase letters, digits and dashes")
	}
	return filepath.Join(recipes.Dir(), name+".yaml"), nil
}

// triggerLabel renders a trigger the way a human would say it, server-side, so
// the console never has to re-implement cron parsing.
func triggerLabel(t recipes.Trigger) string {
	switch {
	case t.Schedule != "":
		return cronLabel(t.Schedule)
	case t.Event != "":
		return "on " + t.Event
	case t.Webhook != "":
		return "on webhook " + t.Webhook
	case t.Manual:
		return "manual"
	default:
		return "no trigger"
	}
}

// cronLabel turns the common cron shapes into English and falls back to the
// expression itself. A wrong plain-English reading would be worse than none,
// so anything unrecognised is shown verbatim.
func cronLabel(expr string) string {
	f := strings.Fields(expr)
	// Six fields is robfig's seconds-first form, five is standard.
	if len(f) == 6 {
		f = f[1:]
	}
	if len(f) != 5 {
		return expr
	}
	min, hour, dom, mon, dow := f[0], f[1], f[2], f[3], f[4]

	numeric := func(s string) bool {
		if s == "" {
			return false
		}
		for _, c := range s {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	if !numeric(min) || !numeric(hour) {
		return expr
	}
	at := fmt.Sprintf("%02s:%02s", hour, min)

	days := map[string]string{
		"0": "Sunday", "1": "Monday", "2": "Tuesday", "3": "Wednesday",
		"4": "Thursday", "5": "Friday", "6": "Saturday", "7": "Sunday",
	}
	switch {
	case dom == "*" && mon == "*" && dow == "*":
		return "daily at " + at
	case dom == "*" && mon == "*" && days[dow] != "":
		return "every " + days[dow] + " at " + at
	case mon == "*" && dow == "*" && numeric(dom):
		return "monthly on day " + dom + " at " + at
	default:
		return expr
	}
}

// builtinNames is the set of recipes KARMAX ships, used to label a recipe's
// source so the console can tell "we shipped this" from "someone made this".
func builtinNames() map[string]bool {
	out := map[string]bool{}
	for _, n := range recipes.BuiltinNames() {
		out[n] = true
	}
	return out
}

func summariseRecipe(r *recipes.Recipe, builtins map[string]bool) recipeSummary {
	source := "generated"
	if builtins[r.Name] {
		source = "builtin"
	}
	updated := ""
	if r.Path != "" {
		if st, err := os.Stat(r.Path); err == nil {
			updated = rfc3339(st.ModTime())
		}
	}
	return recipeSummary{
		Name:    r.Name,
		Source:  source,
		Enabled: r.IsEnabled(),
		Trigger: recipeTrigger{
			Event: r.On.Event, Schedule: r.On.Schedule,
			Webhook: r.On.Webhook, Manual: r.On.Manual,
		},
		TriggerLabel: triggerLabel(r.On),
		Steps:        len(r.Steps),
		UpdatedAt:    updated,
	}
}

// detailFor reads a recipe from disk and renders it in full.
func detailFor(name string) (recipeDetail, error) {
	path, err := recipePath(name)
	if err != nil {
		return recipeDetail{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return recipeDetail{}, err
	}
	r, err := recipes.Parse(path, data)
	if err != nil {
		return recipeDetail{}, err
	}
	perms := recipes.Describe(r)
	if perms == nil {
		perms = []string{}
	}
	return recipeDetail{
		recipeSummary: summariseRecipe(r, builtinNames()),
		YAML:          string(data),
		Permissions:   perms,
	}, nil
}

func (s *ConsoleServer) handleConsoleRecipes(w http.ResponseWriter, r *http.Request) {
	loaded := recipes.LoadAll(recipes.Dir())
	builtins := builtinNames()

	out := make([]recipeSummary, 0, len(loaded))
	for _, l := range loaded {
		// A recipe that does not parse is still a file the operator has, and
		// hiding it makes a broken workflow look like a missing one.
		if l.Recipe == nil {
			name := strings.TrimSuffix(filepath.Base(l.Path), ".yaml")
			out = append(out, recipeSummary{
				Name: name, Source: "generated", Enabled: false,
				TriggerLabel: "does not parse", Steps: 0,
			})
			continue
		}
		out = append(out, summariseRecipe(l.Recipe, builtins))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"recipes": out})
}

func (s *ConsoleServer) handleConsoleRecipeDetail(w http.ResponseWriter, r *http.Request) {
	d, err := detailFor(r.PathValue("name"))
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such recipe"})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *ConsoleServer) handleRecipeGenerate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "describe what the workflow should do"})
		return
	}
	if s.generate == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]any{"error": "no model is configured for recipe generation on this server"})
		return
	}

	draft, err := recipegen.Generate(r.Context(), recipegen.Request{Description: req.Description}, s.generate)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}

	parsed, err := recipes.Parse("generated.yaml", []byte(draft.YAML))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	perms := recipes.Describe(parsed)
	if perms == nil {
		perms = []string{}
	}
	warnings := draft.Warnings
	if warnings == nil {
		warnings = []string{}
	}

	s.audit(r, "human", consoleUser(r).Member, "console.recipe.generate", parsed.Name, "", req.Description)
	writeJSON(w, http.StatusOK, map[string]any{
		"draft_id":      uuid.New().String(),
		"name":          parsed.Name,
		"yaml":          draft.YAML,
		"trigger_label": triggerLabel(parsed.On),
		"dry_run":       splitLines(draft.DryRun),
		"permissions":   perms,
		"warnings":      warnings,
	})
}

// splitLines turns a report into one entry per action, dropping the numbering
// the console adds back itself.
func splitLines(s string) []string {
	out := []string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i := strings.IndexByte(line, ' '); i > 0 && strings.HasSuffix(line[:i], ".") {
			if _, err := fmt.Sscanf(line[:i], "%d.", new(int)); err == nil {
				line = strings.TrimSpace(line[i+1:])
			}
		}
		out = append(out, line)
	}
	return out
}

// forceDisabled rewrites a recipe's `enabled:` key to false, adding one if the
// YAML has none. This is applied to every save: a recipe must never be born
// live, and trusting the submitted YAML to say so is trusting the wrong side.
func forceDisabled(yaml string) string {
	lines := strings.Split(yaml, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "enabled:") {
			lines[i] = "enabled: false"
			return strings.Join(lines, "\n")
		}
	}
	// Insert after `name:` so the file still reads naturally.
	for i, l := range lines {
		if strings.HasPrefix(l, "name:") {
			rest := append([]string{"enabled: false"}, lines[i+1:]...)
			return strings.Join(append(lines[:i+1], rest...), "\n")
		}
	}
	return "enabled: false\n" + yaml
}

// setEnabled rewrites the `enabled:` key to the given value.
func setEnabled(yaml string, on bool) string {
	val := "false"
	if on {
		val = "true"
	}
	lines := strings.Split(yaml, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "enabled:") {
			lines[i] = "enabled: " + val
			return strings.Join(lines, "\n")
		}
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "name:") {
			rest := append([]string{"enabled: " + val}, lines[i+1:]...)
			return strings.Join(append(lines[:i+1], rest...), "\n")
		}
	}
	return "enabled: " + val + "\n" + yaml
}

func (s *ConsoleServer) handleRecipeCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		YAML string `json:"yaml"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	path, err := recipePath(req.Name)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if _, err := os.Stat(path); err == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a recipe with that name already exists"})
		return
	}

	body := forceDisabled(req.YAML)
	if _, err := recipes.Parse(path, []byte(body)); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(recipes.Dir(), 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", consoleUser(r).Member, "console.recipe.create", req.Name, "", "saved disabled")
	s.respondRecipe(w, req.Name)
}

func (s *ConsoleServer) handleRecipeUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	path, err := recipePath(name)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	existing, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such recipe"})
		return
	}

	var req struct {
		YAML string `json:"yaml"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	// An edit preserves the enabled state the file already had. Taking it from
	// the submitted YAML would let a text edit silently switch a workflow on.
	wasEnabled := false
	if prev, perr := recipes.Parse(path, existing); perr == nil {
		wasEnabled = prev.IsEnabled()
	}
	body := setEnabled(req.YAML, wasEnabled)

	if _, err := recipes.Parse(path, []byte(body)); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	s.audit(r, "human", consoleUser(r).Member, "console.recipe.update", name, "", "")
	s.respondRecipe(w, name)
}

func (s *ConsoleServer) handleRecipeEnable(w http.ResponseWriter, r *http.Request) {
	s.flipRecipe(w, r, true)
}

func (s *ConsoleServer) handleRecipeDisable(w http.ResponseWriter, r *http.Request) {
	s.flipRecipe(w, r, false)
}

func (s *ConsoleServer) flipRecipe(w http.ResponseWriter, r *http.Request, on bool) {
	name := r.PathValue("name")
	path, err := recipePath(name)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "no such recipe"})
		return
	}

	body := setEnabled(string(data), on)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	verb := "console.recipe.disable"
	if on {
		verb = "console.recipe.enable"
	}
	s.audit(r, "human", consoleUser(r).Member, verb, name, "", "")
	s.respondRecipe(w, name)
}

func (s *ConsoleServer) respondRecipe(w http.ResponseWriter, name string) {
	d, err := detailFor(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}
