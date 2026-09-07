package karmahelper

import (
	"errors"
	"testing"
)

// Running out of tool passes is an unfinished turn, not a failed one: the model
// called tools until the budget ran out and never wrote the answer. Treated as
// a failure, everything it just learned is discarded and the operator is told
// nothing — 101 turns died that way in three days on this machine.
func TestPassExhaustionIsRecognised(t *testing.T) {
	for _, err := range []error{
		errors.New("exceeded tool execution passes"),
		errors.New("non-retryable error: exceeded tool execution passes"),
		errors.New("exceeded tool execution passes: tool call parsing failed: bad json"),
	} {
		if !isToolPassExhaustion(err) {
			t.Errorf("not recognised, so the turn stays silent: %v", err)
		}
	}
}

// It must stay distinct from a real breakage: answering with tools off is the
// right move for an unfinished turn and the wrong one for a dead endpoint,
// which needs the fallback models and then the out-of-band path.
func TestOtherFailuresAreNotMistakenForPassExhaustion(t *testing.T) {
	for _, err := range []error{
		errors.New("429 Too Many Requests: rate_limit_exceeded"),
		errors.New("dial tcp 127.0.0.1:9000: connection refused"),
		errors.New("context deadline exceeded"),
		nil,
	} {
		if isToolPassExhaustion(err) {
			t.Errorf("treated as pass exhaustion: %v", err)
		}
	}
	// And the transport detector must not claim this one.
	if isTransportFailure(errors.New("exceeded tool execution passes")) {
		t.Error("pass exhaustion was classed as a transport failure")
	}
}
