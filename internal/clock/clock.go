// Package clock fires durable timers into the event log.
//
// A stateless harness cannot say "wait three days, then continue" — it exits
// and the intention goes with it. A timer here survives a restart, a crash, or
// a week of downtime, which is what makes "check back Thursday" a primitive
// rather than a cron job that re-derives its own state.
package clock

import (
	"context"
	"fmt"
	"time"

	"github.com/MelloB1989/karmax/internal/bus"
	"github.com/MelloB1989/karmax/internal/store"
	"go.uber.org/zap"
)

// resolution bounds how late a timer can be. Timers here measure hours and
// days, so a few seconds of lag costs nothing and a tight tick would just wake
// the disk for no one.
const resolution = 10 * time.Second

// batch caps one sweep, so a backlog after long downtime is worked through
// rather than published in one burst.
const batch = 64

type Clock struct {
	store     *store.Store
	bus       *bus.Log
	workspace string
	log       *zap.Logger
	tick      time.Duration
}

func New(s *store.Store, b *bus.Log, workspace string, log *zap.Logger) *Clock {
	if workspace == "" {
		workspace = store.DefaultWorkspace
	}
	return &Clock{store: s, bus: b, workspace: workspace, log: log, tick: resolution}
}

// Arm schedules a timer. Re-arming the same id moves its deadline.
func (c *Clock) Arm(t store.Timer) error {
	if t.Workspace == "" {
		t.Workspace = c.workspace
	}
	if t.Kind == "" {
		t.Kind = string(bus.EventTimerFired)
	}
	if t.FireAt.IsZero() {
		return fmt.Errorf("clock: timer %q has no deadline", t.ID)
	}
	return c.store.SetTimer(t)
}

// After is Arm with the deadline expressed as a delay.
func (c *Clock) After(t store.Timer, d time.Duration) error {
	t.FireAt = time.Now().Add(d)
	return c.Arm(t)
}

// Cancel disarms a timer.
func (c *Clock) Cancel(id string) error { return c.store.CancelTimer(id) }

// Pending lists armed timers, earliest first.
func (c *Clock) Pending(limit int) ([]store.Timer, error) {
	return c.store.PendingTimers(c.workspace, limit)
}

func (c *Clock) Start(ctx context.Context) {
	go func() {
		// Immediately, not after the first tick: timers that came due while the
		// process was down are the ones that most need firing.
		c.sweep()
		t := time.NewTicker(c.tick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.sweep()
			}
		}
	}()
}

func (c *Clock) sweep() {
	due, err := c.store.DueTimers(c.workspace, time.Now(), batch)
	if err != nil {
		c.log.Warn("could not read due timers", zap.Error(err))
		return
	}
	for _, t := range due {
		if err := c.bus.Publish(eventFor(t)); err != nil {
			// Left armed on purpose: the next sweep retries rather than losing it.
			c.log.Error("timer could not be published; it stays armed",
				zap.String("timer", t.ID), zap.Error(err))
			continue
		}
		if _, err := c.store.MarkTimerFired(t.ID, time.Now()); err != nil {
			c.log.Warn("could not mark a timer fired", zap.String("timer", t.ID), zap.Error(err))
		}
	}
}

// eventFor builds the event a timer publishes.
//
// The id is derived from the timer rather than random, so publishing it twice —
// which happens when the process dies between the append and the mark — appends
// once. That turns at-least-once delivery into exactly-once firing.
func eventFor(t store.Timer) bus.Event {
	payload := map[string]any{}
	for k, v := range t.Payload {
		payload[k] = v
	}
	payload["timer_id"] = t.ID
	payload["fire_at"] = t.FireAt.Format(time.RFC3339)
	if t.Loop != "" {
		payload["loop"] = t.Loop
	}
	return bus.Event{
		ID:        fmt.Sprintf("timer:%s:%d", t.ID, t.FireAt.Unix()),
		Kind:      bus.EventKind(t.Kind),
		AgentID:   t.AgentID,
		Timestamp: time.Now(),
		Payload:   payload,
	}
}
