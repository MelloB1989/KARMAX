package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// gateHarness is one account whose example-api-cpu-high alarm a test moves
// between states, with each move in the alarm history as CloudWatch keeps it.
type gateHarness struct {
	*harness
	acct *fakeAPI
	t    *testing.T
}

func newGate(t *testing.T, cfg Config) *gateHarness {
	t.Helper()
	acct := &fakeAPI{account: "111122223333"}
	acct.alarms = []Alarm{cpuAlarm("OK", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))}
	cfg.Profiles = []string{"ops-admin"}
	g := &gateHarness{harness: newHarness(t, cfg, map[string]*fakeAPI{"ops-admin": acct}), acct: acct, t: t}
	g.poll() // first sight: the alarm is OK
	if evs := g.take(); len(evs) != 0 {
		t.Fatalf("first sight published %+v", evs)
	}
	return g
}

// move sets the alarm's state a minute after the last poll, and records it.
func (g *gateHarness) move(to string, after time.Duration) {
	at := g.now.Add(after)
	from := g.acct.alarms[0].State
	g.acct.alarms[0] = cpuAlarm(to, at)
	g.acct.history = append(g.acct.history, Transition{Alarm: g.acct.alarms[0].Name, At: at, From: from, To: to,
		Reason: g.acct.alarms[0].Reason})
}

// poll moves the clock to the next 5-minute poll and runs it.
func (g *gateHarness) poll() AlarmReport {
	g.t.Helper()
	g.now = g.now.Add(5 * time.Minute)
	rep, err := g.src.PollAlarms(context.Background())
	if err != nil {
		g.t.Fatal(err)
	}
	return rep
}

func kinds(g *gateHarness) []string {
	var out []string
	for _, e := range g.take() {
		out = append(out, e.Type)
	}
	return out
}

// A flip into ALARM for one 5-minute window, the common flap, tells nothing:
// neither the ALARM nor the OK after it.
func TestABriefAlarmPublishesNothing(t *testing.T) {
	g := newGate(t, Config{})
	g.move("ALARM", time.Minute)
	g.poll()
	g.move("OK", time.Minute)
	g.poll()
	g.poll()
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("a 5-minute ALARM published %v", got)
	}
}

// An alarm that stays in ALARM for the debounce is told once, urgently, with
// the time it went off; its recovery is told after.
func TestALongAlarmIsToldAfterTheDebounce(t *testing.T) {
	g := newGate(t, Config{})
	g.move("ALARM", time.Minute)
	fired := g.acct.alarms[0].UpdatedAt
	g.poll()
	g.poll()
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("published %v before the third poll in ALARM", got)
	}
	g.poll()
	evs := g.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Urgent || evs[0].Backlog {
		t.Fatalf("third poll in ALARM published %+v, want one urgent alarm.ALARM", evs)
	}
	if !evs[0].At.Equal(fired) || !strings.Contains(evs[0].Summary, "OK → ALARM") {
		t.Errorf("at %v summary %q; want the transition that started it", evs[0].At, evs[0].Summary)
	}
	for i := 0; i < 10; i++ {
		g.poll()
	}
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("an ALARM still going was told again: %v", got)
	}
	g.move("OK", time.Minute)
	g.poll()
	evs = g.take()
	if len(evs) != 1 || evs[0].Type != "alarm.OK" || evs[0].Urgent {
		t.Fatalf("recovery published %+v, want one non-urgent alarm.OK", evs)
	}
}

