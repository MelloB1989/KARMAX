package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// runs records what an approval asked the agent to do.
type runs struct {
	mu      sync.Mutex
	prompts []string
	done    chan struct{}
}

func watchRuns(s *Server) *runs {
	r := &runs{done: make(chan struct{}, 8)}
	s.runApproved = func(_ context.Context, prompt string) (string, error) {
		r.mu.Lock()
		r.prompts = append(r.prompts, prompt)
		r.mu.Unlock()
		r.done <- struct{}{}
		return "Sent Dana the update.", nil
	}
	return r
}

func (r *runs) wait(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the approved action never ran")
	}
}

// The app, the operator on WhatsApp and the policy can all approve the same
// proposal at once. Its action runs once; the others are told it is already
// decided.
func TestAnApprovedActionRunsOnceHoweverManyApprove(t *testing.T) {
	s := newTaskTestServer(t)
	r := watchRuns(s)
	if err := s.store.CreateProposal(store.StoredProposal{ID: "p1", AgentID: "nexus", Title: "Reply to Dana", ProposedAction: "send the update"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for _, by := range []string{"operator (whatsapp)", ByPolicy, "operator (whatsapp)"} {
		wg.Add(1)
		go func(by string) {
			defer wg.Done()
			if err := s.DecideProposal("p1", "approve", "", by); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(by)
	}
	// The app's own button, too.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/proposals/p1/decision", strings.NewReader(`{"decision":"approve"}`))
	req.SetPathValue("id", "p1")
	wg.Add(1)
	go func() { defer wg.Done(); s.handleProposalDecision(w, req) }()
	wg.Wait()
	r.wait(t)
	time.Sleep(50 * time.Millisecond) // a second run, if there were one, would have started
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) != 1 {
		t.Fatalf("the action ran %d times, want once", len(r.prompts))
	}
	if appWon := w.Code == http.StatusOK; ok+boolInt(appWon) != 1 {
		t.Errorf("%d WhatsApp/policy deciders and app=%v succeeded; want exactly one decider", ok, appWon)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// An action cleared by the policy rather than by the operator is not
// presented to the agent as the operator's approval, and the agent is told to
// stop at anything on the hard floor. Its outcome is recorded like any other.
func TestAPolicyApprovalIsNotPresentedAsTheOperators(t *testing.T) {
	s := newTaskTestServer(t)
	r := watchRuns(s)
	if err := s.store.CreateProposal(store.StoredProposal{ID: "p2", AgentID: "nexus", Title: "Reply to Dana", ProposedAction: "send the update"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideProposal("p2", "approve", "not on the hard floor (payments or spending money)", ByPolicy); err != nil {
		t.Fatal(err)
	}
	r.wait(t)
	r.mu.Lock()
	prompt := r.prompts[0]
	r.mu.Unlock()
	if strings.Contains(prompt, "operator APPROVED") {
		t.Errorf("a policy approval was presented as the operator's:\n%s", prompt)
	}
	for _, want := range []string{"send the update", "hard floor", "propose", "payments or spending money"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ := s.store.GetProposal("p2")
		if p.Status == "executed" {
			if p.DecidedBy != ByPolicy || p.Result != "Sent Dana the update." {
				t.Errorf("recorded %+v", p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %q, want executed", p.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
