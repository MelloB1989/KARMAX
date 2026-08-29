package runtime

import (
	"context"
	"strings"
	"testing"
)

// SetRepoTokenMinter existed, NewRepoTokenMinter existed, and nothing ever
// called either — so a host with a fully configured GitHub App still failed
// every sandbox with "no GitHub credential", and the error pointed at
// GITHUB_TOKEN, which is the broad credential the App exists to avoid.
func TestAWiredMinterIsWhatTheSandboxUses(t *testing.T) {
	rt := &KarmaxRuntime{}
	var asked string
	rt.SetRepoTokenMinter(func(_ context.Context, repo string) (string, error) {
		asked = repo
		return "ghs-scoped-to-one-repo", nil
	})

	tok, err := rt.repoToken(context.Background(), "dev-zeromoblt/o-refine-react")
	if err != nil {
		t.Fatalf("a wired minter still failed: %v", err)
	}
	if tok != "ghs-scoped-to-one-repo" {
		t.Errorf("got %q, so the sandbox is not using the minted token", tok)
	}
	// The scoping is the point: a token for the wrong repo is a token for a
	// repo this run was never asked to touch.
	if asked != "dev-zeromoblt/o-refine-react" {
		t.Errorf("minted for %q, not the repo of the run", asked)
	}
}

// With no minter and no environment token, the failure has to name the fix.
func TestNoCredentialFailsWithSomethingActionable(t *testing.T) {
	rt := &KarmaxRuntime{}
	t.Setenv("GITHUB_TOKEN", "")
	_, err := rt.repoToken(context.Background(), "o/r")
	if err == nil {
		t.Fatal("no credential at all reported success")
	}
	if !strings.Contains(err.Error(), "GitHub App") {
		t.Errorf("error does not say what to do: %v", err)
	}
}
