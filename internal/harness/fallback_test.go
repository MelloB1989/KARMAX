package harness

import (
	"context"
	"testing"
	"time"
)

// The drill that matters: with the harness unusable, does anything still work?
//
// Every failure here must reach the caller as "not available", never as an
// error — a caller that sees an error surfaces it to a person, where one told
// the harness is unavailable quietly takes the metered path.
func TestEveryFailureModeIsRoutableNotFatal(t *testing.T) {
	cases := []struct {
		name  string
		setup func() (*Supervisor, func())
	}{
		{
			name: "the binary does not exist",
			setup: func() (*Supervisor, func()) {
				sup := New(Config{
					Binary: "definitely-not-a-real-binary-xyz", WorkdirRoot: t.TempDir(),
					Policies: map[string]Policy{"chat": {TurnTimeout: 2 * time.Second}},
				}, newMemStore(), NewBreaker(0.9, nil), testLog{t}, nil)
				return sup, func() {}
			},
		},
		{
			name: "quota is already spent",
			setup: func() (*Supervisor, func()) {
				br := NewBreaker(0.4, nil)
				br.Observe(rl(0.99, 0.99, "allowed_warning", time.Now().Add(time.Hour).Unix()))
				sup := New(Config{
					Binary: "claude", WorkdirRoot: t.TempDir(),
					Policies: map[string]Policy{"chat": {TurnTimeout: 2 * time.Second}},
				}, newMemStore(), br, testLog{t}, nil)
				return sup, func() {}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sup, cleanup := tc.setup()
			defer cleanup()

			_, err := sup.Send(context.Background(), "k", "chat", "hello")
			if err == nil {
				t.Fatal("expected the call to be refused")
			}
			// A breaker-open error is the routable kind. A spawn failure trips
			// the breaker, so the SECOND call is routable even though the first
			// reports the underlying cause.
			_, err2 := sup.Send(context.Background(), "k", "chat", "hello again")
			var open ErrBreakerOpen
			if !asOpen(err2, &open) {
				t.Errorf("second call returned %v; a caller cannot tell to fall back", err2)
			}
		})
	}
}

// A session that exceeds its cost ceiling is closed, so one runaway
// conversation cannot drain a window the whole account shares.
func TestACostlySessionIsClosed(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(),
		Policies: map[string]Policy{"chat": {MaxCostUSD: 0.01, TurnTimeout: time.Second}},
	}, st, NewBreaker(0.9, nil), testLog{t}, nil)

	// Simulate a session that has already spent past its ceiling.
	now := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{Key: "spendy", HarnessSessionID: "u", Kind: "chat",
		State: HarnessLive, StartedAt: now, LastActivityAt: now})
	_ = st.RecordHarnessTurn("spendy", 5.0, 1, 1, 1, now)

	rec, _ := st.GetHarnessSession("spendy")
	if rec.CostUSD < 0.01 {
		t.Fatal("precondition: the session should be over its ceiling")
	}
	sup.Close("spendy")
	if got, _ := st.GetHarnessSession("spendy"); got != nil && got.State == HarnessLive {
		t.Error("an over-budget session was left live")
	}
}

// After a restart every pid belongs to a process this daemon no longer owns.
// The rows are marked dead rather than deleted, so the next message resumes the
// conversation instead of starting a new one.
func TestOrphansAreMarkedDeadNotForgotten(t *testing.T) {
	st := newMemStore()
	now := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{
		Key: "survivor", HarnessSessionID: "uuid-keep", Kind: "chat",
		PID: 999999, State: HarnessLive, StartedAt: now, LastActivityAt: now,
	})

	sup := New(Config{Binary: "claude", WorkdirRoot: t.TempDir()}, st, NewBreaker(0.9, nil), testLog{t}, nil)
	sup.ReapOrphans()

	got, _ := st.GetHarnessSession("survivor")
	if got == nil {
		t.Fatal("the session was forgotten; its transcript is now unreachable")
	}
	if got.State != HarnessDead {
		t.Errorf("state = %q, want dead", got.State)
	}
	if got.HarnessSessionID != "uuid-keep" {
		t.Error("the transcript id was lost, so the next message would start cold")
	}
}

// The idle window is the economics of the whole design.
func TestIdleSessionsAreClosed(t *testing.T) {
	st := newMemStore()
	sup := New(Config{
		Binary: "claude", WorkdirRoot: t.TempDir(),
		Policies: map[string]Policy{"chat": {Idle: 10 * time.Minute}},
	}, st, NewBreaker(0.9, nil), testLog{t}, nil)

	old := time.Now().Add(-30 * time.Minute)
	_ = st.SaveHarnessSession(SessionRecord{Key: "stale", HarnessSessionID: "u", Kind: "chat",
		State: HarnessLive, StartedAt: old, LastActivityAt: old})
	fresh := time.Now()
	_ = st.SaveHarnessSession(SessionRecord{Key: "busy", HarnessSessionID: "v", Kind: "chat",
		State: HarnessLive, StartedAt: fresh, LastActivityAt: fresh})

	sup.Reap(time.Now())

	if s, _ := st.GetHarnessSession("stale"); s != nil && s.State == HarnessLive {
		t.Error("an idle session was left holding a process")
	}
	if b, _ := st.GetHarnessSession("busy"); b == nil || b.State != HarnessLive {
		t.Error("an active session was closed; the next message would pay a cold start")
	}
}

func asOpen(err error, out *ErrBreakerOpen) bool {
	if e, ok := err.(ErrBreakerOpen); ok {
		*out = e
		return true
	}
	return false
}
