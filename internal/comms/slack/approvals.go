package slack

import (
	"context"
	"fmt"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// Action ids on the two buttons an approval message carries. proposalID rides
// as the button's value, not the action id — the id says which button was
// pressed, the value says what it was pressed about.
const (
	approveActionID = "karmax_approve"
	rejectActionID  = "karmax_reject"
)

// PostApproval posts an Approve/Reject prompt to target ("<channel>" or
// "<channel>:<thread_ts>", same syntax as Send) and returns the ts of the
// message it posted, so the caller can remember which message to update once
// a decision comes back. proposalID is opaque here — the id an agent asked a
// role to decide on — and travels unchanged on both buttons.
func (c *Channel) PostApproval(ctx context.Context, target, title, summary, proposalID string) (string, error) {
	if c.api == nil {
		return "", fmt.Errorf("slack: not connected")
	}
	channel, thread := splitTarget(target)
	if channel == "" {
		return "", fmt.Errorf("slack: no channel to send to")
	}

	body := title
	if summary != "" {
		body += "\n" + summary
	}
	opts := []slack.MsgOption{
		slack.MsgOptionBlocks(approvalBlocks(title, summary, proposalID)...),
		// Fallback text for notifications/screen readers, and what
		// resolveApproval rebuilds from once a decision comes in.
		slack.MsgOptionText(body, false),
	}
	if thread != "" {
		opts = append(opts, slack.MsgOptionTS(thread))
	}
	_, ts, err := c.api.PostMessageContext(ctx, channel, opts...)
	return ts, err
}

func approvalBlocks(title, summary, proposalID string) []slack.Block {
	text := "*" + title + "*"
	if summary != "" {
		text += "\n" + summary
	}
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("karmax_approval",
			slack.NewButtonBlockElement(approveActionID, proposalID,
				slack.NewTextBlockObject(slack.PlainTextType, "Approve", true, false)).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement(rejectActionID, proposalID,
				slack.NewTextBlockObject(slack.PlainTextType, "Reject", true, false)).WithStyle(slack.StyleDanger),
		),
	}
}

// handleInteraction turns a block_actions payload into a comms.Decision,
// updates the original message so it stops offering a choice that's already
// made, and hands the decision to whoever is draining Decisions().
func (c *Channel) handleInteraction(ctx context.Context, cb slack.InteractionCallback) {
	d, original, channel, ts, ok := decisionFromInteraction(cb, c.id)
	if !ok {
		return
	}

	if err := c.resolveApproval(ctx, channel, ts, original, d.Approved, d.DeciderName); err != nil {
		c.log.Warn("could not update the approval message",
			zap.String("proposal", d.ProposalID), zap.Error(err))
	}

	select {
	case c.decisions <- d:
	default:
		// Full inbox means whatever drains this is behind; dropping here beats
		// blocking the socket pump the way a full message inbox already does.
		c.log.Warn("decisions channel full; dropped a decision", zap.String("proposal", d.ProposalID))
	}
}

// decisionFromInteraction is the pure half of handleInteraction: given a
// Slack interaction payload, decide whether it's one of ours and if so what
// it means. Kept separate from the network calls so the mapping itself is
// directly testable.
func decisionFromInteraction(cb slack.InteractionCallback, channelID string) (d comms.Decision, originalText, channel, ts string, ok bool) {
	if cb.Type != slack.InteractionTypeBlockActions {
		return comms.Decision{}, "", "", "", false
	}
	for _, action := range cb.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		var approved bool
		switch action.ActionID {
		case approveActionID:
			approved = true
		case rejectActionID:
			approved = false
		default:
			continue
		}
		deciderName := cb.User.Name
		if deciderName == "" {
			deciderName = cb.User.ID
		}
		d = comms.Decision{
			ProposalID:  action.Value,
			Approved:    approved,
			DeciderID:   cb.User.ID,
			DeciderName: deciderName,
			ChannelID:   channelID,
			At:          time.Now(),
		}
		return d, cb.Message.Text, cb.Channel.ID, resolveMessageTS(cb), true
	}
	return comms.Decision{}, "", "", "", false
}

// resolveMessageTS finds the ts of the message the buttons were attached to.
// Message.Timestamp is set for a normal channel post; Container.MessageTs
// covers the shapes (e.g. an ephemeral or attachment-hosted action) where the
// message itself doesn't carry its own ts in the payload.
func resolveMessageTS(cb slack.InteractionCallback) string {
	if cb.Message.Timestamp != "" {
		return cb.Message.Timestamp
	}
	return cb.Container.MessageTs
}

// resolveApproval rewrites the approval message once a decision is in, so it
// reads "Approved by @who" (or "Rejected by @who") instead of leaving live
// buttons on a question that's already settled. original is the title+summary
// text PostApproval stored as the message's fallback text, kept so the
// context isn't lost — only the buttons are.
func (c *Channel) resolveApproval(ctx context.Context, channel, ts, original string, approved bool, deciderName string) error {
	if c.api == nil {
		return fmt.Errorf("slack: not connected")
	}
	if channel == "" || ts == "" {
		return fmt.Errorf("slack: no message to update")
	}
	verb := "Rejected"
	if approved {
		verb = "Approved"
	}
	text := original + fmt.Sprintf("\n\n%s by %s", verb, deciderName)
	_, _, _, err := c.api.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionBlocks(slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil)),
		slack.MsgOptionText(text, false),
	)
	return err
}
