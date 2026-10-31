package store

import (
	"testing"
	"time"
)

// KARMAX acts as the operator on other services too, through the operator's
// own login, so the service shows its messages as the operator's. The sent-as
// ledger is how they are told apart.
func TestSentAsOperatorKnowsWhatKarmaxSaid(t *testing.T) {
	s := newTestStore(t)
	for _, m := range []StoredChannelMessage{
		{ID: "1", ChannelID: "spaces/AAAA", ChannelType: "gchat", SenderName: "quinn@example.org", Direction: "outbound", Content: "I'll send the brief tonight"},
		{ID: "2", ChannelID: "dana@example.org", ChannelType: "gchat", SenderName: "quinn@example.org", Direction: "outbound", Content: "Done, see the doc"},
		{ID: "3", ChannelID: "#ops", ChannelType: "slack", SenderName: "quinn@example.org", Direction: "outbound", Content: "on it"},
	} {
		if err := s.SaveChannelMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	cases := []struct {
		service, account, text string
		at                     time.Time
		want                   bool
	}{
		{"gchat", "quinn@example.org", "I'll send the brief tonight", now, true},
		{"gchat", "Quinn@Example.org", "  I'll send the brief\n tonight ", now, true},
		{"gchat", "quinn@example.org", "Done, see the doc", now, true},
		{"gchat", "quinn@example.org", "something the operator typed", now, false},
		{"gchat", "quinn@example.net", "I'll send the brief tonight", now, false},
		{"gchat", "quinn@example.org", "on it", now, false},
		{"gchat", "quinn@example.org", "I'll send the brief tonight", now.Add(-2 * time.Hour), false},
	}
	for _, c := range cases {
		if got := s.SentAsOperator(c.service, c.account, c.text, c.at); got != c.want {
			t.Errorf("SentAsOperator(%q, %q, %q) = %v, want %v", c.service, c.account, c.text, got, c.want)
		}
	}
}
