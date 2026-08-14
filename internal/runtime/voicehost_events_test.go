package runtime

import "testing"

func TestMissingCallEventsFindsWhatAnsweringNeeds(t *testing.T) {
	// The agent re-registering the webhook with message events only is how
	// answering silently died; the repair has to see exactly what is missing.
	missing := missingCallEvents([]any{"incoming_message", "outgoing_message"})
	if len(missing) != 2 {
		t.Fatalf("missing = %v", missing)
	}
	if got := missingCallEvents([]any{"incoming_message", "call.incoming", "call.ended"}); len(got) != 0 {
		t.Errorf("a complete subscription needs no repair, got %v", got)
	}
	// A malformed events field must read as everything-missing, not as fine.
	if got := missingCallEvents(nil); len(got) != 2 {
		t.Errorf("nil events should miss both, got %v", got)
	}
}
