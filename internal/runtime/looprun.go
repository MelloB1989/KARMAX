package runtime

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/hostpaths"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Durable loop execution.
//
// Four properties, each of which was missing and each of which cost something
// real:
//
//   - A failed run is RETRIED with backoff and dead-lettered if it keeps
//     failing, instead of logged and forgotten. 141 failures over three days
//     were dropped this way.
//   - Only one run of a loop is in flight at a time, enforced by a lease in
//     SQLite rather than by hoping the schedule is slower than the work.
//   - A panic in loop code fails that run instead of taking the daemon down.
//   - Every run is recorded, so a loop that has gone quiet is visible rather
//     than merely absent.

const (
	// maxAttempts before a run is dead-lettered. Three is enough to ride out a
	// restart or a brief outage; more just delays the moment someone is told.
	maxAttempts = 3
	// leaseTTL must exceed the run timeout, or a slow run loses its lease and a
	// second copy starts alongside it — the exact overlap this prevents.
	loopRunTimeout = 12 * time.Minute
	leaseTTL       = 15 * time.Minute
	// retryBase is the first backoff; it doubles per attempt (1m, 2m, 4m).
	retryBase = time.Minute
)

// retryDelay is how long to wait before attempt n+1.
func retryDelay(attempt int) time.Duration {
	d := retryBase << (attempt - 1)
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

// runLoopDurable executes a loop with a lease, a run record, and a retry.
//
// Replaces the previous fire-and-forget goroutine. The signature keeps the
// trigger because a retry has to remember what originally caused the work.
func (rt *KarmaxRuntime) runLoopDurable(parent context.Context, l loopkit.Loop, trigger loopkit.Trigger, attempt int) {
	runID := uuid.New().String()
	owner := runID

	// Single-flight. A loop that is already running is not run again: the old
	// behaviour let a 4-minute run on a 2-minute schedule pile up, which is how
	// the same WhatsApp message got answered twice.
	got, err := rt.store.AcquireLoopLease(l.Name, owner, leaseTTL)
	if err != nil {
		rt.log.Warn("loop lease failed; running anyway", zap.String("loop", l.Name), zap.Error(err))
	} else if !got {
		rt.log.Info("loop already running; skipping this trigger",
			zap.String("loop", l.Name), zap.String("trigger", trigger.Kind))
		return
	}
	defer func() {
		if err := rt.store.ReleaseLoopLease(l.Name, owner); err != nil {
			rt.log.Warn("could not release loop lease", zap.String("loop", l.Name), zap.Error(err))
		}
	}()

	executionID := rt.executionFor(l.Name, attempt)

	started := time.Now()
	if err := rt.store.StartLoopRun(store.LoopRun{
		ID: runID, Loop: l.Name, Trigger: trigger.Kind,
		Attempt: attempt, StartedAt: started,
	}); err != nil {
		rt.log.Warn("could not record loop run", zap.String("loop", l.Name), zap.Error(err))
	}

	runErr := rt.executeLoop(parent, l, trigger, executionID)

	if runErr == nil {
		rt.finishExecution(l.Name, executionID)
		if err := rt.store.FinishLoopRun(runID, l.Name, "ok", "", attempt, nil); err != nil {
			rt.log.Warn("could not record loop success", zap.String("loop", l.Name), zap.Error(err))
		}
		rt.log.Info("loopkit loop done",
			zap.String("loop", l.Name), zap.Duration("took", time.Since(started)))
		return
	}

	// Failure. Schedule a retry unless this loop has had its chances.
	status, msg := "failed", runErr.Error()
	var next *time.Time
	if attempt >= maxAttempts {
		status = "dead"
		// Nothing will resume this, so the checkpoints are just clutter.
		rt.finishExecution(l.Name, executionID)
		rt.log.Error("loop failed permanently; work was NOT done",
			zap.String("loop", l.Name), zap.Int("attempts", attempt), zap.Error(runErr))
		// A dead loop is the one thing here worth interrupting someone about:
		// it means scheduled work silently did not happen.
		builtin.PushAppNotification(rt.store, rt.loopDefaultAgent, "alert",
			fmt.Sprintf("Loop %q failed %d times", l.Name, attempt),
			truncErr(msg, 300))
	} else {
		t := time.Now().Add(retryDelay(attempt))
		next = &t
		rt.log.Warn("loop failed; retry scheduled",
			zap.String("loop", l.Name), zap.Int("attempt", attempt),
			zap.Time("retry_at", t), zap.Error(runErr))
	}
	if err := rt.store.FinishLoopRun(runID, l.Name, status, msg, attempt, next); err != nil {
		rt.log.Warn("could not record loop failure", zap.String("loop", l.Name), zap.Error(err))
	}
}

// executionFor returns the execution id this attempt belongs to: a new one for
// a first attempt, the previous one for a retry, so checkpoints carry across.
func (rt *KarmaxRuntime) executionFor(loop string, attempt int) string {
	if attempt > 1 {
		if id, err := rt.store.LoopExecution(loop); err == nil && id != "" {
			if n, err := rt.store.CompletedSteps(id); err == nil && n > 0 {
				rt.log.Info("resuming a loop where it stopped",
					zap.String("loop", loop), zap.Int("steps_done", n), zap.Int("attempt", attempt))
			}
			return id
		}
	}
	id := uuid.New().String()
	if err := rt.store.SetLoopExecution(loop, id); err != nil {
		rt.log.Warn("could not record the loop execution; checkpoints will not carry across attempts",
			zap.String("loop", loop), zap.Error(err))
		return ""
	}
	return id
}

// finishExecution drops the checkpoints once the work is over either way.
func (rt *KarmaxRuntime) finishExecution(loop, executionID string) {
	if executionID == "" {
		return
	}
	if err := rt.store.ClearLoopSteps(executionID); err != nil {
		rt.log.Warn("could not clear loop checkpoints", zap.String("loop", loop), zap.Error(err))
	}
	if err := rt.store.SetLoopExecution(loop, ""); err != nil {
		rt.log.Warn("could not clear the loop execution", zap.String("loop", loop), zap.Error(err))
	}
}

// executeLoop runs the loop body, converting a panic into an error.
//
// Loop code is third-party — it comes from the marketplace registry — so a nil
// map or a bad index in someone's loop must not take down the daemon that runs
// everything else.
func (rt *KarmaxRuntime) executeLoop(parent context.Context, l loopkit.Loop, trigger loopkit.Trigger, executionID string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("loop panicked: %v\n%s", r, debug.Stack())
		}
	}()

	ctx, cancel := context.WithTimeout(parent, loopRunTimeout)
	defer cancel()

	agentID := rt.loopDefaultAgent
	ns := agentID
	if len(rt.cfg.Agents) > 0 && rt.cfg.Agents[0].Memory.Namespace != "" {
		ns = rt.cfg.Agents[0].Memory.Namespace
	}

	k := &loopKit{
		loopName:    l.Name,
		agentID:     agentID,
		namespace:   ns,
		rt:          rt,
		mem:         rt.memory.For(agentID, ns),
		wacliPath:   hostWacli(),
		trigger:     trigger,
		executionID: executionID,
	}
	rt.log.Info("running loopkit loop",
		zap.String("loop", l.Name), zap.String("trigger", trigger.Kind))
	return l.Run(ctx, k)
}

