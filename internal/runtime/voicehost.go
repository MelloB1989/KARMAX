package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/agent"
	"github.com/MelloB1989/karmax/internal/config"
	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/memory"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
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

A summary of what you remember about them is in your context — answer from it directly.
For anything it does not cover, call memory.lookup ONCE with a couple of keywords; it is fast.
When they tell you something worth keeping — a decision, a plan, a preference — save it with
memory.ingest. After a save, your reply is ONLY the short confirmation ("noted") — do not
revisit earlier topics.
If they ask for real work — send a message, check email or calendar, look something up online,
write code, research — hand it to KARMAX with orchestrator.send RIGHT AWAY and tell them it is
being done; you will be told when it finishes and can then say the outcome. KARMAX reaches people
by name and has every contact, so never ask the caller for a number or an id, and never read a
number, id or link aloud. Never claim work is done unless you were told so.
When the conversation is over — they say bye, that's all, hang up, or go quiet after thanking
you — say a brief goodbye and call call.hangup in the same turn.
If a turn is marked as interrupting your last reply, they did not hear all of it: do not repeat
it wholesale, just continue naturally from what they said.
Never invent facts about their life. If memory has nothing, say so plainly.`

// voiceBrain answers a call with a dedicated fast session.
//
// Not the orchestrator's own session: that carries the whole persona, a dozen
// tool schemas and a growing history, and took nine to ten seconds to answer
// "hello" — measured. A call gets the fast model, this five-line prompt, no
// tools, and history that lasts exactly as long as the call.
type voiceBrain struct {
	session *karmahelper.Session
	// lookup and brief power pre-answer retrieval: memory relevant to the
	// utterance is fetched BEFORE the model runs — a millisecond store search —
	// so most questions answer in one pass instead of model → tool → model.
	// The tool stays for what keyword overlap misses.
	lookup *voiceMemoryLookup
	brief  string

	// notices is what the brain says unprompted — a handed-off task coming
	// back mid-call. done closes when the call ends, after which results go
	// to the operator's chat instead of a line nobody is on.
	notices chan voice.Reply
	done    chan struct{}
	endOnce sync.Once
	// hangup is set by the call.hangup tool and consumed by the next reply.
	hangup atomic.Bool
	// delegate runs a request through the orchestrator — the agent with all
	// the tools — and deliver reaches the operator once the call is over.
	delegate func(ctx context.Context, request string) (string, error)
	deliver  func(text string) error
	inFlight atomic.Int32
	log      *zap.Logger
}

// maxDelegations bounds work handed off from one call. A caller who asks for
// five things gets five; a runaway loop does not get fifty.
const maxDelegations = 5

func (b *voiceBrain) Notices() <-chan voice.Reply { return b.notices }

func (b *voiceBrain) End() {
	b.endOnce.Do(func() { close(b.done) })
}

func (b *voiceBrain) Greeting(ctx context.Context, peer string) string {
	const greeting = "Hey, it's KARMAX. What do you need?"
	// Seeded into the session as the opening assistant turn. Without it the
	// model's first exposure is a bare instruction on empty history — and on
	// exactly those cold first turns it was observed ignoring a save request
	// entirely while handling the same request fine mid-conversation.
	b.session.SetHistory(models.AIChatHistory{Messages: []models.AIMessage{{
		Role: models.Assistant, Message: greeting,
	}}})
	return greeting
}

func (b *voiceBrain) Answer(ctx context.Context, u voice.Utterance) (voice.Reply, error) {
	// Retrieval before generation: whatever memory the utterance's own words
	// reach is in context before the model speaks, so it rarely needs the tool
	// round-trip — the difference between one model pass and two, which on a
	// phone is the difference between an answer and a pause.
	if hits := b.lookup.linesFor(u.Text, 5); len(hits) > 0 {
		b.session.SetContext(b.brief + "\n## Memory matching what they just said\n- " +
			strings.Join(hits, "\n- ") + "\n")
	} else {
		b.session.SetContext(b.brief)
	}
	if u.Interrupted {
		b.markLastReplyUnheard()
	}
	text, calls, _, err := b.session.Chat(ctx, u.Text)
	if err != nil {
		return voice.Reply{}, err
	}
	if len(calls) > 0 {
		names := make([]string, 0, len(calls))
		for _, c := range calls {
			names = append(names, c.Name)
		}
		b.log.Info("voice: the brain used tools", zap.Strings("tools", names))
	}
	return voice.Reply{Text: speakable(text), Hangup: b.hangup.Swap(false)}, nil
}

// markLastReplyUnheard annotates the previous assistant turn so the model
// knows the caller cut it off. Without this the history says the reply was
// delivered in full, and the model builds on words the caller never heard.
func (b *voiceBrain) markLastReplyUnheard() {
	h := b.session.GetHistory()
	for i := len(h.Messages) - 1; i >= 0; i-- {
		if h.Messages[i].Role != models.Assistant {
			continue
		}
		if !strings.HasPrefix(h.Messages[i].Message, "(the caller interrupted this") {
			h.Messages[i].Message = "(the caller interrupted this before hearing all of it) " + h.Messages[i].Message
			b.session.SetHistory(h)
		}
		return
	}
}

// voiceHangupTool ends the call after the current reply.
type voiceHangupTool struct{ brain *voiceBrain }

func (t *voiceHangupTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "call.hangup",
		Description: "End this call once your current reply has been spoken. Call it in the same " +
			"turn as your goodbye, when the caller has said bye, that's all, hang up, or is clearly done.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func (t *voiceHangupTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	t.brain.hangup.Store(true)
	return tools.SuccessResult(map[string]any{
		"status": "will hang up after this reply", "note": "say your goodbye now; keep it to one short sentence",
	}), nil
}

// voiceDelegateTool hands real work to the orchestrator without holding the
// line for it.
type voiceDelegateTool struct{ brain *voiceBrain }

func (t *voiceDelegateTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "orchestrator.send",
		Description: "Hand a task or message to KARMAX, the main assistant with all the tools — sending " +
			"messages, email, calendar, web research, code, anything beyond memory. It runs in the " +
			"background: you keep talking, and when it finishes you are told the outcome to relay. If the " +
			"call ends first, the outcome is sent to the operator as a message. Say the request fully, in " +
			"one message, with every detail the caller gave.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"request": {"type": "string", "description": "The complete task or message, as the caller meant it, with names, times and details."}
			},
			"required": ["request"]
		}`),
	}
}

