package harness

import (
	"testing"
	"time"
)

func rl(five, seven float64, status string, resets int64) *RateLimit {
	return &RateLimit{
		Status: status,
		UnifiedWindows: map[string]Window{
			"five_hour": {Utilization: five, ResetsAt: resets},
			"seven_day": {Utilization: seven, ResetsAt: resets},
		},
	}
}

// The whole point of reading the harness's own numbers: the operator's
// interactive sessions consume the same account and are invisible to any ledger
// KARMAX keeps. At 88% of the weekly window, KARMAX must stand down even though
// its own spend that day was trivial.
func TestStandsDownWhenTheAccountIsNearlySpent(t *testing.T) {
	var tripped bool
	b := NewBreaker(0.4, func(t bool, _ string) { tripped = t })

	b.Observe(rl(0.12, 0.88, "allowed_warning", time.Now().Add(time.Hour).Unix()))

	if ok, why := b.Allow(); ok {
		t.Error("should have tripped at 88% of the weekly window")
	} else if why == "" {
		t.Error("a refusal must say why")
	}
	if !tripped {
		t.Error("the trip was not announced")
	}
}

func TestAllowsWhileWellInsideTheShare(t *testing.T) {
	b := NewBreaker(0.4, nil)
	b.Observe(rl(0.05, 0.10, "allowed", time.Now().Add(time.Hour).Unix()))
	if ok, why := b.Allow(); !ok {
		t.Errorf("should allow at 10%% of a 40%% share: %s", why)
	}
}

// A refusal from the harness is fact, not estimate, and outranks the share.
func TestARefusalTripsRegardlessOfUtilisation(t *testing.T) {
	b := NewBreaker(0.9, nil)
	b.Observe(rl(0.01, 0.01, "rejected", 0))
	if ok, _ := b.Allow(); ok {
		t.Error("an explicit refusal must trip the breaker")
	}
}

// When the window it named has rolled, the breaker lets go by itself.
func TestResetsOnceTheWindowRolls(t *testing.T) {
	b := NewBreaker(0.4, nil)
	b.Observe(rl(0.9, 0.9, "allowed_warning", time.Now().Add(-time.Minute).Unix()))
	if ok, _ := b.Allow(); !ok {
		t.Error("a window whose reset time has passed should no longer block")
	}
}

// A missing binary or a dead login is not about quota and must still stop calls.
func TestHardFailuresTrip(t *testing.T) {
	b := NewBreaker(0.4, nil)
	b.TripOn("could not start claude: executable file not found")
	ok, why := b.Allow()
	if ok {
		t.Error("a spawn failure must trip the breaker")
	}
	if why == "" {
		t.Error("the reason should be preserved for the operator")
	}
}

// Trips must not re-announce on every turn, or the operator learns to ignore it.
func TestTripIsAnnouncedOnce(t *testing.T) {
	n := 0
	b := NewBreaker(0.4, func(tripped bool, _ string) {
		if tripped {
			n++
		}
	})
	for i := 0; i < 5; i++ {
		b.Observe(rl(0.99, 0.99, "allowed_warning", time.Now().Add(time.Hour).Unix()))
	}
	if n != 1 {
		t.Errorf("announced %d times, want 1", n)
	}
}
