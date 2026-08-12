package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/recipes"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Writing its own workflows.
//
// The substrate was already here — a recipe format, a directory, a watcher that
// picks up changes within three seconds, and a loader that gives every recipe
// the durable-run machinery for free. What was missing was any way for the agent
// to use it: no tool wrote a recipe, nothing told it the directory existed, and
// a parse error went to the daemon log where the agent could never see it.
//
// That last part matters most. Writing a file it cannot validate is how an agent
// produces confident nonsense; the check has to answer back.

// RecipeTool lets the agent read, validate and write its own recipes.
type RecipeTool struct{}

func (t *RecipeTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "recipe.write",
		Description: "Create, inspect or validate your own recurring workflows (recipes). " +
			"A recipe is YAML with a trigger (schedule/event/manual) and numbered steps, and it starts running within seconds of being written. " +
			"Use action 'check' to validate BEFORE 'write' — it returns the exact line and a suggested fix. " +
			"Use 'list' to see what already exists and 'read' to see how one is written. " +
			"Write a recipe when the operator asks for something recurring; use scheduler.add for a one-off.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {"type": "string", "enum": ["list", "read", "check", "write", "delete"], "description": "'check' validates without saving; 'write' validates AND saves."},
				"name": {"type": "string", "description": "Recipe name, e.g. 'morning-brief'. Becomes <name>.yaml."},
				"content": {"type": "string", "description": "For 'check' and 'write': the full recipe YAML."}
			},
			"required": ["action"]
		}`),
	}
}

func (t *RecipeTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	action := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", input["action"])))
	name := sanitizeRecipeName(fmt.Sprintf("%v", input["name"]))
	content, _ := input["content"].(string)
	dir := recipes.Dir()

	switch action {
	case "list", "":
		loaded := recipes.LoadAll(dir)
		out := make([]map[string]any, 0, len(loaded))
		for _, l := range loaded {
			row := map[string]any{"name": recipeNameOf(l)}
			if l.Err != nil {
				row["broken"] = l.Err.Error()
			} else if l.Recipe != nil {
				row["steps"] = len(l.Recipe.Steps)
				row["trigger"] = describeTrigger(l.Recipe)
			}
			out = append(out, row)
		}
		sort.Slice(out, func(i, j int) bool {
			return fmt.Sprint(out[i]["name"]) < fmt.Sprint(out[j]["name"])
		})
		return tools.SuccessResult(map[string]any{"dir": dir, "recipes": out}), nil

	case "read":
		if name == "" {
			return tools.ErrorResult(fmt.Errorf("name is required to read a recipe")), nil
		}
		data, err := os.ReadFile(filepath.Join(dir, name+".yaml"))
		if err != nil {
			return tools.ErrorResult(fmt.Errorf("no recipe %q: %w", name, err)), nil
		}
		return tools.SuccessResult(map[string]any{"name": name, "content": string(data)}), nil

	case "check", "write":
		if strings.TrimSpace(content) == "" {
			return tools.ErrorResult(fmt.Errorf("content is required for %q", action)), nil
		}
		if name == "" {
			return tools.ErrorResult(fmt.Errorf("name is required for %q", action)), nil
		}
		// Parsed before it is written, and the error is handed BACK rather than
		// logged: an agent that cannot see why its recipe is invalid will write
		// the same broken one again.
		r, err := recipes.Parse(filepath.Join(dir, name+".yaml"), []byte(content))
		if err != nil {
			return tools.ErrorResult(fmt.Errorf("%s is not a valid recipe: %s", name, describeRecipeError(err))), nil
		}
		if action == "check" {
			return tools.SuccessResult(map[string]any{
				"valid": true, "name": r.Name, "steps": len(r.Steps),
				"trigger": describeTrigger(r),
				"note":    "valid — call action 'write' to save it",
			}), nil
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return tools.ErrorResult(err), nil
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(content), 0o644); err != nil {
			return tools.ErrorResult(err), nil
		}
		return tools.SuccessResult(map[string]any{
			"status": "saved", "name": r.Name, "steps": len(r.Steps),
			"trigger": describeTrigger(r),
			"note":    "live within a few seconds — the recipe directory is watched",
		}), nil

	case "delete":
		if name == "" {
			return tools.ErrorResult(fmt.Errorf("name is required to delete")), nil
		}
		if err := os.Remove(filepath.Join(dir, name+".yaml")); err != nil {
			return tools.ErrorResult(fmt.Errorf("could not delete %q: %w", name, err)), nil
		}
		return tools.SuccessResult(map[string]any{"status": "deleted", "name": name}), nil
	}
	return tools.ErrorResult(fmt.Errorf("unknown action %q (use list, read, check, write or delete)", action)), nil
}

// recipeNameOf names a loaded recipe, falling back to its filename when the
// file is too broken to have parsed a name out of.
func recipeNameOf(l recipes.Loaded) string {
	if l.Recipe != nil && l.Recipe.Name != "" {
		return l.Recipe.Name
	}
	return strings.TrimSuffix(filepath.Base(l.Path), filepath.Ext(l.Path))
}

// describeRecipeError surfaces the located line and suggested fix a recipe
// error carries, which is the whole reason the parser produces them.
func describeRecipeError(err error) string {
	var re *recipes.Error
	if errors.As(err, &re) {
		msg := re.Message
		if re.Line > 0 {
			msg = fmt.Sprintf("line %d: %s", re.Line, msg)
		}
		if re.Fix != "" {
			msg += " — " + re.Fix
		}
		return msg
	}
	return err.Error()
}

// sanitizeRecipeName keeps a name to a single safe file, so a recipe cannot be
// written outside the recipe directory.
func sanitizeRecipeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "<nil>" {
		return ""
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".yaml"), ".yml")
	name = filepath.Base(name)
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
}

func describeTrigger(r *recipes.Recipe) string {
	switch {
	case r == nil:
		return ""
	case r.On.Schedule != "":
		return "schedule: " + r.On.Schedule
	case r.On.Event != "":
		return "event: " + r.On.Event
	case r.On.Webhook != "":
		return "webhook: " + r.On.Webhook
	case r.On.Manual:
		return "manual"
	}
	return "none"
}
