package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// HarnessSender is the slice of the supervisor this package needs, kept narrow
// so internal/agent does not depend on the harness package's internals.
type HarnessSender interface {
	Send(ctx context.Context, key, kind, text string) (HarnessTurn, error)
}

// HarnessTurn is one harness reply, reduced to what a brain cares about.
type HarnessTurn struct {
	Text      string
	ToolCalls []HarnessToolCall
	Available bool   // false when the breaker is open
	Reason    string // why, when it is not
	// TimedOut: the turn was written and its time ran out before it ended —
	// the work is longer than a turn, not broken.
	TimedOut bool
}

// HarnessToolCall is a tool the harness invoked during a turn.
type HarnessToolCall struct {
	Name  string
	Input json.RawMessage
	// Failed: the call's result came back as an error.
	Failed bool
}

// harnessBrain runs an agent's turns inside a long-lived harness session.
//
// The transcript belongs to the harness. KARMAX keeps a history alongside it
// only to build the per-turn context block, and never replays it back in: two
// memories of one conversation is exactly what produced a self-reinforcing
// refusal loop once already, where the model read its own earlier output as
// input and escalated it every turn.
type harnessBrain struct {
	send       HarnessSender
	sessionKey string
	kind       string

	// fallback is the metered path, used whenever the harness declines. It is
	// never removed: an engine that can be rate-limited needs one that cannot.
	fallback Brain

	mu        sync.Mutex
	turnCtx   string
	lastTurns int64
}

// NewHarnessBrain wires an agent to a harness session, with the API brain as
// its fallback.
func NewHarnessBrain(send HarnessSender, sessionKey, kind string, fallback Brain) Brain {
	return &harnessBrain{send: send, sessionKey: sessionKey, kind: kind, fallback: fallback}
}

// harnessBudget is a HarnessSender that knows how long a turn of a kind can
// take. Optional, so a sender that does not know leaves the old bound.
type harnessBudget interface {
	TurnBudget(kind string) time.Duration
}

// TurnBudget is the harness's own limit for this kind plus the fallback's
// budget, so a turn the harness declines late still leaves the fallback a
// whole turn of its own.
func (h *harnessBrain) TurnBudget() time.Duration {
	primary := defaultTurnBudget
	if b, ok := h.send.(harnessBudget); ok {
		if d := b.TurnBudget(h.kind); d > 0 {
			primary = d
		}
	}
	if h.fallback == nil {
		return primary
	}
	return primary + turnBudget(h.fallback)
}

func (h *harnessBrain) SetTurnContext(dynamicContext string) {
	h.mu.Lock()
	h.turnCtx = dynamicContext
	h.mu.Unlock()
	// The fallback needs it too: it may serve the very next turn.
	if h.fallback != nil {
		h.fallback.SetTurnContext(dynamicContext)
	}
}

func (h *harnessBrain) ProcessMessageWithheld(ctx context.Context, userMessage string,
	lent []tools.Tool, withhold map[string]bool) (string, []karmahelper.ToolCallRecord, error) {

	dyn, carried := turnContextFrom(ctx)
	if !carried {
		h.mu.Lock()
		dyn = h.turnCtx
		h.turnCtx = ""
		h.mu.Unlock()
	}

	prompt := composeHarnessPrompt(dyn, userMessage, lent, withhold)

	turn, err := h.send.Send(ctx, h.sessionKey, h.kind, prompt)
	if err != nil || !turn.Available {
		if err == nil {
			err = fmt.Errorf("harness unavailable: %s", turn.Reason)
		}
		if h.fallback == nil {
			return "", nil, err
		}
		// The harness took the turn and acted before it failed — commands
		// ran, messages may have gone. The fallback has none of that
		// conversation, so handing it the same message is how one "go ahead"
		// gets done twice. What it did goes back with the failure, so the
		// operator can be told it rather than asked to send it again — or,
		// for a request, so the work continues as a task (ErrUnfinished).
		if n := len(turn.ToolCalls); n > 0 {
			return "", harnessRecords(turn.ToolCalls), fmt.Errorf("harness turn failed after %d tool call(s), so it is not run again on the fallback: %w", n, &unfinishedError{err})
		}
		// A request whose turn simply ran out of time is longer than a turn.
		// A lighter, shorter model will not do in less time what this one
		// could not; the request continues as a task instead.
		if turn.TimedOut && RequestFrom(ctx) != nil {
			return "", nil, &unfinishedError{err}
		}
		// Nothing left to answer with: the fallback would only start a
		// process to fail in the same instant, and its "deadline exceeded"
		// would replace the reason the harness gave.
		if ctx.Err() != nil {
			return "", nil, fmt.Errorf("%w (no time left to fall back: %v)", err, ctx.Err())
		}
		// Falling back is the normal case, not an incident: the account's
		// windows are shared with the operator and run out routinely.
		return h.fallback.ProcessMessageWithheld(ctx, userMessage, lent, withhold)
	}

	// Tool calls are mapped back because half of KARMAX reads them: the
	// act-evidence guard, the recent-actions context, and the duplicate-send
	// counters all decide from this list. A brain that returned none would look
	// like an agent that promised things and did nothing.
	return turn.Text, harnessRecords(turn.ToolCalls), nil
}

