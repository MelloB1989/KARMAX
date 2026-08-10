package telegram

import (
	"strings"
	"testing"

	"github.com/PaulSonOfLars/gotgbot/v2"
	"go.uber.org/zap"
)

func newTestChannel() *Channel {
	c := New("telegram-test", "token", zap.NewNop())
	c.botID, c.botUser = 42, "karmaxbot"
	return c
}

func msg(m *gotgbot.Message) gotgbot.Update { return gotgbot.Update{Message: m} }

// Bots — including ourselves — are ignored, or KARMAX answers its own messages.
func TestBotsAreIgnored(t *testing.T) {
	c := newTestChannel()
	c.route(msg(&gotgbot.Message{
		From: &gotgbot.User{Id: 42, IsBot: true}, Text: "hello",
		Chat: gotgbot.Chat{Id: 1, Type: "private"},
	}))
	select {
	case got := <-c.IncomingMessages():
		t.Fatalf("a bot's message was routed: %q", got.Content)
	default:
	}
}

// Media with no caption still wakes the agent, with a marker — otherwise a
// photo somebody sent looks exactly like nothing happening.
func TestMediaWithoutTextStillArrives(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    *gotgbot.Message
		want string
	}{
		{"a photo", &gotgbot.Message{Photo: []gotgbot.PhotoSize{{}}}, "photo"},
		{"a file", &gotgbot.Message{Document: &gotgbot.Document{FileName: "notes.pdf"}}, "notes.pdf"},
		{"a voice note", &gotgbot.Message{Voice: &gotgbot.Voice{}}, "voice note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestChannel()
			tc.m.From = &gotgbot.User{Id: 7, FirstName: "Maya"}
			tc.m.Chat = gotgbot.Chat{Id: 1, Type: "private"}
			c.route(msg(tc.m))
			select {
			case got := <-c.IncomingMessages():
				if !strings.Contains(got.Content, tc.want) {
					t.Errorf("content = %q, want a mention of %q", got.Content, tc.want)
				}
			default:
				t.Fatal("media was dropped silently")
			}
		})
	}
}

// Being addressed is decided here, not by the model, because it changes whether
// KARMAX speaks at all.
func TestBeingAddressedIsDecidedInCode(t *testing.T) {
	c := newTestChannel()
	c.route(msg(&gotgbot.Message{
		From: &gotgbot.User{Id: 7, Username: "maya"}, Text: "hey @KarmaxBot can you check",
		Chat: gotgbot.Chat{Id: -100, Type: "supergroup", Title: "Team"},
	}))
	got := <-c.IncomingMessages()
	if got.Metadata["mentions_me"] != true {
		t.Error("an @mention of the bot was not recognised")
	}
	if got.Metadata["is_group"] != true {
		t.Error("a supergroup was not recognised as a group")
	}

	// A reply to one of our messages is equally "addressed to us".
	c2 := newTestChannel()
	c2.route(msg(&gotgbot.Message{
		From: &gotgbot.User{Id: 7}, Text: "yes please",
		Chat:           gotgbot.Chat{Id: -100, Type: "group"},
		ReplyToMessage: &gotgbot.Message{From: &gotgbot.User{Id: 42}},
	}))
	got2 := <-c2.IncomingMessages()
	if got2.Metadata["quoted_is_from_me"] != true {
		t.Error("a reply to our own message was not recognised")
	}
}

// A long answer is split rather than rejected by Telegram's limit.
func TestLongMessagesAreSplit(t *testing.T) {
	parts := Chunks(strings.Repeat("a line\n", 2000), maxMessage)
	if len(parts) < 2 {
		t.Fatalf("got %d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > maxMessage {
			t.Errorf("part %d is %d bytes, over the limit", i, len(p))
		}
	}
}
