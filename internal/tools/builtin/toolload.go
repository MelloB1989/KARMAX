package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Asking for a tool the model was told about but not given.
//
// The agent's always-on context lists every tool by name and one line of
// purpose; the full schemas are ~5,000 tokens and would otherwise ride on every
// routing decision. When the model decides it needs one, it asks here and the
// agent re-runs the turn with that tool actually available.

// LoadToolTool resolves a requested tool name against the registry so a request
// for something that does not exist fails immediately, with the real names,
// rather than being handed back to a model that will guess again.
type LoadToolTool struct {
	Registry *tools.Registry
	// Available lists the names the agent could lend. Nil means "anything in the
	// registry"; a non-nil list is the authority, so a tool the agent cannot bind
	// is never advertised as loadable.
	Available []string
}

func (t *LoadToolTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "tools.load",
		Description: "Request the full definition of a tool listed in your tool index so you can call it. " +
			"Use this the moment you decide a task needs a tool you do not already have: name it here, and it becomes available immediately. " +
			"You can ask for several at once. Tools you already hold do not need loading.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"names": {
					"type": "array",
					"items": {"type": "string"},
					"description": "Tool names from your index, e.g. [\"google_workspace\", \"whatsapp.read\"]."
				}
			},
			"required": ["names"]
		}`),
	}
}

func (t *LoadToolTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	names := stringList(input["names"])
	if len(names) == 0 {
		if single, ok := input["name"].(string); ok && strings.TrimSpace(single) != "" {
			names = []string{single}
		}
	}
	if len(names) == 0 {
		return tools.ErrorResult(fmt.Errorf("name at least one tool to load")), nil
	}

	var loaded, unknown []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if t.known(n) {
			loaded = append(loaded, n)
		} else {
			unknown = append(unknown, n)
		}
	}

	if len(loaded) == 0 {
		return tools.ErrorResult(fmt.Errorf("no such tool: %s — available: %s",
			strings.Join(unknown, ", "), strings.Join(t.names(), ", "))), nil
	}

	out := map[string]any{
		// The agent reads this to decide what to lend the follow-up turn.
		"loaded": loaded,
		"status": "available now — call them in your next step",
	}
	if len(unknown) > 0 {
		out["unknown"] = unknown
	}
	return tools.SuccessResult(out), nil
}

func (t *LoadToolTool) known(name string) bool {
	if len(t.Available) > 0 {
		for _, a := range t.Available {
			if a == name || tools.CanonicalName(a) == tools.CanonicalName(name) {
				return true
			}
		}
		return false
	}
	if t.Registry == nil {
		return false
	}
	_, ok := t.Registry.Get(name)
	return ok
}

func (t *LoadToolTool) names() []string {
	if len(t.Available) > 0 {
		out := append([]string{}, t.Available...)
		sort.Strings(out)
		return out
	}
	if t.Registry == nil {
		return nil
	}
	var out []string
	for _, m := range t.Registry.List() {
		out = append(out, m.Name)
	}
	sort.Strings(out)
	return out
}

// stringList accepts the shapes a model produces for an array argument.
func stringList(v any) []string {
	switch vv := v.(type) {
	case []string:
		return vv
	case []any:
		var out []string
		for _, e := range vv {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		// A single name, or a JSON array that arrived as text.
		s := strings.TrimSpace(vv)
		if strings.HasPrefix(s, "[") {
			var arr []string
			if json.Unmarshal([]byte(s), &arr) == nil {
				return arr
			}
		}
		if s == "" {
			return nil
		}
		return []string{s}
	}
	return nil
}
