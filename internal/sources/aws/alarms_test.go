package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func cpuAlarm(state string, at time.Time) Alarm {
	return Alarm{
		Name: "example-api-cpu-high", ARN: "arn:aws:cloudwatch:us-east-1:111122223333:alarm:example-api-cpu-high",
		State: state, UpdatedAt: at, Reason: "Threshold Crossed: 1 datapoint [93.2] was greater than the threshold (80.0).",
		Namespace: "AWS/EC2", Metric: "CPUUtilization", Statistic: "Average", Period: 300,
		Dimensions: []Dimension{{"InstanceId", "i-0example"}}, Comparison: "GreaterThanThreshold", Threshold: f64(80),
	}
}

// An alarm going off is published once, urgently, with what a person needs
// to judge it: the reason, the metric and the threshold.
func TestAnAlarmGoingOffIsPublishedOnceAndUrgent(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background()) // first sight: nothing has happened yet

	fired := h.now.Add(2 * time.Minute)
	acct.alarms = []Alarm{cpuAlarm("ALARM", fired)}
	acct.history = []Transition{{Alarm: "example-api-cpu-high", At: fired, From: "OK", To: "ALARM", Reason: acct.alarms[0].Reason}}
	h.now = h.now.Add(5 * time.Minute)
	if _, err := h.src.PollAlarms(context.Background()); err != nil {
		t.Fatal(err)
	}
	evs := h.take()
	if len(evs) != 1 {
		t.Fatalf("published %d events, want 1: %+v", len(evs), evs)
	}
	e := evs[0]
	if e.Source != "aws" || e.Account != "111122223333/us-east-1" || e.Type != "alarm.ALARM" || !e.Urgent {
		t.Errorf("event = %s %s %s urgent=%v", e.Source, e.Account, e.Type, e.Urgent)
	}
	for _, want := range []string{"Threshold Crossed", "CPUUtilization", "80", "GreaterThanThreshold", "InstanceId=i-0example"} {
		if !strings.Contains(e.Summary, want) {
			t.Errorf("summary %q lacks %q", e.Summary, want)
		}
	}
	if !e.At.Equal(fired) || e.Key == "" || !strings.Contains(e.Title, "example-api-cpu-high") {
		t.Errorf("at %v key %q title %q", e.At, e.Key, e.Title)
	}
	if e.Data["profile"] != "ops-admin" || e.Data["region"] != "us-east-1" {
		t.Errorf("data = %v; the agent needs the profile and region to look deeper", e.Data)
	}

	h.now = h.now.Add(5 * time.Minute)
	h.src.PollAlarms(context.Background())
	if again := h.take(); len(again) != 0 {
		t.Fatalf("the same transition was published again: %+v", again)
	}
}

// Back to OK is news, not an emergency; an alarm merely getting data is not
// news at all.
func TestARecoveryIsPublishedButNotUrgent(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())

	t1 := h.now.Add(time.Minute)
	acct.alarms = []Alarm{cpuAlarm("OK", t1.Add(time.Minute))}
	acct.history = []Transition{
		{Alarm: "example-api-cpu-high", At: t1, From: "INSUFFICIENT_DATA", To: "OK"},
		{Alarm: "example-api-cpu-high", At: t1.Add(time.Second), From: "OK", To: "INSUFFICIENT_DATA"},
		{Alarm: "example-api-cpu-high", At: t1.Add(time.Minute), From: "ALARM", To: "OK", Reason: "Threshold Crossed: back under"},
	}
	h.now = h.now.Add(5 * time.Minute)
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 1 || evs[0].Type != "alarm.OK" || evs[0].Urgent {
		t.Fatalf("events = %+v, want one non-urgent alarm.OK", evs)
	}
}

// An alarm that fired and already recovered between two reads is told, but
// nobody needs waking for it.
func TestAnAlarmThatAlreadyRecoveredIsNotUrgent(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())

	t1 := h.now.Add(time.Minute)
	acct.alarms = []Alarm{cpuAlarm("OK", t1.Add(2*time.Minute))}
	acct.history = []Transition{
		{Alarm: "example-api-cpu-high", At: t1, From: "OK", To: "ALARM"},
		{Alarm: "example-api-cpu-high", At: t1.Add(2 * time.Minute), From: "ALARM", To: "OK"},
	}
	h.now = h.now.Add(5 * time.Minute)
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 2 || evs[0].Type != "alarm.ALARM" || evs[0].Urgent || evs[1].Type != "alarm.OK" {
		t.Fatalf("events = %+v", evs)
	}
	if !strings.Contains(evs[0].Summary, "since recovered") {
		t.Errorf("summary %q does not say it already recovered", evs[0].Summary)
	}
}

