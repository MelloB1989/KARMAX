package builtin

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// ActivityTool reports what the operator has actually been building.
//
// KARMAX already records every task it delegated to a coding harness — the
// description, whether it finished, and when. Nothing could read that back
// until now, which meant "what did I ship this week" had no answer that did not
// involve somebody scrolling through git.
//
// It reports the DESCRIPTION of each task, never the output. Harness output is
// full of paths, hostnames and occasionally secrets, and this tool exists to
// feed things that summarise — including one that posts publicly.
type ActivityTool struct {
	Store   *store.Store
	AgentID string
}

func (t *ActivityTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "activity.recent",
		Description: "What the operator has been building: the engineering tasks KARMAX ran " +
			"on their behalf, newest first, with what each was and whether it finished. " +
			"Returns the task descriptions only — never the harness output.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"days":  {"type": "integer", "description": "How far back to look. Default 1, max 30."},
				"limit": {"type": "integer", "description": "How many to return. Default 20, max 100."}
			}
		}`),
	}
}

func (t *ActivityTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	days := clampInt(input["days"], 1, 1, 30)
	limit := clampInt(input["limit"], 20, 1, 100)
	since := time.Now().AddDate(0, 0, -days)

	sessions, err := t.Store.ListCodingSessions(t.AgentID)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})

	out := make([]map[string]any, 0, limit)
	for _, cs := range sessions {
		if cs.UpdatedAt.Before(since) {
			continue
		}
		out = append(out, map[string]any{
			"task":    oneParagraph(cs.Description),
			"status":  cs.Status,
			"harness": cs.ToolType,
			"when":    cs.UpdatedAt.Format(time.RFC3339),
		})
		if len(out) >= limit {
			break
		}
	}

	return tools.SuccessResult(map[string]any{
		"days": days, "count": len(out), "tasks": out,
	}), nil
}

func clampInt(v any, def, min, max int) int {
	n := def
	switch x := v.(type) {
	case float64:
		n = int(x)
	case int:
		n = x
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// oneParagraph collapses a multi-line task prompt to its first paragraph.
//
// A delegated task is often a whole brief; what happened is in the first few
// lines, and the rest is context nothing summarising needs.
func oneParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i > 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > 400 {
		return string([]rune(s)[:397]) + "…"
	}
	return s
}
