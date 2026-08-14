package turn

import (
	"slices"
	"testing"
)

func drive(t *testing.T, m *Machine, events ...Event) {
	t.Helper()
	for _, e := range events {
		m.Handle(e)
	}
}

func wantActions(t *testing.T, got []Action, want ...Action) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
}

func TestGreetingOnAnswer(t *testing.T) {
	m := New()
	if m.State() != Idle {
		t.Fatalf("initial state = %v, want idle", m.State())
	}
	wantActions(t, m.Handle(Answered), PlayGreeting)
	if m.State() != Speaking {
		t.Fatalf("state = %v, want speaking", m.State())
	}
}

func TestNormalTurnCycle(t *testing.T) {
	m := New()
	drive(t, m, Answered)

	// Greeting finishes, floor passes to the peer.
	wantActions(t, m.Handle(ReplyDone))
	if m.State() != Listening {
		t.Fatalf("state = %v, want listening", m.State())
	}

	// They speak, then stop: generate a reply and arm the filler.
	m.Handle(PeerSpeechStart)
	wantActions(t, m.Handle(PeerSpeechEnd), BeginReply, ArmFiller)
	if m.State() != Thinking {
		t.Fatalf("state = %v, want thinking", m.State())
	}

	wantActions(t, m.Handle(ReplyFirstAudio))
	if m.State() != Speaking {
		t.Fatalf("state = %v, want speaking", m.State())
	}

	wantActions(t, m.Handle(ReplyDone))
	if m.State() != Listening {
		t.Fatalf("state = %v, want listening", m.State())
	}
}

func TestBargeInWhileSpeaking(t *testing.T) {
	m := New()
	drive(t, m, Answered)

	// The peer talks over us — stop immediately and drop the queued audio.
	wantActions(t, m.Handle(PeerSpeechStart), BargeIn, CancelReply)
	if m.State() != Listening {
		t.Fatalf("state = %v, want listening", m.State())
	}
}

func TestPeerKeepsTalkingWhileThinking(t *testing.T) {
	m := New()
	drive(t, m, Answered, ReplyDone, PeerSpeechStart, PeerSpeechEnd)
	if m.State() != Thinking {
		t.Fatalf("setup: state = %v, want thinking", m.State())
	}

	// They resumed, so the reply we were about to give answers a stale turn.
	wantActions(t, m.Handle(PeerSpeechStart), CancelReply)
	if m.State() != Listening {
		t.Fatalf("state = %v, want listening", m.State())
	}
}

func TestQuotaWrapsUpGracefully(t *testing.T) {
	m := New()
	drive(t, m, Answered, ReplyDone, PeerSpeechStart, PeerSpeechEnd)

	// Mid-generation the ceiling is hit: abandon the reply, say goodbye instead.
	wantActions(t, m.Handle(QuotaExhausted), CancelReply, WrapUp)
	if !m.WrappingUp() {
		t.Fatal("expected the machine to be wrapping up")
	}
	if m.State() != Speaking {
		t.Fatalf("state = %v, want speaking", m.State())
	}

	// The peer interrupting the goodbye must not restart the conversation.
	wantActions(t, m.Handle(PeerSpeechStart))
	if m.State() != Speaking {
		t.Fatalf("state = %v, want speaking", m.State())
	}

	// Once the goodbye finishes, the call ends rather than returning to listening.
	wantActions(t, m.Handle(ReplyDone), End)
	if m.State() != Idle {
		t.Fatalf("state = %v, want idle", m.State())
	}
}

func TestQuotaWhileSpeakingCutsCurrentLine(t *testing.T) {
	m := New()
	drive(t, m, Answered)
	wantActions(t, m.Handle(QuotaExhausted), BargeIn, WrapUp)
}

func TestQuotaWhileListeningJustWrapsUp(t *testing.T) {
	m := New()
	drive(t, m, Answered, ReplyDone)
	if m.State() != Listening {
		t.Fatalf("setup: state = %v", m.State())
	}
	wantActions(t, m.Handle(QuotaExhausted), WrapUp)
}

func TestQuotaIsIdempotent(t *testing.T) {
	m := New()
	drive(t, m, Answered)
	m.Handle(QuotaExhausted)
	wantActions(t, m.Handle(QuotaExhausted))
}

func TestHangupEndsFromAnyState(t *testing.T) {
	for _, setup := range [][]Event{
		{},
		{Answered},
		{Answered, ReplyDone},
		{Answered, ReplyDone, PeerSpeechStart, PeerSpeechEnd},
	} {
		m := New()
		drive(t, m, setup...)
		wantActions(t, m.Handle(PeerHungUp), End)
		if m.State() != Idle {
			t.Fatalf("after hangup state = %v, want idle", m.State())
		}
	}
}

func TestEventsAfterEndAreIgnored(t *testing.T) {
	m := New()
	drive(t, m, Answered)
	m.Handle(PeerHungUp)

	for _, e := range []Event{Answered, PeerSpeechStart, PeerSpeechEnd, ReplyFirstAudio, ReplyDone} {
		if got := m.Handle(e); got != nil {
			t.Fatalf("%v after end produced %v, want nil", e, got)
		}
	}
}

func TestSpeechEndWhileSpeakingIsIgnored(t *testing.T) {
	m := New()
	drive(t, m, Answered)
	// VAD can report the tail of an earlier utterance after we have started replying.
	wantActions(t, m.Handle(PeerSpeechEnd))
	if m.State() != Speaking {
		t.Fatalf("state = %v, want speaking", m.State())
	}
}

func TestStringsAreStable(t *testing.T) {
	// These land in logs and in the app's live call view.
	if Listening.String() != "listening" || PeerSpeechEnd.String() != "peer_speech_end" || BargeIn.String() != "barge_in" {
		t.Fatal("state/event/action names changed unexpectedly")
	}
}
