package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools/builtin"
	"github.com/MelloB1989/karmax/pkg/loopkit"
	"go.uber.org/zap"
)

// A loop's triggering event, owned from delivery until its run ends.
//
// The loop subscribers start a run in a goroutine and return, and the bus
// moves past the event as soon as they do; a loop already running queued the
// event in memory. So a crash at any point before the run ended — while it
// waited its turn, while a workflow triaged the message, in the second before
// it handed the message to the full agent — lost the message, and a run
// resumed after a crash came back with no trigger at all.
//
// The subscriber now records the event (store.LoopTrigger) before it
// returns; the run marks it running, then done or due a retry when it ends.
// At start every trigger still open is replayed with its payload — unless
// its run had already acted (a replay would say it twice; the operator is
// told instead), or the request it handed the message to owns it now.

// triggerEventID is the bus event behind an event- or webhook-triggered run.
func triggerEventID(t loopkit.Trigger) string {
	if t.Kind != loopkit.TriggerEvent && t.Kind != loopkit.TriggerWebhook {
		return ""
	}
	id, _ := t.Payload["event_id"].(string)
	return strings.TrimSpace(id)
}

// triggerKey is a run's trigger row, or "" for a run no event triggered.
func triggerKey(loop string, t loopkit.Trigger) string {
	id := triggerEventID(t)
	if id == "" {
		return ""
	}
	return store.LoopTriggerKey(loop, id)
}

// deliverToLoops is a loop subscriber's whole job: own the event for every
// loop listening, then start their runs. Every trigger is recorded before any
// run starts and before this returns — an event that cannot be recorded is
// not acknowledged, so the bus delivers it again — and a redelivery starts no
// second run for a loop that already owns the event.
func (rt *KarmaxRuntime) deliverToLoops(ctx context.Context, loops []string, t loopkit.Trigger) error {
	var start []loopkit.Loop
	for _, name := range loops {
		l, found := rt.loopkitLoops[name]
		if !found {
			continue
		}
		created, err := rt.ownLoopTriggerNew(name, t)
		if err != nil {
			return fmt.Errorf("record %s's trigger: %w", name, err)
		}
		if created {
			start = append(start, l)
		}
	}
	for _, l := range start {
		go rt.runLoopDurable(ctx, l, t, 1)
	}
	return nil
}

// ownLoopTrigger records an event a loop is about to run for, before the
// subscriber returns and the bus moves past it.
func (rt *KarmaxRuntime) ownLoopTrigger(loop string, t loopkit.Trigger) error {
	_, err := rt.ownLoopTriggerNew(loop, t)
	return err
}

// ownLoopTriggerNew is ownLoopTrigger, reporting whether the trigger is new
// (true too for a run no event triggered, which has nothing to record).
func (rt *KarmaxRuntime) ownLoopTriggerNew(loop string, t loopkit.Trigger) (bool, error) {
	id := triggerEventID(t)
	if id == "" || rt.store == nil {
		return true, nil
	}
	raw, err := json.Marshal(t.Payload)
	if err != nil {
		return false, err
	}
	return rt.store.RecordLoopTrigger(store.LoopTrigger{Loop: loop, EventID: id,
		Conversation: conversationOf(t), Kind: t.Kind, Payload: string(raw)})
}

// markTrigger moves a run's trigger along; attempt counts a run starting.
func (rt *KarmaxRuntime) markTrigger(loop string, t loopkit.Trigger, status string, attempt bool) {
	key := triggerKey(loop, t)
	if key == "" || rt.store == nil {
		return
	}
	if err := rt.store.SetLoopTriggerStatus(key, status, attempt); err != nil {
		rt.log.Warn("could not record a loop trigger's progress", zap.String("loop", loop), zap.Error(err))
	}
}

// finishTrigger closes a run's trigger, and with it the triggers that waited
// in the same conversation before it: the pass read the whole thread.
func (rt *KarmaxRuntime) finishTrigger(loop string, t loopkit.Trigger) {
	key := triggerKey(loop, t)
	if key == "" || rt.store == nil {
		return
	}
	rt.markTrigger(loop, t, store.LoopTriggerDone, false)
	if row, found, err := rt.store.GetLoopTrigger(key); err == nil && found && row.Conversation != "" {
		if _, err := rt.store.SupersedeLoopTriggers(loop, row.Conversation, row.CreatedAt); err != nil {
			rt.log.Warn("could not close the triggers a pass answered", zap.String("loop", loop), zap.Error(err))
		}
	}
}

// markTriggerActed records that a run has put something in front of another
// person, so a crash after it is not replayed into a second message.
func (rt *KarmaxRuntime) markTriggerActed(loop string, t loopkit.Trigger) {
	if key := triggerKey(loop, t); key != "" && rt.store != nil {
		_ = rt.store.MarkLoopTriggerActed(key)
	}
}

