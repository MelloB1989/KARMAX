package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
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
	"github.com/MelloB1989/karmax/pkg/loopkit"
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
const voicePrompt = `You are KARMAX on a live phone call. You are the voice: the communication layer between the caller and KARMAX, the assistant that holds the context and does the work.

Speak the way a person talks on the phone. Everything you write is read aloud by a speech synthesiser, so write only words meant to be heard.
Keep it short: one or two plain sentences, the answer first, no preamble. Go longer only when the caller asks for detail, and even then in short spoken sentences.
Use everyday conversational language, contractions, and the rhythm of speech. No markdown, no lists, no headings, no emoji, no symbols, no abbreviations, no URLs read aloud.
Say things the way people say them aloud: "twenty dollars" and not a dollar sign, "eighty billion parameters" and not 80B, "about two and a half percent", "three thirty in the afternoon". Say model and product names as words a person would pronounce, and drop version codes that add nothing. Use commas and full stops, not dashes or brackets.
If you did not catch something, say so briefly and ask them to repeat.

Context comes from two places: a short summary of what is remembered is in your context, and
memory.lookup searches the rest — call it ONCE with a couple of keywords when the summary does not
cover the question; it is fast. For a quick question that needs KARMAX's live knowledge or tools,
ask orchestrator.send and relay the answer.
Anything that needs doing — work, research, code, messages, anything that takes more than a moment —
becomes a task: call task.create RIGHT AWAY with a full goal (every detail the caller gave), tell
them it is underway, and never claim it is done. The caller is messaged as it progresses. If they
ask how things are going, call task.list and answer from it.
Never ask the caller for a number or an id and never read one aloud.
When they tell you something worth keeping — a decision, a plan, a preference — save it with
memory.ingest; after a save your reply is ONLY the short confirmation.
When the conversation is over — they say bye, that's all, hang up, or go quiet after thanking
you — say a brief goodbye and call the call.hangup tool in the same turn.
Tools are invoked, never spoken: never write a tool name, tool syntax or JSON in your reply, because every word you write is said aloud. To end the call, invoke the call.hangup tool.
If a turn is marked as interrupting your last reply, they did not hear all of it: do not repeat
it wholesale, just continue naturally from what they said.
Never invent facts. If memory has nothing, say so plainly.`

// voiceTagPrompt is added only when the call's voice performs audio tags; the
// others would read the brackets aloud.
const voiceTagPrompt = `

The voice can perform audio tags written in square brackets right before the words they colour, such as [laughs], [excited], [sighs], [whispers] or [curious]. Use one only where it genuinely fits the moment, at most one or two in a reply, never as the whole reply, and never in a goodbye or a confirmation that needs to be clear.`

func callPrompt(tags bool) string {
	if tags {
		return voicePrompt + voiceTagPrompt
	}
	return voicePrompt
}

// voiceSession is the part of a karma session a call uses.
type voiceSession interface {
	Chat(ctx context.Context, msg string) (string, []karmahelper.ToolCallRecord, karmahelper.TokenInfo, error)
	SetHistory(models.AIChatHistory)
	GetHistory() models.AIChatHistory
	SetContext(string)
	PrimeTurn(string) func(context.Context) error
	// SetTurnStream receives the model's text as it streams; nil stops it.
	SetTurnStream(func(string))
}

