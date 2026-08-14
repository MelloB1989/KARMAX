// Package sarvam wraps the streaming speech APIs.
//
// Both directions are WebSockets carrying JSON with base64 audio inside. The formats line up with
// what a WhatsApp call already carries — signed 16-bit little-endian, 16 kHz, mono — so nothing
// here resamples in either direction.
//
// WIRE FORMAT: verified against the live sockets, not inferred from docs. Two details are worth
// knowing because neither is guessable and both fail loudly only at runtime: the per-message audio
// `encoding` enum accepts "audio/wav" and nothing else even when the bytes are raw PCM (the codec
// is chosen once, at connect time), and inbound messages come in two shapes — "events" carrying a
// VAD signal_type, and "data" carrying a settled transcript. There is no partial/final flag.
package sarvam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
)

const (
	// SampleRate and mono channels are fixed by what the call carries.
	SampleRate = 16000

	// DefaultSTTModel handles transcription and translation across Indian languages.
	DefaultSTTModel = "saaras:v3"
	// DefaultTTSModel is Bulbul v3.
	DefaultTTSModel = "bulbul:v3"
	// DefaultSpeaker is a warm female voice. bulbul:v3 serves only a subset of the full speaker
	// catalogue — several names the API otherwise accepts are rejected against this model.
	DefaultSpeaker = "ritu"

	readLimit    = 1 << 22
	dialTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
)

// Config carries the credentials and endpoints for both clients.
type Config struct {
	APIKey   string
	STTURL   string
	TTSURL   string
	STTModel string
	TTSModel string
}

// EventKind distinguishes what arrived from the transcriber.
type EventKind int

const (
	// SpeechStart is the VAD reporting the peer began talking. It is what triggers barge-in.
	SpeechStart EventKind = iota
	// SpeechEnd is the VAD reporting they stopped. It is what starts a reply.
	SpeechEnd
	// Transcript carries recognised words, partial or final.
	Transcript
)

// Event is one message from the transcriber.
type Event struct {
	Kind  EventKind
	Text  string
	Final bool
}

// STT is a live transcription socket.
type STT struct {
	conn   *websocket.Conn
	events chan Event
}

// sttAudio is one chunk of the peer's voice going up.
//
// The chunk is nested under "audio", and `encoding` is an enum whose only accepted value is
// "audio/wav" — even when the payload is raw PCM. Raw PCM is selected once, at connect time, by the
// input_audio_codec query parameter; the per-message label stays "audio/wav" regardless. Sending
// the honest "audio/x-raw" here is rejected outright.
type sttAudio struct {
	Audio sttAudioBody `json:"audio"`
}

type sttAudioBody struct {
	Data       string `json:"data"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

// sttMessage is anything coming back down.
//
// Two shapes share the socket: VAD signals arrive as type "events" carrying a signal_type, and
// recognised speech arrives as type "data" carrying a transcript. There is no partial/final flag —
// a "data" message is a settled utterance.
type sttMessage struct {
	Type string `json:"type"`
	Data struct {
		SignalType string  `json:"signal_type"`
		Transcript string  `json:"transcript"`
		OccurredAt float64 `json:"occured_at"`
	} `json:"data"`
}

// DialSTT opens a transcription socket for one call.
//
// VAD signals are requested explicitly: without them the relay would have to do its own endpointing
// over audio it has already shipped upstream, which is both duplicated work and slower to react.
func DialSTT(ctx context.Context, cfg Config, language string) (*STT, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("sarvam: api key is required")
	}
	model := cfg.STTModel
	if model == "" {
		model = DefaultSTTModel
	}

	q := url.Values{}
	q.Set("model", model)
	q.Set("language-code", language)
	q.Set("mode", "transcribe")
	q.Set("input_audio_codec", "pcm_s16le")
	q.Set("sample_rate", fmt.Sprint(SampleRate))
	q.Set("vad_signals", "true")
	q.Set("high_vad_sensitivity", "true")

	endpoint := cfg.STTURL
	if endpoint == "" {
		endpoint = "wss://api.sarvam.ai/speech-to-text/ws"
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, endpoint+"?"+q.Encode(), &websocket.DialOptions{
		HTTPHeader: http.Header{"api-subscription-key": []string{cfg.APIKey}},
	})
	if err != nil {
		return nil, fmt.Errorf("sarvam stt dial: %w", err)
	}
	conn.SetReadLimit(readLimit)

	s := &STT{conn: conn, events: make(chan Event, 32)}
	go s.read(ctx)
	return s, nil
}

// Send ships one chunk of peer audio.
//
// Audio goes up continuously rather than being held until the peer stops talking. By the time the
// VAD reports the end of speech, transcription is essentially already done — which takes the
// recogniser off the critical path between their last word and our first.
func (s *STT) Send(ctx context.Context, pcm []byte) error {
	msg, err := json.Marshal(sttAudio{Audio: sttAudioBody{
		Data:       base64.StdEncoding.EncodeToString(pcm),
		Encoding:   "audio/wav",
		SampleRate: SampleRate,
	}})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return s.conn.Write(wctx, websocket.MessageText, msg)
}

// Events yields transcription events until the socket closes.
func (s *STT) Events() <-chan Event { return s.events }

// read pumps the socket into the event channel.
func (s *STT) read(ctx context.Context) {
	defer close(s.events)
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		var m sttMessage
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		ev, ok := m.event()
		if !ok {
			continue
		}
		select {
		case s.events <- ev:
		case <-ctx.Done():
			return
		}
	}
}

// event maps a wire message onto an Event, reporting false for anything not worth surfacing.
func (m sttMessage) event() (Event, bool) {
	switch m.Type {
	case "events":
		switch m.Data.SignalType {
		case "START_SPEECH":
			return Event{Kind: SpeechStart}, true
		case "END_SPEECH":
			return Event{Kind: SpeechEnd}, true
		}
	case "data":
		if m.Data.Transcript == "" {
			return Event{}, false
		}
		return Event{Kind: Transcript, Text: m.Data.Transcript, Final: true}, true
	}
	return Event{}, false
}

// Close shuts the socket down.
func (s *STT) Close() error {
	return s.conn.Close(websocket.StatusNormalClosure, "")
}
