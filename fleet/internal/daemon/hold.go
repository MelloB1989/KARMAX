package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/reconcile"
)

// Hold is what keeps a worker from new tasks for its quota, and when that
// lifts: a rate limit it hit (cooling, until its window resets), its 5-hour
// window at low_5h or more (low), its 7-day window at reserved_7d or more
// (reserved). An agent's slots share its account and its rate limit, so
// they share its hold.
type Hold struct {
	Why []string `json:"why"` // "rate-limited", "5h 92%", "7d 95%"
	// Until is when the last of them lifts; zero when a window's reset is
	// not known (no turn has reported it).
	Until time.Time `json:"until,omitzero"`
}

// Held reports whether anything holds the worker back.
func (h Hold) Held() bool { return len(h.Why) > 0 }

// Lifts says when h lifts, for people: "at 04:10 (in 2h13m)".
func (h Hold) Lifts(now time.Time) string {
	if h.Until.IsZero() {
		return "when a turn reports its new window (the reset is not known)"
	}
	return "at " + When(h.Until, now)
}

// String is the hold for people: "5h 92%, lifts at 04:10 (in 2h13m)".
func (h Hold) String(now time.Time) string {
	return strings.Join(h.Why, ", ") + ", lifts " + h.Lifts(now)
}

// When is t for people, in local time, with how far off it is: "04:10 (in
// 2h13m)", "Tue 10:00 (in 3d4h)" when more than 20 hours away.
func When(t, now time.Time) string {
	f := "15:04"
	if d := t.Sub(now); d > 20*time.Hour || d < -20*time.Hour {
		f = "Mon 15:04"
	}
	s := t.Local().Format(f)
	if d := t.Sub(now); d > 0 {
		return s + " (in " + Dur(d) + ")"
	}
	return s + " (now)"
}

func hold(phase reconcile.Phase, cooling time.Time, low, reserved bool, five, seven float64, fiveResets, sevenResets time.Time) Hold {
	var h Hold
	known := true
	lift := func(t time.Time) {
		if t.IsZero() {
			known = false
		} else if t.After(h.Until) {
			h.Until = t
		}
	}
	if phase == reconcile.Cooling {
		h.Why = append(h.Why, "rate-limited")
		lift(cooling)
	}
	if low {
		h.Why = append(h.Why, fmt.Sprintf("5h %.0f%%", five))
		lift(fiveResets)
	}
	if reserved {
		h.Why = append(h.Why, fmt.Sprintf("7d %.0f%%", seven))
		lift(sevenResets)
	}
	if !known {
		h.Until = time.Time{}
	}
	return h
}

// Hold is the row's quota hold, as of the row: a window that has reset
// since the last tick no longer holds it.
func (r Row) Hold() Hold {
	return hold(reconcile.Phase(r.State), r.Cooling, r.Low, r.Reserved, r.FiveHour, r.SevenDay, r.FiveResets, r.SevenResets)
}

func stateHold(st reconcile.State) Hold {
	return hold(st.Phase, st.CoolingUntil, st.Low, st.Reserved, st.Quota.FiveHour, st.Quota.SevenDay,
		st.Quota.FiveHourResets, st.Quota.SevenDayResets)
}

// quotaNews is what the orchestrator hears of an agent's quota, from its
// slot 1's tick (its slots share the account): that the agent is held back
// from new tasks and when that lifts, and, once it has been held, that it is
// free again. A rate limit alone is told already (fleet.agent.cooling, with
// its end). next.QuotaHeld remembers a hold until its end is told, so an
// end the agent spent down or out of reach is told when it is back.
func quotaNews(agent string, old, next reconcile.State, now time.Time) (reconcile.State, []reconcile.Action) {
	push := func(event, text string) []reconcile.Action {
		return []reconcile.Action{{Kind: reconcile.Push, Event: "fleet.agent." + event, Text: text}}
	}
	h := stateHold(next)
	switch {
	case h.Held():
		next.QuotaHeld = true
		quota := next.Low && !old.Low || next.Reserved && !old.Reserved
		if !quota || next.Phase == reconcile.Cooling {
			return next, nil
		}
		return next, push("quota_held", fmt.Sprintf("%s is held back by its quota (%s): new tasks go to other agents until it lifts %s; tasks it has carry on",
			agent, strings.Join(h.Why, ", "), h.Lifts(now)))
	case next.QuotaHeld:
		switch next.Phase {
		case reconcile.Standby, reconcile.Waiting, reconcile.Working:
		default:
			return next, nil // down, disabled, starting: told when it is back
		}
		if !next.UnreachableSince.IsZero() {
			return next, nil
		}
		next.QuotaHeld = false
		text := agent + " is free of its quota hold: it takes tasks again"
		if was := stateHold(old); was.Held() {
			text = fmt.Sprintf("%s is free of its quota hold (it was %s): it takes tasks again", agent, strings.Join(was.Why, ", "))
		}
		return next, push("quota_reset", text)
	}
	return next, nil
}
