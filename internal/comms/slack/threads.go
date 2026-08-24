package slack

import (
	"context"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/safety"
	"github.com/slack-go/slack"
)

// PostThread posts text into channel — inside threadTS if given, as a new
// top-level message otherwise — and returns the ts of the message it posted.
// That ts is what a case binds to (case_store.BindCaseThread): Send never
// handed it back because nothing needed it before there was a case to attach
// it to.
func (c *Channel) PostThread(ctx context.Context, channel, threadTS, text string) (string, error) {
	if c.api == nil {
		return "", fmt.Errorf("slack: not connected")
	}
	if channel == "" {
		return "", fmt.Errorf("slack: no channel to send to")
	}
	opts := []slack.MsgOption{slack.MsgOptionText(text, false), slack.MsgOptionDisableLinkUnfurl()}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, ts, err := c.api.PostMessageContext(ctx, channel, opts...)
	return ts, err
}

// ThreadReply is one message read back from a thread.
type ThreadReply struct {
	User      string
	UserName  string
	Text      string
	Timestamp string
}

// ThreadReplies returns a thread's messages oldest-first, root included — the
// catch-up a case needs when it wasn't the one running while the thread moved.
func (c *Channel) ThreadReplies(ctx context.Context, channel, threadTS string) ([]ThreadReply, error) {
	if c.api == nil {
		return nil, fmt.Errorf("slack: not connected")
	}
	if channel == "" || threadTS == "" {
		return nil, fmt.Errorf("slack: channel and thread ts are both required")
	}

	var out []ThreadReply
	cursor := ""
	for {
		msgs, hasMore, next, err := c.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: channel,
			Timestamp: threadTS,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return out, err
		}
		for _, m := range msgs {
			out = append(out, ThreadReply{
				User:      m.User,
				UserName:  c.userName(ctx, m.User),
				Text:      m.Text,
				Timestamp: m.Timestamp,
			})
		}
		if !hasMore || next == "" {
			break
		}
		cursor = next
	}
	return out, nil
}

// FormatThreadReplies renders a thread as fenced text safe to hand to a
// model. Whoever is in a Slack thread is not the operator, and their words
// get the same fence a WhatsApp read already gets (internal/safety/fence.go)
// — a thread is exactly the kind of thing an attacker can post into.
func FormatThreadReplies(channel string, replies []ThreadReply) string {
	var b strings.Builder
	for _, r := range replies {
		name := r.UserName
		if name == "" {
			name = r.User
		}
		fmt.Fprintf(&b, "%s: %s\n", name, r.Text)
	}
	return safety.Fence("a Slack thread in "+channel, b.String())
}
