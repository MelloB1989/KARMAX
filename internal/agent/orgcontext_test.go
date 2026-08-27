package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

func agentWithStore(t *testing.T) (*Agent, *store.Store) {
	t.Helper()
	db, err := store.New(filepath.Join(t.TempDir(), "a.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Agent{store: db, log: zap.NewNop()}, db
}

// The whole point of the organisation section: what an admin writes has to
// reach the model, or it is a form that changes nothing.
func TestTheOrgContextReachesTheTurn(t *testing.T) {
	a, db := agentWithStore(t)

	if err := db.SaveOrgProfile(store.OrgProfile{
		Org: store.DefaultOrg, Name: "Zero Moblt", Domain: "zeromoblt.com",
		Context: "Tickets live in YouTrack, not Jira.",
	}); err != nil {
		t.Fatal(err)
	}

	got := a.buildOrgContext()
	for _, want := range []string{"organisation you work for", "Zero Moblt", "Tickets live in YouTrack"} {
		if !strings.Contains(got, want) {
			t.Errorf("the turn context omits %q:\n%s", want, got)
		}
	}
}

// Read fresh each turn, so editing it in the console takes effect on the next
// message rather than the next restart.
func TestTheOrgContextIsReadFreshEachTurn(t *testing.T) {
	a, db := agentWithStore(t)

	if err := db.SaveOrgProfile(store.OrgProfile{Org: store.DefaultOrg, Name: "Before"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.buildOrgContext(), "Before") {
		t.Fatal("the first read did not see the profile")
	}

	if err := db.SaveOrgProfile(store.OrgProfile{Org: store.DefaultOrg, Name: "After"}); err != nil {
		t.Fatal(err)
	}
	got := a.buildOrgContext()
	if strings.Contains(got, "Before") || !strings.Contains(got, "After") {
		t.Errorf("an edit did not take effect without a restart: %q", got)
	}
}

// A heading with nothing under it spends context to tell the model the company
// has no name.
func TestAnEmptyProfileAddsNothingToThePrompt(t *testing.T) {
	a, _ := agentWithStore(t)
	if got := a.buildOrgContext(); got != "" {
		t.Errorf("an unset profile added %q to every turn", got)
	}
}

// An agent with no store must not panic building its context.
func TestNoStoreIsSurvivable(t *testing.T) {
	a := &Agent{log: zap.NewNop()}
	if got := a.buildOrgContext(); got != "" {
		t.Errorf("got %q with no store", got)
	}
}