// voiceBrain answers a call with a dedicated fast session.
//
// Not the orchestrator's own session: that carries the whole persona, a dozen
// tool schemas and a growing history, and took nine to ten seconds to answer
// "hello" — measured. A call gets the fast model, this five-line prompt, no
// tools, and history that lasts exactly as long as the call.
type voiceBrain struct {
	session voiceSession
	// lookup and brief power pre-answer retrieval: memory relevant to the
	// utterance is fetched BEFORE the model runs — a millisecond store search —
	// so most questions answer in one pass instead of model → tool → model.
	// The tool stays for what keyword overlap misses.
	lookup *voiceMemoryLookup
	brief  string
	// late holds hits from a search that outran its turn's budget.
	late chan []string
	// tags: the call's voice performs inline audio tags.
	tags bool

	// callBrief is why an outbound call was placed; it replaces the greeting.
	callBrief string
	ledger    *voiceLedger
	spendMu   sync.Mutex
	spent     float64

	// notices is what the brain says unprompted — a handed-off task coming
	// back mid-call. done closes when the call ends, after which results go
	// to the operator's chat instead of a line nobody is on.
	notices chan voice.Reply
	done    chan struct{}
	endOnce sync.Once
	// hangup is set by the call.hangup tool and consumed by the next reply.
	hangup atomic.Bool
	// endVerdict asks Jev how likely the call is over after this turn; nil when unavailable.
	endVerdict func(ctx context.Context, state any) (float64, error)
	endAbove   float64
	direction  string
	group      bool
	toolNames  []string
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
	b.endOnce.Do(func() {
		close(b.done)
		b.spendMu.Lock()
		spent := b.spent
		b.spendMu.Unlock()
		if b.ledger != nil {
			b.log.Info("voice: call spend", zap.Float64("usd", spent))
		}
	})
}

// addSpend is the session's usage hook.
func (b *voiceBrain) addSpend(u karmahelper.Usage) {
	usd := b.ledger.record(u)
	b.spendMu.Lock()
	b.spent += usd
	b.spendMu.Unlock()
}

// openFromBrief has the model open an outbound call from its brief.
// The bool says the opening was already spoken as it streamed.
func (b *voiceBrain) openFromBrief(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ts := b.startStream(ctx)
	defer b.session.SetTurnStream(nil)
	text, _, _, err := b.session.Chat(ctx, "You placed this phone call and the person has just picked up. "+
		"Nothing has been said yet. Reason for the call: "+b.callBrief+"\n\nSay your opening line now: "+
		"one or two short spoken sentences that get to the point of the call.")
	ts.Flush()
	if err != nil && ts.Spoken() == 0 {
		b.hangup.Store(false)
		b.log.Warn("voice: could not open the call from its brief", zap.Error(err))
		return "", false
	}
	// A one-way call ends once the opening is spoken.
	if b.hangup.Swap(false) {
		select {
		case b.notices <- voice.Reply{Hangup: true}:
		default:
		}
	}
	if ts.Spoken() > 0 {
		return "", true
	}
	return speakableFor(dropToolText(text, b.toolNames), b.tags), false
}

// startStream routes the model's text to the call as it is generated, or returns nil when the integration cannot stream.
func (b *voiceBrain) startStream(ctx context.Context) *turnSpeech {
	ts := newTurnSpeech(voice.SayFrom(ctx), b.tags)
	if ts != nil {
		ts.names = b.toolNames
		b.session.SetTurnStream(ts.Write)
	}
	return ts
}

func (b *voiceBrain) Greeting(ctx context.Context, peer string) string {
	if b.ledger.blocked() {
		b.notices <- voice.Reply{Text: voiceBudgetLine, Hangup: true}
		return ""
	}
	if b.callBrief != "" {
		if opening, streamed := b.openFromBrief(ctx); streamed || opening != "" {
			return opening
		}
	}
	const greeting = "Hey, it's KARMAX. What do you need?"
	// Seeded into the session as the opening assistant turn. Without it the
	// model's first exposure is a bare instruction on empty history — and on
	// exactly those cold first turns it was observed ignoring a save request
	// entirely while handling the same request fine mid-conversation.
	b.session.SetHistory(models.AIChatHistory{Messages: []models.AIMessage{{
		Role: models.Assistant, Message: greeting,
	}}})
	// On Claude Code the call's session is a process that has not started
	// yet, and starting it would otherwise land on the caller's first
	// question. Warm it while the greeting plays: the brief, the
	// instructions, and what has already been said.
	if b.brief != "" {
		b.session.SetContext(b.brief)
	}
	// Open the memory connection while the greeting plays. Calls are rare, so
	// the connection to GitLoom has almost always gone idle, and the first
	// lookup of a call paid a fresh TLS handshake on top of a one-second
	// search — past the pre-answer budget, so the caller's first question was
	// answered without memory. The result is thrown away; the warm connection
	// is the point.
	if b.lookup != nil {
		go b.lookup.search(peer, 1)
	}
	if prime := b.session.PrimeTurn("A phone call has just connected, and you have already said to the caller: \"" +
		greeting + "\". Nothing has been said back yet. Reply to this message with only: ok"); prime != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := prime(ctx); err != nil {
				b.log.Warn("voice: could not warm the call's session", zap.Error(err))
			}
		}()
	}
	return greeting
}

