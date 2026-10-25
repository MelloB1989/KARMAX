package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/memory"
)

// Recaller searches the operator's long-term memory (*memory.Client).
type Recaller interface {
	Recall(ctx context.Context, query string, limit int) (*memory.Result, error)
}

// ErrMemoryOff is a memory call on a fleet whose fleet.yaml has no memory
// section (or no key for it).
var ErrMemoryOff = errors.New("memory is off: fleet.yaml has no memory section, or its key is not in the env file")

// Recall searches the operator's memory. Who may is the API's to decide.
func (f *Fleet) Recall(ctx context.Context, query string, limit int) (*memory.Result, error) {
	if f.Memory == nil {
		return nil, ErrMemoryOff
	}
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: recall needs a query", ErrBadInput)
	}
	return f.Memory.Recall(ctx, query, limit)
}

// Suggest sends a fact worth keeping to the orchestrator, which alone saves
// to the memory, with its own tools, if it agrees. It returns who it went
// to. An agent's suggestion is filed under its task, like its reports.
func (f *Fleet) Suggest(ctx context.Context, from, text, why string, now time.Time) (string, error) {
	if f.Memory == nil {
		return "", ErrMemoryOff
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%w: a suggestion needs text", ErrBadInput)
	}
	head := "Memory suggestion from " + from
	if _, ok := f.cfg.Worker(from); ok {
		if task := f.reportTask(from, now); task != "" {
			head += " (task " + task + ")"
		}
	}
	msg := head + ":\n" + strings.TrimSpace(text)
	if why = strings.TrimSpace(why); why != "" {
		msg += "\nWhy: " + why
	}
	msg += "\nSave it with your memory tools if it should be kept, or let it go. fleetd does not write memory."
	to := f.cfg.Orchestrator.Name
	return to, f.send(ctx, from, to, msg, "", now)
}

// OperatorAssigner reports whether a task's assigner is the operator's side
// for placing work: the operator, one of their sessions or the orchestrator
// (a name without an @, which only the full scope can set). A guest's task
// is never: their runs carry their login. Whether a task may use the
// operator's data is its own AdminWork, not this.
func (f *Fleet) OperatorAssigner(assigner string) bool {
	return assigner != "" && !strings.Contains(assigner, "@")
}
