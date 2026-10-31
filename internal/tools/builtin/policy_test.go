package builtin

import (
	"context"
	"strings"
	"testing"

	"github.com/MelloB1989/karmax/internal/policy"
)

// Under "act, then report" an approval for ordinary work is a question the
// operator decided not to be asked. propose declines it and says to act.
func TestProposeDeclinesWhatIsNotOnTheFloor(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)
	p := policy.Default()
	tool := &ProposeTool{Store: s, Policy: &p}

	res, err := tool.Execute(context.Background(), map[string]any{
		"title": "Reply to Dana about the release", "action": "Send: ships Friday"})
	if err != nil || res.IsError {
		t.Fatalf("declining is guidance, not a failure: %v %+v", err, res)
	}
	out, _ := res.Output.(map[string]any)
	if out["status"] != "not_needed" || !strings.Contains(out["message"].(string), "Do it") {
		t.Errorf("result = %+v", out)
	}
	if rows, _ := s.ListProposals("pending", 10); len(rows) != 0 || len(*got) != 0 {
		t.Errorf("an off-floor proposal was created: %+v %+v", rows, *got)
	}
}

// On the floor, the proposal is created and says which rule it is under, so
// the mirror can send it without a second opinion.
func TestProposeOnTheFloorIsCreatedWithItsRule(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)
	p := policy.Default()
	tool := &ProposeTool{Store: s, Policy: &p}

	res, _ := tool.Execute(context.Background(), map[string]any{
		"title": "Delete the old staging bucket", "action": "aws s3 rb s3://staging-old --force",
		"floor": "deleting repositories, data or cloud resources"})
	if out, _ := res.Output.(map[string]any); out["status"] != "pending_approval" {
		t.Fatalf("result = %+v", res)
	}
	if len(*got) != 1 || (*got)[0].Kind != MirrorApproval || (*got)[0].Floor != "deleting repositories, data or cloud resources" {
		t.Errorf("mirror saw %+v", *got)
	}
}

// A loop's proposal names no floor rule; whoever observes approvals decides
// whether it waits for the operator, so the "Approval needed" phone push is
// theirs to send — not sent ahead of a decision that may need nobody.
func TestARaisedProposalLeavesThePhonePushToItsObserver(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)
	id, err := CreateProposal(s, "nexus", "task", "Reply to Dana", "", "Send: ships Friday", "normal")
	if err != nil || id == "" {
		t.Fatalf("create: %q %v", id, err)
	}
	if len(*got) != 1 || (*got)[0].ProposalID != id || (*got)[0].Push == nil {
		t.Fatalf("the observer was not handed the push: %+v", *got)
	}
}

// Ask-first proposes anything, floor or not.
func TestProposeUnderAskFirstCreatesAnything(t *testing.T) {
	s := mirrorStore(t)
	captureMirror(t)
	p := policy.New(policy.Config{Autonomy: "ask"})
	res, _ := (&ProposeTool{Store: s, Policy: &p}).Execute(context.Background(), map[string]any{
		"title": "Reply to Dana", "action": "Send: ships Friday"})
	if out, _ := res.Output.(map[string]any); out["status"] != "pending_approval" {
		t.Fatalf("result = %+v", res)
	}
}

// The model marks what cannot wait; the mirror trusts it rather than asking
// again.
func TestAppPushCarriesUrgency(t *testing.T) {
	s := mirrorStore(t)
	got := captureMirror(t)
	tool := &AppPushTool{Store: s}
	if _, err := tool.Execute(context.Background(), map[string]any{"title": "FYI", "body": "routine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), map[string]any{"title": "Prod down", "body": "5xx", "urgent": true}); err != nil {
		t.Fatal(err)
	}
	PushAppNotificationUrgent(s, "", "alert", "Brain down", "no replies")
	if len(*got) != 3 || (*got)[0].Urgent || !(*got)[1].Urgent || !(*got)[2].Urgent {
		t.Errorf("urgency not carried: %+v", *got)
	}
}