func (b *voiceBrain) Answer(ctx context.Context, u voice.Utterance) (voice.Reply, error) {
	if b.ledger.blocked() {
		return voice.Reply{Text: voiceBudgetLine, Hangup: true}, nil
	}
	// Retrieval before generation, but only what arrives within a short budget:
	// the model is never held for memory. A search that misses the budget keeps
	// running and its hits join the next turn's context. The memory.lookup tool
	// covers anything deeper.
	endCh := b.askEnd(ctx, u)
	lookupStart := time.Now()
	hits := b.collectHits(u.Text)
	lookupTook := time.Since(lookupStart)
	if len(hits) > 0 {
		b.session.SetContext(b.brief + "\n## Memory that may be relevant\n- " +
			strings.Join(hits, "\n- ") + "\n")
	} else {
		b.session.SetContext(b.brief)
	}
	if u.Interrupted {
		b.markLastReplyUnheard()
	}
	ts := b.startStream(ctx)
	defer b.session.SetTurnStream(nil)
	modelStart := time.Now()
	text, calls, _, err := b.session.Chat(ctx, u.Text)
	ts.Flush()
	// Where a reply's time went, per turn: on a phone the total is the
	// product, and a slow reply is only fixable once it says which half was slow.
	b.log.Info("voice: turn timing",
		zap.Duration("lookup", lookupTook.Round(time.Millisecond)),
		zap.Duration("model", time.Since(modelStart).Round(time.Millisecond)),
		zap.Int("memory_hits", len(hits)), zap.Int("tool_calls", len(calls)),
		zap.Int("sentences_streamed", ts.Spoken()))
	if err != nil {
		if ts.Spoken() > 0 {
			b.log.Warn("voice: the turn failed after part of the reply was spoken", zap.Error(err))
			return voice.Reply{Hangup: b.endAfter(endCh)}, nil
		}
		return voice.Reply{}, err
	}
	if len(calls) > 0 {
		names := make([]string, 0, len(calls))
		for _, c := range calls {
			names = append(names, c.Name)
		}
		b.log.Info("voice: the brain used tools", zap.Strings("tools", names))
	}
	// Already spoken sentence by sentence; only an unstreamed reply goes out here.
	spoken := ""
	if ts.Spoken() == 0 {
		spoken = speakableFor(dropToolText(text, b.toolNames), b.tags)
	}
	return voice.Reply{Text: spoken, Hangup: b.endAfter(endCh)}, nil
}

// askEnd starts Jev's end-of-call verdict beside the memory lookup; nil when Jev is not wired.
func (b *voiceBrain) askEnd(ctx context.Context, u voice.Utterance) <-chan float64 {
	if b.endVerdict == nil {
		return nil
	}
	state := map[string]any{
		"caller_said":       u.Text,
		"recent_transcript": b.recentTranscript(6),
		"group_call":        b.group,
		"direction":         b.direction,
	}
	ch := make(chan float64, 1)
	go func() {
		defer close(ch)
		p, err := b.endVerdict(ctx, state)
		if err != nil {
			b.log.Warn("voice: end-call verdict unavailable", zap.Error(err))
			return
		}
		b.log.Info("voice: end-call verdict", zap.Float64("probability", p), zap.Float64("threshold", b.endAbove))
		ch <- p
	}()
	return ch
}

