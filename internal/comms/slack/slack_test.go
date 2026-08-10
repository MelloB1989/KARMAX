package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// newTestChannel returns a channel with the user-name cache pre-seeded, so
// delivery resolves names without touching the network.
func newTestChannel(names map[string]string) *Channel {
	c := New("slack-test", "xapp-x", "xoxb-x", zap.NewNop())
	c.botID = "UBOT"
	for k, v := range names {
		c.names[k] = v
	}
	return c
}

// A long answer is split, not truncated.
//
// Cutting at the limit stops mid-sentence and gives the operator no way to ask
// for the rest, which is worse than two messages.
func TestChunksSplitOnLineBoundaries(t *testing.T) {
	long := strings.Repeat("a line of text\n", 500) // well over the limit
	parts := chunks(long, maxMessage)
	if len(parts) < 2 {
		t.Fatalf("expected the message to be split, got %d part(s)", len(parts))
	}
	for i, p := range parts {
		if len(p) > maxMessage {
			t.Errorf("part %d is %d bytes, over the %d limit", i, len(p), maxMessage)
		}
	}
	if joined := strings.ReplaceAll(strings.Join(parts, "\n"), "\n", ""); joined != strings.ReplaceAll(long, "\n", "") {
		t.Error("chunking dropped or altered content")
	}
}

// A single line with nothing to break on is still bounded.
func TestOneEnormousLineIsStillSplit(t *testing.T) {
	parts := chunks(strings.Repeat("x", maxMessage*2+50), maxMessage)
	if len(parts) < 3 {
		t.Fatalf("got %d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > maxMessage {
			t.Errorf("part %d is %d bytes, over the limit", i, len(p))
		}
	}
}

// A short message is one message, unchanged.
func TestShortMessagesAreNotTouched(t *testing.T) {
	parts := chunks("just a line", maxMessage)
	if len(parts) != 1 || parts[0] != "just a line" {
		t.Errorf("got %#v", parts)
	}
}

// KARMAX must not answer itself. Slack echoes a bot's own posts back over the
// socket, so forwarding them is an immediate loop.
func TestOurOwnMessagesAreIgnored(t *testing.T) {
	c := newTestChannel(nil)
	for _, tc := range []struct {
		name string
		evt  *slackevents.MessageEvent
	}{
		{"our bot user", &slackevents.MessageEvent{User: "UBOT", Text: "hello", Channel: "C1"}},
		{"any bot", &slackevents.MessageEvent{BotID: "B123", Text: "hello", Channel: "C1"}},
		{"no user at all", &slackevents.MessageEvent{Text: "hello", Channel: "C1"}},
		{"an edit", &slackevents.MessageEvent{User: "U1", SubType: "message_changed", Text: "hi", Channel: "C1"}},
		{"a join", &slackevents.MessageEvent{User: "U1", SubType: "channel_join", Text: "hi", Channel: "C1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.handleEvent(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: tc.evt},
			})
			select {
			case msg := <-c.IncomingMessages():
				t.Fatalf("this should not have been routed: %q", msg.Content)
			default:
			}
		})
	}
}

// A real message from a person is routed, with the sender resolved.
func TestAPersonsMessageIsRouted(t *testing.T) {
	c := newTestChannel(map[string]string{"U1": "maya"})
	c.handleEvent(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
			User: "U1", Text: "can you check the deploy", Channel: "C1",
			TimeStamp: "1717171717.000100", ThreadTimeStamp: "1717171700.000100",
		}},
	})
	select {
	case msg := <-c.IncomingMessages():
		if msg.Content != "can you check the deploy" {
			t.Errorf("content = %q", msg.Content)
		}
		if msg.SenderName != "maya" {
			t.Errorf("sender = %q, want the resolved name", msg.SenderName)
		}
		// The thread is what makes a reply land under the question.
		if msg.ReplyToID != "1717171700.000100" {
			t.Errorf("thread lost: %q", msg.ReplyToID)
		}
	default:
		t.Fatal("a real message was not routed")
	}
}

// An @-mention in a channel the bot is not a member of arrives as a different
// event, and has to work the same way.
func TestAnAppMentionIsRouted(t *testing.T) {
	c := newTestChannel(map[string]string{"U2": "sam"})
	c.handleEvent(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
			User: "U2", Text: "<@UBOT> status?", Channel: "C9", TimeStamp: "1717171718.000100",
		}},
	})
	select {
	case msg := <-c.IncomingMessages():
		if !strings.Contains(msg.Content, "status?") {
			t.Errorf("content = %q", msg.Content)
		}
	default:
		t.Fatal("an app mention was not routed")
	}
}

// The target carries an optional thread, and a Slack timestamp contains a dot
// but never a colon — which is why the colon is safe as the separator.
func TestTargetsCarryAnOptionalThread(t *testing.T) {
	for _, tc := range []struct{ in, channel, thread string }{
		{"C123", "C123", ""},
		{"C123:1717171700.000100", "C123", "1717171700.000100"},
		{"", "", ""},
	} {
		channel, thread := splitTarget(tc.in)
		if channel != tc.channel || thread != tc.thread {
			t.Errorf("splitTarget(%q) = %q,%q; want %q,%q", tc.in, channel, thread, tc.channel, tc.thread)
		}
	}
}