func (t *voiceDelegateTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	request, _ := input["request"].(string)
	request = strings.TrimSpace(request)
	if request == "" {
		return tools.ErrorResult(fmt.Errorf("say what to do")), nil
	}
	b := t.brain
	if b.delegate == nil {
		return tools.ErrorResult(fmt.Errorf("the orchestrator is not available on this instance")), nil
	}
	if b.inFlight.Load() >= maxDelegations {
		return tools.ErrorResult(fmt.Errorf("too many tasks already running from this call; wait for one to finish")), nil
	}
	b.inFlight.Add(1)
	go b.runDelegation(request)
	return tools.SuccessResult(map[string]any{
		"status": "handed to KARMAX; it is working on it now",
		"note":   "tell the caller it is being done and you will say when it is finished — do not claim it is done",
	}), nil
}

// runDelegation runs one handed-off request to completion and routes the
// outcome to wherever the caller is: the call if it is still up, their chat if
// it is not. That second path is also what keeps the promise "I'll handle it"
// — before this, nothing did.
func (b *voiceBrain) runDelegation(request string) {
	defer b.inFlight.Add(-1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	prompt := "The operator asked for this on a phone call with your voice assistant, which is relaying " +
		"it to you now. Do it, then reply with the outcome in one or two plain spoken sentences — no " +
		"markdown, no lists, no links read aloud — because it will be spoken back to them if they are " +
		"still on the call.\n\nRequest: " + request
	out, err := b.delegate(ctx, prompt)
	var text string
	switch {
	case err != nil:
		b.log.Warn("voice: a handed-off task failed", zap.String("request", request), zap.Error(err))
		text = "The task I handed off did not go through: " + speakable(err.Error())
	default:
		text = speakable(out)
	}
	if strings.TrimSpace(text) == "" {
		text = "That task is done."
	}
	select {
	case <-b.done:
		// The call is over. Their chat is where they will look.
		if b.deliver != nil {
			if derr := b.deliver("📞 From your call — " + text); derr != nil {
				b.log.Warn("voice: could not deliver a task result after the call", zap.Error(derr))
			}
		}
	default:
		select {
		case b.notices <- voice.Reply{Text: text}:
		case <-b.done:
			if b.deliver != nil {
				_ = b.deliver("📞 From your call — " + text)
			}
		case <-time.After(30 * time.Second):
			if b.deliver != nil {
				_ = b.deliver("📞 From your call — " + text)
			}
		}
	}
}

// voiceModel is the model a call speaks with, read once at startup — reading it
// per call went through the agent's lock, which the agent holds for a whole
// turn, and a call arriving mid-turn hung before it had done anything.
type voiceModel struct {
	provider, model string
	namespace       string
	fallbacks       []karmahelper.FallbackModel
}

func pickVoiceModel(a *agent.Agent) voiceModel {
	def := a.Snapshot().Def
	ns := def.Memory.Namespace
	if ns == "" {
		ns = def.ID
	}
	// A fallback, because a transient provider error on a phone call otherwise
	// becomes an apology. NOT the agent's own list verbatim: probing showed it
	// carries a model this transport 400s on instantly and a duplicate of the
	// voice primary, so "fallback" meant erroring once and retrying the same
	// pool. The main model is the one real alternative.
	var fallbacks []karmahelper.FallbackModel
	if def.Model != "" && def.MemoryModelCfg.Model != "" && def.Model != def.MemoryModelCfg.Model {
		fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: def.Provider, Model: def.Model})
	}
	if def.MemoryModelCfg.Model != "" {
		return voiceModel{def.MemoryModelCfg.Provider, def.MemoryModelCfg.Model, ns, fallbacks}
	}
	return voiceModel{def.Provider, def.Model, ns, fallbacks}
}

