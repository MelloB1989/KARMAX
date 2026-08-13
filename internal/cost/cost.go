// Package cost turns token counts into money.
//
// The rates lived in the cost command, so the only thing that could answer "what
// are we spending" was a human running a CLI. The agent whose whole job is
// deciding between acting itself and delegating to a subscription harness could
// not see the price of that choice, and nothing compared the running total to
// the budget the operator actually set.
package cost

import (
	"strings"
	"time"
)

// Rate is a model's published per-million-token price.
type Rate struct {
	In  float64
	Out float64
}

// Published per-million-token rates. Deliberately approximate: this is a
// tripwire, not an invoice, and the provider's billing is the authority.
var modelRates = map[string]Rate{
	"claude-sonnet-4.6": {3.00, 15.00},
	"claude-sonnet-4-6": {3.00, 15.00},
	"claude-haiku-4.5":  {1.00, 5.00},
	"claude-haiku-4-5":  {1.00, 5.00},
	"claude-opus-4.6":   {5.00, 25.00},
	"claude-opus-4-6":   {5.00, 25.00},
}

// Cache multipliers against the input rate: a read is far cheaper than fresh
// input, a write slightly dearer. Getting these wrong in either direction makes
// caching look like it is not working, or like it is free.
const (
	cacheReadFactor  = 0.10
	cacheWriteFactor = 1.25
)

// RateFor matches a model name against the table, tolerating the provider
// prefixes and version suffixes Bedrock adds
// ("global.anthropic.claude-sonnet-4-6-v1:0").
func RateFor(model string) (Rate, bool) {
	if r, ok := modelRates[model]; ok {
		return r, true
	}
	lower := strings.ToLower(model)
	for name, r := range modelRates {
		if strings.Contains(lower, name) {
			return r, true
		}
	}
	return Rate{}, false
}

// Usage is the token counts one line of spend covers.
type Usage struct {
	Model        string
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
}

// Estimate prices one usage line, reporting false when the model has no known
// rate — an unpriced model must read as unknown, never as zero, or the total
// quietly understates the bill.
func Estimate(u Usage) (float64, bool) {
	r, ok := RateFor(u.Model)
	if !ok {
		return 0, false
	}
	perMillion := func(tokens int64, rate float64) float64 {
		return (float64(tokens) / 1e6) * rate
	}
	return perMillion(u.InputTokens, r.In) +
		perMillion(u.CacheRead, r.In*cacheReadFactor) +
		perMillion(u.CacheWrite, r.In*cacheWriteFactor) +
		perMillion(u.OutputTokens, r.Out), true
}

// Budget is a monthly spend target and where the current run rate sits against
// it.
type Budget struct {
	// MonthlyUSD is the operator's cap. Zero means none was set.
	MonthlyUSD float64
	// Spent is the estimated cost over the window measured.
	Spent float64
	// Days is the width of that window.
	Days float64
}

// Projected extrapolates the window to a 30-day month.
func (b Budget) Projected() float64 {
	if b.Days <= 0 {
		return 0
	}
	return b.Spent * 30 / b.Days
}

// Status describes the run rate against the budget in one word.
type Status string

const (
	StatusNone   Status = "no budget set"
	StatusOK     Status = "within budget"
	StatusNear   Status = "approaching budget"
	StatusOver   Status = "over budget"
	nearFraction        = 0.8
)

func (b Budget) Status() Status {
	if b.MonthlyUSD <= 0 {
		return StatusNone
	}
	switch p := b.Projected(); {
	case p > b.MonthlyUSD:
		return StatusOver
	case p > b.MonthlyUSD*nearFraction:
		return StatusNear
	default:
		return StatusOK
	}
}

// Headroom is what the projection leaves of the budget, negative when over.
func (b Budget) Headroom() float64 {
	if b.MonthlyUSD <= 0 {
		return 0
	}
	return b.MonthlyUSD - b.Projected()
}

// WindowDays converts a start time to the window width Budget expects, with a
// floor so a window shorter than an hour cannot project a wild monthly figure
// from a handful of calls.
func WindowDays(since time.Time) float64 {
	days := time.Since(since).Hours() / 24
	if days < 1.0/24 {
		return 1.0 / 24
	}
	return days
}