// History is read from where the last read stopped, with an overlap for
// late-arriving entries, not from the start every time.
func TestHistoryIsReadFromWhereItLeftOff(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}, AlarmLookback: 24 * time.Hour}, map[string]*fakeAPI{"ops-admin": acct})
	start := h.now
	h.src.PollAlarms(context.Background())
	h.now = h.now.Add(5 * time.Minute)
	h.src.PollAlarms(context.Background())

	if len(acct.historyAsk) != 2 {
		t.Fatalf("history read %d times", len(acct.historyAsk))
	}
	if !acct.historyAsk[0].Equal(start.Add(-24 * time.Hour)) {
		t.Errorf("first read from %v, want the lookback", acct.historyAsk[0])
	}
	if got := acct.historyAsk[1]; got.Before(start.Add(-historyOverlap)) || got.After(start) {
		t.Errorf("second read from %v, want just before %v", got, start)
	}
}

// The first read of an account tells memory about alarms already firing, once,
// without treating something old as an emergency.
func TestAStandingAlarmIsToldOnceOnFirstSight(t *testing.T) {
	acct := &fakeAPI{account: "111122223333"}
	acct.alarms = []Alarm{cpuAlarm("ALARM", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)), {Name: "example-quiet", State: "OK"}}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || evs[0].Urgent {
		t.Fatalf("events = %+v, want one non-urgent standing alarm", evs)
	}
	h.now = h.now.Add(5 * time.Minute)
	h.src.PollAlarms(context.Background())
	if again := h.take(); len(again) != 0 {
		t.Fatalf("the standing alarm was told again: %+v", again)
	}
}