func newVoiceFactory(rt *KarmaxRuntime, a *agent.Agent, m voiceModel) voice.Factory {
	return func() voice.Brain {
		// The brain gets the two memory verbs and nothing else. Lookup is a
		// purpose-built fast read — the agent's own memory.retrieve is a
		// sub-agent that traverses the index for seconds, which is a fine cost
		// in a chat and a dead line on a phone. Ingest is the agent's OWN bound
		// tool, so a fact said on a call lands in the same memory everything
		// else reads.
		brain := &voiceBrain{
			notices: make(chan voice.Reply, 4),
			done:    make(chan struct{}),
			log:     rt.log,
			deliver: rt.messageOperator,
			// The orchestrator's own turn, with its own tools — the difference
			// between the brain that talks and the agent that does.
			delegate: func(ctx context.Context, request string) (string, error) {
				out, _, err := a.ChatDetailed(ctx, request, nil)
				return out, err
			},
		}
		mem := rt.memory.For(a.Snapshot().Def.ID, m.namespace)
		voiceTools := append(
			[]tools.Tool{
				&voiceMemoryLookup{store: rt.store, mem: mem, namespace: m.namespace},
				&voiceHangupTool{brain: brain},
				&voiceDelegateTool{brain: brain},
			},
			a.NamedTools("memory.ingest")...,
		)
		session := karmahelper.NewSession(karmahelper.SessionConfig{
			Kind:         "voice",
			Provider:     m.provider,
			Model:        m.model,
			SystemPrompt: voicePrompt,
			// Room for a tool call carrying a whole request. Sixty-four was a
			// latency control, and it truncated orchestrator.send's arguments
			// mid-JSON — the model saw a broken call, said "let me try that
			// again", and called it three times in one turn. Reply LENGTH is
			// held down by the prompt, which is where it belongs.
			MaxTokens: 300,
			// Three: a lookup, a hand-off, and the words. The last pass is
			// answered without tools (karma), so running out is a reply not
			// an error — but two was too few for the common shape of a
			// delegation turn and turned it into an apology.
			MaxToolPasses:  3,
			MaxRetries:     1,
			FallbackModels: m.fallbacks,
		}, voiceTools)
		// Synchronous on purpose: a few milliseconds of SQLite before the
		// greeting buys most questions a zero-lookup answer, and a context set
		// concurrently with the first turn would race the session.
		brief := memoryBrief(rt.store, mem, m.namespace)
		// A phone assistant that has to ask what day it is has already lost
		// the caller. Cheap, and it goes in the per-call brief rather than the
		// cached prefix, since it changes.
		brief = "Now: " + time.Now().Format("Monday, 2 January 2006, 3:04 PM MST") + "\n" + brief
		session.SetContext(brief)
		brain.session = session
		brain.lookup = &voiceMemoryLookup{store: rt.store, mem: mem, namespace: m.namespace}
		brain.brief = brief
		return brain
	}
}

