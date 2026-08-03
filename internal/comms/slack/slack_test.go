package slack

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

// newTestChannel returns a channel with the user-name cache pre-seeded, so
// route() resolves names without touching the network.
func newTestChannel(names map[string]string) *Channel {
	c := New("slack-test", "xapp-x", "xoxb-x", zap.NewNop())
	c.botID = "UBOT"
	for k, v := range names {
		c.names[k] = v
	}
	return c
}

func newEvent(text, user, channel, channelType string) eventPayload {
	var p eventPayload
	p.Event.Type = "message"
	p.Event.Text = text
	p.Event.User = user
	p.Event.Channel = channel
	p.Event.ChannelType = channelType
	p.Event.TS = "1717171717.000100"
	return p
}

func TestRouteResolvesNamesAndMentions(t *testing.T) {
	c := newTestChannel(map[string]string{"U123": "siva", "UBOT": "karmax"})
	c.route(newEvent("hey <@UBOT> can <@U123> ship this?", "U123", "C99", "channel"))

	msg := <-c.IncomingMessages()
	if msg.SenderName != "siva" {
		t.Errorf("sender should resolve to a display name, got %q", msg.SenderName)
	}
	// Raw Slack IDs in the body are unreadable to both operator and model.
	if strings.Contains(msg.Content, "<@") || strings.Contains(msg.Content, "U123") {
		t.Errorf("mentions should be rendered as names, got %q", msg.Content)
	}
	if msg.Metadata["mentions_me"] != true {
		t.Error("a <@UBOT> tag must set mentions_me")
	}
	if msg.Metadata["mention_count"] != 2 {
		t.Errorf("mention_count should be 2, got %v", msg.Metadata["mention_count"])
	}
	if msg.Metadata["is_group"] != true {
		t.Error("a channel message is a group message")
	}
}

func TestRouteDMIsNotAGroup(t *testing.T) {
	c := newTestChannel(map[string]string{"U123": "siva"})
	c.route(newEvent("ping", "U123", "D42", "im"))

	msg := <-c.IncomingMessages()
	if msg.Metadata["is_group"] != false {
		t.Error("a DM must not be marked as a group")
	}
	if msg.Metadata["mentions_me"] != false {
		t.Error("no tag means mentions_me is false, even in a DM")
	}
}

func TestRouteIgnoresBotsAndSubtypes(t *testing.T) {
	c := newTestChannel(nil)

	// Our own posts come back over the socket; replying to them would loop.
	self := newEvent("something I said", "UBOT", "C99", "channel")
	c.route(self)

	bot := newEvent("deploy finished", "U777", "C99", "channel")
	bot.Event.BotID = "B123"
	c.route(bot)

	edit := newEvent("typo fixed", "U123", "C99", "channel")
	edit.Event.Subtype = "message_changed"
	c.route(edit)

	empty := newEvent("   ", "U123", "C99", "channel")
	c.route(empty)

	select {
	case msg := <-c.IncomingMessages():
		t.Fatalf("nothing should have been routed, got %q", msg.Content)
	default:
	}
}

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
	// Splitting must not lose content.
	if joined := strings.ReplaceAll(strings.Join(parts, "\n"), "\n", ""); joined != strings.ReplaceAll(long, "\n", "") {
		t.Error("chunking dropped or altered content")
	}
}