// resumeInterruptedRuns reschedules work the daemon was killed in the middle
// of, which used to be marked dead and left there.
func (rt *KarmaxRuntime) resumeInterruptedRuns() {
	runs, err := rt.store.ReapStaleLoopRuns(time.Now().Add(-leaseTTL))
	if err != nil {
		rt.log.Warn("could not check for interrupted loop runs", zap.Error(err))
		return
	}
	for _, r := range runs {
		if r.Attempt >= maxAttempts {
			rt.log.Error("a loop run was interrupted after its last attempt; work was NOT done",
				zap.String("loop", r.Loop))
			continue
		}
		// Soon rather than immediately: a daemon that crashed mid-run may be
		// about to crash again, and a thundering resume makes that worse.
		next := time.Now().Add(retryDelay(r.Attempt))
		if err := rt.store.FinishLoopRun(r.ID, r.Loop, "interrupted",
			"the daemon stopped mid-run", r.Attempt, &next); err != nil {
			rt.log.Warn("could not schedule a resume", zap.String("loop", r.Loop), zap.Error(err))
			continue
		}
		steps, _ := rt.store.CompletedSteps(mustExecution(rt, r.Loop))
		rt.log.Warn("resuming a loop run the daemon interrupted",
			zap.String("loop", r.Loop), zap.Int("attempt", r.Attempt+1),
			zap.Int("steps_already_done", steps), zap.Time("at", next))
	}
}

