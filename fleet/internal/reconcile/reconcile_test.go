package reconcile

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/config"
	"github.com/MelloB1989/karmax/fleet/internal/observe"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func th() config.Thresholds {
	c, err := config.Parse([]byte("home: /h\nhosts: {kali: {exec: [docker]}}\norchestrator: {host: kali, token_env: O}\nagents: {agent-03: {host: kali, token_env: T}}"))
	if err != nil {
		panic(err)
	}
	return c.Thresholds
}

func live(status string) []observe.Agent {
	return []observe.Agent{{PID: 7, Kind: "interactive", Name: "agent-03", SessionID: "s1", Status: status}}
}

func obs(status string) Obs {
	return Obs{Now: t0, ContainerUp: true, PaneAlive: true, Sessions: live(status), TranscriptMTime: t0}
}

func kinds(acts []Action) []Kind {
	out := make([]Kind, len(acts))
	for i, a := range acts {
		out[i] = a.Kind
	}
	return out
}

func has(acts []Action, k Kind) *Action {
	for i := range acts {
		if acts[i].Kind == k {
			return &acts[i]
		}
	}
	return nil
}

func TestHealthyIdleStandbyDoesNothing(t *testing.T) {
	st, acts := Decide(th(), State{Name: "agent-03"}, obs("idle"))
	if len(acts) != 0 || st.Phase != Standby || st.SessionID != "s1" {
		t.Fatalf("phase=%s acts=%v", st.Phase, kinds(acts))
	}
}

func TestBusyIsWorkingAndIdleWithATaskIsWaiting(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, obs("busy"))
	if st.Phase != Working || !st.LastBusy.Equal(t0) {
		t.Fatalf("busy: %s", st.Phase)
	}
	o := obs("idle")
	o.Now = t0.Add(time.Minute)
	st, _ = Decide(th(), st, o)
	if st.Phase != Waiting {
		t.Fatalf("idle with a task: %s", st.Phase)
	}
}

// No live session: the pane is restarted on the last session, so the
// conversation continues.
func TestNoLiveSessionRestartsOnTheLastSession(t *testing.T) {
	o := obs("")
	o.Sessions = nil
	o.PaneAlive = false
	st, acts := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	a := has(acts, Restart)
	if a == nil || a.Session != "s1" {
		t.Fatalf("acts = %v", acts)
	}
	if len(st.Restarts) != 1 {
		t.Errorf("restarts recorded = %d", len(st.Restarts))
	}
}

// Three failed restarts in the window: archive, start fresh, alert you.
func TestACrashLoopStartsFreshAndAlerts(t *testing.T) {
	o := obs("")
	o.Sessions, o.PaneAlive = nil, false
	st := State{Name: "agent-03", SessionID: "s1",
		Restarts: []time.Time{t0.Add(-8 * time.Minute), t0.Add(-4 * time.Minute)}}
	st, acts := Decide(th(), st, o)
	if has(acts, Rotate) == nil || has(acts, Alert) == nil || has(acts, Restart) != nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
	if r := has(acts, Rotate); r.Reason != ReasonCrashed || r.Session != "s1" {
		t.Errorf("rotate = %+v", r)
	}
	if len(st.Restarts) != 0 {
		t.Error("a fresh start clears the failure count")
	}
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.crashed" {
		t.Errorf("orchestrator not told: %v", kinds(acts))
	}
}

