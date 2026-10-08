// Package reconcile decides what to do about one agent, once per tick.
//
// Decide is pure: it is handed the agent's remembered State and what was just
// observed, and returns the new State and the actions to take. fleetd runs
// the actions. Keeping the decision free of I/O is what lets every row of the
// lifecycle table (docs/AGENT-FLEET.md) be tested without a container.
package reconcile

import (
	"fmt"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

// Phase is where an agent is in its lifecycle.
type Phase string

const (
	Standby  Phase = "standby"  // idle, no task: ready for work
	Working  Phase = "working"  // a turn in flight
	Waiting  Phase = "waiting"  // idle with a task open
	Cooling  Phase = "cooling"  // rate-limited until its window resets
	Disabled Phase = "disabled" // its subscription refused it; needs you
	Down     Phase = "down"     // its container is not running
)

// State is what fleetd remembers about one agent between ticks.
type State struct {
	Name      string `json:"name"`
	Phase     Phase  `json:"phase"`
	SessionID string `json:"session_id"`
	Task      string `json:"task,omitempty"`
	// Done is set by `fleetctl done`; the next idle tick rotates.
	Done bool `json:"done,omitempty"`

	LastBusy       time.Time   `json:"last_busy"`
	Restarts       []time.Time `json:"restarts,omitempty"`
	NudgedAt       time.Time   `json:"nudged_at,omitempty"`
	CompactedAt    int64       `json:"compacted_at,omitempty"` // transcript size when /compact was typed
	CoolingUntil   time.Time   `json:"cooling_until,omitempty"`
	DisabledReason string      `json:"disabled_reason,omitempty"`

	// Low and Reserved are quota flags, independent of the phase: a low agent
	// still works, but new tasks should prefer others.
	Low      bool  `json:"low,omitempty"`
	Reserved bool  `json:"reserved,omitempty"`
	Quota    Quota `json:"quota"`

	ContextPct float64 `json:"context_pct,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Alerted keys alerts already sent, so each fires once per episode.
	Alerted map[string]bool `json:"alerted,omitempty"`
}

// Quota is the account's last known windows. It is the account's truth, so
// it is kept across sessions and aged out only at its reset.
type Quota struct {
	FiveHour       float64   `json:"five_hour"`
	FiveHourResets time.Time `json:"five_hour_resets"`
	SevenDay       float64   `json:"seven_day"`
	SevenDayResets time.Time `json:"seven_day_resets"`
	At             time.Time `json:"at"`
}

// Obs is one tick's observation of an agent.
type Obs struct {
	Now         time.Time
	ContainerUp bool
	PaneAlive   bool
	// PanePID is the pid of the claude the pane runs, when known; it tells the
	// agent's own session from a stray using the same name.
	PanePID         int
	Sessions        []observe.Agent // interactive sessions in the container
	Status          *observe.Status // latest status-line snapshot
	Events          []observe.Event // hook events since the last tick
	TranscriptSize  int64
	TranscriptMTime time.Time
}

// Kind is what an action does.
type Kind string

const (
	Restart      Kind = "restart"       // restart the pane on Session (resume)
	Rotate       Kind = "rotate"        // archive Session, start a fresh one
	ArchiveStray Kind = "archive_stray" // archive and stop a session that is not the agent's
	Compact      Kind = "compact"       // type /compact into the pane
	Prompt       Kind = "prompt"        // type Text into the pane
	Push         Kind = "push"          // tell the orchestrator (webhook)
	Alert        Kind = "alert"         // tell you (phone)
)

// Reasons a session ends.
const (
	ReasonTaskDone = "task_done"
	ReasonStale    = "stale"
	ReasonCrashed  = "crashed"
	ReasonStray    = "stray"
	ReasonRestored = "restored"
)

// Action is one thing for fleetd to do.
type Action struct {
	Kind    Kind
	Session string
	PID     int
	Task    string
	Reason  string
	Event   string // Push: fleet.agent.<…>
	Text    string
}

func (a Action) String() string {
	return fmt.Sprintf("%s(%s %s %s)", a.Kind, a.Session, a.Reason, a.Event)
}

// Decide is one tick for one agent.
func Decide(th config.Thresholds, st State, o Obs) (State, []Action) {
	var acts []Action
	if st.Alerted == nil {
		st.Alerted = map[string]bool{}
	}
	push := func(event, text string) {
		acts = append(acts, Action{Kind: Push, Event: "fleet.agent." + event, Text: text})
	}
	alertOnce := func(key, text string) {
		if !st.Alerted[key] {
			st.Alerted[key] = true
			acts = append(acts, Action{Kind: Alert, Text: text})
		}
	}

	st = observeQuota(th, st, o)
	if st.Reserved {
		alertOnce("reserved", fmt.Sprintf("%s: 7-day window at %.0f%% — small tasks only until it resets", st.Name, st.Quota.SevenDay))
	} else {
		delete(st.Alerted, "reserved")
	}

	// A subscription that refused the agent is not fought with restarts.
	if st.Phase == Disabled {
		return st, nil
	}
	for _, e := range o.Events {
		if e.Event != "StopFailure" {
			continue
		}
		switch kind := e.ErrorType(); kind {
		case "authentication_failed", "billing_error", "account_on_hold":
			st.Phase, st.DisabledReason = Disabled, kind
			push("disabled", st.Name+" disabled: "+kind)
			alertOnce("disabled", fmt.Sprintf("%s disabled: %s — its subscription needs you", st.Name, kind))
			return st, acts
		case "rate_limit":
			until := st.Quota.FiveHourResets
			if !until.After(o.Now) {
				until = o.Now.Add(th.CoolingDefault)
			}
			if st.Phase != Cooling {
				push("cooling", fmt.Sprintf("%s rate-limited until %s", st.Name, until.Format(time.Kitchen)))
			}
			st.Phase, st.CoolingUntil = Cooling, until
		}
	}

	if !o.ContainerUp {
		if st.Phase != Down {
			push("down", st.Name+"'s container is not running")
		}
		st.Phase = Down
		return st, acts
	}

	// Which session is the agent's: the one the pane runs, else the one named
	// for the agent. Every other session in the container is a stray.
	var mine *observe.Agent
	for i, s := range o.Sessions {
		if s.Name == st.Name && (o.PanePID == 0 || s.PID == o.PanePID) {
			mine = &o.Sessions[i]
			break
		}
	}
	for _, s := range o.Sessions {
		if (mine != nil && s.SessionID == mine.SessionID) || s.Activity() != "idle" {
			continue
		}
		acts = append(acts, Action{Kind: ArchiveStray, Session: s.SessionID, PID: s.PID, Reason: ReasonStray})
	}

	if mine == nil || !o.PaneAlive {
		st = restart(th, st, o, &acts, push, alertOnce)
		return st, acts
	}
	st.SessionID = mine.SessionID
	if st.Phase == Down {
		st.Phase = Standby
	}
	delete(st.Alerted, "crashloop")
	busy := mine.Activity() == "busy"
	if busy {
		st.LastBusy = o.Now
	}
	idleSince := st.LastBusy
	if o.TranscriptMTime.After(idleSince) {
		idleSince = o.TranscriptMTime
	}
	idle := o.Now.Sub(idleSince)

	if st.Phase == Cooling {
		if o.Now.Before(st.CoolingUntil) {
			return st, acts // cooling is never stale, and never archived for it
		}
		st.CoolingUntil = time.Time{}
		if st.Task != "" && !busy {
			acts = append(acts, Action{Kind: Prompt, Text: "Your rate-limit window has reset. Continue the task you were working on."})
		}
	}

	switch {
	case busy:
		st.Phase = Working
	case st.Task != "":
		st.Phase = Waiting
	default:
		st.Phase = Standby
	}
	if busy {
		st.NudgedAt = time.Time{}
		return st, acts
	}

	// Everything below acts on an idle session only.
	rotate := func(reason string) {
		acts = append(acts, Action{Kind: Rotate, Session: st.SessionID, Task: st.Task, Reason: reason})
		st.Task, st.Done, st.Phase = "", false, Standby
		st.NudgedAt, st.CompactedAt, st.LastBusy = time.Time{}, 0, o.Now
	}
	switch {
	case st.Done:
		rotate(ReasonTaskDone)
		return st, acts
	case st.Task == "" && idle > th.StandbyIdle && o.TranscriptSize >= th.StandbyMinTranscript:
		rotate(ReasonStale)
		return st, acts
	case st.Task != "" && idle > th.WaitingArchive:
		task := st.Task
		push("stale", fmt.Sprintf("%s idle %s on %s: archived (worktree kept), back on standby", st.Name, round(idle), task))
		acts = append(acts, Action{Kind: Alert, Text: fmt.Sprintf("%s gave up waiting on %s after %s; its worktree is kept", st.Name, task, round(idle))})
		rotate(ReasonStale)
		return st, acts
	case st.Task != "" && idle > th.WaitingNudge && st.NudgedAt.IsZero():
		st.NudgedAt = o.Now
		push("idle_on_task", fmt.Sprintf("%s idle %s on %s", st.Name, round(idle), st.Task))
	}

	if o.TranscriptSize >= th.CompactBytes && o.TranscriptSize > st.CompactedAt+th.CompactBytes/4 {
		st.CompactedAt = o.TranscriptSize
		acts = append(acts, Action{Kind: Compact, Session: st.SessionID})
	}
	return st, acts
}

// restart handles "no live session": resume the last one, or after repeated
// failures start fresh and say so.
func restart(th config.Thresholds, st State, o Obs, acts *[]Action, push func(string, string), alertOnce func(string, string)) State {
	var recent []time.Time
	for _, t := range st.Restarts {
		if o.Now.Sub(t) < th.RestartWindow {
			recent = append(recent, t)
		}
	}
	if len(recent)+1 >= th.RestartFailures {
		*acts = append(*acts, Action{Kind: Rotate, Session: st.SessionID, Task: st.Task, Reason: ReasonCrashed})
		push("crashed", fmt.Sprintf("%s crash-looped; its session was archived and it starts fresh", st.Name))
		alertOnce("crashloop", fmt.Sprintf("%s failed to start %d times in %s; started fresh", st.Name, len(recent)+1, th.RestartWindow))
		st.Restarts, st.Task, st.Done, st.Phase = nil, "", false, Standby
		return st
	}
	st.Restarts = append(recent, o.Now)
	*acts = append(*acts, Action{Kind: Restart, Session: st.SessionID})
	return st
}

// observeQuota folds the status line's windows into the agent's last known
// quota, ages windows out at their reset, and sets the quota flags.
func observeQuota(th config.Thresholds, st State, o Obs) State {
	if s := o.Status; s != nil {
		if w, ok := s.FiveHour(); ok {
			st.Quota.FiveHour, st.Quota.FiveHourResets, st.Quota.At = w.Used, w.Resets, o.Now
		}
		if w, ok := s.SevenDay(); ok {
			st.Quota.SevenDay, st.Quota.SevenDayResets, st.Quota.At = w.Used, w.Resets, o.Now
		}
		if s.ContextWindow.UsedPercentage != nil {
			st.ContextPct = s.ContextPct()
		}
		st.CostUSD = s.Cost.TotalCostUSD
	}
	if !st.Quota.FiveHourResets.IsZero() && !o.Now.Before(st.Quota.FiveHourResets) {
		st.Quota.FiveHour, st.Quota.FiveHourResets = 0, time.Time{}
	}
	if !st.Quota.SevenDayResets.IsZero() && !o.Now.Before(st.Quota.SevenDayResets) {
		st.Quota.SevenDay, st.Quota.SevenDayResets = 0, time.Time{}
	}
	st.Low = st.Quota.FiveHour >= th.Low5h
	st.Reserved = st.Quota.SevenDay >= th.Reserved7d
	return st
}

func round(d time.Duration) string {
	if d >= time.Hour {
		return d.Round(time.Hour).String()
	}
	return d.Round(time.Minute).String()
}
