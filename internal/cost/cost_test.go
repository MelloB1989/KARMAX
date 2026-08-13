package cost

import (
	"testing"
	"time"
)

func TestRateForToleratesBedrockNaming(t *testing.T) {
	// Bedrock prefixes the region scope and suffixes a version; an unmatched
	// model prices as zero, which would silently understate the bill.
	for _, model := range []string{
		"global.anthropic.claude-sonnet-4-6-v1:0",
		"anthropic.claude-sonnet-4.6",
		"claude-sonnet-4-6",
	} {
		if _, ok := RateFor(model); !ok {
			t.Errorf("no rate matched for %q", model)
		}
	}
	if _, ok := RateFor("some-other-model"); ok {
		t.Error("an unknown model must report no rate, not a default one")
	}
}

func TestEstimatePricesCacheBelowFreshInput(t *testing.T) {
	fresh, ok := Estimate(Usage{Model: "claude-sonnet-4-6", InputTokens: 1_000_000})
	if !ok {
		t.Fatal("expected a rate")
	}
	cached, _ := Estimate(Usage{Model: "claude-sonnet-4-6", CacheRead: 1_000_000})
	if cached >= fresh {
		t.Errorf("a cache read (%.2f) must cost less than fresh input (%.2f)", cached, fresh)
	}
	written, _ := Estimate(Usage{Model: "claude-sonnet-4-6", CacheWrite: 1_000_000})
	if written <= fresh {
		t.Errorf("a cache write (%.2f) should cost more than fresh input (%.2f)", written, fresh)
	}
}

func TestEstimateReportsUnpricedRatherThanZero(t *testing.T) {
	if _, ok := Estimate(Usage{Model: "mystery", InputTokens: 5_000_000}); ok {
		t.Error("an unpriced model must not report a cost")
	}
}

func TestBudgetStatusTracksTheRunRate(t *testing.T) {
	cases := []struct {
		name  string
		spent float64
		want  Status
	}{
		{"comfortable", 2, StatusOK},
		{"close", 4.5, StatusNear},
		{"over", 6, StatusOver},
	}
	for _, c := range cases {
		b := Budget{MonthlyUSD: 20, Spent: c.spent, Days: 7}
		if got := b.Status(); got != c.want {
			t.Errorf("%s: $%.2f/7d projects $%.2f/month, status = %q, want %q",
				c.name, c.spent, b.Projected(), got, c.want)
		}
	}
}

func TestBudgetWithNoCapReportsNone(t *testing.T) {
	b := Budget{Spent: 100, Days: 1}
	if b.Status() != StatusNone {
		t.Errorf("status = %q, want %q", b.Status(), StatusNone)
	}
	if b.Headroom() != 0 {
		t.Errorf("headroom against no budget should be 0, got %v", b.Headroom())
	}
}

func TestHeadroomGoesNegativeWhenOver(t *testing.T) {
	b := Budget{MonthlyUSD: 20, Spent: 7, Days: 7} // $30/month
	if h := b.Headroom(); h >= 0 {
		t.Errorf("headroom = %.2f, want negative", h)
	}
}

func TestWindowDaysFloorsVeryShortWindows(t *testing.T) {
	// Without a floor, a few calls in the first seconds of a window project an
	// absurd monthly figure and the budget reads as blown.
	if d := WindowDays(time.Now()); d < 1.0/24 {
		t.Errorf("window = %v, want at least an hour", d)
	}
	if d := WindowDays(time.Now().AddDate(0, 0, -7)); d < 6.9 || d > 7.1 {
		t.Errorf("seven days should measure ~7, got %v", d)
	}
}