func TestOldRestartsFallOutOfTheWindow(t *testing.T) {
	o := obs("")
	o.Sessions, o.PaneAlive = nil, false
	st := State{Name: "agent-03", SessionID: "s1",
		Restarts: []time.Time{t0.Add(-time.Hour), t0.Add(-50 * time.Minute)}}
	_, acts := Decide(th(), st, o)
	if has(acts, Restart) == nil || has(acts, Rotate) != nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

// A second session in the container, or one not named for the agent, is
// archived — but only once idle.
func TestStraySessionsAreArchived(t *testing.T) {
	o := obs("idle")
	o.Sessions = append(o.Sessions,
		observe.Agent{PID: 9, Kind: "interactive", Name: "helper", SessionID: "s9", Status: "idle"},
		observe.Agent{PID: 10, Kind: "interactive", Name: "busy-one", SessionID: "s10", Status: "busy"})
	_, acts := Decide(th(), State{Name: "agent-03"}, o)
	var archived []string
	for _, a := range acts {
		if a.Kind == ArchiveStray {
			archived = append(archived, a.Session)
		}
	}
	if !slices.Equal(archived, []string{"s9"}) {
		t.Fatalf("archived %v, want only the idle stray", archived)
	}
}

// A second session even under the agent's own name is a stray: one agent, one
// session. The one the pane runs is kept.
func TestADuplicateNamedSessionKeepsThePanes(t *testing.T) {
	o := obs("idle")
	o.PanePID = 7
	o.Sessions = append(o.Sessions, observe.Agent{PID: 8, Kind: "interactive", Name: "agent-03", SessionID: "s8", Status: "idle"})
	st, acts := Decide(th(), State{Name: "agent-03"}, o)
	if a := has(acts, ArchiveStray); a == nil || a.Session != "s8" || st.SessionID != "s1" {
		t.Fatalf("acts = %v, current = %s", acts, st.SessionID)
	}
}

// One task, one session.
func TestADoneTaskRotatesOnceIdle(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", Done: true, SessionID: "s1"}
	st2, acts := Decide(th(), st, obs("busy"))
	if has(acts, Rotate) != nil {
		t.Fatal("rotated a busy session")
	}
	st2, acts = Decide(th(), st2, obs("idle"))
	r := has(acts, Rotate)
	if r == nil || r.Reason != ReasonTaskDone || r.Task != "T1" {
		t.Fatalf("acts = %v", acts)
	}
	if st2.Task != "" || st2.Done || st2.Phase != Standby {
		t.Errorf("state after rotate = %+v", st2)
	}
}

func TestAStaleStandbyRotatesOnlyWithANonTrivialTranscript(t *testing.T) {
	o := obs("idle")
	o.Now = t0.Add(13 * time.Hour)
	o.TranscriptSize = 1 << 10
	_, acts := Decide(th(), State{Name: "agent-03", LastBusy: t0}, o)
	if has(acts, Rotate) != nil {
		t.Fatal("rotated a standby session with a trivial transcript")
	}
	o.TranscriptSize = 1 << 20
	_, acts = Decide(th(), State{Name: "agent-03", LastBusy: t0}, o)
	if r := has(acts, Rotate); r == nil || r.Reason != ReasonStale {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

func TestWaitingTooLongNudgesOnceThenArchives(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", LastBusy: t0}
	o := obs("idle")
	o.Now = t0.Add(3 * time.Hour)
	st, acts := Decide(th(), st, o)
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.idle_on_task" {
		t.Fatalf("acts = %v", kinds(acts))
	}
	o.Now = t0.Add(4 * time.Hour)
	st, acts = Decide(th(), st, o)
	if has(acts, Push) != nil {
		t.Error("nudged twice for the same idle stretch")
	}
	o.Now = t0.Add(25 * time.Hour)
	_, acts = Decide(th(), st, o)
	r := has(acts, Rotate)
	if r == nil || r.Reason != ReasonStale || has(acts, Alert) == nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.stale" {
		t.Errorf("orchestrator not told it went stale")
	}
}

func TestABigIdleTranscriptIsCompacted(t *testing.T) {
	o := obs("idle")
	o.TranscriptSize = 9 << 20
	st, acts := Decide(th(), State{Name: "agent-03", Task: "T1", LastBusy: t0}, o)
	if has(acts, Compact) == nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
	// Once per transcript size: not again until it grows past the mark again.
	_, acts = Decide(th(), st, o)
	if has(acts, Compact) != nil {
		t.Error("compacted twice")
	}
}

// status is a status-line snapshot with both windows; resets is the 5-hour
// window's reset.
func status(five, seven float64, resets time.Time) *observe.Status {
	s, err := observe.ParseStatus([]byte(`{"session_id":"s1","rate_limits":{"five_hour":{"used_percentage":` +
		jsonNum(five) + `,"resets_at":` + jsonNum(float64(resets.Unix())) +
		`},"seven_day":{"used_percentage":` + jsonNum(seven) + `}}}`))
	if err != nil {
		panic(err)
	}
	return s
}

func jsonNum(v float64) string { b, _ := json.Marshal(v); return string(b) }

func TestQuotaFlags(t *testing.T) {
	o := obs("idle")
	o.Status = status(85, 10, t0.Add(time.Hour))
	st, acts := Decide(th(), State{Name: "agent-03"}, o)
	if !st.Low || st.Reserved || has(acts, Alert) != nil {
		t.Fatalf("85%% 5h: low=%v reserved=%v acts=%v", st.Low, st.Reserved, kinds(acts))
	}
	o.Status = status(10, 92, t0.Add(time.Hour))
	st, acts = Decide(th(), st, o)
	if st.Low || !st.Reserved || has(acts, Alert) == nil {
		t.Fatalf("92%% 7d: low=%v reserved=%v acts=%v", st.Low, st.Reserved, kinds(acts))
	}
	_, acts = Decide(th(), st, o)
	if has(acts, Alert) != nil {
		t.Error("the reserved alert fires once, not every tick")
	}
	if st.Quota.FiveHour != 10 || st.Quota.SevenDay != 92 || st.Quota.At != t0 {
		t.Errorf("quota kept = %+v", st.Quota)
	}
}

// The last known quota is kept for a fresh session that has not reported yet,
// and aged out at its reset.
func TestQuotaIsKeptUntilItsReset(t *testing.T) {
	o := obs("idle")
	o.Status = status(85, 10, t0.Add(time.Hour))
	st, _ := Decide(th(), State{Name: "agent-03"}, o)
	o.Status = &observe.Status{SessionID: "s2"} // rotated, no call yet
	o.Now = t0.Add(30 * time.Minute)
	st, _ = Decide(th(), st, o)
	if !st.Low || st.Quota.FiveHour != 85 {
		t.Fatalf("lost the last known window: %+v", st.Quota)
	}
	o.Now = t0.Add(2 * time.Hour)
	st, _ = Decide(th(), st, o)
	if st.Low || st.Quota.FiveHour != 0 {
		t.Fatalf("kept a window past its reset: %+v", st.Quota)
	}
}

func stopFailure(kind string) observe.Event {
	return observe.Event{T: t0.Unix(), Event: "StopFailure", Session: "s1", Raw: json.RawMessage(`{"error_type":"` + kind + `"}`)}
}

func TestRateLimitedCoolsUntilTheResetThenResumes(t *testing.T) {
	o := obs("idle")
	o.Status = status(100, 50, t0.Add(40*time.Minute))
	o.Events = []observe.Event{stopFailure("rate_limit")}
	st, acts := Decide(th(), State{Name: "agent-03", Task: "T1"}, o)
	if st.Phase != Cooling || !st.CoolingUntil.Equal(t0.Add(40*time.Minute)) {
		t.Fatalf("phase=%s until=%v", st.Phase, st.CoolingUntil)
	}
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.cooling" {
		t.Errorf("acts = %v", kinds(acts))
	}
	// Cooling is never stale.
	o.Events = nil
	o.Now = t0.Add(30 * time.Minute)
	st, acts = Decide(th(), st, o)
	if st.Phase != Cooling || has(acts, Rotate) != nil {
		t.Fatalf("phase=%s acts=%v", st.Phase, kinds(acts))
	}
	o.Now = t0.Add(41 * time.Minute)
	st, acts = Decide(th(), st, o)
	if st.Phase != Waiting || has(acts, Prompt) == nil {
		t.Fatalf("after reset: phase=%s acts=%v", st.Phase, kinds(acts))
	}
}

