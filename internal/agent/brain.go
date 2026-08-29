package agent

import (
	"context"

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