// endAfter is whether this reply ends the call: the model's tool, or Jev above the threshold.
func (b *voiceBrain) endAfter(ch <-chan float64) bool {
	hang := b.hangup.Swap(false)
	if ch == nil {
		return hang
	}
	select {
	case p, ok := <-ch:
		return hang || (ok && p >= b.endAbove)
	case <-time.After(endVerdictWait):
		return hang
	}
}

// endVerdictWait bounds the wait for a verdict that outlived the model's reply.
const endVerdictWait = 2 * time.Second

// recentTranscript is the last n turns, both sides, oldest first.
func (b *voiceBrain) recentTranscript(n int) []string {
	msgs := b.session.GetHistory().Messages
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		who := "caller"
		if m.Role == models.Assistant {
			who = "assistant"
		}
		if t := strings.TrimSpace(m.Message); t != "" {
			out = append(out, who+": "+t)
		}
	}
	return out
}

// endCallQuestion is the Jev question asked on every caller turn.
const endCallQuestion = "This is a live phone call. Should the call end right after the assistant's reply to what the caller just said? " +
	"Answer yes only if the caller said goodbye, asked to hang up or end the call, or is clearly finished and has nothing more to ask. " +
	"Answer no if the conversation is ongoing, the caller asked a question or made a request, or they only paused or acknowledged something."

func endCallQuestions() loopkit.Questions {
	return loopkit.Questions{"end": loopkit.Noul(endCallQuestion).When(
		"the caller said bye, asked to hang up, or is clearly done",
		"the conversation is mid-flow, or the caller is asking or requesting something")}
}

// endAboveThreshold is KARMAX_VOICE_END_THRESHOLD, default 0.7.
func endAboveThreshold() float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv("KARMAX_VOICE_END_THRESHOLD")), 64); err == nil && v > 0 && v <= 1 {
		return v
	}
	return 0.7
}

// collectHits waits up to the pre-answer budget for memory matching the
// utterance, and adds any hits a previous turn's search found too late.
func (b *voiceBrain) collectHits(text string) []string {
	var late []string
	select {
	case late = <-b.late:
	default:
	}
	if b.lookup == nil {
		return late
	}
	res := make(chan []string, 1)
	go func() { res <- b.lookup.linesFor(text, 5) }()
	select {
	case hits := <-res:
		return append(late, hits...)
	case <-time.After(preAnswerBudget()):
		// Left to finish; the next turn picks it up.
		go func() {
			if hits := <-res; len(hits) > 0 {
				select {
				case b.late <- hits:
				default:
				}
			}
		}()
		return late
	}
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
		text = "The task I handed off did not go through: " + speakableFor(err.Error(), b.tags)
	default:
		text = speakableFor(out, b.tags)
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
	// bedrockKeyEnv names the env var holding the calls-only bearer key.
	bedrockKeyEnv string
	bedrockRegion string
	budgetUSD     float64
}

const (
	defaultVoiceKeyEnv = "KARMAX_VOICE_BEDROCK_API_KEY"
	defaultVoiceRegion = "us-east-1"
)

// withVoiceBedrock fills the key env name, region and cap from the agent definition.
func withVoiceBedrock(m voiceModel, def agent.AgentDef) voiceModel {
	m.bedrockKeyEnv = def.VoiceBedrockKeyEnv
	if m.bedrockKeyEnv == "" {
		m.bedrockKeyEnv = defaultVoiceKeyEnv
	}
	m.bedrockRegion = def.VoiceBedrockRegion
	if m.bedrockRegion == "" {
		m.bedrockRegion = defaultVoiceRegion
	}
	m.budgetUSD = def.VoiceBudgetUSD
	return m
}

