package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/sources"
	"github.com/MelloB1989/karmax/internal/tools"
	"go.uber.org/zap"
)

// accountTool is a tool whose schema takes an account.
type accountTool struct{ name string }

func (s *accountTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{Name: s.name, Description: "Google Workspace.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"account":{"type":"string"},"command":{"type":"string"}}}`)}
}
func (s *accountTool) Execute(context.Context, map[string]any) (tools.ToolResult, error) {
	return tools.SuccessResult(nil), nil
}

// The agent is told which of the operator's accounts exist on each service,
// and which of its tools act as which — derived from what is connected and
// what it holds, never a hand-kept list.
func TestTheAccountsSectionSaysWhatActsAsWhom(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	a.allTools = append(toolSet("github.comment", "github.issues", "github.comment@work", "memory.ingest"),
		&accountTool{name: "google"})
	a.SetAccounts(func() []AccountInfo {
		return []AccountInfo{
			{Service: "github", Name: "GitHub", Connected: true},
			{Service: "github", Account: "work", Name: "GitHub", Connected: true},
			{Service: "gog", Name: "Google (gog)", Connected: true, Problem: "token expired"},
			{Service: "notion", Name: "Notion"}, // not connected: not worth a line
		}
	})
	a.noteSourceAccount(sources.Event{Source: "gcal", Account: "rowan@example.test"}, time.Now())
	a.noteSourceAccount(sources.Event{Source: "github", Account: "octo-work"}, time.Now())

	got := a.buildAccountsContext()
	for _, want := range []string{
		"operator's own",
		"github", "primary", "github.comment, github.issues", "work", "github.comment@work",
		"token expired",
		"google (account)",
		"gcal:rowan@example.test", "github:octo-work",
		"explicitly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("accounts context omits %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Notion") {
		t.Errorf("an unconnected service was listed:\n%s", got)
	}
	if strings.Contains(got, "memory.ingest") {
		t.Errorf("a tool that acts on no account was listed:\n%s", got)
	}
}

// With nothing connected and no events seen, the section costs nothing.
func TestNoAccountsAddsNothing(t *testing.T) {
	if got := (&Agent{log: zap.NewNop()}).buildAccountsContext(); got != "" {
		t.Errorf("got %q", got)
	}
}
