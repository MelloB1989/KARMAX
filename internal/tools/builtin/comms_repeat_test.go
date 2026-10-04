package builtin

import (
	"testing"
	"time"
)

// One incoming message becomes several model passes — answer, load a tool,
// answer again — and each pass decided independently to reply, because nothing
// told it the earlier reply had gone out. The recipient got three different
// answers to one question. The tool now reports the earlier send back.
func TestSecondSendIsToldTheFirstOneLanded(t *testing.T) {
	tool := &CommsSendTool{}

	if _, repeat := tool.noteSent("group@g.us", "done, reminder set for 9:30 PM IST"); repeat {
		t.Fatal("the first send to a recipient is not a repeat")
	}
	previous, repeat := tool.noteSent("group@g.us", "done, reminder set for 9:30 AM EDT")
	if !repeat {
		t.Fatal("a second send moments later must be reported as a repeat")
	}
	if previous != "done, reminder set for 9:30 PM IST" {
		t.Errorf("the model must be told what it already said, got %q", previous)
	}
	// A different recipient is a different conversation.
	if _, repeat := tool.noteSent("someone-else@lid", "hello"); repeat {
		t.Error("a send to a different recipient must not count as a repeat")
	}
	// Case and spacing in the target must not create a second identity.
	if _, repeat := tool.noteSent("  GROUP@g.us ", "third"); !repeat {
		t.Error("the same recipient written differently must still be recognised")
	}
	// Old entries expire rather than accumulating for the life of the process.
	tool.sent["group@g.us"] = sentNote{at: time.Now().Add(-alreadySaidWindow - time.Minute), text: "stale"}
	if _, repeat := tool.noteSent("group@g.us", "much later"); repeat {
		t.Error("a send long after the window is a fresh message, not a repeat")
	}
}

// A reply to a new message from the same person is a new exchange, not a repeat.
func TestASendAfterTheirNewMessageIsNotARepeat(t *testing.T) {
	heard := false
	tool := &CommsSendTool{HeardFromSince: func(string, time.Time) bool { return heard }}
	tool.noteSent("op@lid", "all good, how are you?")
	heard = true
	if _, repeat := tool.noteSent("op@lid", "my bad, noted"); repeat {
		t.Fatal("answering their correction was flagged as already replied")
	}
	heard = false
	if _, repeat := tool.noteSent("op@lid", "my bad, noted from now on"); !repeat {
		t.Fatal("a second send with nothing new from them must still be flagged")
	}
}