func TestRateLimitedWithoutAKnownResetCoolsForTheDefault(t *testing.T) {
	o := obs("idle")
	o.Events = []observe.Event{stopFailure("rate_limit")}
	st, _ := Decide(th(), State{Name: "agent-03"}, o)
	if !st.CoolingUntil.Equal(t0.Add(time.Hour)) {
		t.Fatalf("until = %v", st.CoolingUntil)
	}
}

func TestAuthFailureDisablesAndAlertsOnce(t *testing.T) {
	for _, kind := range []string{"authentication_failed", "billing_error", "account_on_hold"} {
		o := obs("idle")
		o.Events = []observe.Event{stopFailure(kind)}
		st, acts := Decide(th(), State{Name: "agent-03"}, o)
		if st.Phase != Disabled || st.DisabledReason != kind || has(acts, Alert) == nil {
			t.Fatalf("%s: phase=%s acts=%v", kind, st.Phase, kinds(acts))
		}
		// Disabled is left alone: no restarts fighting a dead token.
		o.Events = nil
		o.Sessions, o.PaneAlive = nil, false
		_, acts = Decide(th(), st, o)
		if len(acts) != 0 {
			t.Fatalf("%s: acted on a disabled agent: %v", kind, kinds(acts))
		}
	}
}

func TestAStoppedContainerIsReportedNotRestarted(t *testing.T) {
	o := obs("")
	o.ContainerUp = false
	o.Sessions, o.PaneAlive = nil, false
	st, acts := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	if st.Phase != Down || has(acts, Restart) != nil {
		t.Fatalf("phase=%s acts=%v", st.Phase, kinds(acts))
	}
}

// An agent fleetd has never seen busy has no idle clock yet; it starts now,
// rather than at the zero time — which would make every new task 2000 years
// stale on its first tick.
func TestTheIdleClockStartsAtFirstSight(t *testing.T) {
	o := obs("idle")
	o.TranscriptMTime = time.Time{}
	st, acts := Decide(th(), State{Name: "agent-03", Task: "T1"}, o)
	if len(acts) != 0 || st.Phase != Waiting || !st.LastBusy.Equal(t0) {
		t.Fatalf("phase=%s lastBusy=%v acts=%v", st.Phase, st.LastBusy, kinds(acts))
	}
}

// After a rotation the archived session is gone from the container; a
// restart before the new session is seen must not point back at it.
func TestARotationForgetsTheOldSession(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1", Done: true}, obs("idle"))
	if st.SessionID != "" {
		t.Fatalf("still remembers %q after rotating it away", st.SessionID)
	}
	o := obs("")
	o.Sessions, o.PaneAlive = nil, false
	_, acts := Decide(th(), st, o)
	if r := has(acts, Restart); r == nil || r.Session != "" {
		t.Fatalf("restart = %+v, want one that names no session (agent-run decides)", r)
	}
}

// A session stuck on a dialog hears no messages. Nobody is at its terminal,
// so after a few minutes you are told — once — with the way in.
func TestASessionStuckOnADialogIsReported(t *testing.T) {
	o := obs("waiting")
	o.Sessions[0].WaitingFor = "dialog open"
	st, acts := Decide(th(), State{Name: "agent-03"}, o)
	if len(acts) != 0 {
		t.Fatalf("reported at once: %v", kinds(acts))
	}
	o.Now = t0.Add(6 * time.Minute)
	st, acts = Decide(th(), st, o)
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.stuck" || has(acts, Alert) == nil ||
		!strings.Contains(has(acts, Alert).Text, "fleetctl attach agent-03") {
		t.Fatalf("acts = %v", acts)
	}
	o.Now = t0.Add(7 * time.Minute)
	if st, acts = Decide(th(), st, o); len(acts) != 0 {
		t.Fatalf("reported twice: %v", kinds(acts))
	}
	// Answered: the episode ends, and a later dialog is reported again.
	o = obs("idle")
	o.Now = t0.Add(8 * time.Minute)
	st, _ = Decide(th(), st, o)
	if !st.WaitingSince.IsZero() || st.Alerted["stuck"] {
		t.Fatalf("episode not cleared: %+v", st)
	}
}

// A pane that is alive but whose session has not registered yet is starting,
// not dead: it gets a grace period before it is restarted.
func TestALivePaneWithoutASessionGetsAGracePeriod(t *testing.T) {
	o := obs("")
	o.Sessions = nil
	st, acts := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	if len(acts) != 0 {
		t.Fatalf("restarted a starting pane: %v", kinds(acts))
	}
	o.Now = t0.Add(3 * time.Minute)
	_, acts = Decide(th(), st, o)
	if has(acts, Restart) == nil {
		t.Fatalf("a pane with no session for 3m was left alone: %v", kinds(acts))
	}
}

// The session the agent's own records name is never a stray, even when the
// pane's pid cannot be matched to it.
func TestTheAgentsOwnSessionIsNeverAStray(t *testing.T) {
	o := obs("idle")
	o.PanePID = 999 // matches nothing
	o.CurrentSession = "s1"
	o.Sessions[0].Name = "renamed"
	st, acts := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	if has(acts, ArchiveStray) != nil || has(acts, Restart) != nil || st.SessionID != "s1" {
		t.Fatalf("acts = %v, session = %s", acts, st.SessionID)
	}
}

