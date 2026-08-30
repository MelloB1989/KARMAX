package karmahelper

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/MelloB1989/karma/ai"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

// actorSpy records the actor its context carried when the model called it.
type actorSpy struct{ saw string }

func (a *actorSpy) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "google.mail.search", Description: "search mail",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}
func (a *actorSpy) Execute(ctx context.Context, _ map[string]any) (tools.ToolResult, error) {
	a.saw = connectorkit.ActorFrom(ctx)
	return tools.SuccessResult("ok"), nil
}

// karma builds the context it hands a tool handler from context.Background(),
// so nothing on the caller's context survives the trip through the model. For
// a per-user connector that is fatal: it resolves whose Google account to use
// from the context, found nobody, and refused every call — while the connector
// was connected, granted and healthy. From Slack that reads as the agent
// saying it cannot access your mail.
func TestTheActorSurvivesTheTripThroughTheModel(t *testing.T) {
	spy := &actorSpy{}
	s := &Session{}
	s.setActor("mellob")

	gt := karmaxToolToGoFunctionTool(spy, nil, s.currentActor)
	// Exactly what karma does: a context with nothing on it.
	if _, err := gt.Handler(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if spy.saw != "mellob" {
		t.Fatalf("the tool ran on behalf of %q, so a per-user connector refuses it", spy.saw)
	}
}

// A turn with no actor must not inherit the last one's: reading one person's
// mailbox to answer another's question is not a bug to tighten later.
func TestAnActorIsNotInheritedByTheNextTurn(t *testing.T) {
	spy := &actorSpy{}
	s := &Session{}
	s.setActor("mellob")
	s.setActor(connectorkit.ActorFrom(context.Background())) // next turn: nobody

	gt := karmaxToolToGoFunctionTool(spy, nil, s.currentActor)
	if _, err := gt.Handler(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if spy.saw != "" {
		t.Fatalf("a turn on nobody's behalf acted as %q", spy.saw)
	}
}

// An actor already on the context wins: the caller is closer to the truth than
// a field the session is holding.
func TestAnExplicitActorOnTheContextIsNotOverwritten(t *testing.T) {
	spy := &actorSpy{}
	s := &Session{}
	s.setActor("mellob")

	gt := karmaxToolToGoFunctionTool(spy, nil, s.currentActor)
	ctx := connectorkit.WithActor(context.Background(), "priya")
	if _, err := gt.Handler(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if spy.saw != "priya" {
		t.Fatalf("the context said priya, the tool acted as %q", spy.saw)
	}
}

// The default path — no lent tools, no withheld tools — is the one nearly
// every turn takes. Its model is built once, in NewSession, before any turn
// exists; wiring the actor only into the per-turn rebuild left that path
// exactly as broken as before, and the direct tool call still worked, so it
// looked fixed.
func TestTheDefaultPathCarriesTheActorToo(t *testing.T) {
	spy := &actorSpy{}
	s := NewSession(SessionConfig{}, []tools.Tool{spy})
	s.setActor("mellob")

	var handler func(context.Context, ai.FuncParams) (string, error)
	for _, gt := range s.kai.GoFunctionTools {
		if gt.Name == "google_mail_search" {
			handler = gt.Handler
		}
	}
	if handler == nil {
		t.Fatal("the session's model does not hold the tool at all")
	}
	if _, err := handler(context.Background(), ai.FuncParams{}); err != nil {
		t.Fatal(err)
	}
	if spy.saw != "mellob" {
		t.Fatalf("the default path ran the tool on behalf of %q", spy.saw)
	}
}
