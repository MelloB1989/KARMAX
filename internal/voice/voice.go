// Package voice is the standard a voice integration plugs into.
//
// The audio never comes here. An integration — wacli's WhatsApp calling today,
// anything with a microphone tomorrow — owns transport, media, transcription,
// synthesis, turn-taking and barge-in, and crosses this boundary with TEXT: it
// reports what the caller said and is told what to say back. That is the whole
// contract, and it is what lets one Brain serve every integration while
// integrations vary freely underneath.
//
// The first cut streamed raw audio into KARMAX and ran the speech pipeline
// here. It worked, and every part of it was in the wrong place: speech belongs
// with the thing holding the call, and the daemon's job is deciding what to
// say. What remains here is exactly that decision, plus the registry that lets
// several integrations offer calls at once.
package voice

import "context"

// Utterance is one settled thing the caller said.
type Utterance struct {
	CallID   string
	Peer     string
	PeerName string
	Language string
	Text     string
}

// Reply is what to do about it.
type Reply struct {
	// Text is spoken to the caller. Empty says nothing, which is the right
	// response to a cough.
	Text string
	// Hangup ends the call after Text (if any) is spoken.
	Hangup bool
}

// Brain decides what a call says. One conversation, one Brain: implementations
// carry per-call history and are built fresh by a Factory.
type Brain interface {
	// Greeting opens the call before the caller has said anything.
	Greeting(ctx context.Context, peer string) string
	// Answer is called once per settled utterance.
	Answer(ctx context.Context, u Utterance) (Reply, error)
}

// Factory builds the brain for one call. A conversation has its own history,
// and one shared across calls would let yesterday's call answer today's.
type Factory func() Brain

// CallOptions shape an outgoing call.
type CallOptions struct {
	Language string
	Voice    string
	RingFor  int // seconds; zero means the integration's default
}

// Provider is a voice integration that can place calls. Answering is the
// integration's own affair — it hears the ring, it holds the media — so the
// interface carries only what KARMAX initiates.
type Provider interface {
	Name() string
	Place(ctx context.Context, to string, opts CallOptions) error
}