// A claude the agent itself runs (a nested `claude -p` in a tool call) has
// its own session, and no login in the tool's environment; its failure is
// not the subscription's.
func TestAFailureInAnotherSessionDoesNotDisableTheAgent(t *testing.T) {
	o := obs("busy")
	e := stopFailure("authentication_failed")
	e.Session = "a-nested-session"
	o.Events = []observe.Event{e}
	st, _ := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	if st.Phase == Disabled {
		t.Fatalf("disabled by another session's failure")
	}
	e.Session = "s1"
	o.Events = []observe.Event{e}
	if st, _ = Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o); st.Phase != Disabled {
		t.Fatalf("own session's failure: phase=%s", st.Phase)
	}
}

const bypassDialog = `  WARNING: Claude Code running in Bypass Permissions mode

  In Bypass Permissions mode, Claude Code will not ask for your approval before running potentially dangerous commands.

  By proceeding, you accept all responsibility for actions taken while running in
  Bypass Permissions mode.

  ❯ 1. No, exit
    2. Yes, I accept
`

// The bypass-mode warning opens before any session registers. The pane is
// alive, so it must not be restarted (that opens the dialog again), and
// after a few minutes the operator is told — once — with the way in.
func TestAPaneOnTheBypassWarningIsStuckNotRestarted(t *testing.T) {
	if Dialog(bypassDialog) == "" {
		t.Fatal("the bypass warning is not recognised")
	}
	o := obs("")
	o.Sessions = nil
	o.Screen = bypassDialog
	st := State{Name: "agent-03", SessionID: "s1"}
	for _, minutes := range []int{0, 2, 4} {
		o.Now = t0.Add(time.Duration(minutes) * time.Minute)
		var acts []Action
		if st, acts = Decide(th(), st, o); len(acts) != 0 {
			t.Fatalf("at %d min: %v, want nothing yet", minutes, kinds(acts))
		}
	}
	o.Now = t0.Add(6 * time.Minute) // past paneGrace too: a plain orphan would be restarted here
	st, acts := Decide(th(), st, o)
	p, a := has(acts, Push), has(acts, Alert)
	if p == nil || p.Event != "fleet.agent.stuck" || a == nil || !strings.Contains(a.Text, "Bypass Permissions") ||
		!strings.Contains(a.Text, "fleetctl attach agent-03") || has(acts, Restart) != nil {
		t.Fatalf("acts = %v", acts)
	}
	o.Now = t0.Add(30 * time.Minute)
	if _, acts = Decide(th(), st, o); len(acts) != 0 {
		t.Fatalf("reported twice, or restarted: %v", kinds(acts))
	}
}

func TestDialogSeesThroughWrappedLinesAndIgnoresRegisteredSessions(t *testing.T) {
	wrapped := "By proceeding, you accept all responsibility for actions taken while running in\n  Bypass Permissions mode."
	if Dialog(wrapped) == "" {
		t.Error("a wrapped line was missed")
	}
	if Dialog("a perfectly normal prompt") != "" {
		t.Error("an ordinary screen reads as a dialog")
	}
	// A working session whose transcript merely mentions it is not stuck.
	o := obs("busy")
	o.Screen = bypassDialog
	st, acts := Decide(th(), State{Name: "agent-03", SessionID: "s1"}, o)
	o.Now = t0.Add(10 * time.Minute)
	if _, acts = Decide(th(), st, o); has(acts, Alert) != nil {
		t.Fatalf("a registered session raised the dialog alert: %v", acts)
	}
}

// agent-run will not start claude into the first-run wizard when HOME is
// unwritable; it says so in the event log, and you are told once.
func TestAnUnwritableHomeIsAlertedOncePerEpisode(t *testing.T) {
	o := obs("")
	o.Sessions, o.PaneAlive = nil, false
	o.Events = []observe.Event{{T: t0.Unix(), Event: "agent-run.home_unwritable"}}
	st, acts := Decide(th(), State{Name: "agent-03"}, o)
	a := has(acts, Alert)
	if a == nil || !strings.Contains(a.Text, "HOME is not writable") || !strings.Contains(a.Text, "fleetctl image build") {
		t.Fatalf("acts = %v", acts)
	}
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.home_unwritable" {
		t.Errorf("orchestrator not told: %v", kinds(acts))
	}
	o.Now = t0.Add(time.Minute)
	if _, acts = Decide(th(), st, o); has(acts, Alert) != nil {
		t.Fatalf("alerted twice: %v", kinds(acts))
	}
	// A session comes up: HOME is writable; a later episode alerts again.
	up := obs("idle")
	up.Now = t0.Add(2 * time.Minute)
	if st, _ = Decide(th(), st, up); st.Alerted["home"] {
		t.Fatal("episode not cleared once a session is up")
	}
}

// agent-run exits when the shared code directory is empty and Docker restarts
// the container: the agent shows as code_missing (not waiting or down) across
// the restarts, you are told once, and a session coming up clears it.
func TestAnEmptyCodeDirShowsAsCodeMissingAndAlertsOnce(t *testing.T) {
	o := obs("")
	o.Sessions, o.PaneAlive = nil, false
	o.Events = []observe.Event{{T: t0.Unix(), Event: "agent-run.code_missing"}}
	st, acts := Decide(th(), State{Name: "agent-03", Phase: Waiting, Task: "t1"}, o)
	if st.Phase != CodeMissing {
		t.Fatalf("phase = %s, want code_missing", st.Phase)
	}
	// At boot agent-run often starts before the share is mounted, and the
	// next restart finds it: that race is not news.
	if has(acts, Alert) != nil || has(acts, Push) != nil {
		t.Fatalf("told at once: %v", kinds(acts))
	}
	// Between restarts the container is down: still code_missing.
	o.Now, o.Events, o.ContainerUp = t0.Add(time.Minute), nil, false
	st, acts = Decide(th(), st, o)
	if st.Phase != CodeMissing || has(acts, Alert) != nil {
		t.Fatalf("phase = %s acts = %v", st.Phase, kinds(acts))
	}
	// Still missing after codeMissingAfter: the share is down. Told, once.
	o.Now = t0.Add(codeMissingAfter)
	st, acts = Decide(th(), st, o)
	if a := has(acts, Alert); a == nil || !strings.Contains(a.Text, "code directory") {
		t.Fatalf("acts = %v", acts)
	}
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.code_missing" {
		t.Errorf("orchestrator not told: %v", kinds(acts))
	}
	o.Events, o.ContainerUp = []observe.Event{{T: t0.Unix(), Event: "agent-run.code_missing"}}, true
	o.Now = t0.Add(codeMissingAfter + time.Minute)
	if st, acts = Decide(th(), st, o); has(acts, Alert) != nil || has(acts, Push) != nil {
		t.Fatalf("told twice: %v", kinds(acts))
	}
	up := obs("idle")
	up.Now = t0.Add(codeMissingAfter + 2*time.Minute)
	if st, _ = Decide(th(), st, up); st.Phase == CodeMissing || st.Alerted["code"] || !st.CodeMissingSince.IsZero() {
		t.Fatalf("not cleared once a session is up: %s %v", st.Phase, st.Alerted)
	}
}

