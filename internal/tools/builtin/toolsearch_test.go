package builtin

import (
	"context"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

type describedTool struct{ name, desc string }

func (d describedTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: d.name, Description: d.desc}
}
func (describedTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.SuccessResult("ok"), nil
}

func searchFixture() *ToolSearchTool {
	reg := tools.NewRegistry()
	reg.Register(describedTool{"linkedin.post", "Post to LinkedIn as the operator."})
	reg.Register(describedTool{"comms.send", "Send a message via WhatsApp or Discord."})
	reg.Register(describedTool{"memory.retrieve", "Search memory. Mentions linkedin sometimes."})
	reg.Register(describedTool{"file.read", "Read a file from disk."})
	return &ToolSearchTool{Registry: reg, Held: []string{"comms.send"}, Loadable: []string{"file.read"}}
}

func searchResults(t *testing.T, tool *ToolSearchTool, q string) []map[string]any {
	t.Helper()
	res, err := tool.Execute(context.Background(), map[string]any{"query": q})
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %v", err, res.Error)
	}
	out := res.Output.(map[string]any)
	rows, _ := out["results"].([]map[string]any)
	return rows
}

func TestToolSearchRanksNameMatchesFirst(t *testing.T) {
	// memory.retrieve mentions linkedin in its description; linkedin.post is
	// what someone searching "linkedin" actually wants.
	rows := searchResults(t, searchFixture(), "post to linkedin")
	if len(rows) == 0 {
		t.Fatal("no results")
	}
	if rows[0]["name"] != "linkedin.post" {
		t.Errorf("first result = %v, want linkedin.post", rows[0]["name"])
	}
}

func TestToolSearchSaysHowToReachEachTool(t *testing.T) {
	tool := searchFixture()
	byName := map[string]string{}
	for _, r := range searchResults(t, tool, "linkedin send file") {
		byName[r["name"].(string)] = r["you_have"].(string)
	}
	// The distinction is the whole point: one is callable now, one needs a
	// load, one only exists for a child the orchestrator builds.
	for name, want := range map[string]string{
		"comms.send":    "held",
		"file.read":     "loadable",
		"linkedin.post": "grant_to_subagent",
	} {
		if byName[name] != want {
			t.Errorf("%s reachability = %q, want %q", name, byName[name], want)
		}
	}
}

func TestToolSearchIgnoresConnectives(t *testing.T) {
	// "how can i post" must not match everything via "how" and "can".
	rows := searchResults(t, searchFixture(), "how can i post")
	if len(rows) != 1 || rows[0]["name"] != "linkedin.post" {
		names := make([]string, 0, len(rows))
		for _, r := range rows {
			names = append(names, r["name"].(string))
		}
		t.Errorf("stop words leaked into matching, got %v", names)
	}
}

func TestToolSearchAdmitsWhenNothingMatches(t *testing.T) {
	res, _ := searchFixture().Execute(context.Background(), map[string]any{"query": "quantum teleportation"})
	out := res.Output.(map[string]any)
	if out["note"] == nil {
		t.Error("an empty result must say so rather than looking like a complete answer")
	}
}
