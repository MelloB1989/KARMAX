package store

import (
	"testing"
	"time"
)

// KARMAX speaks through the operator's own WhatsApp account, so wacli shows its
// messages as the operator's. What KARMAX itself sent is how they are told apart.
func TestSentWhatsAppKnowsWhatKarmaxSaid(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []StoredChannelMessage{
		{ID: "1", ChannelID: "917000000001", ChannelType: "whatsapp", Direction: "outbound", Content: "I'll send the brief tonight"},
		{ID: "2", ChannelID: "5790000000001@lid", ChannelType: "whatsapp", Direction: "outbound", Content: "Done ✅"},
		{ID: "3", ChannelID: "917000000002", ChannelType: "whatsapp", Direction: "inbound", Content: "can we move the call?"},
	} {
		if err := s.SaveChannelMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	cases := []struct {
		chat, text string
		at         time.Time
		want       bool
	}{
		{"917000000001@s.whatsapp.net", "I'll send the brief tonight", now, true},
		{"917000000001@s.whatsapp.net", "  I'll send the brief tonight\n", now, true},
		{"5790000000001:3@lid", "Done ✅", now, true},
		{"917000000001@s.whatsapp.net", "something the operator typed", now, false},
		{"917000000009@s.whatsapp.net", "I'll send the brief tonight", now, false},
		{"917000000001@s.whatsapp.net", "I'll send the brief tonight", now.Add(-2 * time.Hour), false},
		{"917000000002@s.whatsapp.net", "can we move the call?", now, false},
	}
	for _, c := range cases {
		if got := s.SentWhatsApp(c.chat, c.text, c.at); got != c.want {
			t.Errorf("SentWhatsApp(%q, %q, %v) = %v, want %v", c.chat, c.text, c.at.Sub(now), got, c.want)
		}
	}
}