// After a power cut Docker can fail to start a container (its bind of the
// code share, before the share is mounted) and never retries. fleetd starts
// it, once a minute, and tells you only if that keeps failing.
func TestAContainerDockerFailedToStartIsStartedAgain(t *testing.T) {
	const why = `error mounting "/home/op/code": no such device`
	o := Obs{Now: t0, ContainerFailed: why}
	st, acts := Decide(th(), State{Name: "agent-03", Phase: Working, Task: "T1"}, o)
	if a := has(acts, Start); a == nil || a.Reason != why || st.Phase != Down {
		t.Fatalf("phase=%s acts=%v", st.Phase, acts)
	}
	if has(acts, Push) != nil {
		t.Fatalf("pushed down while fleetd starts it: %v", kinds(acts)) // the alert below is the news
	}
	o.Now = t0.Add(30 * time.Second)
	if st, acts = Decide(th(), st, o); has(acts, Start) != nil {
		t.Fatalf("started again within a minute: %v", kinds(acts))
	}
	o.Now = t0.Add(time.Minute)
	if st, acts = Decide(th(), st, o); has(acts, Start) == nil || has(acts, Alert) != nil {
		t.Fatalf("a minute on: %v", kinds(acts))
	}
	o.Now = t0.Add(10 * time.Minute)
	if st, acts = Decide(th(), st, o); has(acts, Alert) == nil || !strings.Contains(has(acts, Alert).Text, why) {
		t.Fatalf("ten minutes on: %v", acts)
	}
	up := obs("idle")
	up.Now = t0.Add(11 * time.Minute)
	if st, _ = Decide(th(), st, up); !st.StartTried.IsZero() || !st.StartFailing.IsZero() || st.Alerted["start"] {
		t.Fatalf("running again, still marked failing: %+v", st)
	}
	if st.Task != "T1" {
		t.Fatalf("the task was dropped: %+v", st)
	}
}

// A container failed to start, its host went out of reach and rebooted, and
// half an hour on raced its code share once more. That is a fresh start, not
// one failing for half an hour: no alert until it has failed startAlertAfter
// since the host came back. The alert gives runc's cause, not Docker's layers.
func TestAHostBackFromOutOfReachCountsAFailedStartFromNow(t *testing.T) {
	const why = `failed to create task for container: failed to create shim task: OCI runtime create failed: runc create failed: unable to start container process: error during container init: error mounting "/home/op/code" to rootfs at "/home/op/code": no such device`
	st, _ := Decide(th(), State{Name: "agent-03", Phase: Working, Task: "T1"}, Obs{Now: t0, ContainerFailed: why})
	away := Obs{Now: t0.Add(2 * time.Minute), Unreachable: "pc2: no route to host"}
	st, _ = Decide(th(), st, away)
	away.Now = t0.Add(34 * time.Minute)
	st, _ = Decide(th(), st, away)

	back := Obs{Now: t0.Add(34*time.Minute + 30*time.Second), ContainerFailed: why}
	st, acts := Decide(th(), st, back)
	if has(acts, Start) == nil || has(acts, Alert) != nil {
		t.Fatalf("back from the reboot: %v", acts)
	}
	back.Now = back.Now.Add(startAlertAfter)
	_, acts = Decide(th(), st, back)
	if a := has(acts, Alert); a == nil || a.Text != `agent-03's container will not start: error mounting "/home/op/code" to rootfs at "/home/op/code": no such device` {
		t.Fatalf("still failing: %v", acts)
	}

	// A blip shorter than unreachableAfter is no reboot: the clock runs on.
	st, _ = Decide(th(), State{Name: "agent-03", Phase: Working, Task: "T1"}, Obs{Now: t0, ContainerFailed: why})
	st, _ = Decide(th(), st, Obs{Now: t0.Add(8 * time.Minute), Unreachable: "pc2: no route to host"})
	_, acts = Decide(th(), st, Obs{Now: t0.Add(startAlertAfter), ContainerFailed: why})
	if has(acts, Alert) == nil {
		t.Fatalf("a blip restarted the clock: %v", acts)
	}
}

