package voice

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// The conversation server: one WebSocket per call, text both ways.
//
// An integration connects when a call goes live, sends start, then one
// utterance per settled thing the caller said; it is told what to say back.
// The protocol is deliberately three message types each way — anything a
// future integration needs beyond this earns its way in as a message type,
// not a second socket.

const (
	readLimit        = 1 << 20
	handshakeTimeout = 10 * time.Second
	// maxCall bounds one conversation. A call nobody hangs up costs money for
	// as long as it runs.
	maxCall = 10 * time.Minute
)

// wire is every message in either direction.
type wire struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id,omitempty"`
	Peer      string `json:"peer,omitempty"`
	PeerName  string `json:"peer_name,omitempty"`
	Language  string `json:"language,omitempty"`
	Direction string `json:"direction,omitempty"`
	Text      string `json:"text,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ServeConversation runs one call's conversation to completion. It always
// closes conn.
func ServeConversation(ctx context.Context, conn *websocket.Conn, factory Factory, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	if factory == nil {
		log.Error("voice: no brain factory; refusing the call")
		conn.CloseNow()
		return
	}
	conn.SetReadLimit(readLimit)
	ctx, cancel := context.WithTimeout(ctx, maxCall)
	defer cancel()
	defer conn.CloseNow()

	start, err := readStart(ctx, conn)
	if err != nil {
		log.Warn("voice: handshake failed", zap.Error(err))
		return
	}
	brain := factory()
	log.Info("voice: conversation started",
		zap.String("call_id", start.CallID), zap.String("peer_name", start.PeerName),
		zap.String("direction", start.Direction))

	say := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		data, err := json.Marshal(wire{Type: "say", Text: text})
		if err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil && ctx.Err() == nil {
			log.Warn("voice: could not send the reply", zap.Error(err))
		}
	}

	say(brain.Greeting(ctx, start.Peer))

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m wire
		if err := json.Unmarshal(data, &m); err != nil {
			log.Warn("voice: bad message from the integration", zap.Error(err))
			continue
		}
		switch m.Type {
		case "utterance":
			text := strings.TrimSpace(m.Text)
			if text == "" {
				continue
			}
			log.Info("voice: heard the caller", zap.String("said", text))
			started := time.Now()
			reply, err := brain.Answer(ctx, Utterance{
				CallID: start.CallID, Peer: start.Peer, PeerName: start.PeerName,
				Language: start.Language, Text: text,
			})
			if err != nil {
				log.Warn("voice: the brain could not answer", zap.Error(err))
				// Said out loud rather than swallowed: silence on a phone reads
				// as a dropped line, and the caller talks over the recovery.
				reply = Reply{Text: "Sorry — something went wrong on my side."}
			}
			log.Info("voice: replying",
				zap.Duration("took", time.Since(started).Round(time.Millisecond)),
				zap.String("reply", reply.Text))
			say(reply.Text)
			if reply.Hangup {
				data, _ := json.Marshal(wire{Type: "hangup"})
				_ = conn.Write(ctx, websocket.MessageText, data)
				return
			}
		case "ended":
			log.Info("voice: conversation ended", zap.String("reason", m.Reason))
			return
		}
	}
}

// readStart waits for the opening frame.
func readStart(ctx context.Context, conn *websocket.Conn) (wire, error) {
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	for {
		kind, data, err := conn.Read(hctx)
		if err != nil {
			return wire{}, err
		}
		if kind != websocket.MessageText {
			continue
		}
		var m wire
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		if m.Type == "start" {
			return m, nil
		}
	}
}
