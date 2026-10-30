package api

import (
	"testing"

	"github.com/MelloB1989/karmax/internal/store"
)

// An approval decided outside the app — the operator on WhatsApp — is recorded
// with who decided, and a decided proposal cannot be decided twice.
func TestDecideProposalRecordsWhoDecided(t *testing.T) {
	s := newTaskTestServer(t)
	if err := s.store.CreateProposal(store.StoredProposal{ID: "p1", AgentID: "nexus", Title: "post it"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideProposal("p1", "maybe", "", "operator"); err == nil {
		t.Fatal("a decision other than approve or reject was accepted")
	}
	if err := s.DecideProposal("p1", "reject", "", "operator (whatsapp)"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	p, err := s.store.GetProposal("p1")
	if err != nil || p == nil {
		t.Fatalf("get: %v %v", p, err)
	}
	if p.Status != "rejected" || p.DecidedBy != "operator (whatsapp)" {
		t.Fatalf("got status %q by %q", p.Status, p.DecidedBy)
	}
	if err := s.DecideProposal("p1", "approve", "", "operator"); err == nil {
		t.Fatal("a rejected proposal was approved afterwards")
	}
	if err := s.DecideProposal("nope", "approve", "", "operator"); err == nil {
		t.Fatal("an unknown proposal was decided")
	}
}