func pickVoiceModel(a *agent.Agent) voiceModel {
	def := a.Snapshot().Def
	ns := def.Memory.Namespace
	if ns == "" {
		ns = def.ID
	}
	// A dedicated call model wins. It exists because a call's budget is a
	// second or two per reply, and the engine that suits the rest of KARMAX
	// may not meet it: on Claude Code a reply measured 8 to 11 seconds, on
	// Haiku over Bedrock 1.1 plain and 2.1 with a tool. If it fails, the call
	// falls back to the memory model rather than going silent.
	if v := def.VoiceModelCfg; v.Model != "" {
		var fallbacks []karmahelper.FallbackModel
		// Configured fallbacks first — another model on the same fast
		// provider rides out one model's capacity trouble at the same speed —
		// then the memory model, slower but on a different engine entirely.
		for _, f := range def.VoiceFallbackModels {
			fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: f.Provider, Model: f.Model})
		}
		if m := def.MemoryModelCfg; m.Model != "" && (m.Provider != v.Provider || m.Model != v.Model) {
			fallbacks = append(fallbacks, karmahelper.FallbackModel{Provider: m.Provider, Model: m.Model})
		}
		return withVoiceBedrock(voiceModel{provider: v.Provider, model: v.Model, namespace: ns, fallbacks: fallbacks}, def)
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
		return withVoiceBedrock(voiceModel{provider: def.MemoryModelCfg.Provider, model: def.MemoryModelCfg.Model, namespace: ns, fallbacks: fallbacks}, def)
	}
	return withVoiceBedrock(voiceModel{provider: def.Provider, model: def.Model, namespace: ns, fallbacks: fallbacks}, def)
}

func newVoiceFactory(rt *KarmaxRuntime, a *agent.Agent, m voiceModel) voice.Factory {
	return func(call voice.Call) voice.Brain {
		operator := voiceCallerIsOperator(call.Peer, a.IsOperatorChat)
		key := os.Getenv(m.bedrockKeyEnv)
		if key == "" && m.provider == "bedrock" {
			rt.log.Warn("voice: the call Bedrock key env var is empty; using default AWS credentials", zap.String("env", m.bedrockKeyEnv))
		}
		chain := []string{m.model}
		for _, f := range m.fallbacks {
			if f.Provider == "bedrock" {
				chain = append(chain, f.Model)
			}
		}
		// The brain gets the two memory verbs and nothing else. Lookup is a
		// purpose-built fast read — the agent's own memory.retrieve is a
		// sub-agent that traverses the index for seconds, which is a fine cost
		// in a chat and a dead line on a phone. Ingest is the agent's OWN bound
		// tool, so a fact said on a call lands in the same memory everything
		// else reads.
		brain := &voiceBrain{
			notices:   make(chan voice.Reply, 4),
			tags:      call.Tags,
			late:      make(chan []string, 1),
			done:      make(chan struct{}),
			log:       rt.log,
			deliver:   rt.messageOperator,
			callBrief: strings.TrimSpace(call.Brief),
			ledger:    newVoiceLedger(rt.store, key, m.budgetUSD, chain, rt.messageOperator, rt.log),
			// The orchestrator's own turn, with its own tools — the difference
			// between the brain that talks and the agent that does.
			delegate: func(ctx context.Context, request string) (string, error) {
				out, _, err := a.ChatDetailed(ctx, request, nil)
				return out, err
			},
		}
		mem := rt.memory.For(a.Snapshot().Def.ID, m.namespace)
		lookup := &voiceMemoryLookup{store: rt.store, mem: mem, namespace: m.namespace}
		chID, target := rt.operatorDM()
		ref := &harnessRef{rt: rt}
		// Every caller gets every tool, by the operator's choice.
		voiceTools := append([]tools.Tool{
			&voiceHangupTool{brain: brain},
			lookup,
			&voiceDelegateTool{brain: brain},
			&voiceTaskCreateTool{inner: &taskCreateTool{ref: ref, agentID: agentIDOf(rt.cfg)},
				channelID: chID, target: target, notify: rt.messageOperator},
			&taskListTool{ref: ref},
		}, a.NamedTools("memory.ingest")...)
		session := karmahelper.NewSession(karmahelper.SessionConfig{
			Kind:         "voice",
			Provider:     m.provider,
			Model:        m.model,
			SystemPrompt: callPrompt(call.Tags),
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
			BedrockAPIKey:  key,
			BedrockRegion:  m.bedrockRegion,
			OnUsage:        brain.addSpend,
		}, voiceTools)
		// Synchronous on purpose: a few milliseconds of SQLite before the
		// greeting buys most questions a zero-lookup answer, and a context set
		// concurrently with the first turn would race the session.
		brief := memoryBrief(rt.store, mem, m.namespace)
		if !operator {
			brief = "Caller: " + call.Peer + " (not the operator)\n" + brief
		}
		// A phone assistant that has to ask what day it is has already lost
		// the caller. Cheap, and it goes in the per-call brief rather than the
		// cached prefix, since it changes.
		brief = "Now: " + time.Now().Format("Monday, 2 January 2006, 3:04 PM MST") + "\n" + brief
		session.SetContext(brief)
		brain.session = session
		brain.direction = call.Direction
		brain.group = strings.HasSuffix(call.Peer, "@g.us")
		brain.endAbove = endAboveThreshold()
		brain.endVerdict = func(ctx context.Context, state any) (float64, error) {
			d, err := rt.decide(ctx, state, endCallQuestions())
			if err != nil {
				return 0, err
			}
			return d.Noul("end"), nil
		}
		for _, t := range voiceTools {
			brain.toolNames = append(brain.toolNames, t.Manifest().Name)
		}
		brain.lookup = lookup
		brain.brief = brief
		return brain
	}
}