// setTriggerOwner records the request a run handed its message to.
func (rt *KarmaxRuntime) setTriggerOwner(loop string, t loopkit.Trigger, requestID string) {
	if key := triggerKey(loop, t); key != "" && rt.store != nil {
		if err := rt.store.SetLoopTriggerOwner(key, requestID); err != nil {
			rt.log.Warn("could not record which request owns a loop's message", zap.String("loop", loop), zap.Error(err))
		}
	}
}

// triggerFromRow is the trigger a recorded event replays as.
func triggerFromRow(row store.LoopTrigger) (loopkit.Trigger, error) {
	payload := map[string]any{}
	if strings.TrimSpace(row.Payload) != "" {
		if err := json.Unmarshal([]byte(row.Payload), &payload); err != nil {
			return loopkit.Trigger{}, err
		}
	}
	payload["event_id"] = row.EventID
	// Said, so a workflow that cares can re-read the thread before it acts.
	payload["replayed"] = true
	kind := row.Kind
	if kind == "" {
		kind = loopkit.TriggerEvent
	}
	return loopkit.Trigger{Kind: kind, Payload: payload}, nil
}

// replayLoopTriggers runs, at start, every event a loop was triggered by and
// never finished with.
func (rt *KarmaxRuntime) replayLoopTriggers(ctx context.Context) {
	if rt.store == nil {
		return
	}
	open, err := rt.store.OpenLoopTriggers("")
	if err != nil {
		rt.log.Warn("could not read the loop triggers the daemon died with", zap.Error(err))
		return
	}
	for _, row := range open {
		l, ok := rt.loopkitLoops[row.Loop]
		if !ok {
			_ = rt.store.SetLoopTriggerStatus(row.Key, store.LoopTriggerDone, false)
			continue
		}
		if skip, why := rt.replayBlocked(row); skip {
			rt.log.Info("not replaying a loop trigger", zap.String("loop", row.Loop),
				zap.String("event", row.EventID), zap.String("why", why))
			_ = rt.store.SetLoopTriggerStatus(row.Key, store.LoopTriggerDone, false)
			continue
		}
		trig, err := triggerFromRow(row)
		if err != nil {
			rt.log.Warn("a loop trigger is unreadable", zap.String("loop", row.Loop), zap.Error(err))
			_ = rt.store.SetLoopTriggerStatus(row.Key, store.LoopTriggerDone, false)
			continue
		}
		rt.log.Info("replaying an event a loop never finished with",
			zap.String("loop", row.Loop), zap.String("event", row.EventID), zap.Int("attempt", row.Attempts+1))
		go rt.runLoopDurable(ctx, l, trig, row.Attempts+1)
	}
}

// replayBlocked reports whether a recorded trigger must not be replayed.
func (rt *KarmaxRuntime) replayBlocked(row store.LoopTrigger) (bool, string) {
	trig, _ := triggerFromRow(row)
	switch {
	case row.Acted:
		// The loop tier never retries a run that has messaged somebody: the
		// retry re-composes and sends again, in other words. Told instead.
		builtin.PushAppNotification(rt.store, rt.loopDefaultAgent, "alert",
			fmt.Sprintf("Loop %q was cut off mid-way", row.Loop),
			fmt.Sprintf("KARMAX stopped while %s was handling a message (%s) after it had already sent something, so it was not run again. Check that conversation.",
				row.Loop, row.Conversation))
		return true, "its run had already acted"
	case row.Owner != "":
		if _, found, _ := rt.store.AgentTurnByEvent(row.Owner); found {
			return true, "the request it was handed to owns it (" + row.Owner + ")"
		}
	}
	if queuedTooLong(trig) {
		return true, "too old to act on now"
	}
	if row.Attempts >= maxAttempts {
		builtin.PushAppNotification(rt.store, rt.loopDefaultAgent, "alert",
			fmt.Sprintf("Loop %q gave up on a message", row.Loop),
			fmt.Sprintf("%s did not finish with a message (%s) in %d attempts across restarts.", row.Loop, row.Conversation, row.Attempts))
		return true, "out of attempts"
	}
	return false, ""
}

// retryLoopTriggers replays a loop's triggers whose runs failed, with their
// payloads, when the loop's retry comes due. False when there were none and
// the retry should run as it always has.
func (rt *KarmaxRuntime) retryLoopTriggers(ctx context.Context, l loopkit.Loop) bool {
	if rt.store == nil {
		return false
	}
	rows, err := rt.store.OpenLoopTriggers(l.Name, store.LoopTriggerRetry)
	if err != nil || len(rows) == 0 {
		return false
	}
	for _, row := range rows {
		if skip, why := rt.replayBlocked(row); skip {
			rt.log.Info("not retrying a loop trigger", zap.String("loop", l.Name), zap.String("why", why))
			_ = rt.store.SetLoopTriggerStatus(row.Key, store.LoopTriggerDone, false)
			continue
		}
		trig, err := triggerFromRow(row)
		if err != nil {
			_ = rt.store.SetLoopTriggerStatus(row.Key, store.LoopTriggerDone, false)
			continue
		}
		go rt.runLoopDurable(ctx, l, trig, row.Attempts+1)
	}
	return true
}
