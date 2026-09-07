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
// KARMAX keeps. At 88% of the weekly window KARMAX must get out of the way —
// but get out of the way is not the same as stop.
//
// It used to stop, because there was a metered API path behind it to stand down
// TO. There is not any more: the harness is the only engine, so refusing here
// is not falling back to something slower, it is going silent. Past our share
// the work still happens, on the cheapest model.
func TestPastOurShareItDegradesRatherThanStopping(t *testing.T) {
	var announced bool
	b := NewBreaker(0.4, func(t bool, _ string) { announced = t })

	b.Observe(rl(0.12, 0.88, "allowed_warning", time.Now().Add(time.Hour).Unix()))

	d := b.Decide()
	if !d.Allow {
		t.Error("being past our share must not stop the turn — there is nothing behind it")
	}
	if !d.Degrade {
		t.Error("at 88% of a 40% share the turn should have been degraded to the cheap tier")
	}
	if d.Reason == "" {
		t.Error("a change of tier must say why")
	}
	if !announced {
		t.Error("the degrade was not announced")
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

// A degrade is not a stop, and the two must stay distinguishable. Reporting
// "past our share" the same way as "the account said no" is how a temporary
// tier change gets treated as an outage.
func TestARefusalStopsWhereAShareOverrunOnlyDegrades(t *testing.T) {
	spent := NewBreaker(0.4, nil)
	spent.Observe(rl(0.99, 0.99, "allowed_warning", time.Now().Add(time.Hour).Unix()))
	if d := spent.Decide(); !d.Allow || !d.Degrade {
		t.Errorf("99%% of the window should degrade, not stop: %+v", d)
	}

	refused := NewBreaker(0.4, nil)
	refused.Observe(rl(0.01, 0.01, "rejected", time.Now().Add(time.Hour).Unix()))
	if d := refused.Decide(); d.Allow {
		t.Error("the account refusing outright must stop the turn")
	}
}

// Coming back under the share must clear the degrade, or one busy afternoon
// leaves KARMAX on the cheap model until the process restarts.
func TestDroppingBackUnderTheShareRestoresTheGoodModel(t *testing.T) {
	var last bool
	b := NewBreaker(0.4, func(tripped bool, _ string) { last = tripped })
	b.Observe(rl(0.9, 0.9, "allowed_warning", time.Now().Add(time.Hour).Unix()))
	if d := b.Decide(); !d.Degrade {
		t.Fatal("precondition: should be degraded")
	}
	b.Observe(rl(0.05, 0.05, "allowed", time.Now().Add(time.Hour).Unix()))
	if d := b.Decide(); d.Degrade {
		t.Error("back under the share, the good model should be in use again")
	}
	if last {
		t.Error("the recovery was not announced")
	}
}
