package googleworkspace

// The id is the load-bearing decision here, so it is the thing pinned hardest.
//
// There are two Googles in KARMAX — the per-employee OAuth connector and this
// per-machine CLI session — and they live in the same connector registry,
// where Register keyed by id means one would silently replace the other. That
// is not a failure anyone would see until the wrong Google answered a call.

import (
	"context"
	"strings"
	"testing"

	googleconn "github.com/MelloB1989/karmax/internal/connectors/google"
	"github.com/MelloB1989/karmax/pkg/connectorkit"
)

func TestItDoesNotCollideWithTheOAuthGoogle(t *testing.T) {
	mine := New("gog", nil).Manifest().ID
	theirs := googleconn.New().Manifest().ID
	if mine == theirs {
		t.Fatalf("both Googles claim %q — one will replace the other in the registry", mine)
	}
	if mine != "google_workspace" {
		// The desktop folds this id for display and lists it in ACCOUNT_IDS.
		// Changing it here silently drops the tile there.
		t.Errorf("want google_workspace, got %q", mine)
	}
}

func TestNothingIsAskedForBecauseGogHoldsTheSession(t *testing.T) {
	c := New("gog", nil)
	if c.Auth().Kind != connectorkit.AuthCLI {
		t.Errorf("a session KARMAX does not hold is AuthCLI, got %v", c.Auth().Kind)
	}
	if len(c.Manifest().Config) != 0 {
		t.Error("there is nothing for the operator to paste, so Config must be empty")
	}
}

func TestTheToolKeepsTheNameAgentsAlreadyKnow(t *testing.T) {
	tools := New("gog", nil).Tools()
	if len(tools) != 1 || tools[0].Name != "google" {
		// Moving this under the connector must not rename it: prompts and
		// recipes in the wild call it `google`.
		t.Fatalf("want one tool named google, got %+v", names(tools))
	}
}

func TestACallIsForwardedToTheRunnerUntouched(t *testing.T) {
	var got map[string]any
	c := New("gog", func(_ context.Context, in map[string]any) (any, error) {
		got = in
		return map[string]any{"output": "ok"}, nil
	})
	in := map[string]any{"args": []any{"gmail", "ls"}, "account": "someone@example.com"}
	if _, err := c.Tools()[0].Call(context.Background(), connectorkit.Credentials{}, in); err != nil {
		t.Fatalf("call: %v", err)
	}
	// Forwarded whole: the runner owns the flag defaults and the account
	// fallback, and a second interpretation here would be a second set of
	// rules to keep in step.
	if len(got) != 2 || got["account"] != "someone@example.com" {
		t.Errorf("input was not passed through intact: %+v", got)
	}
}

func TestARunnerlessBuildSaysSoRatherThanPanicking(t *testing.T) {
	c := New("gog", nil)
	_, err := c.Tools()[0].Call(context.Background(), connectorkit.Credentials{},
		map[string]any{"args": []any{"gmail", "ls"}})
	if err == nil {
		t.Fatal("a connector with no runner must refuse, not dereference nil")
	}
}

func TestHealthSaysWhenGogIsNotOnTheMachine(t *testing.T) {
	c := New("definitely-not-a-real-binary-name", nil)
	err := c.Health(context.Background(), connectorkit.Credentials{})
	if err == nil {
		t.Fatal("a missing binary is not healthy")
	}
	// The operator has to learn it is the CLI that is missing, not their login.
	if !strings.Contains(err.Error(), "not on this machine") {
		t.Errorf("want a message about the CLI being absent, got: %v", err)
	}
}

func names(ts []connectorkit.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}
