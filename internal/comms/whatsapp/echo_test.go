package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/internal/comms"
	"go.uber.org/zap"
)

// A message sent through wacli directly — a shell's `wacli send`, a
// workflow's whatsapp_send_message — never passed through comms, so nothing
// recorded it, and nothing could tell it had been sent. wacli echoes every
// send it makes as an outgoing_message from "wacli_api"; that echo is now
// handed to comms to record. What the operator typed on their own phone
// ("whatsapp_event") is theirs, not KARMAX's, and is not.
func TestARawSendIsEchoedToCommsAndThePhoneIsNot(t *testing.T) {
	w := New("wa", "/bin/true", "", "", zap.NewNop())
	var got []comms.Echo
	w.SetEchoSink(func(e comms.Echo) { got = append(got, e) })

	sent := time.Now().Add(-time.Second)
	w.routeEvent(wacliWebhookEnvelope{Event: "outgoing_message", Payload: wacliWebhookPayload{
		Chat:    wacliChat{JID: "120363000000000001@g.us", IsGroup: true},
		Message: wacliMessage{ID: "3EB0A1", Content: "Here is the Northwind deck.", Timestamp: sent, IsFromMe: true},
		Source:  "wacli_api",
	}})
	w.routeEvent(wacliWebhookEnvelope{Event: "outgoing_message", Payload: wacliWebhookPayload{
		Chat:    wacliChat{JID: "120363000000000001@g.us"},
		Message: wacliMessage{ID: "3EB0A2", MessageType: "document", FileName: "northwind-deck.pdf", Timestamp: sent, IsFromMe: true},
		Source:  "wacli_api",
	}})
	w.routeEvent(wacliWebhookEnvelope{Event: "outgoing_message", Payload: wacliWebhookPayload{
		Chat:    wacliChat{JID: "120363000000000001@g.us"},
		Message: wacliMessage{ID: "3EB0A3", Content: "typed on the phone", Timestamp: sent, IsFromMe: true},
		Source:  "whatsapp_event",
	}})

	if len(got) != 2 {
		t.Fatalf("echoes = %+v, want the two raw sends only", got)
	}
	if got[0].Target != "120363000000000001@g.us" || got[0].Content != "Here is the Northwind deck." || got[0].MessageID != "3EB0A1" {
		t.Errorf("text echo = %+v", got[0])
	}
	if got[1].Content != comms.FileMessage("northwind-deck.pdf") {
		t.Errorf("file echo = %+v, want it recorded as the file", got[1])
	}
	select {
	case m := <-w.inbox:
		t.Errorf("an outgoing message was routed to the agent: %+v", m)
	default:
	}
}

// wacli's own history answers whether a message went out: what a delivery
// cut off between the send and its record asks before sending again.
func TestSentSinceAsksWaclisOwnHistory(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "wacli")
	body := "#!/bin/bash\n" +
		"echo \"$@\" > " + filepath.Join(dir, "args") + "\n" +
		`echo '{"messages":[{"id":"1","content":"Here is the Northwind deck.","is_from_me":true},{"id":"2","content":"","file_name":"northwind-deck.pdf","is_from_me":true}]}'` + "\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	w := New("wa", bin, "", "", zap.NewNop())
	since := time.Now().Add(-time.Hour)
	ctx := context.Background()

	if ok, err := w.SentSince(ctx, "120363000000000001@g.us", " Here is the Northwind deck. ", since); err != nil || !ok {
		t.Errorf("a sent text was not found: %v %v", ok, err)
	}
	if ok, _ := w.SentSince(ctx, "120363000000000001@g.us", comms.FileMessage("/srv/out/northwind-deck.pdf"), since); !ok {
		t.Error("a sent file was not found")
	}
	if ok, _ := w.SentSince(ctx, "120363000000000001@g.us", "something never sent", since); ok {
		t.Error("an unsent message was found")
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"messages", "--chat 120363000000000001@g.us", "--from-me yes", "--after"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("wacli was asked %q, missing %q", args, want)
		}
	}
}