// unfinishedError is a harness failure that is ErrUnfinished: it reads as
// the failure itself, and is both.
type unfinishedError struct{ err error }

func (e *unfinishedError) Error() string   { return e.err.Error() }
func (e *unfinishedError) Unwrap() []error { return []error{e.err, ErrUnfinished} }

// harnessRecords maps a harness turn's tool calls to the records the rest of
// the agent reads.
func harnessRecords(calls []HarnessToolCall) []karmahelper.ToolCallRecord {
	records := make([]karmahelper.ToolCallRecord, 0, len(calls))
	for _, tc := range calls {
		var input map[string]any
		if len(tc.Input) > 0 {
			_ = json.Unmarshal(tc.Input, &input)
		}
		rec := karmahelper.ToolCallRecord{
			Name:  tc.Name,
			Input: input,
		}
		if tc.Failed {
			rec.Result = tools.ToolResult{IsError: true}
			rec.Error = errors.New("tool call failed")
		}
		records = append(records, rec)
	}
	return records
}

// composeHarnessPrompt assembles one turn's text.
//
// The per-turn context goes FIRST and the operator's message last, matching
// where the API path puts it — the model should meet the same material in the
// same order whichever engine is running.
func composeHarnessPrompt(dynamicContext, userMessage string, lent []tools.Tool, withhold map[string]bool) string {
	var b strings.Builder
	if strings.TrimSpace(dynamicContext) != "" {
		b.WriteString(dynamicContext)
		if !strings.HasSuffix(dynamicContext, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(withhold) > 0 {
		// Stated, because the harness has its own toolset and cannot have a
		// tool taken away the way the API path can. The real gate is that the
		// withheld KARMAX tools refuse when called; this is the polite warning
		// that saves a wasted attempt.
		names := make([]string, 0, len(withhold))
		for n := range withhold {
			names = append(names, n)
		}
		b.WriteString("WITHHELD ON THIS PASS: " + strings.Join(names, ", ") +
			". Do not attempt them; they will refuse. Finish without them.\n\n")
	}
	if len(lent) > 0 {
		b.WriteString("Extra tools available this turn, via `karmax tool call <name> k=v`:\n")
		for _, t := range lent {
			m := t.Manifest()
			b.WriteString("  - " + m.Name + ": " + firstSentence(m.Description) + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(userMessage)
	return b.String()
}

func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return s[:i]
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// The harness owns its transcript, so compaction is not KARMAX's problem here.
// Reporting true would start a second summarisation of a conversation that is
// already being managed, and reporting a history that then gets replayed is how
// one conversation becomes two.
func (h *harnessBrain) NeedsCompaction() bool { return false }

func (h *harnessBrain) GetHistory() *models.AIChatHistory {
	if h.fallback != nil {
		return h.fallback.GetHistory()
	}
	return &models.AIChatHistory{}
}

func (h *harnessBrain) SetHistory(hist models.AIChatHistory) {
	if h.fallback != nil {
		h.fallback.SetHistory(hist)
	}
}

func (h *harnessBrain) GetTotalTokens() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastTurns
}

func (h *harnessBrain) GetKeepRecent() int {
	if h.fallback != nil {
		return h.fallback.GetKeepRecent()
	}
	return 10
}

func (h *harnessBrain) ResetTokenCount() {
	h.mu.Lock()
	h.lastTurns = 0
	h.mu.Unlock()
	if h.fallback != nil {
		h.fallback.ResetTokenCount()
	}
}

var _ Brain = (*harnessBrain)(nil)
