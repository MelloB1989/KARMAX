package slack

import (
	"testing"

	"github.com/slack-go/slack"
)

func blockActionCallback(actionID, value string) slack.InteractionCallback {
	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "U123", Name: "maya"},
	}
	cb.Channel.ID = "C456"
	cb.Message.Timestamp = "1717171700.000100"
	cb.Message.Text = "*Merge the PR?*\nCI is green."
	cb.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: actionID, Value: value},
	}
	return cb
}

// Clicking Approve maps to an approved decision carrying the proposal id and
// the Slack user id of whoever clicked.
func TestDecisionFromInteractionApprove(t *testing.T) {
	cb := blockActionCallback(approveActionID, "proposal-42")
	d, original, channel, ts, ok := decisionFromInteraction(cb, "slack-main")
	if !ok {
		t.Fatal("expected the approve click to be recognised")
	}
	if !d.Approved {
		t.Error("Approved = false, want true")
	}
	if d.ProposalID != "proposal-42" {
		t.Errorf("ProposalID = %q, want %q", d.ProposalID, "proposal-42")
	}
	if d.DeciderID != "U123" {
		t.Errorf("DeciderID = %q, want %q", d.DeciderID, "U123")
	}
	if d.DeciderName != "maya" {
		t.Errorf("DeciderName = %q, want %q", d.DeciderName, "maya")
	}
	if d.ChannelID != "slack-main" {
		t.Errorf("ChannelID = %q, want the KARMAX channel id, got %q", d.ChannelID, d.ChannelID)
	}
	if channel != "C456" || ts != "1717171700.000100" {
		t.Errorf("channel/ts = %q/%q, want C456/1717171700.000100", channel, ts)
	}
	if original != cb.Message.Text {
		t.Errorf("original = %q, want the original message text preserved", original)
	}
}

// Clicking Reject maps to a rejected decision, same shape otherwise.
func TestDecisionFromInteractionReject(t *testing.T) {
	cb := blockActionCallback(rejectActionID, "proposal-7")
	d, _, _, _, ok := decisionFromInteraction(cb, "slack-main")
	if !ok {
		t.Fatal("expected the reject click to be recognised")
	}
	if d.Approved {
		t.Error("Approved = true, want false")
	}
	if d.ProposalID != "proposal-7" {
		t.Errorf("ProposalID = %q, want %q", d.ProposalID, "proposal-7")
	}
}

// A user with no display name falls back to their id, so the decider is
// never blank.
func TestDecisionFromInteractionFallsBackToUserID(t *testing.T) {
	cb := blockActionCallback(approveActionID, "proposal-1")
	cb.User = slack.User{ID: "U999"}
	d, _, _, _, ok := decisionFromInteraction(cb, "slack-main")
	if !ok {
		t.Fatal("expected the click to be recognised")
	}
	if d.DeciderName != "U999" {
		t.Errorf("DeciderName = %q, want the id as a fallback", d.DeciderName)
	}
}

// Anything that isn't our two action ids is ignored — a menu selection or
// some other app's button must not be mistaken for a decision.
func TestDecisionFromInteractionIgnoresUnrelatedActions(t *testing.T) {
	cb := blockActionCallback("some_other_action", "whatever")
	_, _, _, _, ok := decisionFromInteraction(cb, "slack-main")
	if ok {
		t.Fatal("an unrelated action id must not be read as a decision")
	}
}

// Anything that isn't a block_actions interaction (a view submission, a
// shortcut, ...) is ignored outright.
func TestDecisionFromInteractionIgnoresNonBlockActions(t *testing.T) {
	cb := blockActionCallback(approveActionID, "proposal-1")
	cb.Type = slack.InteractionTypeViewSubmission
	_, _, _, _, ok := decisionFromInteraction(cb, "slack-main")
	if ok {
		t.Fatal("a non-block_actions interaction must not be read as a decision")
	}
}

// resolveMessageTS prefers the message's own timestamp, and falls back to the
// container's when the payload shape doesn't carry one directly.
func TestResolveMessageTS(t *testing.T) {
	cb := blockActionCallback(approveActionID, "proposal-1")
	if got := resolveMessageTS(cb); got != "1717171700.000100" {
		t.Errorf("resolveMessageTS() = %q, want the message timestamp", got)
	}

	cb.Message.Timestamp = ""
	cb.Container.MessageTs = "1717171800.000200"
	if got := resolveMessageTS(cb); got != "1717171800.000200" {
		t.Errorf("resolveMessageTS() = %q, want the container's message ts as a fallback", got)
	}
}
