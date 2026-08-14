package runtime

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/internal/voice/sarvam"
	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// The agent, on the phone.
//
// wacli bridges the call to this endpoint and moves the audio; everything about
// what is actually said happens here, through the same agent that answers
// WhatsApp — same memory, same tools, same operator. A second brain for voice
// would be a second set of facts to keep in sync.

// voiceAgent adapts the agent to a spoken conversation.
type voiceAgent struct {
	agent *agent.Agent
	log   *zap.Logger
}

// Answer asks the agent, with the shape of the reply set by the medium.
//
// A phone call cannot be skimmed, re-read, or scrolled back: the same content
// that reads well in a message is unbearable spoken. So the instruction is part
// of the prompt rather than a hope — short sentences, no lists, no markdown,
// and the answer first.
func (v *voiceAgent) Answer(ctx context.Context, peer, said string) (string, error) {
	prompt := "You are ON A PHONE CALL with the operator. They just said:\n\n" +
		strings.TrimSpace(said) + "\n\n" +
		"Reply as speech: one or two short sentences, the answer first, no lists, no markdown, " +
		"no URLs read aloud. If you need to do something, do it with your tools and say what you did. " +
		"If you did not understand, say so briefly and ask them to repeat."
	reply, err := v.agent.Chat(ctx, prompt)
	if err != nil {
		return "", err
	}
	return speakable(reply), nil
}

// Greeting opens the call.
func (v *voiceAgent) Greeting(ctx context.Context, peer string) string {
	return "Hey, it's KARMAX. What do you need?"
}

// speakable strips what a synthesiser should not read out.
//
// The agent writes for a screen everywhere else, and its habits come with it:
// asterisks read as "asterisk", a bare URL is spelled character by character,
// and a bullet list becomes a monotone. Cheaper to clean here than to hope the
// model never does it.
func speakable(s string) string {
	s = strings.NewReplacer("**", "", "*", "", "`", "", "#", "", "_", " ").Replace(s)
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-•→ "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, " ")
}

// mountVoice serves the relay wacli bridges calls to.
func (rt *KarmaxRuntime) mountVoice(wh interface {
	AddHandler(string, http.HandlerFunc)
}, a *agent.Agent) {
	key := strings.TrimSpace(os.Getenv("SARVAM_API_KEY"))
	if key == "" {
		rt.log.Info("voice calls are off: SARVAM_API_KEY is not set")
		return
	}
	if a == nil {
		rt.log.Warn("voice calls are off: no agent to answer them")
		return
	}

	cfg := voice.Config{
		Sarvam: sarvam.Config{APIKey: key},
		Agent:  &voiceAgent{agent: a, log: rt.log},
		Log:    rt.log,
	}

	wh.AddHandler("/voice", func(w http.ResponseWriter, r *http.Request) {
		// Loopback only. The relay speaks for the operator with their memory and
		// their tools, and it authenticates nobody — wacli reaches it across
		// localhost, and nothing else should reach it at all.
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "voice relay is local-only", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			rt.log.Warn("voice: could not upgrade the connection", zap.Error(err))
			return
		}
		rt.log.Info("voice: a call connected")
		voice.Serve(r.Context(), conn, cfg)
	})
	rt.log.Info("voice relay listening", zap.String("path", "/voice"))
}

// isLoopback reports whether a request came from this machine.
func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// VoiceRelayURL is where wacli should bridge a call to.
func (rt *KarmaxRuntime) VoiceRelayURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/voice", rt.cfg.Webhooks.Port)
}

// voiceAgentID names the agent that answers calls: the WhatsApp channel's
// agent, or the first configured one. The caller reached the operator's number,
// so they should get the agent that already speaks for it.
func (rt *KarmaxRuntime) voiceAgentID() string {
	for _, ch := range rt.cfg.Comms.Channels {
		if strings.EqualFold(ch.Type, "whatsapp") && ch.AgentID != "" {
			return ch.AgentID
		}
	}
	if len(rt.cfg.Agents) > 0 {
		return rt.cfg.Agents[0].ID
	}
	return ""
}
