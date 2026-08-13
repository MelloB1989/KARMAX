package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/MelloB1989/karmax/internal/tools"
)

// Finding a tool without carrying it.
//
// The orchestrator holds a dozen tools and knows the rest only as an index it
// was handed at startup — which says nothing about the ones it was never
// granted. Asked to post to LinkedIn it concluded KARMAX could not, while
// linkedin.post sat in the registry the whole time, one sub-agent away.
//
// Search rather than a listing: eighty tools with descriptions is most of a
// routing decision's context budget, and the orchestrator does not need to
// carry the catalogue to use it. It needs to be able to ask.

// ToolSearchTool answers "what can this instance do about X".
type ToolSearchTool struct {
	Registry *tools.Registry
	// Held and Loadable are this agent's own tools, so a result can say whether
	// it is already in hand or needs a sub-agent built around it.
	Held     []string
	Loadable []string
}

func (t *ToolSearchTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "tools.search",
		Description: "Search every tool on this instance by what it does — 'post to linkedin', 'read email', " +
			"'github issues'. Use it BEFORE saying you cannot do something, and to pick the tools for a " +
			"sub-agent: results say whether each tool is yours already or must be granted to a child via " +
			"subagent.spawn's 'tools' list.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "What you are trying to do, in your own words. Omit to list everything."},
				"limit": {"type": "integer", "description": "Maximum results. Default 12."}
			}
		}`),
	}
}

func (t *ToolSearchTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Registry == nil {
		return tools.ErrorResult(fmt.Errorf("no tool registry on this instance")), nil
	}
	query, _ := input["query"].(string)
	limit := 12
	if n, ok := numberArg(input["limit"]); ok && n > 0 {
		limit = int(n)
	}

	mine := map[string]string{}
	for _, n := range t.Held {
		mine[n] = "held"
	}
	for _, n := range t.Loadable {
		if mine[n] == "" {
			mine[n] = "loadable"
		}
	}

	type scored struct {
		manifest tools.ToolManifest
		score    int
	}
	var hits []scored
	terms := searchTerms(query)
	for _, m := range t.Registry.List() {
		s := scoreTool(m, terms)
		if s > 0 || len(terms) == 0 {
			hits = append(hits, scored{m, s})
		}
	}
	// Score first, then name, so repeated searches return a stable order.
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].manifest.Name < hits[j].manifest.Name
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}

	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		where := mine[h.manifest.Name]
		if where == "" {
			where = "grant_to_subagent"
		}
		results = append(results, map[string]any{
			"name":     h.manifest.Name,
			"does":     tools.Summarize(h.manifest.Description),
			"you_have": where,
		})
	}
	if len(results) == 0 {
		return tools.SuccessResult(map[string]any{
			"query": query, "results": results,
			"note": "nothing matched — try fewer or different words before concluding it cannot be done",
		}), nil
	}
	return tools.SuccessResult(map[string]any{
		"query":   query,
		"results": results,
		"legend": "held = you can call it now; loadable = call tools.load first; " +
			"grant_to_subagent = not yours, pass it in subagent.spawn's 'tools' list and the child can use it",
	}), nil
}

// searchTerms splits a query into the words worth matching on.
func searchTerms(q string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(q)) {
		f = strings.Trim(f, ".,'\"?!()")
		// Short connectives match everything and rank nothing.
		if len(f) < 3 || searchStopWords[f] {
			continue
		}
		out = append(out, f)
	}
	return out
}

var searchStopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "how": true, "can": true,
	"you": true, "get": true, "use": true, "that": true, "this": true, "from": true,
	"what": true, "does": true, "any": true, "all": true, "some": true, "way": true,
}

// scoreTool ranks a tool against the query.
//
// A name match outweighs a description match: someone searching "linkedin"
// wants linkedin.post above the six tools whose descriptions merely mention it.
func scoreTool(m tools.ToolManifest, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	name := strings.ToLower(m.Name)
	desc := strings.ToLower(m.Description)
	score := 0
	for _, term := range terms {
		switch {
		case name == term:
			score += 12
		case strings.Contains(name, term):
			score += 6
		case strings.Contains(desc, term):
			score++
		}
	}
	return score
}
