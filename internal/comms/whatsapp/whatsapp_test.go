package whatsapp

import (
	"encoding/json"
	"testing"
)

func TestCallEventsSurviveEnvelopeDecoding(t *testing.T) {
	// The typed payload only knows the message shape, and decoding a call
	// through it silently discarded call_id and peer_jid — the phone rang out
	// while the handler saw an event about nobody.
	body := []byte(`{"event":"call.incoming","payload":{"call_id":"ABC123","peer_jid":"5794649083972@lid","peer_name":"Kartik","direction":"incoming","state":"ringing","video":false}}`)

	var env wacliWebhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Event != "call.incoming" {
		t.Errorf("event = %q", env.Event)
	}
	var call wacliCallPayload
	if err := json.Unmarshal(env.Raw, &call); err != nil {
		t.Fatalf("raw payload lost: %v", err)
	}
	if call.CallID != "ABC123" || call.PeerJID != "5794649083972@lid" {
		t.Errorf("call fields dropped: %+v", call)
	}

	// And a message envelope still decodes the typed view.
	msg := []byte(`{"event":"incoming_message","payload":{"chat":{"jid":"x@lid","name":"X"},"message":{"id":"m1","content":"hi"},"source":"whatsapp_event"}}`)
	if err := json.Unmarshal(msg, &env); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if env.Payload.Message.Content != "hi" || env.Payload.Chat.JID != "x@lid" {
		t.Errorf("message view broken: %+v", env.Payload)
	}
}
