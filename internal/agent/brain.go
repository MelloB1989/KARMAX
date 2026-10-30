package agent

import (
	"context"
	"time"

	"github.com/MelloB1989/karma/models"
	"github.com/MelloB1989/karmax/internal/tools"
	"github.com/MelloB1989/karmax/pkg/karmahelper"
)

// Brain is the thinking half of an agent, so it can be swapped for one that
// runs on a coding harness instead of a metered API.
//
// The set is deliberately what MainModelSession already exposes, so adopting it
// is an assertion rather than a rewrite. Most of it is history and token
// plumbing; only ProcessMessageWithheld actually thinks.
type Brain interface {
	// ProcessMessageWithheld is one turn. lent adds tools for this turn only;
	// withhold takes them away, which is how an observe pass is denied a voice.
	ProcessMessageWithheld(ctx context.Context, userMessage string,
		lent []tools.Tool, withhold map[string]bool) (string, []karmahelper.ToolCallRecord, error)

	// SetTurnContext installs the per-turn context block — the clock, the
	// profile, what the agent has just done — before the next turn.
	SetTurnContext(dynamicContext string)

	// The history and token surface. A brain that keeps its own transcript
	// reports NeedsCompaction false and leaves the rest to its own engine.
	NeedsCompaction() bool
	GetHistory() *models.AIChatHistory
	SetHistory(models.AIChatHistory)
	GetTotalTokens() int64
	GetKeepRecent() int
	ResetTokenCount()
}

// The API-backed brain has satisfied this all along; naming it is the whole
// change. If a method is added to MainModelSession that the harness cannot
// honour, this line is what fails, at compile time, rather than at 2am.
var _ Brain = (*MainModelSession)(nil)

// defaultTurnBudget bounds one turn of a brain that does not say how long its
// turns may take. A hung provider must never wedge a conversation's worker.
const defaultTurnBudget = 3 * time.Minute

// turnBudget is how long the agent gives b for one turn.
//
// A brain that knows its own limit (TurnBudget) is given it, and never less
// than the default. The agent's three minutes used to be imposed on every
// brain, including a harness session configured for longer and one that
// first has to wait out a turn another session started — so a turn the
// orchestrator finished inside its own configured time was cut off by the
// agent's, and the operator was told to send the message again.
func turnBudget(b Brain) time.Duration {
	if tb, ok := b.(interface{ TurnBudget() time.Duration }); ok {
		if d := tb.TurnBudget(); d > defaultTurnBudget {
			return d
		}
	}
	return defaultTurnBudget
}