// memoryBrief is what the brain knows before the caller says anything: the
// pinned and important facts, one line each.
func memoryBrief(s *store.Store, mem *memory.Manager, namespace string) string {
	// Whichever store is real. Reading the table directly meant the brief was
	// built from a snapshot frozen on 10 August while the rest of KARMAX had
	// moved to GitLoom — the call opened already a week out of date.
	if mem != nil {
		if entries, err := mem.Recent(80); err == nil && len(entries) > 0 {
			return renderBrief(entries)
		}
	}
	if s == nil {
		return ""
	}
	stored, err := s.ListMemoryEntries(namespace, 80)
	if err != nil || len(stored) == 0 {
		return ""
	}
	entries := make([]memory.MemoryEntry, 0, len(stored))
	for _, e := range stored {
		entries = append(entries, memory.MemoryEntry{
			Content: e.Content, Pinned: e.Pinned, Importance: e.Importance,
		})
	}
	return renderBrief(entries)
}

func renderBrief(entries []memory.MemoryEntry) string {
	var b strings.Builder
	b.WriteString("## What you remember about the operator\n")
	kept := 0
	for _, e := range entries {
		if !e.Pinned && e.Importance < 3 {
			continue
		}
		line := strings.Join(strings.Fields(e.Content), " ")
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		b.WriteString("- " + line + "\n")
		if kept++; kept >= 14 {
			break
		}
	}
	if kept == 0 {
		return ""
	}
	return b.String()
}

// voiceMemoryLookup is memory retrieval at phone speed: a direct search of the
// memory store, milliseconds, no model in the loop.
type voiceMemoryLookup struct {
	store     *store.Store
	namespace string
	// mem is the memory manager, which knows where memory actually lives.
	//
	// This read the store table directly, for speed. That table stopped being
	// written on 10 August, when GitLoom became the store — so the call was
	// answering from a frozen snapshot while every other part of KARMAX read
	// the current one, and confidently said things that had been superseded for
	// a week. Speed is not worth being wrong; the manager is asked first and the
	// table is only a fallback for an instance with no remote.
	mem *memory.Manager
}

func (t *voiceMemoryLookup) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "memory.lookup",
		Description: "Search the operator's memory by keyword — fast enough for a phone call. " +
			"Use a couple of distinctive words (a name, a project), not a sentence.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "One to three keywords."}
			},
			"required": ["query"]
		}`),
	}
}

func (t *voiceMemoryLookup) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	query, _ := input["query"].(string)
	query = strings.TrimSpace(query)
	if query == "" {
		return tools.ErrorResult(fmt.Errorf("give a keyword to search for")), nil
	}
	if t.store == nil && t.mem == nil {
		return tools.ErrorResult(fmt.Errorf("memory is not available on this instance")), nil
	}
	// Each word searched separately and merged, because the store matches by
	// substring and a two-word query only hits entries carrying the exact pair.
	seen := map[string]bool{}
	var lines []string
	for _, word := range strings.Fields(query) {
		for _, line := range t.search(word, 6) {
			if seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, line)
			if len(lines) >= 8 {
				break
			}
		}
		if len(lines) >= 8 {
			break
		}
	}
	if len(lines) == 0 {
		return tools.SuccessResult(map[string]any{
			"found": 0, "note": "nothing in memory matches — say so rather than guessing",
		}), nil
	}
	return tools.SuccessResult(map[string]any{"found": len(lines), "memories": lines}), nil
}

// speakable strips what a synthesiser should not read out. The agent writes for
// a screen everywhere else and its habits come with it: asterisks read as
// "asterisk", a bullet list becomes a monotone.
func speakable(s string) string {
	s = strings.NewReplacer("**", "", "*", "", "`", "", "#", "", "_", " ").Replace(s)
	// Identifiers are not speech. A JID, a LID, a phone number, a URL: read
	// aloud they are fifteen seconds of digits nobody wanted, and they arrive
	// because memory stores them next to the names.
	s = unspeakable.ReplaceAllString(s, "")
	// What is left of "his number is 9198…" once the digits go is "his
	// number is ." — drop the stump too, keeping the punctuation.
	s = danglingIdentifier.ReplaceAllString(s, "$1")
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-•→ "))
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, " ")
}

