package tools

import (
	"strings"
	"testing"
)

type sendingTool struct {
	Tool
	rec SentRecorder
}

func (s *sendingTool) RecordSentWith(r SentRecorder) { s.rec = r }

type plainTool struct{ Tool }

func (plainTool) Manifest() ToolManifest { return ToolManifest{Name: "plain.read"} }

func (s *sendingTool) Manifest() ToolManifest { return ToolManifest{Name: "mail.send"} }

// The runtime hands its one recorder to every registered tool that sends as
// the operator, whatever it is, without knowing it by name.
func TestTheRecorderReachesEveryRegisteredToolThatSends(t *testing.T) {
	reg := NewRegistry()
	sender := &sendingTool{}
	reg.Register(sender)
	reg.Register(plainTool{})
	var got []SentAs
	if n := reg.GiveSentRecorder(func(s SentAs) { got = append(got, s) }); n != 1 {
		t.Errorf("gave the recorder to %d tools, want the one that sends", n)
	}
	sender.rec.Record(SentAs{Service: "email", Text: "hi"})
	if len(got) != 1 {
		t.Fatalf("the sending tool's records did not arrive: %+v", got)
	}
}

// A tool built without a recorder still acts; it just records nothing.
func TestANilRecorderRecordsNothing(t *testing.T) {
	var r SentRecorder
	r.Record(SentAs{Service: "slack", Text: "hi"}) // must not panic
}

// What reaches the ledger reads the same whichever tool acted: what was done,
// then what was said.
func TestWhatWasSentReadsAsActionThenWords(t *testing.T) {
	cases := []struct {
		s    SentAs
		want string
	}{
		{SentAs{Action: "commented on", Text: "Looks  good\nonce CI passes."}, "commented on: Looks good once CI passes."},
		{SentAs{Action: "merged"}, "merged"},
		{SentAs{Text: "On my way."}, "On my way."},
	}
	for _, c := range cases {
		if got := c.s.Content(); got != c.want {
			t.Errorf("Content() = %q, want %q", got, c.want)
		}
	}
	var got []SentAs
	r := SentRecorder(func(s SentAs) { got = append(got, s) })
	r.Record(SentAs{Service: "gchat", Account: "me@example.org", To: "spaces/AAAA", Text: "done"})
	if len(got) != 1 || !strings.Contains(got[0].Content(), "done") {
		t.Errorf("recorded %+v", got)
	}
}
