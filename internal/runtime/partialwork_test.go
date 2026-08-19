package runtime

import (
	"errors"
	"fmt"
	"testing"
)

// A retry is only safe for work that has not happened. "Call Kartik until he
// responds" was carried out three times because a timeout at minute twelve
// discarded the knowledge that minute one had already acted; the failure type
// is how that knowledge survives to the retry decision.
func TestPartialWorkSurvivesWrapping(t *testing.T) {
	inner := fmt.Errorf("wasmloop: wa-monitor ran past its 12m0s limit and was stopped")
	err := error(&partialWorkFailure{err: inner, sends: 3})
	// The scheduler wraps errors as it reports them; the marker must survive.
	wrapped := fmt.Errorf("loop run: %w", err)

	var p *partialWorkFailure
	if !errors.As(wrapped, &p) {
		t.Fatal("the partial-work marker must be recoverable from a wrapped error")
	}
	if p.sends != 3 {
		t.Errorf("sends = %d, want 3", p.sends)
	}
	// And the original cause stays readable for the log.
	if !errors.Is(wrapped, inner) {
		t.Error("the underlying failure must remain reachable")
	}
}

// Only tools that land in front of another human count. An internal read
// repeating is free; a send repeating is the bug this exists to stop.
func TestOutboundClassification(t *testing.T) {
	for _, name := range []string{"whatsapp_send_message", "comms_send", "call_start", "linkedin_post"} {
		if !outboundTools[name] {
			t.Errorf("%s reaches a human and must count as outbound", name)
		}
	}
	for _, name := range []string{"memory_ingest", "whatsapp_search_messages", "app_push", "recall"} {
		if outboundTools[name] {
			t.Errorf("%s does not reach a third party and must not block retries", name)
		}
	}
}
