// Package reconcile decides what to do about one agent, once per tick.
//
// Decide is pure: it is handed the agent's remembered State and what was just
// observed, and returns the new State and the actions to take. fleetd runs
// the actions. Keeping the decision free of I/O is what lets every row of the
// lifecycle table (docs/AGENT-FLEET.md) be tested without a container.
package reconcile

import (
	"fmt"
	"strings"
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
	// CodeMissing: agent-run found the shared code directory empty and exits
	// so Docker restarts the container onto the real mount.
	CodeMissing Phase = "code_missing"
	// Unreachable is shown, never stored: fleetd has not been able to ask the
	// agent's host about it for unreachableAfter (State.OutOfReach). Its
	// phase is kept as it was, for when the host is back.
	Unreachable Phase = "unreachable"
)

// State is what fleetd remembers about one agent between ticks.
type State struct {
	Name      string `json:"name"`
	Phase     Phase  `json:"phase"`
	SessionID string `json:"session_id"`
	Task      string `json:"task,omitempty"`
	// Done is set by `fleetctl done`, at DoneAt; the next idle tick rotates.
	Done   bool      `json:"done,omitempty"`
	DoneAt time.Time `json:"done_at,omitzero"`
	// ReportNudged: the agent, silent on its task with a word owed, was asked
	// to report; cleared once it writes back.
	ReportNudged bool `json:"report_nudged,omitempty"`
	// HookIdle: the session's hooks say its last turn ended (Stop) and no
	// tool has run since. Claude Code calls a session busy while a background
	// command it started runs, turn or no turn; the hooks tell the two apart.
	HookIdle bool `json:"hook_idle,omitempty"`

	LastBusy     time.Time   `json:"last_busy"`
	Restarts     []time.Time `json:"restarts,omitempty"`
	NudgedAt     time.Time   `json:"nudged_at,omitempty"`
	CompactedAt  int64       `json:"compacted_at,omitempty"` // transcript size when /compact was typed
	CoolingUntil time.Time   `json:"cooling_until,omitempty"`
	// WaitingSince is when the session started waiting on something only a
	// person at its terminal can answer (a dialog).
	WaitingSince time.Time `json:"waiting_since,omitzero"`
	// PaneOrphanSince is when the pane was first seen alive with no session
	// registered for it: starting, or wedged.
	PaneOrphanSince time.Time `json:"pane_orphan_since,omitzero"`
	DisabledReason  string    `json:"disabled_reason,omitempty"`

	// Proc is the claude process last seen running the agent's session
	// (Obs.Proc). TurnOpen: that session began a turn (UserPromptSubmit, or
	// seen busy) and no Stop has ended it; TurnProc is the process the turn
	// ran in. When the session is next seen idle in another process, the
	// turn died with the old one — a power cut, a crash, a container restart
	// — and fleetd tells the session to carry on with its task.
	Proc     string `json:"proc,omitempty"`
	TurnOpen bool   `json:"turn_open,omitempty"`
	TurnProc string `json:"turn_proc,omitempty"`
	// StartTried is when fleetd last started the container Docker had left
	// stopped, and StartFailing since when it has been failing to start.
	StartTried   time.Time `json:"start_tried,omitzero"`
	StartFailing time.Time `json:"start_failing,omitzero"`
	// CodeMissingSince is when the agent was first seen without its code
	// share, in the current episode.
	CodeMissingSince time.Time `json:"code_missing_since,omitzero"`
	// UnreachableSince is when fleetd first failed to ask the agent's host
	// about its container, in the current episode.
	UnreachableSince time.Time `json:"unreachable_since,omitzero"`

	// Low and Reserved are quota flags, independent of the phase: a low agent
	// still works, but new tasks should prefer others.
	Low      bool  `json:"low,omitempty"`
	Reserved bool  `json:"reserved,omitempty"`
	Quota    Quota `json:"quota"`

	// Model is the last status line's model, and RecordedAt the tick that
	// wrote this state: the console shows both next to what it saw live.
	Model      string    `json:"model,omitempty"`
	RecordedAt time.Time `json:"recorded_at,omitzero"`
	// ModelID is the model's id as the status line gave it (Model is its
	// display name), for telling whether a session runs the model wanted.
	ModelID string `json:"model_id,omitempty"`
	// ModelAsked is the model fleetd last typed /model for, ModelAskedKey the
	// session and process it was typed into, and ModelAskedAt when: one ask
	// per change, and a fresh session or process is asked again.
	ModelAsked    string    `json:"model_asked,omitempty"`
	ModelAskedKey string    `json:"model_asked_key,omitempty"`
	ModelAskedAt  time.Time `json:"model_asked_at,omitzero"`

	ContextPct float64 `json:"context_pct,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Alerted keys alerts already sent, so each fires once per episode.
	Alerted map[string]bool `json:"alerted,omitempty"`
}

// OutOfReach reports whether the agent's host has been out of fleetd's reach
// for unreachableAfter: it shows as unreachable, and takes no new work.
func (s State) OutOfReach(now time.Time) bool {
	return !s.UnreachableSince.IsZero() && now.Sub(s.UnreachableSince) >= unreachableAfter
}

// Recovering reports whether the agent is out of work only while fleetd
// brings it back: its container failed to start and fleetd is starting it,
// or it came up before its code share (a boot's race). Neither has gone on
// long enough to be news.
func (s State) Recovering(now time.Time) bool {
	switch s.Phase {
	case Down:
		return !s.StartFailing.IsZero() && now.Sub(s.StartFailing) < startAlertAfter
	case CodeMissing:
		return !s.CodeMissingSince.IsZero() && now.Sub(s.CodeMissingSince) < codeMissingAfter
	}
	return false
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
	// ContainerFailed is Docker's error when the container is stopped because
	// Docker could not start it, which its restart policy never retries.
	ContainerFailed string
	PaneAlive       bool
	// PanePID is the pid of the claude the pane runs, when known; it tells the
	// agent's own session from a stray using the same name.
	PanePID int
	// Proc names the claude process the pane runs: the host's boot id and
	// the pane's pid, so a process after a reboot is never taken for the one
	// before it.
	Proc string
	// Screen is the pane's visible text (its last lines), for dialogs that
	// block a session before it has registered one.
	Screen string
	// CurrentSession is the session agent-run last started (/work/.fleet/current).
	CurrentSession  string
	Sessions        []observe.Agent // interactive sessions in the container
	Status          *observe.Status // latest status-line snapshot
	Events          []observe.Event // hook events since the last tick
	TranscriptSize  int64
	TranscriptMTime time.Time
	// Owes: the agent owes its open task the next word (the ledger's last
	// message on the task was to it), so it was still working on it.
	Owes bool
	// Asking: the agent has a question out under its task that nobody has
	// answered (it is blocked in fleetctl ask, or waits for an answer): it is
	// not idle, only waiting on someone else, and is not nudged to report or
	// archived for it. AnsweredAt is when its latest question was answered:
	// the idle clock counts from then.
	Asking     bool
	AnsweredAt time.Time
	// Unreachable is the agent's host when it could not be asked about the
	// container: nothing else in the observation is known. (Why is in
	// fleetd's fleet.unreachable event.)
	Unreachable string
	// Outbox is what fleetctl tell queued in the container while fleetd did
	// not answer, oldest first. fleetd delivers it; it decides nothing.
	Outbox []Outboxed
}

// Outboxed is one message an agent queued for fleetd (fleetctl tell).
type Outboxed struct {
	File string    `json:"-"`
	From string    `json:"from"`
	To   string    `json:"to"`
	Text string    `json:"text"`
	Kind string    `json:"kind,omitempty"`
	At   time.Time `json:"at"`
}

// Kind is what an action does.
type Kind string

const (
	Start        Kind = "start"         // start the container Docker left stopped
	Restart      Kind = "restart"       // restart the pane on Session (resume)
	Rotate       Kind = "rotate"        // archive Session, start a fresh one
	ArchiveStray Kind = "archive_stray" // archive and stop a session that is not the agent's
	Compact      Kind = "compact"       // type /compact into the pane
	Prompt       Kind = "prompt"        // type Text into the pane
	Push         Kind = "push"          // tell the orchestrator (webhook)
	Alert        Kind = "alert"         // tell you (phone)
)

// paneGrace is how long a live pane may run without a registered session
// before it is treated as wedged and restarted.
const paneGrace = 2 * time.Minute

// dialogs are the screens Claude Code stops on until a person answers, by a
// line that is on screen only while they are open.
var dialogs = []struct{ marker, name string }{
	{"By proceeding, you accept all responsibility for actions taken while running in Bypass Permissions mode", "the Bypass Permissions warning"},
}

// Dialog names the blocking dialog a pane's text shows, or "".
func Dialog(screen string) string {
	flat := strings.Join(strings.Fields(screen), " ") // a narrow pane wraps lines
	for _, d := range dialogs {
		if strings.Contains(flat, d.marker) {
			return d.name
		}
	}
	return ""
}

// stuckAfter is how long a session may wait on a dialog before you are told.
const stuckAfter = 5 * time.Minute

// startEvery is how often fleetd starts a container Docker failed to start,
// and startAlertAfter how long that may go on before you are told.
const (
	startEvery      = time.Minute
	startAlertAfter = 10 * time.Minute
)

// dockerCause is what runc found wrong in Docker's start error, without the
// layers ("failed to create shim task: OCI runtime create failed: …") that
// passed it up.
func dockerCause(err string) string {
	if _, cause, ok := strings.Cut(err, "error during container init: "); ok {
		return cause
	}
	return err
}

// reportNudge is how long an agent may sit idle, its turn over, owing its task
// a word, before it is asked to report: its report failed to send (fleetd was
// away), or it stopped without one.
const reportNudge = 10 * time.Minute

// reportText is what such an agent is asked.
func reportText(task string) string {
	return fmt.Sprintf("Task %s: whoever gave it to you has heard nothing from you since their last message. "+
		"If you have finished, send your report now — and if a report failed to send earlier, send it again. "+
		"If you are blocked, or waiting on something, report that instead. Do not start the task over.", task)
}

// doneGrace is how long a session may stay busy after its task is done
// before it is rotated anyway: the task is closed, and the agent is out of the
// pool until it has its fresh session.
const doneGrace = 3 * time.Minute

// codeMissingAfter is how long an agent may go without its code share before
// you are told: at boot it is a race, over within a restart or two.
const codeMissingAfter = 3 * time.Minute

// unreachableAfter is how long an agent's host may be out of fleetd's reach
// before the agent shows as unreachable and you are told: a boot, a network
// blip, a Docker restart pass well within it.
const unreachableAfter = 5 * time.Minute

// turnSettle is how long a session may sit idle, in the process that ran
// it, on a turn no Stop ended before the turn is taken as over: a person
// pressed Escape (Claude Code runs no Stop hook for that).
const turnSettle = 2 * time.Minute

// resumeText is what a session whose turn was cut short is told.
func resumeText(task string) string {
	return fmt.Sprintf("Your session was restarted while you were working on task %s "+
		"(a power cut, a crash or a container restart). Whatever it was running was killed: "+
		"the turn you were in, and any background command you were waiting on. Check where "+
		"things stand first: git status in your worktree, and what your last steps left behind. "+
		"Then carry on with the task from there. Do not start it over. Run anything long as a "+
		"background command of your session, so you are woken when it ends — never detached with "+
		"nohup, setsid or &.", task)
}

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

	// A host back from out of reach has most likely rebooted, and its
	// containers race its code share anew: how long one has failed to start,
	// or run without its code, counts from now, not from before the outage.
	if o.Unreachable == "" && st.OutOfReach(o.Now) {
		st.StartTried, st.StartFailing, st.CodeMissingSince = time.Time{}, time.Time{}, time.Time{}
	}

	st = observeQuota(th, st, o)
	if st.Reserved {
		alertOnce("reserved", fmt.Sprintf("%s: 7-day window at %.0f%% — small tasks only until it resets", st.Name, st.Quota.SevenDay))
	} else {
		delete(st.Alerted, "reserved")
	}

	// agent-run holds off starting claude while HOME is unwritable (an image
	// built with another home than fleet.yaml's); it logs this on each retry.
	for _, e := range o.Events {
		if e.Event == "agent-run.home_unwritable" && !st.Alerted["home"] {
			push("home_unwritable", st.Name+": HOME is not writable in its container")
			alertOnce("home", fmt.Sprintf("%s: HOME is not writable in its container, so it cannot start — the image's home does not match fleet.yaml; rebuild with fleetctl image build", st.Name))
		}
	}

	// agent-run exits, rather than start claude, when the shared code
	// directory is empty (a bind of the NFS automount stub before the share
	// was mounted); Docker's restart policy re-binds it. Until a session is
	// up again the agent shows as code_missing, not waiting or down.
	// At boot that is a race agent-run usually wins on its next restart, so it
	// is news only once it has gone on for codeMissingAfter.
	for _, e := range o.Events {
		if e.Event == "agent-run.code_missing" {
			if st.CodeMissingSince.IsZero() {
				st.CodeMissingSince = o.Now
			}
			st.Phase = CodeMissing
		}
	}
	if st.Phase == CodeMissing && o.Unreachable == "" && !st.CodeMissingSince.IsZero() && o.Now.Sub(st.CodeMissingSince) >= codeMissingAfter && !st.Alerted["code"] {
		push("code_missing", st.Name+": the shared code directory is empty in its container; it is restarting until the mount is there")
		alertOnce("code", fmt.Sprintf("%s: the shared code directory is empty in its container (NFS not mounted?) — it restarts until the share is there", st.Name))
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
			// Only the agent's own session speaks for its subscription: a
			// claude it runs in a tool call has no login there, and fails.
			if st.SessionID != "" && e.Session != "" && e.Session != st.SessionID {
				continue
			}
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

	if o.Unreachable != "" {
		// Not down: unknown. At boot fleetd starts before the network, and a
		// host that cannot be asked has said nothing about its containers.
		if st.UnreachableSince.IsZero() {
			st.UnreachableSince = o.Now
		}
		if st.OutOfReach(o.Now) && !st.Alerted["unreachable"] {
			push("unreachable", fmt.Sprintf("%s: its host has been out of reach since %s", st.Name, st.UnreachableSince.Format(time.Kitchen)))
			alertOnce("unreachable", fmt.Sprintf("fleetd cannot reach %s — its agents are not seen until it is back", o.Unreachable))
		}
		return st, acts
	}
	st.UnreachableSince = time.Time{}
	delete(st.Alerted, "unreachable")

	if !o.ContainerUp {
		if o.ContainerFailed != "" {
			// Docker could not start the container (after a power cut: a
			// bind of the code share before the share was mounted), and its
			// restart policy never retries that. Start it again, every
			// startEvery, until it runs. A container someone stopped carries
			// no error, and one `fleetctl down` removed is not there at all.
			if st.StartFailing.IsZero() {
				st.StartFailing = o.Now
			}
			if o.Now.Sub(st.StartTried) >= startEvery {
				st.StartTried = o.Now
				acts = append(acts, Action{Kind: Start, Reason: o.ContainerFailed})
			}
			if o.Now.Sub(st.StartFailing) >= startAlertAfter {
				alertOnce("start", fmt.Sprintf("%s's container will not start: %s", st.Name, dockerCause(o.ContainerFailed)))
			}
		}
		if st.Phase == CodeMissing {
			return st, acts // between restarts: still waiting on the mount
		}
		if st.Phase != Down && o.ContainerFailed == "" { // one fleetd is starting is not news yet (startAlertAfter)
			push("down", st.Name+"'s container is not running")
		}
		st.Phase = Down
		return st, acts
	}
	st.StartTried, st.StartFailing = time.Time{}, time.Time{}
	delete(st.Alerted, "start")

	// Which session is the agent's: the one the pane runs, else the one named
	// for the agent. Every other session in the container is a stray.
	var mine *observe.Agent
	for i, s := range o.Sessions {
		if s.Name == st.Name && (o.PanePID == 0 || s.PID == o.PanePID) {
			mine = &o.Sessions[i]
			break
		}
	}
	// Failing that, the session the agent's own records name — never archive
	// the agent's own session as a stray because a pid did not line up.
	own := func(sid string) bool { return sid != "" && (sid == o.CurrentSession || sid == st.SessionID) }
	if mine == nil {
		for i, s := range o.Sessions {
			if own(s.SessionID) {
				mine = &o.Sessions[i]
				break
			}
		}
	}
	for _, s := range o.Sessions {
		if (mine != nil && s.SessionID == mine.SessionID) || own(s.SessionID) || s.Activity() != "idle" {
			continue
		}
		acts = append(acts, Action{Kind: ArchiveStray, Session: s.SessionID, PID: s.PID, Reason: ReasonStray})
	}

	// The agent's turns, from its hooks. They are read here, before any
	// early return, because they outlive a power cut on the agent's volume:
	// a Stop written just before one must close the turn, not leave it open
	// to be taken for cut short. A turn begun since the last tick ran in the
	// process seen then.
	for _, e := range o.Events {
		if e.Session == "" || !(own(e.Session) || (mine != nil && e.Session == mine.SessionID)) {
			continue
		}
		switch e.Event {
		case "UserPromptSubmit":
			st.TurnOpen, st.TurnProc = true, firstOf(st.Proc, o.Proc)
			st.HookIdle = false
		case "PreToolUse", "PostToolUse", "PostToolUseFailure":
			st.HookIdle = false
		case "Stop", "StopFailure":
			st.TurnOpen = false
			st.HookIdle = true
		}
	}

	if d := Dialog(o.Screen); mine == nil && o.PaneAlive && d != "" {
		// A dialog is open and no session has registered behind it: it hears
		// nothing, and restarting the pane would only open it again. Tell
		// the operator, once, with the way in.
		st.PaneOrphanSince = time.Time{}
		if st.WaitingSince.IsZero() {
			st.WaitingSince = o.Now
		}
		if o.Now.Sub(st.WaitingSince) >= stuckAfter && !st.Alerted["stuck"] {
			push("stuck", fmt.Sprintf("%s is waiting on %q and hears no messages", st.Name, d))
			alertOnce("stuck", fmt.Sprintf("%s is stuck on %q — fleetctl attach %s", st.Name, d, st.Name))
		}
		return st, acts
	}
	if mine == nil && o.PaneAlive {
		// A live pane with no session yet is usually claude starting up.
		if st.PaneOrphanSince.IsZero() {
			st.PaneOrphanSince = o.Now
		}
		if o.Now.Sub(st.PaneOrphanSince) < paneGrace {
			return st, acts
		}
	}
	if mine == nil || !o.PaneAlive {
		st.PaneOrphanSince = time.Time{}
		st = restart(th, st, o, &acts, push, alertOnce)
		return st, acts
	}
	st.PaneOrphanSince = time.Time{}
	st.SessionID = mine.SessionID
	// A new process: the old one died (a power cut, a crash, a container
	// restart) and the session was resumed in this one.
	moved := st.Proc != "" && o.Proc != "" && st.Proc != o.Proc
	st.Proc = o.Proc
	if st.Phase == Down || st.Phase == CodeMissing {
		st.Phase = Standby
	}
	delete(st.Alerted, "code") // a session is up: the code is mounted
	st.CodeMissingSince = time.Time{}
	delete(st.Alerted, "crashloop")
	delete(st.Alerted, "home") // a session is up: HOME is writable now
	// A dialog nobody answers leaves the session deaf to messages. Say so,
	// once per episode, after a few minutes: it may be a passing prompt.
	if mine.Activity() == "waiting" && mine.WaitingFor != "" {
		if st.WaitingSince.IsZero() {
			st.WaitingSince = o.Now
		}
		if o.Now.Sub(st.WaitingSince) >= stuckAfter && !st.Alerted["stuck"] {
			push("stuck", fmt.Sprintf("%s is waiting on %q and hears no messages", st.Name, mine.WaitingFor))
			alertOnce("stuck", fmt.Sprintf("%s is stuck on %q — fleetctl attach %s", st.Name, mine.WaitingFor, st.Name))
		}
	} else {
		st.WaitingSince = time.Time{}
		delete(st.Alerted, "stuck")
	}
	busy := mine.Activity() == "busy"
	if !o.Owes {
		st.ReportNudged = false // it wrote back: a later instruction may be nudged again
	}
	if busy || (st.LastBusy.IsZero() && o.TranscriptMTime.IsZero()) {
		st.LastBusy = o.Now // busy now, or never seen: the idle clock starts here
	}
	idleSince := st.LastBusy
	if o.TranscriptMTime.After(idleSince) {
		idleSince = o.TranscriptMTime
	}
	if o.AnsweredAt.After(idleSince) && !o.AnsweredAt.After(o.Now) {
		idleSince = o.AnsweredAt // what it waited for an answer does not count as idleness
	}
	idle := o.Now.Sub(idleSince)

	if st.Phase == Cooling {
		if o.Now.Before(st.CoolingUntil) {
			return st, acts // cooling is never stale, and never archived for it
		}
		st.CoolingUntil = time.Time{}
		if st.Task != "" && !busy {
			acts = append(acts, Action{Kind: Prompt, Text: "Your rate-limit window has reset. Continue the task you were working on."})
			st.TurnOpen = false // that is its carry-on
		}
	}

	if busy {
		st.TurnOpen, st.TurnProc = true, o.Proc
	} else {
		// The session is back, idle, in another process than its turn's: the
		// turn was cut short. Or it is back in a new process with its task
		// still owed a word: between turns, it was waiting on a background
		// command of its own, which died with the old process. Either way it
		// is told to carry on, and the outage does not count as idleness.
		// One that had reported and waits on its orchestrator is left be.
		cut := st.TurnOpen && st.TurnProc != "" && o.Proc != "" && st.TurnProc != o.Proc
		if (cut || (moved && o.Owes)) && st.Task != "" && !st.Done {
			acts = append(acts, Action{Kind: Prompt, Text: resumeText(st.Task)})
			push("resumed", fmt.Sprintf("%s was restarted while working on %s and told to carry on", st.Name, st.Task))
			st.LastBusy, idle = o.Now, 0
		}
		if cut || (st.TurnOpen && idle > turnSettle) {
			st.TurnOpen = false // over; with no Stop when a person pressed Escape
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
	// A task that is done rotates once its turn is over, though a background
	// command it left keeps the session busy (one that never ends would keep
	// the agent out of the pool for good) — and after doneGrace, regardless.
	doneNow := st.Done && (st.HookIdle || (!st.DoneAt.IsZero() && o.Now.Sub(st.DoneAt) >= doneGrace))
	if busy && !doneNow {
		st.NudgedAt = time.Time{}
		return st, acts
	}

	// Everything below acts on an idle session only (or a done one, above).
	rotate := func(reason string) {
		acts = append(acts, Action{Kind: Rotate, Session: st.SessionID, Task: st.Task, Reason: reason})
		st.Task, st.Done, st.Phase, st.SessionID = "", false, Standby, "" // the next one is learned when seen
		st.TurnOpen, st.HookIdle, st.DoneAt = false, false, time.Time{}
		st.NudgedAt, st.CompactedAt, st.LastBusy = time.Time{}, 0, o.Now
		st.Model, st.ModelID = "", "" // the last session's, not the fresh one's
	}
	switch {
	case st.Done:
		rotate(ReasonTaskDone)
		return st, acts
	case st.Task == "" && idle > th.StandbyIdle && o.TranscriptSize >= th.StandbyMinTranscript:
		rotate(ReasonStale)
		return st, acts
	case st.Task != "" && o.Asking:
		// Waiting on an answer is someone else's to give (fleetd escalates it):
		// not idle on the task, so neither nudged nor given up on.
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

	if o.Owes && !o.Asking && st.Task != "" && !st.ReportNudged && idle >= reportNudge {
		st.ReportNudged = true
		acts = append(acts, Action{Kind: Prompt, Text: reportText(st.Task)})
		push("report_nudged", fmt.Sprintf("%s was silent %s on %s with a report owed, and was asked for it", st.Name, round(idle), st.Task))
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
		st.Restarts, st.Task, st.Done, st.Phase, st.SessionID = nil, "", false, Standby, ""
		st.TurnOpen = false
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

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func round(d time.Duration) string {
	if d >= time.Hour {
		return d.Round(time.Hour).String()
	}
	return d.Round(time.Minute).String()
}
