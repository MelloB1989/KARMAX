package builtin

import (
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/tools"
)

func grantRegistry() *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(capTool("linkedin.post"))
	reg.Register(capTool("comms.send"))
	reg.Register(capTool("subagent.spawn"))
	return reg
}

func TestResolveGrantPullsFromTheWholeInstance(t *testing.T) {
	// The point of naming tools: the orchestrator does not carry linkedin.post
	// and should not have to, but can still build a child around it.
	tool := &SubagentTool{Registry: grantRegistry()}
	got, err := tool.resolveGrant([]string{"linkedin.post"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0].Manifest().Name != "linkedin.post" {
		t.Fatalf("got %v", manifestNamesOf(got))
	}
}

func TestResolveGrantRefusesAnUnknownName(t *testing.T) {
	// Dropping it silently gives the child a brief it cannot carry out and an
	// answer that sounds finished.
	tool := &SubagentTool{Registry: grantRegistry()}
	_, err := tool.resolveGrant([]string{"linkedin.post", "nope.tool"})
	if err == nil {
		t.Fatal("an unknown tool must fail the spawn")
	}
	if !strings.Contains(err.Error(), "nope.tool") {
		t.Errorf("error should name the missing tool, got: %v", err)
	}
}

func TestResolveGrantNeverGrantsSpawning(t *testing.T) {
	tool := &SubagentTool{Registry: grantRegistry()}
	got, err := tool.resolveGrant([]string{"comms.send", "subagent.spawn"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, n := range manifestNamesOf(got) {
		if n == "subagent.spawn" {
			t.Error("a child must not be able to spawn")
		}
	}
}

func TestResolveGrantEmptyMeansInherit(t *testing.T) {
	tool := &SubagentTool{Registry: grantRegistry()}
	got, err := tool.resolveGrant(nil)
	if err != nil || got != nil {
		t.Errorf("no names should mean inherit, got %v %v", manifestNamesOf(got), err)
	}
}

func manifestNamesOf(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Manifest().Name)
	}
	return out
}
