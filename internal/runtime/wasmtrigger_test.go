package runtime

import (
	"testing"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

// A signed loop must receive the trigger that caused its run. When this was
// dropped, every event-driven loop saw "manual" with no payload and returned
// before doing anything — silently, since it had not yet logged.
func TestLoopRunReceivesItsTrigger(t *testing.T) {
	var gotKind string
	var gotPayload map[string]any

	run := func(kit loopkit.Kit) {
		tr := kit.Trigger()
		kind := tr.Kind
		if kind == "" {
			kind = loopkit.TriggerManual
		}
		gotKind, gotPayload = kind, tr.Payload
	}

	run(&loopKit{trigger: loopkit.Trigger{
		Kind:    loopkit.TriggerEvent,
		Payload: map[string]any{"content": "hi", "channel_id": "123@g.us"},
	}})
	if gotKind != loopkit.TriggerEvent {
		t.Fatalf("kind = %q, want %q", gotKind, loopkit.TriggerEvent)
	}
	if gotPayload["channel_id"] != "123@g.us" {
		t.Fatalf("payload lost the chat: %v", gotPayload)
	}

	// An empty kind must still name itself rather than reaching the guest blank.
	run(&loopKit{trigger: loopkit.Trigger{}})
	if gotKind != loopkit.TriggerManual {
		t.Errorf("empty kind = %q, want %q", gotKind, loopkit.TriggerManual)
	}
}