// operatorDM is the WhatsApp channel and the operator's own chat, where task updates go.
func (rt *KarmaxRuntime) operatorDM() (channelID, target string) {
	if rt.comms == nil || rt.cfg == nil {
		return "", ""
	}
	channelID, _ = rt.comms.FindChannelIDByType("whatsapp")
	for _, ch := range rt.cfg.Comms.Channels {
		if strings.EqualFold(ch.Type, "whatsapp") && ch.Settings["target_chat"] != "" {
			return channelID, ch.Settings["target_chat"]
		}
	}
	return "", ""
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
	// The same lookup that runs before every reply: the whole query first,
	// word by word only if that finds nothing. The substring store this once
	// searched word by word is gone — memory is GitLoom, which ranks whole
	// queries, and a per-word loop paid a network round trip for each word.
	lines := t.linesWithin(query, 8, toolLookupBudget)
	if len(lines) == 0 {
		return tools.SuccessResult(map[string]any{
			"found": 0, "note": "nothing in memory matches — say so rather than guessing",
		}), nil
	}
	return tools.SuccessResult(map[string]any{"found": len(lines), "memories": lines}), nil
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
	// KARMAX_VOICE=off is the operator's kill switch: no answering, no placing,
	// no brain endpoint — while the Sarvam key stays in place for when it comes
	// back. A switch, because "stop all calls" should not mean digging a
	// credential out of a file later.
	if !voiceEnabled() || !cfg.Webhooks.Enabled {
		return ""
	}
	return fmt.Sprintf("ws://127.0.0.1:%d/voice", cfg.Webhooks.Port)
}

// voiceEnabled reads the operator's switch.
//
// It used to be inferred from SARVAM_API_KEY being set here — but speech is the
// integration's affair, the keys live in wacli's environment and not this one,
// and once calls could run on Modulate and ElevenLabs a Sarvam key said nothing
// about whether a call could be held. So it is an explicit switch: on, or off.
// The Sarvam key still counts as "on" so an install that relied on it keeps
// working.
func voiceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KARMAX_VOICE"))) {
	case "off":
		return false
	case "on":
		return true
	}
	return strings.TrimSpace(os.Getenv("SARVAM_API_KEY")) != ""
}

