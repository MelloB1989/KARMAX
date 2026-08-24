package loopkit

import "errors"

// ErrSuspended ends a run that is waiting on something that has not happened.
//
// It is not a failure and must not be treated as one: no retry, no dark-loop
// alert, and — critically — the run's step checkpoints are KEPT, because the
// whole point is that the work already done is not done again when the event
// arrives. A run that awaits a ticket transition may end here on Monday and
// resume on Thursday inside a different process.
var ErrSuspended = errors.New("run suspended, waiting on an event")

// Suspended reports whether err ended a run by parking it.
func Suspended(err error) bool { return errors.Is(err, ErrSuspended) }
