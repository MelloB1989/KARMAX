// Package voice runs a spoken conversation over a call.
//
// wacli holds the call and moves the audio; this holds the conversation. One
// WebSocket carries both: binary frames are 16 kHz mono s16le PCM, text frames
// are JSON control. The protocol is wacli's, defined by wa/voicerelay.go on its
// voicestream branch.
//
// The relay lives inside KARMAX rather than beside it. The design it came from
// put it on a server because an APK cannot hold a provider key and because
// splitting speech, reasoning and synthesis across an ocean is slow — neither
// applies when the daemon, the bridge and the key are already on one machine.
// Here the conversation IS the agent, with its memory and its tools.
package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/internal/voice/sarvam"
	"github.com/MelloB1989/karmax/internal/voice/turn"
	"github.com/coder/websocket"
	"go.uber.org/zap"
)

const (
	// readLimit bounds one inbound message. Audio frames are 1,920 bytes; the
	// limit is here so a confused client cannot exhaust memory.
	readLimit = 1 << 20
	// handshakeTimeout bounds the wait for the opening hello.
	handshakeTimeout = 10 * time.Second
	// maxCall bounds a single conversation. A call nobody hangs up costs money
	// for as long as it runs.
	maxCall = 10 * time.Minute
)

// Answerer turns what the caller said into what to say back.
//
// The agent behind it holds memory and tools, so this is deliberately the whole
// interface: the relay's job is audio and turn-taking, not deciding anything.
type Answerer interface {
	// Answer is called once per completed utterance. Returning an empty string
	// says nothing, which is the right response to a cough.
	Answer(ctx context.Context, peer, said string) (string, error)
	// Greeting opens the call before the caller has said anything.
	Greeting(ctx context.Context, peer string) string
}

// Config is what a relay needs to run.
type Config struct {
	Sarvam sarvam.Config
	Agent  Answerer
	Log    *zap.Logger
}

// hello is the opening frame wacli sends.
type hello struct {
	Type     string   `json:"type"`
	Token    string   `json:"token"`
	Peer     string   `json:"peer"`
	Language string   `json:"language"`
	Voice    string   `json:"voice"`
	Cached   []string `json:"cached,omitempty"`
}

// event is every text frame in either direction.
type event struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Final  bool   `json:"final,omitempty"`
	State  string `json:"state,omitempty"`
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// session is one live call.
type session struct {
	cfg  Config
	conn *websocket.Conn
	log  *zap.Logger

	peer string
	mu   sync.Mutex

	stt *sarvam.STT
	tts *sarvam.TTS
	fsm *turn.Machine
}

// Serve runs one conversation to completion. It always closes conn.
func Serve(ctx context.Context, conn *websocket.Conn, cfg Config) {
	conn.SetReadLimit(readLimit)
	log := cfg.Log
	if log == nil {
		log = zap.NewNop()
	}
	s := &session{cfg: cfg, conn: conn, log: log, fsm: turn.New()}

	ctx, cancel := context.WithTimeout(ctx, maxCall)
	defer cancel()
	defer conn.CloseNow()

	h, err := s.handshake(ctx)
	if err != nil {
		s.log.Warn("voice: handshake failed", zap.Error(err))
		return
	}
	s.peer = h.Peer

	if err := s.dial(ctx, h); err != nil {
		s.log.Warn("voice: could not reach the speech provider", zap.Error(err))
		s.send(ctx, event{Type: "end", Reason: "speech provider unavailable"})
		return
	}
	defer s.stt.Close()
	defer s.tts.Close()

	s.send(ctx, event{Type: "state", State: "connected"})
	if greeting := strings.TrimSpace(s.cfg.Agent.Greeting(ctx, s.peer)); greeting != "" {
		s.say(ctx, greeting)
	}

	go s.pumpAudio(ctx)
	go s.pumpSpeech(ctx)
	s.readPhone(ctx)
}

