package awsdash

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelloB1989/karmax/fleet/internal/fleetapi"
)

// Cost Explorer is read with three calls: every day since the first of last
// month by record type (the daily bars, the month so far, last month, the
// credits), this month by service, and the forecast to the month's end.
// Amounts are what was used: credits and refunds are left out of them and
// said apart, or an account on credits would cost nothing every day.

// minAmount is the least a service must cost to be listed.
const minAmount = 0.005

const day = "2006-01-02"

// noCredits filters credits and refunds out of a Cost Explorer read.
const noCredits = `{"Not":{"Dimensions":{"Key":"RECORD_TYPE","Values":["Credit","Refund"]}}}`

// credit is a record type that is not usage.
func credit(kind string) bool { return kind == "Credit" || kind == "Refund" }

// readCost reads the month's cost as of now (Cost Explorer's days are
// UTC). It returns nil when the daily costs could not be read; the other
// two only leave their part empty. errs say what failed.
func readCost(ctx context.Context, c *caller, now time.Time) (*fleetapi.AWSCost, []string) {
	u := now.UTC()
	today := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonth, nextMonth := month.AddDate(0, -1, 0), month.AddDate(0, 1, 0)
	period := func(from, to time.Time) string { return "Start=" + from.Format(day) + ",End=" + to.Format(day) }

	var (
		wg               sync.WaitGroup
		days, services   []byte
		ahead            []byte
		dErr, sErr, fErr error
	)
	wg.Go(func() {
		days, dErr = pages(ctx, c, "ce", "get-cost-and-usage", "--time-period", period(lastMonth, tomorrow),
			"--granularity", "DAILY", "--metrics", "UnblendedCost", "--group-by", "Type=DIMENSION,Key=RECORD_TYPE", "--region", homeRegion)
	})
	wg.Go(func() {
		services, sErr = pages(ctx, c, "ce", "get-cost-and-usage", "--time-period", period(month, tomorrow),
			"--granularity", "MONTHLY", "--metrics", "UnblendedCost", "--group-by", "Type=DIMENSION,Key=SERVICE", "--filter", noCredits, "--region", homeRegion)
	})
	wg.Go(func() {
		ahead, fErr = c.call(ctx, "ce", "get-cost-forecast", "--time-period", period(today, nextMonth),
			"--metric", "UNBLENDED_COST", "--granularity", "MONTHLY", "--region", homeRegion)
	})
	wg.Wait()

	var errs []string
	fail := func(what string, err error) { errs = append(errs, trim("cost "+what+": "+err.Error())) }
	if dErr != nil {
		fail("daily", dErr)
		return nil, errs
	}
	out := &fleetapi.AWSCost{At: now, ByService: []fleetapi.AWSAmount{}, Daily: []fleetapi.AWSAmount{}}
	var beforeToday float64 // this month's, today left out
	results, err := costResults(days)
	if err != nil {
		fail("daily", err)
		return nil, errs
	}
	for _, r := range results {
		at, err := time.Parse(day, r.TimePeriod.Start)
		if err != nil {
			continue
		}
		amt, credits, unit := r.used()
		if unit != "" {
			out.Currency = unit
		}
		if !at.Before(month) {
			out.Credits += credits
		}
		switch {
		case at.Before(month):
			out.LastMonth += amt
		default:
			out.MonthToDate += amt
			if at.Before(today) {
				beforeToday += amt
			}
		}
		if !at.Before(today.AddDate(0, 0, -13)) {
			out.Daily = append(out.Daily, fleetapi.AWSAmount{Name: r.TimePeriod.Start, Amount: amt})
		}
	}

	if sErr == nil {
		var rs []costResult
		if rs, sErr = costResults(services); sErr == nil {
			by := map[string]float64{}
			for _, r := range rs {
				for _, g := range r.Groups {
					amt, _ := g.Metrics.UnblendedCost.amount()
					by[strings.Join(g.Keys, " ")] += amt
				}
			}
			for name, amt := range by {
				if amt >= minAmount {
					out.ByService = append(out.ByService, fleetapi.AWSAmount{Name: name, Amount: amt})
				}
			}
			slices.SortFunc(out.ByService, func(a, b fleetapi.AWSAmount) int {
				if a.Amount != b.Amount {
					if a.Amount > b.Amount {
						return -1
					}
					return 1
				}
				return strings.Compare(a.Name, b.Name)
			})
		}
	}
	if sErr != nil {
		fail("by service", sErr)
	}

	// The forecast is from today on: the month's end is what came before
	// today and it. A new account has no forecast yet, and that is no error.
	if fErr == nil {
		var f struct{ Total money }
		if fErr = json.Unmarshal(ahead, &f); fErr == nil {
			amt, _ := f.Total.amount()
			out.Forecast = beforeToday + amt
		}
	}
	if fErr != nil && !strings.Contains(fErr.Error(), "DataUnavailable") {
		fail("forecast", fErr)
	}
	if out.Currency == "" {
		out.Currency = "USD"
	}
	return out, errs
}

// money is an amount as Cost Explorer says it: a decimal string and a unit.
type money struct{ Amount, Unit string }

func (m money) amount() (float64, string) {
	v, _ := strconv.ParseFloat(m.Amount, 64)
	return v, m.Unit
}

type costResult struct {
	TimePeriod struct{ Start, End string }
	Total      struct{ UnblendedCost money }
	Groups     []struct {
		Keys    []string
		Metrics struct{ UnblendedCost money }
	}
}

// used is a result by record type as what was used and the credits and
// refunds (a positive amount); one with no groups is all used.
func (r costResult) used() (amt, credits float64, unit string) {
	if len(r.Groups) == 0 {
		amt, unit = r.Total.UnblendedCost.amount()
		return amt, 0, unit
	}
	for _, g := range r.Groups {
		v, u := g.Metrics.UnblendedCost.amount()
		if u != "" {
			unit = u
		}
		if len(g.Keys) > 0 && credit(g.Keys[0]) {
			credits -= v
			continue
		}
		amt += v
	}
	return amt, credits, unit
}

// costResults is every page's ResultsByTime, in order.
func costResults(pagesJSON []byte) ([]costResult, error) {
	var all []costResult
	dec := json.NewDecoder(strings.NewReader(string(pagesJSON)))
	for dec.More() {
		var p struct{ ResultsByTime []costResult }
		if err := dec.Decode(&p); err != nil {
			return nil, unreadable(err)
		}
		all = append(all, p.ResultsByTime...)
	}
	return all, nil
}

// pages runs a Cost Explorer read, and again for each next page (the CLI
// does not page it): the answers, one after another.
func pages(ctx context.Context, c *caller, args ...string) ([]byte, error) {
	var out []byte
	token := ""
	for range 10 {
		a := args
		if token != "" {
			a = append(slices.Clone(args), "--next-page-token", token)
		}
		b, err := c.call(ctx, a...)
		if err != nil {
			return nil, err
		}
		out = append(append(out, b...), '\n')
		var p struct{ NextPageToken string }
		if json.Unmarshal(b, &p) != nil || p.NextPageToken == "" {
			break
		}
		token = p.NextPageToken
	}
	return out, nil
}
