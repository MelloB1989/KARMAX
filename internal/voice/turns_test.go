package voice

import (
	"testing"

	"go.uber.org/zap"
)

func TestOfferKeepsOnlyTheNewestUtterance(t *testing.T) {
	// Observed on a live call: "Hi", "Hi", then a question. The reply to the
	// first arrived after the second was spoken, the reply to the second after
	// the question, and the question went unanswered. A queue is what made the
	// conversation run a turn behind.
	s := &session{turns: make(chan string, 1), log: zap.NewNop()}

	s.offer("Hi")
	s.offer("Hi again")
	s.offer("what are my pending tasks?")

	got := <-s.turns
	if got != "what are my pending tasks?" {
		t.Errorf("runner should get the newest utterance, got %q", got)
	}
	select {
	case extra := <-s.turns:
		t.Errorf("nothing stale should remain, found %q", extra)
	default:
	}
}

func TestOfferNeverBlocks(t *testing.T) {
	// The STT goroutine calls this; blocking it would stall transcription for
	// the whole call.
	s := &session{turns: make(chan string, 1), log: zap.NewNop()}
	for i := 0; i < 50; i++ {
		s.offer("utterance")
	}
}