// A role that may not read alarm history still sees transitions, by
// comparing each alarm's state with the last read.
func TestWithoutHistoryAccessStatesAreCompared(t *testing.T) {
	acct := &fakeAPI{account: "111122223333", historyErr: &DeniedError{Err: errors.New("cloudwatch:DescribeAlarmHistory")}}
	acct.alarms = []Alarm{cpuAlarm("OK", time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": acct})
	h.src.PollAlarms(context.Background())
	if evs := h.take(); len(evs) != 0 {
		t.Fatalf("an alarm in OK on first sight was published: %+v", evs)
	}

	h.now = h.now.Add(5 * time.Minute)
	acct.alarms = []Alarm{cpuAlarm("ALARM", h.now.Add(-time.Minute))}
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 1 || evs[0].Type != "alarm.ALARM" || !evs[0].Urgent {
		t.Fatalf("events = %+v, want an urgent alarm.ALARM", evs)
	}

	h.now = h.now.Add(5 * time.Minute)
	acct.alarms = []Alarm{cpuAlarm("OK", h.now.Add(-time.Minute))}
	h.src.PollAlarms(context.Background())
	evs = h.take()
	if len(evs) != 1 || evs[0].Type != "alarm.OK" || evs[0].Urgent {
		t.Fatalf("events = %+v, want a non-urgent alarm.OK", evs)
	}
	if hl := h.health("ops-admin"); !strings.Contains(hl.Detail, "DescribeAlarmHistory") {
		t.Errorf("health %+v does not say history is not readable", hl)
	}
}

// Two profiles on one account and region are one set of alarms: read once.
func TestOneAccountAndRegionIsReadOnce(t *testing.T) {
	admin := &fakeAPI{account: "111122223333"}
	readonly := &fakeAPI{account: "111122223333"}
	eu := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin", "ops-readonly", "ops-eu"}},
		map[string]*fakeAPI{"ops-admin": admin, "ops-readonly": readonly, "ops-eu": eu})
	h.src.PollAlarms(context.Background())

	if admin.count("Alarms") != 1 || readonly.count("Alarms") != 0 {
		t.Errorf("alarms read by admin %d, readonly %d; want the account read once", admin.count("Alarms"), readonly.count("Alarms"))
	}
	if eu.count("Alarms") != 1 {
		t.Errorf("the same account in another region is other alarms; read %d times", eu.count("Alarms"))
	}
	if hl := h.health("ops-readonly"); hl.Status != StatusSkipped || !strings.Contains(hl.Detail, "ops-admin") {
		t.Errorf("readonly health = %+v", hl)
	}
}

// A profile whose login expired is skipped with the exact command that fixes
// it, and the others are read as usual. Another profile on the same account
// stands in for it.
func TestAnExpiredLoginIsSkippedWithItsFix(t *testing.T) {
	expired := &fakeAPI{account: "111122223333", identityErr: &AuthError{Err: errors.New("the SSO session has expired")}}
	standIn := &fakeAPI{account: "111122223333"}
	other := &fakeAPI{account: "444455556666", identityErr: &AuthError{Err: errors.New("token expired")}}
	static := &fakeAPI{account: "777788889999"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin", "ops-readonly", "staging", "static"}},
		map[string]*fakeAPI{"ops-admin": expired, "ops-readonly": standIn, "staging": other, "static": static})
	if _, err := h.src.PollAlarms(context.Background()); err != nil {
		t.Fatalf("one expired login failed the whole read: %v", err)
	}
	if standIn.count("Alarms") != 1 || static.count("Alarms") != 1 || other.count("Alarms") != 0 {
		t.Errorf("reads: stand-in %d, static %d, expired %d", standIn.count("Alarms"), static.count("Alarms"), other.count("Alarms"))
	}
	if hl := h.health("staging"); hl.Status != StatusFailed || !strings.Contains(hl.Detail, "aws sso login --sso-session other-sso") {
		t.Errorf("staging health = %+v", hl)
	}
	if hl := h.health("ops-admin"); hl.Status != StatusFailed || !strings.Contains(hl.Detail, "aws sso login --sso-session example-sso") {
		t.Errorf("ops-admin health = %+v", hl)
	}
	if hl := h.health("ops-readonly"); hl.Status != StatusHealthy {
		t.Errorf("stand-in health = %+v", hl)
	}
}

// A static profile's account is not in the config; it is asked, and a static
// profile on an account already read is not read twice.
func TestAStaticProfileIsPlacedByAsking(t *testing.T) {
	admin := &fakeAPI{account: "111122223333"}
	static := &fakeAPI{account: "111122223333"}
	h := newHarness(t, Config{Profiles: []string{"ops-admin", "static"}}, map[string]*fakeAPI{"ops-admin": admin, "static": static})
	h.src.PollAlarms(context.Background())
	if static.count("Alarms") != 0 || h.health("static").Status != StatusSkipped {
		t.Errorf("static read %d times, health %+v", static.count("Alarms"), h.health("static"))
	}
}

// When nothing at all can be read the loop fails, so health shows it; one
// account being out is only that account's health.
func TestNothingReadableFailsThePoll(t *testing.T) {
	broken := &fakeAPI{account: "111122223333", alarmsErr: errors.New("connection refused")}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": broken})
	if _, err := h.src.PollAlarms(context.Background()); err == nil {
		t.Fatal("a poll that read nothing reported success")
	}
	if hl := h.health("ops-admin"); hl.Status != StatusDegraded || !strings.Contains(hl.Detail, "connection refused") {
		t.Errorf("health = %+v", hl)
	}
}

// Logins expiring is the ordinary state of SSO, not an outage: the poll
// succeeds and health carries the fix.
func TestOnlyExpiredLoginsIsNotAFailedPoll(t *testing.T) {
	expired := &fakeAPI{account: "111122223333", identityErr: &AuthError{Err: errors.New("expired")}}
	h := newHarness(t, Config{Profiles: []string{"ops-admin"}}, map[string]*fakeAPI{"ops-admin": expired})
	rep, err := h.src.PollAlarms(context.Background())
	if err != nil {
		t.Fatalf("expired logins failed the poll: %v", err)
	}
	if len(rep.Fixes) != 1 || rep.Fixes[0] != "aws sso login --sso-session example-sso" {
		t.Errorf("fixes = %v", rep.Fixes)
	}
}

// An alias names the account where the operator configured one.
func TestAnAliasNamesTheAccount(t *testing.T) {
	acct := &fakeAPI{account: "444455556666"}
	acct.alarms = []Alarm{cpuAlarm("ALARM", time.Date(2026, 10, 30, 23, 0, 0, 0, time.UTC))}
	h := newHarness(t, Config{Profiles: []string{"staging"}, Aliases: map[string]string{"444455556666": "example-staging"}},
		map[string]*fakeAPI{"staging": acct})
	h.src.PollAlarms(context.Background())
	evs := h.take()
	if len(evs) != 1 || evs[0].Account != "example-staging/ap-south-1" {
		t.Fatalf("events = %+v", evs)
	}
}
