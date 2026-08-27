package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/sandbox"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// Reviving runs that are waiting on the world.
//
// A run that called Await ended. What brings it back is this: every event is
// checked against the parked waiters, and the one that matches resolves its
// waiter and re-runs the loop on the SAME execution — so the steps it already
// finished return their recorded results and it carries on from the wait.

const (
	waiterSweepEvery      = time.Minute
	sandboxPollEvery      = 5 * time.Second
	defaultSandboxTimeout = 45 * time.Minute
)

// startWaiters wires the two ways a parked run comes back: a matching event,
// and a deadline passing.
func (rt *KarmaxRuntime) startWaiters(ctx context.Context) {
	// Every kind, because what a recipe waits on is decided when it runs, not
	// when the daemon starts.
	rt.bus.Consume(ctx, bus.SubWaiters, nil, func(_ context.Context, evt bus.Event) error {
		rt.resolveWaiters(ctx, string(evt.Kind), evt.Payload)
		return nil
	})

	go func() {
		t := time.NewTicker(waiterSweepEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rt.sweepWaiters(ctx)
			}
		}
	}()
}

// resolveWaiters wakes every parked run whose match this event satisfies.
func (rt *KarmaxRuntime) resolveWaiters(ctx context.Context, kind string, payload map[string]any) {
	pending, err := rt.store.PendingWaiters(kind)
	if err != nil {
		rt.log.Warn("could not read pending waiters", zap.String("kind", kind), zap.Error(err))
		return
	}
	for _, w := range pending {
		if !waiterMatches(w.MatchJSON, payload) {
			continue
		}
		body, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		// Compare-and-set: two events arriving together must not resume the
		// same run twice.
		won, err := rt.store.ResolveWaiter(w.ID, string(body))
		if err != nil || !won {
			continue
		}
		rt.log.Info("resuming a run that was waiting",
			zap.String("loop", w.Loop), zap.String("event", kind))
		rt.resumeWaiter(ctx, w, payload)
	}
}

// sweepWaiters gives up on waits whose deadline has passed. The run is resumed
// rather than abandoned, so the recipe gets to decide what a timeout means.
func (rt *KarmaxRuntime) sweepWaiters(ctx context.Context) {
	expired, err := rt.store.ExpireWaiters(time.Now())
	if err != nil {
		rt.log.Warn("could not expire waiters", zap.Error(err))
		return
	}
	for _, w := range expired {
		rt.log.Info("a wait timed out",
			zap.String("loop", w.Loop), zap.String("event", w.EventKind))
		rt.resumeWaiter(ctx, w, map[string]any{"timeout": true, "event": w.EventKind})
	}
}

// resumeWaiter re-runs the loop on its existing execution.
//
// Attempt 2 is what makes executionFor reuse the stored execution id, which is
// what makes the replay skip every step that already finished.
func (rt *KarmaxRuntime) resumeWaiter(ctx context.Context, w store.Waiter, payload map[string]any) {
	l, found := rt.loopkitLoops[w.Loop]
	if !found {
		rt.log.Warn("a waiter names a loop that is no longer installed",
			zap.String("loop", w.Loop))
		return
	}
	trigger := loopkit.Trigger{Kind: loopkit.TriggerEvent, Payload: map[string]any{}}
	for k, v := range payload {
		trigger.Payload[k] = v
	}
	trigger.Payload["event_kind"] = w.EventKind
	trigger.Payload["resumed"] = true
	go rt.runLoopOn(ctx, l, trigger, 2, w.ExecutionID)
}

// waiterMatches reports whether every field the recipe asked for is present in
// the event with that value. An empty match is deliberately permissive — the
// recipe said "any event of this kind".
func waiterMatches(matchJSON string, payload map[string]any) bool {
	if strings.TrimSpace(matchJSON) == "" || matchJSON == "{}" || matchJSON == "null" {
		return true
	}
	var want map[string]string
	if err := json.Unmarshal([]byte(matchJSON), &want); err != nil {
		return false
	}
	for k, v := range want {
		got, ok := payload[k]
		if !ok || !strings.EqualFold(fmt.Sprint(got), v) {
			return false
		}
	}
	return true
}

// reconcileSandboxes is startup recovery for containers.
//
// A sandbox outlives the process that launched it. Without this the daemon
// restarts, forgets the container, and leaves it running against a repo with a
// live token — so every run recorded as in-flight is asked about once, and
// closed out if it has finished or vanished.
func (rt *KarmaxRuntime) reconcileSandboxes(ctx context.Context) {
	live, err := rt.store.LiveSandboxRuns()
	if err != nil {
		rt.log.Warn("could not list live sandbox runs", zap.Error(err))
		return
	}
	if len(live) == 0 {
		return
	}
	drv, err := rt.sandboxDriver()
	if err != nil {
		rt.log.Warn("sandbox runs are in flight but no driver is available",
			zap.Int("runs", len(live)), zap.Error(err))
		return
	}
	for _, r := range live {
		if r.ContainerID == "" {
			_ = rt.store.UpdateSandboxRun(r.ID, sandbox.StateGone, 0, "never recorded a container", "")
			continue
		}
		st, err := drv.Poll(ctx, r.ContainerID)
		if err != nil || st.State == sandbox.StateGone {
			_ = rt.store.UpdateSandboxRun(r.ID, sandbox.StateGone, 0, "lost across a restart", "")
			continue
		}
		if st.State == sandbox.StateExited || st.State == sandbox.StateFailed {
			logs, _ := drv.Logs(ctx, r.ContainerID, 200)
			_ = rt.store.UpdateSandboxRun(r.ID, st.State, st.ExitCode, "", logs)
			_ = drv.Kill(ctx, r.ContainerID)
			continue
		}
		rt.log.Info("a sandbox survived the restart and is still running",
			zap.String("run", r.ID), zap.String("container", r.ContainerID))
	}
}
