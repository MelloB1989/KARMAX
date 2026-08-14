// Package turn holds the conversation state machine.
//
// It is deliberately free of sockets, audio and clocks: turn-taking is where a voice agent feels
// human or does not, and that logic deserves to be exercised without a WhatsApp call and two
// upstream providers standing behind it. The session drives it; it decides nothing about timing.
package turn

import "fmt"

// State is where the conversation currently sits.
type State int

const (
	// Idle is before the peer answers and after the call ends.
	Idle State = iota
	// Listening means the peer has the floor.
	Listening
	// Thinking means we have their words and are producing a reply.
	Thinking
	// Speaking means our audio is playing into the call.
	Speaking
)

func (s State) String() string {
	switch s {
	case Idle:
		return "idle"
	case Listening:
		return "listening"
	case Thinking:
		return "thinking"
	case Speaking:
		return "speaking"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// Event is something that happened, from the call or from an upstream provider.
type Event int

const (
	// Answered fires when the peer picks up.
	Answered Event = iota
	// PeerSpeechStart is the provider's VAD reporting the peer has begun talking.
	PeerSpeechStart
	// PeerSpeechEnd is the provider's VAD reporting they have stopped.
	PeerSpeechEnd
	// ReplyFirstAudio is the first synthesised chunk of our reply.
	ReplyFirstAudio
	// ReplyDone is the last of it.
	ReplyDone
	// QuotaExhausted means the spend ceiling has been reached mid-call.
	QuotaExhausted
	// PeerHungUp ends everything.
	PeerHungUp
)

func (e Event) String() string {
	switch e {
	case Answered:
		return "answered"
	case PeerSpeechStart:
		return "peer_speech_start"
	case PeerSpeechEnd:
		return "peer_speech_end"
	case ReplyFirstAudio:
		return "reply_first_audio"
	case ReplyDone:
		return "reply_done"
	case QuotaExhausted:
		return "quota_exhausted"
	case PeerHungUp:
		return "peer_hung_up"
	}
	return fmt.Sprintf("event(%d)", int(e))
}

// Action is what the session should do about a transition.
type Action int

const (
	// PlayGreeting plays the cached opening line, disclosure included.
	PlayGreeting Action = iota
	// BargeIn tells the phone to drop queued audio immediately.
	BargeIn
	// CancelReply abandons in-flight generation and synthesis.
	CancelReply
	// BeginReply starts generating from what the peer just said.
	BeginReply
	// ArmFiller schedules a cached acknowledgement. The session decides when to actually play it —
	// only if generation is still running after a short delay — because a filler that lands on top
	// of the real reply is worse than no filler at all.
	ArmFiller
	// WrapUp switches to a graceful close. Cutting a grandmother off mid-sentence because a spend
	// ceiling was reached is not an acceptable way to enforce a quota.
	WrapUp
	// End tears the session down.
	End
)

func (a Action) String() string {
	switch a {
	case PlayGreeting:
		return "play_greeting"
	case BargeIn:
		return "barge_in"
	case CancelReply:
		return "cancel_reply"
	case BeginReply:
		return "begin_reply"
	case ArmFiller:
		return "arm_filler"
	case WrapUp:
		return "wrap_up"
	case End:
		return "end"
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Machine is one call's turn-taking. Not safe for concurrent use; the session owns it and feeds it
// from a single goroutine.
type Machine struct {
	state    State
	wrapping bool
	ended    bool
}

// New returns a machine waiting for the peer to answer.
func New() *Machine { return &Machine{state: Idle} }

// State reports the current state.
func (m *Machine) State() State { return m.state }

// WrappingUp reports whether the call is closing out after quota exhaustion.
func (m *Machine) WrappingUp() bool { return m.wrapping }

// Handle applies one event and returns what the session should do.
func (m *Machine) Handle(e Event) []Action {
	if m.ended {
		return nil
	}

	switch e {
	case PeerHungUp:
		m.state = Idle
		m.ended = true
		return []Action{End}

	case QuotaExhausted:
		// Say goodbye properly rather than dropping the line.
		if m.wrapping {
			return nil
		}
		prev := m.state
		m.wrapping = true
		m.state = Speaking
		switch prev {
		case Thinking:
			return []Action{CancelReply, WrapUp}
		case Speaking:
			return []Action{BargeIn, WrapUp}
		default:
			return []Action{WrapUp}
		}
	}

	switch m.state {
	case Idle:
		if e == Answered {
			m.state = Speaking
			return []Action{PlayGreeting}
		}

	case Listening:
		switch e {
		case PeerSpeechEnd:
			m.state = Thinking
			return []Action{BeginReply, ArmFiller}
		case ReplyFirstAudio:
			// Synthesis that was already in flight when the peer interrupted.
			m.state = Speaking
			return nil
		}

	case Thinking:
		switch e {
		case PeerSpeechStart:
			// They carried on talking. What we were about to say answers a question they have
			// since changed, so throw it away rather than replying to a stale turn.
			m.state = Listening
			return []Action{CancelReply}
		case ReplyFirstAudio:
			m.state = Speaking
			return nil
		case ReplyDone:
			// Generation produced nothing playable.
			m.state = Listening
			return nil
		}

	case Speaking:
		switch e {
		case PeerSpeechStart:
			if m.wrapping {
				// Let the goodbye finish; there is nothing after it anyway.
				return nil
			}
			m.state = Listening
			return []Action{BargeIn, CancelReply}
		case ReplyDone:
			if m.wrapping {
				m.state = Idle
				m.ended = true
				return []Action{End}
			}
			m.state = Listening
			return nil
		case PeerSpeechEnd:
			// VAD can report the tail of the peer's previous utterance while we are already
			// replying. Nothing to do.
			return nil
		}
	}

	return nil
}