// With debounce_polls 1 an ALARM is told on the poll that finds it, as before
// the gate.
func TestDebounceOfOneTellsAtOnce(t *testing.T) {
	g := newGate(t, Config{AlarmDebounce: 1})
	g.move("ALARM", time.Minute)
	g.move("OK", 2*time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 2 || got[0] != "alarm.ALARM" || got[1] != "alarm.OK" {
		t.Fatalf("published %v, want the ALARM and the OK at once", got)
	}
}

// A flap storm, 100 flips in and out of ALARM, publishes at most one event.
func TestAFlapStormPublishesAtMostOneEvent(t *testing.T) {
	g := newGate(t, Config{})
	for i := 0; i < 100; i++ {
		g.move("ALARM", time.Minute)
		g.poll()
		g.move("OK", time.Minute)
		g.poll()
	}
	if got := kinds(g); len(got) > 1 {
		t.Fatalf("100 flips published %d events: %v", len(got), got)
	}
	// Two flips inside one poll window are no different.
	g2 := newGate(t, Config{})
	for i := 0; i < 50; i++ {
		g2.move("ALARM", time.Minute)
		g2.move("OK", 2*time.Minute)
		g2.poll()
	}
	if got := kinds(g2); len(got) > 1 {
		t.Fatalf("50 flips within polls published %d events: %v", len(got), got)
	}
}

// After an ALARM is told, the next ALARMs within the cooldown are held, even
// past the debounce; the first one after it is told with the count.
func TestTheCooldownHoldsFlipsAndCountsThem(t *testing.T) {
	g := newGate(t, Config{AlarmCooldown: 6 * time.Hour})
	g.move("ALARM", time.Minute)
	g.poll()
	g.poll()
	g.poll()
	g.move("OK", time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 2 {
		t.Fatalf("the first episode published %v, want its ALARM and OK", got)
	}
	// 14 flips inside the cooldown, each held for three polls: past the
	// debounce, short of the breakthrough.
	var held []HeldAlarm
	for i := 0; i < 14; i++ {
		g.move("ALARM", time.Minute)
		g.poll()
		g.poll()
		g.poll()
		g.move("OK", time.Minute)
		held = g.poll().Held
	}
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("flips inside the cooldown published %v", got)
	}
	if len(held) != 1 || held[0].Alarm != "example-api-cpu-high" || held[0].Count != 14 {
		t.Errorf("held = %+v, want the alarm with 14", held)
	}
	g.now = g.now.Add(6 * time.Hour)
	g.move("ALARM", time.Minute)
	g.poll()
	g.poll()
	g.poll()
	evs := g.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Urgent {
		t.Fatalf("after the cooldown published %+v, want one urgent ALARM", evs)
	}
	if !strings.Contains(evs[0].Summary, "flipped 14 times since the last report") || evs[0].Data["flips_since_report"] != 14 {
		t.Errorf("summary %q data %v; want the 14 held flips", evs[0].Summary, evs[0].Data["flips_since_report"])
	}
}

// An ALARM that holds inside the cooldown is an outage, not a flap: it is
// told once it has held for the breakthrough.
func TestAnAlarmThatHoldsBreaksThroughTheCooldown(t *testing.T) {
	g := newGate(t, Config{AlarmCooldown: 6 * time.Hour})
	g.move("ALARM", time.Minute)
	g.poll()
	g.poll()
	g.poll()
	g.move("OK", time.Minute)
	g.poll()
	g.take()

	g.move("ALARM", time.Minute)
	for i := 1; i < cooldownBreakthrough*DefaultAlarmDebounce; i++ {
		g.poll()
	}
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("published %v before the breakthrough", got)
	}
	g.poll()
	evs := g.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Urgent {
		t.Fatalf("a held ALARM published %+v, want one urgent ALARM", evs)
	}
}

// The ignore list hides what it names, slashes and all, and nothing else.
func TestTheIgnoreListHidesWhatItNames(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}, AlarmDebounce: 1,
		AlarmIgnore: []string{"TargetTracking-*-AlarmHigh-*", "*-AlarmLow-*"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())

	at := h.now.Add(time.Minute)
	names := []string{
		"TargetTracking-service/example-cluster/example-api-AlarmHigh-0a1b2c3d",
		"TargetTracking-service/example-cluster/example-api-AlarmLow-4e5f6a7b",
		"example-api-cpu-high",
	}
	for _, n := range names {
		acct.alarms = append(acct.alarms, Alarm{Name: n, State: "ALARM", UpdatedAt: at})
		acct.history = append(acct.history, Transition{Alarm: n, At: at, From: "OK", To: "ALARM"})
	}
	h.now = h.now.Add(5 * time.Minute)
	rep, err := h.src.PollAlarms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	evs := h.take()
	if len(evs) != 1 || !strings.Contains(evs[0].Title, "example-api-cpu-high") {
		t.Fatalf("published %+v, want only example-api-cpu-high", evs)
	}
	if rep.Ignored != 2 || rep.InAlarm != 3 || rep.Alarms != 3 {
		t.Errorf("report = %+v; ignored alarms are still counted", rep)
	}
	if hl := h.health("ops-admin"); !strings.Contains(hl.Detail, "2 ignored") {
		t.Errorf("health %q does not say how many are ignored", hl.Detail)
	}
}