// An agent without its code share whose host then goes out of reach is not
// told about while it is away (nothing is known of it), and back from the
// reboot it has codeMissingAfter again to find the share.
func TestCodeMissingIsNotToldWhileTheHostIsAwayNorAtOnceWhenBack(t *testing.T) {
	missing := []observe.Event{{T: t0.Unix(), Event: "agent-run.code_missing"}}
	st, _ := Decide(th(), State{Name: "agent-03", Phase: Waiting, Task: "t1"}, Obs{Now: t0, Events: missing})
	if st.Phase != CodeMissing {
		t.Fatalf("phase = %s", st.Phase)
	}
	away := Obs{Now: t0.Add(time.Minute), Unreachable: "pc2: no route to host"}
	for _, at := range []time.Duration{time.Minute, codeMissingAfter, 30 * time.Minute} {
		away.Now = t0.Add(at)
		var acts []Action
		if st, acts = Decide(th(), st, away); has(acts, Alert) != nil && !strings.Contains(has(acts, Alert).Text, "cannot reach") {
			t.Fatalf("at %s: %v", at, acts)
		}
	}
	back := Obs{Now: t0.Add(31 * time.Minute), Events: []observe.Event{{T: t0.Add(31 * time.Minute).Unix(), Event: "agent-run.code_missing"}}}
	st, acts := Decide(th(), st, back)
	if st.Phase != CodeMissing || has(acts, Alert) != nil {
		t.Fatalf("back: phase=%s acts=%v", st.Phase, acts)
	}
	back.Now, back.Events = back.Now.Add(codeMissingAfter), nil
	if _, acts = Decide(th(), st, back); has(acts, Alert) == nil || !strings.Contains(has(acts, Alert).Text, "code directory") {
		t.Fatalf("still missing: %v", acts)
	}
}

// A container that is merely stopped (by a person, or not there after
// `fleetctl down`) carries no Docker error and is left alone.
func TestAStoppedContainerIsNotStarted(t *testing.T) {
	_, acts := Decide(th(), State{Name: "agent-03"}, Obs{Now: t0})
	if has(acts, Start) != nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

func atProc(o Obs, proc string, at time.Duration, events ...observe.Event) Obs {
	o.Proc, o.Now, o.Events = proc, t0.Add(at), events
	return o
}

// A turn in flight when the power went is not lost: when the session comes
// back (resumed, idle, in a new process) it is told to carry on.
func TestATurnCutShortByARestartIsCarriedOn(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("busy"), "boot1/100", 0))
	if !st.TurnOpen || st.TurnProc != "boot1/100" {
		t.Fatalf("busy: %+v", st)
	}
	// Three hours of outage, then the session is back in another boot.
	st, acts := Decide(th(), st, atProc(obs("idle"), "boot2/90", 3*time.Hour))
	p := has(acts, Prompt)
	if p == nil || !strings.Contains(p.Text, "T1") || !strings.Contains(p.Text, "carry on") {
		t.Fatalf("acts = %v", acts)
	}
	if a := has(acts, Push); a == nil || a.Event != "fleet.agent.resumed" {
		t.Fatalf("acts = %v", acts)
	}
	if st.TurnOpen || has(acts, Rotate) != nil || !st.LastBusy.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("the outage counted as idleness, or the turn stayed open: %+v %v", st, kinds(acts))
	}
	// It is told once.
	if _, acts = Decide(th(), st, atProc(obs("idle"), "boot2/90", 3*time.Hour+10*time.Second)); has(acts, Prompt) != nil {
		t.Fatalf("told twice: %v", kinds(acts))
	}
}

// A turn that ended (its Stop is on the agent's volume) just before the power
// went is not resumed, even when the Stop is read only after the restart,
// while the new session is still registering.
func TestATurnThatEndedBeforeTheCutIsLeftAlone(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("busy"), "boot1/100", 0))
	starting := atProc(obs(""), "boot2/90", time.Hour, observe.Event{Event: "Stop", Session: "s1"})
	starting.Sessions = nil // claude is starting: no session registered yet
	st, _ = Decide(th(), st, starting)
	if st.TurnOpen {
		t.Fatalf("the Stop was lost: %+v", st)
	}
	if _, acts := Decide(th(), st, atProc(obs("idle"), "boot2/90", time.Hour+time.Minute)); has(acts, Prompt) != nil {
		t.Fatalf("acts = %v", acts)
	}
}

// A turn begun between the last tick and the power cut ran in the process
// seen at that tick, though fleetd reads its UserPromptSubmit only after.
func TestATurnBegunJustBeforeTheCutIsCarriedOn(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("idle"), "boot1/100", 0))
	o := atProc(obs("idle"), "boot2/90", time.Hour, observe.Event{Event: "UserPromptSubmit", Session: "s1"})
	if _, acts := Decide(th(), st, o); has(acts, Prompt) == nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

// Escape ends a turn with no Stop. Once it has been idle a while in the same
// process, the turn is over, and a later restart does not revive it.
func TestATurnStoppedByEscapeIsNotRevivedByALaterRestart(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("busy"), "boot1/100", 0))
	st, acts := Decide(th(), st, atProc(obs("idle"), "boot1/100", time.Minute))
	if !st.TurnOpen || has(acts, Prompt) != nil {
		t.Fatalf("closed too soon: %+v", st)
	}
	st, _ = Decide(th(), st, atProc(obs("idle"), "boot1/100", 5*time.Minute))
	if st.TurnOpen {
		t.Fatalf("still open after idling: %+v", st)
	}
	if _, acts = Decide(th(), st, atProc(obs("idle"), "boot2/90", time.Hour)); has(acts, Prompt) != nil {
		t.Fatalf("revived: %v", kinds(acts))
	}
}

// A restart with no task, or with the task done, has nothing to carry on.
func TestACutTurnWithNoOpenTaskIsNotPrompted(t *testing.T) {
	for _, st0 := range []State{{Name: "agent-03"}, {Name: "agent-03", Task: "T1", Done: true}} {
		st, _ := Decide(th(), st0, atProc(obs("busy"), "boot1/100", 0))
		st.Done = st0.Done
		if _, acts := Decide(th(), st, atProc(obs("idle"), "boot2/90", time.Hour)); has(acts, Prompt) != nil {
			t.Fatalf("%+v: acts = %v", st0, acts)
		}
	}
}

