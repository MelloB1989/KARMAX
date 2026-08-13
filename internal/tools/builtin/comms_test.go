package builtin

import (
	"context"
	"strings"
	"testing"
)

func sendTool(sent *[]string) *CommsSendTool {
	return &CommsSendTool{
		SendFunc: func(channelID, target, content string) error {
			*sent = append(*sent, channelID+"|"+target)
			return nil
		},
		DefaultChannelID: func() (string, bool) { return "whatsapp-main", true },
		KnownChannelID:   func(id string) bool { return id == "whatsapp-main" },
	}
}

func TestCommsSendRejectsAChannelIDAsRecipient(t *testing.T) {
	var sent []string
	res, err := sendTool(&sent).Execute(context.Background(), map[string]any{
		"target": "whatsapp-main", "content": "hi",
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("sending to a channel id must fail rather than reach the transport")
	}
	if len(sent) != 0 {
		t.Errorf("nothing should have been sent, got %v", sent)
	}
	// The message has to say what to do instead, or the model retries the same
	// call and the failure becomes a loop.
	for _, want := range []string{"channel", "not a recipient", "channel_id"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("error should mention %q, got: %s", want, res.Error)
		}
	}
}

func TestCommsSendAllowsARealRecipient(t *testing.T) {
	var sent []string
	res, err := sendTool(&sent).Execute(context.Background(), map[string]any{
		"target": "917671837092", "content": "hi",
	})
	if err != nil || res.IsError {
		t.Fatalf("a real target must send: %v %v", err, res.Error)
	}
	if len(sent) != 1 || sent[0] != "whatsapp-main|917671837092" {
		t.Errorf("sent = %v", sent)
	}
}
