package slack

import (
	"context"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// PostThread refuses to guess a channel, and never makes a network call to
// find that out — the check happens before the API call, not after it fails.
func TestPostThreadRequiresAChannel(t *testing.T) {
	c := &Channel{api: slack.New("xoxb-test")}
	if _, err := c.PostThread(context.Background(), "", "", "hi"); err == nil {
		t.Fatal("PostThread() with no channel should have failed")
	}
}

// A channel with no live connection refuses to post rather than panicking on
// a nil client.
func TestPostThreadRequiresAConnection(t *testing.T) {
	c := &Channel{}
	if _, err := c.PostThread(context.Background(), "C1", "", "hi"); err == nil {
		t.Fatal("PostThread() with no connection should have failed")
	}
}

// Reading a thread needs both a channel and a ts to know which thread.
func TestThreadRepliesRequiresChannelAndTS(t *testing.T) {
	c := &Channel{api: slack.New("xoxb-test")}
	if _, err := c.ThreadReplies(context.Background(), "", ""); err == nil {
		t.Fatal("ThreadReplies() with nothing to read should have failed")
	}
	if _, err := c.ThreadReplies(context.Background(), "C1", ""); err == nil {
		t.Fatal("ThreadReplies() with no thread ts should have failed")
	}
}

// The rendered thread is fenced, and readable — names are used over bare ids
// when one is known.
func TestFormatThreadRepliesFencesAndRendersNames(t *testing.T) {
	out := FormatThreadReplies("C1", []ThreadReply{
		{User: "U1", UserName: "maya", Text: "can you check the deploy"},
		{User: "U2", Text: "looking now"}, // no resolved name: falls back to the id
	})
	if !strings.Contains(out, "maya: can you check the deploy") {
		t.Errorf("expected the resolved name to be used, got: %s", out)
	}
	if !strings.Contains(out, "U2: looking now") {
		t.Errorf("expected the id fallback when no name is known, got: %s", out)
	}
	if !strings.Contains(out, "untrusted-content") {
		t.Errorf("expected the thread to be fenced, got: %s", out)
	}
}