// Between turns, waiting on a background command of its own (a build, a test
// run): the restart killed the command, and Claude Code starts no turn to say
// so. An agent that still owes its task a word is told to carry on.
func TestAnAgentWaitingOnItsOwnBackgroundCommandIsToldToCarryOn(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("busy"), "boot1/100", 0, observe.Event{Event: "UserPromptSubmit", Session: "s1"}))
	st, _ = Decide(th(), st, atProc(obs("idle"), "boot1/100", 10*time.Second, observe.Event{Event: "Stop", Session: "s1"}))
	if st.TurnOpen {
		t.Fatalf("the turn ended: %+v", st)
	}
	back := atProc(obs("idle"), "boot2/90", time.Hour)
	back.Owes = true
	_, acts := Decide(th(), st, back)
	if p := has(acts, Prompt); p == nil || !strings.Contains(p.Text, "background command") {
		t.Fatalf("acts = %v", acts)
	}
}

// One that had reported and waits on its orchestrator owes nothing: a restart
// does not wake it.
func TestAnAgentThatReportedIsNotWokenByARestart(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("idle"), "boot1/100", 0))
	if _, acts := Decide(th(), st, atProc(obs("idle"), "boot2/90", time.Hour)); has(acts, Prompt) != nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

// Owing in the same process is only working between turns: nothing to do.
func TestOwingInTheSameProcessIsLeftAlone(t *testing.T) {
	st, _ := Decide(th(), State{Name: "agent-03", Task: "T1"}, atProc(obs("idle"), "boot1/100", 0))
	o := atProc(obs("idle"), "boot1/100", time.Minute)
	o.Owes = true
	if _, acts := Decide(th(), st, o); has(acts, Prompt) != nil {
		t.Fatalf("acts = %v", kinds(acts))
	}
}

// fleetd could not ask the agent's host about it — at boot, before the
// network is up; the host is off — so what the agent is doing is unknown. It
// is not down: nothing is pushed, nothing restarted, and the phase it had is
// kept. Once the host has been out of reach for a while it is out of reach
// (shown as unreachable) and you are told, once.
func TestAnUnreachableHostIsNotDown(t *testing.T) {
	st0 := State{Name: "agent-03", Phase: Waiting, Task: "T1", SessionID: "s1"}
	o := Obs{Now: t0, Unreachable: "pc2: ssh: connect to host 192.0.2.26 port 22: Network is unreachable"}
	st, acts := Decide(th(), st0, o)
	if len(acts) != 0 || st.Phase != Waiting || st.Task != "T1" || st.SessionID != "s1" || !st.UnreachableSince.Equal(t0) {
		t.Fatalf("at once: phase=%s acts=%v %+v", st.Phase, acts, st)
	}
	o.Now = t0.Add(unreachableAfter - time.Second)
	if st, acts = Decide(th(), st, o); len(acts) != 0 || st.Phase != Waiting {
		t.Fatalf("within the grace: phase=%s acts=%v", st.Phase, acts)
	}
	o.Now = t0.Add(unreachableAfter)
	st, acts = Decide(th(), st, o)
	if st.Phase != Waiting || !st.OutOfReach(o.Now) || has(acts, Alert) == nil || !strings.Contains(has(acts, Alert).Text, "pc2") || has(acts, Push) == nil {
		t.Fatalf("after the grace: phase=%s acts=%v", st.Phase, acts)
	}
	// One alert per episode, and the alert's text is the host's, not the
	// agent's, so a host's agents alert as one.
	other, _ := Decide(th(), State{Name: "agent-04", UnreachableSince: t0}, o)
	_, otherActs := Decide(th(), other, o)
	if a := has(otherActs, Alert); a != nil {
		t.Fatalf("alerted twice: %v", otherActs)
	}
	o.Now = t0.Add(time.Hour)
	if st, acts = Decide(th(), st, o); len(acts) != 0 {
		t.Fatalf("again: %v", acts)
	}

	// Back: the phase is the observed one again, and the episode is over.
	back := obs("idle")
	back.Now = t0.Add(2 * time.Hour)
	st, _ = Decide(th(), st, back)
	if st.Phase != Waiting || !st.UnreachableSince.IsZero() || st.Alerted["unreachable"] || st.OutOfReach(back.Now) {
		t.Fatalf("back: %+v", st)
	}
}

// The text of the alert names only the host: every agent on it says the same.
func TestTheUnreachableAlertIsTheSameForEveryAgentOnTheHost(t *testing.T) {
	o := Obs{Now: t0.Add(unreachableAfter), Unreachable: "pc2: connection refused"}
	_, a := Decide(th(), State{Name: "agent-03", UnreachableSince: t0}, o)
	_, b := Decide(th(), State{Name: "agent-04", UnreachableSince: t0}, o)
	if has(a, Alert) == nil || has(b, Alert) == nil || has(a, Alert).Text != has(b, Alert).Text {
		t.Fatalf("a=%v b=%v", a, b)
	}
}

// Claude Code reports a session busy while any background command it started
// runs, turn or no turn. A task that is done must not stay unrotated behind
// one (a leftover that never ends would keep the agent out of the pool for
// good): once the hooks say its turn is over, it rotates.
func TestADoneTaskRotatesWhenItsTurnIsOverThoughABackgroundCommandRuns(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", Done: true, DoneAt: t0, SessionID: "s1"}
	o := obs("busy") // a background shell
	o.Events = []observe.Event{{T: t0.Unix(), Event: "Stop", Session: "s1"}}
	st, acts := Decide(th(), st, o)
	if a := has(acts, Rotate); a == nil || a.Task != "T1" || st.Done {
		t.Fatalf("not rotated: %v %+v", kinds(acts), st)
	}
}