// With no ignore list nothing is hidden.
func TestNoIgnoreListHidesNothing(t *testing.T) {
	g := newGate(t, Config{AlarmDebounce: 1})
	g.move("ALARM", time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 1 {
		t.Fatalf("published %v", got)
	}
}

// A restart (a new Source over the same store) neither forgets an ALARM's
// polls nor tells one twice.
func TestTheGateSurvivesARestart(t *testing.T) {
	g := newGate(t, Config{})
	g.move("ALARM", time.Minute)
	g.poll()
	g.poll()
	restart := func() {
		kv, now := g.kv, g.now
		g.harness = newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": g.acct})
		g.kv, g.now = kv, now
	}
	restart()
	g.poll()
	if got := kinds(g); len(got) != 1 || got[0] != "alarm.ALARM" {
		t.Fatalf("the third poll in ALARM, after a restart, published %v", got)
	}
	restart()
	for i := 0; i < 3; i++ {
		g.poll()
	}
	if got := kinds(g); len(got) != 0 {
		t.Fatalf("a restart told the ALARM again: %v", got)
	}
	g.move("OK", time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 1 || got[0] != "alarm.OK" {
		t.Fatalf("after a restart the recovery published %v", got)
	}
}

// An account's first read tells a standing ALARM at once (as backlog, as
// before the gate), and its recovery later.
func TestAnAlarmInAlarmOnFirstSightIsToldAtOnce(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	acct.alarms = []Alarm{cpuAlarm("ALARM", time.Date(2026, 10, 31, 0, 50, 0, 0, time.UTC))}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Backlog {
		t.Fatalf("first sight published %+v, want the standing ALARM as backlog", evs)
	}
	h.now = h.now.Add(5 * time.Minute)
	at := h.now.Add(-time.Minute)
	acct.alarms = []Alarm{cpuAlarm("OK", at)}
	acct.history = []Transition{{Alarm: "example-api-cpu-high", At: at, From: "ALARM", To: "OK"}}
	h.src.PollAlarms(context.Background())
	if evs := h.take(); len(evs) != 1 || evs[0].Type != "alarm.OK" {
		t.Fatalf("its recovery published %+v", evs)
	}
}

// An alarm created after the account was first read, and found in ALARM, is
// told at once and urgently: the gate holds back only alarms it knows.
func TestANewAlarmIsToldAtOnce(t *testing.T) {
	g := newGate(t, Config{AlarmCooldown: 6 * time.Hour})
	at := g.now.Add(time.Minute)
	g.acct.alarms = append(g.acct.alarms, Alarm{Name: "example-new-alarm", State: "ALARM", UpdatedAt: at})
	g.acct.history = append(g.acct.history, Transition{Alarm: "example-new-alarm", At: at, From: "INSUFFICIENT_DATA", To: "ALARM"})
	g.poll()
	evs := g.take()
	if len(evs) != 1 || !evs[0].Urgent || evs[0].Backlog || !strings.Contains(evs[0].Title, "example-new-alarm") {
		t.Fatalf("published %+v, want the new alarm, urgent", evs)
	}
	// ... and one found in ALARM with nothing in its history yet.
	g.acct.alarms = append(g.acct.alarms, Alarm{Name: "example-newer-alarm", State: "ALARM", UpdatedAt: g.now})
	g.poll()
	if evs := g.take(); len(evs) != 1 || !evs[0].Urgent || !strings.Contains(evs[0].Title, "example-newer-alarm") {
		t.Fatalf("published %+v, want the newer alarm, urgent", evs)
	}
}

// An alarm whose ALARM was told is never held after: going to
// INSUFFICIENT_DATA does not end it, a return to ALARM from there is told as
// before the gate, and its way back to OK is told.
func TestAToldAlarmIsNeverHeld(t *testing.T) {
	g := newGate(t, Config{AlarmCooldown: 6 * time.Hour})
	g.move("ALARM", time.Minute)
	g.poll()
	g.poll()
	g.poll()
	g.take()

	g.move("INSUFFICIENT_DATA", time.Minute)
	g.poll()
	g.move("ALARM", time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 1 || got[0] != "alarm.ALARM" {
		t.Fatalf("INSUFFICIENT_DATA -> ALARM of a told alarm published %v", got)
	}
	g.move("INSUFFICIENT_DATA", time.Minute)
	g.poll()
	g.move("OK", time.Minute)
	g.poll()
	if got := kinds(g); len(got) != 1 || got[0] != "alarm.OK" {
		t.Fatalf("a told alarm back to OK by way of INSUFFICIENT_DATA published %v", got)
	}
}

// Without alarm history, states compared from poll to poll go through the
// same gate.
func TestTheGateWorksWithoutHistory(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", historyErr: &DeniedError{Err: errors.New("cloudwatch:DescribeAlarmHistory")}}
	acct.alarms = []Alarm{cpuAlarm("OK", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	poll := func(state string) {
		h.now = h.now.Add(5 * time.Minute)
		if acct.alarms[0].State != state {
			acct.alarms[0] = cpuAlarm(state, h.now.Add(-time.Minute))
		}
		if _, err := h.src.PollAlarms(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h.src.PollAlarms(context.Background())
	poll("ALARM")
	poll("OK")
	if evs := h.take(); len(evs) != 0 {
		t.Fatalf("a brief ALARM published %+v", evs)
	}
	poll("ALARM")
	poll("ALARM")
	poll("ALARM")
	if evs := h.take(); len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Urgent {
		t.Fatalf("a long ALARM published %+v", evs)
	}
	poll("OK")
	if evs := h.take(); len(evs) != 1 || evs[0].Type != "alarm.OK" {
		t.Fatalf("its recovery published %+v", evs)
	}
}
