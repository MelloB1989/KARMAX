package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// A question that expired unanswered is still the operator's to answer: a
// reply to it a day late must reach review.resolve, so the agent is shown it.
func TestAnExpiredQuestionCanStillBeAnswered(t *testing.T) {
	s, err := store.New(filepath.Join(t.TempDir(), "a.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for _, rv := range []store.StoredReview{
		{ID: "rv-open", Namespace: "nexus", TargetKind: "reminder", DedupKey: "a", Question: "Is the plumber still coming on Monday?"},
		{ID: "rv-late", Namespace: "nexus", TargetKind: "reminder", DedupKey: "b", Question: "Is the parcel still on its way?",
			CreatedAt: time.Now().Add(-10 * 24 * time.Hour)},
		{ID: "rv-long-gone", Namespace: "nexus", TargetKind: "reminder", DedupKey: "c", Question: "Did the lease get signed?",
			CreatedAt: time.Now().Add(-60 * 24 * time.Hour)},
	} {
		if err := s.CreateReview(rv); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ExpireReviews("nexus", time.Now().Add(-7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	a := &Agent{log: zap.NewNop(), store: s, def: AgentDef{ID: "nexus"}}
	got := a.buildReviewContext()
	for _, want := range []string{"rv-open", "rv-late"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s is not shown:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "expired") {
		t.Errorf("an expired question is not marked as one:\n%s", got)
	}
}
