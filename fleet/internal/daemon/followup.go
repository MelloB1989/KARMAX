package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/ledger"
)

// ErrConflict is an operation the fleet's state refuses: no free agent, an
// agent busy, a task already closed. The server answers 409.
// (Track B declares this in run.go too; the lead keeps one.)
var ErrConflict = errors.New("conflict")

// openTask reads a task that must exist and still be open.
func (f *Fleet) openTask(id string) (*ledger.Task, error) {
	t, err := f.db.Task(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w %s", ErrNoTask, id)
	}
	if err != nil {
		return nil, err
	}
	if !t.Closed.IsZero() {
		return nil, fmt.Errorf("%w: task %s is closed", ErrConflict, id)
	}
	return t, nil
}

// Followup relays more instructions, or an answer to its question, to the
// agent working a task. The task must be open.
func (f *Fleet) Followup(ctx context.Context, task, from, text string, now time.Time) error {
	t, err := f.openTask(task)
	if err != nil {
		return err
	}
	return f.Tell(ctx, from, t.Agent, fmt.Sprintf("Task %s follow-up from %s. %s", task, from, text), now)
}

// Cancel stops an agent's work on a task: it presses Escape in the agent's
// session, tells it the task is cancelled, and closes the task as
// "cancelled". The session is kept. A failed interrupt or notice is returned,
// but the task is closed regardless: the caller no longer wants it.
func (f *Fleet) Cancel(ctx context.Context, task, from, reason string, now time.Time) error {
	t, err := f.openTask(task)
	if err != nil {
		return err
	}
	notice := fmt.Sprintf("Task %s was cancelled by %s: stop work on it now, leave the branch as it is, and report nothing further.", task, from)
	if reason = strings.TrimSpace(reason); reason != "" {
		notice += " " + reason
	}
	var errs []error
	if err := f.box(t.Agent).Interrupt(ctx); err != nil {
		errs = append(errs, fmt.Errorf("interrupt %s: %w", t.Agent, err))
	}
	if err := f.Tell(ctx, from, t.Agent, notice, now); err != nil {
		errs = append(errs, fmt.Errorf("notify %s: %w", t.Agent, err))
	}
	if err := f.Close(t.Agent, task, "cancelled", "", "", now); err != nil {
		errs = append(errs, err)
	}
	f.event(t.Agent, "fleet.cancel", "", map[string]string{"task": task, "by": from})
	return errors.Join(errs...)
}

// taskOwner is the agent a task was given to, for done/close by task alone.
func (f *Fleet) taskOwner(task string) (string, error) {
	if task == "" {
		return "", fmt.Errorf("name an agent or a --task")
	}
	t, err := f.db.Task(task)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w %s", ErrNoTask, task)
	}
	if err != nil {
		return "", err
	}
	return t.Agent, nil
}