// unspeakable matches things that must never be read aloud: WhatsApp ids,
// long digit runs (phone numbers, LIDs), and URLs.
var unspeakable = regexp.MustCompile(`(?i)\b\d[\d:]{7,}@[a-z.]+|\bhttps?://\S+|\b\d{9,}\b`)

var danglingIdentifier = regexp.MustCompile(
	`(?i)\s*[—–-]?\s*\b(?:his|her|their|the|whose)?\s*(?:number|id|jid|lid|phone|link|url)\s+(?:is|:)\s*([.,;!?]|$)`)

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
	// KARMAX_VOICE=off is the operator's kill switch: no answering, no placing,
	// no brain endpoint — while the Sarvam key stays in place for when it comes
	// back. A switch, because "stop all calls" should not mean digging a
	// credential out of a file later.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KARMAX_VOICE")), "off") {
		return ""
	}
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
		rt.log.Info("voice is off (KARMAX_VOICE=off, no SARVAM_API_KEY, or webhooks disabled) — calls will not be answered or placed")
		return
	}
	if a == nil {
		rt.log.Warn("voice is off: no agent to speak as")
		return
	}

	factory := newVoiceFactory(rt, a, pickVoiceModel(a))
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
		// In the background: wacli may still be starting, and nothing about
		// answering should delay the rest of boot.
		go rt.ensureAnswering(wa)
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

// linesFor is the lookup's engine without the tool wrapping: memory lines the
// text's own words reach, deduplicated, newest first.
func (t *voiceMemoryLookup) linesFor(text string, limit int) []string {
	if t == nil || (t.store == nil && t.mem == nil) {
		return nil
	}
	seen := map[string]bool{}
	var lines []string
	for _, word := range strings.Fields(strings.ToLower(text)) {
		word = strings.Trim(word, ".,'\"?!()")
		// Short connectives match everything and retrieve nothing.
		if len(word) < 4 || voiceStopWords[word] {
			continue
		}
		for _, line := range t.search(word, 4) {
			if seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, line)
			if len(lines) >= limit {
				return lines
			}
		}
	}
	return lines
}

var voiceStopWords = map[string]bool{
	"what": true, "whats": true, "with": true, "that": true, "this": true,
	"have": true, "about": true, "tell": true, "know": true, "remember": true,
	"latest": true, "there": true, "your": true, "just": true, "like": true,
	"they": true, "them": true, "then": true, "when": true, "will": true,
}