func mustExecution(rt *KarmaxRuntime, loop string) string {
	id, _ := rt.store.LoopExecution(loop)
	return id
}

// retryWorker dispatches retries whose time has come, and keeps the run history
// from growing without bound.
//
// A ticker rather than a timer per failure: timers die with the process, and
// the whole point is that a retry survives a restart.
func (rt *KarmaxRuntime) retryWorker(ctx context.Context) {
	rt.resumeInterruptedRuns()

	tick := time.NewTicker(30 * time.Second)
	prune := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	defer prune.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			if _, err := rt.store.PruneLoopRuns(time.Now().AddDate(0, 0, -14)); err != nil {
				rt.log.Warn("could not prune loop run history", zap.Error(err))
			}
			// Only events every subscriber has already read are removed, so a
			// lagging subscriber is never pruned out from under.
			if n, err := rt.store.PruneEventLog(store.DefaultWorkspace, time.Now().AddDate(0, 0, -14)); err != nil {
				rt.log.Warn("could not prune the event log", zap.Error(err))
			} else if n > 0 {
				rt.log.Info("pruned consumed events", zap.Int64("count", n))
			}
			// Only timers that already fired; an armed one is waiting, not stale.
			if _, err := rt.store.PruneTimers(time.Now().AddDate(0, 0, -14)); err != nil {
				rt.log.Warn("could not prune fired timers", zap.Error(err))
			}
			if _, err := rt.store.PruneLoopSteps(time.Now().AddDate(0, 0, -7)); err != nil {
				rt.log.Warn("could not prune loop checkpoints", zap.Error(err))
			}
		case <-tick.C:
			due, err := rt.store.DueLoopRetries(time.Now())
			if err != nil {
				rt.log.Warn("could not read due loop retries", zap.Error(err))
				continue
			}
			for _, st := range due {
				l, ok := rt.loopkitLoops[st.Loop]
				if !ok {
					// The loop was uninstalled between the failure and the
					// retry; clear it rather than retrying forever.
					_ = rt.store.ClearLoopRetry(st.Loop)
					continue
				}
				// Cleared before dispatch so the next tick does not dispatch
				// the same retry again while this one is still running.
				if err := rt.store.ClearLoopRetry(st.Loop); err != nil {
					rt.log.Warn("could not clear loop retry", zap.String("loop", st.Loop), zap.Error(err))
					continue
				}
				rt.log.Info("retrying failed loop",
					zap.String("loop", st.Loop), zap.Int("attempt", st.RetryAttempt+1))
				go rt.runLoopDurable(ctx, l,
					loopkit.Trigger{Kind: "retry"}, st.RetryAttempt+1)
			}
		}
	}
}

