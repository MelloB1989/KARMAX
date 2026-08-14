package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/internal/voice/sarvam"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
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
//
// It does NOT use the orchestrator's own session. That session carries the
// whole nexus persona, thirteen tool schemas and a growing history, and on
// Sonnet it took nine to ten seconds to answer "hello" — measured, twice. A
// phone call cannot wait that long: the caller assumes the line is dead and
// starts talking again.
//
// So a call gets its own session — the fast model, a short prompt, no tools —
// and its own history, which lasts exactly as long as the call.
type voiceAgent struct {
	agent   *agent.Agent
	session *karmahelper.Session
	log     *zap.Logger
}

// voicePrompt is the whole brief. Short on purpose: every token here is paid on
// every turn of the conversation, and the medium does most of the instructing.
const voicePrompt = `You are KARMAX, the operator's personal AI assistant, speaking with them ON THE PHONE.

Reply the way a person on a call does: one or two short sentences, the answer first.
No lists, no markdown, no URLs read aloud, no preamble.
If you did not catch something, say so briefly and ask them to repeat.
If they ask for something you cannot do over the phone, say you will handle it after the call.
Never invent facts about their life — if you do not know, say you will check.`

// voiceModel is the model a call speaks with, read once.
type voiceModel struct{ provider, model string }

// pickVoiceModel reads the model config ONCE, at startup.
//
// It used to be read per call, via the agent's Snapshot — which takes the
// agent's lock, and the agent holds that lock for the length of a turn. A call
// arriving while the agent was working therefore blocked before it had done
// anything at all: the relay logged that a call had connected and then nothing,
// no greeting, no audio, until the caller gave up. None of this config changes
// while the daemon runs, so none of it belongs in the call path.
func pickVoiceModel(a *agent.Agent) voiceModel {
	def := a.Snapshot().Def
	if def.MemoryModelCfg.Model != "" {
		return voiceModel{def.MemoryModelCfg.Provider, def.MemoryModelCfg.Model}
	}
	return voiceModel{def.Provider, def.Model}
}

// newVoiceSession builds the conversational half of a call.
func newVoiceSession(m voiceModel) *karmahelper.Session {
	provider, model := m.provider, m.model
	return karmahelper.NewSession(karmahelper.SessionConfig{
		Provider:     provider,
		Model:        model,
		SystemPrompt: voicePrompt,
		// One or two sentences. Also a latency control — the reply cannot be
		// slow to generate or slow to speak if it cannot be long.
		MaxTokens: 90,
	}, nil)
}

// Answer asks the agent, with the shape of the reply set by the medium.
//
// A phone call cannot be skimmed, re-read, or scrolled back: the same content
// that reads well in a message is unbearable spoken. So the instruction is part
// of the prompt rather than a hope — short sentences, no lists, no markdown,
// and the answer first.
func (v *voiceAgent) Answer(ctx context.Context, peer, said string) (string, error) {
	reply, _, _, err := v.session.Chat(ctx, strings.TrimSpace(said))
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

	model := pickVoiceModel(a)
	cfg := voice.Config{
		Sarvam: sarvam.Config{APIKey: key},
		NewAnswerer: func() voice.Answerer {
			return &voiceAgent{agent: a, session: newVoiceSession(model), log: rt.log}
		},
		Log: rt.log,
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

// voiceRelayURL is where wacli bridges a call's audio, or empty when calling is
// off. Computed from config rather than the runtime so the tool can be built
// before the runtime exists.
func voiceRelayURL(cfg *config.KarmaxConfig) string {
	if strings.TrimSpace(os.Getenv("SARVAM_API_KEY")) == "" || !cfg.Webhooks.Enabled {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d/voice", cfg.Webhooks.Port)
}

// wireCallAnswering teaches the WhatsApp channel to pick up.
//
// Ringing KARMAX and having it answer is the other half of calling — placing
// worked and answering did not exist, so the operator stood listening to their
// own assistant ring out. The channel does the deciding (it sees the webhook);
// this supplies the mechanism: bridge the ringing call to our relay.
//
// Answering is deliberately mechanical — no model in the loop. A pickup that
// waits on a routing decision is a pickup the caller gives up on.
func (rt *KarmaxRuntime) wireCallAnswering() {
	relay := voiceRelayURL(rt.cfg)
	if relay == "" || rt.waChannel == nil {
		return
	}
	endpoint := strings.TrimRight(hostpaths.WacliAPIURL(), "/") + "/calls/stream/answer"
	rt.waChannel.SetAnswerStream(func(callID string) error {
		body, err := json.Marshal(map[string]any{
			"call_id":   callID,
			"relay_url": relay,
			// The relay is loopback-only and authenticates nobody; the token is
			// the protocol's field, not a secret.
			"token":    "local",
			"language": "en-IN",
		})
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return fmt.Errorf("answer refused (%s): %.200s", resp.Status, raw)
		}
		return nil
	})
	rt.log.Info("incoming calls will be answered live", zap.String("relay", relay))
}
