package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/voice"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// The agent, on the phone.
//
// Integrations own the audio; this file owns what gets said and which
// integration says it. The brain is the same agent that answers WhatsApp text —
// same memory, same operator — because a second brain for voice is a second set
// of facts to keep in sync. wacli's WhatsApp calling is the first registered
// integration; anything else that can hold a call registers the same way.

// voicePrompt is the whole brief. Short on purpose: every token here is paid on
// every turn of the conversation, and the medium does most of the instructing.
const voicePrompt = `You are KARMAX, the operator's personal AI assistant, speaking with them ON THE PHONE.

Reply the way a person on a call does: one or two short sentences, the answer first.
No lists, no markdown, no URLs read aloud, no preamble.
If you did not catch something, say so briefly and ask them to repeat.
If they ask for something you cannot do over the phone, say you will handle it after the call.
Never invent facts about their life — if you do not know, say you will check.`

// voiceBrain answers a call with a dedicated fast session.
//
// Not the orchestrator's own session: that carries the whole persona, a dozen
// tool schemas and a growing history, and took nine to ten seconds to answer
// "hello" — measured. A call gets the fast model, this five-line prompt, no
// tools, and history that lasts exactly as long as the call.
type voiceBrain struct {
	session *karmahelper.Session
}

func (b *voiceBrain) Greeting(ctx context.Context, peer string) string {
	return "Hey, it's KARMAX. What do you need?"
}

func (b *voiceBrain) Answer(ctx context.Context, u voice.Utterance) (voice.Reply, error) {
	text, _, _, err := b.session.Chat(ctx, u.Text)
	if err != nil {
		return voice.Reply{}, err
	}
	return voice.Reply{Text: speakable(text)}, nil
}

// voiceModel is the model a call speaks with, read once at startup — reading it
// per call went through the agent's lock, which the agent holds for a whole
// turn, and a call arriving mid-turn hung before it had done anything.
type voiceModel struct{ provider, model string }

func pickVoiceModel(a *agent.Agent) voiceModel {
	def := a.Snapshot().Def
	if def.MemoryModelCfg.Model != "" {
		return voiceModel{def.MemoryModelCfg.Provider, def.MemoryModelCfg.Model}
	}
	return voiceModel{def.Provider, def.Model}
}

func newVoiceFactory(m voiceModel) voice.Factory {
	return func() voice.Brain {
		return &voiceBrain{session: karmahelper.NewSession(karmahelper.SessionConfig{
			Provider:     m.provider,
			Model:        m.model,
			SystemPrompt: voicePrompt,
			// One or two sentences. Also a latency control — the reply cannot
			// be slow to generate or slow to speak if it cannot be long.
			MaxTokens: 90,
		}, nil)}
	}
}

// speakable strips what a synthesiser should not read out. The agent writes for
// a screen everywhere else and its habits come with it: asterisks read as
// "asterisk", a bullet list becomes a monotone.
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

// wacliVoice is the WhatsApp calling integration, spoken for by wacli.
type wacliVoice struct {
	apiURL   string
	brainURL string
}

func (w *wacliVoice) Name() string { return "whatsapp" }

func (w *wacliVoice) Place(ctx context.Context, to string, opts voice.CallOptions) error {
	body := map[string]any{"to": to, "brain_url": w.brainURL}
	if opts.Language != "" {
		body["language"] = opts.Language
	}
	if opts.Voice != "" {
		body["voice"] = opts.Voice
	}
	if opts.RingFor > 0 {
		body["ring_for_seconds"] = opts.RingFor
	}
	return w.post(ctx, "/calls/spoken", body)
}

// Answer picks up a ringing call. Not part of the Provider interface — the
// integration hears its own ring — but the WhatsApp channel delegates the
// mechanism here.
func (w *wacliVoice) Answer(ctx context.Context, callID string) error {
	return w.post(ctx, "/calls/spoken/answer", map[string]any{
		"call_id": callID, "brain_url": w.brainURL, "language": "en-IN",
	})
}

func (w *wacliVoice) post(ctx context.Context, path string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(w.apiURL, "/")+path, bytes.NewReader(payload))
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
		// The bridge says which half failed, and that is the part worth acting on.
		return fmt.Errorf("wacli refused (%s): %.300s", resp.Status, raw)
	}
	return nil
}

// brainURL is where integrations hold their conversations, or empty when voice
// is off. Computed from config so the tool can exist before the runtime does.
func brainURL(cfg *config.KarmaxConfig) string {
	if strings.TrimSpace(os.Getenv("SARVAM_API_KEY")) == "" || !cfg.Webhooks.Enabled {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d/voice", cfg.Webhooks.Port)
}

// mountVoice serves the conversation endpoint and registers the integrations.
func (rt *KarmaxRuntime) mountVoice(wh interface {
	AddHandler(string, http.HandlerFunc)
}, a *agent.Agent) {
	url := brainURL(rt.cfg)
	if url == "" {
		rt.log.Info("voice is off: SARVAM_API_KEY is not set or webhooks are disabled")
		return
	}
	if a == nil {
		rt.log.Warn("voice is off: no agent to speak as")
		return
	}

	factory := newVoiceFactory(pickVoiceModel(a))
	wh.AddHandler("/voice", func(w http.ResponseWriter, r *http.Request) {
		// Loopback only. The brain speaks for the operator and authenticates
		// nobody — integrations reach it across localhost, and nothing else
		// should reach it at all.
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "the voice brain is local-only", http.StatusForbidden)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			rt.log.Warn("voice: could not upgrade the connection", zap.Error(err))
			return
		}
		rt.log.Info("voice: an integration connected")
		voice.ServeConversation(r.Context(), conn, factory, rt.log)
	})

	wa := &wacliVoice{apiURL: hostpaths.WacliAPIURL(), brainURL: url}
	rt.voice.Register(wa)
	rt.log.Info("voice ready", zap.Strings("integrations", rt.voice.Names()), zap.String("brain", url))

	// Teach the WhatsApp channel to pick up. Answering is mechanical — no model
	// in the loop, because a pickup that waits on a routing decision is one the
	// caller gives up on.
	if rt.waChannel != nil {
		rt.waChannel.SetAnswerStream(func(callID string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return wa.Answer(ctx, callID)
		})
		rt.log.Info("incoming calls will be answered live")
	}
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

// voiceAgentID names the agent that speaks on calls: the WhatsApp channel's
// agent, or the first configured one.
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