// handshake reads the opening frame.
func (s *session) handshake(ctx context.Context) (hello, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	kind, data, err := s.conn.Read(hctx)
	if err != nil {
		return hello{}, err
	}
	if kind != websocket.MessageText {
		return hello{}, fmt.Errorf("expected a text hello, got a binary frame")
	}
	var h hello
	if err := json.Unmarshal(data, &h); err != nil {
		return hello{}, err
	}
	if h.Type != "hello" {
		return hello{}, fmt.Errorf("expected hello, got %q", h.Type)
	}
	return h, nil
}

// dial opens the transcription and synthesis sockets.
func (s *session) dial(ctx context.Context, h hello) error {
	language := strings.TrimSpace(h.Language)
	if language == "" {
		language = "en-IN"
	}
	stt, err := sarvam.DialSTT(ctx, s.cfg.Sarvam, language)
	if err != nil {
		return fmt.Errorf("transcription: %w", err)
	}
	tts, err := sarvam.DialTTS(ctx, s.cfg.Sarvam, language, strings.TrimSpace(h.Voice), 0)
	if err != nil {
		stt.Close()
		return fmt.Errorf("synthesis: %w", err)
	}
	s.stt, s.tts = stt, tts
	return nil
}

// readPhone forwards the caller's audio to the transcriber until the call ends.
func (s *session) readPhone(ctx context.Context) {
	for {
		kind, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		switch kind {
		case websocket.MessageBinary:
			if err := s.stt.Send(ctx, data); err != nil {
				s.log.Debug("voice: could not forward audio", zap.Error(err))
				return
			}
		case websocket.MessageText:
			var ev event
			if json.Unmarshal(data, &ev) == nil && ev.Type == "end" {
				return
			}
		}
	}
}

// pumpAudio forwards synthesised speech to the call.
func (s *session) pumpAudio(ctx context.Context) {
	for chunk := range s.tts.Audio() {
		if err := s.conn.Write(ctx, websocket.MessageBinary, chunk); err != nil {
			return
		}
	}
}

// pumpSpeech turns what the caller said into a reply.
//
// Barge-in is reported the instant the caller starts talking, before any
// transcript exists — the point is to stop KARMAX mid-sentence, and waiting for
// words would talk over them for the length of a phrase.
func (s *session) pumpSpeech(ctx context.Context) {
	var heard strings.Builder
	for ev := range s.stt.Events() {
		switch ev.Kind {
		case sarvam.SpeechStart:
			s.send(ctx, event{Type: "barge_in"})

		case sarvam.Transcript:
			if ev.Final {
				heard.WriteString(ev.Text)
				heard.WriteByte(' ')
			}
			s.send(ctx, event{Type: "transcript", Text: ev.Text, Final: ev.Final})

		case sarvam.SpeechEnd:
			said := strings.TrimSpace(heard.String())
			heard.Reset()
			if said == "" {
				continue
			}
			s.answer(ctx, said)
		}
	}
}

// answer asks the agent and speaks the reply.
func (s *session) answer(ctx context.Context, said string) {
	s.send(ctx, event{Type: "state", State: "thinking"})
	reply, err := s.cfg.Agent.Answer(ctx, s.peer, said)
	if err != nil {
		s.log.Warn("voice: the agent could not answer", zap.Error(err))
		// Said out loud rather than swallowed: silence on a phone call reads as
		// a dropped line, and the caller will start talking over the recovery.
		reply = "Sorry — something went wrong on my side."
	}
	if strings.TrimSpace(reply) == "" {
		s.send(ctx, event{Type: "state", State: "listening"})
		return
	}
	s.say(ctx, reply)
}

// say synthesises one utterance.
func (s *session) say(ctx context.Context, text string) {
	s.send(ctx, event{Type: "state", State: "speaking"})
	if err := s.tts.Speak(ctx, text); err != nil {
		s.log.Warn("voice: could not speak", zap.Error(err))
		return
	}
	if err := s.tts.Flush(ctx); err != nil {
		s.log.Debug("voice: flush failed", zap.Error(err))
	}
}

// send writes one control frame, serialised because several goroutines report.
func (s *session) send(ctx context.Context, ev event) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.Write(ctx, websocket.MessageText, data)
}
