package reconcile

import (
	"encoding/json"
	"slices"
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
