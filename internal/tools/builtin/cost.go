package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MelloB1989/karmax/internal/cost"
	"github.com/MelloB1989/karmax/internal/store"
	"github.com/MelloB1989/karmax/internal/tools"
)

// Knowing what it costs to think.
//
// The agent's whole job is choosing between doing work itself on metered
// inference and handing it to a harness on a flat subscription. It was making
// that call with no idea what either side costs, and the operator's budget was a
// number only the operator remembered — so "we are over" was something nobody
// could say without running a CLI.

// CostTool reports model spend against the configured budget.
type CostTool struct {
	Store *store.Store
	// BudgetUSDPerMonth is the operator's cap; zero means none set.
	BudgetUSDPerMonth float64
}

func (t *CostTool) Manifest() tools.ToolManifest {
	return tools.ToolManifest{
		Name: "cost.report",
		Description: "What inference has cost recently, per model, against the operator's monthly budget. " +
			"Check it before starting expensive work, when asked what things are costing, and when deciding " +
			"whether to do something yourself or delegate it — delegation to a harness is flat-rate, your own " +
			"turns are metered.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"days": {"type": "integer", "description": "How many days back to total. Default 7."}
			}
		}`),
	}
}

func (t *CostTool) Execute(ctx context.Context, input map[string]any) (tools.ToolResult, error) {
	if t.Store == nil {
		return tools.ErrorResult(fmt.Errorf("usage accounting is not available on this instance")), nil
	}
	days := 7
	if d, ok := numberArg(input["days"]); ok && d > 0 {
		days = int(d)
	}

	since := time.Now().AddDate(0, 0, -days)
	totals, err := t.Store.UsageSince(since)
	if err != nil {
		return tools.ErrorResult(err), nil
	}
	if len(totals) == 0 {
		return tools.SuccessResult(map[string]any{
			"days": days, "note": "no model usage recorded in this window",
		}), nil
	}

	var spent float64
	var unpriced []string
	lines := make([]map[string]any, 0, len(totals))
	for _, u := range totals {
		line := map[string]any{
			"model": u.Model, "instance": u.Kind, "calls": u.Calls,
			"input_tokens": u.InputTokens, "cached_tokens": u.CacheRead,
			"output_tokens": u.OutputTokens,
		}
		if c, ok := cost.Estimate(cost.Usage{
			Model: u.Model, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
			CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
		}); ok {
			spent += c
			line["est_usd"] = round2(c)
		} else {
			unpriced = append(unpriced, u.Model)
		}
		lines = append(lines, line)
	}

	budget := cost.Budget{
		MonthlyUSD: t.BudgetUSDPerMonth,
		Spent:      spent,
		Days:       cost.WindowDays(since),
	}
	out := map[string]any{
		"days":              days,
		"by_model":          lines,
		"est_usd":           round2(spent),
		"projected_monthly": round2(budget.Projected()),
		"status":            string(budget.Status()),
	}
	if budget.MonthlyUSD > 0 {
		out["budget_monthly_usd"] = budget.MonthlyUSD
		out["headroom_monthly_usd"] = round2(budget.Headroom())
	}
	if len(unpriced) > 0 {
		// Said plainly: a model with no rate contributes nothing to the total,
		// so the figure is a floor rather than an estimate.
		out["unpriced_models"] = unpriced
		out["note"] = "some models have no published rate here, so the total is a lower bound"
	}
	return tools.SuccessResult(out), nil
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// numberArg reads a JSON number, which arrives as float64 through the tool
// boundary but may be an int when a caller builds the map directly.
func numberArg(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
