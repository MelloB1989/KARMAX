package store

import (
	"path/filepath"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// A proposal is decided once. The app, the operator on WhatsApp and the
// policy can all reach the same pending proposal at nearly the same moment;
// whichever decides first wins, and the others are told it is already
// decided, so an approved action is never run twice.
func TestAProposalIsDecidedOnce(t *testing.T) {
	s, err := New(filepath.Join(t.TempDir(), "p.db"), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateProposal(StoredProposal{ID: "p1", AgentID: "nexus", Title: "Reply to Dana"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	won := make(chan string, 8)
	for _, by := range []string{"operator (app)", "operator (whatsapp)", "policy", "operator (app)"} {
		wg.Add(1)
		go func(by string) {
			defer wg.Done()
			ok, err := s.DecideProposalIfPending("p1", "approved", "", by)
			if err != nil {
				t.Error(err)
			}
			if ok {
				won <- by
			}
		}(by)
	}
	wg.Wait()
	close(won)
	var winners []string
	for by := range won {
		winners = append(winners, by)
	}
	if len(winners) != 1 {
		t.Fatalf("%d deciders won (%v), want exactly one", len(winners), winners)
	}
	p, _ := s.GetProposal("p1")
	if p.Status != "approved" || p.DecidedBy != winners[0] {
		t.Errorf("proposal is %q by %q, want approved by %q", p.Status, p.DecidedBy, winners[0])
	}
	if ok, _ := s.DecideProposalIfPending("p1", "rejected", "", "operator"); ok {
		t.Error("an approved proposal was rejected afterwards")
	}
	if ok, err := s.DecideProposalIfPending("missing", "approved", "", "operator"); ok || err != nil {
		t.Errorf("an unknown proposal: ok=%v err=%v", ok, err)
	}
}