// Busy with no Stop yet: its last turn may still be writing. It is given a
// few minutes, then rotated anyway — the task is closed.
func TestADoneTaskBusyWithoutAStopRotatesAfterAGrace(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", Done: true, DoneAt: t0, SessionID: "s1"}
	o := obs("busy")
	o.Now = t0.Add(time.Minute)
	st, acts := Decide(th(), st, o)
	if has(acts, Rotate) != nil {
		t.Fatalf("rotated mid-turn at once: %v", kinds(acts))
	}
	o.Now = t0.Add(doneGrace)
	o.Events = []observe.Event{{T: t0.Unix(), Event: "PreToolUse", Session: "s1"}}
	if _, acts = Decide(th(), st, o); has(acts, Rotate) == nil {
		t.Fatalf("not rotated after the grace: %v", kinds(acts))
	}
}

// Not done: a busy agent is left alone, background command or not — it may
// be waiting on its own build, and is woken when the build ends.
func TestAnOpenTaskBusyInTheBackgroundIsNotRotated(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", SessionID: "s1"}
	o := obs("busy")
	o.Events = []observe.Event{{T: t0.Unix(), Event: "Stop", Session: "s1"}}
	o.Now = t0.Add(time.Hour)
	if st, acts := Decide(th(), st, o); len(acts) != 0 || st.Phase != Working {
		t.Fatalf("acts %v phase %s", kinds(acts), st.Phase)
	}
}

// An agent that owes its task a word, is idle with its turn over, and has
// said nothing for reportNudge — its report failed to send, or it stopped
// without one — is asked once to report or say what blocks it. Not before,
// not while it works, not when it owes nothing; again only after it has
// written back and been given something new.
func TestAnAgentSilentOnItsTaskIsAskedToReportOnce(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", SessionID: "s1", HookIdle: true, LastBusy: t0}
	o := obs("idle")
	o.TranscriptMTime = t0
	o.Owes = true
	o.Now = t0.Add(reportNudge - time.Minute)
	if _, acts := Decide(th(), st, o); has(acts, Prompt) != nil {
		t.Fatalf("too soon: %v", kinds(acts))
	}
	o.Now = t0.Add(reportNudge)
	st, acts := Decide(th(), st, o)
	p := has(acts, Prompt)
	if p == nil || !strings.Contains(p.Text, "T1") || !strings.Contains(p.Text, "report") {
		t.Fatalf("not asked: %v", acts)
	}
	o.Now = t0.Add(reportNudge + time.Hour)
	if st, acts = Decide(th(), st, o); has(acts, Prompt) != nil {
		t.Fatalf("asked twice: %v", kinds(acts))
	}
	// It wrote back; a follow-up came; silence again: asked again.
	o.Owes = false
	st, _ = Decide(th(), st, o)
	o.Owes = true
	o.Now = o.Now.Add(time.Minute)
	if _, acts = Decide(th(), st, o); has(acts, Prompt) == nil {
		t.Fatalf("not asked about the next instruction: %v", kinds(acts))
	}

	// Owing nothing, or working, or its task done: never.
	for name, edit := range map[string]func(*State, *Obs){
		"owes nothing": func(s *State, o *Obs) { o.Owes = false },
		"busy":         func(s *State, o *Obs) { o.Sessions = live("busy") },
		"done":         func(s *State, o *Obs) { s.Done = true },
		"no task":      func(s *State, o *Obs) { s.Task = "" },
	} {
		s := State{Name: "agent-03", Task: "T1", SessionID: "s1", HookIdle: true, LastBusy: t0}
		ob := obs("idle")
		ob.TranscriptMTime, ob.Owes, ob.Now = t0, true, t0.Add(time.Hour)
		edit(&s, &ob)
		if _, acts := Decide(th(), s, ob); has(acts, Prompt) != nil && has(acts, Rotate) == nil {
			t.Errorf("%s: asked: %v", name, kinds(acts))
		}
	}
}

// An agent waiting on an answer to its question is not idle on its task: it
// is neither nudged to report nor given up on, however long the wait.
func TestAnAgentWaitingOnAnAnswerIsNotNudgedOrArchived(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", SessionID: "s1", HookIdle: true, LastBusy: t0}
	o := obs("idle")
	o.TranscriptMTime = t0
	o.Owes = true
	o.Asking = true
	for _, d := range []time.Duration{reportNudge, 3 * time.Hour, 30 * time.Hour} {
		o.Now = t0.Add(d)
		var acts []Action
		st, acts = Decide(th(), st, o)
		if len(acts) != 0 {
			t.Fatalf("after %s asking: %v", d, kinds(acts))
		}
	}
	// Answered: it is idle on the task again, from the answer, not the question.
	o.Asking = false
	o.AnsweredAt = t0.Add(30 * time.Hour)
	o.Now = o.AnsweredAt.Add(3 * time.Hour)
	st, acts := Decide(th(), st, o)
	if p := has(acts, Push); p == nil || p.Event != "fleet.agent.idle_on_task" || has(acts, Rotate) != nil {
		t.Fatalf("3h after the answer: %v", kinds(acts))
	}
	o.Now = o.AnsweredAt.Add(25 * time.Hour)
	if _, acts = Decide(th(), st, o); has(acts, Rotate) == nil {
		t.Fatalf("25h after the answer: %v", kinds(acts))
	}
}

// Without the answer's time the stale clock would have run through the wait.
func TestTheIdleClockCountsFromTheAnswerNotTheQuestion(t *testing.T) {
	st := State{Name: "agent-03", Task: "T1", SessionID: "s1", HookIdle: true, LastBusy: t0}
	o := obs("idle")
	o.TranscriptMTime = t0
	o.Now = t0.Add(25 * time.Hour)
	o.AnsweredAt = t0.Add(24 * time.Hour)
	if _, acts := Decide(th(), st, o); has(acts, Rotate) != nil {
		t.Fatalf("archived 1h after an answer: %v", kinds(acts))
	}
}