// ensureAnswering makes "calls get picked up" true continuously, not just on
// the happy path.
//
// Two ways it silently stopped being true. The wacli webhook is managed BY THE
// AGENT — the system prompt even shows it re-registering with message events
// only — so one re-registration dropped call.incoming and every call after it
// rang out with nothing logged anywhere. And a call that rings while KARMAX is
// restarting is announced to a webhook nobody is serving; by the time the
// daemon is back the announcement is gone, though the call itself often still
// rings. So on startup: repair the subscription, then answer anything already
// ringing.
func (rt *KarmaxRuntime) ensureAnswering(wa *wacliVoice) {
	api := strings.TrimRight(wa.apiURL, "/")
	client := &http.Client{Timeout: 10 * time.Second}

	// wacli may still be coming up alongside us.
	var hooks struct {
		Webhooks []map[string]any `json:"webhooks"`
	}
	for attempt := 0; attempt < 6; attempt++ {
		resp, err := client.Get(api + "/webhooks")
		if err == nil {
			err = json.NewDecoder(resp.Body).Decode(&hooks)
			resp.Body.Close()
			if err == nil {
				break
			}
		}
		if attempt == 5 {
			rt.log.Warn("could not reach wacli to verify call answering", zap.Error(err))
			return
		}
		time.Sleep(2 * time.Second)
	}

	for _, h := range hooks.Webhooks {
		url, _ := h["url"].(string)
		if !strings.Contains(url, "/comms/whatsapp") {
			continue
		}
		if missing := missingCallEvents(h["events"]); len(missing) > 0 {
			rt.repairWebhook(client, api, h, missing)
		}
	}

	// Anything mid-ring right now.
	var calls struct {
		Calls []struct {
			CallID    string `json:"call_id"`
			Direction string `json:"direction"`
			State     string `json:"state"`
		} `json:"calls"`
	}
	if resp, err := client.Get(api + "/calls?active=true"); err == nil {
		_ = json.NewDecoder(resp.Body).Decode(&calls)
		resp.Body.Close()
	}
	for _, c := range calls.Calls {
		if c.Direction != "incoming" || c.State != "ringing" {
			continue
		}
		rt.log.Info("a call was already ringing at startup; answering it", zap.String("call_id", c.CallID))
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := wa.Answer(ctx, c.CallID); err != nil {
			rt.log.Warn("could not answer the in-progress ring", zap.Error(err))
		}
		cancel()
	}
}

// missingCallEvents names the call events a webhook subscription lacks.
func missingCallEvents(events any) []string {
	have := map[string]bool{}
	if list, ok := events.([]any); ok {
		for _, e := range list {
			if s, ok := e.(string); ok {
				have[s] = true
			}
		}
	}
	var missing []string
	for _, want := range []string{"call.incoming", "call.ended"} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

// repairWebhook re-creates the subscription with call events restored,
// preserving everything else it carried. Create first, delete second, so a
// failure leaves the working subscription in place.
func (rt *KarmaxRuntime) repairWebhook(client *http.Client, api string, h map[string]any, missing []string) {
	body := map[string]any{}
	for _, k := range []string{"url", "scope", "chat_jids", "secret", "include_mentions",
		"message_types", "context_limit", "max_attempts", "timeout_seconds"} {
		if v, ok := h[k]; ok && v != nil {
			body[k] = v
		}
	}
	events := []any{}
	if list, ok := h["events"].([]any); ok {
		events = list
	}
	for _, m := range missing {
		events = append(events, m)
	}
	body["events"] = events

	payload, err := json.Marshal(body)
	if err != nil {
		return
	}
	resp, err := client.Post(api+"/webhooks", "application/json", bytes.NewReader(payload))
	if err != nil {
		rt.log.Warn("could not repair the call-event subscription", zap.Error(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		rt.log.Warn("wacli refused the repaired subscription", zap.String("body", string(raw)))
		return
	}
	if id, ok := h["id"].(float64); ok {
		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/webhooks/%d", api, int(id)), nil)
		if _, err := client.Do(req); err != nil {
			rt.log.Warn("replaced the subscription but could not remove the old one", zap.Error(err))
		}
	}
	rt.log.Info("restored call events on the wacli webhook", zap.Strings("restored", missing))
}

// search returns memory lines for a query from wherever memory actually lives.
func (t *voiceMemoryLookup) search(query string, limit int) []string {
	var lines []string
	if t.mem != nil {
		results, err := t.mem.Search(query, limit)
		if err == nil {
			for _, r := range results {
				if line := memoryLine(r.Excerpt); line != "" {
					lines = append(lines, line)
				}
			}
			return lines
		}
	}
	if t.store == nil {
		return nil
	}
	entries, err := t.store.SearchMemoryEntries(t.namespace, query, limit)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if line := memoryLine(e.Content); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// memoryLine collapses one memory to a single readable line.
func memoryLine(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	if len(line) > 180 {
		line = line[:180] + "…"
	}
	return line
}
