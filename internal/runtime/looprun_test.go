package runtime

import (
	"testing"
	"time"

	"github.com/MelloB1989/karmax/pkg/loopkit"
)

func TestRetryBackoffGrowsAndIsCapped(t *testing.T) {
	// Doubling matters: a loop failing because the gateway is down should not
	// hammer it once a minute for the whole outage.
	if retryDelay(1) >= retryDelay(2) || retryDelay(2) >= retryDelay(3) {
		t.Errorf("backoff does not grow: %v %v %v", retryDelay(1), retryDelay(2), retryDelay(3))
	}
	// And it has to stop growing, or a long-lived daemon schedules a retry
	// days out and the work is effectively abandoned without being marked dead.
	if d := retryDelay(20); d > 30*time.Minute {
		t.Errorf("backoff at attempt 20 = %v, want it capped at 30m", d)
	}
}

func TestLeaseOutlivesTheRunTimeout(t *testing.T) {
	// If the lease expired first, a slow run would lose it mid-flight and the
	// next tick would start a second copy alongside — the overlap the lease
	// exists to prevent, reintroduced by a constant.
	if leaseTTL <= loopRunTimeout {
		t.Fatalf("leaseTTL %v must exceed loopRunTimeout %v", leaseTTL, loopRunTimeout)
	}
}

func TestDarkDetection(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	every2m := loopkit.Every("2m")
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	// Started long ago, so uptime is not what is being measured here.
	booted := now.Add(-30 * 24 * time.Hour)

	cases := []struct {
		name     string
		schedule loopkit.Schedule
		last     *time.Time
		seen     bool
		want     bool
	}{
		{"succeeded a moment ago", every2m, ago(time.Minute), true, false},
		{"one missed interval is a blip", every2m, ago(3 * time.Minute), true, false},
		{"three missed intervals is a pattern", every2m, ago(10 * time.Minute), true, true},
		{"scheduled but never once succeeded", every2m, nil, false, true},
		// The case that actually happened: a 3h loop quiet for 3 days.
		{"memory-merge, quiet for days", loopkit.Every("3h"), ago(72 * time.Hour), true, true},
		// A cron loop's period is not a duration. Guessing one would report a
		// daily briefing as dark every morning before it runs.
		{"cron loops are not judged on elapsed time", loopkit.Schedule{Cron: "0 30 8 * * *"}, ago(20 * time.Hour), true, false},
		// Manual and event-driven loops are not expected to tick at all.
		{"unscheduled loops are never dark", loopkit.Schedule{}, nil, false, false},
	}
	for _, c := range cases {
		if got := isDark(c.schedule, c.last, c.seen, now, booted); got != c.want {
			t.Errorf("%s: isDark = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFreshlyStartedLoopsAreNotDark(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	// The daemon came up ninety seconds ago and no loop has had a turn yet.
	// Reporting all of them as broken on every restart is how a health page
	// teaches people to ignore it.
	justBooted := now.Add(-90 * time.Second)
	if isDark(loopkit.Every("2m"), nil, false, now, justBooted) {
		t.Error("a loop that has not had a turn yet was reported as dark")
	}
	// Once it has had three intervals of opportunity and still never
	// succeeded, it is genuinely dark.
	if !isDark(loopkit.Every("2m"), nil, false, now, now.Add(-20*time.Minute)) {
		t.Error("a loop that never succeeded despite ample time should be dark")
	}
}

func TestTransportFailuresAreDistinguishedFromRefusals(t *testing.T) {
	// Only unreachability should fall through to the harness. A refusal or a
	// bad prompt fails the same way on both paths, so retrying it costs twice
	// and answers no better.
	transport := []string{
		`Post "http://localhost:9000/v1/messages": dial tcp [::1]:9000: connect: connection refused`,
		"all models failed, primary error: exhausted 3 retries",
		"context deadline exceeded",
		"502 Bad Gateway",
	}
	for _, s := range transport {
		if !isTransportFailure(errString(s)) {
			t.Errorf("should be treated as unreachable: %q", s)
		}
	}
	refusals := []string{
		"invalid_request_error: max_tokens must be greater than 0",
		"the model declined to answer",
		"authentication_error: invalid api key",
	}
	for _, s := range refusals {
		if isTransportFailure(errString(s)) {
			t.Errorf("should NOT trigger a harness fallback: %q", s)
		}
	}
	if isTransportFailure(nil) {
		t.Error("nil is not a failure")
	}
}

func TestScheduleRendering(t *testing.T) {
	if got := renderSchedule(loopkit.Every("45m")); got != "@every 45m" {
		t.Errorf("got %q", got)
	}
	if got := renderSchedule(loopkit.Schedule{Cron: "0 30 8 * * *"}); got != "0 30 8 * * *" {
		t.Errorf("got %q", got)
	}
	if got := renderSchedule(loopkit.Schedule{}); got != "" {
		t.Errorf("an unscheduled loop should render empty, got %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
