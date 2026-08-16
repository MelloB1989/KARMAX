package voice

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// The conversation server: one WebSocket per call, text both ways.
//
// An integration connects when a call goes live, sends start, then one
// utterance per settled thing the caller said; it is told what to say back.
// Utterances carry an id and replies say which id they answer, so a reply to
// something the caller has already moved past is recognisable as stale on
// both sides. A reply with no id is the brain speaking of its own accord.
// The protocol is deliberately small — anything a future integration needs
// beyond this earns its way in as a field, not a second socket.

const (
	readLimit        = 1 << 20
	handshakeTimeout = 10 * time.Second
	// maxCall bounds one conversation. A call nobody hangs up costs money for
	// as long as it runs.
	maxCall = 10 * time.Minute
)

// wire is every message in either direction.
type wire struct {
	Type        string `json:"type"`
	CallID      string `json:"call_id,omitempty"`
	Peer        string `json:"peer,omitempty"`
	PeerName    string `json:"peer_name,omitempty"`
	Language    string `json:"language,omitempty"`
	Direction   string `json:"direction,omitempty"`
	Text        string `json:"text,omitempty"`
	Reason      string `json:"reason,omitempty"`
	ID          int64  `json:"id,omitempty"`
	For         int64  `json:"for,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
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
	if e, ok := brain.(Ender); ok {
		defer e.End()
	}
	log.Info("voice: conversation started",
		zap.String("call_id", start.CallID), zap.String("peer_name", start.PeerName),
		zap.String("direction", start.Direction))

	write := func(m wire) {
		data, err := json.Marshal(m)
		if err != nil {
			return
		}
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil && ctx.Err() == nil {
			log.Warn("voice: could not write to the integration", zap.Error(err))
		}
	}
	say := func(text string, answering int64) {
		if text = strings.TrimSpace(text); text != "" {
			write(wire{Type: "say", Text: text, For: answering})
		}
	}
	say(brain.Greeting(ctx, start.Peer), 0)

	// The reader runs on its own so the newest utterance is known even while
	// an answer is being composed: latest is what makes a reply stale.
	var latest atomic.Int64
	msgs := make(chan wire, 16)
	go func() {
		defer close(msgs)
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
			if m.Type == "utterance" && m.ID > latest.Load() {
				latest.Store(m.ID)
			}
			select {
			case msgs <- m:
			case <-ctx.Done():
				return
			}
		}
	}()

	var notices <-chan Reply
	if n, ok := brain.(Notifier); ok {
		notices = n.Notices()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case notice, ok := <-notices:
			if !ok {
				notices = nil
				continue
			}
			log.Info("voice: speaking unprompted", zap.String("reply", notice.Text))
			say(notice.Text, 0)
			if notice.Hangup {
				write(wire{Type: "hangup"})
				return
			}
		case m, ok := <-msgs:
			if !ok {
				return
			}
			switch m.Type {
			case "utterance":
				text := strings.TrimSpace(m.Text)
				if text == "" {
					continue
				}
				log.Info("voice: heard the caller", zap.String("said", text), zap.Bool("interrupted", m.Interrupted))
				started := time.Now()
				reply, err := brain.Answer(ctx, Utterance{
					CallID: start.CallID, Peer: start.Peer, PeerName: start.PeerName,
					Language: start.Language, Text: text, Interrupted: m.Interrupted,
				})
				if err != nil {
					log.Warn("voice: the brain could not answer", zap.Error(err))
					// Said out loud rather than swallowed: silence on a phone
					// reads as a dropped line, and the caller talks over the
					// recovery.
					reply = Reply{Text: "Sorry — something went wrong on my side."}
				}
				// The caller spoke again while this was being composed. What
				// they said next is already queued, and it supersedes this: a
				// conversation that answers what the caller has moved past
				// falls one turn behind and stays there.
				if m.ID != 0 && latest.Load() > m.ID {
					log.Info("voice: reply superseded before it was spoken",
						zap.Int64("for", m.ID), zap.Int64("latest", latest.Load()))
					continue
				}
				log.Info("voice: replying",
					zap.Duration("took", time.Since(started).Round(time.Millisecond)),
					zap.String("reply", reply.Text))
				say(reply.Text, m.ID)
				if reply.Hangup {
					write(wire{Type: "hangup"})
					return
				}
			case "ended":
				log.Info("voice: conversation ended", zap.String("reason", m.Reason))
				return
			}
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