// LoopHealth is one loop's standing, as `karmax status` reports it.
type LoopHealth struct {
	Name        string     `json:"name"`
	Schedule    string     `json:"schedule,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	LastFailure *time.Time `json:"last_failure,omitempty"`
	ConsecFails int        `json:"consecutive_failures"`
	Running     bool       `json:"running"`
	RetryAt     *time.Time `json:"retry_at,omitempty"`
	// Dark means the loop has not succeeded in long enough that something is
	// wrong even though nothing is currently failing — the state the daemon had
	// no way to express while memory-merge quietly stopped for three days.
	Dark      bool   `json:"dark"`
	LastError string `json:"last_error,omitempty"`
}

// LoopHealthReport returns the standing of every registered loop.
//
// Registered rather than only-those-with-history on purpose: a loop that has
// never run at all is exactly the case worth surfacing, and joining from the
// state table alone would leave it out.
func (rt *KarmaxRuntime) LoopHealthReport() ([]LoopHealth, error) {
	states, err := rt.store.LoopStates()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]store.LoopState, len(states))
	for _, s := range states {
		byName[s.Loop] = s
	}

	now := time.Now()
	out := make([]LoopHealth, 0, len(rt.loopkitLoops))
	for name, l := range rt.loopkitLoops {
		h := LoopHealth{Name: name, Schedule: renderSchedule(l.Schedule)}
		st, seen := byName[name]
		if seen {
			h.LastSuccess = st.LastSuccessAt
			h.LastFailure = st.LastFailureAt
			h.ConsecFails = st.ConsecFails
			h.RetryAt = st.NextRetryAt
			h.LastError = st.LastError
			h.Running = st.LeaseUntil != nil && st.LeaseUntil.After(now)
		}
		h.Dark = isDark(l.Schedule, h.LastSuccess, seen, now, rt.startedAt)
		out = append(out, h)
	}
	sortLoopHealth(out)
	return out, nil
}

// isDark decides whether a scheduled loop has been quiet for too long.
//
// The threshold comes from the loop's own schedule rather than a fixed number,
// because "@every 2m" silent for an hour and "@every 12h" silent for an hour
// are very different events. Three missed intervals is the signal: one is a
// blip, three is a pattern.
//
// Measured from the later of the last success and the daemon's start. A loop
// that has never succeeded because the process came up ninety seconds ago has
// not gone dark — it has not had a turn yet, and reporting it as broken on
// every restart is how a health page teaches people to ignore it.
func isDark(schedule loopkit.Schedule, lastSuccess *time.Time, seen bool, now, since time.Time) bool {
	interval, ok := scheduleInterval(schedule)
	if !ok {
		return false // manual and event-driven loops are not expected to tick
	}
	quietSince := since
	if seen && lastSuccess != nil && lastSuccess.After(quietSince) {
		quietSince = *lastSuccess
	}
	return now.Sub(quietSince) > 3*interval
}

// scheduleInterval reads the interval form. A cron spec returns false: its
// period is not a duration, and guessing one would raise false alarms against
// a loop that legitimately runs once a day.
func scheduleInterval(schedule loopkit.Schedule) (time.Duration, bool) {
	every := strings.TrimSpace(schedule.Every)
	if every == "" {
		return 0, false
	}
	d, err := time.ParseDuration(every)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// renderSchedule describes a schedule for display.
func renderSchedule(s loopkit.Schedule) string {
	switch {
	case strings.TrimSpace(s.Every) != "":
		return "@every " + strings.TrimSpace(s.Every)
	case strings.TrimSpace(s.Cron) != "":
		return strings.TrimSpace(s.Cron)
	default:
		return ""
	}
}

func sortLoopHealth(hs []LoopHealth) {
	// Worst first: dark, then failing, then healthy. An operator reading this
	// wants the problems at the top, not alphabetical order.
	rank := func(h LoopHealth) int {
		switch {
		case h.Dark:
			return 0
		case h.ConsecFails > 0:
			return 1
		default:
			return 2
		}
	}
	for i := range hs {
		for j := i + 1; j < len(hs); j++ {
			if rank(hs[j]) < rank(hs[i]) ||
				(rank(hs[j]) == rank(hs[i]) && hs[j].Name < hs[i].Name) {
				hs[i], hs[j] = hs[j], hs[i]
			}
		}
	}
}

func truncErr(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// hostWacli locates the wacli binary, matching what the rest of the runtime uses.
func hostWacli() string { return hostpaths.Wacli() }