// mountVoice serves the conversation endpoint and registers the integrations.
func (rt *KarmaxRuntime) mountVoice(wh interface {
	AddHandler(string, http.HandlerFunc)
}, a *agent.Agent) {
	url := brainURL(rt.cfg)
	if url == "" {
		rt.log.Info("voice is off (KARMAX_VOICE is not on, or webhooks are disabled) — calls will not be answered or placed")
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
	add := func(found []string) bool {
		for _, line := range found {
			if seen[line] {
				continue
			}
			seen[line] = true
			lines = append(lines, line)
			if len(lines) >= limit {
				return true
			}
		}
		return false
	}

	// The whole sentence first, as one search. Memory is GitLoom, where every
	// search is a network round trip plus a fetch per hit, and ranking a
	// sentence is what its retrieval is built for. Searching word by word paid
	// that three or four times over — three seconds a turn, measured, before
	// the model had even started.
	if add(t.search(text, limit)) || len(lines) > 0 {
		return lines
	}

	// Word by word only when the sentence found nothing, and only within a
	// budget: a lookup that finds nothing must not also cost the caller the
	// pause it was meant to save.
	deadline := time.Now().Add(lookupWordBudget)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		if time.Now().After(deadline) {
			break
		}
		word = strings.Trim(word, ".,'\"?!()")
		// Short connectives match everything and retrieve nothing.
		if len(word) < 4 || voiceStopWords[word] {
			continue
		}
		if add(t.search(word, 4)) {
			return lines
		}
	}
	return lines
}

// preAnswerLookupBudget bounds the lookup that runs before every reply.
//
// It is a head start, not a requirement: the model can still look memory up
// itself when it needs to. GitLoom answers a specific question in about a
// second, measured, and a broad name like "Kartik" in closer to three — so
// this catches the questions memory can actually answer and lets the rest go
// ahead without it rather than holding the caller.
const defaultPreAnswerBudget = 500 * time.Millisecond

// preAnswerBudget is KARMAX_VOICE_LOOKUP_MS, in milliseconds, or the default.
func preAnswerBudget() time.Duration {
	if ms, err := strconv.Atoi(strings.TrimSpace(os.Getenv("KARMAX_VOICE_LOOKUP_MS"))); err == nil && ms >= 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return defaultPreAnswerBudget
}

// linesWithin is linesFor that gives up after budget. The search is left to
// finish in the background; its result is simply not waited for.
func (t *voiceMemoryLookup) linesWithin(text string, limit int, budget time.Duration) []string {
	done := make(chan []string, 1)
	go func() { done <- t.linesFor(text, limit) }()
	select {
	case lines := <-done:
		return lines
	case <-time.After(budget):
		return nil
	}
}

// toolLookupBudget bounds a lookup the model asks for mid-call. Longer than
// the pre-answer one, since the model chose to wait for it, but still short:
// the caller is on the line, and "nothing in memory" said promptly is better
// than the same answer after a silence.
const toolLookupBudget = 3 * time.Second

// lookupWordBudget bounds the word-by-word fallback.
const lookupWordBudget = 1200 * time.Millisecond

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
				if line := memoryLine(hitText(r)); line != "" {
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

// hitText is what a search hit actually says.
//
// GitLoom returns ranked hits with empty snippets; the memory package fetches
// each body by path into Content and leaves Excerpt empty. Reading Excerpt
// alone threw every hit away — every call-time lookup came back empty while
// the same memory answered the question perfectly through the full retriever.
func hitText(r memory.SearchResult) string {
	if strings.TrimSpace(r.Excerpt) != "" {
		return r.Excerpt
	}
	return r.Entry.Content
}

// memoryLine collapses one memory to a single readable line.
func memoryLine(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	if len(line) > 180 {
		line = line[:180] + "…"
	}
	return line
}

// BrainURL is the voice brain's WebSocket URL for CLI callers, empty when voice is off.
func BrainURL(cfg *config.KarmaxConfig) string { return brainURL(cfg) }
